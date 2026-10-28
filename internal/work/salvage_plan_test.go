package work

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// F1: a branch that forked at an old base, while the target advanced with
// NEW files after the fork, must report a true delta that excludes the
// post-fork target files and labels the naive target..branch deletion count
// as fabricated.
func TestSalvagePlanTrueDeltaExcludesPostForkFilesAndLabelsFabricatedDeletions(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	repo, base := sharedLandingBaseRepo(t)

	// Branch forks at base, then makes its own true change: it deletes
	// README.md (a real deletion the branch is responsible for) and adds
	// its own file.
	landingGit(t, repo, "switch", "-c", "stale-branch")
	landingGit(t, repo, "rm", "README.md")
	writeLandingFile(t, filepath.Join(repo, "branch-work.txt"), "branch work\n")
	landingGit(t, repo, "add", "branch-work.txt")
	landingGit(t, repo, "commit", "-m", "branch: drop README, add branch-work")
	branchTip := landingGitOutput(t, repo, "rev-parse", "HEAD")

	// Target (main) advances AFTER the fork with two new files the branch
	// never saw and never touched.
	landingGit(t, repo, "switch", "main")
	writeLandingFile(t, filepath.Join(repo, "post-fork-1.txt"), "post fork 1\n")
	writeLandingFile(t, filepath.Join(repo, "post-fork-2.txt"), "post fork 2\n")
	landingGit(t, repo, "add", "post-fork-1.txt", "post-fork-2.txt")
	landingGit(t, repo, "commit", "-m", "main: two post-fork files")
	targetTip := landingGitOutput(t, repo, "rev-parse", "HEAD")

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "salvage plan reports the true delta",
		GitDelivery:      localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := store.SalvagePlan(w.ID, SalvagePlanOptions{Branch: "refs/heads/stale-branch"}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("SalvagePlan: %v", err)
	}

	if plan.MergeBase != base {
		t.Fatalf("merge_base = %s, want fork point %s", plan.MergeBase, base)
	}
	if !plan.StaleBase {
		t.Fatalf("stale_base = false, want true (merge_base %s != target tip %s)", plan.MergeBase, targetTip)
	}
	if plan.TargetSHA != targetTip {
		t.Fatalf("target_sha = %s, want %s", plan.TargetSHA, targetTip)
	}
	if plan.BranchSHA != branchTip {
		t.Fatalf("branch_sha = %s, want %s", plan.BranchSHA, branchTip)
	}

	for _, f := range plan.TrueFiles {
		if f == "post-fork-1.txt" || f == "post-fork-2.txt" {
			t.Fatalf("true_files must exclude post-fork target files the branch never touched, got %v", plan.TrueFiles)
		}
	}
	if !stringSliceContains(plan.TrueFiles, "README.md") || !stringSliceContains(plan.TrueFiles, "branch-work.txt") {
		t.Fatalf("true_files must include the branch's own changes, got %v", plan.TrueFiles)
	}

	if plan.NaiveDeletions <= plan.TrueDeletions {
		t.Fatalf("naive_deletions (%d) must exceed true_deletions (%d): naive counts the post-fork files as deleted too", plan.NaiveDeletions, plan.TrueDeletions)
	}
	if plan.TrueDeletions != 1 {
		t.Fatalf("true_deletions = %d, want 1 (README.md, the branch's real deletion)", plan.TrueDeletions)
	}
	if !strings.Contains(plan.FabricatedDeletionLabel, "fabricated by stale-base comparison") {
		t.Fatalf("fabricated_deletion_label = %q, want the fabricated-by-stale-base phrase", plan.FabricatedDeletionLabel)
	}

	if len(plan.TrueCommits) != 1 || plan.TrueCommits[0].SHA != branchTip {
		t.Fatalf("true_commits = %+v, want exactly the one branch commit %s", plan.TrueCommits, branchTip)
	}
}

