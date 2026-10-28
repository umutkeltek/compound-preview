package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/runattempt"
)

func TestSupportsByHarness(t *testing.T) {
	r := DefaultRegistry()
	// Current Claude Code and Codex releases both expose blocking PreToolUse hooks.
	if !r.Supports("claude_code", "PreToolUse") {
		t.Error("claude_code should support PreToolUse block")
	}
	if !r.Supports("codex", "PreToolUse") {
		t.Error("codex should support the stable native PreToolUse block")
	}
	if !r.Supports("claude_zai", "PreToolUse") {
		t.Error("claude_zai should support the Claude Code native PreToolUse block")
	}
	// claude_code AND codex now both have native SessionEnd events with transcript_path in
	// the wire payload. codex 0.149.x confirmed via binary strings|grep (see harness.go
	// codex limitations comment + packet premise verification). Conflating SessionEnd and
	// Stop would still silently overclaim, but the independence proof
	// (TestStopAndSessionEndCapabilitiesAreIndependent) is the right fence against that --
	// this test pins the table stakes.
	if !r.Supports("claude_code", "SessionEnd") {
		t.Error("claude_code should support session-end receipts")
	}
	if !r.Supports("codex", "SessionEnd") {
		t.Error("codex should support session-end receipts — 0.149.x ships native SessionEnd with transcript_path")
	}
	// Both can inject/continue at end-of-turn via Stop (codex via JSON block/continuation).
	if !r.Supports("claude_code", "Stop") || !r.Supports("codex", "Stop") {
		t.Error("both should support stop feedback/continuation")
	}
}

// TestStopAndSessionEndCapabilitiesAreIndependent proves the registry no longer
// conflates the two events: mutating one capability must not move the other.
func TestStopAndSessionEndCapabilitiesAreIndependent(t *testing.T) {
	reg := Registry{Harnesses: map[string]Harness{
		"only_stop": {
			Name:                 "only_stop",
			InstructionMechanism: "test fixture",
			Capabilities:         Capabilities{StopFeedback: true, SessionEndReceipt: false},
		},
		"only_session_end": {
			Name:                 "only_session_end",
			InstructionMechanism: "test fixture",
			Capabilities:         Capabilities{StopFeedback: false, SessionEndReceipt: true},
		},
	}}
	if !reg.Supports("only_stop", "Stop") {
		t.Error("only_stop should support Stop")
	}
	if reg.Supports("only_stop", "SessionEnd") {
		t.Error("only_stop should NOT support SessionEnd — mutating StopFeedback leaked into SessionEndReceipt")
	}
	if !reg.Supports("only_session_end", "SessionEnd") {
		t.Error("only_session_end should support SessionEnd")
	}
	if reg.Supports("only_session_end", "Stop") {
		t.Error("only_session_end should NOT support Stop — mutating SessionEndReceipt leaked into StopFeedback")
	}
}

func TestGenericFallback(t *testing.T) {
	r := DefaultRegistry()
	h, ok := r.Get("some-unknown-harness")
	if ok {
		t.Error("unknown harness should not be found")
	}
	if !h.Fallback {
		t.Error("fallback flag should be set on the generic stand-in")
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop", "SessionEnd"} {
		if r.Supports("some-unknown-harness", event) {
			t.Errorf("generic fallback must not claim native %s support", event)
		}
	}
	if len(h.Limitations) == 0 {
		t.Error("generic harness should carry its fail-open limitation")
	}
}

func TestNamesSorted(t *testing.T) {
	got := DefaultRegistry().Names()
	want := []string{
		"claude_code",
		"claude_deepseek_flash_api",
		"claude_deepseek_flash_nvidia",
		"claude_gpt",
		"claude_gpt56_sol_wpcom",
		"claude_gpt56_sol_wpcom_headroom",
		"claude_kimi",
		"claude_minimax",
		"claude_neuralwatt",
		"claude_qwen",
		"claude_zai",
		"claude_zai_full",
		"codex",
		"deepseek_harness",
		"hermes",
		"oh_my_pi",
		"opencode",
		"prime_agent",
		"qwen_code",
	}
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Names() = %v, want %v", got, want)
		}
	}
}

