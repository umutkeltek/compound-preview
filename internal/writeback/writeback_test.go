package writeback

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/envelope"
)

var testNow = time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)

func TestMutatingGateRequiresAllowedPolicy(t *testing.T) {
	gate := baseGate()
	pd := policyDecision("hold")

	checked, err := EvaluateGate(gate, &pd, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if checked.Status != GateHeld {
		t.Fatalf("status = %q, want %q", checked.Status, GateHeld)
	}
}

func TestMutatingGateAllowsPolicy(t *testing.T) {
	gate := baseGate()
	pd := policyDecision("allow")

	checked, err := EvaluateGate(gate, &pd, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if checked.Status != GateAllowed {
		t.Fatalf("status = %q, want %q", checked.Status, GateAllowed)
	}

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordGate(checked); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.home, "ledger", "owner-writeback-gates.jsonl")); err != nil {
		t.Fatal(err)
	}
}

func TestNoPromoteDecisionIsIdempotentForActiveCandidate(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, created, err := store.RecordNoPromote(baseNoPromote(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatalf("first no-promote should create")
	}
	second, created, err := store.RecordNoPromote(baseNoPromote(), testNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatalf("second active no-promote should reuse existing decision")
	}
	if second.ID != first.ID {
		t.Fatalf("idempotent replay returned %q, want %q", second.ID, first.ID)
	}
}

func TestNoPromoteRequiresEvidence(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := baseNoPromote()
	d.EvidenceRefs = nil
	if _, _, err := store.RecordNoPromote(d, testNow); err == nil {
		t.Fatalf("expected missing evidence error")
	}
}

func baseGate() OwnerWritebackGate {
	return OwnerWritebackGate{
		CandidateRef:      "cand_memory_1",
		CandidateKind:     "memory",
		OwnerTarget:       OwnerTarget{Kind: "brain_page", Ref: "brain://project/test"},
		GateBasis:         "candidate would update owner truth",
		ActionIntentRef:   "act_writeback_1",
		MutationClass:     "local_reversible",
		PolicyDecisionRef: "pol_writeback_1",
		EvidenceRefs:      []string{"ev_runtime_1"},
	}
}

func baseNoPromote() NoPromoteDecision {
	return NoPromoteDecision{
		CandidateRef:  "cand_memory_1",
		CandidateKind: "memory",
		OwnerTarget:   OwnerTarget{Kind: "brain_page", Ref: "brain://project/test"},
		Basis:         "candidate repeats lower-quality information already covered by owner",
		EvidenceRefs:  []string{"ev_review_1"},
	}
}

func policyDecision(outcome string) envelope.PolicyDecision {
	return envelope.PolicyDecision{
		Header: envelope.Header{
			SchemaVersion: envelope.SVPolicyDecision,
			ID:            "pol_writeback_1",
			CreatedAt:     testNow.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "test", Name: "writeback-test"},
			IDs:           envelope.IDs{WorkID: "work_test"},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		ActionIntentID: "act_writeback_1",
		Outcome:        outcome,
		Reasons:        []string{"test"},
		Obligations:    []string{},
	}
}
