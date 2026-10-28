package work

import (
	"os"
	"path/filepath"
	"testing"

	"compound/internal/policy"
)

func TestClaimPlanEvaluatesPolicyWithoutClaiming(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "claim readiness policy is visible",
		DesiredOutcome:   "claim plan remains proposal-only",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.ClaimPlan(w.ID, fixedNow, ClaimPlanOptions{Actor: "codex", Record: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "claim_policy_allowed" {
		t.Fatalf("status = %q policy=%+v", plan.Status, plan.Policy)
	}
	if !plan.Policy.Allowed || plan.Policy.Outcome != "allow_with_obligations" {
		t.Fatalf("policy = %+v", plan.Policy)
	}
	if plan.ActionIntent.Capability != "work.claim" || plan.Decision.ActionIntentID != plan.ActionIntent.ID {
		t.Fatalf("intent/decision not threaded: intent=%s decision=%s", plan.ActionIntent.ID, plan.Decision.ActionIntentID)
	}
	if len(plan.ClaimCommand) != 6 || plan.ClaimCommand[2] != "claim" || plan.ClaimCommand[3] != w.ID {
		t.Fatalf("claim command = %v", plan.ClaimCommand)
	}
	if plan.PlanPath == "" {
		t.Fatalf("missing recorded plan path: %+v", plan)
	}
	if _, err := os.Stat(plan.PlanPath); err != nil {
		t.Fatalf("recorded plan missing: %v", err)
	}
	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "ready" {
		t.Fatalf("claim plan must not claim work, state=%s", after.State)
	}
	if _, err := s.readLease(w.ID); err == nil {
		t.Fatal("claim plan must not write a lease")
	}
}

func TestClaimPlanReportsStandingGrantCoverage(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	grantPath := filepath.Join(t.TempDir(), "standing-grants.yaml")
	raw := []byte(`authority:
  decision_ref: "grant:test-claim-work"
  reviewed_by: "operator:test"
grants:
- grant_id: grant_claim_work
  capabilities: ["work.claim"]
  target_scope: "**/work/work/*.json"
  mutation_class: local_reversible
  reversibility_ceiling: reversible
  blast_radius: local
`)
	if err := os.WriteFile(grantPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.ImportStandingGrants(s.home, grantPath, fixedNow); err != nil {
		t.Fatal(err)
	}
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "claim grant coverage visible"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.ClaimPlan(w.ID, fixedNow, ClaimPlanOptions{Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Policy.Outcome != "allow_with_obligations" || plan.Policy.StandingGrantID == "" {
		t.Fatalf("expected standing-grant allow_with_obligations, got %+v", plan.Policy)
	}
}

func TestDispatchPlanIncludesClaimPolicySummary(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "dispatch policy visible"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.DispatchPlan(fixedNow, DispatchPlanOptions{Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Selected == nil || plan.Selected.WorkID != w.ID {
		t.Fatalf("selected = %+v", plan.Selected)
	}
	if plan.Selected.ClaimPolicy == nil {
		t.Fatalf("dispatch selected item missing claim policy: %+v", plan.Selected)
	}
	if !plan.Selected.ClaimPolicy.Allowed || plan.Selected.ClaimPolicy.ActionIntentID == "" {
		t.Fatalf("claim policy = %+v", plan.Selected.ClaimPolicy)
	}
}
