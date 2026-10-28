package director

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/authority"
	"compound/internal/decisions"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/mem"
	"compound/internal/reviewartifact"
	"compound/internal/routeevidence"
	"compound/internal/work"
)

var directorTestNow = time.Date(2026, 7, 6, 9, 30, 0, 0, time.UTC)

func TestBuildProposalRequiresEvidenceRefs(t *testing.T) {
	home := seededDirectorHome(t)
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}

	proposal, skipped := buildProposal(home, store, candidate{
		Kind:             "review_debt",
		SourceID:         "synthetic-review",
		Title:            "Resolve synthetic review debt",
		Consequence:      "Adding this creates bounded synthetic review work",
		CapabilityClass:  "judge-critique",
		Owner:            "compound-tests",
		SuccessPredicate: "resolve synthetic review debt",
	}, directorTestNow)
	if skipped == nil || skipped.Reason != "missing_evidence_refs" {
		t.Fatalf("buildProposal skip = %+v, want missing_evidence_refs", skipped)
	}
	if proposal.ID != "" {
		t.Fatalf("proposal should not be built without evidence: %+v", proposal)
	}
}

func TestProposeDedupesPriorDirectorProposal(t *testing.T) {
	home := seededDirectorHome(t)
	seedDirectorApplyGrant(t, home, directorTestNow)
	seedReviewDebt(t, home, "synthetic-review-debt", "review-debt-01")

	first, err := Propose(home, Options{Apply: true, Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	if first.ProposalCount != 1 || first.Proposals[0].PlanPath == "" || first.Proposals[0].WorkState != "proposed" {
		t.Fatalf("first director apply should record one proposed plan: %+v", first)
	}
	assertHasRef(t, first.Proposals[0].EvidenceRefs, "proposed_by:director")
	assertHasPrefix(t, first.Proposals[0].EvidenceRefs, "director_trigger:")
	assertHasPrefix(t, first.Proposals[0].EvidenceRefs, "route_plan:")
	assertHasRef(t, first.Proposals[0].EvidenceRefs, "review:synthetic-review-debt/review-debt-01")
	if !strings.HasPrefix(first.Proposals[0].Consequence, "Adding this creates ") {
		t.Fatalf("proposal consequence should name add effect: %q", first.Proposals[0].Consequence)
	}
	if len(first.Proposals[0].RoutePlan.Chain) == 0 {
		t.Fatalf("proposal missing suggested route chain: %+v", first.Proposals[0].RoutePlan)
	}
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(first.Proposals[0].WorkID); err == nil {
		t.Fatalf("director apply must record a create-plan preview, not create work %s", first.Proposals[0].WorkID)
	}

	second, err := Propose(home, Options{Apply: true, Now: directorTestNow.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if second.ProposalCount != 0 || second.SuppressedDuplicateCount != 1 {
		t.Fatalf("second apply should suppress prior director proposal: %+v", second)
	}
	dup := second.SuppressedDuplicates[0]
	if dup.Reason != "prior_director_proposal" || len(dup.ExistingPlanIDs) == 0 || len(dup.EvidenceRefs) == 0 {
		t.Fatalf("prior proposal duplicate should carry plan ids and evidence refs: %+v", dup)
	}
}

func TestProposeApplyCreatesRunLevelReceiptTransition(t *testing.T) {
	home := seededDirectorHome(t)
	seedDirectorApplyGrant(t, home, directorTestNow)
	seedReviewDebt(t, home, "synthetic-review-debt", "review-debt-01")

	report, err := Propose(home, Options{Apply: true, Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	if report.ProposalCount != 1 {
		t.Fatalf("director apply should record one proposal: %+v", report)
	}
	if report.RunWorkID == "" || report.RunReceiptID == "" || report.RunTransitionID == "" {
		t.Fatalf("apply run missing enclosing work/receipt/transition ids: %+v", report)
	}
	if report.RunMoveType != "control_plane_repair" || report.NoDelta {
		t.Fatalf("proposal-producing apply should be a changed control-plane run: %+v", report)
	}
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	runWork, err := store.Get(report.RunWorkID)
	if err != nil {
		t.Fatalf("run work %s not stored: %v", report.RunWorkID, err)
	}
	if err := runWork.Validate(); err != nil {
		t.Fatalf("run work invalid: %v", err)
	}
	if runWork.State != "completed" || runWork.IDs.TransitionID != report.RunTransitionID {
		t.Fatalf("run work not closed through transition: %+v", runWork)
	}
	receipt := loadReceipt(t, home, report.RunReceiptID)
	if receipt.IDs.WorkID != report.RunWorkID || receipt.MoveType != "control_plane_repair" || !receipt.StateDelta.Changed {
		t.Fatalf("run receipt not tied to changed run work: %+v", receipt)
	}
	for _, want := range []string{"proposal_count=1", "suppressed_duplicate_count=0", "skipped_count=0"} {
		if !strings.Contains(receipt.StateDelta.Summary, want) {
			t.Fatalf("run receipt summary missing %q: %q", want, receipt.StateDelta.Summary)
		}
	}
	assertHasPrefix(t, receipt.ProofRefs, "director_counts:proposal_count=1,suppressed_duplicate_count=0,skipped_count=0")
	transition := loadTransition(t, home, report.RunTransitionID)
	if transition.IDs.WorkID != report.RunWorkID || !transition.SecondOrder.FutureBehaviorChanged {
		t.Fatalf("run transition not tied to changed run work: %+v", transition)
	}
}

func TestProposeApplyEmptyRunUsesAuditOnlyNoDeltaReceipt(t *testing.T) {
	home := seededDirectorHome(t)
	seedDirectorApplyGrant(t, home, directorTestNow)

	report, err := Propose(home, Options{Apply: true, Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	if report.ProposalCount != 0 || report.RunWorkID == "" || report.RunReceiptID == "" || report.RunTransitionID == "" {
		t.Fatalf("empty apply should still create enclosing run proof: %+v", report)
	}
	if report.RunMoveType != "audit_only" || !report.NoDelta {
		t.Fatalf("empty apply should be explicitly no-delta audit_only: %+v", report)
	}
	receipt := loadReceipt(t, home, report.RunReceiptID)
	if receipt.MoveType != "audit_only" || receipt.StateDelta.Changed {
		t.Fatalf("empty run receipt should count for nothing: %+v", receipt)
	}
	for _, want := range []string{"proposal_count=0", "suppressed_duplicate_count=0", "skipped_count=0", "no_delta=true"} {
		if !strings.Contains(receipt.StateDelta.Summary, want) {
			t.Fatalf("empty run receipt summary missing %q: %q", want, receipt.StateDelta.Summary)
		}
	}
	assertHasRef(t, receipt.ProofRefs, "no_delta:director_apply_empty")
	transition := loadTransition(t, home, report.RunTransitionID)
	if transition.SecondOrder.FutureBehaviorChanged {
		t.Fatalf("audit_only transition should not claim future behavior changed: %+v", transition)
	}
}

func TestProposeDedupesOpenWork(t *testing.T) {
	home := seededDirectorHome(t)
	seedReviewDebt(t, home, "synthetic-review-debt", "review-debt-01")

	dryRun, err := Propose(home, Options{Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	if dryRun.ProposalCount != 1 {
		t.Fatalf("dry run should expose proposal before open-work duplicate: %+v", dryRun)
	}

	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	existing, err := store.Create(work.CreateInput{
		Owner:            "compound-tests",
		Actor:            "operator-test",
		SuccessPredicate: "existing open work blocks duplicate director proposal",
		EvidenceRefs:     []string{dryRun.Proposals[0].TriggerRef},
		State:            "proposed",
	}, directorTestNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(existing.ID, work.AdvanceInput{Actor: "operator-test", To: "ready"}, directorTestNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	afterOpenWork, err := Propose(home, Options{Now: directorTestNow.Add(2 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if afterOpenWork.ProposalCount != 0 || afterOpenWork.SuppressedDuplicateCount != 1 {
		t.Fatalf("open work should suppress proposal: %+v", afterOpenWork)
	}
	dup := afterOpenWork.SuppressedDuplicates[0]
	if dup.Reason != "open_work" || len(dup.ExistingWorkIDs) == 0 || len(dup.EvidenceRefs) == 0 {
		t.Fatalf("open-work duplicate should carry work ids and evidence refs: %+v", dup)
	}
}

func TestProposeDoesNotStarveOtherClasses(t *testing.T) {
	home := seededDirectorHome(t)
	for i := 0; i < 100; i++ {
		seedReviewDebtWithCreatedAt(t, home, "synthetic-review-debt", fmt.Sprintf("review-debt-%03d", i), directorTestNow.Add(time.Duration(i)*time.Minute))
	}
	seedFailingJobHealth(t, home)

	report, err := Propose(home, Options{Now: directorTestNow.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if report.ProposalCount != 5 {
		t.Fatalf("director run should still cap proposals at five: %+v", report)
	}
	if !hasProposalKind(report, "job_health") {
		t.Fatalf("job_health candidate was starved by overflowing review debt: %+v", report.Proposals)
	}
	assertNoHigherSeverityClassCapSkippedBelowSelected(t, report)
}

func TestProposeCapsRunAtFive(t *testing.T) {
	home := seededDirectorHome(t)
	for i := 0; i < 7; i++ {
		seedReviewDebt(t, home, "synthetic-review-debt", "review-debt-cap-"+string(rune('a'+i)))
	}

	report, err := Propose(home, Options{Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	if report.ProposalCount != 5 || report.SkippedCount != 2 {
		t.Fatalf("director run should cap proposals at five: %+v", report)
	}
	for _, skipped := range report.Skipped {
		if skipped.Reason != "cap_reached" || len(skipped.EvidenceRefs) == 0 {
			t.Fatalf("cap skip should preserve evidence refs: %+v", skipped)
		}
	}
}

func TestDirectorProposalsCannotAdvancePastProposedWithoutOperator(t *testing.T) {
	home := seededDirectorHome(t)
	seedDirectorApplyGrant(t, home, directorTestNow)
	seedReviewDebt(t, home, "synthetic-review-debt", "review-debt-01")

	report, err := Propose(home, Options{Apply: true, Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	if report.ProposalCount != 1 {
		t.Fatalf("director apply should record one proposal: %+v", report)
	}
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := store.AdmitCreatePlanPath(report.Proposals[0].PlanPath, directorTestNow.Add(time.Minute), work.AdmitCreatePlanOptions{Actor: "operator-test"})
	if err != nil {
		t.Fatal(err)
	}
	if admitted.WorkState != "proposed" {
		t.Fatalf("admitted director work should remain proposed: %+v", admitted)
	}

	plan, err := store.AdvancePlan(admitted.WorkID, directorTestNow.Add(2*time.Minute), work.AdvancePlanOptions{Actor: "codex", To: "ready"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status == "advance_policy_allowed" || plan.NoAction == "" || !strings.Contains(plan.NoAction, "operator provenance") {
		t.Fatalf("non-operator advance plan should be blocked by provenance: %+v", plan)
	}
	if _, err := store.Advance(admitted.WorkID, work.AdvanceInput{Actor: "codex", To: "ready"}, directorTestNow.Add(3*time.Minute)); err == nil || !strings.Contains(err.Error(), "operator provenance") {
		t.Fatalf("non-operator advance should fail on operator provenance, got %v", err)
	}
	after, err := store.Get(admitted.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "proposed" {
		t.Fatalf("failed non-operator advance mutated director work: %+v", after)
	}

	if _, err := store.Advance(admitted.WorkID, work.AdvanceInput{Actor: "operator-test", To: "ready"}, directorTestNow.Add(4*time.Minute)); err != nil {
		t.Fatalf("operator provenance should be able to release proposed director work: %v", err)
	}
}

func seededDirectorHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	source := filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml")
	if _, err := routeevidence.ImportModelCapabilitiesYAML(home, source, directorTestNow); err != nil {
		t.Fatalf("import route fixture: %v", err)
	}
	return home
}

// seedDirectorApplyGrant records an active director-apply grant decision
// record (grants-as-gates, 2026-07-10) so `Propose(home, Options{Apply:
// true})` — which now refuses without one — can run in tests exercising
// apply-mode behavior other than the grant gate itself.
func seedDirectorApplyGrant(t *testing.T, home string, now time.Time) {
	t.Helper()
	memStore, err := mem.Open(home)
	if err != nil {
		t.Fatalf("open mem store for grant seed: %v", err)
	}
	if _, err := decisions.Record(memStore, home, decisions.RecordInput{
		Kind:       decisions.KindGrant,
		ID:         "director-apply-grant-test",
		Title:      "Grant director --apply (test fixture)",
		ReviewedBy: "operator:test",
		Scope:      "director-apply",
		Now:        now,
	}); err != nil {
		t.Fatalf("seed director-apply grant: %v", err)
	}
}

func seedReviewDebt(t *testing.T, home, area, id string) {
	t.Helper()
	seedReviewDebtWithCreatedAt(t, home, area, id, time.Time{})
}

func seedReviewDebtWithCreatedAt(t *testing.T, home, area, id string, createdAt time.Time) {
	t.Helper()
	payload := map[string]any{
		"id":            id,
		"status":        "review_required",
		"owner":         "compound-tests",
		"evidence_refs": []string{"synthetic:evidence:" + id},
		"next_action":   "decide bounded synthetic follow-up",
	}
	if !createdAt.IsZero() {
		payload["created_at"] = createdAt.UTC().Format(time.RFC3339)
	}
	if _, err := reviewartifact.Write(home, area, id, payload, "  ", "synthetic review debt\n"); err != nil {
		t.Fatalf("write review debt: %v", err)
	}
}

func seedFailingJobHealth(t *testing.T, home string) {
	t.Helper()
	source := filepath.Join(t.TempDir(), "cron-jobs.json")
	data := `{
  "version": 1,
  "generated_at": "2026-07-06T08:00:00Z",
  "jobs": [
    {
      "job_id": "critical-job",
      "name": "critical failing job",
      "enabled": true,
      "state": "scheduled",
      "schedule": "every hour",
      "script": "critical.sh",
      "last_status": "error",
      "last_error": "synthetic failure",
      "last_run_at": "2026-07-06T07:30:00Z"
    }
  ]
}`
	if err := os.WriteFile(source, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.ImportJobRoster(home, source, directorTestNow); err != nil {
		t.Fatal(err)
	}
}

func loadReceipt(t *testing.T, home, id string) envelope.Receipt {
	t.Helper()
	var receipt envelope.Receipt
	readEnvelope(t, home, id, &receipt)
	if err := receipt.Validate(); err != nil {
		t.Fatalf("receipt %s invalid: %v", id, err)
	}
	return receipt
}

func loadTransition(t *testing.T, home, id string) envelope.Transition {
	t.Helper()
	var transition envelope.Transition
	readEnvelope(t, home, id, &transition)
	if err := transition.Validate(); err != nil {
		t.Fatalf("transition %s invalid: %v", id, err)
	}
	return transition
}

func readEnvelope(t *testing.T, home, id string, out any) {
	t.Helper()
	store, err := eventstore.Open(home)
	if err != nil {
		t.Fatalf("read envelope %s: %v", id, err)
	}
	record, err := store.Read(id)
	if err != nil {
		t.Fatalf("read envelope %s: %v", id, err)
	}
	if err := json.Unmarshal(record.Bytes, out); err != nil {
		t.Fatalf("parse envelope %s: %v", id, err)
	}
}

func hasProposalKind(report Report, kind string) bool {
	for _, proposal := range report.Proposals {
		if proposal.Kind == kind {
			return true
		}
	}
	return false
}

func assertNoHigherSeverityClassCapSkippedBelowSelected(t *testing.T, report Report) {
	t.Helper()
	for _, skipped := range report.Skipped {
		if skipped.Reason != "cap_reached" {
			continue
		}
		skippedRank := proposalClassRank(skipped.Class)
		for _, proposal := range report.Proposals {
			selectedClass := candidateClass(candidate{Kind: proposal.Kind})
			if proposalClassRank(selectedClass) > skippedRank {
				t.Fatalf("higher-severity class %s was cap-skipped while lower-severity class %s got a slot: report=%+v", skipped.Class, selectedClass, report)
			}
		}
	}
}

func proposalClassRank(class string) int {
	for i, known := range proposalClassSeverityOrder {
		if class == known {
			return i
		}
	}
	return len(proposalClassSeverityOrder)
}

func assertHasRef(t *testing.T, refs []string, want string) {
	t.Helper()
	for _, ref := range refs {
		if ref == want {
			return
		}
	}
	t.Fatalf("refs missing %q: %v", want, refs)
}

func assertHasPrefix(t *testing.T, refs []string, wantPrefix string) {
	t.Helper()
	for _, ref := range refs {
		if strings.HasPrefix(ref, wantPrefix) {
			return
		}
	}
	t.Fatalf("refs missing prefix %q: %v", wantPrefix, refs)
}
