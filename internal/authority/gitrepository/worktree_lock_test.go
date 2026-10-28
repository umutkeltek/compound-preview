package gitrepository

import (
	"path/filepath"
	"strings"
	"testing"
)

// The lock reader is proved against REAL `git worktree lock` output, not a
// hand-built fixture: the whole point of reading git's marker file directly is
// that it stays byte-compatible with what git writes, and a fixture the test
// wrote itself could not fail if git changed its layout.
func TestReadWorktreeLockMatchesRealGitLockAndUnlock(t *testing.T) {
	repo := initSnapshotRepo(t)
	linked := filepath.Join(t.TempDir(), "lane")
	snapshotRunGit(t, repo, "worktree", "add", "-b", "lane", linked, "main")

	before, err := ReadWorktreeLock(linked)
	if err != nil {
		t.Fatalf("read unlocked worktree: %v", err)
	}
	if !before.Linked || before.Locked || before.Reason != "" {
		t.Fatalf("fresh linked worktree = %+v, want linked and unlocked", before)
	}

	const reason = "active governed delegate"
	snapshotRunGit(t, repo, "worktree", "lock", "--reason", reason, linked)
	locked, err := ReadWorktreeLock(linked)
	if err != nil {
		t.Fatalf("read locked worktree: %v", err)
	}
	if !locked.Locked || locked.Reason != reason {
		t.Fatalf("locked worktree = %+v, want locked with reason %q", locked, reason)
	}

	if err := UnlockWorktree(linked); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	after, err := ReadWorktreeLock(linked)
	if err != nil {
		t.Fatalf("read after unlock: %v", err)
	}
	if after.Locked {
		t.Fatalf("worktree still locked after unlock: %+v", after)
	}
	// git itself must agree the tree is releasable now; a marker file removed
	// behind git's back would still show as locked in its own listing.
	if out := snapshotGitOutput(t, repo, "worktree", "list", "--porcelain"); strings.Contains(out, "locked") {
		t.Fatalf("git still reports a locked worktree:\n%s", out)
	}
}

// The main working tree cannot carry a lock. Reporting Linked=false keeps a
// caller from concluding "unlocked, safe to reap" about a path that could
// never have been locked.
func TestReadWorktreeLockReportsMainWorktreeAsUnlockable(t *testing.T) {
	repo := initSnapshotRepo(t)
	state, err := ReadWorktreeLock(repo)
	if err != nil {
		t.Fatalf("read main worktree: %v", err)
	}
	if state.Linked || state.Locked {
		t.Fatalf("main worktree = %+v, want unlinked and unlocked", state)
	}
}

func TestReadWorktreeLockFailsOnNonWorktreePath(t *testing.T) {
	if _, err := ReadWorktreeLock(t.TempDir()); err == nil {
		t.Fatal("reading a directory that is not a worktree must fail, not report unlocked")
	}
	if _, err := ReadWorktreeLock("  "); err == nil {
		t.Fatal("empty worktree path must fail")
	}
}
