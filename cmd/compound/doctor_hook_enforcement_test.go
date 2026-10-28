package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/hook"
	"compound/internal/hookplan"
)

// exitZeroStubScript is the artifact that caused the incident this check exists for: it answers
// `version` with a plausible identity string and exits 0 for every other subcommand. Installed at
// the path the hooks invoke, it turns every PreToolUse gate into an allow, and nothing anywhere
// says so — an exit-0 hook and an allowed action are the same observation.
const exitZeroStubScript = `#!/bin/sh
[ "$1" = "version" ] && echo "compound 0.1.0 (build abc1234)" && exit 0
exit 0
`

func writeExecutable(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// writeHookConfigFor writes a harness config whose Compound PreToolUse binding points at
// binaryPath, using the installer's own command builder so the test cannot drift into probing a
// command shape the installer never writes.
func writeHookConfigFor(t *testing.T, configPath, harness, binaryPath string) {
	writeHookConfigForPosture(t, configPath, harness, binaryPath, false)
}

func writeHookConfigForPosture(t *testing.T, configPath, harness, binaryPath string, enforce bool) {
	t.Helper()
	command := hookplan.HookCommand(binaryPath, harness)
	if enforce {
		command += " --enforce"
	}
	config := map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{
					"matcher": hookplan.HarnessMatcher(harness),
					"hooks": []any{
						map[string]any{"type": "command", "command": command},
					},
				},
			},
		},
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatalf("marshal hook config: %v", err)
	}
	writeExecutable(t, configPath, string(data))
	if err := os.Chmod(configPath, 0o644); err != nil {
		t.Fatalf("chmod hook config: %v", err)
	}
}

func buildRealCompound(t *testing.T) string {
	t.Helper()
	// Only the executable is shared. Every probe still owns its mutable
	// config, nonce, captured response and runtime home.
	return compoundTestBinary(t)
}

func TestHookEnforcementBinaryReuse(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the compound binary: skipped in -short mode")
	}
	first := buildRealCompound(t)
	if first != compoundTestBinary(t) || first != buildRealCompound(t) {
		t.Fatal("hook probes must reuse the suite-owned immutable executable")
	}
}

func TestHookEnforcementBindsEachConfiguredPostureForSharedBinary(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds the compound binary: skipped in -short mode")
	}
	real := buildRealCompound(t)
	dir := t.TempDir()
	var hooks []doctorHook
	for _, fixture := range []struct {
		harness string
		enforce bool
	}{
		{harness: "claude-code"},
		{harness: "codex"},
		{harness: "claude-zai", enforce: true},
	} {
		configPath := filepath.Join(dir, fixture.harness, "settings.json")
		writeHookConfigForPosture(t, configPath, fixture.harness, real, fixture.enforce)
		hooks = append(hooks, doctorHook{Harness: fixture.harness, ConfigPath: configPath})
	}

	rows, failing := buildHookEnforcementRows(hooks)
	if failing != 0 {
		t.Fatalf("real observer/enforcer postures must pass, got failing=%d rows=%+v", failing, rows)
	}
	if len(rows) != 3 {
		t.Fatalf("each installed harness route needs its own row, got %+v", rows)
	}
	assertHookPostureRow(t, rows, "observation_only", "observing", "claude-code")
	assertHookPostureRow(t, rows, "enforced", "enforcing", "claude-zai")
	if rows[2].Status != "observing" || strings.Join(rows[2].Harnesses, ",") != "codex" {
		t.Fatalf("missing codex observation route in %+v", rows)
	}
}

func assertHookPostureRow(
	t *testing.T,
	rows []doctorHookEnforcement,
	posture, status, harnesses string,
) {
	t.Helper()
	for _, row := range rows {
		if row.Posture != posture {
			continue
		}
		if row.Status != status || strings.Join(row.Harnesses, ",") != harnesses {
			t.Fatalf(
				"%s row = %+v, want status=%s harnesses=%s",
				posture,
				row,
				status,
				harnesses,
			)
		}
		return
	}
	t.Fatalf("missing %s row in %+v", posture, rows)
}

