package work

import (
	"testing"
	"time"
)

func TestContinueRunAdmitsPreparesClaimsAndAdvancesUntilWait(t *testing.T) {
	s := newStore(t)
	plan, err := s.CreatePlan(CreateInput{
		WorkID:           "work_continue_preview",
		Owner:            "compound-core",
		Actor:            "codex",
		State:            "proposed",
		SuccessPredicate: "continue admits and starts suggested work",
		DesiredOutcome:   "safe lifecycle plumbing reaches running",
		EvidenceRefs:     []string{"test://continue-preview"},
	}, fixedNow, CreatePlanOptions{
		Record:        true,
		Capability:    "product.action_review",
		Action:        "plan_work_from_reviewed_product_action",
		TargetKind:    "product_action_review",
		TargetID:      "continue_demo",
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := s.ContinueRun(fixedNow.Add(time.Minute), ContinueRunOptions{
		ContinueOptions: ContinueOptions{Actor: "codex", ExpectWorkID: plan.WorkID},
		MaxSteps:        10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "waiting" || run.StepCount != 4 || run.WaitReason != "run_evidence" {
		t.Fatalf("continue run = %+v", run)
	}
	wantActions := []string{"admit", "prepare", "claim", "advance"}
	for i, want := range wantActions {
		if run.Steps[i].Action != want || run.Steps[i].WorkID != plan.WorkID {
			t.Fatalf("step %d = %+v, want action=%s work=%s", i, run.Steps[i], want, plan.WorkID)
		}
	}
	if !contains(run.BlockedOn, "work.run_evidence:"+plan.WorkID) {
		t.Fatalf("continue run blocked_on = %+v", run.BlockedOn)
	}
	after, err := s.Get(plan.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "running" || after.Authority.Owner != "codex" {
		t.Fatalf("continued work = %+v", after)
	}
}

func TestContinueRunStopsAtCapacityConfirmationInsteadOfRetryingReadyWork(t *testing.T) {
	s := newStore(t)
	plan, err := s.CreatePlan(CreateInput{
		WorkID:           "work_continue_capacity_wait",
		Owner:            "compound-core",
		Actor:            "codex",
		State:            "proposed",
		SuccessPredicate: "continue stops at a capacity confirmation",
	}, fixedNow, CreatePlanOptions{
		Record:        true,
		Capability:    "product.action_review",
		Action:        "plan_work_from_reviewed_product_action",
		TargetKind:    "product_action_review",
		TargetID:      "continue_capacity_wait",
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
	})
	if err != nil {
		t.Fatal(err)
	}
	previousGuard := LaneCapacityGuard
	LaneCapacityGuard = func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		return LaneCapacityGuardResult{
			Capacity:             DispatchLaneCapacity{State: "unknown", DataRef: "test://capacity"},
			ConfirmationRequired: true,
			Message:              "capacity confirmation required",
		}, nil
	}
	t.Cleanup(func() { LaneCapacityGuard = previousGuard })

	run, err := s.ContinueRun(fixedNow.Add(time.Minute), ContinueRunOptions{
		ContinueOptions: ContinueOptions{Actor: "codex", ExpectWorkID: plan.WorkID},
		MaxSteps:        6,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "waiting" || run.StepCount != 3 || run.CanContinue {
		t.Fatalf("continue must stop after admit, prepare, and one capacity-gated claim: %+v", run)
	}
	wantActions := []string{"admit", "prepare", "claim"}
	for i, want := range wantActions {
		if run.Steps[i].Action != want {
			t.Fatalf("step %d = %+v, want action=%s", i, run.Steps[i], want)
		}
	}
	if run.WaitReason != "capacity_confirmation_required" || !contains(run.BlockedOn, "lane.capacity_confirmation:codex") {
		t.Fatalf("capacity wait identity lost: reason=%q blocked=%v", run.WaitReason, run.BlockedOn)
	}
	after, err := s.Get(plan.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "ready" {
		t.Fatalf("capacity confirmation must preserve ready work: %+v", after)
	}
}

func TestContinueLeavesOperatorAdmissionPreviewWaiting(t *testing.T) {
	s := newStore(t)
	plan, err := s.CreatePlan(CreateInput{
		WorkID:           "work_continue_operator_preview",
		Owner:            "compound-core",
		Actor:            "codex",
		State:            "proposed",
		SuccessPredicate: "operator input remains a needs-me preview",
	}, fixedNow, CreatePlanOptions{
		Record:        true,
		Capability:    "product.action_review",
		Action:        "plan_work_from_private_config",
		TargetKind:    "private_config",
		TargetID:      "asc",
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		ActionContext: &WorkActionContext{
			Kind:             "workbench_review_card",
			Title:            "ASC private config",
			ActionSummary:    "operator supplies private config",
			RequiresOperator: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := s.ContinueRun(fixedNow.Add(time.Minute), ContinueRunOptions{
		ContinueOptions: ContinueOptions{Actor: "codex", ExpectWorkID: plan.WorkID},
		MaxSteps:        5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.StepCount != 1 || run.Steps[0].Action != "none" || run.WaitReason != "needs_review" {
		t.Fatalf("operator preview should wait: %+v", run)
	}
	if _, err := s.Get(plan.WorkID); err == nil {
		t.Fatalf("operator-required preview should not be admitted")
	}
}

func TestContinuePrepareRefusesOperatorRequiredWork(t *testing.T) {
	s := newStore(t)
	// An operator-required proposal that has already been admitted into a real
	// proposed envelope (its action-context snapshot rides along). The autonomous
	// continue loop must not silently prepare it to ready: the admit path already
	// refuses operator-required work, and the prepare-candidate path must enforce
	// the same admission gate rather than advancing purely because policy allows.
	operatorPlan, err := s.CreatePlan(CreateInput{
		WorkID:           "work_continue_prepare_operator",
		Owner:            "compound-core",
		Actor:            "codex",
		State:            "proposed",
		SuccessPredicate: "operator input remains a needs-me gate",
	}, fixedNow, CreatePlanOptions{
		Record:        true,
		Capability:    "product.action_review",
		Action:        "plan_work_from_private_config",
		TargetKind:    "private_config",
		TargetID:      "asc",
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		ActionContext: &WorkActionContext{
			Kind:             "workbench_review_card",
			Title:            "ASC private config",
			ActionSummary:    "operator supplies private config",
			RequiresOperator: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitCreatePlanPath(operatorPlan.PlanPath, fixedNow, AdmitCreatePlanOptions{Actor: "codex"}); err != nil {
		t.Fatal(err)
	}

	// An ordinary proposal with no operator gate: the prepare-candidate path must
	// still advance this one, proving the guard is targeted, not a blanket halt.
	ordinary, err := s.Create(CreateInput{Owner: "compound-core", State: "proposed", SuccessPredicate: "ordinary proposed work"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	operatorStep, err := s.Continue(fixedNow.Add(time.Minute), ContinueOptions{Actor: "codex", ExpectWorkID: operatorPlan.WorkID})
	if err != nil {
		t.Fatal(err)
	}
	if operatorStep.Action == "prepare" {
		t.Fatalf("operator-required work must not be prepared, got action=%q status=%q", operatorStep.Action, operatorStep.Status)
	}
	operatorAfter, err := s.Get(operatorPlan.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	if operatorAfter.State != "proposed" {
		t.Fatalf("operator-required work should stay proposed, got %q", operatorAfter.State)
	}
	if operatorStep.CanContinue {
		t.Fatalf("operator-required work must not report can-continue (would busy-loop ContinueRun)")
	}
	if !contains(operatorStep.BlockedOn, "work.ready_state:"+operatorPlan.WorkID) {
		t.Fatalf("operator-required work should report its ready-state block, got blocked_on=%+v", operatorStep.BlockedOn)
	}

	ordinaryStep, err := s.Continue(fixedNow.Add(2*time.Minute), ContinueOptions{Actor: "codex", ExpectWorkID: ordinary.ID})
	if err != nil {
		t.Fatal(err)
	}
	if ordinaryStep.Action != "prepare" || ordinaryStep.WorkID != ordinary.ID {
		t.Fatalf("ordinary proposed work should be prepared, got %+v", ordinaryStep)
	}
	ordinaryAfter, err := s.Get(ordinary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ordinaryAfter.State != "ready" {
		t.Fatalf("ordinary proposed work should advance to ready, got %q", ordinaryAfter.State)
	}
}

func TestContinueExpectWorkPinsPrepareTarget(t *testing.T) {
	s := newStore(t)
	first, err := s.Create(CreateInput{Owner: "compound-core", State: "proposed", SuccessPredicate: "first proposed work"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Create(CreateInput{Owner: "compound-core", State: "proposed", SuccessPredicate: "second proposed work"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	step, err := s.Continue(fixedNow.Add(time.Minute), ContinueOptions{Actor: "codex", ExpectWorkID: second.ID})
	if err != nil {
		t.Fatal(err)
	}
	if step.Action != "prepare" || step.WorkID != second.ID {
		t.Fatalf("expected pinned prepare for %s, got %+v", second.ID, step)
	}
	firstAfter, err := s.Get(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondAfter, err := s.Get(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstAfter.State != "proposed" || secondAfter.State != "ready" {
		t.Fatalf("pinning failed: first=%s second=%s", firstAfter.State, secondAfter.State)
	}
}
