package mem

import (
	"testing"
	"time"

	"compound/internal/candidates"
)

var runtimeProofTestTime = time.Date(2026, 7, 9, 15, 0, 0, 0, time.UTC)

func seedParkedBeliefCandidate(t *testing.T, s *Store, id string) Candidate {
	t.Helper()
	cand := Candidate{
		ID:              id,
		Entity:          "agent-lane:fixture--route--model--unknown",
		Predicate:       BeliefPredicateTendsTo,
		Object:          "observed",
		Derivation:      "deterministic",
		ProvenanceLane:  "lanebeliefs-fold",
		ClaimType:       "status",
		CanonClass:      "status",
		Qualifiers:      map[string]string{"domain": "scope-creep"},
		Visibility:      "contextual",
		State:           "parked",
		ObservationDate: runtimeProofTestTime.Format(time.RFC3339),
		CreatedAt:       runtimeProofTestTime.Format(time.RFC3339),
	}
	if _, err := s.RecordStableCandidate(cand); err != nil {
		t.Fatalf("seed candidate: %v", err)
	}
	return cand
}

// A belief candidate carries an EMPTY promotion_basis (the B1 valve forbids a pre-stamped basis),
// so a bare promote refuses — there is no valid basis to promote on.
func TestRuntimeProofPromoteRefusedWithoutMark(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	c := seedParkedBeliefCandidate(t, s, "cand_lanebelief_rp_refuse")
	if _, err := s.Adjudicate(c.ID, "promote", "work_rp", runtimeProofTestTime); err == nil {
		t.Fatal("promote without any basis must refuse; got nil error")
	}
}

// RecordRuntimeProofCorroboration attaches the mark corroborationPromotionBasis reads, so the
// SAME governed Adjudicate("promote") now derives promotion_basis=runtime_proof and mints a
// contextual-visibility claim.
func TestRuntimeProofCorroborationDerivesRuntimeProofBasis(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	c := seedParkedBeliefCandidate(t, s, "cand_lanebelief_rp_ok")

	added, err := s.RecordRuntimeProofCorroboration(c.ID, []string{"rec_crosslane_proof"}, runtimeProofTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Fatalf("marks added = %d, want 1", added)
	}

	res, err := s.Adjudicate(c.ID, "promote", "work_rp", runtimeProofTestTime)
	if err != nil {
		t.Fatalf("promote after runtime-proof mark: %v", err)
	}
	if res.Claim == nil {
		t.Fatal("promote produced no claim")
	}
	if res.Claim.PromotionBasis != candidates.BasisRuntimeProof {
		t.Fatalf("claim promotion_basis = %q, want %q", res.Claim.PromotionBasis, candidates.BasisRuntimeProof)
	}
	if res.Claim.Visibility != "contextual" {
		t.Fatalf("claim visibility = %q, want contextual", res.Claim.Visibility)
	}
}

// The door is idempotent on the ref set: recording the same proof ref twice appends one mark, and
// an empty ref set is refused (a runtime proof must cite at least one run/state-delta ref).
func TestRuntimeProofCorroborationIdempotentAndRequiresRef(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	c := seedParkedBeliefCandidate(t, s, "cand_lanebelief_rp_idem")

	if _, err := s.RecordRuntimeProofCorroboration(c.ID, nil, runtimeProofTestTime); err == nil {
		t.Fatal("empty proof-ref set must refuse")
	}
	if added, err := s.RecordRuntimeProofCorroboration(c.ID, []string{"rec_p"}, runtimeProofTestTime); err != nil || added != 1 {
		t.Fatalf("first mark = (%d,%v), want (1,nil)", added, err)
	}
	if added, err := s.RecordRuntimeProofCorroboration(c.ID, []string{"rec_p"}, runtimeProofTestTime); err != nil || added != 0 {
		t.Fatalf("repeat mark = (%d,%v), want (0,nil) — idempotent", added, err)
	}
}