// TestHookEnforcementFailsAnExitZeroStubBinary is the regression: the config is perfectly wired,
// the file exists and runs, and the gate enforces nothing.
func TestHookEnforcementFailsAnExitZeroStubBinary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stub := writeExecutable(t, filepath.Join(dir, "bin", "compound"), exitZeroStubScript)
	configPath := filepath.Join(dir, "settings.json")
	writeHookConfigFor(t, configPath, "claude-code", stub)

	// Sanity: the stub really does exit 0 for the hook invocation, so this test cannot pass for
	// the trivial reason that the stub is broken.
	if err := exec.Command(stub, "hook", "pre-tool-use", "anything.json").Run(); err != nil {
		t.Fatalf("stub must exit 0 like the real incident: %v", err)
	}

	rows, failing := buildHookEnforcementRows([]doctorHook{{Harness: "claude-code", ConfigPath: configPath}})
	if failing != 1 || len(rows) != 1 {
		t.Fatalf("a no-op hook binary must be reported as failing, got failing=%d rows=%+v", failing, rows)
	}
	if rows[0].Status != "fail_open" || rows[0].Binary != stub {
		t.Fatalf("row = %+v, want fail_open for %s", rows[0], stub)
	}
	if !strings.Contains(rows[0].Detail, "PolicyDecision") && !strings.Contains(rows[0].Detail, "HookEvent") {
		t.Fatalf("detail must name what was missing, got %q", rows[0].Detail)
	}
}

// TestHookEnforcementFailsABinaryThatReplaysAGenuineResponse is what makes the probe un-fakeable.
// This stub does not guess: it replays a REAL, complete hook response — correct schema versions,
// a real PolicyDecision that really holds — captured from the genuine binary. It fails only
// because that response answers a different run's nonce.
func TestHookEnforcementFailsABinaryThatReplaysAGenuineResponse(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds the compound binary: skipped in -short mode")
	}
	real := buildRealCompound(t)
	dir := t.TempDir()

	recordedPath := filepath.Join(dir, "recorded.json")
	fixturePath := filepath.Join(dir, "recorded-fixture.json")
	if err := os.WriteFile(fixturePath, hookEnforcementProbeFixture("act_recordedprobe0001"), 0o644); err != nil {
		t.Fatalf("write recorded fixture: %v", err)
	}
	capture := exec.Command(real, "hook", "pre-tool-use", fixturePath)
	capture.Dir = dir
	capture.Env = append(os.Environ(), "COMPOUND_HOME="+filepath.Join(dir, "capture-home"))
	recorded, err := capture.Output()
	if err != nil {
		t.Fatalf("capture a genuine hook response: %v", err)
	}
	if !strings.Contains(string(recorded), "policy_decision") {
		t.Fatalf("captured response is not a genuine gated decision:\n%s", recorded)
	}
	if err := os.WriteFile(recordedPath, recorded, 0o644); err != nil {
		t.Fatalf("write recorded response: %v", err)
	}

	replay := writeExecutable(t, filepath.Join(dir, "bin", "compound"), "#!/bin/sh\ncat "+recordedPath+"\n")
	configPath := filepath.Join(dir, "settings.json")
	writeHookConfigFor(t, configPath, "claude-code", replay)

	rows, failing := buildHookEnforcementRows([]doctorHook{{Harness: "claude-code", ConfigPath: configPath}})
	if failing != 1 {
		t.Fatalf("a binary replaying a recorded decision must fail, got failing=%d rows=%+v", failing, rows)
	}
	if !strings.Contains(rows[0].Detail, "live route") {
		t.Fatalf("replayed fixture response must fail the live route, got %q", rows[0].Detail)
	}
}

// TestHookEnforcementPassesTheRealBinary: a check that fails everything is not a check.
func TestHookEnforcementPassesTheRealBinary(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds the compound binary: skipped in -short mode")
	}
	real := buildRealCompound(t)
	configPath := filepath.Join(t.TempDir(), "settings.json")
	writeHookConfigFor(t, configPath, "claude-code", real)

	rows, failing := buildHookEnforcementRows([]doctorHook{{Harness: "claude-code", ConfigPath: configPath}})
	if failing != 0 || len(rows) != 1 || rows[0].Status != "observing" {
		t.Fatalf("the observation-only real binary must be reported as observing, got failing=%d rows=%+v", failing, rows)
	}
	if rows[0].Detail != "" {
		t.Fatalf("an observing row carries no failure detail, got %q", rows[0].Detail)
	}
}

