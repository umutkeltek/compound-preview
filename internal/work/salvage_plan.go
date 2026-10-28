package work

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SalvagePlanSchemaVersion identifies the shape of a SalvagePlan.
const SalvagePlanSchemaVersion = "compound.work_salvage_plan.v0.1"

// salvageTeethReproofNote is the fixed obligation line every salvage plan
// carries: salvage output is input, never pre-verified work. A stale
// executor's branch may look green in isolation, but every falsifier that
// proved the original slice must be re-proven against the salvaged result
// before it counts as landed (LATEST-110 salvage law).
const salvageTeethReproofNote = "salvage is input, never pre-verified work: every falsifier's teeth (its red-before-green proof) must be re-proven on the salvaged result before any of it counts as landed."

// SalvagePlanOptions names the caller inputs for SalvagePlan. Branch is the
// stale executor branch being evaluated against the envelope's bound
// git_delivery target; nothing else is caller-suppliable, so the plan is
// always computed against the WorkEnvelope's own authority, never a caller
// assertion.
type SalvagePlanOptions struct {
	Branch string
}

// SalvagePlanCommit is one commit true to the branch's actual contribution
// (merge_base..branch), carrying its subject for a human-legible plan.
type SalvagePlanCommit struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
}

// SalvagePlan is the read-only report produced by `compound work
// salvage-plan`. It records NO event, takes NO lease, needs NO policy
// decision, and mutates nothing: it exists to make the LATEST-110 salvage
// law -- true delta is merge_base..branch, not target..branch -- a computed
// fact instead of a claim a stale executor has to get right by hand.
type SalvagePlan struct {
	SchemaVersion string `json:"schema_version"`
	GeneratedAt   string `json:"generated_at"`
	WorkID        string `json:"work_id"`

	RepoPath        string `json:"repo_path"`
	TargetRef       string `json:"target_ref"`
	TargetBranchRef string `json:"target_branch_ref"`
	TargetSHA       string `json:"target_sha"`

	Branch    string `json:"branch"`
	BranchSHA string `json:"branch_sha"`

	MergeBase string `json:"merge_base"`
	// StaleBase is true when merge_base != target tip: the branch forked
	// before the target's current position, so a naive target..branch diff
	// can report deletions for files the branch never touched (they were
	// simply added to target AFTER the fork).
	StaleBase bool `json:"stale_base"`

	// TrueCommits is rev-list --reverse merge_base..branch: the branch's
	// actual contribution, in landing order.
	TrueCommits []SalvagePlanCommit `json:"true_commits"`
	// TrueFiles is diff --name-only merge_base..branch: the files the
	// branch actually changed.
	TrueFiles []string `json:"true_files"`

	// NaiveDeletions is the deletion count from a naive target..branch
	// diff. When StaleBase is true this count is fabricated: see
	// FabricatedDeletionLabel.
	NaiveDeletions int `json:"naive_deletions"`
	// TrueDeletions is the deletion count from the true merge_base..branch
	// diff -- the trustworthy figure.
	TrueDeletions int `json:"true_deletions"`
	// FabricatedDeletionLabel is set only when StaleBase is true. It names
	// NaiveDeletions as fabricated by stale-base comparison so a reader
	// (human or agent) never mistakes it for the branch's real footprint.
	FabricatedDeletionLabel string `json:"fabricated_deletion_label,omitempty"`

	// TeethReproofNote is always present: salvage is input, never
	// pre-verified work.
	TeethReproofNote string `json:"teeth_reproof_note"`
	// SuccessorCommands is the sanctioned successor sequence, copy-pasteable:
	// a fresh worktree off the target tip, then cherry-pick the true commits
	// in order. Never a rebase or merge of the stale branch in place.
	SuccessorCommands []string `json:"successor_commands"`
}

