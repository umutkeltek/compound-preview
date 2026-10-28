package gitrepository

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrepareReferenceTransactionDisablesRepositoryHooks(t *testing.T) {
	repo := t.TempDir()
	runReferenceGit(t, repo, "init", "-b", "main")
	runReferenceGit(t, repo, "config", "user.email", "test@example.invalid")
	runReferenceGit(t, repo, "config", "user.name", "Compound Test")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runReferenceGit(t, repo, "add", "seed.txt")
	runReferenceGit(t, repo, "commit", "-m", "seed")
	tip := referenceGitOutput(t, repo, "rev-parse", "refs/heads/main")

	marker := filepath.Join(t.TempDir(), "reference-hook-ran")
	hooksDir := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := fmt.Sprintf(`#!/bin/sh
if [ ! -e %q ]; then
  printf ran > %q
  git -C %q update-ref refs/heads/reentrant %s
fi
exit 93
`, marker, marker, repo, tip)
	if err := os.WriteFile(filepath.Join(hooksDir, "reference-transaction"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	runReferenceGit(t, repo, "config", "core.hooksPath", hooksDir)

	done := make(chan error, 1)
	go func() {
		tx, err := PrepareReferenceTransaction(repo, []ReferenceExpectation{{Ref: "refs/heads/main", SHA: tip}})
		if err == nil {
			err = tx.Commit()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("verify-only reference transaction ran repository hook: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("verify-only reference transaction deadlocked in repository hook")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("repository reference-transaction hook executed: stat err=%v", err)
	}
	if out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "refs/heads/reentrant").CombinedOutput(); err == nil {
		t.Fatalf("re-entrant hook created a ref: %s", strings.TrimSpace(string(out)))
	}
}

func runReferenceGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
}

func referenceGitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out))
}
