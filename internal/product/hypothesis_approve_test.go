package product

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"path/filepath"

	"compound/internal/eventstore"
	"compound/internal/reviewartifact"
)

// The five falsifiers of the hypothesis-approve packet (work_6ede073fe356,
// recon work_76974eb65f88 §4). Falsifier 1 (the BuildHypothesisPlan gate)
// lives in hypothesis_plan_test.go. Synthetic product ids and metric names
// only.

const tApproveSentence = "synthetic first-time downloads per 1000 impressions should rise at least 10% within the 28-day window after the onboarding screen ships"

// TestHypothesisApproveRewritesDraftAndKeepsPendingReview is falsifier 2: the
// approval rewrites the candidate in place (placeholder gone, verdict STILL
// park, NextAction forward), records one loop transition, and the hypothesis
// stays in PendingReview (product doctor still counts pending_review: 1). A
// verdict flip to operationalize here is the regression this test exists to
// catch: it would silently drop the approved hypothesis out of the
// measurement path.
func TestHypothesisApproveRewritesDraftAndKeepsPendingReview(t *testing.T) {
	home := t.TempDir()
	productID := "product:synth-notes"
	changeEvent := "v2.4 ships the new onboarding screen"
	candidateID := mintHypothesisDraftFixture(t, home, productID, changeEvent, []string{"synthetic_first_time_downloads"})

	res, err := HypothesisApprove(home, HypothesisApproveInput{
		CandidateID:       candidateID,
		ExpectedEffect:    tApproveSentence,
		Metrics:           []string{"synthetic_first_time_downloads", "synthetic_impressions"},
		ObservationWindow: "28d after change",
		ReviewedBy:        "sandbox-operator",
	}, tProduct)
	if err != nil {
		t.Fatal(err)
	}

	rewritten, err := loadHypothesisCandidate(home, candidateID)
	if err != nil {
		t.Fatal(err)
	}
	if rewritten.ExpectedEffect != tApproveSentence {
		t.Fatalf("expected_effect = %q, want the operator sentence", rewritten.ExpectedEffect)
	}
	if strings.HasPrefix(rewritten.ExpectedEffect, HypothesisDraftExpectedEffectPlaceholderPrefix) {
		t.Fatal("placeholder still present after approval")
	}
	if rewritten.Verdict != candidates.Park {
		t.Fatalf("verdict = %q, want park: flipping it drops the hypothesis out of PendingReview", rewritten.Verdict)
	}
	if rewritten.Result != "unknown" {
		t.Fatalf("result = %q, want unknown", rewritten.Result)
	}
	if strings.TrimSpace(rewritten.NextAction) == "" || strings.Contains(rewritten.NextAction, "replaces the placeholder") {
		t.Fatalf("NextAction must be forward and updated, got %q", rewritten.NextAction)
	}

	store, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Read(res.TransitionID)
	if err != nil {
		t.Fatalf("approval transition %s not recorded: %v", res.TransitionID, err)
	}
	var tr envelope.Transition
	if err := json.Unmarshal(record.Bytes, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.SourceTraceID != "product_hypothesis_approve:"+candidateID {
		t.Fatalf("transition source trace = %q", tr.SourceTraceID)
	}
	if !containsRef(tr.BeforeRefs, "product_hypothesis:"+candidateID) || !containsRef(tr.AfterRefs, "product_hypothesis:"+candidateID) {
		t.Fatalf("transition refs must include product_hypothesis:%s, before=%v after=%v", candidateID, tr.BeforeRefs, tr.AfterRefs)
	}
	if tr.StateDelta == "" || !strings.Contains(tr.StateDelta, "verdict stays park") {
		t.Fatalf("transition summary must name the keep-park decision, got %q", tr.StateDelta)
	}

	// The doctor assertion from the packet: still pending_review: 1.
	doctor, err := DoctorWithOptions(home, productID, DoctorOptions{Now: tProduct})
	if err != nil {
		t.Fatal(err)
	}
	if doctor.PendingReviewCount != 1 || len(doctor.PendingReview) != 1 || doctor.PendingReview[0].ID != candidateID {
		t.Fatalf("pending review = %d (%v), want exactly the approved hypothesis %s", doctor.PendingReviewCount, doctor.PendingReview, candidateID)
	}
	if !isOpenLoopHypothesis(doctor.PendingReview[0]) {
		t.Fatal("approved hypothesis must remain an open loop hypothesis (the measurement path)")
	}
}

// TestHypothesisApproveRefusesPlaceholderSentence is falsifier 3: passing the
// placeholder text as --expected-effect must error. Handing the draft's own
// blank back to it would be a no-op wearing a receipt.
func TestHypothesisApproveRefusesPlaceholderSentence(t *testing.T) {
	home := t.TempDir()
	candidateID := mintHypothesisDraftFixture(t, home, "product:synth-notes", "v2.4 ships the new onboarding screen", []string{"synthetic_first_time_downloads"})
	placeholder := strings.ReplaceAll(strings.ReplaceAll(HypothesisDraftExpectedEffectPlaceholder, "%s", "downloads"), "%s", "the change")

	_, err := HypothesisApprove(home, HypothesisApproveInput{
		CandidateID:    candidateID,
		ExpectedEffect: placeholder,
	}, tProduct)
	if err == nil || !strings.Contains(err.Error(), HypothesisDraftExpectedEffectPlaceholderPrefix) {
		t.Fatalf("placeholder sentence must be refused naming the placeholder, got: %v", err)
	}
	if _, rerr := HypothesisApprove(home, HypothesisApproveInput{CandidateID: candidateID, ExpectedEffect: "  " + placeholder}, tProduct); rerr == nil {
		t.Fatal("placeholder behind whitespace must also be refused")
	}
	unchanged, err := loadHypothesisCandidate(home, candidateID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(unchanged.ExpectedEffect, HypothesisDraftExpectedEffectPlaceholderPrefix) || unchanged.Verdict != candidates.Park {
		t.Fatalf("refused approval must leave the draft untouched, got effect %q verdict %q", unchanged.ExpectedEffect, unchanged.Verdict)
	}
}

// TestHypothesisApproveRefusesResultedAndNonParkedCandidates is falsifier 4:
// an already-resulted candidate (the price-step-2499 shape) must error — the
// loop is closed and the review verdict, not approval, is its door. The
// non-parked variant refuses for the same reason from the other side.
func TestHypothesisApproveRefusesResultedAndNonParkedCandidates(t *testing.T) {
	home := t.TempDir()
	resulted := HypothesisCandidate{
		SchemaVersion:  ProductHypothesisCandidate,
		ID:             "hyp_synth_price_step_resulted",
		Kind:           string(candidates.ProductHypothesis),
		ProductID:      "product:synth-notes",
		ChangeEvent:    "raised the pro tier price one step",
		ExpectedEffect: "pro-tier revenue per install rises without cutting conversion",
		Metrics:        []string{"synthetic_proceeds_per_install"},
		Result:         "supported",
		NextAction:     "keep the price step; test the next acquisition lever",
		Verdict:        candidates.Park,
		CreatedAt:      tProduct.Format(time.RFC3339),
	}
	if err := writeHypothesisCandidate(home, resulted); err != nil {
		t.Fatal(err)
	}
	if _, err := HypothesisApprove(home, HypothesisApproveInput{CandidateID: resulted.ID, ExpectedEffect: tApproveSentence}, tProduct); err == nil || !strings.Contains(err.Error(), "already has result") {
		t.Fatalf("resulted candidate must be refused, got: %v", err)
	}

	operationalized := resulted
	operationalized.ID = "hyp_synth_operationalized"
	operationalized.Result = "unknown"
	operationalized.Verdict = candidates.Operationalize
	if err := writeHypothesisCandidate(home, operationalized); err != nil {
		t.Fatal(err)
	}
	if _, err := HypothesisApprove(home, HypothesisApproveInput{CandidateID: operationalized.ID, ExpectedEffect: tApproveSentence}, tProduct); err == nil || !strings.Contains(err.Error(), "only a parked hypothesis") {
		t.Fatalf("non-parked candidate must be refused, got: %v", err)
	}
}

// TestHypothesisApproveThenPlanBuildsOperatorSentence is falsifier 5: after the
// approval, BuildHypothesisPlan succeeds and the plan's ExpectedEffect is
// exactly the operator's sentence.
func TestHypothesisApproveThenPlanBuildsOperatorSentence(t *testing.T) {
	home := t.TempDir()
	productID := "product:synth-notes"
	candidateID := mintHypothesisDraftFixture(t, home, productID, "v2.4 ships the new onboarding screen", []string{"synthetic_first_time_downloads"})
	if _, err := HypothesisApprove(home, HypothesisApproveInput{CandidateID: candidateID, ExpectedEffect: tApproveSentence, ReviewedBy: "sandbox-operator"}, tProduct); err != nil {
		t.Fatal(err)
	}

	plan, err := BuildHypothesisPlan(home, candidateID, "", tProduct)
	if err != nil {
		t.Fatalf("BuildHypothesisPlan must succeed after approval, got: %v", err)
	}
	if plan.ExpectedEffect != tApproveSentence {
		t.Fatalf("plan ExpectedEffect = %q, want the operator sentence", plan.ExpectedEffect)
	}
	if strings.Contains(plan.HypothesisKey, HypothesisDraftExpectedEffectPlaceholderPrefix) {
		t.Fatalf("hypothesis key still carries the placeholder: %q", plan.HypothesisKey)
	}
}

// TestHypothesisPlaceholderPrefixHeadsTemplate is the coupling falsifier: the
// gate prefix must remain the head of the placeholder template, so renaming
// the template without moving the prefix turns this red instead of silently
// unlocking the gates.
func TestHypothesisPlaceholderPrefixHeadsTemplate(t *testing.T) {
	if !strings.HasPrefix(HypothesisDraftExpectedEffectPlaceholder, HypothesisDraftExpectedEffectPlaceholderPrefix) {
		t.Fatalf("placeholder template %q no longer starts with the gate prefix %q", HypothesisDraftExpectedEffectPlaceholder, HypothesisDraftExpectedEffectPlaceholderPrefix)
	}
}

func containsRef(refs []string, want string) bool {
	for _, ref := range refs {
		if ref == want {
			return true
		}
	}
	return false
}

// TestProductHypothesisQueueItemExposesApproveAction is packet item 3: the
// product-hypotheses review queue item must carry the approve verb as its
// Action — replacing the words-only "owner has not exposed a safe action"
// fallback — so `compound reviews` and the console can actually fire it. Also
// covers the legacy pre-gate plan (expected_effect still the placeholder):
// same action, plus a blocked reason that names the repair path.
func TestProductHypothesisQueueItemExposesApproveAction(t *testing.T) {
	home := t.TempDir()
	productID := "product:synth-notes"
	changeEvent := "v2.4 ships the new onboarding screen"
	candidateID := mintHypothesisDraftFixture(t, home, productID, changeEvent, []string{"synthetic_first_time_downloads"})
	if _, err := HypothesisApprove(home, HypothesisApproveInput{CandidateID: candidateID, ExpectedEffect: tApproveSentence, ReviewedBy: "sandbox-operator"}, tProduct); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildHypothesisPlan(home, candidateID, "", tProduct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordHypothesisPlan(home, plan); err != nil {
		t.Fatal(err)
	}

	report, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: productHypothesisReviewArea}, tProduct)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Items) != 1 {
		t.Fatalf("queue items = %d, want 1", len(report.Items))
	}
	AnnotateReviewQueue(&report)
	reviewartifact.Recount(&report)

	item := report.Items[0]
	if len(item.Actions) != 1 {
		t.Fatalf("item actions = %d, want the approve action", len(item.Actions))
	}
	action := item.Actions[0]
	if action.ID != "product_hypothesis_approve" ||
		action.Argv[0] != "compound" || action.Argv[1] != "product" || action.Argv[2] != "hypothesis-approve" || action.Argv[3] != candidateID {
		t.Fatalf("approve action = %+v", action)
	}
	for _, arg := range action.Argv {
		if strings.HasPrefix(arg, "--") {
			t.Fatalf("action argv must not carry operator-input flags (they are appended by a governed renderer), got %v", action.Argv)
		}
	}
	if !action.RequiresReview {
		t.Fatal("approve action must require review")
	}
	if report.Counts.ReviewRequiredMissingAction != 0 || report.Counts.ActionableCount != 1 {
		t.Fatalf("counts: actionable=%d missing_action=%d, want the item actionable", report.Counts.ActionableCount, report.Counts.ReviewRequiredMissingAction)
	}
	var group *reviewartifact.QueueGroup
	for i := range report.Groups {
		if report.Groups[i].Area == productHypothesisReviewArea {
			group = &report.Groups[i]
		}
	}
	if group == nil {
		t.Fatal("no product-hypotheses group")
	}
	if group.SafeBatchAction == nil || group.SafeBatchAction.ID != "product_hypothesis_approve" {
		t.Fatalf("group must expose the approve action instead of the words-only fallback, got %+v reason=%q", group.SafeBatchAction, group.TerminalReason)
	}
	if group.TerminalReason == "owner has not exposed a safe action for this decision group" {
		t.Fatal("words-only fallback still firing for product-hypotheses")
	}

	// Legacy pre-gate shape: a plan whose expected_effect is still the
	// placeholder. The action stays, and the blocked reason names the repair.
	legacy := plan
	legacy.ID = "php_legacy_placeholder"
	legacy.ExpectedEffect = fmt.Sprintf(HypothesisDraftExpectedEffectPlaceholder, "synthetic_first_time_downloads", changeEvent)
	if _, err := RecordHypothesisPlan(home, legacy); err != nil {
		t.Fatal(err)
	}
	legacyReport, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: productHypothesisReviewArea}, tProduct)
	if err != nil {
		t.Fatal(err)
	}
	AnnotateReviewQueue(&legacyReport)
	var legacyItem *reviewartifact.QueueItem
	for i := range legacyReport.Items {
		if legacyReport.Items[i].ID == legacy.ID {
			legacyItem = &legacyReport.Items[i]
		}
	}
	if legacyItem == nil {
		t.Fatal("legacy placeholder plan missing from queue")
	}
	if len(legacyItem.Actions) != 1 || legacyItem.Actions[0].ID != "product_hypothesis_approve" {
		t.Fatalf("legacy item actions = %+v", legacyItem.Actions)
	}
	if !strings.Contains(legacyItem.BlockedReason, "unfilled draft") || !strings.Contains(legacyItem.NextAction, "re-run product hypothesis-plan") {
		t.Fatalf("legacy item must name the repair path, blocked=%q next=%q", legacyItem.BlockedReason, legacyItem.NextAction)
	}
}