func TestClaudeZaiCapabilitiesAreExplicitAndKeepDistinctIdentity(t *testing.T) {
	r := DefaultRegistry()
	zai, found := r.Get("claude_zai")
	if !found || zai.Fallback || zai.Name != "claude_zai" {
		t.Fatalf("claude_zai registry row = %+v found=%v, want explicit distinct non-fallback", zai, found)
	}
	// These five are the events the profile's settings actually register. Stop is
	// still absent and must remain distinct from the bounded SessionEnd receipt.
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "SessionEnd"} {
		if !r.Supports("claude_zai", event) {
			t.Errorf("claude_zai should support %s through its isolated Claude Code profile", event)
		}
	}
	for _, event := range []string{"Stop"} {
		if r.Supports("claude_zai", event) {
			t.Errorf("claude_zai claims %s, but its profile settings do not register that hook", event)
		}
	}
	if zai.InstructionMechanism == DefaultRegistry().Harnesses["claude_code"].InstructionMechanism {
		t.Fatal("claude_zai must document its profile-isolated steering mechanism, not copy the native row verbatim")
	}
}

func TestInstalledClaudeProfilesAdvertiseSessionEndReceipt(t *testing.T) {
	t.Parallel()

	registry := DefaultRegistry()
	for _, harnessName := range []string{"claude_code", "claude_zai", "claude_minimax", "claude_qwen"} {
		if !registry.Supports(harnessName, "SessionEnd") {
			t.Errorf("%s must advertise its installed bounded SessionEnd receipt", harnessName)
		}
	}
	for _, harnessName := range []string{"claude_zai", "claude_minimax", "claude_qwen"} {
		if registry.Supports(harnessName, "Stop") {
			t.Errorf("%s must not conflate the uninstalled Stop hook with SessionEnd", harnessName)
		}
	}
}

func TestHermesCapabilitiesAreExplicitAndConservative(t *testing.T) {
	r := DefaultRegistry()
	hermes, found := r.Get("hermes")
	if !found || hermes.Fallback {
		t.Fatalf("hermes registry row = %+v found=%v, want explicit non-fallback", hermes, found)
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "SessionEnd"} {
		if !r.Supports("hermes", event) {
			t.Errorf("hermes should support %s through its evidenced plugin lifecycle", event)
		}
	}
	if r.Supports("hermes", "Stop") {
		t.Error("hermes must not claim a native Stop feedback mapping")
	}
	if !hermes.Capabilities.NativeSkills || !hermes.Capabilities.MCP {
		t.Fatalf("hermes native capabilities = %+v, want skills + MCP", hermes.Capabilities)
	}
	for _, limitation := range []string{
		"dynamic context uses pre_llm_call; on_session_start fires after the system prompt is built",
		"plugin callback exceptions fail open",
		"native blocking requires an installed and content-attested Compound plugin",
		"on_session_end is turn/run scoped, not final chat lifetime",
		"no-agent cron and script execution requires a wrapper or action contract",
	} {
		if !hasString(hermes.Limitations, limitation) {
			t.Errorf("hermes limitations = %v, missing %q", hermes.Limitations, limitation)
		}
	}
}

func TestOpenCodeCapabilitiesDoNotOverclaimMutationOrEndEvents(t *testing.T) {
	r := DefaultRegistry()
	opencode, found := r.Get("opencode")
	if !found || opencode.Fallback {
		t.Fatalf("opencode registry row = %+v found=%v, want explicit non-fallback", opencode, found)
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PostToolUse"} {
		if !r.Supports("opencode", event) {
			t.Errorf("opencode should support %s", event)
		}
	}
	for _, event := range []string{"PreToolUse", "Stop", "SessionEnd"} {
		if r.Supports("opencode", event) {
			t.Errorf("opencode must not claim %s before complete permission/end-event proof", event)
		}
	}
	if !opencode.Capabilities.NativeSkills || !opencode.Capabilities.MCP {
		t.Fatalf("opencode native capabilities = %+v, want skills + MCP", opencode.Capabilities)
	}
	for _, limitation := range []string{
		"Compound context injection uses stable chat.message message.system; the system context transform remains experimental and unused",
		"tool.execute.before cannot deny; the Compound integration does not register it",
		"injection and observation callbacks fail open",
		"no proven SessionEnd or Stop mapping",
		"installed state and content revision come from installed-hooks authority, not the static harness registry",
		"not a mutation-control lane in imported capability authority",
	} {
		if !hasString(opencode.Limitations, limitation) {
			t.Errorf("opencode limitations = %v, missing %q", opencode.Limitations, limitation)
		}
	}
}