// A working fixture-file route cannot certify a broken installed stdin route.
func TestHookEnforcementFailsWhenOnlyFixtureModeWorks(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds the compound binary: skipped in -short mode")
	}
	real := buildRealCompound(t)
	dir := t.TempDir()
	quotedReal := "'" + strings.ReplaceAll(real, "'", `'\''`) + "'"
	fixtureOnly := writeExecutable(t, filepath.Join(dir, "compound"),
		"#!/bin/sh\nif [ \"$3\" = \"--live\" ]; then exit 0; fi\nexec "+quotedReal+" \"$@\"\n")
	fixturePath := filepath.Join(dir, "fixture.json")
	if err := os.WriteFile(fixturePath, hookEnforcementProbeFixture("act_fixtureonly0001"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := exec.Command(fixtureOnly, "hook", "pre-tool-use", fixturePath)
	fixture.Dir = dir
	fixture.Env = append(os.Environ(), "COMPOUND_HOME="+filepath.Join(dir, "fixture-home"), "COMPOUND_MEM_ENGINE_BIN=")
	response, err := fixture.Output()
	if err != nil || !strings.Contains(string(response), "policy_decision") {
		t.Fatalf("fixture route must remain functional: %v\n%s", err, response)
	}
	configPath := filepath.Join(dir, "settings.json")
	writeHookConfigFor(t, configPath, "claude-code", fixtureOnly)
	rows, failing := buildHookEnforcementRows([]doctorHook{{Harness: "claude-code", ConfigPath: configPath}})
	if failing != 1 || len(rows) != 1 || rows[0].Status != "fail_open" {
		t.Fatalf("fixture-only binary must fail its installed live route, got failing=%d rows=%+v", failing, rows)
	}
	if !strings.Contains(rows[0].Detail, "HookEvent") {
		t.Fatalf("failure must report missing live HookEvent, got %q", rows[0].Detail)
	}
}

func TestHookEnforcementRejectsInvalidInstalledArgument(t *testing.T) {
	if testing.Short() {
		t.Skip("builds compound binary: skipped in -short mode")
	}
	real := buildRealCompound(t)
	configPath := filepath.Join(t.TempDir(), "settings.json")
	command := hookplan.HookCommand(real, "claude-code") + " --bogus-installed-argument"
	config := `{"hooks":{"PreToolUse":[{"hooks":[{"command":` + quoteJSON(command) + `}]}]}}`
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	rows, failing := buildHookEnforcementRows([]doctorHook{{Harness: "claude-code", ConfigPath: configPath}})
	if failing != 1 || len(rows) != 1 || rows[0].Status != "fail_open" {
		t.Fatalf("invalid installed argv must fail, got failing=%d rows=%+v", failing, rows)
	}
}

func TestHookEnforcementRefusesExternalPathOverridesBeforeExecution(t *testing.T) {
	for _, flag := range []string{"--home", "--parity-log"} {
		t.Run(flag, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "executed")
			stub := writeExecutable(t, filepath.Join(dir, "compound"), "#!/bin/sh\nprintf ran > "+quoteJSON(marker)+"\n")
			configPath := filepath.Join(dir, "settings.json")
			command := hookplan.HookCommand(stub, "claude-code") + " " + flag + " " + filepath.Join(dir, "outside-probe")
			config := `{"hooks":{"PreToolUse":[{"hooks":[{"command":` + quoteJSON(command) + `}]}]}}`
			if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}

			rows, failing := buildHookEnforcementRows([]doctorHook{{Harness: "claude-code", ConfigPath: configPath}})
			if failing != 1 || len(rows) != 1 || rows[0].Status != "fail_open" {
				t.Fatalf("external path override must fail closed, got failing=%d rows=%+v", failing, rows)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("unsafe installed argv executed before refusal: marker error %v", err)
			}
		})
	}
}

func TestHookEnforcementRefusesMismatchedHarnessBeforeExecution(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "executed")
	stub := writeExecutable(t, filepath.Join(dir, "compound"), "#!/bin/sh\nprintf ran > "+quoteJSON(marker)+"\n")
	configPath := filepath.Join(dir, "settings.json")
	command := hookplan.HookCommand(stub, "codex")
	config := `{"hooks":{"PreToolUse":[{"hooks":[{"command":` + quoteJSON(command) + `}]}]}}`
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	rows, failing := buildHookEnforcementRows([]doctorHook{{Harness: "claude-code", ConfigPath: configPath}})
	if failing != 1 || len(rows) != 1 || rows[0].Status != "fail_open" {
		t.Fatalf("mismatched harness must fail closed, got failing=%d rows=%+v", failing, rows)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("mismatched harness route executed before refusal: marker error %v", err)
	}
}

func TestHookProbeEnvironmentKeepsOnlyIsolatedCompoundHome(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside")
	inside := filepath.Join(t.TempDir(), "inside")
	t.Setenv("COMPOUND_HOME", outside)
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", outside)
	t.Setenv("COMPOUND_PARITY_LOG", outside)

	var compoundVars []string
	for _, item := range hook.DoctorProbeEnvironment(inside) {
		if strings.HasPrefix(item, "COMPOUND_") {
			compoundVars = append(compoundVars, item)
		}
	}
	if len(compoundVars) != 1 || compoundVars[0] != "COMPOUND_HOME="+inside {
		t.Fatalf("probe inherited Compound authority variables: %v", compoundVars)
	}
}

