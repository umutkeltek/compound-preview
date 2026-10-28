package mem

import (
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
)

// volatileBase is a well-formed low-risk volatile candidate; tests mutate one
// field at a time to isolate each refusal.
//
// ExpiresAt is a horizon relative to the wall clock, not an absolute literal,
// so the value cannot expire out from under the test the way 2026-07-28T12:00:00Z
// did. The literal was carried in by an earlier revision; one calendar day
// after that instant the candidate stopped being accepted as "live" and the
// certifying battery went red for every caller. The repo-wide clock-hygiene
// guard (internal/hygiene/clock_hygiene_test.go) refuses to ship that pattern
// again.
func volatileBase() Candidate {
	return Candidate{
		Entity: "project:compound", Predicate: "build_status", Object: "green as of t0",
		Confidence: 0.9, Derivation: "deterministic", ClaimType: "status",
		SourceTraceRef: "trace:vol1", SnippetHash: "hvol1",
		Visibility: "volatile", ExpiresAt: time.Now().UTC().Add(365 * 24 * time.Hour).Format(time.RFC3339),
		PromotionBasis: candidates.BasisRuntimeProof,
	}
}

// Falsifier (a): a volatile write with no clock is a contradiction — REFUSED at
// every write door, and a planted clockless-volatile fixture must not persist.
func TestVolatileWithoutClockRefusedAtCapture(t *testing.T) {
	s := newStore(t)
	clockless := volatileBase()
	clockless.ExpiresAt = ""
	_, err := s.Capture([]Candidate{clockless}, t0)
	if err == nil || !strings.Contains(err.Error(), "volatile") || !strings.Contains(err.Error(), "expires_at") {
		t.Fatalf("clockless volatile candidate must refuse at Capture naming volatile+expires_at, got %v", err)
	}
	all, err := s.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("clockless volatile fixture persisted: %d candidates", len(all))
	}
}

func TestVolatileWithoutClockRefusedAtStableCandidateDoor(t *testing.T) {
	s := newStore(t)
	c := volatileBase()
	c.ID = "cand_volatile_clockless"
	c.ExpiresAt = ""
	if _, err := s.RecordStableCandidate(c); err == nil || !strings.Contains(err.Error(), "volatile") {
		t.Fatalf("clockless volatile must refuse at stable-candidate door, got %v", err)
	}
}

// Falsifier (a), promote door: Adjudicate is the single claim-append door;
// a clockless-volatile candidate that predates the guard (hand-written file)
// must still be refused at promotion — no clockless volatile claim can enter
// the ledger.
func TestVolatileWithoutClockRefusedAtPromotion(t *testing.T) {
	s := newStore(t)
	good := volatileBase()
	got, err := s.Capture([]Candidate{good}, t0)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-guard candidate: blank the clock AFTER capture, on disk.
	c := got[0]
	c.ExpiresAt = ""
	if err := writeCandidateFile(s, c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Adjudicate(c.ID, "promote", "work_test_vol", t0); err == nil || !strings.Contains(err.Error(), "volatile") {
		t.Fatalf("clockless volatile must refuse at promotion, got %v", err)
	}
	claims, err := s.loadClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Fatalf("clockless volatile claim entered the ledger: %d claims", len(claims))
	}
}

// A well-formed volatile candidate promotes, and the durable claim carries the
// clock (Claim.ExpiresAt is the additive field this packet introduces).
func TestVolatilePromotionCarriesExpiresAt(t *testing.T) {
	s := newStore(t)
	// Capture the expected ExpiresAt before handing the candidate to Capture so
	// the assertion compares against whatever horizon volatileBase chose, not
	// against a re-stated literal that would re-arm the same trap.
	cand := volatileBase()
	want := cand.ExpiresAt
	got, err := s.Capture([]Candidate{cand}, t0)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Adjudicate(got[0].ID, "promote", "work_test_vol", t0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Claim == nil {
		t.Fatal("promotion returned no claim")
	}
	if res.Claim.Visibility != "volatile" {
		t.Fatalf("claim visibility = %q, want volatile", res.Claim.Visibility)
	}
	if res.Claim.ExpiresAt != want {
		t.Fatalf("claim expires_at = %q, want the candidate clock %q", res.Claim.ExpiresAt, want)
	}
	// Volatile IS promoted: recallable under matching scope (only ambient is closed to it).
	rec, err := s.Recall("project:compound", RecallOpts{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, cl := range rec.Claims {
		if cl.ID == res.Claim.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("volatile claim not recallable under matching scope: %+v", rec.Claims)
	}
}

// Non-volatile promotion must NOT inherit a candidate's park-decay TTL: a
// decay-parked candidate's ExpiresAt is candidate-lifecycle state, not claim
// state. Only volatile carries the clock onto the ledger.
func TestNonVolatilePromotionDropsCandidateParkClock(t *testing.T) {
	s := newStore(t)
	c := volatileBase()
	c.Visibility = "operational"
	c.ExpiresAt = t0.Add(30 * 24 * time.Hour).Format(time.RFC3339) // a park-decay TTL, not a claim clock
	got, err := s.Capture([]Candidate{c}, t0)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Adjudicate(got[0].ID, "promote", "work_test_vol", t0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Claim == nil || res.Claim.ExpiresAt != "" {
		t.Fatalf("non-volatile claim must not carry candidate park clock, got %+v", res.Claim)
	}
}

// Falsifier (f), write half: a doctrine-class (operator-permanent) claim marked
// volatile is a contradiction — decisions/principles/behavior never auto-cool,
// so they may never carry the auto-cooling visibility at all. Refused at write.
func TestVolatileDoctrineClassRefusedAtWrite(t *testing.T) {
	s := newStore(t)
	cases := []struct{ claimType, canonClass string }{
		{"decision", ""},        // derives to decision
		{"preference", ""},      // derives to principle
		{"", "behavior"},        // explicit operator-permanent class
		{"status", "principle"}, // explicit wins over low-risk claim type
	}
	for _, tc := range cases {
		c := volatileBase()
		c.ClaimType = tc.claimType
		c.CanonClass = tc.canonClass
		_, err := s.Capture([]Candidate{c}, t0)
		if err == nil || !strings.Contains(err.Error(), "volatile") {
			t.Fatalf("volatile %s/%s must refuse at write, got %v", tc.claimType, tc.canonClass, err)
		}
	}
}

// volatile is now a recognized visibility everywhere visibilities are validated.
func TestVolatileIsARecognizedVisibility(t *testing.T) {
	if !oneOf("volatile", Visibilities) {
		t.Fatalf("volatile missing from Visibilities %v (transition.md ladder row)", Visibilities)
	}
}

// writeCandidateFile writes a candidate file directly, bypassing Capture's
// guards — the "hand-edited / pre-guard ledger" simulation used by the
// promotion-door test.
func writeCandidateFile(s *Store, c Candidate) error {
	return candidates.WriteCandidate(s.home, candidates.Memory, c.ID, &c)
}