func TestLoadRegistryManifest(t *testing.T) {
	manifest, err := LoadRegistryManifest(filepath.Join("..", "..", "fixtures", "harness", "harness-registry.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != RegistryManifestSchemaVersion || manifest.ID != "fixture-harness-registry" {
		t.Fatalf("manifest identity = %q/%q", manifest.SchemaVersion, manifest.ID)
	}
	reg, err := manifest.Registry()
	if err != nil {
		t.Fatal(err)
	}
	if !reg.Supports("claude_code", "PreToolUse") {
		t.Fatal("claude_code should support pre-tool block from manifest")
	}
	if !reg.Supports("codex", "PreToolUse") {
		t.Fatal("codex fixture should expose the stable native pre-tool block")
	}
	if !reg.Supports("codex", "SessionEnd") || !reg.Supports("codex", "Stop") {
		t.Fatalf("codex fixture must model Stop=true AND SessionEnd=true (binary 0.149.x proves native SessionEnd)")
	}
	if got := RegistryRevision(reg); !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("registry revision = %q", got)
	}
}

func TestRegistryManifestRejectsDuplicateHarness(t *testing.T) {
	manifest := RegistryManifest{
		SchemaVersion: RegistryManifestSchemaVersion,
		Harnesses: []ManifestHarness{
			{Name: "codex", InstructionMechanism: "AGENTS.md"},
			{Name: "codex", InstructionMechanism: "AGENTS.md"},
		},
	}
	if _, err := manifest.Registry(); err == nil || !strings.Contains(err.Error(), "duplicate harness") {
		t.Fatalf("duplicate harness err = %v", err)
	}
}

func TestRegistryManifestRejectsEmptyHarnessName(t *testing.T) {
	manifest := RegistryManifest{
		SchemaVersion: RegistryManifestSchemaVersion,
		Harnesses:     []ManifestHarness{{InstructionMechanism: "AGENTS.md"}},
	}
	if _, err := manifest.Registry(); err == nil || !strings.Contains(err.Error(), "without name") {
		t.Fatalf("empty harness err = %v", err)
	}
}

func TestRegistryManifestRejectsWrongSchema(t *testing.T) {
	manifest := RegistryManifest{
		SchemaVersion: "compound.harness_registry.v9",
		Harnesses:     []ManifestHarness{{Name: "codex", InstructionMechanism: "AGENTS.md"}},
	}
	if _, err := manifest.Registry(); err == nil || !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("wrong schema err = %v", err)
	}
}

