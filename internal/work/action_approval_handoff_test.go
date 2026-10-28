package work

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/policy"
)

func TestActionApprovalHandoffRefusalsPreserveGrant(t *testing.T) {
	for _, scenario := range []string{"held", "completed", "actor", "work", "leasework", "deny", "security", "expired", "corrupt-log"} {
		t.Run(scenario, func(t *testing.T) {
			s, w, ai := approvalHandoffFixture(t)
			if _, err := s.EvaluateAndRecordAction(w.ID, ai, fixedNow.Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
			grant := approveHandoffFixture(t, s, ai)
			at := fixedNow.Add(5 * time.Second)
			switch scenario {
			case "held", "completed":
				// Fixture-only write: probe preflight without adding unrelated close proofs.
				w.State = scenario
				if err := s.writeJSON(s.workPath(w.ID), &w); err != nil {
					t.Fatal(err)
				}
			case "actor":
				ai.ID = "act_wrong_actor"
				ai.Actor = "other"
			case "leasework":
				lease, err := s.readLease(w.ID)
				if err != nil {
					t.Fatal(err)
				}
				lease.WorkID = "work_other"
				if err := s.writeJSON(s.leasePath(w.ID), &lease); err != nil {
					t.Fatal(err)
				}
			case "work":
				ai.ID = "act_wrong_work"
				ai.IDs.WorkID = "work_other"
			case "security":
				ai.ID = "act_security_floor"
				ai.SecurityFloorOutcome = "deny"
				ai.SecurityReasons = []string{"fixture floor"}
			case "deny":
				if err := os.MkdirAll(filepath.Join(s.home, "config"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(s.home, "config", "policy.yaml"), []byte("class_outcome:\n  external_side_effect:\n    outcome: deny\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "expired":
				at = fixedNow.Add(10 * time.Minute)
			case "corrupt-log":
				if err := os.WriteFile(filepath.Join(s.home, "events", "work.jsonl"), []byte("invalid fixture log\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := s.EvaluateAndRecordAction(w.ID, ai, at)
			if result.Recorded || (err == nil && policy.Allowed(result.Decision)) {
				t.Fatalf("refusal authorized: %+v err=%v", result, err)
			}
			status, err := policy.NewExactActionApprovalStore(s.home).Check(grant.ID, fixedNow.Add(5*time.Second))
			if err != nil || status.Status != policy.ExactApprovalStatusReady {
				t.Fatalf("refusal consumed grant: %+v %v", status, err)
			}
		})
	}
}

func approvalHandoffFixture(t *testing.T) (*Store, envelope.WorkEnvelope, envelope.ActionIntent) {
	t.Helper()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "fixture", SuccessPredicate: "exact approval works"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	w, err = s.Claim(w.ID, "fixture", fixedNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	ai := envelope.ActionIntent{
		Header: newHeader(envelope.SVActionIntent, "act_handoff_fixture", fixedNow, w.ID, "", "private", "tier1"),
		Actor:  "fixture", Capability: "fixture.send", Action: "send",
		Target:        envelope.ActionTarget{Kind: "recipient", ID: "person@example.invalid"},
		MutationClass: "external_side_effect", Reversibility: "manual", BlastRadius: "local",
		Preview: "send the reviewed fixture message", RollbackPlan: "stop before sending",
	}
	return s, w, ai
}

func approveHandoffFixture(t *testing.T, s *Store, ai envelope.ActionIntent) policy.ExactActionApproval {
	t.Helper()
	cfg, _, err := policy.LoadConfig(s.home)
	if err != nil {
		t.Fatal(err)
	}
	as := policy.NewExactActionApprovalStore(s.home)
	p, _, err := as.Plan(ai.ID, cfg, 5*time.Minute, fixedNow.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := as.Approve(p.ID, "operator:fixture", cfg, fixedNow.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestActionApprovalHandoffHeldProposalIsReviewableNotPerformed(t *testing.T) {
	s, before, ai := approvalHandoffFixture(t)
	events, err := s.eventsFor(before.ID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.EvaluateAndRecordAction(before.ID, ai, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if r.Recorded || r.Decision.Outcome != "hold" {
		t.Fatalf("not held: %+v", r)
	}
	after, err := s.Get(before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.ActionIntentIDs) != len(before.ActionIntentIDs) || after.PolicyDecisionID != before.PolicyDecisionID {
		t.Fatal("proposal changed performed work history")
	}
	afterEvents, err := s.eventsFor(before.ID)
	if err != nil || len(afterEvents) != len(events) {
		t.Fatalf("proposal changed lifecycle: %v", err)
	}
	es, err := eventstore.Open(s.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := es.Read(ai.ID); err != nil {
		t.Fatalf("proposal not persisted: %v", err)
	}
	approveHandoffFixture(t, s, ai)
}

func TestActionApprovalHandoffChangedPayloadCannotBurnGrant(t *testing.T) {
	s, w, ai := approvalHandoffFixture(t)
	if _, err := s.EvaluateAndRecordAction(w.ID, ai, fixedNow.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	grant := approveHandoffFixture(t, s, ai)
	changed := ai
	changed.Target.ID = "other@example.invalid"
	if _, err := s.EvaluateAndRecordAction(w.ID, changed, fixedNow.Add(5*time.Second)); err == nil {
		t.Fatal("same ID changed body accepted")
	}
	status, err := policy.NewExactActionApprovalStore(s.home).Check(grant.ID, fixedNow.Add(5*time.Second))
	if err != nil || status.Status != policy.ExactApprovalStatusReady {
		t.Fatalf("grant burned: %+v %v", status, err)
	}
	changed.ID = "act_handoff_other_target"
	r, err := s.EvaluateAndRecordAction(w.ID, changed, fixedNow.Add(5*time.Second))
	if err != nil || r.Recorded || policy.Allowed(r.Decision) {
		t.Fatalf("changed target allowed: %+v %v", r, err)
	}
	r, err = s.EvaluateAndRecordAction(w.ID, ai, fixedNow.Add(6*time.Second))
	if err != nil || !r.Recorded || !policy.Allowed(r.Decision) {
		t.Fatalf("approved retry: %+v %v", r, err)
	}
	if _, err = s.EvaluateAndRecordAction(w.ID, ai, fixedNow.Add(7*time.Second)); err == nil {
		t.Fatal("performed action replay accepted")
	}
}

func TestActionApprovalHandoffConcurrentRetryConsumesOnce(t *testing.T) {
	s, w, ai := approvalHandoffFixture(t)
	if _, err := s.EvaluateAndRecordAction(w.ID, ai, fixedNow.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	approveHandoffFixture(t, s, ai)
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.EvaluateAndRecordAction(w.ID, ai, fixedNow.Add(5*time.Second))
			results <- err == nil && r.Recorded
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("approval retry deadlocked")
	}
	close(results)
	count := 0
	for ok := range results {
		if ok {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("recorded %d times", count)
	}
}

func TestActionApprovalHandoffHaltLeavesApprovalUnconsumed(t *testing.T) {
	s, w, ai := approvalHandoffFixture(t)
	if _, err := s.EvaluateAndRecordAction(w.ID, ai, fixedNow.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	grant := approveHandoffFixture(t, s, ai)
	if err := policy.WriteAutonomyHalt(s.home, policy.AutonomyHalt{Global: true, Reason: "fixture pause"}); err != nil {
		t.Fatal(err)
	}
	r, err := s.EvaluateAndRecordAction(w.ID, ai, fixedNow.Add(5*time.Second))
	if err != nil || r.Recorded || r.Decision.Outcome != "deny" {
		t.Fatalf("halt ignored: %+v %v", r, err)
	}
	status, err := policy.NewExactActionApprovalStore(s.home).Check(grant.ID, fixedNow.Add(5*time.Second))
	if err != nil || status.Status != policy.ExactApprovalStatusReady {
		t.Fatalf("halt consumed grant: %+v %v", status, err)
	}
}
