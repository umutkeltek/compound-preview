package distiller

import (
	"testing"

	"compound/internal/mem"
)

func TestCritiqueUsesTopicAxisForMemoryDuplicates(t *testing.T) {
	prior := PriorState{Current: []mem.Claim{{
		ID:         "claim_lang",
		Entity:     "svc",
		Predicate:  "runtime",
		Object:     "Go",
		Confidence: 0.9,
		ClaimType:  "decision",
		Qualifiers: map[string]string{"domain": "language"},
	}}}
	got := critique(Candidate{
		Kind:       KindMemory,
		Entity:     "svc",
		Predicate:  "implementation_language",
		Object:     " go ",
		Confidence: 0.9,
		ClaimType:  "decision",
		Qualifiers: map[string]string{"domain": "language"},
	}, prior)
	if !got.Duplicate || got.DuplicateClaimID != "claim_lang" {
		t.Fatalf("critique = %+v, want duplicate claim_lang", got)
	}
}

func TestCritiqueDoesNotConflictAcrossDistinctPredicateSlots(t *testing.T) {
	prior := PriorState{Current: []mem.Claim{{
		ID:         "claim_runtime",
		Entity:     "svc",
		Predicate:  "runtime",
		Object:     "Go",
		Confidence: 0.9,
		ClaimType:  "decision",
		Qualifiers: map[string]string{"domain": "language"},
	}}}
	got := critique(Candidate{
		Kind:       KindMemory,
		Entity:     "svc",
		Predicate:  "implementation_language",
		Object:     "Rust",
		Confidence: 0.9,
		ClaimType:  "decision",
		Qualifiers: map[string]string{"domain": "language"},
	}, prior)
	if !got.IsNew || got.Duplicate || got.Supersedes != "" || got.Contradicts != "" {
		t.Fatalf("critique = %+v, want independent new predicate slot", got)
	}
}

func TestCritiqueExactSlotWinsOverBroadDuplicate(t *testing.T) {
	prior := PriorState{Current: []mem.Claim{
		{
			ID:         "claim_implementation_go",
			Entity:     "svc",
			Predicate:  "implementation_language",
			Object:     "Go",
			Confidence: 0.9,
			ClaimType:  "decision",
			Qualifiers: map[string]string{"domain": "language"},
		},
		{
			ID:         "claim_runtime_node",
			Entity:     "svc",
			Predicate:  "runtime",
			Object:     "Node",
			Confidence: 0.9,
			ClaimType:  "decision",
			Qualifiers: map[string]string{"domain": "language"},
		},
	}}
	got := critique(Candidate{
		Kind:       KindMemory,
		Entity:     "svc",
		Predicate:  "runtime",
		Object:     "Go",
		Confidence: 0.9,
		ClaimType:  "decision",
		Qualifiers: map[string]string{"domain": "language"},
	}, prior)
	if got.Supersedes != "claim_runtime_node" || got.Duplicate || got.DuplicateClaimID != "" {
		t.Fatalf("critique = %+v, want exact runtime supersession", got)
	}
}