func TestLoadRegistryManifestDefaultsSourceRef(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.yaml")
	data := []byte(`schema_version: compound.harness_registry.v0.1
harnesses:
  - name: codex
    instruction_mechanism: AGENTS.md
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := LoadRegistryManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SourceRef.Path != path || !strings.HasPrefix(manifest.SourceRef.Revision, "sha256:") || len(manifest.EvidenceRefs) != 1 {
		t.Fatalf("defaulted source ref/evidence = %+v refs=%v", manifest.SourceRef, manifest.EvidenceRefs)
	}
}

func TestLoadOSEntrypointsManifest(t *testing.T) {
	manifest, err := LoadOSEntrypointsManifest(filepath.Join("..", "..", "fixtures", "harness", "os-harness-entrypoints.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != RegistryManifestSchemaVersion || manifest.ID != "os-harness-entrypoints" || manifest.AdapterID != OSEntrypointsAdapterID {
		t.Fatalf("manifest identity = %q/%q/%q", manifest.SchemaVersion, manifest.ID, manifest.AdapterID)
	}
	if manifest.SourceRef.SourceClass != "os-harness-entrypoints" || !strings.HasPrefix(manifest.SourceRef.Revision, "sha256:") {
		t.Fatalf("source ref = %+v", manifest.SourceRef)
	}
	reg, err := manifest.Registry()
	if err != nil {
		t.Fatal(err)
	}
	if !reg.Supports("claude_code", "SessionStart") || !reg.Supports("codex", "SessionStart") {
		t.Fatalf("entrypoint adapter should mark session-start context available")
	}
	if reg.Supports("claude_code", "PreToolUse") || reg.Supports("codex", "PostToolUse") || reg.Supports("hermes", "SessionEnd") {
		t.Fatalf("OS entrypoint adapter should not infer hook/MCP/receipt capabilities")
	}
	claude, ok := reg.Get("claude_code")
	if !ok {
		t.Fatalf("claude source id should canonicalize to claude_code")
	}
	if !strings.Contains(claude.InstructionMechanism, "entrypoint:") || !hasString(claude.Limitations, "source_id=claude") {
		t.Fatalf("claude manifest row = %+v", claude)
	}
}

func TestLoadOSEntrypointsManifestRejectsEmptyEntrypoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness-entrypoints.yaml")
	data := []byte(`version: 2
harnesses:
  codex:
    status: generated_applied
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOSEntrypointsManifest(path); err == nil || !strings.Contains(err.Error(), "needs entrypoint") {
		t.Fatalf("empty entrypoint err = %v", err)
	}
}

func hasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestNewClaudeLaneHarnessesEnrolledWithExpectedLimitations verifies the three
// working switchback Claude lanes (minimax, qwen, zai-full) are enrolled with
// the claude_zai capability set and per-lane extra limitation strings. Falsifier
// for the harness-enrollment contract (switchback Claude-profile lanes).
func TestNewClaudeLaneHarnessesEnrolledWithExpectedLimitations(t *testing.T) {
	r := DefaultRegistry()
	cases := []struct {
		name            string
		extraLimitation string
		hooksInstalled  bool
	}{
		{"claude_minimax", "token-plan provider lane (prepaid); context price cliff near 512k tokens", true},
		{"claude_qwen", "token-plan provider lane (prepaid); night-discount window 22:00-08:00 UTC+8 for execution burns", true},
		// Raised from false once `capability-drift` observed all five hooks in this
		// lane's real settings. The owner resolves claude-zai-full ->
		// _providers/zai-lane/settings.json, so the binding is installed, not planned.
		{"claude_zai_full", "subscription window lane; busy-hour token weighting applies", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, ok := r.Get(tc.name)
			if !ok {
				t.Fatalf("harness %q not enrolled", tc.name)
			}
			if h.Name != tc.name {
				t.Fatalf("harness Name = %q, want %q", h.Name, tc.name)
			}
			if h.Fallback {
				t.Fatalf("harness %q should be explicit non-fallback", tc.name)
			}
			if h.InjectBudgetMS < 1500 {
				t.Fatalf("harness %q InjectBudgetMS = %d, want >= 1500", tc.name, h.InjectBudgetMS)
			}
			if r.InjectionBudgetMS(tc.name) < 1500 {
				t.Fatalf("Registry.InjectionBudgetMS(%q) = %d, want >= 1500", tc.name, r.InjectionBudgetMS(tc.name))
			}
			// This assertion used to require every hook flag true for every lane,
			// which is how the over-claim survived: it pinned the registry against
			// itself rather than against the lane, so it passed while the lanes
			// registered no hooks at all. A declaration is only meaningful when
			// something outside it can disagree -- that is
			// `compound harness capability-drift`, which reads the lane's real
			// settings and launcher.
			//
			// What this test now pins is the invariant that matters: a lane whose
			// hooks were never installed must NOT claim hook capabilities. If you
			// install them, capability-drift reports a (non-fatal) under-claim --
			// raise the declaration then, and only then. claude_minimax made that
			// transition (hook install --harness claude-minimax), so its case pins
			// the raised floor instead: the four context/action hooks plus bounded
			// SessionEnd, while Stop remains unregistered, mirroring claude_zai.
			if tc.hooksInstalled {
				if !h.Capabilities.SessionStartContext || !h.Capabilities.UserPromptContext ||
					!h.Capabilities.PreToolUseBlock || !h.Capabilities.PostToolUseEvent {
					t.Fatalf(
						"harness %q must claim its installed hook bindings: %+v",
						tc.name, h.Capabilities,
					)
				}
				if h.Capabilities.StopFeedback || !h.Capabilities.SessionEndReceipt {
					t.Fatalf(
						"harness %q must keep Stop absent and bounded SessionEnd present: %+v",
						tc.name, h.Capabilities,
					)
				}
			} else if h.Capabilities.SessionStartContext || h.Capabilities.UserPromptContext ||
				h.Capabilities.PreToolUseBlock || h.Capabilities.PostToolUseEvent ||
				h.Capabilities.StopFeedback || h.Capabilities.SessionEndReceipt {
				t.Fatalf(
					"harness %q claims hook capabilities without installed hooks: %+v",
					tc.name, h.Capabilities,
				)
			}
			if !hasString(h.Limitations, "profile-local settings and history require separate hook installation") {
				t.Fatalf("harness %q must keep the un-performed hook-installation limitation", tc.name)
			}
			if !hasString(h.Limitations, tc.extraLimitation) {
				t.Fatalf("harness %q Limitations = %v, missing %q", tc.name, h.Limitations, tc.extraLimitation)
			}
			// Carry the same Claude-profile base limitations as claude_zai.
			zai, _ := r.Get("claude_zai")
			for _, want := range zai.Limitations {
				if !hasString(h.Limitations, want) {
					t.Fatalf("harness %q missing base limitation %q (from claude_zai)", tc.name, want)
				}
			}
		})
	}
}

