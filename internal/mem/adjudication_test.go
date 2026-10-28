package mem

import (
	"testing"
	"time"
)

// Depth probe: the promotion decision is pure and table-testable without a
// COMPOUND_HOME. It fails if the decision (verdict→state, duplicate detection,
// claim/edge construction) regresses back into Adjudicate's I/O path.
func TestPromotionDecisionIsPure(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)

	for v, want := range map[string]string{"reject": "rejected", "park": "parked", "no_promote": "no_promote"} {
		if got := nonPromoteState(v); got != want {
			t.Errorf("nonPromoteState(%q) = %q, want %q", v, got, want)
		}
	}

	prior := Claim{ID: "claim_prior", Entity: "e", Predicate: "p", Object: "v1", Qualifiers: map[string]string{"status": "accepted"}}
	newer := Candidate{ID: "cand1", Entity: "e", Predicate: "p", Object: "v2", Confidence: 0.9, SourceTraceRef: "trace1"}

	if isDuplicatePromotion(newer, prior, true) {
		t.Error("different object must not be a duplicate")
	}
	if !isDuplicatePromotion(Candidate{Object: "v1", Qualifiers: map[string]string{"status": "accepted"}}, prior, true) {
		t.Error("same object and qualifiers must be a duplicate")
	}
	if isDuplicatePromotion(Candidate{Object: "v1", Qualifiers: map[string]string{"status": "deprecated"}}, prior, true) {
		t.Error("same object with changed qualifiers must not be a duplicate")
	}
	if isDuplicatePromotion(newer, Claim{}, false) {
		t.Error("no prior must not be a duplicate")
	}

	claim := promotedClaim(newer, prior, true, 5, now)
	if claim.Seq != 5 || claim.Object != "v2" || claim.Entity != "e" || claim.Supersedes != "claim_prior" {
		t.Fatalf("promotedClaim = %+v", claim)
	}
	if len(claim.EvidenceRefs) != 2 || claim.EvidenceRefs[0] != "cand1" || claim.EvidenceRefs[1] != "trace1" {
		t.Fatalf("evidence refs = %v", claim.EvidenceRefs)
	}
	first := promotedClaim(newer, Claim{}, false, 1, now)
	if first.Supersedes != "" {
		t.Errorf("first claim supersedes nothing, got %q", first.Supersedes)
	}

	e := supersedesEdge(claim, prior, newer, 5, now)
	if e.Kind != "supersedes" || e.From != claim.ID || e.To != "claim_prior" || e.Seq != 5 {
		t.Fatalf("supersedesEdge = %+v", e)
	}
}