// A branch that forks exactly at the target's current tip (no target
// advance since the fork) must NOT be labeled stale/fabricated.
func TestSalvagePlanFreshBaseCarriesNoFabricatedLabel(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	repo, base := sharedLandingBaseRepo(t)
	landingGit(t, repo, "switch", "-c", "fresh-branch")
	writeLandingFile(t, filepath.Join(repo, "fresh.txt"), "fresh\n")
	landingGit(t, repo, "add", "fresh.txt")
	landingGit(t, repo, "commit", "-m", "fresh work")
	landingGit(t, repo, "switch", "main")

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "fresh branch salvage plan",
		GitDelivery:      localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := store.SalvagePlan(w.ID, SalvagePlanOptions{Branch: "refs/heads/fresh-branch"}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("SalvagePlan: %v", err)
	}
	if plan.StaleBase {
		t.Fatalf("stale_base = true, want false: merge_base %s == target tip %s", plan.MergeBase, plan.TargetSHA)
	}
	if plan.FabricatedDeletionLabel != "" {
		t.Fatalf("fabricated_deletion_label = %q, want empty for a fresh (non-stale) base", plan.FabricatedDeletionLabel)
	}
	if plan.NaiveDeletions != plan.TrueDeletions {
		t.Fatalf("naive_deletions (%d) must equal true_deletions (%d) when the base is fresh", plan.NaiveDeletions, plan.TrueDeletions)
	}
}

