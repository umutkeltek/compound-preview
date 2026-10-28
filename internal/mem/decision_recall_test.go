package mem

import (
	"fmt"
	"strings"
	"testing"
)

func TestDistinctDecisionsDoNotCollapse(t *testing.T) {
	s := newStore(t)
	var claims []Claim
	for i := 1; i <= 34; i++ {
		id := fmt.Sprintf("DEC-%04d", i)
		claims = append(claims, Claim{
			ID:         id,
			Seq:        int64(i),
			Entity:     "decisions",
			Predicate:  "decided",
			Object:     "decision object " + id,
			ClaimType:  "decision",
			Qualifiers: map[string]string{"domains": "memory"},
			Confidence: 0.9,
			Visibility: "ambient",
			CreatedAt:  rfc3339(t0),
		})
	}

	proposals, reopened, err := s.synthesisContradictions(claims, nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 0 || len(reopened) != 0 {
		t.Fatalf("distinct decision claims collapsed into contradictions: proposals=%+v reopened=%+v", proposals, reopened)
	}
	if claims[0].Topic() != "memory" {
		t.Fatalf("decision claim topic = %q, want plural domains qualifier", claims[0].Topic())
	}
}

func TestGenuineContradictionStillGroupsAndMarks(t *testing.T) {
	s := newStore(t)
	claims := []Claim{
		reviewTestClaim("claim_runtime_go", 1, "project:api", "runtime", "Go", "architecture"),
		reviewTestClaim("claim_runtime_rust", 2, "project:api", "runtime", "Rust", "architecture"),
	}
	if err := appendJSONL(s.claimsLog(), &claims[0]); err != nil {
		t.Fatal(err)
	}
	if err := appendJSONL(s.claimsLog(), &claims[1]); err != nil {
		t.Fatal(err)
	}
	if err := appendJSONL(s.edgesLog(), &Edge{
		Seq:             3,
		Kind:            EdgeKindNeedsReview,
		From:            "synth_contradiction_seed",
		To:              claims[0].ID,
		Confidence:      0.4,
		ConfidenceDelta: -0.4,
		EvidenceRefs:    []string{"synth_contradiction_seed"},
		CreatedAt:       rfc3339(t0),
	}); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.loadClaims()
	if err != nil {
		t.Fatal(err)
	}
	proposals, reopened, err := s.synthesisContradictions(loaded, nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 1 {
		t.Fatalf("proposals = %+v, want one genuine contradiction", proposals)
	}
	if len(reopened) != 1 || reopened[0].ClaimID != claims[1].ID {
		t.Fatalf("reopened = %+v, want only the unmarked claim reopened", reopened)
	}
	marks, err := s.claimReviewMarksAsOf(0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := marks[claims[0].ID]; !ok {
		t.Fatalf("pre-marked contradiction claim was cleared unexpectedly: %+v", marks)
	}
	if _, ok := marks[claims[1].ID]; !ok {
		t.Fatalf("new contradiction claim was not marked: %+v", marks)
	}
}

func TestSweepSelfHealsStaleQuarantine(t *testing.T) {
	s := newStore(t)
	stale := reviewTestClaim("claim_stale", 1, "project:api", "language", "Go", "architecture")
	realMarked := reviewTestClaim("claim_real_go", 2, "project:runtime", "language", "Go", "architecture")
	realPeer := reviewTestClaim("claim_real_rust", 3, "project:runtime", "language", "Rust", "architecture")
	for _, claim := range []Claim{stale, realMarked, realPeer} {
		claim := claim
		if err := appendJSONL(s.claimsLog(), &claim); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range []Edge{
		{
			Seq:             4,
			Kind:            EdgeKindNeedsReview,
			From:            "synth_contradiction_stale",
			To:              stale.ID,
			Confidence:      0.6,
			ConfidenceDelta: -0.6,
			EvidenceRefs:    []string{"synth_contradiction_stale"},
			CreatedAt:       rfc3339(t0),
		},
		{
			Seq:             5,
			Kind:            EdgeKindNeedsReview,
			From:            "synth_contradiction_real",
			To:              realMarked.ID,
			Confidence:      0.5,
			ConfidenceDelta: -0.5,
			EvidenceRefs:    []string{"synth_contradiction_real"},
			CreatedAt:       rfc3339(t0),
		},
	} {
		edge := edge
		if err := appendJSONL(s.edgesLog(), &edge); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.SynthesisSweep([]string{"contradict"}, t0); err != nil {
		t.Fatal(err)
	}
	marks, err := s.claimReviewMarksAsOf(0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := marks[stale.ID]; ok {
		t.Fatalf("stale synthesis quarantine still active: %+v", marks)
	}
	if _, ok := marks[realMarked.ID]; !ok {
		t.Fatalf("real contradiction mark was cleared: %+v", marks)
	}
	if _, ok := marks[realPeer.ID]; !ok {
		t.Fatalf("real contradiction peer was not marked: %+v", marks)
	}
}

func TestClaimIndexTextIncludesMeaningfulID(t *testing.T) {
	meaningful := Claim{ID: "DEC-0021", Entity: "decisions", Predicate: "decided", Object: "Commands are copied"}
	if got := ClaimIndexText(meaningful); !strings.Contains(got, "DEC-0021") {
		t.Fatalf("ClaimIndexText(%+v) = %q, want meaningful id", meaningful, got)
	}

	hashLike := Claim{ID: "claim_abc123", Entity: "decisions", Predicate: "decided", Object: "Commands are copied"}
	if got := ClaimIndexText(hashLike); strings.Contains(got, "claim_abc123") {
		t.Fatalf("ClaimIndexText(%+v) = %q, should skip hash-like id", hashLike, got)
	}
}

func reviewTestClaim(id string, seq int64, entity, predicate, object, claimType string) Claim {
	return Claim{
		ID:         id,
		Seq:        seq,
		Entity:     entity,
		Predicate:  predicate,
		Object:     object,
		ClaimType:  claimType,
		Qualifiers: map[string]string{"domains": "stack"},
		Confidence: 0.9,
		Visibility: "ambient",
		CreatedAt:  rfc3339(t0),
	}
}
