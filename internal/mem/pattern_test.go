package mem

import (
	"testing"

	"compound/internal/candidates"
)

// A topic revisited across sessions is a recurrence Pattern (hypothesis, not canon); a single
// claim is not. The spine-native convergence of Gen-1 pattern.Build.
func TestPatternsRecurrence(t *testing.T) {
	s := newStore(t)
	promote := func(pred, obj, trace string) {
		c, err := s.Capture([]Candidate{{Entity: "svc", Predicate: pred, Object: obj, Confidence: 0.9, PromotionBasis: candidates.BasisExplicitConfirmation, SourceTraceRef: trace}}, t0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := promoteForTest(t, s, c[0].ID, "", t0); err != nil {
			t.Fatal(err)
		}
	}
	promote("lang", "node", "tr_a") // session a touches lang
	promote("lang", "go", "tr_b")   // session b revisits lang (supersession) → recurrence
	promote("db", "postgres", "tr_a")

	pats, err := s.Patterns()
	if err != nil {
		t.Fatal(err)
	}
	if len(pats) != 1 {
		t.Fatalf("expected 1 pattern (lang recurred, db did not); got %d: %+v", len(pats), pats)
	}
	p := pats[0]
	if p.ID != "pat::svc::lang" || len(p.SupportClaims) != 2 || p.Sessions != 2 || p.Status != "hypothesis" {
		t.Errorf("want svc::lang, 2 support claims, 2 sessions, hypothesis; got %+v", p)
	}
}
