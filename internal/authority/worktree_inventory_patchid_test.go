package authority

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Main moves by rebase and squash, so a branch main already absorbed reads
// arbitrarily many commits "ahead" by SHA while holding zero unlanded patches.
// Classifying on the raw `git rev-list --count main..HEAD` therefore labelled
// fully-landed trees stalled_unlanded, which emptied the retirement plan of the
// very trees that were safe to retire. These tests build real repositories where
// the SHA count and the patch-id count DISAGREE, so an implementation that reads
// the SHA count cannot pass them.

// W-rebased: main absorbed the commit under a different SHA. ahead-by-SHA is 1,
// unlanded-by-patch-id is 0 -> landed_clean.
func TestWorktreeInventoryLandedByPatchIDAfterRebase(t *testing.T) {
	fix := newPatchIDInventoryFixture(t)

	row := patchIDRowForBranch(t, buildPatchIDInventory(t, fix, ExecWorktreeRetirementGit{}), "lane-rebased")

	if row.AheadCommits != 1 {
		t.Fatalf("fixture is not exercising the defect: ahead-by-SHA = %d, want 1 (main must NOT contain this SHA)", row.AheadCommits)
	}
	if row.Contained {
		t.Fatalf("fixture is not exercising the defect: HEAD reads as contained in main, so the SHA count and the patch-id count agree")
	}
	if row.UnlandedCommits != 0 {
		t.Fatalf("unlanded_commits = %d, want 0 (the patch is already in main under a different SHA); row=%+v", row.UnlandedCommits, row)
	}
	if row.LandedByPatchID != 1 {
		t.Fatalf("landed_by_patch_id = %d, want 1; row=%+v", row.LandedByPatchID, row)
	}
	if row.Class != WorktreeClassLandedClean {
		t.Fatalf("class = %q, want landed_clean — a rebased-and-landed tree is being held as unlanded work; row=%+v", row.Class, row)
	}
	if !strings.Contains(row.Reason, "patch-id") {
		t.Fatalf("reason must say the evidence is patch-id, got %q", row.Reason)
	}
	if !strings.Contains(row.NextAction, "worktree-retire") {
		t.Fatalf("next_action = %q, want the governed retire path", row.NextAction)
	}
}

// W-unlanded: a commit whose patch is nowhere in main. The data-loss guard —
// this must never read as landed_clean.
func TestWorktreeInventoryUnlandedPatchStaysStalled(t *testing.T) {
	fix := newPatchIDInventoryFixture(t)

	row := patchIDRowForBranch(t, buildPatchIDInventory(t, fix, ExecWorktreeRetirementGit{}), "lane-unlanded")

	if row.Class != WorktreeClassStalledUnlanded {
		t.Fatalf("class = %q, want stalled_unlanded; row=%+v", row.Class, row)
	}
	if row.Class == WorktreeClassLandedClean {
		t.Fatal("data-loss guard breached: a commit absent from main by patch-id read as landed_clean")
	}
	if row.UnlandedCommits != 1 {
		t.Fatalf("unlanded_commits = %d, want 1; row=%+v", row.UnlandedCommits, row)
	}
	if row.LandedByPatchID != 0 {
		t.Fatalf("landed_by_patch_id = %d, want 0; row=%+v", row.LandedByPatchID, row)
	}
	if !strings.Contains(row.Reason, "patch is not in main") {
		t.Fatalf("reason must quote the patch-id count, got %q", row.Reason)
	}
}

// W-dirty: zero unlanded commits, but uncommitted files exist. Dirtiness alone
// still holds the tree, because uncommitted bytes are in no commit at all and so
// no patch-id can vouch for them.
func TestWorktreeInventoryDirtyTreeStaysStalledWithZeroUnlanded(t *testing.T) {
	fix := newPatchIDInventoryFixture(t)

	row := patchIDRowForBranch(t, buildPatchIDInventory(t, fix, ExecWorktreeRetirementGit{}), "lane-dirty")

	if row.UnlandedCommits != 0 {
		t.Fatalf("fixture is not exercising the dirty-only path: unlanded_commits = %d, want 0; row=%+v", row.UnlandedCommits, row)
	}
	if row.DirtyFiles < 1 {
		t.Fatalf("fixture is not dirty: %+v", row)
	}
	if row.Class != WorktreeClassStalledUnlanded {
		t.Fatalf("class = %q, want stalled_unlanded — uncommitted work is held by no patch; row=%+v", row.Class, row)
	}
	if !strings.Contains(row.Reason, "uncommitted file(s)") {
		t.Fatalf("reason must name the uncommitted files, got %q", row.Reason)
	}
}

// A failed patch-id read must fail CLOSED. The fixture used here is the one that
// reads landed_clean when `git cherry` succeeds, so an implementation that
// swallows the error and treats the count as zero would hand a retire action for
// a tree whose contents it could not read.
func TestWorktreeInventoryFailsClosedWhenPatchIDReadFails(t *testing.T) {
	fix := newPatchIDInventoryFixture(t)

	row := patchIDRowForBranch(t, buildPatchIDInventory(t, fix, cherryFailingGit{}), "lane-rebased")

	if row.Class == WorktreeClassLandedClean {
		t.Fatalf("fail-open breach: patch-id read failed yet the tree reads landed_clean and would be retired; row=%+v", row)
	}
	if row.Class != WorktreeClassUnknown {
		t.Fatalf("class = %q, want unknown when the patch-id count is unreadable; row=%+v", row.Class, row)
	}
	if !strings.Contains(row.Reason, "git cherry") {
		t.Fatalf("reason must name the unreadable command, got %q", row.Reason)
	}
	if strings.Contains(row.NextAction, "worktree-retire") {
		t.Fatalf("next_action must not offer retirement on unreadable facts, got %q", row.NextAction)
	}
}

