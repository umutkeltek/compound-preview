package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// testRepoRoot is resolved exactly once, before any test in this package runs. This
// ordering is load-bearing: at least one test in this package calls os.Chdir, so a
// lazily-resolved cwd could end up building from the wrong directory.
var testRepoRoot string

// testBinDir holds the binaries built once per `go test` run by compoundTestBinary and
// compoundMockEngineTestBinary. Removed after m.Run() returns.
var testBinDir string

func TestMain(m *testing.M) {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "testmain: os.Getwd: %v\n", err)
		os.Exit(1)
	}
	testRepoRoot = filepath.Join(wd, "..", "..")

	dir, err := os.MkdirTemp("", "compound-test-bin-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "testmain: create temp bin dir: %v\n", err)
		os.Exit(1)
	}
	testBinDir = dir

	code := m.Run()
	os.RemoveAll(testBinDir)
	os.Exit(code)
}

// isolatedTestUserHome returns a disposable HOME to hand a child process, with Go's
// own telemetry disabled inside it.
//
// A child that runs Go tooling writes counter files under the user config dir
// ($HOME/Library/Application Support/go/telemetry on darwin, $HOME/.config/... on
// linux) and can leave a detached uploader behind. Those writes land AFTER the
// child exits, so they race t.TempDir()'s RemoveAll and fail the PARENT with
// `TempDir RemoveAll cleanup: ... directory not empty` — a red gate produced by
// telemetry bookkeeping, not by anything under test (lived on a loaded machine:
// TestHomeDoctorCapacityFeedWarnings, which asserts only on doctor's JSON).
// Seeding the documented `off` mode file (exactly what `go telemetry off` writes)
// makes the isolated home inert instead of merely private.
func isolatedTestUserHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	disableGoTelemetryIn(t, home)
	return home
}

// disableGoTelemetryIn makes an already-created test HOME inert for Go telemetry.
// Both platform config-dir layouts are seeded so the guarantee does not depend on
// which OS runs the battery.
func disableGoTelemetryIn(t *testing.T, home string) {
	t.Helper()
	for _, configDir := range []string{
		filepath.Join(home, "Library", "Application Support"),
		filepath.Join(home, ".config"),
	} {
		dir := filepath.Join(configDir, "go", "telemetry")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("prepare telemetry-off test home: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "mode"), []byte("off\n"), 0o644); err != nil {
			t.Fatalf("disable go telemetry in test home: %v", err)
		}
	}
}

var (
	compoundBinOnce sync.Once
	compoundBinPath string
	compoundBinErr  error

	compoundMockEngineBinOnce sync.Once
	compoundMockEngineBinPath string
	compoundMockEngineBinErr  error
)

// compoundTestBinary builds ./cmd/compound exactly once per test binary run and returns
// the path to the built executable. It is lazy (behind sync.Once): a `go test -run
// TestOneThing` invocation that never execs the compound binary must not pay the build
// cost. Every caller in this package gets the same binary, replacing what used to be a
// fresh `go run`/`go build` per call site.
func compoundTestBinary(t *testing.T) string {
	t.Helper()
	compoundBinOnce.Do(func() {
		exe := filepath.Join(testBinDir, "compound")
		// -buildvcs=false skips the git status stamping check in checkouts
		// the build does not own, including CI runners and mounted containers.
		// VCS metadata plays no role in these functional tests.
		cmd := exec.Command("go", "build", "-buildvcs=false", "-o", exe, "./cmd/compound")
		cmd.Dir = testRepoRoot
		cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
		out, err := cmd.CombinedOutput()
		if err != nil {
			compoundBinErr = fmt.Errorf("build ./cmd/compound: %w\n%s", err, out)
			return
		}
		compoundBinPath = exe
	})
	if compoundBinErr != nil {
		t.Fatalf("%v", compoundBinErr)
	}
	return compoundBinPath
}

// compoundMockEngineTestBinary builds cmd/compound-mock-engine exactly once
// per test binary run and returns the path to the built executable. The
// build uses an absolute package path resolved via `go list -m`, so it
// works from any cwd (the prior `cmd.Dir = testRepoRoot` form only worked
// when the test binary was started from cmd/compound/; running it from
// anywhere else produced a silent "go: go.mod file not found" failure
// because testRepoRoot = wd/../.. only resolves to the repo root when wd
// is the package directory). Lazy for the same reason as compoundTestBinary.
func compoundMockEngineTestBinary(t *testing.T) string {
	t.Helper()
	compoundMockEngineBinOnce.Do(func() {
		exe := filepath.Join(testBinDir, "compound-mock-engine")
		pkgPath := compoundModuleRoot(t) + string(filepath.Separator) + "cmd" + string(filepath.Separator) + "compound-mock-engine"
		cmd := exec.Command("go", "build", "-buildvcs=false", "-o", exe, pkgPath)
		cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
		out, err := cmd.CombinedOutput()
		if err != nil {
			compoundMockEngineBinErr = fmt.Errorf("build %s: %w\n%s", pkgPath, err, out)
			return
		}
		compoundMockEngineBinPath = exe
	})
	if compoundMockEngineBinErr != nil {
		t.Fatalf("%v", compoundMockEngineBinErr)
	}
	return compoundMockEngineBinPath
}

var (
	moduleRootOnce sync.Once
	moduleRootDir  string
	moduleRootErr  error
)

// compoundModuleRoot returns the absolute path of the Go module root,
// resolved once per test binary run via `go list -m -f {{.Dir}}`. Used by
// helpers that need an absolute package path so `go build` works from
// any cwd.
func compoundModuleRoot(t *testing.T) string {
	t.Helper()
	moduleRootOnce.Do(func() {
		out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
		if err != nil {
			moduleRootErr = fmt.Errorf("go list -m: %w", err)
			return
		}
		moduleRootDir = strings.TrimSpace(string(out))
	})
	if moduleRootErr != nil {
		t.Fatalf("%v", moduleRootErr)
	}
	return moduleRootDir
}

// telemetryOffHome disables Go telemetry inside an existing directory and returns it,
// for call sites whose HOME is a tempdir they already use for other fixtures.
func telemetryOffHome(t *testing.T, home string) string {
	t.Helper()
	disableGoTelemetryIn(t, home)
	return home
}
