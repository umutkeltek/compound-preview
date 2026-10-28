package work

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/runattempt"
)

func TestAdvancePlanEvaluatesPolicyWithoutAdvancing(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "advance plan previews running"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatal(err)
	}
	plan, err := s.AdvancePlan(w.ID, fixedNow, AdvancePlanOptions{Actor: "codex", To: "running", Record: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "advance_policy_allowed" || !plan.Policy.Allowed || plan.ToState != "running" {
		t.Fatalf("advance plan policy = %+v", plan)
	}
	if len(plan.AdvanceCommand) == 0 || plan.AdvanceCommand[0] != "compound" {
		t.Fatalf("advance command missing: %+v", plan.AdvanceCommand)
	}
	if plan.PlanPath == "" {
		t.Fatalf("advance plan was not recorded: %+v", plan)
	}
	if _, err := os.Stat(plan.PlanPath); err != nil {
		t.Fatalf("advance plan artifact missing: %v", err)
	}
	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "claimed" {
		t.Fatalf("advance plan must not mutate work, state=%s", after.State)
	}
}

func TestAdvanceRecordsPolicyAction(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "advance records policy"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(w.ID, "codex", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	// S1: RunAttempt ids are minted externally and attached via RunAttemptIDs. The
	// pre-S1 field `RunID` (which stuffed `run_real` into ActionIntentIDs) was retired
	// when ActionIntentIDs went act_*-only.
	raStore := runattempt.Open(s.home)
	ra, err := raStore.Mint(w.ID, "codex", runattempt.HarnessPlaceholder, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raStore.SetOrigin(ra.ID, runattempt.OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	runAttemptID := ra.ID
	advanced, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "codex", RunAttemptIDs: []string{runAttemptID}}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if advanced.State != "running" || advanced.PolicyDecisionID == "" {
		t.Fatalf("advance did not record policy: %+v", advanced)
	}
	if len(advanced.ActionIntentIDs) != len(claimed.ActionIntentIDs)+1 {
		t.Fatalf("advance action chain count = %d (claimed=%d, want +1)", len(advanced.ActionIntentIDs), len(claimed.ActionIntentIDs))
	}
	if contains(advanced.ActionIntentIDs, "run_real") {
		t.Fatalf("ActionIntentIDs must not contain legacy run id \"run_real\" after S1; got %v", advanced.ActionIntentIDs)
	}
	if !contains(advanced.RunAttemptIDs, runAttemptID) {
		t.Fatalf("RunAttemptIDs missing %q, got %v", runAttemptID, advanced.RunAttemptIDs)
	}
	advanceActionID := advanced.ActionIntentIDs[len(claimed.ActionIntentIDs)]
	if err := raStore.AttachActionIntent(runAttemptID, advanceActionID); err != nil {
		t.Fatalf("bind execution RunAttempt to advance action: %v", err)
	}
	if exists, err := s.eventExists(advanceActionID); err != nil || !exists {
		t.Fatalf("advance action envelope missing: %v", err)
	}
	if exists, err := s.eventExists(advanced.PolicyDecisionID); err != nil || !exists {
		t.Fatalf("advance policy envelope missing: %v", err)
	}
	closed, err := s.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "advance governed",
		ProofRefs: []string{"proof:advance-policy"},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if contains(closed.Receipt.Skills.Used, "work.advance") {
		t.Fatalf("internal advance capability leaked into used skills: %v", closed.Receipt.Skills.Used)
	}
}

func TestAdvancePlanAndAdvanceRejectTerminalTargetsWithoutProoflessTerminal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		from     string
		to       string
		needsRun bool
	}{
		{name: "running to completed", from: "running", to: "completed", needsRun: true},
		{name: "running to failed", from: "running", to: "failed", needsRun: true},
		{name: "ready to cancelled", from: "ready", to: "cancelled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "terminal state belongs to Close"}, fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			if tc.needsRun {
				if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Advance(w.ID, AdvanceInput{Actor: "codex", To: "running"}, fixedNow); err != nil {
					t.Fatal(err)
				}
			}
			before, eventsBefore, err := s.Show(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			if before.State != tc.from {
				t.Fatalf("setup state = %q, want %q", before.State, tc.from)
			}

			if _, err := s.AdvancePlan(w.ID, fixedNow, AdvancePlanOptions{Actor: "codex", To: tc.to, Record: true}); err == nil || !strings.Contains(err.Error(), "use Close") {
				t.Errorf("AdvancePlan %s -> %s error = %v", tc.from, tc.to, err)
			}
			plans, err := filepath.Glob(filepath.Join(s.home, "advance-plans", "*.json"))
			if err != nil {
				t.Fatal(err)
			}
			if len(plans) != 0 {
				t.Errorf("rejected terminal advance plan recorded artifacts: %v", plans)
			}

			if _, err := s.Advance(w.ID, AdvanceInput{Actor: "codex", To: tc.to}, fixedNow); err == nil || !strings.Contains(err.Error(), "use Close") {
				t.Errorf("Advance %s -> %s error = %v", tc.from, tc.to, err)
			}
			after, eventsAfter, err := s.Show(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.State != before.State || len(after.ActionIntentIDs) != len(before.ActionIntentIDs) || after.PolicyDecisionID != before.PolicyDecisionID || after.IDs.TransitionID != "" {
				t.Errorf("rejected terminal advance mutated work: before=%+v after=%+v", before, after)
			}
			if len(eventsAfter) != len(eventsBefore) {
				t.Errorf("rejected terminal advance appended events: before=%+v after=%+v", eventsBefore, eventsAfter)
			}
			for _, pattern := range []string{"rec_*.json", "tr_*.json"} {
				artifacts, err := filepath.Glob(filepath.Join(s.home, "events", pattern))
				if err != nil {
					t.Fatal(err)
				}
				if len(artifacts) != 0 {
					t.Errorf("rejected terminal advance emitted %s artifacts: %v", pattern, artifacts)
				}
			}

			closed, err := s.Close(w.ID, CloseInput{Actor: "codex", Outcome: tc.to, MoveType: "audit_only", Summary: "terminal state entered through Close"}, fixedNow)
			if err != nil {
				t.Fatalf("Close %s -> %s: %v", tc.from, tc.to, err)
			}
			if closed.Work.State != tc.to || closed.Receipt.ID == "" || closed.Transition.ID == "" || closed.Work.IDs.TransitionID != closed.Transition.ID {
				t.Errorf("Close did not emit terminal proof chain: %+v", closed)
			}
		})
	}
}

// Falsifier 5: AdvancePlan (the non-mutating preview) must report the same lease-holder
// denial the real advance would hit, without recording or mutating anything — no
// allowed-preview-then-refused-act divergence. With a valid steal, the preview reports
// steal-readiness and the emitted advance command carries --steal --reason.
func TestAdvancePlanReportsLeaseHolderDenialWithoutMutating(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "advance plan lease preview"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "actor-a", fixedNow); err != nil {
		t.Fatal(err)
	}
	before, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.AdvancePlan(w.ID, fixedNow, AdvancePlanOptions{Actor: "actor-b", To: "running"})
	if err != nil {
		t.Fatalf("advance-plan: %v", err)
	}
	if plan.NoAction == "" || !strings.Contains(plan.NoAction, "actor-a") {
		t.Fatalf("advance-plan must report the lease holder denial, got NoAction=%q", plan.NoAction)
	}
	if len(plan.AdvanceCommand) != 0 {
		t.Fatalf("advance-plan must not offer an advance command when lease-blocked: %v", plan.AdvanceCommand)
	}
	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != before.State || len(after.ActionIntentIDs) != len(before.ActionIntentIDs) {
		t.Fatalf("advance-plan must not mutate work: before=%+v after=%+v", before, after)
	}

	stealPlan, err := s.AdvancePlan(w.ID, fixedNow, AdvancePlanOptions{
		Actor: "actor-b", To: "running",
		Steal: true, StealReason: "actor-a executor is dead",
	})
	if err != nil {
		t.Fatalf("advance-plan with steal: %v", err)
	}
	if len(stealPlan.AdvanceCommand) == 0 {
		t.Fatalf("steal-ready advance-plan must offer an advance command: %+v", stealPlan)
	}
	if !commandHasFlagValue(stealPlan.AdvanceCommand, "--reason", "actor-a executor is dead") {
		t.Fatalf("steal-ready advance command must carry --reason: %v", stealPlan.AdvanceCommand)
	}
	found := false
	for _, part := range stealPlan.AdvanceCommand {
		if part == "--steal" {
			found = true
		}
	}
	if !found {
		t.Fatalf("steal-ready advance command must carry --steal: %v", stealPlan.AdvanceCommand)
	}
}

func TestAdvanceBlockedByPolicyDoesNotMutate(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "advance hold"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(w.ID, "codex", fixedNow)
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
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "codex"}, fixedNow); err == nil {
		t.Fatal("advance should be blocked by policy hold")
	}
	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "claimed" || len(after.ActionIntentIDs) != len(claimed.ActionIntentIDs) || after.PolicyDecisionID != claimed.PolicyDecisionID {
		t.Fatalf("blocked advance mutated work: before=%+v after=%+v", claimed, after)
	}
}
