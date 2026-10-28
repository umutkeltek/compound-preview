package authority

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/policy"
	"compound/internal/reviewartifact"
)

func TestSourceControlReviewPlanRecordsReviewWithoutMutatingRepo(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	writeFile(t, filepath.Join(repo, "scratch.txt"), "scratch\n")

	before := gitStatusShort(t, repo)
	snap, _, _, _, err := ImportGitSnapshot(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildSourceControlReviewPlan(snap, "work_dirty_gate", time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != SourceControlReviewPlanSchemaVersion ||
		plan.Status != "review_required" ||
		plan.WorkID != "work_dirty_gate" ||
		plan.ChangedPaths != 2 ||
		plan.ReviewDraft.Verification == "" {
		t.Fatalf("bad source-control review plan: %+v", plan)
	}
	home := t.TempDir()
	paths, err := RecordSourceControlReviewPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	review, err := os.ReadFile(paths.ReviewPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(review)
	for _, want := range []string{"does not stage", "Required Evidence", "work_dirty_gate"} {
		if !strings.Contains(text, want) {
			t.Fatalf("review missing %q:\n%s", want, text)
		}
	}
	if _, err := os.Stat(paths.PlanPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("dirty-plan must not promote claims, stat err=%v", err)
	}
	after := gitStatusShort(t, repo)
	if before != after {
		t.Fatalf("repo status mutated\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestRecordSourceControlReviewAnnotatesQueue(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	writeFile(t, filepath.Join(repo, "scratch.txt"), "scratch\n")
	before := gitStatusShort(t, repo)
	snap, _, _, _, err := ImportGitSnapshot(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildSourceControlReviewPlan(snap, "work_dirty_gate", time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	paths, err := RecordSourceControlReviewPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: SourceControlCleanupReviewArea}, time.Date(2026, 6, 29, 12, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateSourceControlCleanupQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.ReviewRequired != 1 || queue.Items[0].Readiness != "needs_review" || len(queue.Items[0].Actions) != 0 {
		t.Fatalf("unedited source-control review queue = %+v", queue)
	}
	reviewYAML := fmt.Sprintf(`schema_version: %s
id: %s
plan_id: %s
repo_id: %q
decision: parked
rationale: "only scratch work remains parked outside this change"
confidence: 0.9
next_action: "leave untracked scratch parked until owner classifies it"
evidence_refs:
 - %q
`, SourceControlReviewSchemaVersion, plan.ReviewDraft.ReviewID, plan.ID, plan.RepoID, plan.EvidenceRefs[0])
	if err := os.WriteFile(paths.ReviewYAMLPath, []byte(reviewYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	queue, err = reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: SourceControlCleanupReviewArea}, time.Date(2026, 6, 29, 12, 2, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateSourceControlCleanupQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.ReviewRequired != 1 || queue.Items[0].Readiness != "ready_to_record" || len(queue.Items[0].Actions) != 1 {
		t.Fatalf("edited source-control review queue = %+v", queue)
	}
	expectedSourceDigest := sourceControlExpectedDigestArg(t, queue.Items[0].Actions[0])
	review, err := LoadSourceControlReview(paths.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	record, err := RecordSourceControlReview(home, review, paths.ReviewYAMLPath, time.Date(2026, 6, 29, 12, 3, 0, 0, time.UTC), expectedSourceDigest)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "recorded" || record.Decision != "parked" || record.ReviewPath == "" {
		t.Fatalf("bad source-control review record: %+v", record)
	}
	queue, err = reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: SourceControlCleanupReviewArea}, time.Date(2026, 6, 29, 12, 4, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateSourceControlCleanupQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.OK != 1 || queue.Counts.ReviewRequired != 0 || queue.Items[0].Status != "recorded" || queue.Items[0].Readiness != "review_recorded" {
		t.Fatalf("recorded source-control review queue = %+v", queue)
	}
	after := gitStatusShort(t, repo)
	if before != after {
		t.Fatalf("source-control review mutated repo\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// The plan pins review_id == plan_id, so joining the pair produced a doubled
// stem no sidecar reader stats. The recorded disposition must land on the
// sidecar stem its own core item owns, or the review can never be drained.
func TestRecordSourceControlReviewWritesSidecarStemDisposition(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	snapshot, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildSourceControlReviewPlan(snapshot, "work_review_sidecar_stem", fixedSourceControlReviewTime)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	paths, err := RecordSourceControlReviewPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	review := writeValidSourceControlReview(t, paths.ReviewYAMLPath, plan)
	report, err := RecordSourceControlReview(home, review, paths.ReviewYAMLPath, fixedSourceControlReviewTime.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(paths.ReviewYAMLPath)
	wantPath := filepath.Join(dir, plan.ID+".reviewed.json")
	if filepath.Clean(report.ReviewPath) != filepath.Clean(wantPath) {
		t.Fatalf("recorded review path = %q, want sidecar stem %q", report.ReviewPath, wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("recorded disposition is not on the sidecar stem: %v", err)
	}
	doubled := filepath.Join(dir, plan.ID+"."+plan.ReviewDraft.ReviewID+".reviewed.json")
	if _, err := os.Stat(doubled); err == nil {
		t.Fatalf("recorded disposition still written at the doubled stem %q", doubled)
	}
}

// The producer minted a fresh scr_ item for every new snapshot of the same repo
// path and never retired the previous one, so a single root accumulated hundreds
// of open reviews that all describe the same question. A newer snapshot must
// supersede the older open item for that path instead of stacking a second one,
// and the superseded item must leave the operator queue.
func TestRecordSourceControlReviewPlanSupersedesStaleOpenPlanForSameRepoPath(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	first, firstPaths := recordSourceControlPlanForRepo(t, home, repo, "work_supersede_same_path", fixedSourceControlReviewTime)

	writeFile(t, filepath.Join(repo, "NOTES.md"), "a newer snapshot of the same repo path\n")
	second, _ := recordSourceControlPlanForRepo(t, home, repo, "work_supersede_same_path", fixedSourceControlReviewTime.Add(time.Minute))
	if second.ID == first.ID {
		t.Fatalf("newer snapshot must mint a distinct plan id, got %q twice", first.ID)
	}

	dir := filepath.Dir(firstPaths.PlanPath)
	// `<stem>.decision.json` is the sidecar suffix internal/home selects as a
	// terminal review disposition, so the superseded group drains with its core.
	supersessionPath := filepath.Join(dir, first.ID+".decision.json")
	data, err := os.ReadFile(supersessionPath)
	if err != nil {
		t.Fatalf("older open plan was not superseded by the newer snapshot: %v", err)
	}
	var supersession SourceControlReviewSupersession
	if err := json.Unmarshal(data, &supersession); err != nil {
		t.Fatal(err)
	}
	if supersession.SchemaVersion != SourceControlReviewSupersessionSchemaVersion ||
		supersession.Status != "superseded" ||
		supersession.PlanID != first.ID ||
		supersession.SupersededBy != second.ID ||
		filepath.Clean(supersession.GitRoot) != filepath.Clean(first.GitRoot) {
		t.Fatalf("supersession does not name its plan and successor: %+v", supersession)
	}
	if _, err := os.Stat(filepath.Join(dir, second.ID+".decision.json")); err == nil {
		t.Fatalf("the newest plan must stay open, not supersede itself")
	}
	if _, err := os.Stat(firstPaths.PlanPath); err != nil {
		t.Fatalf("superseding must keep the older plan as evidence: %v", err)
	}

	queue := sourceControlCleanupQueueItems(t, home, 2)
	items := map[string]reviewartifact.QueueItem{}
	for _, item := range queue.Items {
		items[item.ID] = item
	}
	superseded := items[first.ID]
	if superseded.Status != "resolved" || superseded.Readiness != "no_action" || len(superseded.Actions) != 0 {
		t.Fatalf("superseded item still asks the operator: %+v", superseded)
	}
	if !strings.Contains(superseded.NextAction, second.ID) {
		t.Fatalf("superseded item does not name its successor: %q", superseded.NextAction)
	}
	if current := items[second.ID]; current.Status != "review_required" {
		t.Fatalf("newest item must stay open for review: %+v", current)
	}
	if queue.Counts.ReviewRequired != 1 || queue.Counts.OK != 1 {
		t.Fatalf("one repo path must leave exactly one open item, counts = %+v", queue.Counts)
	}
}

// Supersession must never overwrite a recorded disposition (immutable operator
// evidence, including a sealed mutation authorization), and must never retire a
// sibling describing the SAME snapshot — a count-only plan and a mutation-bound
// plan share one snapshot under different ids, so retiring the live one would
// silently drop an authorization the operator still owes.
func TestRecordSourceControlReviewPlanKeepsRecordedAndSameSnapshotPlans(t *testing.T) {
	repo := initGitRepo(t)
	home := t.TempDir()
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	recorded, recordedPaths := recordSourceControlPlanForRepo(t, home, repo, "work_supersede_guard_recorded", fixedSourceControlReviewTime)
	review := writeValidSourceControlReview(t, recordedPaths.ReviewYAMLPath, recorded)
	if _, err := RecordSourceControlReview(home, review, recordedPaths.ReviewYAMLPath, fixedSourceControlReviewTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(repo, "NOTES.md"), "a newer snapshot of the same repo path\n")
	open, openPaths := recordSourceControlPlanForRepo(t, home, repo, "work_supersede_guard_open", fixedSourceControlReviewTime.Add(2*time.Minute))
	dir := filepath.Dir(openPaths.PlanPath)
	if _, err := os.Stat(filepath.Join(dir, recorded.ID+".decision.json")); err == nil {
		t.Fatalf("a recorded disposition must never be superseded by a later snapshot")
	}
	if _, err := os.Stat(filepath.Join(dir, recorded.ID+".reviewed.json")); err != nil {
		t.Fatalf("recorded disposition evidence disappeared: %v", err)
	}

	snapshot, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	binding := SourceControlMutationBinding{
		ActionWorkID:      "work_supersede_guard_open",
		MutationInputHash: "mutation-input:sha256:" + strings.Repeat("d", 64),
		Capability:        "filesystem.write",
		Action:            "write_file",
		MutationClass:     "local_reversible",
		TargetPaths:       []string{"README.md"},
		ActionIntentIDs:   []string{"act_supersede_guard_open"},
	}
	mutation, err := BuildSourceControlMutationReviewPlan(snapshot, binding.ActionWorkID, binding, fixedSourceControlReviewTime.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordSourceControlReviewPlan(home, mutation); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, open.ID+".decision.json")); err == nil {
		t.Fatalf("a sibling describing the same snapshot must stay open, not be superseded")
	}
}

func recordSourceControlPlanForRepo(t *testing.T, home, repo, workID string, now time.Time) (SourceControlReviewPlan, SourceControlReviewPlanPaths) {
	t.Helper()
	snapshot, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildSourceControlReviewPlan(snapshot, workID, now)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := RecordSourceControlReviewPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	return plan, paths
}

func sourceControlCleanupQueueItems(t *testing.T, home string, want int) reviewartifact.QueueReport {
	t.Helper()
	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: SourceControlCleanupReviewArea}, fixedSourceControlReviewTime.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateSourceControlCleanupQueue(&queue)
	reviewartifact.Recount(&queue)
	if len(queue.Items) != want {
		t.Fatalf("source-control cleanup queue items = %d, want %d: %+v", len(queue.Items), want, queue)
	}
	return queue
}

func TestSourceControlReviewQueueFilesCleanSupersededPlanWithoutDeletingEvidence(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	snapshot, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildSourceControlReviewPlan(snapshot, "work_dirty_state_resolved", fixedSourceControlReviewTime)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	paths, err := RecordSourceControlReviewPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(repo, "README.md"), "initial\n")
	current, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "clean" {
		t.Fatalf("test repository status = %q, want clean", current.Status)
	}

	queue := sourceControlCleanupQueue(t, home)
	item := queue.Items[0]
	if item.Status != "resolved" || item.Readiness != "no_action" || item.Band != reviewartifact.BandFiled || len(item.Actions) != 0 {
		t.Fatalf("clean repository retained a current dirty-review action: %+v", item)
	}
	if queue.Counts.ReviewRequired != 0 || queue.Counts.OK != 1 || len(queue.Groups) != 0 {
		t.Fatalf("clean repository retained operator attention: counts=%+v groups=%+v", queue.Counts, queue.Groups)
	}
	if _, err := os.Stat(paths.PlanPath); err != nil {
		t.Fatalf("historical dirty-plan evidence was not preserved: %v", err)
	}
}

func TestSourceControlReviewQueueFilesLegacyInvalidPlanWhenCurrentRepoIsClean(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	snapshot, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildSourceControlReviewPlan(snapshot, "work_legacy_dirty_state", fixedSourceControlReviewTime)
	if err != nil {
		t.Fatal(err)
	}
	plan.Head = plan.Head[:12]
	if err := ValidateSourceControlReviewPlan(plan); err == nil {
		t.Fatal("test precondition: legacy short-SHA plan unexpectedly validated")
	}
	home := t.TempDir()
	if _, err := reviewartifact.WriteJSON(home, SourceControlCleanupReviewArea, plan.ID+".json", plan, "  "); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(repo, "README.md"), "initial\n")
	current, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "clean" {
		t.Fatalf("test repository status = %q, want clean", current.Status)
	}

	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: SourceControlCleanupReviewArea}, fixedSourceControlReviewTime.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateSourceControlCleanupQueue(&queue)
	reviewartifact.Recount(&queue)
	item := queue.Items[0]
	if item.Status != "resolved" || item.Readiness != "no_action" || item.Band != reviewartifact.BandFiled || len(item.Actions) != 0 {
		t.Fatalf("clean repository retained a legacy-invalid dirty-review action: %+v", item)
	}
	if queue.Counts.ReviewRequired != 0 || queue.Counts.OK != 1 || len(queue.Groups) != 0 {
		t.Fatalf("clean repository retained legacy-invalid operator attention: counts=%+v groups=%+v", queue.Counts, queue.Groups)
	}
}

func TestSourceControlReviewQueueKeepsLegacyInvalidPlanBlockedWhileRepoIsDirty(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	snapshot, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildSourceControlReviewPlan(snapshot, "work_legacy_dirty_state_current", fixedSourceControlReviewTime)
	if err != nil {
		t.Fatal(err)
	}
	plan.Head = plan.Head[:12]
	home := t.TempDir()
	if _, err := reviewartifact.WriteJSON(home, SourceControlCleanupReviewArea, plan.ID+".json", plan, "  "); err != nil {
		t.Fatal(err)
	}

	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: SourceControlCleanupReviewArea}, fixedSourceControlReviewTime.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateSourceControlCleanupQueue(&queue)
	reviewartifact.Recount(&queue)
	item := queue.Items[0]
	if item.Status != "review_required" || item.Readiness != "blocked" || item.Band != reviewartifact.BandNeedsYou {
		t.Fatalf("dirty repository lost its legacy-invalid review block: %+v", item)
	}
	if !strings.Contains(item.BlockedReason, "head must be a full git object id") ||
		queue.Counts.ReviewRequired != 1 || len(queue.Groups) != 1 {
		t.Fatalf("dirty repository did not preserve truthful blocked attention: item=%+v counts=%+v groups=%+v", item, queue.Counts, queue.Groups)
	}
}

func TestSourceControlReviewQueueBlocksWhenDirtyStateChangedSincePlan(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "first dirty state\n")
	snapshot, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildSourceControlReviewPlan(snapshot, "work_dirty_state_changed", fixedSourceControlReviewTime)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if _, err := RecordSourceControlReviewPlan(home, plan); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(repo, "README.md"), "different dirty state\n")
	queue := sourceControlCleanupQueue(t, home)
	item := queue.Items[0]
	if item.Status != "blocked" || item.Readiness != "blocked" || item.Band != reviewartifact.BandNeedsYou || len(item.Actions) != 0 {
		t.Fatalf("changed dirty state retained stale review authority: %+v", item)
	}
	if !strings.Contains(item.BlockedReason, "changed since this dirty-plan") ||
		!strings.Contains(item.NextAction, "regenerate") {
		t.Fatalf("changed dirty state did not expose a truthful recovery: %+v", item)
	}
	if queue.Counts.ReviewRequired != 1 || queue.Counts.OK != 0 {
		t.Fatalf("changed dirty state left the wrong attention counts: %+v", queue.Counts)
	}
}

func TestSourceControlReviewQueueBlocksWhenCurrentRepoCannotBeRead(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	snapshot, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildSourceControlReviewPlan(snapshot, "work_dirty_state_unreadable", fixedSourceControlReviewTime)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if _, err := RecordSourceControlReviewPlan(home, plan); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(repo, repo+".offline"); err != nil {
		t.Fatal(err)
	}

	queue := sourceControlCleanupQueue(t, home)
	item := queue.Items[0]
	if item.Status != "blocked" || item.Readiness != "blocked" || item.Band != reviewartifact.BandNeedsYou || len(item.Actions) != 0 {
		t.Fatalf("unreadable repository retained unverified review authority: %+v", item)
	}
	if !strings.Contains(item.BlockedReason, "cannot read current source-control state") ||
		!strings.Contains(item.NextAction, "restore repository visibility") {
		t.Fatalf("unreadable repository did not expose a truthful recovery: %+v", item)
	}
}

func TestSourceControlReviewQueueSeedsStableOwnerAuthority(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	snapshot, _, _, _, err := ImportGitSnapshot(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 7, 17, 9, 30, 0, 0, time.UTC)
	plan, err := BuildSourceControlReviewPlan(snapshot, "work_review_authority", createdAt)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	paths, err := RecordSourceControlReviewPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	writeValidSourceControlReview(t, paths.ReviewYAMLPath, plan)

	first := sourceControlCleanupQueue(t, home)
	second := sourceControlCleanupQueue(t, home)
	if len(first.Items[0].Actions) != 1 || len(second.Items[0].Actions) != 1 {
		t.Fatalf("source-control review action missing: first=%+v second=%+v", first.Items[0].Actions, second.Items[0].Actions)
	}
	action := first.Items[0].Actions[0]
	again := second.Items[0].Actions[0]
	if action.ID != "record_source_control_cleanup_review" || action.ActionIntent == nil || action.AuthorityContext == nil {
		t.Fatalf("source-control review action is not authority-seeded: %+v", action)
	}
	if action.ActionIntent.ID != again.ActionIntent.ID || action.ActionIntent.IDs.WorkID != plan.WorkID || action.ActionIntent.CreatedAt != plan.CreatedAt {
		t.Fatalf("action intent identity is not stable owner identity: first=%+v second=%+v", action.ActionIntent, again.ActionIntent)
	}
	if action.ActionIntent.Capability != "review.source_control" || action.ActionIntent.Action != action.ID || action.ActionIntent.Target.Kind != "file" || action.ActionIntent.Target.Path != paths.ReviewYAMLPath {
		t.Fatalf("action intent does not describe the exact review-record target: %+v", action.ActionIntent)
	}
	if action.ActionIntent.Privacy != "private" || action.ActionIntent.Authority.SourceClass != "runtime" || action.ActionIntent.Authority.Status != "current" {
		t.Fatalf("action intent does not carry private runtime authority: %+v", action.ActionIntent)
	}
	if action.ActionIntent.MutationClass != "local_reversible" || action.ActionIntent.Reversibility != "easy" || action.ActionIntent.BlastRadius != "local" {
		t.Fatalf("action intent outcome dimensions are wrong: %+v", action.ActionIntent)
	}
	if !strings.Contains(action.ActionIntent.RollbackPlan, paths.PlanPath) || !strings.Contains(action.ActionIntent.RollbackPlan, paths.ReviewYAMLPath) || !strings.Contains(action.ActionIntent.RollbackPlan, wantSourceControlReviewedReportPath(paths.ReviewYAMLPath, plan)) {
		t.Fatalf("verified owner recovery is missing an exact plan/YAML/report rollback: %+v", action.ActionIntent)
	}
	if err := action.ActionIntent.Validate(); err != nil {
		t.Fatalf("owner-seeded action intent invalid: %v", err)
	}
	ctx := action.AuthorityContext
	if ctx.TargetScope != "private" || ctx.VerifiedRecoverability != policy.RecoverabilityVerified || ctx.OperatorControlContinuity != policy.OperatorControlContinuous {
		t.Fatalf("authority context does not carry owner outcome evidence: %+v", ctx)
	}
	wantReportPath := wantSourceControlReviewedReportPath(paths.ReviewYAMLPath, plan)
	proofs := strings.Join(ctx.ExpectedProof, "\n")
	if !strings.Contains(ctx.ExpectedReceipt, "review report") || !strings.Contains(proofs, wantReportPath) || !strings.Contains(proofs, plan.WorkID) || !strings.Contains(proofs, paths.PlanPath) || !strings.Contains(proofs, plan.PlanDigest) {
		t.Fatalf("authority context does not truthfully describe the review report and work trail: %+v", ctx)
	}
	encoded, err := json.Marshal(action)
	if err != nil {
		t.Fatal(err)
	}
	if bytes := string(encoded); strings.Contains(bytes, "authority_context") || strings.Contains(bytes, "verified_recoverability") {
		t.Fatalf("producer-only authority context leaked into action JSON: %s", bytes)
	}
}

func TestSourceControlReviewAuthorityBindsValidatedDispositionMeaning(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	snapshot, _, _, _, err := ImportGitSnapshot(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildSourceControlReviewPlan(snapshot, "work_review_disposition", fixedSourceControlReviewTime)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	paths, err := RecordSourceControlReviewPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	baseReview := writeValidSourceControlReview(t, paths.ReviewYAMLPath, plan)
	baseAction := sourceControlCleanupQueue(t, home).Items[0].Actions[0]
	baseDigest := sourceControlExpectedDigestArg(t, baseAction)
	if !strings.Contains(strings.Join(baseAction.AuthorityContext.ExpectedProof, "\n"), baseDigest) {
		t.Fatalf("authority proof does not bind exact review source digest: %+v", baseAction.AuthorityContext)
	}

	tests := []struct {
		name   string
		mutate func(*SourceControlReview)
	}{
		{name: "decision", mutate: func(review *SourceControlReview) { review.Decision = "owner_action_required" }},
		{name: "rationale", mutate: func(review *SourceControlReview) { review.Rationale = "a materially different reviewed rationale" }},
		{name: "confidence", mutate: func(review *SourceControlReview) { review.Confidence = 0.75 }},
		{name: "reviewer", mutate: func(review *SourceControlReview) { review.ReviewedBy = "operator:reviewer" }},
		{name: "next action", mutate: func(review *SourceControlReview) { review.NextAction = "record a different reviewed next action" }},
		{name: "evidence", mutate: func(review *SourceControlReview) {
			review.EvidenceRefs = append(review.EvidenceRefs, "evidence:additional")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			review := baseReview
			review.EvidenceRefs = append([]string{}, baseReview.EvidenceRefs...)
			tc.mutate(&review)
			writeSourceControlReviewValue(t, paths.ReviewYAMLPath, review)
			action := sourceControlCleanupQueue(t, home).Items[0].Actions[0]
			if action.ActionIntent.ID == baseAction.ActionIntent.ID || sourceControlExpectedDigestArg(t, action) == baseDigest {
				t.Fatalf("material review edit retained action identity: base=%+v changed=%+v", baseAction, action)
			}
		})
	}
}

func TestSourceControlReviewConsumersRejectMalformedBodies(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	snapshot, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildSourceControlReviewPlan(snapshot, "work_review_body_validation", fixedSourceControlReviewTime)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*SourceControlReview)
	}{
		{name: "reviewed at malformed", mutate: func(review *SourceControlReview) { review.ReviewedAt = "not-a-timestamp" }},
		{name: "reviewed at zero", mutate: func(review *SourceControlReview) { review.ReviewedAt = "0001-01-01T00:00:00Z" }},
		{name: "empty evidence entry", mutate: func(review *SourceControlReview) { review.EvidenceRefs = append(review.EvidenceRefs, "") }},
		{name: "whitespace evidence entry", mutate: func(review *SourceControlReview) { review.EvidenceRefs = append(review.EvidenceRefs, "   ") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			paths, err := RecordSourceControlReviewPlan(home, plan)
			if err != nil {
				t.Fatal(err)
			}
			review := writeValidSourceControlReview(t, paths.ReviewYAMLPath, plan)
			tc.mutate(&review)
			writeSourceControlReviewValue(t, paths.ReviewYAMLPath, review)

			queue := sourceControlCleanupQueue(t, home)
			if len(queue.Items[0].Actions) != 0 {
				t.Errorf("malformed review mounted authority action: %+v", queue.Items[0].Actions)
			}
			if _, err := RecordSourceControlReview(home, review, paths.ReviewYAMLPath, fixedSourceControlReviewTime.Add(time.Minute)); err == nil {
				t.Errorf("malformed review body was recorded: %+v", review)
			}
		})
	}
}

func TestValidateSourceControlReviewRejectsNonFiniteConfidence(t *testing.T) {
	valid := SourceControlReview{
		SchemaVersion: SourceControlReviewSchemaVersion,
		ID:            "scr_review_confidence",
		PlanID:        "scr_review_confidence",
		RepoID:        "repo:confidence",
		Decision:      "parked",
		Rationale:     "confidence must be finite",
		Confidence:    1,
		EvidenceRefs:  []string{"evidence:confidence"},
	}
	for _, confidence := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		review := valid
		review.Confidence = confidence
		if err := ValidateSourceControlReview(review); err == nil {
			t.Fatalf("non-finite confidence accepted: %v", confidence)
		}
	}
}

func TestRecordSourceControlReviewPlanRejectsStructurallyInvalidPlan(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	snapshot, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := BuildSourceControlReviewPlan(snapshot, "work_strict_plan", fixedSourceControlReviewTime)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		want   string
		mutate func(*SourceControlReviewPlan)
	}{
		{name: "schema", want: "schema_version", mutate: func(plan *SourceControlReviewPlan) { plan.SchemaVersion = "compound.source_control_review_plan.v9" }},
		{name: "status", want: "status", mutate: func(plan *SourceControlReviewPlan) { plan.Status = "recorded" }},
		{name: "source status", want: "source_status", mutate: func(plan *SourceControlReviewPlan) { plan.SourceStatus = "clean" }},
		{name: "repo identity", want: "repo_id", mutate: func(plan *SourceControlReviewPlan) { plan.RepoID = "" }},
		{name: "head identity", want: "head", mutate: func(plan *SourceControlReviewPlan) { plan.Head = "short" }},
		{name: "fingerprint", want: "changed_state_fingerprint", mutate: func(plan *SourceControlReviewPlan) { plan.ChangedStateFingerprint = "sha256:bad" }},
		{name: "counts", want: "changed_paths", mutate: func(plan *SourceControlReviewPlan) { plan.ChangedPaths++ }},
		{name: "draft identity", want: "review_draft.review_id", mutate: func(plan *SourceControlReviewPlan) { plan.ReviewDraft.ReviewID = "scr_other" }},
		{name: "draft semantics", want: "review_draft.verification", mutate: func(plan *SourceControlReviewPlan) { plan.ReviewDraft.Verification = "" }},
		{name: "required evidence", want: "review_draft.required_evidence", mutate: func(plan *SourceControlReviewPlan) { plan.ReviewDraft.RequiredEvidence = []string{""} }},
		{name: "mutation binding", want: "mutation_binding", mutate: func(plan *SourceControlReviewPlan) { plan.MutationBinding = &SourceControlMutationBinding{} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := valid
			plan.ReviewDraft.RequiredEvidence = append([]string{}, valid.ReviewDraft.RequiredEvidence...)
			tc.mutate(&plan)
			plan.PlanDigest = SourceControlReviewPlanDigest(plan)
			if _, err := RecordSourceControlReviewPlan(t.TempDir(), plan); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("structurally invalid plan error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRecordSourceControlReviewRejectsStructurallyInvalidPersistedPlan(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	snapshot, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildSourceControlReviewPlan(snapshot, "work_strict_record", fixedSourceControlReviewTime)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	paths, err := RecordSourceControlReviewPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	review := writeValidSourceControlReview(t, paths.ReviewYAMLPath, plan)
	plan.Status = "recorded"
	plan.PlanDigest = SourceControlReviewPlanDigest(plan)
	writeJSON(t, paths.PlanPath, plan)
	if _, err := RecordSourceControlReview(home, review, paths.ReviewYAMLPath, fixedSourceControlReviewTime.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("record accepted structurally invalid persisted plan: %v", err)
	}
}

func TestRecordSourceControlReviewRejectsOrphanReviewIdentity(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	snapshot, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildSourceControlReviewPlan(snapshot, "work_review_identity", fixedSourceControlReviewTime)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	paths, err := RecordSourceControlReviewPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	valid := writeValidSourceControlReview(t, paths.ReviewYAMLPath, plan)

	t.Run("review id", func(t *testing.T) {
		review := valid
		review.ID = "scr_other"
		if _, err := RecordSourceControlReview(home, review, paths.ReviewYAMLPath, fixedSourceControlReviewTime.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "review_id") {
			t.Fatalf("orphan review id accepted: %v", err)
		}
	})

	t.Run("source path", func(t *testing.T) {
		otherPath := filepath.Join(filepath.Dir(paths.ReviewYAMLPath), "other.review.yaml")
		writeSourceControlReviewValue(t, otherPath, valid)
		if _, err := RecordSourceControlReview(home, valid, otherPath, fixedSourceControlReviewTime.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "source path") {
			t.Fatalf("orphan source path accepted: %v", err)
		}
	})
}

func TestSourceControlReviewAuthoritySeedRequiresStableOwnerIdentity(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	snapshot, _, _, _, err := ImportGitSnapshot(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildSourceControlReviewPlan(snapshot, "work_review_authority", fixedSourceControlReviewTime)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	item := reviewartifact.QueueItem{ID: plan.ID, JSONPath: filepath.Join(dir, plan.ID+".json")}
	reviewPath := filepath.Join(dir, plan.ID+".review.yaml")
	review := writeValidSourceControlReview(t, reviewPath, plan)
	sourceDigest, err := SourceControlReviewSourceDigest(review)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		mutatePlan   func(*SourceControlReviewPlan)
		mutateItem   func(*reviewartifact.QueueItem)
		mutateReview func(string) string
	}{
		{name: "missing work identity", mutatePlan: func(plan *SourceControlReviewPlan) { plan.WorkID = "" }},
		{name: "empty work suffix", mutatePlan: func(plan *SourceControlReviewPlan) { plan.WorkID = "work_" }},
		{name: "synthetic work identity", mutatePlan: func(plan *SourceControlReviewPlan) { plan.WorkID = "work_live_review_authority" }},
		{name: "unstable timestamp", mutatePlan: func(plan *SourceControlReviewPlan) { plan.CreatedAt = "not-a-timestamp" }},
		{name: "mismatched plan path", mutateItem: func(item *reviewartifact.QueueItem) { item.JSONPath = filepath.Join(dir, "other.json") }},
		{name: "mismatched review path", mutateReview: func(string) string { return filepath.Join(dir, "other.review.yaml") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidatePlan := plan
			candidateItem := item
			candidateReview := reviewPath
			if tc.mutatePlan != nil {
				tc.mutatePlan(&candidatePlan)
				candidatePlan.PlanDigest = SourceControlReviewPlanDigest(candidatePlan)
			}
			if tc.mutateItem != nil {
				tc.mutateItem(&candidateItem)
			}
			if tc.mutateReview != nil {
				candidateReview = tc.mutateReview(candidateReview)
			}
			intent, context := sourceControlRecordReviewAuthoritySeed(candidateItem, candidatePlan, review, candidateReview, sourceDigest, "record_source_control_cleanup_review")
			if intent != nil || context != nil {
				t.Fatalf("invalid owner identity was authority-seeded: intent=%+v context=%+v", intent, context)
			}
		})
	}
}

func wantSourceControlReviewedReportPath(reviewYAML string, plan SourceControlReviewPlan) string {
	return filepath.Join(filepath.Dir(reviewYAML), sourceControlReviewReportName(plan.ID, plan.ReviewDraft.ReviewID))
}

func TestSourceControlReviewPlanRequiresDirtySnapshot(t *testing.T) {
	repo := initGitRepo(t)
	snap, _, _, _, err := ImportGitSnapshot(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildSourceControlReviewPlan(snap, "", time.Now()); err == nil || !strings.Contains(err.Error(), "dirty source-control snapshot required") {
		t.Fatalf("err = %v, want dirty snapshot requirement", err)
	}
}

func TestRecordedSourceControlMutationReviewRequiresExactImmutableBinding(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	writeFile(t, filepath.Join(repo, "SECOND.md"), "second\n")
	snapshot, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Head) != 40 {
		t.Fatalf("review binding requires full immutable HEAD, got %q", snapshot.Head)
	}
	binding := SourceControlMutationBinding{
		ActionWorkID:      "work_live_session_a",
		MutationInputHash: "mutation-input:sha256:" + strings.Repeat("a", 64),
		Capability:        "filesystem.write",
		Action:            "multi_edit_file",
		MutationClass:     "local_reversible",
		TargetPaths:       []string{"README.md", "SECOND.md"},
		ActionIntentIDs:   []string{"act_live_a", "act_live_b"},
	}
	plan, err := BuildSourceControlMutationReviewPlan(snapshot, "work_owner", binding, fixedSourceControlReviewTime)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	paths, err := RecordSourceControlReviewPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	report := authorizeSourceControlMutationPlan(t, home, paths, plan)
	if report.PlanDigest == "" || report.PlanDigest != plan.PlanDigest || report.WorkID != "work_owner" ||
		report.ReviewedBy != "operator:test" || report.SourceDigest == "" || report.AuthorizationDigest == "" {
		t.Fatalf("recorded report lost immutable plan/work binding: %+v", report)
	}

	matched, err := FindRecordedSourceControlMutationReview(home, snapshot, binding)
	if err != nil || matched == nil {
		t.Fatalf("exact recorded review did not match: report=%+v err=%v", matched, err)
	}

	stateCases := []struct {
		name   string
		mutate func(*SourceControlSnapshot)
	}{
		{"repo", func(s *SourceControlSnapshot) { s.RepoID = "repo:other" }},
		{"root", func(s *SourceControlSnapshot) { s.GitRoot = filepath.Join(repo, "other-clone") }},
		{"remote", func(s *SourceControlSnapshot) { s.Remote = "git@example.invalid/other.git" }},
		{"head", func(s *SourceControlSnapshot) { s.Head = strings.Repeat("b", 40) }},
		{"content-index fingerprint", func(s *SourceControlSnapshot) { s.ChangedStateFingerprint = "sha256:" + strings.Repeat("b", 64) }},
	}
	for _, tc := range stateCases {
		t.Run("mismatched "+tc.name, func(t *testing.T) {
			changed := snapshot
			tc.mutate(&changed)
			if got, findErr := FindRecordedSourceControlMutationReview(home, changed, binding); findErr != nil || got != nil {
				t.Fatalf("mismatched %s matched: report=%+v err=%v", tc.name, got, findErr)
			}
		})
	}

	bindingCases := []struct {
		name   string
		mutate func(*SourceControlMutationBinding)
	}{
		{"action work", func(b *SourceControlMutationBinding) { b.ActionWorkID = "work_live_other" }},
		{"input", func(b *SourceControlMutationBinding) {
			b.MutationInputHash = "mutation-input:sha256:" + strings.Repeat("b", 64)
		}},
		{"capability", func(b *SourceControlMutationBinding) { b.Capability = "filesystem.delete" }},
		{"action", func(b *SourceControlMutationBinding) { b.Action = "edit_file" }},
		{"class", func(b *SourceControlMutationBinding) { b.MutationClass = "destructive" }},
		{"partial target set", func(b *SourceControlMutationBinding) { b.TargetPaths = []string{"README.md"} }},
	}
	for _, tc := range bindingCases {
		t.Run("mismatched "+tc.name, func(t *testing.T) {
			changed := binding
			changed.TargetPaths = append([]string{}, binding.TargetPaths...)
			changed.ActionIntentIDs = append([]string{}, binding.ActionIntentIDs...)
			tc.mutate(&changed)
			if got, findErr := FindRecordedSourceControlMutationReview(home, snapshot, changed); findErr != nil || got != nil {
				t.Fatalf("mismatched %s matched: report=%+v err=%v", tc.name, got, findErr)
			}
		})
	}

	originalPlanBytes, err := os.ReadFile(paths.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	var tampered SourceControlReviewPlan
	if err := json.Unmarshal(originalPlanBytes, &tampered); err != nil {
		t.Fatal(err)
	}
	tampered.Head = strings.Repeat("c", 40)
	tamperedBytes, err := json.MarshalIndent(tampered, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.PlanPath, tamperedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, findErr := FindRecordedSourceControlMutationReview(home, snapshot, binding); findErr != nil || got != nil {
		t.Fatalf("tampered plan after review matched: report=%+v err=%v", got, findErr)
	}
}

func TestRecordedSourceControlMutationReviewSelectsNewestExactSealedReport(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	snapshot, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	binding := SourceControlMutationBinding{
		ActionWorkID:      "work_live_newest_exact",
		MutationInputHash: "mutation-input:sha256:" + strings.Repeat("9", 64),
		Capability:        "filesystem.write",
		Action:            "write_file",
		MutationClass:     "local_reversible",
		TargetPaths:       []string{"README.md"},
		ActionIntentIDs:   []string{"act_newest_exact"},
	}
	plans := make([]SourceControlReviewPlan, 2)
	for i, owner := range []string{"work_newest_exact_a", "work_newest_exact_b"} {
		plans[i], err = BuildSourceControlMutationReviewPlan(snapshot, owner, binding, fixedSourceControlReviewTime)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Put CreatedAt order deliberately opposite the filename/hash order. A scan
	// ordered by immutable report time must win; lexical directory order must not.
	olderIndex := 0
	if plans[1].ID > plans[0].ID {
		olderIndex = 1
	}
	newerIndex := 1 - olderIndex
	home := t.TempDir()
	paths := make([]SourceControlReviewPlanPaths, len(plans))
	for i, plan := range plans {
		paths[i], err = RecordSourceControlReviewPlan(home, plan)
		if err != nil {
			t.Fatal(err)
		}
	}
	older := authorizeSourceControlMutationPlanAt(t, home, paths[olderIndex], plans[olderIndex], fixedSourceControlReviewTime.Add(time.Minute))
	newer := authorizeSourceControlMutationPlanAt(t, home, paths[newerIndex], plans[newerIndex], fixedSourceControlReviewTime.Add(2*time.Minute))
	if older.CreatedAt >= newer.CreatedAt || plans[olderIndex].ID <= plans[newerIndex].ID {
		t.Fatalf("test precondition failed: older=%s/%s newer=%s/%s", older.CreatedAt, plans[olderIndex].ID, newer.CreatedAt, plans[newerIndex].ID)
	}

	matched, err := FindRecordedSourceControlMutationReview(home, snapshot, binding)
	if err != nil {
		t.Fatal(err)
	}
	if matched == nil || matched.ID != newer.ID {
		t.Fatalf("selected report = %+v, want newest exact sealed report %s (older lexical-first report %s)", matched, newer.ID, older.ID)
	}
}

func TestRecordedSourceControlMutationReviewOrdersRFC3339NanoByInstant(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	snapshot, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	binding := SourceControlMutationBinding{
		ActionWorkID:      "work_live_fractional_order",
		MutationInputHash: "mutation-input:sha256:" + strings.Repeat("6", 64),
		Capability:        "filesystem.write",
		Action:            "write_file",
		MutationClass:     "local_reversible",
		TargetPaths:       []string{"README.md"},
		ActionIntentIDs:   []string{"act_fractional_order"},
	}
	makeReport := func(home, owner string) SourceControlReviewReport {
		t.Helper()
		plan, buildErr := BuildSourceControlMutationReviewPlan(snapshot, owner, binding, fixedSourceControlReviewTime)
		if buildErr != nil {
			t.Fatal(buildErr)
		}
		paths, recordErr := RecordSourceControlReviewPlan(home, plan)
		if recordErr != nil {
			t.Fatal(recordErr)
		}
		return authorizeSourceControlMutationPlanAt(t, home, paths, plan, fixedSourceControlReviewTime)
	}
	reseal := func(report *SourceControlReviewReport, createdAt string) {
		t.Helper()
		report.CreatedAt = createdAt
		report.AuthorizationDigest = sourceControlReviewReportDigest(*report)
		writeJSON(t, report.ReviewPath, *report)
	}

	home := t.TempDir()
	older := makeReport(home, "work_fractional_order_older")
	newer := makeReport(home, "work_fractional_order_newer")
	reseal(&older, "2026-07-13T12:00:20Z")
	reseal(&newer, "2026-07-13T12:00:20.9Z")

	matched, err := FindRecordedSourceControlMutationReview(home, snapshot, binding)
	if err != nil {
		t.Fatal(err)
	}
	if matched == nil || matched.ID != newer.ID {
		t.Fatalf("fractional timestamp selection = %+v, want chronologically newer %s (%s after %s)", matched, newer.ID, newer.CreatedAt, older.CreatedAt)
	}

	// Equal instants may have different legal RFC3339Nano spellings. The report
	// id, not the raw timestamp spelling or directory order, breaks that tie.
	higherID, lowerID := &older, &newer
	if higherID.ID < lowerID.ID {
		higherID, lowerID = lowerID, higherID
	}
	reseal(higherID, "2026-07-13T12:00:21.0Z")
	reseal(lowerID, "2026-07-13T12:00:21Z")
	matched, err = FindRecordedSourceControlMutationReview(home, snapshot, binding)
	if err != nil {
		t.Fatal(err)
	}
	if matched == nil || matched.ID != higherID.ID {
		t.Fatalf("equal-instant selection = %+v, want deterministic higher id %s", matched, higherID.ID)
	}
}

func TestSourceControlMutationAuthorizationRequiresReviewerAndSealedSource(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	snapshot, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	binding := SourceControlMutationBinding{
		ActionWorkID:      "work_live_reviewer",
		MutationInputHash: "mutation-input:sha256:" + strings.Repeat("c", 64),
		Capability:        "filesystem.write",
		Action:            "write_file",
		MutationClass:     "local_reversible",
		TargetPaths:       []string{"README.md"},
		ActionIntentIDs:   []string{"act_live_reviewer"},
	}
	plan, err := BuildSourceControlMutationReviewPlan(snapshot, "work_owner_reviewer", binding, fixedSourceControlReviewTime)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	paths, err := RecordSourceControlReviewPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	review := SourceControlReview{
		SchemaVersion: SourceControlReviewSchemaVersion,
		ID:            plan.ReviewDraft.ReviewID,
		PlanID:        plan.ID,
		RepoID:        plan.RepoID,
		Decision:      "authorize_mutation",
		Rationale:     "operator reviewed the exact mutation",
		Confidence:    1,
		EvidenceRefs:  append([]string{}, plan.EvidenceRefs...),
	}
	if _, err := RecordSourceControlReview(home, review, paths.ReviewYAMLPath, fixedSourceControlReviewTime.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "reviewed_by") {
		t.Fatalf("authorization without reviewer error = %v", err)
	}
	review.ReviewedBy = "codex"
	if _, err := RecordSourceControlReview(home, review, paths.ReviewYAMLPath, fixedSourceControlReviewTime.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "operator provenance") {
		t.Fatalf("authorization with executor reviewer error = %v", err)
	}
	review.ReviewedBy = "operator:test"
	report, err := RecordSourceControlReview(home, review, paths.ReviewYAMLPath, fixedSourceControlReviewTime.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.ReviewedBy != review.ReviewedBy || !validSHA256(report.SourceDigest) || !validSHA256(report.AuthorizationDigest) {
		t.Fatalf("authorization seal missing: %+v", report)
	}
	reportBytes, err := os.ReadFile(report.ReviewPath)
	if err != nil {
		t.Fatal(err)
	}
	sourceBytes, err := os.ReadFile(paths.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}

	tampered := report
	tampered.ReviewedBy = "operator:other"
	writeJSON(t, report.ReviewPath, tampered)
	if got, findErr := FindRecordedSourceControlMutationReview(home, snapshot, binding); findErr != nil || got != nil {
		t.Fatalf("reviewer-tampered authorization matched: report=%+v err=%v", got, findErr)
	}
	if err := os.WriteFile(report.ReviewPath, reportBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ReviewYAMLPath, append(sourceBytes, []byte("# tampered after review\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, findErr := FindRecordedSourceControlMutationReview(home, snapshot, binding); findErr != nil || got != nil {
		t.Fatalf("source-tampered authorization matched: report=%+v err=%v", got, findErr)
	}
}

func TestSourceControlMutationAuthorizationQueueFailsLoudlyOnTampering(t *testing.T) {
	tests := []struct {
		name   string
		tamper func(*testing.T, SourceControlReviewPlanPaths, SourceControlReviewReport)
	}{
		{
			name: "authorization report",
			tamper: func(t *testing.T, _ SourceControlReviewPlanPaths, report SourceControlReviewReport) {
				report.NextAction = "tampered after authorization was sealed"
				writeJSON(t, report.ReviewPath, report)
			},
		},
		{
			name: "reviewed YAML",
			tamper: func(t *testing.T, paths SourceControlReviewPlanPaths, _ SourceControlReviewReport) {
				data, err := os.ReadFile(paths.ReviewYAMLPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(paths.ReviewYAMLPath, append(data, []byte("# tampered after authorization\n")...), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := initGitRepo(t)
			writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
			snapshot, err := ReadGitSnapshot(repo)
			if err != nil {
				t.Fatal(err)
			}
			binding := SourceControlMutationBinding{
				ActionWorkID:      "work_queue_authorization",
				MutationInputHash: "mutation-input:sha256:" + strings.Repeat("f", 64),
				Capability:        "filesystem.write",
				Action:            "write_file",
				MutationClass:     "local_reversible",
				TargetPaths:       []string{"README.md"},
				ActionIntentIDs:   []string{"act_queue_authorization"},
			}
			plan, err := BuildSourceControlMutationReviewPlan(snapshot, binding.ActionWorkID, binding, fixedSourceControlReviewTime)
			if err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			paths, err := RecordSourceControlReviewPlan(home, plan)
			if err != nil {
				t.Fatal(err)
			}
			report := authorizeSourceControlMutationPlan(t, home, paths, plan)

			baseline := sourceControlCleanupQueue(t, home)
			if baseline.Items[0].Status != "recorded" || baseline.Items[0].Readiness != "review_recorded" || baseline.Counts.OK != 1 {
				t.Fatalf("valid sealed mutation authorization queue = %+v, want recorded baseline", baseline)
			}

			tc.tamper(t, paths, report)
			queue := sourceControlCleanupQueue(t, home)
			item := queue.Items[0]
			if item.Status != "blocked" || item.Readiness != "blocked" || queue.Counts.OK != 0 || queue.Counts.ReviewRequired != 1 {
				t.Fatalf("tampered mutation authorization queue = %+v, want blocked open review", queue)
			}
			if !strings.Contains(item.BlockedReason, "authorization") || !strings.Contains(item.NextAction, "regenerate") || !strings.Contains(item.NextAction, "re-review") {
				t.Fatalf("tampered mutation authorization guidance = %+v, want explicit regenerate and re-review", item)
			}
			if len(item.Actions) != 1 || item.Actions[0].ID != "regenerate_source_control_mutation_review" || !strings.Contains(item.Actions[0].Command, "dirty-plan") {
				t.Fatalf("tampered mutation authorization action = %+v, want one regeneration action", item.Actions)
			}
		})
	}
}

func TestNonAuthorizingReportCannotBeEditedIntoAuthorization(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	snapshot, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	binding := SourceControlMutationBinding{
		ActionWorkID:      "work_live_parked",
		MutationInputHash: "mutation-input:sha256:" + strings.Repeat("d", 64),
		Capability:        "filesystem.write",
		Action:            "write_file",
		MutationClass:     "local_reversible",
		TargetPaths:       []string{"README.md"},
		ActionIntentIDs:   []string{"act_live_parked"},
	}
	plan, err := BuildSourceControlMutationReviewPlan(snapshot, "work_owner_parked", binding, fixedSourceControlReviewTime)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	paths, err := RecordSourceControlReviewPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	report, err := RecordSourceControlReview(home, SourceControlReview{
		SchemaVersion: SourceControlReviewSchemaVersion,
		ID:            plan.ReviewDraft.ReviewID,
		PlanID:        plan.ID,
		RepoID:        plan.RepoID,
		Decision:      "parked",
		Rationale:     "leave mutation parked",
		Confidence:    1,
		EvidenceRefs:  append([]string{}, plan.EvidenceRefs...),
	}, paths.ReviewYAMLPath, fixedSourceControlReviewTime.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	report.Decision = "authorize_mutation"
	report.Status = "recorded"
	writeJSON(t, report.ReviewPath, report)
	if got, findErr := FindRecordedSourceControlMutationReview(home, snapshot, binding); findErr != nil || got != nil {
		t.Fatalf("edited parked report authorized mutation: report=%+v err=%v", got, findErr)
	}
}

func TestRealActionWorkMustMatchReviewOwner(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	snapshot, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	binding := SourceControlMutationBinding{
		ActionWorkID:      "work_real_action",
		MutationInputHash: "mutation-input:sha256:" + strings.Repeat("e", 64),
		Capability:        "filesystem.write",
		Action:            "write_file",
		MutationClass:     "local_reversible",
		TargetPaths:       []string{"README.md"},
		ActionIntentIDs:   []string{"act_real_action"},
	}
	if _, err := BuildSourceControlMutationReviewPlan(snapshot, "work_other_owner", binding, fixedSourceControlReviewTime); err == nil || !strings.Contains(err.Error(), "must equal owner work_id") {
		t.Fatalf("cross-work authorization error = %v", err)
	}
	if _, err := BuildSourceControlMutationReviewPlan(snapshot, binding.ActionWorkID, binding, fixedSourceControlReviewTime); err != nil {
		t.Fatalf("same real action/owner work rejected: %v", err)
	}
}

func TestLegacyCountOnlySourceControlReviewCannotAuthorizeMutation(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	snapshot, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildSourceControlReviewPlan(snapshot, "work_owner", fixedSourceControlReviewTime)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	paths, err := RecordSourceControlReviewPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	review := SourceControlReview{
		SchemaVersion: SourceControlReviewSchemaVersion,
		ID:            plan.ReviewDraft.ReviewID, PlanID: plan.ID, RepoID: plan.RepoID,
		Decision: "authorize_mutation", Rationale: "legacy count-only plan must not authorize",
		Confidence: 1, EvidenceRefs: append([]string{}, plan.EvidenceRefs...),
	}
	if _, err := RecordSourceControlReview(home, review, paths.ReviewYAMLPath, fixedSourceControlReviewTime.Add(time.Minute)); err == nil {
		t.Fatal("legacy count-only review unexpectedly authorized a mutation")
	}
}

var fixedSourceControlReviewTime = time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

// Authorizations recorded before the report name was de-duped live at the
// doubled stem and are immutable evidence. The queue must keep validating them
// exactly, or de-duping the writer would silently invalidate recorded operator
// authority.
func TestRecordedSourceControlMutationQueueAcceptsLegacyDoubledStemReport(t *testing.T) {
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	snapshot, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	binding := SourceControlMutationBinding{
		ActionWorkID:      "work_legacy_authorization",
		MutationInputHash: "mutation-input:sha256:" + strings.Repeat("c", 64),
		Capability:        "filesystem.write",
		Action:            "write_file",
		MutationClass:     "local_reversible",
		TargetPaths:       []string{"README.md"},
		ActionIntentIDs:   []string{"act_legacy_authorization"},
	}
	plan, err := BuildSourceControlMutationReviewPlan(snapshot, binding.ActionWorkID, binding, fixedSourceControlReviewTime)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	paths, err := RecordSourceControlReviewPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	report := authorizeSourceControlMutationPlan(t, home, paths, plan)

	// Re-shape the sealed authorization exactly as the pre-de-dupe writer left it.
	dir := filepath.Dir(paths.ReviewYAMLPath)
	legacyPath := filepath.Join(dir, sourceControlLegacyReviewReportName(plan.ID, plan.ReviewDraft.ReviewID))
	if err := os.Remove(report.ReviewPath); err != nil {
		t.Fatal(err)
	}
	report.ReviewPath = legacyPath
	report.AuthorizationDigest = ""
	report.AuthorizationDigest = sourceControlReviewReportDigest(report)
	writeJSON(t, legacyPath, report)

	queue := sourceControlCleanupQueue(t, home)
	if queue.Items[0].Status != "recorded" || queue.Items[0].Readiness != "review_recorded" || queue.Counts.OK != 1 {
		t.Fatalf("legacy doubled-stem authorization queue = %+v, want recorded", queue)
	}
	found, err := FindRecordedSourceControlMutationReview(home, snapshot, binding)
	if err != nil || found == nil {
		t.Fatalf("legacy doubled-stem authorization not found: report=%+v err=%v", found, err)
	}
}

func authorizeSourceControlMutationPlan(t *testing.T, home string, paths SourceControlReviewPlanPaths, plan SourceControlReviewPlan) SourceControlReviewReport {
	return authorizeSourceControlMutationPlanAt(t, home, paths, plan, fixedSourceControlReviewTime.Add(time.Minute))
}

func authorizeSourceControlMutationPlanAt(t *testing.T, home string, paths SourceControlReviewPlanPaths, plan SourceControlReviewPlan, now time.Time) SourceControlReviewReport {
	t.Helper()
	review := SourceControlReview{
		SchemaVersion: SourceControlReviewSchemaVersion,
		ID:            plan.ReviewDraft.ReviewID, PlanID: plan.ID, RepoID: plan.RepoID,
		Decision: "authorize_mutation", Rationale: "exact reviewed dirty mutation may proceed once",
		Confidence: 1, ReviewedBy: "operator:test", EvidenceRefs: append([]string{}, plan.EvidenceRefs...),
	}
	report, err := RecordSourceControlReview(home, review, paths.ReviewYAMLPath, now)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func sourceControlCleanupQueue(t *testing.T, home string) reviewartifact.QueueReport {
	t.Helper()
	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: SourceControlCleanupReviewArea}, fixedSourceControlReviewTime.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateSourceControlCleanupQueue(&queue)
	reviewartifact.Recount(&queue)
	if len(queue.Items) != 1 {
		t.Fatalf("source-control cleanup queue items = %d, want one: %+v", len(queue.Items), queue)
	}
	return queue
}

func writeValidSourceControlReview(t *testing.T, path string, plan SourceControlReviewPlan) SourceControlReview {
	t.Helper()
	review := SourceControlReview{
		SchemaVersion: SourceControlReviewSchemaVersion,
		ID:            plan.ReviewDraft.ReviewID,
		PlanID:        plan.ID,
		RepoID:        plan.RepoID,
		Decision:      "parked",
		Rationale:     "reviewed source-control state remains deliberately parked",
		Confidence:    1,
		NextAction:    "keep the reviewed state parked under its owner",
		EvidenceRefs:  append([]string{}, plan.EvidenceRefs...),
	}
	writeSourceControlReviewValue(t, path, review)
	return review
}

func writeSourceControlReviewValue(t *testing.T, path string, review SourceControlReview) {
	t.Helper()
	data, err := json.MarshalIndent(review, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func sourceControlExpectedDigestArg(t *testing.T, action reviewartifact.Action) string {
	t.Helper()
	for i, arg := range action.Argv {
		if arg == "--expected-source-digest" && i+1 < len(action.Argv) {
			return action.Argv[i+1]
		}
	}
	t.Fatalf("source-control action missing expected source digest: %+v", action)
	return ""
}

func gitStatusShort(t *testing.T, repo string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "status", "--porcelain=v1", "-b")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status failed: %v\n%s", err, string(out))
	}
	return string(out)
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, byte('\n'))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