// TestNewClaudeLaneHarnessesListedInDefaultRegistrySorted verifies the three
// enrolled harnesses appear in DefaultRegistry().Names() so the registry list
// surface (compound harness list backing) reflects enrollment.
func TestNewClaudeLaneHarnessesListedInDefaultRegistrySorted(t *testing.T) {
	got := DefaultRegistry().Names()
	for _, name := range []string{"claude_minimax", "claude_qwen", "claude_zai_full"} {
		var found bool
		for _, candidate := range got {
			if candidate == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("DefaultRegistry().Names() = %v, missing %q", got, name)
		}
	}
}

// launchProfileFixtureEntry keeps the one source field that defines this
// slice's boundary. A Prime-family profile is not a Claude adapter merely
// because it shares a provider route with one.
type launchProfileFixtureEntry struct {
	Harness string `json:"harness"`
}

// launchProfilesFixture mirrors the switchback/launch-profiles@1 shape with
// synthetic values. It snapshots the Claude-Code profiles covered by this
// registry slice without depending on operator-local state.
type launchProfilesFixture struct {
	Schema         string                               `json:"schema"`
	LaunchProfiles map[string]launchProfileFixtureEntry `json:"launch_profiles"`
}

// readLaunchProfilesFixture loads the committed fixture. The fixture lives
// under testdata/ so the test never depends on a path outside the repo.
func readLaunchProfilesFixture(t *testing.T) launchProfilesFixture {
	t.Helper()
	path := filepath.Join("testdata", "launch-profiles.fixture.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var fixture launchProfilesFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("parse fixture %s: %v", path, err)
	}
	if fixture.Schema != "switchback/launch-profiles@1" {
		t.Fatalf("fixture schema = %q, want switchback/launch-profiles@1", fixture.Schema)
	}
	if len(fixture.LaunchProfiles) == 0 {
		t.Fatalf("fixture has no launch_profiles entries")
	}
	for id, profile := range fixture.LaunchProfiles {
		if strings.TrimSpace(profile.Harness) == "" {
			t.Fatalf("fixture profile %q has no harness identity", id)
		}
	}
	return fixture
}