// SalvagePlan reports a stale executor branch's true delta against workID's
// bound git_delivery target. It is read-only: no event, no lease, no policy
// decision, no mutation. Refusals are fail-closed, clear one-line errors --
// there is no soft "status" field to interpret, unlike LandCheck.
func (s *Store) SalvagePlan(workID string, opts SalvagePlanOptions, now time.Time) (SalvagePlan, error) {
	if now.IsZero() {
		now = time.Now()
	}
	workID = strings.TrimSpace(workID)
	if workID == "" {
		return SalvagePlan{}, fmt.Errorf("work id required")
	}
	w, err := s.Get(workID)
	if err != nil {
		return SalvagePlan{}, fmt.Errorf("work %s not found: %w", workID, err)
	}
	if w.GitDelivery == nil {
		return SalvagePlan{}, fmt.Errorf("work %s has no git_delivery contract; salvage-plan requires a bound target repository to compute a true delta against", workID)
	}
	if terminalStates[w.State] {
		return SalvagePlan{}, fmt.Errorf("work %s is %s (terminal); salvage-plan only serves stale in-flight work, not closed work", workID, w.State)
	}
	branch := strings.TrimSpace(opts.Branch)
	if branch == "" {
		return SalvagePlan{}, fmt.Errorf("--branch required")
	}
	if !strings.HasPrefix(branch, "refs/heads/") {
		return SalvagePlan{}, fmt.Errorf("--branch %q must be a local branch ref (refs/heads/<branch>)", branch)
	}

	contract := *w.GitDelivery
	repoPath := contract.TargetWorktreeRoot
	targetRef, targetBranchRef, err := normalizeLandingTargetRef(repoPath, contract.TargetLocalRef)
	if err != nil {
		return SalvagePlan{}, fmt.Errorf("git_delivery.target_local_ref: %w", err)
	}
	targetSHA, err := runSalvageGitOutput(repoPath, "rev-parse", "--verify", targetBranchRef+"^{commit}")
	if err != nil {
		return SalvagePlan{}, fmt.Errorf("resolve target branch %s: %w", targetRef, err)
	}
	if !validLandingObjectID(targetSHA) {
		return SalvagePlan{}, fmt.Errorf("target branch %s did not resolve to a full Git object id", targetRef)
	}
	branchSHA, err := runSalvageGitOutput(repoPath, "rev-parse", "--verify", branch+"^{commit}")
	if err != nil {
		return SalvagePlan{}, fmt.Errorf("branch %s does not resolve in %s: %w", branch, repoPath, err)
	}
	if !validLandingObjectID(branchSHA) {
		return SalvagePlan{}, fmt.Errorf("branch %s did not resolve to a full Git object id", branch)
	}

	mergeBase, err := runSalvageGitOutput(repoPath, "merge-base", targetBranchRef, branch)
	if err != nil {
		return SalvagePlan{}, fmt.Errorf("compute merge-base of %s and %s: %w", targetRef, branch, err)
	}
	if !validLandingObjectID(mergeBase) {
		return SalvagePlan{}, fmt.Errorf("merge-base of %s and %s did not resolve to a full Git object id", targetRef, branch)
	}

	trueCommits, err := salvageTrueCommits(repoPath, mergeBase, branch)
	if err != nil {
		return SalvagePlan{}, err
	}

	trueFilesOut, err := runSalvageGitOutput(repoPath, "diff", "--name-only", mergeBase+".."+branch)
	if err != nil {
		return SalvagePlan{}, fmt.Errorf("list true files %s..%s: %w", mergeBase, branch, err)
	}
	trueFiles := salvageNonEmptyLines(trueFilesOut)

	naiveShortstat, err := runSalvageGitOutput(repoPath, "diff", "--shortstat", targetBranchRef+".."+branch)
	if err != nil {
		return SalvagePlan{}, fmt.Errorf("compute naive shortstat %s..%s: %w", targetRef, branch, err)
	}
	trueShortstat, err := runSalvageGitOutput(repoPath, "diff", "--shortstat", mergeBase+".."+branch)
	if err != nil {
		return SalvagePlan{}, fmt.Errorf("compute true shortstat %s..%s: %w", mergeBase, branch, err)
	}
	naiveDeletions := parseShortstatDeletions(naiveShortstat)
	trueDeletions := parseShortstatDeletions(trueShortstat)

	staleBase := mergeBase != targetSHA
	var fabricatedLabel string
	if staleBase {
		fabricatedLabel = fmt.Sprintf(
			"naive_deletions (%d, from a %s..%s comparison) is fabricated by stale-base comparison: merge_base %s does not equal target tip %s, so target-only files the branch never touched are counted as branch deletions; true_deletions (%d, from merge_base..branch) is the trustworthy count",
			naiveDeletions, targetRef, branch, mergeBase, targetSHA, trueDeletions,
		)
	}

	plan := SalvagePlan{
		SchemaVersion:           SalvagePlanSchemaVersion,
		GeneratedAt:             now.UTC().Format(time.RFC3339Nano),
		WorkID:                  w.ID,
		RepoPath:                repoPath,
		TargetRef:               targetRef,
		TargetBranchRef:         targetBranchRef,
		TargetSHA:               targetSHA,
		Branch:                  branch,
		BranchSHA:               branchSHA,
		MergeBase:               mergeBase,
		StaleBase:               staleBase,
		TrueCommits:             trueCommits,
		TrueFiles:               trueFiles,
		NaiveDeletions:          naiveDeletions,
		TrueDeletions:           trueDeletions,
		FabricatedDeletionLabel: fabricatedLabel,
		TeethReproofNote:        salvageTeethReproofNote,
		SuccessorCommands:       salvageSuccessorCommands(w.ID, targetSHA, trueCommits),
	}
	return plan, nil
}

