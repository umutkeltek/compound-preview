package console

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const canonicalCapability64Hex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// mutation refusal without explicit home

func TestValidateMutationLaunchEnvironmentRefusesWithoutExplicitHome(t *testing.T) {
	t.Setenv("COMPOUND_HOME", "")
	t.Setenv(CapabilityEnv, canonicalCapability64Hex)
	if err := ValidateMutationLaunchEnvironment(true); err == nil {
		t.Fatalf("ValidateMutationLaunchEnvironment(true) accepted mutation without explicit COMPOUND_HOME; want refusal")
	}
}

// exact 64-hex capability validation

func TestValidateMutationLaunchEnvironmentRequiresExactly64Hex(t *testing.T) {
	cases := []struct {
		name string
		cap  string
		want bool
	}{
		{name: "canonical", cap: canonicalCapability64Hex, want: true},
		{name: "short", cap: "abc", want: false},
		{name: "non-hex same length", cap: strings.Repeat("Z", 64), want: false},
		{name: "sixty-forthree letters", cap: strings.Repeat("a", 63), want: false},
		{name: "sixtyfive letters", cap: strings.Repeat("a", 65), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(CapabilityEnv, tc.cap)
			t.Setenv("COMPOUND_HOME", "/tmp/compound-test-home")
			err := ValidateMutationLaunchEnvironment(true)
			if tc.want && err != nil {
				t.Fatalf("ValidateMutationLaunchEnvironment accepted canonical capability and rejected only this case: %v", err)
			}
			if !tc.want && err == nil {
				t.Fatalf("ValidateMutationLaunchEnvironment accepted invalid capability %q", tc.cap)
			}
		})
	}
}

func TestValidateMutationLaunchEnvironmentSkippedWhenMutationDisabled(t *testing.T) {
	t.Setenv(CapabilityEnv, "")
	t.Setenv("COMPOUND_HOME", "")
	if err := ValidateMutationLaunchEnvironment(false); err != nil {
		t.Fatalf("ValidateMutationLaunchEnvironment(false) returned %v; want nil when mutation is off", err)
	}
}

// build-mode mutation refusal

func TestBuildLifecycleLaunchPlanRefusesMutationInBuildMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("COMPOUND_HOME", home)
	if _, err := BuildLifecycleLaunchPlan(LifecycleBuild, "", true, false, true); err == nil {
		t.Fatalf("BuildLifecycleLaunchPlan(build, mutations=true) accepted mutation in build mode; want refusal")
	}
}

func TestValidateProductionBuildAcceptsMissingArtifactsAsRefusal(t *testing.T) {
	if err := ValidateProductionBuild(t.TempDir()); err == nil {
		t.Fatalf("ValidateProductionBuild accepted empty console dir; want refusal for missing BUILD_ID")
	}
}