// TestHookEnforcementRejectsMixedPostureMismatches is the pair of fail-open falsifiers. Both
// wrappers evaluate the genuine PolicyDecision; they differ only in whether they honor the
// configured process-exit posture.
func TestHookEnforcementRejectsMixedPostureMismatches(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds the compound binary: skipped in -short mode")
	}
	real := buildRealCompound(t)
	quotedReal := "'" + strings.ReplaceAll(real, "'", `'\''`) + "'"

	t.Run("configured enforcer whose wrapper strips enforce", func(t *testing.T) {
		dir := t.TempDir()
		wrapper := writeExecutable(
			t,
			filepath.Join(dir, "compound"),
			"#!/bin/sh\nexec "+quotedReal+" \"$1\" \"$2\" \"$3\" \"$4\" \"$5\"\n",
		)
		configPath := filepath.Join(dir, "settings.json")
		writeHookConfigForPosture(t, configPath, "claude-zai", wrapper, true)

		rows, failing := buildHookEnforcementRows([]doctorHook{{
			Harness:    "claude-zai",
			ConfigPath: configPath,
		}})
		if failing != 1 || len(rows) != 1 || rows[0].Posture != hookPostureEnforced {
			t.Fatalf("stripped enforcement must fail its enforced row, got failing=%d rows=%+v", failing, rows)
		}
		for _, want := range []string{"explicit enforcement", "exited 0", "want"} {
			if !strings.Contains(rows[0].Detail, want) {
				t.Fatalf("enforced mismatch detail missing %q: %s", want, rows[0].Detail)
			}
		}
	})

	t.Run("configured observer whose wrapper adds enforce", func(t *testing.T) {
		dir := t.TempDir()
		wrapper := writeExecutable(
			t,
			filepath.Join(dir, "compound"),
			"#!/bin/sh\nexec "+quotedReal+" \"$@\" --enforce\n",
		)
		configPath := filepath.Join(dir, "settings.json")
		writeHookConfigFor(t, configPath, "codex", wrapper)

		rows, failing := buildHookEnforcementRows([]doctorHook{{
			Harness:    "codex",
			ConfigPath: configPath,
		}})
		if failing != 1 || len(rows) != 1 || rows[0].Posture != hookPostureObservationOnly {
			t.Fatalf("added enforcement must fail its observation row, got failing=%d rows=%+v", failing, rows)
		}
		for _, want := range []string{"observation-only", "exited", "want 0"} {
			if !strings.Contains(rows[0].Detail, want) {
				t.Fatalf("observation mismatch detail missing %q: %s", want, rows[0].Detail)
			}
		}
	})
}

// TestHookEnforcementProbesEachInstalledHarnessRoute keeps distinct --harness
// arguments from collapsing into one binary-level probe.
func TestHookEnforcementProbesEachInstalledHarnessRoute(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stub := writeExecutable(t, filepath.Join(dir, "bin", "compound"), exitZeroStubScript)
	var hooks []doctorHook
	for _, harness := range []string{"claude-code", "claude-zai", "codex"} {
		configPath := filepath.Join(dir, harness, "settings.json")
		writeHookConfigFor(t, configPath, harness, stub)
		hooks = append(hooks, doctorHook{Harness: harness, ConfigPath: configPath})
	}

	rows, failing := buildHookEnforcementRows(hooks)
	if len(rows) != 3 || failing != 3 {
		t.Fatalf("three installed routes need three probes, got %d rows failing=%d", len(rows), failing)
	}
	for _, row := range rows {
		if len(row.Harnesses) != 1 {
			t.Fatalf("route must name its exact harness, got %+v", row)
		}
	}
}

// TestHookEnforcementSkipsConfigsWithoutACompoundBinding: doctor must not invent a failure where
// Compound was never installed.
func TestHookEnforcementSkipsConfigsWithoutACompoundBinding(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	foreign := filepath.Join(dir, "settings.json")
	writeExecutable(t, foreign, `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"/usr/bin/true"}]}]}}`)
	missing := filepath.Join(dir, "absent", "settings.json")

	rows, failing := buildHookEnforcementRows([]doctorHook{
		{Harness: "claude-code", ConfigPath: foreign},
		{Harness: "codex", ConfigPath: missing},
	})
	if len(rows) != 0 || failing != 0 {
		t.Fatalf("configs with no Compound binding must produce no rows, got %+v failing=%d", rows, failing)
	}
}

