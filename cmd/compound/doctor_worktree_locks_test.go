package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
	"compound/internal/work"
)

// doctorLockFixedNow keeps the seeded attempts off wall-clock so the join is
// never judged against a moving staleness window.
func doctorLockFixedNow() time.Time { return time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC) }

// buildWorktreeLockRows is the doctor join over REAL git state: a real linked
// worktree, really locked by git, whose only delegate run attempt is terminal.
// A synthetic lock marker would prove nothing about whether doctor can see a
// lock git actually took.
func TestDoctorFlagsWorktreeLockWhoseDelegateIsTerminal(t *testing.T) {
	t.Parallel()
	repo, _, _ := initLandingCLIRepo(t)
	runLandingCLIGit(t, repo, "switch", "main")

	stale := filepath.Join(t.TempDir(), "stale-lane")
	live := filepath.Join(t.TempDir(), "live-lane")
	runLandingCLIGit(t, repo, "worktree", "add", "-b", "stale-lane", stale, "main")
	runLandingCLIGit(t, repo, "worktree", "add", "-b", "live-lane", live, "main")
	runLandingCLIGit(t, repo, "worktree", "lock", "--reason", "active governed delegate", stale)
	runLandingCLIGit(t, repo, "worktree", "lock", "--reason", "active governed delegate", live)

	home := t.TempDir()
	raStore := runattempt.Open(home)
	seedAttempt(t, raStore, "work_stale000000", stale, true)
	seedAttempt(t, raStore, "work_live0000000", live, false)

	rows, staleCount := buildWorktreeLockRows(home)
	if staleCount != 1 {
		t.Fatalf("stale lock count = %d, want 1; rows=%+v", staleCount, rows)
	}
	var staleRow, liveRow *work.DelegateWorktreeLockRow
	for i := range rows {
		switch rows[i].Workspace {
		case stale:
			staleRow = &rows[i]
		case live:
			liveRow = &rows[i]
		}
	}
	if staleRow == nil || !staleRow.Stale || staleRow.Reason != "active governed delegate" {
		t.Fatalf("stale row = %+v, want flagged with the git lock reason", staleRow)
	}
	if liveRow == nil || liveRow.Stale {
		t.Fatalf("live row = %+v, want present and NOT flagged", liveRow)
	}

	var out bytes.Buffer
	printWorktreeLocks(&out, doctorReport{WorktreeLocks: rows, WorktreeLocksStale: staleCount})
	rendered := out.String()
	if !strings.Contains(rendered, "STALE (owning delegate terminal)") || !strings.Contains(rendered, stale) {
		t.Fatalf("doctor output does not name the leaked lock:\n%s", rendered)
	}
	if !strings.Contains(rendered, "held (delegate live)") {
		t.Fatalf("doctor output does not distinguish a lock still in use:\n%s", rendered)
	}
	if !strings.Contains(rendered, work.ExperimentalDelegateMutationsFlag) || !strings.Contains(rendered, "unsupported in v0.1") {
		t.Fatalf("doctor advertised an ungated delegate teardown:\n%s", rendered)
	}
}

func TestDoctorWorktreeLockSectionIsQuietWithoutLocks(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printWorktreeLocks(&out, doctorReport{})
	if !strings.Contains(out.String(), "no locked delegate workspaces") {
		t.Fatalf("empty lock section output:\n%s", out.String())
	}
}

func seedAttempt(t *testing.T, store *runattempt.Store, workID, workspace string, terminal bool) {
	t.Helper()
	ra, err := store.Mint(workID, "codex", "claude", doctorLockFixedNow())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetWorkspace(ra.ID, workspace); err != nil {
		t.Fatal(err)
	}
	if terminal {
		if _, err := store.Complete(ra.ID, runattempt.StatusAbandoned, doctorLockFixedNow()); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := store.MarkRunning(ra.ID, doctorLockFixedNow()); err != nil {
		t.Fatal(err)
	}
}
