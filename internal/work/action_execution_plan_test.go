package work

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestActionExecutionPlanConsumesActionContextWithoutExecuting(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx, workID := runningActionContextWork(t, s)
	before, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents, err := s.eventsFor(workID)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := s.ActionExecutionPlan(workID, fixedNow.Add(20*time.Minute), ActionExecutionPlanOptions{Actor: "codex", Record: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "action_execution_policy_allowed" || !plan.Policy.Allowed {
		t.Fatalf("action execution plan policy not allowed: %+v", plan)
	}
	requireWorkActionContext(t, "action execution plan", plan.ActionContext, ctx)
	if !sameStringSlice(plan.ActionCommand, ctx.Command) {
		t.Fatalf("action command mismatch: got=%v want=%v", plan.ActionCommand, ctx.Command)
	}
	if plan.ActionIntent.Capability != "work.action_context.execute" ||
		plan.ActionIntent.Target.ID != ctx.SourceID ||
		plan.ActionIntent.Target.Path != ctx.SourceRef ||
		!strings.Contains(plan.ActionIntent.Preview, strings.Join(ctx.Command, " ")) {
		t.Fatalf("action intent did not consume action context: %+v", plan.ActionIntent)
	}
	if plan.Decision.ActionIntentID != plan.ActionIntent.ID || plan.Policy.PolicyDecisionID != plan.Decision.ID {
		t.Fatalf("policy decision not threaded: policy=%+v decision=%+v", plan.Policy, plan.Decision)
	}
	if plan.ReceiptDraft.Outcome != "completed" ||
		plan.ReceiptDraft.MoveType != "outward_delta" ||
		!contains(plan.ReceiptDraft.ProofRefs, ctx.SourceRef) ||
		!contains(plan.ReceiptDraft.ProofRefs, "product_action_review:parr_exec") ||
		!strings.Contains(plan.ReceiptDraft.StateDelta.Summary, ctx.Proof) {
		t.Fatalf("receipt draft missing action proof boundary: %+v", plan.ReceiptDraft)
	}
	if err := plan.ReceiptDraft.Validate(); err != nil {
		t.Fatalf("receipt draft invalid: %v", err)
	}
	if len(plan.RecordCommand) == 0 ||
		!sameStringSlice(plan.RecordCommand, []string{"compound", "work", "act", workID, "--plan", plan.PlanPath}) {
		t.Fatalf("record command missing: %+v", plan.RecordCommand)
	}
	if len(plan.CloseCommand) == 0 || !sameStringSlice(plan.CloseCommand, []string{"compound", "work", "close-plan", workID, "--action-plan", plan.PlanPath, "--actor", "codex"}) {
		t.Fatalf("close command missing receipt shape: %+v", plan.CloseCommand)
	}
	if plan.PlanPath == "" {
		t.Fatalf("plan path missing: %+v", plan)
	}
	if _, err := os.Stat(plan.PlanPath); err != nil {
		t.Fatalf("plan artifact missing: %v", err)
	}
	loaded, err := LoadActionExecutionPlan(plan.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	if !sameStringSlice(loaded.RecordCommand, plan.RecordCommand) {
		t.Fatalf("record command not persisted: loaded=%+v plan=%+v", loaded.RecordCommand, plan.RecordCommand)
	}
	after, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	afterEvents, err := s.eventsFor(workID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != before.State ||
		len(after.ActionIntentIDs) != len(before.ActionIntentIDs) ||
		after.PolicyDecisionID != before.PolicyDecisionID ||
		len(afterEvents) != len(beforeEvents) {
		t.Fatalf("action execution planning mutated work: before=%+v after=%+v events_before=%d events_after=%d", before, after, len(beforeEvents), len(afterEvents))
	}
	if _, err := s.RecordAction(workID, plan.ActionIntent, plan.Decision, fixedNow.Add(21*time.Minute)); err != nil {
		t.Fatalf("record action from plan failed: %v", err)
	}
	recorded, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(recorded.ActionIntentIDs, plan.ActionIntent.ID) || recorded.PolicyDecisionID != plan.Decision.ID {
		t.Fatalf("record action did not thread plan intent/decision: %+v", recorded)
	}
}

func TestActionExecutionPlanRequiresClaimedOrRunningWork(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx, workID := admittedActionContextWork(t, s)
	plan, err := s.ActionExecutionPlan(workID, fixedNow.Add(20*time.Minute), ActionExecutionPlanOptions{Actor: "codex", Record: true})
	if err != nil {
		t.Fatal(err)
	}
	requireWorkActionContext(t, "not executable action plan", plan.ActionContext, ctx)
	if plan.Status != "not_executable" ||
		!strings.Contains(plan.NoAction, "requires claimed or running work") ||
		len(plan.ActionCommand) == 0 ||
		len(plan.RecordCommand) != 0 {
		t.Fatalf("ready/admitted action work should not be executable: %+v", plan)
	}
	after, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "proposed" {
		t.Fatalf("action execution plan must not advance work: %+v", after)
	}
}

func TestReviewCardActionExecutionPlanPreservesGateLifecycleWithoutExecuting(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	source, err := s.Create(CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "held app-store-connect ASC daily signals need private config before scheduler",
		State:            "held",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	cardID := "operator-gate-" + source.ID + "-app-store-connect-private-config"
	report, err := s.BuildReviewCardWorkPlan(cardID, fixedNow.Add(time.Minute), ReviewCardWorkPlanOptions{Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := s.AdmitCreatePlanPath(report.WorkPlan.PlanPath, fixedNow.Add(2*time.Minute), AdmitCreatePlanOptions{Actor: "operator-test"})
	if err != nil {
		t.Fatal(err)
	}
	workID := report.WorkPlan.WorkID
	if admitted.Status != "admitted" || admitted.ActionContext == nil || admitted.ActionContext.Kind != "workbench_review_card" {
		t.Fatalf("review-card work was not admitted with action context: %+v", admitted)
	}
	if _, err := s.Advance(workID, AdvanceInput{To: "ready", Actor: "codex"}, fixedNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(workID, "codex", fixedNow.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(workID, AdvanceInput{To: "running", Actor: "codex"}, fixedNow.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	before, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents, err := s.eventsFor(workID)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := s.ActionExecutionPlan(workID, fixedNow.Add(6*time.Minute), ActionExecutionPlanOptions{Actor: "codex", Record: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "action_execution_policy_allowed" || !plan.Policy.Allowed {
		t.Fatalf("review-card action execution should be policy-allowed proposal: %+v", plan)
	}
	requireWorkActionContext(t, "review-card action plan", plan.ActionContext, admitted.ActionContext)
	if plan.ActionContext.SourceID != cardID ||
		plan.ActionContext.SourceRef != "compound://workbench/review-cards/"+cardID ||
		!sameStringSlice(plan.ActionCommand, report.ReviewCard.ActionCommand) ||
		!contains(plan.RequiredInputs, "private ASC product mapping config") ||
		!contains(plan.ExpectedValidation, "no product signal apply and no scheduler load") ||
		!contains(plan.StopConditions, "requested action would fetch live ASC data, apply ProductSignal, load scheduler, or clear held work") ||
		!strings.Contains(plan.Boundary, "no live fetch") {
		t.Fatalf("review-card action plan lost gate details: %+v", plan)
	}
	if plan.ActionIntent.Capability != "work.action_context.execute" ||
		plan.ActionIntent.Target.ID != cardID ||
		plan.ActionIntent.Target.Path != "compound://workbench/review-cards/"+cardID ||
		!strings.Contains(plan.ActionIntent.Preview, strings.Join(report.ReviewCard.ActionCommand, " ")) {
		t.Fatalf("review-card action intent did not target the card command: %+v", plan.ActionIntent)
	}
	if plan.ReceiptDraft.MoveType != "outward_delta" ||
		!contains(plan.ReceiptDraft.ProofRefs, "workbench_review_card:"+cardID) ||
		!contains(plan.ReceiptDraft.ProofRefs, "operator_gate:asc-private-config") ||
		!strings.Contains(plan.ReceiptDraft.StateDelta.Summary, report.ReviewCard.Proof) {
		t.Fatalf("review-card receipt draft lost proof refs: %+v", plan.ReceiptDraft)
	}
	if len(plan.RecordCommand) == 0 || !sameStringSlice(plan.RecordCommand, []string{"compound", "work", "act", workID, "--plan", plan.PlanPath}) {
		t.Fatalf("review-card record command missing: %+v", plan.RecordCommand)
	}
	afterPlan, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	afterPlanEvents, err := s.eventsFor(workID)
	if err != nil {
		t.Fatal(err)
	}
	if afterPlan.State != before.State ||
		len(afterPlan.ActionIntentIDs) != len(before.ActionIntentIDs) ||
		afterPlan.PolicyDecisionID != before.PolicyDecisionID ||
		len(afterPlanEvents) != len(beforeEvents) {
		t.Fatalf("review-card planning mutated work before action proof: before=%+v after=%+v events_before=%d events_after=%d", before, afterPlan, len(beforeEvents), len(afterPlanEvents))
	}
	sourceAfterPlan, err := s.Get(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceAfterPlan.State != "held" {
		t.Fatalf("review-card action plan must not clear source held work: %+v", sourceAfterPlan)
	}

	shallowClose, err := s.ClosePlan(workID, fixedNow.Add(7*time.Minute), ClosePlanOptions{Actor: "codex", ActionPlanPath: plan.PlanPath, Record: true})
	if err != nil {
		t.Fatal(err)
	}
	if shallowClose.Status != "not_closeable" || !strings.Contains(shallowClose.NoAction, "not recorded") || len(shallowClose.CloseCommand) != 0 {
		t.Fatalf("close-plan should refuse review-card action before work act records it: %+v", shallowClose)
	}
	if _, err := s.RecordAction(workID, plan.ActionIntent, plan.Decision, fixedNow.Add(8*time.Minute)); err != nil {
		t.Fatalf("record review-card action failed: %v", err)
	}
	recorded, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(recorded.ActionIntentIDs, plan.ActionIntent.ID) || recorded.PolicyDecisionID != plan.Decision.ID {
		t.Fatalf("record action did not thread review-card intent/decision: %+v", recorded)
	}
	closePlan, err := s.ClosePlan(workID, fixedNow.Add(9*time.Minute), ClosePlanOptions{Actor: "codex", ActionPlanPath: plan.PlanPath, Record: true})
	if err != nil {
		t.Fatal(err)
	}
	if closePlan.Status != "close_policy_allowed" || closePlan.ActionExecutionPlanID != plan.ID || closePlan.ReceiptDraft.ID == "" ||
		!contains(closePlan.ReceiptDraft.ProofRefs, "workbench_review_card:"+cardID) {
		t.Fatalf("close-plan did not consume review-card action receipt draft: %+v", closePlan)
	}
	sourceAfterRecord, err := s.Get(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceAfterRecord.State != "held" {
		t.Fatalf("recording review-card action must not clear source held work: %+v", sourceAfterRecord)
	}
}

func runningActionContextWork(t *testing.T, s *Store) (*WorkActionContext, string) {
	t.Helper()
	ctx, workID := admittedActionContextWork(t, s)
	if _, err := s.Advance(workID, AdvanceInput{To: "ready", Actor: "codex"}, fixedNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(workID, "codex", fixedNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(workID, AdvanceInput{To: "running", Actor: "codex"}, fixedNow.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	return ctx, workID
}

func admittedActionContextWork(t *testing.T, s *Store) (*WorkActionContext, string) {
	t.Helper()
	ctx := &WorkActionContext{
		Kind:               "product_action_review",
		SourceRef:          "compound://reviews/product-action-cards/reviewed_exec.json",
		SourceID:           "parr_exec",
		Title:              "Import daily product signal",
		Command:            []string{"compound", "product", "signal", "fixture.yaml"},
		RequiredInputs:     []string{"fixture signal"},
		ExpectedValidation: []string{"product timeline changes"},
		StopConditions:     []string{"missing product authority"},
		Boundary:           "temp home only",
		Proof:              "timeline transition appears",
		EvidenceRefs:       []string{"product_action_review:parr_exec"},
	}
	plan, err := s.CreatePlan(CreateInput{
		WorkID:           "work_product_action_execution",
		Owner:            "product:demo",
		Actor:            "codex",
		State:            "proposed",
		SuccessPredicate: "reviewed action receives execution receipt plan",
		DesiredOutcome:   "future executor records proof without rereading review",
		EvidenceRefs:     []string{"product_action_review:parr_exec"},
	}, fixedNow, CreatePlanOptions{
		Actor:         "codex",
		Capability:    "product.action_review",
		Action:        "plan_work_from_reviewed_product_action",
		TargetKind:    "product_action_review",
		TargetID:      "parr_exec",
		ActionContext: ctx,
		Record:        true,
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitCreatePlan(plan, plan.PlanPath, fixedNow.Add(time.Minute), AdmitCreatePlanOptions{Actor: "operator-test"}); err != nil {
		t.Fatal(err)
	}
	return ctx, plan.WorkID
}