// TestValidateProductionBuildAcceptsRealNextBuildLayout is the layout-fidelity
// falsifier: the gate MUST pass against the exact tree `next build` emits
// under the production distDir (console/next.config.ts sets `distDir:
// .next/${buildKind}`). If a future regression points the gate at a path
// `next build` does not produce (e.g. `.next/standalone`, which only exists
// when the standalone output feature is enabled), this test surfaces it
// before the launchd console service can crash-loop on the missing BUILD_ID.
func TestValidateProductionBuildAcceptsRealNextBuildLayout(t *testing.T) {
	consoleDir := t.TempDir()
	productionDir := filepath.Join(consoleDir, ".next", "production")
	if err := os.MkdirAll(productionDir, 0o755); err != nil {
		t.Fatalf("create production dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(productionDir, "BUILD_ID"), []byte("test-build-id\n"), 0o644); err != nil {
		t.Fatalf("write BUILD_ID: %v", err)
	}
	for _, name := range []string{"prerender-manifest.json", "required-server-files.json", "routes-manifest.json"} {
		if err := os.WriteFile(filepath.Join(productionDir, name), []byte("{}"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := ValidateProductionBuild(consoleDir); err != nil {
		t.Fatalf("ValidateProductionBuild refused a real next build layout (.next/production); want pass: %v", err)
	}
}

func TestValidateProductionBuildRejectsBuildArtifactsInWrongLocation(t *testing.T) {
	consoleDir := t.TempDir()
	wrongDir := filepath.Join(consoleDir, ".next", "standalone")
	if err := os.MkdirAll(wrongDir, 0o755); err != nil {
		t.Fatalf("create wrong dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wrongDir, "BUILD_ID"), []byte("test-build-id\n"), 0o644); err != nil {
		t.Fatalf("write BUILD_ID: %v", err)
	}
	if err := ValidateProductionBuild(consoleDir); err == nil {
		t.Fatalf("ValidateProductionBuild passed against a tree placed at .next/standalone; want refusal because the production build emits .next/production")
	}
}

// capability non-disclosure in JSON

func TestLaunchPlanJSONOpaqueCapabilityRequirementNotValue(t *testing.T) {
	home := t.TempDir()
	t.Setenv("COMPOUND_HOME", home)
	t.Setenv(CapabilityEnv, canonicalCapability64Hex)
	plan, err := BuildLifecycleLaunchPlan(LifecycleDev, "", true, false, true)
	if err != nil {
		t.Fatalf("BuildLifecycleLaunchPlan dev+mutations: %v", err)
	}
	if !plan.CapabilityRequired {
		t.Fatalf("BuildLifecycleLaunchPlan should set CapabilityRequired=true when mutations enabled; got %+v", plan)
	}
	rendered, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("json.Marshal launch plan: %v", err)
	}
	if strings.Contains(string(rendered), CapabilityEnv) {
		t.Fatalf("LaunchPlan JSON leaked env name %q: %s", CapabilityEnv, rendered)
	}
	if strings.Contains(string(rendered), canonicalCapability64Hex) {
		t.Fatalf("LaunchPlan JSON leaked capability value %q: %s", canonicalCapability64Hex, rendered)
	}
}

func TestLaunchPlanJSONStripsCapabilityFromHandConstructedEnv(t *testing.T) {
	plan := LaunchPlan{
		SchemaVersion: LaunchPlanSchemaVersion,
		Env: map[string]string{
			CapabilityEnv: canonicalCapability64Hex,
			MutationsEnv:  "1",
		},
	}
	rendered, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("json.Marshal hand-constructed launch plan: %v", err)
	}
	if strings.Contains(string(rendered), CapabilityEnv) || strings.Contains(string(rendered), canonicalCapability64Hex) {
		t.Fatalf("LaunchPlan JSON leaked hand-constructed capability material: %s", rendered)
	}
}

func TestBuildLaunchPlanMutationsOffKeepsCapabilityRequiredFalse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("COMPOUND_HOME", home)
	t.Setenv(CapabilityEnv, "")
	plan, err := BuildLaunchPlan("", false, true)
	if err != nil {
		t.Fatalf("BuildLaunchPlan mutations=off: %v", err)
	}
	if plan.CapabilityRequired {
		t.Fatalf("BuildLaunchPlan must not set CapabilityRequired when mutations disabled; got %+v", plan)
	}
}

// sanitized launch environment

func TestLaunchEnvStripsOperatorShellAmbientAndAddsOnlyPostureKeys(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("COMPOUND_CONSOLE_CAPABILITY", canonicalCapability64Hex)
	t.Setenv("COMPOUND_CONSOLE_SESSION_TOKEN", "supersecret")
	t.Setenv("COMPOUND_CONSOLE_MUTATIONS", "1")
	t.Setenv("COMPOUND_CONSOLE_PRIVATE_FALLBACKS", "1")
	t.Setenv("EDITOR", "vim")
	t.Setenv("COMPOUND_CONSOLE_LEGACY_TEST", "should-surface-non-blocked")
	defer os.Unsetenv("COMPOUND_CONSOLE_CAPABILITY")
	defer os.Unsetenv("COMPOUND_CONSOLE_SESSION_TOKEN")
	defer os.Unsetenv("COMPOUND_CONSOLE_MUTATIONS")
	defer os.Unsetenv("COMPOUND_CONSOLE_PRIVATE_FALLBACKS")
	defer os.Unsetenv("EDITOR")
	defer os.Unsetenv("COMPOUND_CONSOLE_LEGACY_TEST")

	home := t.TempDir()
	t.Setenv("COMPOUND_HOME", home)
	plan, err := BuildLifecycleLaunchPlan(LifecycleDev, "", true, false, true)
	if err != nil {
		t.Fatalf("BuildLifecycleLaunchPlan dev+mutations: %v", err)
	}
	env := LaunchEnv(plan)

	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{
		"COMPOUND_CONSOLE_SESSION_TOKEN=supersecret",
		"COMPOUND_CONSOLE_PRIVATE_FALLBACKS=1",
	} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("LaunchEnv leaked operator ambient: %q", forbidden)
		}
	}

	mustHave := []string{
		"COMPOUND_HOME=" + home,
		"COMPOUND_CONSOLE_MUTATIONS=1",
		"COMPOUND_CONSOLE_PRIVATE_FALLBACKS=0",
	}
	for _, want := range mustHave {
		if !strings.Contains(joined, want) {
			t.Fatalf("LaunchEnv missing required key %q in: %s", want, joined)
		}
	}

	// mutation dev lifecycle must inject the validated capability into the trusted
	// operator-local Next server process so it can authenticate
	// `x-compound-console-capability` mutation requests.
	if !strings.Contains(joined, CapabilityEnv+"="+canonicalCapability64Hex) {
		t.Fatalf("LaunchEnv dev+mutations must inject %s once for the trusted Next server auth; got: %s", CapabilityEnv, joined)
	}
}