// F2: a missing or malformed --branch refuses closed, with no plan produced.
func TestSalvagePlanRefusesMissingOrMalformedBranch(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	repo, base := sharedLandingBaseRepo(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "branch validation",
		GitDelivery:      localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	if plan, err := store.SalvagePlan(w.ID, SalvagePlanOptions{Branch: ""}, now); err == nil {
		t.Fatalf("empty --branch must refuse, got plan %+v", plan)
	} else if !strings.Contains(err.Error(), "--branch required") {
		t.Fatalf("empty --branch error = %v, want it to name the missing flag", err)
	}

	if plan, err := store.SalvagePlan(w.ID, SalvagePlanOptions{Branch: "stale-branch"}, now); err == nil {
		t.Fatalf("non refs/heads/* --branch must refuse, got plan %+v", plan)
	} else if !strings.Contains(err.Error(), "refs/heads/") {
		t.Fatalf("malformed --branch error = %v, want it to name the refs/heads/ requirement", err)
	}

	if plan, err := store.SalvagePlan(w.ID, SalvagePlanOptions{Branch: "refs/heads/does-not-exist"}, now); err == nil {
		t.Fatalf("branch that does not resolve must refuse, got plan %+v", plan)
	} else if !strings.Contains(err.Error(), "does not resolve") {
		t.Fatalf("unresolved --branch error = %v, want it to say the branch does not resolve", err)
	}
}

// F3: an envelope with no git_delivery contract refuses, naming the gap.
func TestSalvagePlanRefusesEnvelopeWithoutGitDelivery(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "no delivery contract bound",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := store.SalvagePlan(w.ID, SalvagePlanOptions{Branch: "refs/heads/anything"}, now)
	if err == nil {
		t.Fatalf("work without git_delivery must refuse, got plan %+v", plan)
	}
	if !strings.Contains(err.Error(), "git_delivery") {
		t.Fatalf("error = %v, want it to name the missing git_delivery gap", err)
	}
}

// F4: a terminal envelope refuses.
func TestSalvagePlanRefusesTerminalEnvelope(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	repo, base := sharedLandingBaseRepo(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "terminal work is refused",
		GitDelivery:      localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "codex", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	closed, err := store.Close(w.ID, CloseInput{
		Outcome:  "cancelled",
		MoveType: "audit_only",
		Actor:    "codex",
		Summary:  "cancelled before delivery",
	}, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if closed.Work.State != "cancelled" {
		t.Fatalf("fixture setup: work state = %s, want cancelled", closed.Work.State)
	}

	plan, err := store.SalvagePlan(w.ID, SalvagePlanOptions{Branch: "refs/heads/anything"}, now.Add(3*time.Minute))
	if err == nil {
		t.Fatalf("terminal work must refuse salvage-plan, got plan %+v", plan)
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("error = %v, want it to name the terminal state", err)
	}
}

// F5: salvage-plan is read-only. A successful run must not change a single
// byte anywhere under the store home.
func TestSalvagePlanIsZeroMutation(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	repo, base := sharedLandingBaseRepo(t)
	landingGit(t, repo, "switch", "-c", "stale-branch")
	writeLandingFile(t, filepath.Join(repo, "branch-work.txt"), "branch work\n")
	landingGit(t, repo, "add", "branch-work.txt")
	landingGit(t, repo, "commit", "-m", "branch work")
	landingGit(t, repo, "switch", "main")

	home := t.TempDir()
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "salvage-plan never mutates the home",
		GitDelivery:      localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	before := hashTree(t, home)
	if _, err := store.SalvagePlan(w.ID, SalvagePlanOptions{Branch: "refs/heads/stale-branch"}, now.Add(time.Minute)); err != nil {
		t.Fatalf("SalvagePlan: %v", err)
	}
	after := hashTree(t, home)
	if before != after {
		t.Fatalf("salvage-plan mutated the store home: before=%s after=%s", before, after)
	}
}

// F6: the teeth re-proof obligation and the sanctioned successor sequence
// are always present, both in the Go struct (which is what --json marshals)
// and are non-empty strings a human-output renderer can print directly.
func TestSalvagePlanAlwaysCarriesTeethNoteAndSuccessorSequence(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	repo, base := sharedLandingBaseRepo(t)
	landingGit(t, repo, "switch", "-c", "stale-branch")
	writeLandingFile(t, filepath.Join(repo, "branch-work.txt"), "branch work\n")
	landingGit(t, repo, "add", "branch-work.txt")
	landingGit(t, repo, "commit", "-m", "branch work")
	branchTip := landingGitOutput(t, repo, "rev-parse", "HEAD")
	landingGit(t, repo, "switch", "main")
	targetTip := landingGitOutput(t, repo, "rev-parse", "HEAD")

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "successor sequence is copy-pasteable",
		GitDelivery:      localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := store.SalvagePlan(w.ID, SalvagePlanOptions{Branch: "refs/heads/stale-branch"}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("SalvagePlan: %v", err)
	}

	if !strings.Contains(plan.TeethReproofNote, "never pre-verified work") {
		t.Fatalf("teeth_reproof_note = %q, want the salvage-is-input obligation line", plan.TeethReproofNote)
	}
	if len(plan.SuccessorCommands) != 2 {
		t.Fatalf("successor_commands = %v, want exactly 2 commands (worktree add, cherry-pick)", plan.SuccessorCommands)
	}
	wantSuccessorBranch := "land/" + w.ID
	wantWorktreeAdd := "git worktree add -b " + wantSuccessorBranch + " <fresh-worktree-path> " + targetTip
	if plan.SuccessorCommands[0] != wantWorktreeAdd {
		t.Fatalf("successor_commands[0] = %q, want %q", plan.SuccessorCommands[0], wantWorktreeAdd)
	}
	replayed, err := store.SalvagePlan(w.ID, SalvagePlanOptions{Branch: "refs/heads/stale-branch"}, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("SalvagePlan replay: %v", err)
	}
	if replayed.SuccessorCommands[0] != wantWorktreeAdd {
		t.Fatalf("successor branch is not deterministic: first=%q replay=%q", plan.SuccessorCommands[0], replayed.SuccessorCommands[0])
	}
	wantCherryPick := "git cherry-pick " + branchTip
	if plan.SuccessorCommands[1] != wantCherryPick {
		t.Fatalf("successor_commands[1] = %q, want %q", plan.SuccessorCommands[1], wantCherryPick)
	}

	// The JSON shape --json prints is exactly this struct marshaled: prove
	// both load-bearing fields survive the round trip under their wire names.
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["teeth_reproof_note"]; !ok {
		t.Fatalf("JSON output missing teeth_reproof_note: %s", raw)
	}
	if _, ok := decoded["successor_commands"]; !ok {
		t.Fatalf("JSON output missing successor_commands: %s", raw)
	}
}

// A branch with no commits beyond merge_base (nothing to cherry-pick) still
// gets a complete, non-empty successor sequence -- never a bare worktree-add
// with no explanation of why there is nothing to pick.
func TestSalvagePlanEmptyTrueDeltaStillNamesSuccessorStep(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	repo, base := sharedLandingBaseRepo(t)
	landingGit(t, repo, "branch", "empty-branch")

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "empty true delta",
		GitDelivery:      localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := store.SalvagePlan(w.ID, SalvagePlanOptions{Branch: "refs/heads/empty-branch"}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("SalvagePlan: %v", err)
	}
	if len(plan.TrueCommits) != 0 {
		t.Fatalf("true_commits = %+v, want none", plan.TrueCommits)
	}
	if len(plan.SuccessorCommands) != 2 || !strings.Contains(plan.SuccessorCommands[1], "no true commits") {
		t.Fatalf("successor_commands = %v, want a worktree-add plus an explicit nothing-to-cherry-pick note", plan.SuccessorCommands)
	}
	wantWorktreeAdd := "git worktree add -b land/" + w.ID + " <fresh-worktree-path> " + plan.TargetSHA
	if plan.SuccessorCommands[0] != wantWorktreeAdd {
		t.Fatalf("successor_commands[0] = %q, want named-branch command %q", plan.SuccessorCommands[0], wantWorktreeAdd)
	}
}

func stringSliceContains(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

// hashTree deterministically hashes every regular file under root (path +
// mode + content), so two calls detect any byte-level change anywhere in the
// tree regardless of walk order.
func hashTree(t *testing.T, root string) string {
	t.Helper()
	type entry struct {
		path string
		sum  [32]byte
	}
	var entries []entry
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, entry{path: rel, sum: sha256.Sum256(data)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e.path))
		h.Write(e.sum[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestSalvagePlanRefusesMergeReplayAndPreservesSource(t *testing.T) {
	repo, base := sharedLandingBaseRepo(t)
	landingGit(t, repo, "switch", "-c", "salvage-left", base)
	writeLandingFile(t, filepath.Join(repo, "left.txt"), "left\n")
	landingGit(t, repo, "add", "left.txt")
	landingGit(t, repo, "commit", "-m", "left")
	left := landingGitOutput(t, repo, "rev-parse", "HEAD")
	landingGit(t, repo, "switch", "-c", "salvage-right", base)
	writeLandingFile(t, filepath.Join(repo, "right.txt"), "right\n")
	landingGit(t, repo, "add", "right.txt")
	landingGit(t, repo, "commit", "-m", "right")
	right := landingGitOutput(t, repo, "rev-parse", "HEAD")
	landingGit(t, repo, "switch", "-c", "salvage-integration", left)
	landingGit(t, repo, "merge", "--no-ff", "-m", "reviewed integration", right)
	tip := landingGitOutput(t, repo, "rev-parse", "HEAD")
	landingGit(t, repo, "switch", "main")
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	w, err := store.Create(CreateInput{
		Owner: "codex", SuccessPredicate: "retain reviewed merge source",
		GitDelivery: localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.workPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := store.SalvagePlan(w.ID, SalvagePlanOptions{Branch: "refs/heads/salvage-integration"}, now.Add(time.Second))
	if err == nil || !strings.Contains(err.Error(), "merge") || len(plan.SuccessorCommands) != 0 {
		t.Fatalf("merge replay must refuse without blind cherry-pick commands: plan=%+v err=%v", plan, err)
	}
	after, readErr := os.ReadFile(store.workPath(w.ID))
	if readErr != nil || string(before) != string(after) {
		t.Fatalf("read-only refusal changed Work: err=%v", readErr)
	}
	if got := landingGitOutput(t, repo, "rev-parse", "refs/heads/salvage-integration"); got != tip {
		t.Fatalf("refusal moved source: got %s want %s", got, tip)
	}
	if got := landingGitOutput(t, repo, "rev-parse", "main"); got != base {
		t.Fatalf("refusal moved main: got %s want %s", got, base)
	}
}
