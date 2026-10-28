package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/home"
	"compound/internal/reviewartifact"
)

// The guard is proved through the REAL binary, not cmdHome: refusing is an
// os.Exit(2), and an exit code is exactly the part an in-process call cannot
// observe. A caller that reads the exit code is the caller this guard protects.
func TestHomeArchiveHistoryBareInvocationRefusesWithoutArchiving(t *testing.T) {
	t.Parallel()
	bin := compoundTestBinary(t)
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	seedArchivableReview(t, homePath, "pv_bare_guard")
	hot := filepath.Join(homePath, "reviews", "panel-verdicts", "pv_bare_guard.md")
	if _, err := os.Stat(hot); err != nil {
		t.Fatalf("fixture is not hot to begin with: %v", err)
	}

	// 1. Bare invocation -- what an agent probing for usage types. It must
	//    refuse, not sweep every kind.
	out, code := runArchiveHistory(t, bin, homePath)
	if code != 2 {
		t.Fatalf("bare archive-history exit = %d, want 2 (refusal); output:\n%s", code, out)
	}
	if !strings.Contains(out, "usage: compound home archive-history") || !strings.Contains(out, "--kind") {
		t.Fatalf("bare archive-history did not print usage:\n%s", out)
	}
	if _, err := os.Stat(hot); err != nil {
		t.Fatalf("bare archive-history moved a review artifact anyway: %v", err)
	}
	if index, err := home.LoadOperationalArchiveIndex(homePath); err != nil || len(index) != 0 {
		t.Fatalf("bare archive-history wrote archive entries: %+v (err %v)", index, err)
	}

	// 2. --dry-run semantics are unchanged: a bare preview still previews the
	//    full sweep, because seeing what WOULD move is how you pick a --kind.
	out, code = runArchiveHistory(t, bin, homePath, "--dry-run")
	if code != 0 {
		t.Fatalf("bare --dry-run exit = %d, want 0; output:\n%s", code, out)
	}
	if !strings.Contains(out, "# home archive-history") || !strings.Contains(out, "review") {
		t.Fatalf("bare --dry-run stopped previewing the sweep:\n%s", out)
	}
	if _, err := os.Stat(hot); err != nil {
		t.Fatalf("dry run moved a review artifact: %v", err)
	}

	// 3. The explicit path still works -- a guard that broke the real command
	//    would be a worse footgun than the one it replaced.
	out, code = runArchiveHistory(t, bin, homePath, "--kind", "review")
	if code != 0 {
		t.Fatalf("--kind review exit = %d, want 0; output:\n%s", code, out)
	}
	if _, err := os.Stat(hot); !os.IsNotExist(err) {
		t.Fatalf("--kind review did not archive the review artifact (stat err %v)", err)
	}
}

func runArchiveHistory(t *testing.T, bin, homePath string, extra ...string) (string, int) {
	t.Helper()
	args := append([]string{"home", "archive-history"}, extra...)
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run %v: %v\n%s", args, err, out)
		}
		code = exitErr.ExitCode()
	}
	return string(out), code
}

func seedArchivableReview(t *testing.T, homePath, id string) {
	t.Helper()
	if _, err := reviewartifact.Write(homePath, "panel-verdicts", id, map[string]any{
		"id":     id,
		"status": "recorded",
	}, " ", "# Verdict\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewartifact.WriteJSON(homePath, "panel-verdicts", id+".reviewed.json", map[string]any{
		"id":          "pvr_" + id,
		"status":      "recorded",
		"reviewed_by": "compound-test",
	}, " "); err != nil {
		t.Fatal(err)
	}
}
