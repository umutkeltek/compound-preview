package main

import (
	"fmt"
	"io"

	"compound/internal/work"
)

// buildWorktreeLockRows answers the question a stale `git worktree lock` makes
// unanswerable from anywhere else: is this lock still protecting a running
// delegate, or is it a leftover that will refuse `git worktree remove` forever?
//
// Both halves of the answer live in compound: the lock state on disk and the
// RunAttempt records that say who ran in that workspace and whether they are
// terminal. Reported, never repaired — doctor writes nothing, and releasing a
// lock is the experimental teardown path's job (`compound work delegate
// sweep --experimental-delegate-mutations`), not a health command's.
func buildWorktreeLockRows(homePath string) ([]work.DelegateWorktreeLockRow, int) {
	// Doctor writes nothing, so its read of the lock table must not materialize
	// work/ or events/ either (see work.OpenReadOnly).
	store, err := work.OpenReadOnly(homePath)
	if err != nil {
		return nil, 0
	}
	rows, err := store.DelegateWorktreeLockReport(work.NewGitWorktreeLockReaper())
	if err != nil {
		return nil, 0
	}
	stale := 0
	for _, row := range rows {
		if row.Stale {
			stale++
		}
	}
	return rows, stale
}

// printWorktreeLocks renders the locked delegate workspaces. A row whose
// delegates are all terminal is the actionable one; a row still holding live
// attempts is shown too, so "locked" is never silently equated with "leaked".
func printWorktreeLocks(stdout io.Writer, rep doctorReport) {
	fmt.Fprintln(stdout, "\n## worktree locks (delegate teardown)")
	if len(rep.WorktreeLocks) == 0 {
		fmt.Fprintln(stdout, "- no locked delegate workspaces")
		return
	}
	for _, row := range rep.WorktreeLocks {
		state := "held (delegate live)"
		if row.Stale {
			state = "STALE (owning delegate terminal)"
		}
		fmt.Fprintf(stdout, "- %-32s %s attempts=%d live=%d\n", state, row.Workspace, row.Attempts, row.LiveAttempts)
		if row.Reason != "" {
			fmt.Fprintf(stdout, "    reason: %s\n", row.Reason)
		}
	}
	if rep.WorktreeLocksStale > 0 {
		fmt.Fprintf(stdout, "  -> %d lock(s) protect nobody; experimental `compound work delegate sweep <work_id> --experimental-delegate-mutations` reaps a delegate's own lock (unsupported in v0.1)\n", rep.WorktreeLocksStale)
	}
}