// cherryFailingGit runs real Git for everything except the patch-id read, so the
// row reaches the class decision with every OTHER fact readable.
type cherryFailingGit struct{ real ExecWorktreeRetirementGit }

func (g cherryFailingGit) Run(ctx context.Context, repo string, args ...string) (string, error) {
	if len(args) > 0 && args[0] == "cherry" {
		return "", errors.New("fatal: bad revision (injected)")
	}
	return g.real.Run(ctx, repo, args...)
}

type patchIDInventoryFixture struct {
	repo string
}

func buildPatchIDInventory(t *testing.T, fix patchIDInventoryFixture, git WorktreeRetirementGit) WorktreeInventory {
	t.Helper()
	inv, err := BuildWorktreeInventory(context.Background(), WorktreeInventoryDependencies{
		Git: git, Liveness: inventoryFakeLiveness{},
	}, fix.repo, WorktreeInventoryOptions{Now: inventoryFutureNow()})
	if err != nil {
		t.Fatalf("build inventory: %v", err)
	}
	return inv
}

func patchIDRowForBranch(t *testing.T, inv WorktreeInventory, branch string) WorktreeInventoryRow {
	t.Helper()
	for _, row := range inv.Rows {
		if row.Branch == branch && !row.IsMain {
			return row
		}
	}
	t.Fatalf("no row for branch %q", branch)
	return WorktreeInventoryRow{}
}

// newPatchIDInventoryFixture builds a repository whose main branch absorbed one
// lane's commit under a DIFFERENT SHA (cherry-pick + amend), which is what
// rebase/squash landing produces.
func newPatchIDInventoryFixture(t *testing.T) patchIDInventoryFixture {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	retirementRunGit(t, repo, "init", "-b", "main")
	retirementRunGit(t, repo, "config", "user.email", "test@example.invalid")
	retirementRunGit(t, repo, "config", "user.name", "Compound Test")
	writePatchIDFile(t, repo, "seed.txt", "seed\n")
	retirementRunGit(t, repo, "add", "seed.txt")
	retirementRunGit(t, repo, "commit", "-m", "commit A")

	// lane-rebased: commit B on the lane, then land B onto main under a new SHA.
	laneRebased := filepath.Join(root, "wt-rebased")
	retirementRunGit(t, repo, "worktree", "add", "-b", "lane-rebased", laneRebased, "main")
	writePatchIDFile(t, laneRebased, "feature.txt", "feature\n")
	retirementRunGit(t, laneRebased, "add", "feature.txt")
	retirementRunGit(t, laneRebased, "commit", "-m", "commit B")
	commitB := retirementGitOutput(t, laneRebased, "rev-parse", "HEAD")

	// lane-unlanded: a commit main never absorbed, in any form.
	laneUnlanded := filepath.Join(root, "wt-unlanded")
	retirementRunGit(t, repo, "worktree", "add", "-b", "lane-unlanded", laneUnlanded, "main")
	writePatchIDFile(t, laneUnlanded, "unique.txt", "unique\n")
	retirementRunGit(t, laneUnlanded, "add", "unique.txt")
	retirementRunGit(t, laneUnlanded, "commit", "-m", "commit C (never landed)")

	// lane-dirty: no commits of its own, but uncommitted bytes on disk.
	laneDirty := filepath.Join(root, "wt-dirty")
	retirementRunGit(t, repo, "worktree", "add", "-b", "lane-dirty", laneDirty, "main")
	writePatchIDFile(t, laneDirty, "wip.txt", "work in progress\n")

	// Land B onto main with a different SHA: same patch, new commit object.
	retirementRunGit(t, repo, "cherry-pick", commitB)
	retirementRunGit(t, repo, "commit", "--amend", "-m", "commit B (landed by rebase)")
	mainTip := retirementGitOutput(t, repo, "rev-parse", "refs/heads/main")
	if mainTip == commitB {
		t.Fatalf("fixture failed to change the SHA: main tip %s equals lane commit", mainTip)
	}
	return patchIDInventoryFixture{repo: repo}
}

func writePatchIDFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWorktreeInventoryPreservesUncontainedMergeContent(t *testing.T) {
	fix := newMergeOnlyRetirementGitFixture(t)
	row := patchIDRowForBranch(t, buildPatchIDInventory(t, patchIDInventoryFixture{repo: fix.repo}, ExecWorktreeRetirementGit{}), "candidate")
	if row.Contained || row.Class == WorktreeClassLandedClean || strings.Contains(row.NextAction, "worktree-retire") {
		t.Fatalf("uncontained merge-only content must be preserved, not offered for retirement: %+v", row)
	}
}

func TestWorktreeInventoryFailsClosedWhenMergeReadFails(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unreadable", true: "malformed"}[malformed], func(t *testing.T) {
			fix := newPatchIDInventoryFixture(t)
			row := patchIDRowForBranch(t, buildPatchIDInventory(t, fix, mergeReadFailingGit{malformed: malformed}), "lane-rebased")
			if row.Class != WorktreeClassUnknown || strings.Contains(row.NextAction, "worktree-retire") {
				t.Fatalf("unverified merge ancestry must refuse patch-equivalence retirement: %+v", row)
			}
		})
	}
}

type mergeReadFailingGit struct {
	malformed bool
	real      ExecWorktreeRetirementGit
}

func (g mergeReadFailingGit) Run(ctx context.Context, repo string, args ...string) (string, error) {
	if len(args) > 1 && args[0] == "rev-list" && strings.Contains(strings.Join(args, " "), "--min-parents=2") {
		if g.malformed {
			return "not-a-commit\n", nil
		}
		return "", errors.New("merge ancestry unavailable (injected)")
	}
	return g.real.Run(ctx, repo, args...)
}