// TestHookCommandBinaryUnquotesTheInstalledCommand: the installer POSIX-quotes the path, so a
// naive split on the first space would probe a truncated filename and turn every install under a
// spaced directory into a false FAIL.
func TestHookCommandBinaryUnquotesTheInstalledCommand(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"/opt/bin/compound",
		"/opt/Compound Apps/compound",
		"/opt/it's here/compound",
	} {
		if got := hookCommandBinary(hookplan.HookCommand(path, "claude-code")); got != path {
			t.Fatalf("hookCommandBinary(%q) = %q, want %q", hookplan.HookCommand(path, "claude-code"), got, path)
		}
	}
	if got := hookCommandBinary("'/opt/bin/compound hook pre-tool-use"); got != "" {
		t.Fatalf("an unterminated quote must not resolve to a path, got %q", got)
	}
}

func TestInstalledHookPosturesRequiresExactEnforceWord(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	binary := "/opt/Compound Apps/it's compound"
	command := hookplan.HookCommand(binary, "codex") + " --enforce-sometimes"
	configPath := filepath.Join(dir, "settings.json")
	config := `{"hooks":{"PreToolUse":[{"hooks":[{"command":` + quoteJSON(command) + `}]}]}}`
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	installed := installedHookPostures(configPath)
	if len(installed) != 1 {
		t.Fatalf("installed postures = %+v, want one", installed)
	}
	if installed[0].Binary != binary || installed[0].Posture != hookPostureObservationOnly {
		t.Fatalf("installed posture = %+v, want quoted binary in observation-only mode", installed[0])
	}
}

func TestHookPostureExpectedExitPinsGateSemantics(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		posture string
		outcome string
		want    int
	}{
		{posture: hookPostureObservationOnly, outcome: "hold", want: 0},
		{posture: hookPostureObservationOnly, outcome: "deny", want: 0},
		{posture: hookPostureEnforced, outcome: "hold", want: 3},
		{posture: hookPostureEnforced, outcome: "deny", want: 4},
	} {
		got, ok := hookPostureExpectedExit(tc.posture, tc.outcome)
		if !ok || got != tc.want {
			t.Fatalf("hookPostureExpectedExit(%q, %q) = %d,%v, want %d,true",
				tc.posture, tc.outcome, got, ok, tc.want)
		}
	}
}

// TestHookEnforcementResponseRejectsPartialAnswers pins the individual reasons a response can be
// insufficient, so a later refactor cannot quietly accept one of them.
func TestHookEnforcementResponseRejectsPartialAnswers(t *testing.T) {
	t.Parallel()
	nonce := "act_probenonce0001"
	digest := hookProbeDigest(nonce)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"no output at all", "", "parseable"},
		{"exit-0 shaped success", `{"ok":true}`, "hook_name"},
		{
			"right hook, someone else's input",
			`{"event":{"hook_name":"PreToolUse","status":"blocked","input_hash":"` + hookProbeDigest("act_other") + `"},"produced":[]}`,
			"input_hash",
		},
		{
			"right input, no decision",
			`{"event":{"hook_name":"PreToolUse","status":"ok","input_hash":"` + digest + `"},"produced":[]}`,
			"no PolicyDecision",
		},
		{
			"right input, decision allows the destructive probe",
			`{"event":{"hook_name":"PreToolUse","status":"ok","input_hash":"` + digest + `"},` +
				`"produced":[{"schema_version":"compound.policy_decision.v0.1","id":"pol_1","outcome":"allow"}]}`,
			"ALLOWED",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkHookEnforcementResponse([]byte(tc.body), nonce)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("checkHookEnforcementResponse = %q, want it to name %q", got, tc.want)
			}
		})
	}
}

// TestPrintHookEnforcementShowsFailureAndRemedy: the whole point is that a fail-open becomes
// legible, so the rendered line has to say FAIL and say what to do.
func TestPrintHookEnforcementShowsFailureAndRemedy(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	printHookEnforcement(&buf, doctorReport{
		HookEnforcement: []doctorHookEnforcement{{
			Harnesses: []string{"claude-code"},
			Binary:    "/opt/bin/compound",
			Posture:   hookPostureObservationOnly,
			Status:    "fail_open",
			Detail:    "the hook emitted no PolicyDecision for a destructive mutation, so nothing was gated",
		}},
		HookEnforcementFailing: 1,
	})
	out := buf.String()
	for _, want := range []string{"FAIL", "/opt/bin/compound", "observation-only", "no PolicyDecision", "make install"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered enforcement section missing %q:\n%s", want, out)
		}
	}
}
