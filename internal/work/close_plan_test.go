package work

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClosePlanEvaluatesPolicyWithoutClosing(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	workID := readyRunningWork(t, s)
	plan, err := s.ClosePlan(workID, fixedNow, ClosePlanOptions{
		Actor:    "codex",
		Outcome:  "completed",
		MoveType: "control_plane_repair",
		Record:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "close_policy_allowed" || !plan.Policy.Allowed || plan.PlanPath == "" {
		t.Fatalf("close plan not allowed/recorded: %+v", plan)
	}
	if len(plan.CloseCommand) == 0 || plan.CloseCommand[0] != "compound" {
		t.Fatalf("close command missing: %+v", plan.CloseCommand)
	}
	if _, err := os.Stat(plan.PlanPath); err != nil {
		t.Fatalf("close plan artifact missing: %v", err)
	}
	after, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "running" {
		t.Fatalf("close plan must not mutate work: %+v", after)
	}
}

func TestCloseActionIntentIDSeparatesDelimiterAmbiguousLeaseAuthority(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "close identity frames authority fields"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	firstLease := Lease{
		WorkID:  w.ID,
		By:      "holder|reason",
		ClaimAt: "2026-08-29T00:00:00Z",
	}
	if err := s.writeJSON(s.leasePath(w.ID), &firstLease); err != nil {
		t.Fatal(err)
	}
	first, err := s.ClosePlan(w.ID, fixedNow, ClosePlanOptions{
		Actor:       "closer",
		Outcome:     "cancelled",
		MoveType:    "audit_only",
		Steal:       true,
		StealReason: "tail",
	})
	if err != nil {
		t.Fatal(err)
	}

	secondLease := Lease{
		WorkID:  w.ID,
		By:      "holder",
		ClaimAt: "2026-08-29T00:00:00Z",
	}
	if err := s.writeJSON(s.leasePath(w.ID), &secondLease); err != nil {
		t.Fatal(err)
	}
	second, err := s.ClosePlan(w.ID, fixedNow, ClosePlanOptions{
		Actor:       "closer",
		Outcome:     "cancelled",
		MoveType:    "audit_only",
		Steal:       true,
		StealReason: "reason|tail",
	})
	if err != nil {
		t.Fatal(err)
	}

	if first.ActionIntent.ID == second.ActionIntent.ID {
		t.Fatalf("delimiter-ambiguous authority tuples collided at %s", first.ActionIntent.ID)
	}
}

func TestClosePlanSurfacesObservedRunEvidenceWithoutClosing(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := runningWork(t, s, "close plan uses observed evidence")
	trace := writeTrace(t, s, "trace_close_plan_observed", w.ID)
	observed, err := s.ObserveRun(w.ID, ObserveRunInput{
		Actor:   "codex",
		TraceID: trace.ID,
		Note:    "close plan should show this observation",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.ClosePlan(w.ID, fixedNow, ClosePlanOptions{
		Actor:    "codex",
		Outcome:  "completed",
		MoveType: "control_plane_repair",
		Record:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "close_policy_allowed" || !plan.Policy.Allowed || plan.PlanPath == "" {
		t.Fatalf("close plan not allowed/recorded: %+v", plan)
	}
	if !contains(plan.ProofRefs, trace.ID) || !contains(plan.VerificationIDs, trace.ID) {
		t.Fatalf("close plan missing verification proof: %+v", plan)
	}
	if len(plan.RunObservations) != 1 {
		t.Fatalf("run observations = %+v", plan.RunObservations)
	}
	if plan.RunObservations[0].ID != observed.ID || plan.RunObservations[0].TraceID != trace.ID || plan.RunObservations[0].Note != "close plan should show this observation" {
		t.Fatalf("run observation summary = %+v observed=%+v", plan.RunObservations[0], observed)
	}
	if !commandHasFlagValue(plan.CloseCommand, "--proof", trace.ID) {
		t.Fatalf("close command missing proof ref: %+v", plan.CloseCommand)
	}
	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "verifying" || after.IDs.TransitionID != "" {
		t.Fatalf("close plan must not close observed work: %+v", after)
	}
}

func TestClosePlanConsumesRecordedActionExecutionPlanReceiptDraft(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx, workID := runningActionContextWork(t, s)
	actionPlan, err := s.ActionExecutionPlan(workID, fixedNow, ActionExecutionPlanOptions{Actor: "codex", Record: true})
	if err != nil {
		t.Fatal(err)
	}

	shallow, err := s.ClosePlan(workID, fixedNow, ClosePlanOptions{Actor: "codex", ActionPlanPath: actionPlan.PlanPath, Record: true})
	if err != nil {
		t.Fatal(err)
	}
	if shallow.Status != "not_closeable" || !strings.Contains(shallow.NoAction, "not recorded") || len(shallow.CloseCommand) != 0 {
		t.Fatalf("close-plan should refuse action-plan before work act records it: %+v", shallow)
	}

	if _, err := s.RecordAction(workID, actionPlan.ActionIntent, actionPlan.Decision, fixedNow); err != nil {
		t.Fatalf("record planned action failed: %v", err)
	}
	plan, err := s.ClosePlan(workID, fixedNow, ClosePlanOptions{Actor: "codex", ActionPlanPath: actionPlan.PlanPath, Record: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "close_policy_allowed" || !plan.Policy.Allowed {
		t.Fatalf("close-plan did not allow recorded action plan: %+v", plan)
	}
	if plan.ActionExecutionPlanID != actionPlan.ID || plan.ActionExecutionPlanPath != actionPlan.PlanPath {
		t.Fatalf("close-plan did not thread action execution plan identity: %+v", plan)
	}
	if plan.Outcome != actionPlan.ReceiptDraft.Outcome || plan.MoveType != actionPlan.ReceiptDraft.MoveType {
		t.Fatalf("close-plan did not consume receipt draft outcome/move type: got=%s/%s want=%s/%s", plan.Outcome, plan.MoveType, actionPlan.ReceiptDraft.Outcome, actionPlan.ReceiptDraft.MoveType)
	}
	if !contains(plan.ProofRefs, ctx.SourceRef) || !contains(plan.ExpectedValidation, ctx.ExpectedValidation[0]) || !contains(plan.ReceiptDraft.ProofRefs, ctx.SourceRef) {
		t.Fatalf("close-plan did not carry receipt proof/validation: %+v", plan)
	}
	if !commandHasFlagValue(plan.CloseCommand, "--proof", ctx.SourceRef) {
		t.Fatalf("close command missing action receipt proof ref: %+v", plan.CloseCommand)
	}
	after, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "running" || after.IDs.TransitionID != "" {
		t.Fatalf("close-plan must not close work: %+v", after)
	}
}

func TestClosePlanRequiresDeliveryContractForCommitProofFromActionPlan(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	_, workID := runningActionContextWork(t, s)
	actionPlan, err := s.ActionExecutionPlan(workID, fixedNow, ActionExecutionPlanOptions{Actor: "codex", Record: true})
	if err != nil {
		t.Fatal(err)
	}
	commitRef := "commit:" + strings.Repeat("a", 40)
	actionPlan.ProofRefs = mergeUnique(actionPlan.ProofRefs, []string{commitRef})
	actionPlan.ReceiptDraft.ProofRefs = mergeUnique(actionPlan.ReceiptDraft.ProofRefs, []string{commitRef})
	if err := s.writeJSON(actionPlan.PlanPath, &actionPlan); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordAction(workID, actionPlan.ActionIntent, actionPlan.Decision, fixedNow); err != nil {
		t.Fatal(err)
	}

	plan, err := s.ClosePlan(workID, fixedNow, ClosePlanOptions{Actor: "codex", ActionPlanPath: actionPlan.PlanPath})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "not_closeable" || len(plan.CloseCommand) != 0 || !strings.Contains(plan.NoAction, "git_delivery") {
		t.Fatalf("action-plan commit proof without delivery contract must not be closeable: %+v", plan)
	}
}

// TestClosePlanEnumeratesEveryUnmetPreconditionAsNamedChecks pins the one-read contract:
// a not_closeable close-plan must carry EVERY unmet close precondition as a named check
// with a met/unmet verdict — the required work state (close to "completed" needs
// running/verifying), the accepted move types ("system_delta" is NOT a move type), and the
// --by lease holder requirement — not a bare verdict that reveals one refusal per
// trial-and-error re-run. The work here fails exactly two of the three: it is claimed
// (not running/verifying) and requests move_type "system_delta", while --by "codex"
// matches the lease holder, so that check is the met one. Assertions decode the plan's
// serialized JSON (the recorded artifact and --json surface operators read) so the checks
// must survive on the wire, not just in memory.
func TestClosePlanEnumeratesEveryUnmetPreconditionAsNamedChecks(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "close preconditions enumerated in one read"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatal(err)
	}
	plan, err := s.ClosePlan(w.ID, fixedNow, ClosePlanOptions{
		Actor:    "codex",
		Outcome:  "completed",
		MoveType: "system_delta",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "not_closeable" {
		t.Fatalf("close plan status = %q, want not_closeable", plan.Status)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	type planCheck struct {
		Name   string `json:"name"`
		Met    bool   `json:"met"`
		Detail string `json:"detail"`
	}
	var decoded struct {
		Checks []planCheck `json:"checks"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	checks := map[string]planCheck{}
	for _, check := range decoded.Checks {
		checks[check.Name] = check
	}
	for _, name := range []string{"work_state", "move_type", "lease_holder"} {
		if _, ok := checks[name]; !ok {
			t.Fatalf("close plan missing named check %q with met/unmet; checks = %+v (serialized plan: %s)", name, decoded.Checks, raw)
		}
	}
	state := checks["work_state"]
	if state.Met {
		t.Fatalf("work_state check must be unmet for claimed work: %+v", state)
	}
	if !strings.Contains(state.Detail, "running") || !strings.Contains(state.Detail, "verifying") {
		t.Fatalf("work_state detail must name the required work states (running/verifying): %+v", state)
	}
	move := checks["move_type"]
	if move.Met {
		t.Fatalf("move_type check must be unmet for system_delta: %+v", move)
	}
	if !strings.Contains(move.Detail, "system_delta") || !strings.Contains(move.Detail, "outward_delta") || !strings.Contains(move.Detail, "audit_only") {
		t.Fatalf("move_type detail must name the requested type and the accepted move types: %+v", move)
	}
	holder := checks["lease_holder"]
	if !holder.Met {
		t.Fatalf("lease_holder check must be met when --by matches the lease holder: %+v", holder)
	}
}

func TestCloseRecordsPolicyAction(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	workID := readyRunningWork(t, s)
	before, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := s.Close(workID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Actor:     "codex",
		Summary:   "close governed",
		ProofRefs: []string{"proof:close-policy"},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Work.State != "completed" || closed.Work.PolicyDecisionID == "" {
		t.Fatalf("close did not record policy: %+v", closed.Work)
	}
	if len(closed.Work.ActionIntentIDs) != len(before.ActionIntentIDs)+1 {
		t.Fatalf("close run chain = %v, before=%v", closed.Work.ActionIntentIDs, before.ActionIntentIDs)
	}
	closeActionID := closed.Work.ActionIntentIDs[len(before.ActionIntentIDs)]
	if exists, err := s.eventExists(closeActionID); err != nil || !exists {
		t.Fatalf("close action envelope missing: %v", err)
	}
	if exists, err := s.eventExists(closed.Work.PolicyDecisionID); err != nil || !exists {
		t.Fatalf("close policy envelope missing: %v", err)
	}
	if !contains(closed.Transition.ActionRefs, closeActionID) {
		t.Fatalf("transition missing close action ref: %+v", closed.Transition.ActionRefs)
	}
	if contains(closed.Receipt.Skills.Used, "work.close") {
		t.Fatalf("internal close capability leaked into used skills: %v", closed.Receipt.Skills.Used)
	}
}

func TestCloseBlockedByPolicyDoesNotMutate(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	workID := readyRunningWork(t, s)
	before, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.home, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	policyYAML := []byte(`class_outcome:
  local_reversible:
    outcome: hold
`)
	if err := os.WriteFile(filepath.Join(s.home, "config", "policy.yaml"), policyYAML, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Close(workID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Actor:     "codex",
		Summary:   "blocked close",
		ProofRefs: []string{"proof:close-policy"},
	}, fixedNow); err == nil {
		t.Fatal("close should be blocked by policy hold")
	}
	after, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "running" || len(after.ActionIntentIDs) != len(before.ActionIntentIDs) || after.PolicyDecisionID != before.PolicyDecisionID || after.IDs.TransitionID != "" {
		t.Fatalf("blocked close mutated work: before=%+v after=%+v", before, after)
	}
}

func commandHasFlagValue(command []string, flag, value string) bool {
	for i := 0; i+1 < len(command); i++ {
		if command[i] != flag {
			continue
		}
		for _, part := range strings.Split(command[i+1], ",") {
			if part == value {
				return true
			}
		}
	}
	return false
}

func readyRunningWork(t *testing.T, s *Store) string {
	t.Helper()
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "close policy work"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatal(err)
	}
	running, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "codex"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	return running.ID
}
