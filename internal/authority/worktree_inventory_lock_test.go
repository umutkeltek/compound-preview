package authority

import (
	"strings"
	"testing"
	"time"
)

// A `git worktree lock` is the runtime's own leave-alone claim, and a locked
// worktree can be perfectly contained and clean — a governed delegate fleet
// holds 66 such locks on this repo. Before the lock was read, those rows were
// counted as scratch debt and emitted into the sweep list. `git worktree
// remove` refuses a locked tree, so the obvious way to make an automated sweep
// "work" is `remove -f -f`, which would delete an active fleet's checkouts.
//
// The lock therefore has to be visible in the projection, not merely enforced
// by git: nothing may offer a locked worktree for retirement.
func TestLockedWorktreeIsNeverOfferedForRetirement(t *testing.T) {
	aged := int64((72 * time.Hour).Seconds())
	rows := []WorktreeInventoryRow{
		{
			Path:       "/repo/.claude/worktrees/locked-delegate",
			Class:      WorktreeClassLandedClean,
			Contained:  true,
			DirtyFiles: 0,
			AgeSeconds: &aged,
			Locked:     true,
			LockReason: "active governed delegate",
			NextAction: "retire via compound authority worktree-retire (governed)",
		},
		{
			Path:       "/repo/.claude/worktrees/unlocked-scratch",
			Class:      WorktreeClassLandedClean,
			Contained:  true,
			DirtyFiles: 0,
			AgeSeconds: &aged,
			NextAction: "retire via compound authority worktree-retire (governed)",
		},
	}

	var sweep []string
	debt := annotateScratchWorktreeDebt(rows, &sweep)

	if debt != 1 {
		t.Fatalf("scratch debt = %d, want 1 (the locked row must not be counted)", debt)
	}
	if rows[0].DebtEligible {
		t.Fatal("locked worktree marked debt-eligible")
	}
	if !rows[1].DebtEligible {
		t.Fatal("unlocked aged scratch worktree lost its debt eligibility")
	}
	for _, suggestion := range sweep {
		if strings.Contains(suggestion, "locked-delegate") {
			t.Fatalf("sweep list offers a locked worktree for removal: %q", suggestion)
		}
	}
	if len(sweep) != 1 || !strings.Contains(sweep[0], "unlocked-scratch") {
		t.Fatalf("sweep list = %v, want exactly the unlocked scratch row", sweep)
	}
}

// The lock must also reach the operator-facing next_action, so a human reading
// the row does not reach for -f -f either.
func TestLockedWorktreeNextActionNamesTheLock(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		want   string
	}{
		{"with reason", "active governed delegate", "active governed delegate"},
		{"without reason", "", "no reason recorded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := "leave alone — git worktree lock held" + lockReasonSuffix(tc.reason)
			if !strings.Contains(got, "leave alone") || !strings.Contains(got, tc.want) {
				t.Fatalf("next action = %q, want it to name the lock and %q", got, tc.want)
			}
		})
	}
}