// Approval-integrity contract 2026-09-15: the transition evidence — since the
// intent split, the approval INTENT — is recorded BEFORE the candidate state
// changes. A transition-recording failure must leave the candidate
// byte-identical — no approval sentence, no NextAction rewrite — instead of an
// approved-looking candidate with no receipt (the pre-2026-09-15 order wrote
// the candidate first).
func TestHypothesisApproveTransitionFailureLeavesCandidateUntouched(t *testing.T) {
	home := t.TempDir()
	productID := "product:synth-notes"
	changeEvent := "v2.4 ships the new onboarding screen"
	candidateID := mintHypothesisDraftFixture(t, home, productID, changeEvent, []string{"synthetic_first_time_downloads"})
	before, err := loadHypothesisCandidate(home, candidateID)
	if err != nil {
		t.Fatal(err)
	}
	beforeBytes, readErr := os.ReadFile(filepath.Join(home, candidates.InboxDir(candidates.ProductHypothesis), candidateID+".json"))
	if readErr != nil {
		t.Fatal(readErr)
	}

	restore := swapRecordLoopTransition(func(_ string, _ envelope.Transition) error {
		return fmt.Errorf("eventstore closed mid-approval")
	})
	defer restore()

	if _, err := HypothesisApprove(home, HypothesisApproveInput{
		CandidateID:    candidateID,
		ExpectedEffect: tApproveSentence,
		ReviewedBy:     "sandbox-operator",
	}, tProduct); err == nil {
		t.Fatal("a failing transition record must fail the approval")
	}
	afterBytes, readErr := os.ReadFile(filepath.Join(home, candidates.InboxDir(candidates.ProductHypothesis), candidateID+".json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(afterBytes) != string(beforeBytes) {
		t.Fatalf("a failed transition must leave the candidate untouched; candidate was rewritten:\n%s", afterBytes)
	}
	unchanged, err := loadHypothesisCandidate(home, candidateID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.ExpectedEffect != before.ExpectedEffect || unchanged.Verdict != candidates.Park {
		t.Fatalf("failed transition mutated the candidate: %+v", unchanged)
	}
}

// The exact-generation guard: a concurrent writer who changes the candidate
// between the gate read and the write wins, and the approval refuses instead
// of silently overwriting their state (2026-09-15).
func TestHypothesisApproveRefusesWhenGenerationChanged(t *testing.T) {
	home := t.TempDir()
	productID := "product:synth-notes"
	changeEvent := "v2.4 ships the new onboarding screen"
	candidateID := mintHypothesisDraftFixture(t, home, productID, changeEvent, []string{"synthetic_first_time_downloads"})
	beforeBytes, readErr := os.ReadFile(filepath.Join(home, candidates.InboxDir(candidates.ProductHypothesis), candidateID+".json"))
	if readErr != nil {
		t.Fatal(readErr)
	}

	// The concurrent writer records its transition, then flips the candidate
	// (a review verdict landing in the same window).
	restore := swapRecordLoopTransition(func(homePath string, tr envelope.Transition) error {
		if err := RecordTransition(homePath, tr); err != nil {
			return err
		}
		var raced HypothesisCandidate
		if err := json.Unmarshal(beforeBytes, &raced); err != nil {
			return err
		}
		raced.Verdict = candidates.Reject
		raced.NextAction = "concurrent review verdict won the window"
		return writeHypothesisCandidate(homePath, raced)
	})
	defer restore()

	res, err := HypothesisApprove(home, HypothesisApproveInput{
		CandidateID:    candidateID,
		ExpectedEffect: tApproveSentence,
		ReviewedBy:     "sandbox-operator",
	}, tProduct)
	if err == nil {
		t.Fatalf("a generation change during the approval window must refuse the write, got %+v", res)
	}
	if !strings.Contains(err.Error(), "changed since the gate read") {
		t.Fatalf("refusal must name the generation change, got: %v", err)
	}
	// The concurrent writer's state stands — never overwritten.
	after, err := loadHypothesisCandidate(home, candidateID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Verdict != candidates.Reject || after.NextAction != "concurrent review verdict won the window" {
		t.Fatalf("the concurrent writer's state must stand, got %+v", after)
	}
}

func swapRecordLoopTransition(fn func(string, envelope.Transition) error) func() {
	prev := recordLoopTransition
	recordLoopTransition = fn
	return func() { recordLoopTransition = prev }
}
