package product

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/reviewartifact"
)

func TestActionPlanMakesLoopActionCardReviewableWithoutProductMutation(t *testing.T) {
	home := seedLoopModelHome(t)
	model, err := BuildLoopModel(home, LoopOptions{Recent: 3, Now: loopTestNow})
	if err != nil {
		t.Fatal(err)
	}
	loop := findProductLoop(model.Products, "product:loop-demo")
	card := findLoopActionCard(loop.ActionCards, "product_next_bounded_move")
	if card.ID == "" {
		t.Fatalf("missing next-move action card: %+v", loop.ActionCards)
	}
	before, err := LoadTimeline(home, loop.ProductID)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := BuildActionPlan(home, loop.ProductID, card.ID, "", loopTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "review_required" ||
		plan.CardID != card.ID ||
		plan.CardKind != "product_next_bounded_move" ||
		len(plan.Command) == 0 ||
		plan.ActionSummary == "" ||
		plan.Boundary == "" ||
		plan.Proof == "" {
		t.Fatalf("action plan should preserve action-card command/boundary/proof: %+v", plan)
	}
	if !hasString(plan.EvidenceRefs, "product_review:review_loop_conversion") {
		t.Fatalf("action plan should cite source review evidence: %+v", plan.EvidenceRefs)
	}
	signalGapCard := findLoopActionCard(loop.ActionCards, "product_signal_hypothesis_review")
	if signalGapCard.ID == "" {
		t.Fatalf("missing signal-hypothesis review card: %+v", loop.ActionCards)
	}
	signalGapPlan, err := BuildActionPlan(home, loop.ProductID, signalGapCard.ID, "", loopTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if signalGapPlan.CardKind != "product_signal_hypothesis_review" ||
		!hasString(signalGapPlan.EvidenceRefs, "product_signal:sig_loop_unmatched") ||
		!strings.Contains(signalGapPlan.Boundary, "does not mutate ProductSignal") ||
		!strings.Contains(signalGapPlan.Proof, "product_action_card") {
		t.Fatalf("signal gap action plan should preserve signal evidence and no-mutation boundary: %+v", signalGapPlan)
	}

	paths, err := RecordActionPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.PlanPath, paths.ReviewPath, paths.ReviewYAMLPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing action-plan artifact %s: %v", path, err)
		}
	}

	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: ProductActionReviewArea}, loopTestNow)
	if err != nil {
		t.Fatal(err)
	}
	AnnotateReviewQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.ReviewRequired != 1 || queue.Counts.DecisionCardCount != 1 || queue.Counts.ActionableCount != 1 {
		t.Fatalf("product action card should be reviewable: counts=%+v items=%+v", queue.Counts, queue.Items)
	}
	if len(queue.Items) != 1 || len(queue.Items[0].Actions) != 1 ||
		!strings.Contains(queue.Items[0].Actions[0].Command, "compound product action-review") {
		t.Fatalf("review queue should expose product action-review command: %+v", queue.Items)
	}

	reviewDraft, err := os.ReadFile(paths.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	reviewYAML := strings.ReplaceAll(string(reviewDraft), "TODO_decision_propose_work_park_reject", "propose_work")
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_rationale", "make this reviewed card available for a later work proposal")
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_confidence_0_to_1", "0.7")
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_reviewed_by_actor", "operator")
	reviewPath := filepath.Join(filepath.Dir(paths.ReviewYAMLPath), "reviewed-action.review.yaml")
	if err := os.WriteFile(reviewPath, []byte(reviewYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	review, err := LoadActionReview(reviewPath)
	if err != nil {
		t.Fatal(err)
	}
	report, err := ApplyActionReview(home, review, reviewPath, loopTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "recorded" || report.Decision != "propose_work" || report.ReviewedPath == "" {
		t.Fatalf("action review report = %+v", report)
	}
	draft, err := BuildActionWorkDraft(home, report.ReviewedPath)
	if err != nil {
		t.Fatal(err)
	}
	if draft.Status != "work_plan_ready" ||
		draft.WorkOwner != loop.ProductID ||
		draft.WorkID == "" ||
		draft.ActionSummary != plan.ActionSummary ||
		!strings.Contains(draft.SuccessPredicate, plan.ActionSummary) ||
		strings.Contains(draft.SuccessPredicate, "product action-plan") ||
		!strings.Contains(draft.SuccessPredicate, plan.Proof) ||
		!hasString(draft.NonGoals, "do not treat product action-plan review-gate command as completed product action") ||
		!hasString(draft.NonGoals, "do not exceed reviewed boundary: "+plan.Boundary) ||
		!hasString(draft.EvidenceRefs, "product_action_review:"+report.ID) ||
		!hasString(draft.EvidenceRefs, "product_action_plan:"+plan.ID) {
		t.Fatalf("action work draft should preserve command/boundary/proof/evidence: %+v", draft)
	}
	after, err := LoadTimeline(home, loop.ProductID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Signals) != len(before.Signals) ||
		len(after.Hypotheses) != len(before.Hypotheses) ||
		len(after.Transitions) != len(before.Transitions) {
		t.Fatalf("action review must not mutate product loop ledgers: before=%+v after=%+v", before, after)
	}

	queue, err = reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: ProductActionReviewArea}, loopTestNow)
	if err != nil {
		t.Fatal(err)
	}
	AnnotateReviewQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.ReviewRequired != 0 || queue.Counts.OK != 1 || queue.Items[0].Readiness != "no_action" {
		t.Fatalf("recorded action review should make queue terminal: counts=%+v items=%+v", queue.Counts, queue.Items)
	}
}
