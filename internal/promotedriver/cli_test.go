package promotedriver

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureCLI runs fn with os.Stdout/os.Stderr swapped for pipes and returns
// (captured stderr, fn's return).
func captureCLI(fn func() int) (string, int) {
	oldOut, oldErr := os.Stdout, os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout, os.Stderr = wOut, wErr
	code := fn()
	wOut.Close()
	wErr.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	errBytes, _ := io.ReadAll(rErr)
	outBytes, _ := io.ReadAll(rOut)
	_ = outBytes
	return string(errBytes), code
}

// TestRunCLIRejectsContradictoryConfirmAndDryRun: --confirm --dry-run used
// to quietly mean dry-run, letting an automation script believe it applied a
// pass it did not. Contradictory mutation flags are a usage error. The check
// fires before home resolution, so the test has no filesystem side effects.
func TestRunCLIRejectsContradictoryConfirmAndDryRun(t *testing.T) {
	stderr, code := captureCLI(func() int { return RunCLI([]string{"--confirm", "--dry-run"}) })
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "contradictory") {
		t.Fatalf("stderr must name the contradiction: %s", stderr)
	}
}

// TestRunCLIAcceptsDryRunAlone pins that the rejection is about the PAIR,
// not either flag individually. --dry-run alone must plan (no mutation), so
// the exit must not be the usage error.
func TestRunCLIAcceptsDryRunAlone(t *testing.T) {
	t.Setenv("COMPOUND_HOME", t.TempDir())
	stderr, code := captureCLI(func() int { return RunCLI([]string{"--dry-run"}) })
	if code == 2 {
		t.Fatalf("--dry-run alone must not be usage-rejected: %s", stderr)
	}
}

func TestRunCLIRequiresRealWorkIdentityForApply(t *testing.T) {
	stderr, code := captureCLI(func() int { return RunCLI([]string{"--confirm"}) })
	if code != 2 || !strings.Contains(stderr, "--work") {
		t.Fatalf("confirm without work identity = exit %d stderr %q, want usage refusal", code, stderr)
	}
}

func TestRunCLIRejectsRegistryApplyAsPlanOnlyBeforeRuntimeAdmission(t *testing.T) {
	root := t.TempDir()
	t.Setenv("COMPOUND_HOME", filepath.Join(root, "absent-home"))
	stderr, code := captureCLI(func() int {
		return RunCLI([]string{"--confirm", "--work", "work_example", "--registry", "products.example/*.yaml"})
	})
	if code != 2 || !strings.Contains(stderr, "plan-only") {
		t.Fatalf("registry apply = exit %d stderr %q, want plan-only usage refusal", code, stderr)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("registry apply refusal created runtime state: %+v", entries)
	}
}