// TestEveryClaudeCodeLaunchProfileIsEnrolledAsHarness pins the execution gap:
// a declared Claude-Code launch profile must resolve to both a harness and an
// adapter. The guard is intentionally one-directional; non-profile built-ins
// such as codex, hermes, and opencode remain legal registry entries.
func TestEveryClaudeCodeLaunchProfileIsEnrolledAsHarness(t *testing.T) {
	fixture := readLaunchProfilesFixture(t)
	registry := DefaultRegistry()
	adapters := NewAdapterRegistry(runattempt.Open(t.TempDir()))

	wantNames := make([]string, 0, len(fixture.LaunchProfiles))
	skippedNonClaude := 0
	for rawID, profile := range fixture.LaunchProfiles {
		if profile.Harness != "claude-code" {
			// Prime lives in the fixture as a negative selector. This loop
			// proves the Claude-Code enrollment boundary by skipping every
			// non-Claude profile; it does not assert whether non-Claude
			// profiles are enrolled elsewhere. First-class Prime enrollment
			// is a planned separate slice and must not be blocked here.
			if rawID != "prime-minimax" || profile.Harness != "prime-agent" {
				t.Errorf(
					"unexpected non-Claude profile in enrollment fixture: rawID=%q harness=%q "+
						"(want prime-minimax/prime-agent as the negative-selector shape)",
					rawID, profile.Harness,
				)
			}
			skippedNonClaude++
			continue
		}
		normalized := strings.ReplaceAll(strings.TrimSpace(rawID), "-", "_")
		wantNames = append(wantNames, normalized)
		if _, ok := registry.Get(rawID); !ok {
			t.Errorf(
				"Claude-Code launch profile %q (normalized %q) has no registered harness; "+
					"declared-but-not-enrolled leaves the lane with no harness to dispatch into",
				rawID, normalized,
			)
		}
		if _, ok := adapters.AdapterByName(rawID); !ok {
			t.Errorf(
				"Claude-Code launch profile %q (normalized %q) has no adapter registered; "+
					"a registry entry without a runtime adapter is declared-but-not-executor-backed",
				rawID, normalized,
			)
		}
	}

	// The fixture snapshots the ten Claude-Code profiles at enrollment time.
	// Prime is present as a negative selector inside the skipped branch
	// above; the nonzero skippedNonClaude count below proves this loop
	// actually exercises the Claude-Code boundary.
	if len(wantNames) != 10 {
		t.Fatalf("Claude-Code launch profile count = %d, want 10", len(wantNames))
	}
	if skippedNonClaude == 0 {
		t.Fatal("fixture has no non-Claude profile to prove the harness boundary")
	}
	gotNames := registry.Names()
	for _, want := range wantNames {
		var found bool
		for _, candidate := range gotNames {
			if candidate == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("DefaultRegistry().Names() = %v, missing enrolled lane %q", gotNames, want)
		}
	}
}

// TestHarnessIdentityNormalizesHyphenAndUnderscore proves that both public
// launch-profile spelling and internal registry spelling reach the same
// declared harness and executor adapter.
func TestHarnessIdentityNormalizesHyphenAndUnderscore(t *testing.T) {
	fixture := readLaunchProfilesFixture(t)
	registry := DefaultRegistry()
	adapters := NewAdapterRegistry(runattempt.Open(t.TempDir()))

	for rawID, profile := range fixture.LaunchProfiles {
		if profile.Harness != "claude-code" {
			continue
		}
		hyphen := strings.TrimSpace(rawID)
		underscore := strings.ReplaceAll(hyphen, "-", "_")
		h1, ok1 := registry.Get(hyphen)
		h2, ok2 := registry.Get(underscore)
		if !ok1 || !ok2 {
			t.Errorf("hyphen form %q (ok=%v) or underscore form %q (ok=%v) missing from registry",
				hyphen, ok1, underscore, ok2)
			continue
		}
		if h1.Name != h2.Name {
			t.Errorf("hyphen form resolves to %q, underscore form to %q; they must be the same harness",
				h1.Name, h2.Name)
		}
		a1, aok1 := adapters.AdapterByName(hyphen)
		a2, aok2 := adapters.AdapterByName(underscore)
		if !aok1 || !aok2 {
			t.Errorf("hyphen adapter %q (ok=%v) or underscore adapter %q (ok=%v) missing",
				hyphen, aok1, underscore, aok2)
			continue
		}
		if a1.Name() != a2.Name() {
			t.Errorf("hyphen adapter resolves to %q, underscore adapter to %q; they must be the same adapter",
				a1.Name(), a2.Name())
		}
	}
}
