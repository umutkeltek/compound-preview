package policy

import (
	"testing"
	"time"

	"compound/internal/envelope"
)

// TestFreezeMatchIsLiteralNeverInferred proves freeze scope matching is
// literal owner-glob / keyword-substring matching (settled design decision
// 2, grants-as-gates 2026-07-10) — no classifier, no fuzzy inference.
func TestFreezeMatchIsLiteralNeverInferred(t *testing.T) {
	freezes := []FreezeRecord{
		{
			ID:         "direction-freeze-2026-07-10",
			OwnerGlobs: []string{"compound-authority"},
			Keywords:   []string{"absorption_debt", "harness_edit"},
		},
	}

	// A new-organ/absorption-shaped envelope matches the keyword.
	matched, rec := FreezeMatch("compound-director", "director:absorption-debt:os_source_test", []string{"director_trigger:absorption_debt:foo"}, freezes)
	if !matched || rec.ID != "direction-freeze-2026-07-10" {
		t.Fatalf("absorption_debt evidence should match the freeze: matched=%t rec=%+v", matched, rec)
	}

	// A smoothness/wiring envelope (no keyword, no owner glob hit) passes.
	passMatched, _ := FreezeMatch("compound-tests", "director:review-debt:synthetic-review-debt:review-debt-01", []string{"director_trigger:review_debt:x"}, freezes)
	if passMatched {
		t.Fatalf("a non-matching (smoothness/wiring) envelope must pass the freeze untouched")
	}

	// Owner glob match.
	ownerMatched, ownerRec := FreezeMatch("compound-authority", "director:job-health:x", nil, freezes)
	if !ownerMatched || ownerRec.ID != "direction-freeze-2026-07-10" {
		t.Fatalf("owner glob should match: matched=%t rec=%+v", ownerMatched, ownerRec)
	}

	// Empty freeze list never matches anything.
	if matched, _ := FreezeMatch("compound-authority", "director:absorption-debt:x", []string{"director_trigger:absorption_debt:x"}, nil); matched {
		t.Fatalf("no active freeze records must never match")
	}
}

// TestFreezeDenyDecisionIsRecordableDeny mirrors
// TestEditableSurfaceDenyDecisionIsRecordableDeny / TestAutonomyHaltDecisionIsRecordableDeny:
// the freeze denial leaves an auditable, validating PolicyDecision, not just
// an error string.
func TestFreezeDenyDecisionIsRecordableDeny(t *testing.T) {
	ai := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            "act_freeze_test",
			CreatedAt:     time.Now().UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			Authority:     envelope.Authority{SourceClass: "runtime"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "auto-admit:director",
		Capability:    "work.advance",
		Action:        "advance_work_to_admitted",
		Target:        envelope.ActionTarget{Kind: "work", ID: "work_x", Path: "work/work/work_x.json"},
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       "advance work_x proposed -> admitted",
		RollbackPlan:  "leave proposed",
	}
	pd := FreezeDenyDecision(ai, "freeze active (direction-freeze-2026-07-10): envelope matches an active freeze scope", time.Now())
	if pd.Outcome != "deny" {
		t.Fatalf("freeze deny decision must be deny, got %q", pd.Outcome)
	}
	if err := pd.Validate(); err != nil {
		t.Fatalf("freeze deny decision must validate: %v", err)
	}
	found := false
	for _, ref := range pd.EvidenceRefs {
		if ref == FreezeAuthorityRef {
			found = true
		}
	}
	if !found {
		t.Fatalf("freeze deny decision must cite the freeze authority ref, got %v", pd.EvidenceRefs)
	}
}
