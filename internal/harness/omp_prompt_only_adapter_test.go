package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
)

type ompPromptOnlyFixture struct {
	adapter        *OMPPromptOnlyAdapter
	input          RunInput
	projection     OMPOwnerProjection
	projectionPath string
	wrapperPath    string
	nativePath     string
	modelsPath     string
	workspace      string
	compoundHome   string
	argsCapture    string
	envCapture     string
	nativeCapture  string
}

func newOMPPromptOnlyFixture(t *testing.T) ompPromptOnlyFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ownerHome := filepath.Join(root, "home")
	runtimeRoot := filepath.Join(root, "runtime")
	projectionRoot := filepath.Join(runtimeRoot, "state", "profile-conformance")
	modelsPath := filepath.Join(runtimeRoot, "profile", "agent", "models.yml")
	bin := filepath.Join(root, "bin")
	workspace := filepath.Join(root, "workspace")
	compoundHome := filepath.Join(root, "compound")
	for _, dir := range []string{
		ownerHome, filepath.Dir(modelsPath), projectionRoot, bin, workspace,
		compoundHome, filepath.Join(runtimeRoot, "config"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	argsCapture := filepath.Join(root, "args")
	envCapture := filepath.Join(root, "env")
	nativeCapture := filepath.Join(root, "native")
	nativePath := filepath.Join(bin, "omp")
	wrapperPath := filepath.Join(root, "wrapper")
	native := "#!/bin/sh\nprintf '%s' \"$1\" > " + shellQuoteOMP(nativeCapture) + "\n"
	if err := os.WriteFile(nativePath, []byte(native), 0o700); err != nil {
		t.Fatal(err)
	}
	wrapper := `#!/bin/sh
set -eu
export SB_LAUNCH_CREDENTIAL_ENV='OMP_TEST_KEY'
if [ -z "${OMP_TEST_KEY:-}" ]; then
  . "${SWITCHBACK_RUNTIME_ROOT:-${SB_RUNTIME_ROOT:-$HOME/.switchback}}/config/sb.env"
fi
: > ` + shellQuoteOMP(argsCapture) + `
for arg in "$@"; do printf '%s\n' "$arg" >> ` + shellQuoteOMP(argsCapture) + `; done
printf '%s\n' "$HOME" "$SWITCHBACK_RUNTIME_ROOT" "$PATH" "${PI_CONFIG_DIR-}" "${PI_CODING_AGENT_DIR-}" "${OMP_PROFILE-}" "${NODE_OPTIONS-}" "${BUN_OPTIONS-}" "${OMP_TEST_KEY-}" > ` + shellQuoteOMP(envCapture) + `
omp "$6"
`
	if err := os.WriteFile(wrapperPath, []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modelsPath, []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(runtimeRoot, "config", "sb.env"),
		[]byte("OMP_TEST_KEY=fallback\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "sentinel"), []byte("same"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := validOMPOwnerProjection(t)
	p.Profile.ID, p.LaunchProfileRef = "omp", "switchback://launch-profiles/omp"
	p.PromptOnly.Wrapper.Path = wrapperPath
	p.PromptOnly.Wrapper.SHA256, _ = ompFileSHA256(wrapperPath)
	p.PromptOnly.Wrapper.ExpectedSHA256 = p.PromptOnly.Wrapper.SHA256
	p.PromptOnly.Executable.Path = nativePath
	p.PromptOnly.Executable.SHA256, _ = ompFileSHA256(nativePath)
	p.PromptOnly.ModelsConfig.Path = modelsPath
	p.PromptOnly.ModelsConfig.SHA256, _ = ompFileSHA256(modelsPath)
	p.PromptOnly.ModelsConfig.ExpectedSHA256 = p.PromptOnly.ModelsConfig.SHA256
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	projectionPath := filepath.Join(projectionRoot, "omp.json")
	if err := os.WriteFile(projectionPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMP_TEST_KEY", "exported")
	t.Setenv("PI_CONFIG_DIR", "ambient")
	t.Setenv("PI_CODING_AGENT_DIR", "ambient")
	t.Setenv("OMP_PROFILE", "ambient")
	t.Setenv("NODE_OPTIONS", "ambient")
	t.Setenv("BUN_OPTIONS", "ambient")
	a := NewOMPPromptOnlyAdapter(runattempt.Open(compoundHome))
	a.projectionRoot, a.profileID = projectionRoot, "omp"
	a.ownerHome, a.runtimeRoot, a.executableSearchPath = ownerHome, runtimeRoot, bin
	return ompPromptOnlyFixture{
		adapter: a,
		input: RunInput{
			WorkID: "work_fixture", Harness: a.Name(), Workspace: workspace,
			Prompt: "literal prompt -- one argument", Now: time.Now().UTC(),
		},
		projection: p, projectionPath: projectionPath, wrapperPath: wrapperPath,
		nativePath: nativePath, modelsPath: modelsPath, workspace: workspace,
		compoundHome: compoundHome, argsCapture: argsCapture,
		envCapture: envCapture, nativeCapture: nativeCapture,
	}
}

func TestOMPPromptOnlyExactOuterArgvEnvironmentAndNoWorkspaceMutation(t *testing.T) {
	f := newOMPPromptOnlyFixture(t)
	before := mustReadOMPFile(t, filepath.Join(f.workspace, "sentinel"))
	prepared, err := f.adapter.Preflight(context.Background(), f.input)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.PermissionMode != "prompt_only" || len(prepared.Env) != 0 ||
		len(prepared.Args) != 0 || prepared.Stdin != nil {
		t.Fatalf("prepared surface = %+v", prepared)
	}
	values, err := ompPromptOnlyEnvironmentValues(prepared.ompPromptOnly.Environment)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"PI_CONFIG_DIR", "PI_CODING_AGENT_DIR", "OMP_PROFILE",
		"NODE_OPTIONS", "BUN_OPTIONS",
	} {
		if _, ok := values[key]; ok {
			t.Fatalf("ambient %s leaked", key)
		}
	}
	prepared = governedOMPInput(prepared)
	out, err := f.adapter.Run(context.Background(), prepared)
	if err != nil || out.RunAttempt.Status != runattempt.StatusSucceeded {
		t.Fatalf("Run status=%s err=%v", out.RunAttempt.Status, err)
	}
	got := strings.Split(strings.TrimSuffix(string(mustReadOMPFile(t, f.argsCapture)), "\n"), "\n")
	want := []string{"-p", "--no-session", "--cwd", f.workspace, "--no-tools", f.input.Prompt}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("outer argv=%q want=%q", got, want)
	}
	if got := string(mustReadOMPFile(t, f.nativeCapture)); got != f.input.Prompt {
		t.Fatalf("literal prompt=%q", got)
	}
	if after := mustReadOMPFile(t, filepath.Join(f.workspace, "sentinel")); !reflect.DeepEqual(after, before) {
		t.Fatal("workspace mutated")
	}
}

func TestOMPPromptOnlyCredentialSourceReachesWrapper(t *testing.T) {
	for _, missing := range []bool{false, true} {
		name, want := "exported", "exported"
		if missing {
			name, want = "owner_fallback", "fallback"
		}
		t.Run(name, func(t *testing.T) {
			f := newOMPPromptOnlyFixture(t)
			// The fixture's Setenv cleanup restores the original environment.
			if missing {
				if err := os.Unsetenv("OMP_TEST_KEY"); err != nil {
					t.Fatal(err)
				}
			}
			prepared, err := f.adapter.Preflight(context.Background(), f.input)
			if err != nil {
				t.Fatal(err)
			}
			values, err := ompPromptOnlyEnvironmentValues(prepared.ompPromptOnly.Environment)
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := values["OMP_TEST_KEY"]; exists == missing {
				t.Fatal("wrong prepared credential presence")
			}
			out, err := f.adapter.Run(context.Background(), governedOMPInput(prepared))
			if err != nil || out.RunAttempt.Status != runattempt.StatusSucceeded {
				t.Fatalf("Run status=%s err=%v", out.RunAttempt.Status, err)
			}
			got := strings.Split(strings.TrimSuffix(string(mustReadOMPFile(t, f.envCapture)), "\n"), "\n")
			if len(got) != 9 || got[0] != prepared.ompPromptOnly.OwnerHome ||
				got[1] != prepared.ompPromptOnly.RuntimeRoot || got[2] != values["PATH"] ||
				got[8] != want {
				t.Fatal("owner environment or credential source did not reach wrapper")
			}
			for _, value := range got[3:8] {
				if value != "" {
					t.Fatal("ambient steering reached wrapper")
				}
			}
		})
	}
}

func TestOMPPromptOnlyRejectsCallerOverridesAndPreparedDriftBeforeMint(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*RunInput)
	}{
		{"command", func(in *RunInput) { in.Command = "/bin/sh" }},
		{"harness", func(in *RunInput) { in.Harness = "other-harness" }},
		{"args", func(in *RunInput) { in.Args = []string{"--unsafe"} }},
		{"env", func(in *RunInput) { in.Env = []string{"UNSAFE=1"} }},
		{"permission", func(in *RunInput) { in.PermissionMode = "prompt_only" }},
		{"stdin", func(in *RunInput) { in.Stdin = strings.NewReader("unsafe") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOMPPromptOnlyFixture(t)
			in := f.input
			tc.alter(&in)
			if _, err := f.adapter.Preflight(context.Background(), in); err == nil {
				t.Fatal("override admitted")
			}
			assertNoOMPAttempts(t, f.compoundHome)
		})
	}
	for _, field := range []string{"workspace", "prompt"} {
		t.Run("prepared_"+field, func(t *testing.T) {
			f := newOMPPromptOnlyFixture(t)
			in, err := f.adapter.Preflight(context.Background(), f.input)
			if err != nil {
				t.Fatal(err)
			}
			in = governedOMPInput(in)
			if field == "workspace" {
				in.Workspace = t.TempDir()
			} else {
				in.Prompt = "drift"
			}
			out, err := f.adapter.Run(context.Background(), in)
			if err == nil || out.RunAttempt.ID != "" {
				t.Fatalf("drift reached mint: %+v %v", out, err)
			}
			assertNoOMPAttempts(t, f.compoundHome)
		})
	}
}

