package envelope

import (
	"encoding/json"
	"strings"
	"testing"
)

// validCauseTransition is a minimal Transition that validates with Cause == nil,
// so each falsifier can attach a Cause and observe only the Cause rule.
func validCauseTransition() Transition {
	return Transition{
		Header: Header{
			SchemaVersion: SVTransition,
			ID:            "tr_cause_test",
			CreatedAt:     "2026-07-15T00:00:00Z",
			Producer:      Producer{Kind: "cli", Name: "compound"},
			IDs:           IDs{WorkID: "work_cause_test"},
			Authority:     Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			Uncertainty:   Uncertainty{Level: "known"},
		},
		BeforeRefs:  []string{},
		ActionRefs:  []string{},
		AfterRefs:   []string{},
		Projections: Projections{},
		SecondOrder: SecondOrder{},
	}
}

// Falsifier 1: a bogus delta_cause is rejected; all four enum values pass.
func TestTransitionCauseRejectsBadDeltaCause(t *testing.T) {
	tr := validCauseTransition()
	tr.Cause = &TransitionCause{DeltaCause: "frobnicate", CausedByRefs: []string{"work_x"}}
	if err := tr.Validate(); err == nil {
		t.Fatalf("expected validation error for bogus delta_cause, got nil")
	}
	for _, dc := range DeltaCauses {
		tr.Cause = &TransitionCause{DeltaCause: dc, CausedByRefs: []string{"work_x"}}
		if err := tr.Validate(); err != nil {
			t.Fatalf("delta_cause %q should validate: %v", dc, err)
		}
	}
}

// Falsifier 2: a present Cause with no non-empty caused_by ref is rejected; one ref passes.
func TestTransitionCauseRequiresCausedByRef(t *testing.T) {
	tr := validCauseTransition()

	tr.Cause = &TransitionCause{DeltaCause: "repair", CausedByRefs: nil}
	if err := tr.Validate(); err == nil {
		t.Fatalf("expected error for empty caused_by_refs, got nil")
	}

	tr.Cause = &TransitionCause{DeltaCause: "repair", CausedByRefs: []string{"   "}}
	if err := tr.Validate(); err == nil {
		t.Fatalf("expected error for whitespace-only caused_by_refs, got nil")
	}

	tr.Cause = &TransitionCause{DeltaCause: "repair", CausedByRefs: []string{"work_abc"}}
	if err := tr.Validate(); err != nil {
		t.Fatalf("one non-empty caused_by ref should validate: %v", err)
	}
}

// Falsifier 3: a nil Cause validates and never emits a "cause" JSON key (back-compat).
func TestTransitionCauseOmittedIsBackcompat(t *testing.T) {
	tr := validCauseTransition() // Cause left nil
	if err := tr.Validate(); err != nil {
		t.Fatalf("nil Cause should validate: %v", err)
	}
	if err := RoundTrip(&tr); err != nil {
		t.Fatalf("round-trip of nil-Cause transition failed: %v", err)
	}
	b, err := json.Marshal(&tr)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "\"cause\"") {
		t.Fatalf("nil Cause must not emit a cause key: %s", b)
	}
}

// Falsifier F1: a prose next_consumer is rejected by Validate.
func TestTransitionCauseRejectsProseNextConsumer(t *testing.T) {
	tr := validCauseTransition()
	tr.Cause = &TransitionCause{
		DeltaCause:   "accept",
		CausedByRefs: []string{"work_abc"},
		NextConsumer: "someone should probably check the new thing tomorrow",
	}
	if err := tr.Validate(); err == nil {
		t.Fatalf("expected validation error for prose next_consumer, got nil")
	}
}

// Falsifier F2: a slug-shaped next_consumer passes, an empty next_consumer passes
// (audit_only), and a nil Cause passes (backward compat).
func TestTransitionCauseAcceptsSlugEmptyAndNilNextConsumer(t *testing.T) {
	tr := validCauseTransition()

	tr.Cause = &TransitionCause{DeltaCause: "accept", CausedByRefs: []string{"work_abc"}, NextConsumer: "capgraph"}
	if err := tr.Validate(); err != nil {
		t.Fatalf("slug next_consumer %q should validate: %v", tr.Cause.NextConsumer, err)
	}

	tr.Cause = &TransitionCause{DeltaCause: "accept", CausedByRefs: []string{"work_abc"}, NextConsumer: ""}
	if err := tr.Validate(); err != nil {
		t.Fatalf("empty next_consumer (audit_only) should validate: %v", err)
	}

	tr.Cause = nil
	if err := tr.Validate(); err != nil {
		t.Fatalf("nil Cause should validate: %v", err)
	}
}

// Falsifier F3: ValidateBytes over a serialized Transition carrying a prose
// next_consumer rejects it — proves the write/read gate, not just the struct method.
func TestTransitionCauseValidateBytesRejectsProseNextConsumer(t *testing.T) {
	tr := validCauseTransition()
	tr.Cause = &TransitionCause{
		DeltaCause:   "accept",
		CausedByRefs: []string{"work_abc"},
		NextConsumer: "someone should probably check the new thing tomorrow",
	}
	b, err := json.Marshal(&tr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateBytes(b); err == nil {
		t.Fatalf("expected ValidateBytes to reject prose next_consumer, got nil error")
	}
}