func TestLaunchEnvPreservesNonPostureAmbient(t *testing.T) {
	t.Setenv("EDITOR", "vim")
	defer os.Unsetenv("EDITOR")
	home := t.TempDir()
	t.Setenv("COMPOUND_HOME", home)
	plan, err := BuildLifecycleLaunchPlan(LifecycleDev, "", false, false, true)
	if err != nil {
		t.Fatalf("BuildLifecycleLaunchPlan dev: %v", err)
	}
	env := LaunchEnv(plan)
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "EDITOR=vim") {
		t.Fatalf("LaunchEnv dropped non-posture ambient EDITOR; got: %s", joined)
	}
}

func TestLaunchEnvHandConstructedPlanFailsClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("COMPOUND_HOME", home)
	t.Setenv(CapabilityEnv, canonicalCapability64Hex)
	defer os.Unsetenv(CapabilityEnv)
	plan := LaunchPlan{
		SchemaVersion:      LaunchPlanSchemaVersion,
		GeneratedAt:        "2026-08-20T00:00:00Z",
		Mode:               "operator",
		Lifecycle:          LifecycleDev,
		ConsoleDir:         home,
		Env:                map[string]string{"COMPOUND_HOME": home, MutationsEnv: "0", PrivateFallbacksEnv: "0"},
		MutationsEnabled:   true,
		PrivateFallbacks:   false,
		CapabilityRequired: true,
		DryRun:             true,
	}
	env := LaunchEnv(plan)
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, MutationsEnv+"=0") {
		t.Fatalf("LaunchEnv must force an unvalidated plan to projection-only posture; got: %s", joined)
	}
	if strings.Contains(joined, CapabilityEnv+"=") {
		t.Fatalf("LaunchEnv must omit %s from an unvalidated plan; got: %s", CapabilityEnv, joined)
	}
}

func TestLaunchEnvServeMutationCarriesCapability(t *testing.T) {
	home := t.TempDir()
	t.Setenv("COMPOUND_HOME", home)
	t.Setenv(CapabilityEnv, canonicalCapability64Hex)
	plan, err := BuildLifecycleLaunchPlan(LifecycleServe, "", true, false, true)
	if err != nil {
		t.Fatalf("BuildLifecycleLaunchPlan serve+mutations: %v", err)
	}
	joined := strings.Join(LaunchEnv(plan), "\n")
	if !strings.Contains(joined, MutationsEnv+"=1") {
		t.Fatalf("LaunchEnv serve+mutations missing enabled posture: %s", joined)
	}
	if !strings.Contains(joined, CapabilityEnv+"="+canonicalCapability64Hex) {
		t.Fatalf("LaunchEnv serve+mutations missing server capability: %s", joined)
	}
}

