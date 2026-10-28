package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLaneSafetyCheckWarningOnlyExitsZero(t *testing.T) {
	repo := newLaneSafetyFixture(t, false)
	out, err := runLaneSafetyFixture(repo)
	if err != nil {
		t.Fatalf("lane safety warning-only run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "WARN:") {
		t.Fatalf("warning-only output missing WARN verdict:\n%s", out)
	}
}

func TestLaneSafetyCheckBuildFailureExitsNonZero(t *testing.T) {
	repo := newLaneSafetyFixture(t, true)
	out, err := runLaneSafetyFixture(repo)
	if err == nil {
		t.Fatalf("lane safety build failure unexpectedly succeeded:\n%s", out)
	}
	if !strings.Contains(out, "UNSAFE:") {
		t.Fatalf("build-failure output missing UNSAFE verdict:\n%s", out)
	}
}

func newLaneSafetyFixture(t *testing.T, failBuild bool) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("lane safety script requires bash")
	}

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(filepath.Join(repoRoot, "scripts", "lane-safety-check.sh"))
	if err != nil {
		t.Fatal(err)
	}

	fixture := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fixture, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(fixture, "cmd", "compound"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "scripts", "lane-safety-check.sh"), script, 0o755); err != nil {
		t.Fatal(err)
	}
	warningSource := "package fixture\n// filepath.Join(home, \"ledger\")\n"
	if err := os.WriteFile(filepath.Join(fixture, "warning.go"), []byte(warningSource), 0o644); err != nil {
		t.Fatal(err)
	}
	// A gitignored worktree container holding a SECOND copy of the same match. This is the
	// shape that made the old `grep -r .` report every hit once per worktree; the check must
	// see the tracked copy exactly once.
	nested := filepath.Join(fixture, ".claude", "worktrees", "other-lane")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "warning.go"), []byte(warningSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, ".gitignore"), []byte(".claude/\nbin/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The scan is scoped to TRACKED files, so the fixture must be a real repo with real
	// tracked content -- a bare directory tree would not exercise the discriminator at all.
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "test"},
		{"add", "warning.go", ".gitignore"},
		{"commit", "-qm", "fixture"},
	} {
		cmd := exec.Command("git", append([]string{"-C", fixture}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git unavailable (%v): %s", err, out)
		}
	}

	binDir := filepath.Join(fixture, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exitCode := "0"
	if failBuild {
		exitCode = "1"
	}
	fakeGo := "#!/usr/bin/env bash\nexit " + exitCode + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "go"), []byte(fakeGo), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return fixture
}

func runLaneSafetyFixture(repo string) (string, error) {
	cmd := exec.Command("bash", filepath.Join(repo, "scripts", "lane-safety-check.sh"), repo)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// The regression this scoping fixes: a repo carrying agent worktrees holds many near-identical
// copies of its own tree, and `grep -r .` reported every hit once per copy -- 36 times on the
// machine that surfaced it, which made the output unreadable and the check slow. Scoping to the
// tracked set collapses that back to one. If this fails with a count above one, the script has
// gone back to walking the working directory instead of asking git.
func TestLaneSafetyCheckReportsTrackedHitExactlyOnce(t *testing.T) {
	repo := newLaneSafetyFixture(t, false)
	out, err := runLaneSafetyFixture(repo)
	if err != nil {
		t.Fatalf("lane safety run: %v\n%s", err, out)
	}
	if got := strings.Count(out, "warning.go"); got != 1 {
		t.Fatalf("tracked hit reported %d times, want exactly 1 (worktree copies must not be scanned):\n%s", got, out)
	}
	if strings.Contains(out, filepath.Join(".claude", "worktrees")) {
		t.Fatalf("a gitignored worktree copy was scanned:\n%s", out)
	}
}

// Outside a git worktree the scan cannot scope itself, and reporting "no conflicts" there would
// be a silent green over files it never read. It must refuse instead.
func TestLaneSafetyCheckRefusesOutsideGitWorktree(t *testing.T) {
	repo := newLaneSafetyFixture(t, false)
	if err := os.RemoveAll(filepath.Join(repo, ".git")); err != nil {
		t.Fatal(err)
	}
	out, err := runLaneSafetyFixture(repo)
	if err == nil {
		t.Fatalf("scan outside a git worktree must fail, got success:\n%s", out)
	}
	if !strings.Contains(out, "cannot scope the scan to tracked files") {
		t.Fatalf("refusal must name the reason:\n%s", out)
	}
}
