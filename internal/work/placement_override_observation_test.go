package work

import (
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
)

// mkPlacementTestAction builds a local_reversible ActionIntent (baseline
// policy outcome allow_with_obligations) so a PLACEMENT_OVERRIDE fail-open
// preserves an allowing decision RecordAction will accept.
func mkPlacementTestAction(workID, actionID string) envelope.ActionIntent {
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent, ID: actionID, CreatedAt: fixedNow.Format(time.RFC3339),
			Producer: envelope.Producer{Kind: "hook", Name: "compound"}, IDs: envelope.IDs{WorkID: workID},
			Authority: envelope.Authority{SourceClass: "runtime"}, Privacy: "private", Risk: "tier1",
			EvidenceRefs: []string{}, Uncertainty: envelope.Uncertainty{Level: "known"},
		},
		Capability: "capability://skill:filesystem-write", Action: "write_file",
		Target:        envelope.ActionTarget{Kind: "file", Path: "docs/x.md"},
		MutationClass: "local_reversible", Reversibility: "easy",
	}
}

func countPlacementOverrideObservations(events []Event) int {
	n := 0
	for _, e := range events {
		if strings.HasPrefix(e.Note, "placement_override observed:") {
			n++
		}
	}
	return n
}

// TestRecordActionPersistsPlacementOverrideObservationOnlyWhenOverrideFires
// is the S12 proof: PLACEMENT_OVERRIDE stays a fail-open (policy.go already
// appends a visible reason to the PolicyDecision), but RecordAction — the
// consumer that already durably persists every PolicyDecision — must now
// also persist ONE additional observation Event whenever that override
// actually fired, and must persist NOTHING extra when it did not.
func TestRecordActionPersistsPlacementOverrideObservationOnlyWhenOverrideFires(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "o", SuccessPredicate: "p"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "claude_code", fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "claude_code"}, fixedNow); err != nil {
		t.Fatal(err)
	}

	cfg := policy.DefaultConfig()

	// Baseline: ordinary allowed action, no PlacementGuardContext at all.
	// RecordAction must persist the action normally and add NO observation event.
	plainAI := mkPlacementTestAction(w.ID, "act_plain")
	plainPD := policy.EvaluateWithContext(plainAI, cfg, policy.EvaluationContext{}, fixedNow)
	if _, err := s.RecordAction(w.ID, plainAI, plainPD, fixedNow); err != nil {
		t.Fatalf("record plain action: %v", err)
	}
	afterPlain, err := s.eventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := countPlacementOverrideObservations(afterPlain); got != 0 {
		t.Fatalf("no override fired; expected zero observation events, got %d events=%+v", got, afterPlain)
	}

	// PLACEMENT_OVERRIDE fires: policy fail-opens the placement guard and stamps
	// the reason onto the PolicyDecision. RecordAction must persist exactly one
	// additional observation event carrying that reason.
	t.Setenv("PLACEMENT_OVERRIDE", "1")
	overrideAI := mkPlacementTestAction(w.ID, "act_override")
	overridePD := policy.EvaluateWithContext(overrideAI, cfg, policy.EvaluationContext{Placement: &policy.PlacementGuardContext{
		Mode: "block", Confidence: "block", OwnerRef: "owner:decisions", TargetRef: "docs/x.md",
	}}, fixedNow)
	if overridePD.Outcome != "allow_with_obligations" {
		t.Fatalf("override should fail open to the baseline outcome, got %q reasons=%v", overridePD.Outcome, overridePD.Reasons)
	}
	if _, err := s.RecordAction(w.ID, overrideAI, overridePD, fixedNow); err != nil {
		t.Fatalf("record override action: %v", err)
	}
	afterOverride, err := s.eventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := countPlacementOverrideObservations(afterOverride); got != 1 {
		t.Fatalf("expected exactly one override observation event, got %d events=%+v", got, afterOverride)
	}
	last := afterOverride[len(afterOverride)-1]
	if !strings.Contains(last.Note, "placement_override observed:") ||
		!strings.Contains(last.Note, "PLACEMENT_OVERRIDE active, fail-open") ||
		!contains(last.Attached, overridePD.ID) {
		t.Fatalf("override observation event malformed: %+v", last)
	}
}