func TestLaunchEnvBuildModeNeverCarriesCapability(t *testing.T) {
	t.Setenv(CapabilityEnv, canonicalCapability64Hex)
	home := t.TempDir()
	t.Setenv("COMPOUND_HOME", home)
	plan, err := BuildLifecycleLaunchPlan(LifecycleBuild, "", false, false, true)
	if err != nil {
		t.Fatalf("BuildLifecycleLaunchPlan build no-mutations: %v", err)
	}
	env := LaunchEnv(plan)
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, CapabilityEnv) {
		t.Fatalf("Build-mode LaunchEnv must never advertise the out-of-band capability: %s", joined)
	}
}

// existing-behavior preservation (cmp_flags refactor moves these exports without changing semantics)

func TestHomeShortHashMatchesConsoleInstancePinContract(t *testing.T) {
	home := "/tmp/compound-home-pinned"
	got := HomeShortHash(home)
	if len(got) != 7 {
		t.Fatalf("HomeShortHash %q is %d hex chars; want 7", got, len(got))
	}
	id := InstancePinLabel(home)
	if !strings.Contains(id, got) {
		t.Fatalf("InstancePinLabel %q missing short hash %q", id, got)
	}
	if !strings.Contains(id, home) {
		t.Fatalf("InstancePinLabel %q missing home path %q", id, home)
	}
}

func TestResolveCompoundBinPrefersStablePathOverTemp(t *testing.T) {
	stable := "/usr/local/bin/compound"
	temp := "/tmp/compound-dashboard"
	got := ResolveCompoundBin(temp, func(name string) (string, error) { return stable, nil })
	if got != stable {
		t.Fatalf("ResolveCompoundBin(temp=%s, lookPath=%s) = %q; want %q", temp, stable, got, stable)
	}
}

func TestExecutableIsTemporaryRejectsStableBinary(t *testing.T) {
	if ExecutableIsTemporary("/usr/local/bin/compound") {
		t.Fatalf("ExecutableIsTemporary(stable) reported true")
	}
	if !ExecutableIsTemporary("/tmp/compound-dashboard") {
		t.Fatalf("ExecutableIsTemporary(/tmp/compound-dashboard) reported false")
	}
}

func TestValidateDirRejectsMissingPackageJSON(t *testing.T) {
	if _, err := ValidateDir(t.TempDir()); err == nil {
		t.Fatalf("ValidateDir accepted empty dir; want refusal")
	}
}

func TestResolveDirRejectsUnknownTree(t *testing.T) {
	t.Setenv("COMPOUND_CONSOLE_DIR", "")
	restoreCwd, err := os.MkdirTemp("", "compound-test-resolve-empty")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	defer os.RemoveAll(restoreCwd)
	old, _ := os.Getwd()
	if err := os.Chdir(restoreCwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() { _ = os.Chdir(old) }()
	if _, err := ResolveDir(""); err == nil {
		t.Fatalf("ResolveDir accepted unknown tree; want refusal")
	}
}

func TestResolveDirWalksUpToFindConsole(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "console"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "console", "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("package.json: %v", err)
	}
	child := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("mkdir child: %v", err)
	}
	old, _ := os.Getwd()
	if err := os.Chdir(child); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() { _ = os.Chdir(old) }()
	t.Setenv("COMPOUND_CONSOLE_DIR", "")
	got, err := ResolveDir("")
	if err != nil {
		t.Fatalf("ResolveDir: %v", err)
	}
	wantRaw := filepath.Join(root, "console")
	wantAbs, absErr := filepath.Abs(wantRaw)
	if absErr != nil {
		t.Fatalf("abs(want): %v", absErr)
	}
	wantResolved, err := filepath.EvalSymlinks(wantAbs)
	if err != nil {
		t.Fatalf("EvalSymlinks(want): %v", err)
	}
	gotResolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("EvalSymlinks(got): %v", err)
	}
	if filepath.Clean(gotResolved) != filepath.Clean(wantResolved) {
		t.Fatalf("ResolveDir = %q (resolved %q); want %q (resolved %q)", got, gotResolved, wantAbs, wantResolved)
	}
}