func TestOMPPromptOnlyRejectsMissingGovernanceBeforeMint(t *testing.T) {
	for _, field := range []string{"action", "guard", "posture", "policy", "marker"} {
		t.Run(field, func(t *testing.T) {
			f := newOMPPromptOnlyFixture(t)
			in, err := f.adapter.Preflight(context.Background(), f.input)
			if err != nil {
				t.Fatal(err)
			}
			in = governedOMPInput(in)
			switch field {
			case "action":
				in.ActionIntentID = ""
			case "guard":
				in.StartGuard = nil
			case "posture":
				in.ExecutionPosture = nil
			case "policy":
				in.ExecutionPosture.PolicyDecisionID = ""
			case "marker":
				in.ExecutionPosture.AuthorityEvidence[0].Ref = "harness:oh_my_pi"
			}
			out, err := f.adapter.Run(context.Background(), in)
			if err == nil || out.RunAttempt.ID != "" {
				t.Fatalf("ungoverned run reached mint: %+v %v", out, err)
			}
			assertNoOMPAttempts(t, f.compoundHome)
		})
	}
}

func TestOMPPromptOnlyRevalidatesOwnerWrapperNativeBeforeStart(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *ompPromptOnlyFixture)
	}{
		{"owner", func(t *testing.T, f *ompPromptOnlyFixture) {
			p := f.projection
			p.ConformanceRevision = "sha256:" + strings.Repeat("b", 64)
			body, _ := json.Marshal(p)
			if err := os.WriteFile(f.projectionPath, body, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"wrapper", func(t *testing.T, f *ompPromptOnlyFixture) {
			appendOMPFile(t, f.wrapperPath)
		}},
		{"native", func(t *testing.T, f *ompPromptOnlyFixture) {
			appendOMPFile(t, f.nativePath)
		}},
		{"models", func(t *testing.T, f *ompPromptOnlyFixture) { appendOMPFile(t, f.modelsPath) }},
		{"credential", func(t *testing.T, f *ompPromptOnlyFixture) { t.Setenv("OMP_TEST_KEY", "rotated") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOMPPromptOnlyFixture(t)
			in, err := f.adapter.Preflight(context.Background(), f.input)
			if err != nil {
				t.Fatal(err)
			}
			in = governedOMPInput(in)
			in.StartGuard = func(_ context.Context, start func() error) error {
				tc.mutate(t, &f)
				return start()
			}
			out, err := f.adapter.Run(context.Background(), in)
			if err == nil || out.RunAttempt.ID == "" ||
				out.RunAttempt.Status != runattempt.StatusFailed {
				t.Fatalf("drift result=%+v err=%v", out, err)
			}
			if _, err := os.Stat(f.nativeCapture); !os.IsNotExist(err) {
				t.Fatalf("child started after drift: %v", err)
			}
		})
	}
}

func governedOMPInput(in RunInput) RunInput {
	in.ActionIntentID = "act_fixture"
	in.StartGuard = func(_ context.Context, start func() error) error { return start() }
	in.ExecutionPosture = &runattempt.ExecutionPosture{
		PermissionMode: "prompt_only", PolicyDecisionID: "pol_fixture",
		AuthorityEvidence: append([]runattempt.ExecutionPostureEvidence(nil), ompPromptOnlyPostureEvidence...),
	}
	return in
}

func mustReadOMPFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func appendOMPFile(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n# drift\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertNoOMPAttempts(t *testing.T, home string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(home, "run_attempts", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("refusal minted attempts: %v", matches)
	}
}

func shellQuoteOMP(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
