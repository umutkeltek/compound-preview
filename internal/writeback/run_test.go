package writeback

import (
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/work"
)

// testActionIntent mirrors policyDecision()'s header so RecordAction accepts it,
// and references the same ids baseGate()/policyDecision() agree on.
func testActionIntent(workID string) envelope.ActionIntent {
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            "act_writeback_1",
			CreatedAt:     testNow.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "test", Name: "writeback-test"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "writeback-test",
		Capability:    "test.writeback",
		Action:        "record_test_writeback_gate",
		Target:        envelope.ActionTarget{Kind: "test", ID: "t1"},
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       "test writeback",
		RollbackPlan:  "remove COMPOUND_HOME test records",
	}
}

func runnerInput(home, policyOutcome string) GatedWriteback {
	return GatedWriteback{
		Home: home,
		Now:  testNow,
		Work: work.CreateInput{
			Owner:            "test:writeback",
			DesiredOutcome:   "exercise the gated writeback runner",
			SuccessPredicate: "gate recorded with receipt/transition refs",
			NonGoals:         []string{"mutate owner files"},
			State:            "ready",
			Risk:             "tier1",
			Privacy:          "private",
		},
		Actor:           "writeback-test",
		RunningNote:     "running writeback",
		CheckedNote:     "writeback gate checked",
		MoveType:        "control_plane_repair",
		ActionIntentFor: func(workID string) envelope.ActionIntent { return testActionIntent(workID) },
		Evaluate:        func(envelope.ActionIntent) envelope.PolicyDecision { return policyDecision(policyOutcome) },
		GateFor:         func(envelope.ActionIntent, envelope.PolicyDecision) OwnerWritebackGate { return baseGate() },
		BlockedSummary:  func(status, gateID string) string { return "blocked: " + status },
		AllowedSummary:  func(status, gateID string) string { return "routed via " + gateID },
	}
}

func TestRunGatedWriteback_Allowed(t *testing.T) {
	res, err := RunGatedWriteback(runnerInput(t.TempDir(), "allow"))
	if err != nil {
		t.Fatal(err)
	}
	if res.GateStatus != GateAllowed {
		t.Fatalf("gate status = %q, want %q", res.GateStatus, GateAllowed)
	}
	if res.WorkID == "" || res.ActionIntentID == "" || res.PolicyDecisionID == "" || res.GateID == "" {
		t.Fatalf("missing spine refs: %+v", res)
	}
	if res.ReceiptID == "" || res.TransitionID == "" {
		t.Fatalf("allowed run must record receipt + transition: %+v", res)
	}
}

func TestRunGatedWriteback_Blocked(t *testing.T) {
	res, err := RunGatedWriteback(runnerInput(t.TempDir(), "hold"))
	if err != nil {
		t.Fatal(err)
	}
	if res.GateStatus == GateAllowed {
		t.Fatalf("held policy must not produce an allowed gate, got %q", res.GateStatus)
	}
	// A blocked run still closes the work (audit_only) and records the gate +
	// receipt/transition — denied writeback is evidence too.
	if res.WorkID == "" || res.GateID == "" || res.ReceiptID == "" || res.TransitionID == "" {
		t.Fatalf("blocked run must still close + record: %+v", res)
	}
}