// salvageTrueCommits resolves rev-list --reverse merge_base..branch (the
// branch's true contribution, oldest first) and attaches each commit's
// subject for a human-legible plan.
func salvageTrueCommits(repoPath, mergeBase, branch string) ([]SalvagePlanCommit, error) {
	merges, err := runSalvageGitOutput(repoPath, "rev-list", "--min-parents=2", "--max-count=1", mergeBase+".."+branch)
	if err != nil {
		return nil, fmt.Errorf("inspect salvage merge ancestry: %w", err)
	}
	if strings.TrimSpace(merges) != "" {
		return nil, fmt.Errorf("salvage delta contains a merge commit; preserve the source branch and review its parent graph before replay")
	}
	out, err := runSalvageGitOutput(repoPath, "rev-list", "--reverse", mergeBase+".."+branch)
	if err != nil {
		return nil, fmt.Errorf("list true commits %s..%s: %w", mergeBase, branch, err)
	}
	shas := salvageNonEmptyLines(out)
	commits := make([]SalvagePlanCommit, 0, len(shas))
	for _, sha := range shas {
		subject, err := runSalvageGitOutput(repoPath, "log", "-1", "--format=%s", sha)
		if err != nil {
			return nil, fmt.Errorf("read subject for true commit %s: %w", sha, err)
		}
		commits = append(commits, SalvagePlanCommit{SHA: sha, Subject: subject})
	}
	return commits, nil
}

// salvageSuccessorCommands is the sanctioned successor sequence: a fresh
// named worktree branch off the target tip, then cherry-pick the true commits
// in order. The bare `git worktree add <path> <sha>` form produces a DETACHED
// worktree, so a salvaging agent's commits land on no branch and are easy to
// lose a second time -- the -b form is what makes the recovery durable.
// `land/<work_id>` is derived from the WorkEnvelope id purely so the name is
// deterministic across replays and cannot collide across envelopes; it is NOT
// a git_delivery ref (that contract carries an operator-supplied
// TargetLocalRef). The plan never creates the branch; it only renders the
// command.
func salvageSuccessorCommands(workID, targetSHA string, trueCommits []SalvagePlanCommit) []string {
	commands := []string{
		fmt.Sprintf("git worktree add -b land/%s <fresh-worktree-path> %s", workID, targetSHA),
	}
	if len(trueCommits) == 0 {
		commands = append(commands, "# no true commits to cherry-pick: the branch contributes nothing beyond merge_base")
		return commands
	}
	shas := make([]string, len(trueCommits))
	for i, c := range trueCommits {
		shas[i] = c.SHA
	}
	commands = append(commands, "git cherry-pick "+strings.Join(shas, " "))
	return commands
}

// salvageNonEmptyLines splits Git's newline-delimited stdout into non-empty
// lines. runSalvageGitOutput already trims the overall output, so an empty
// result (no commits/files) yields a nil slice rather than one blank entry.
func salvageNonEmptyLines(out string) []string {
	if strings.TrimSpace(out) == "" {
		return nil
	}
	lines := strings.Split(out, "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

// shortstatDeletionsRe extracts the deletions count from a `git diff
// --shortstat` summary line, e.g. " 3 files changed, 10 insertions(+), 4
// deletions(-)". Absent (no deletions in the range) parses as 0.
var shortstatDeletionsRe = regexp.MustCompile(`(\d+) deletions?\(-\)`)

func parseShortstatDeletions(shortstat string) int {
	m := shortstatDeletionsRe.FindStringSubmatch(shortstat)
	if len(m) != 2 {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// runSalvageGitOutput runs Git against repoPath with repository hooks
// disabled (-c core.hooksPath=/dev/null). salvage-plan is read-only and must
// never trigger a pre-commit/post-checkout/etc. hook as a side effect of
// merely computing a plan. This is deliberately a separate helper from
// runLandingGitOutput (land_check.go): that helper's raw, hook-enabled
// behavior is unchanged for landing.
func runSalvageGitOutput(repoPath string, args ...string) (string, error) {
	gitArgs := append([]string{"-c", "core.hooksPath=/dev/null", "-C", repoPath}, args...)
	cmd := exec.Command("git", gitArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
