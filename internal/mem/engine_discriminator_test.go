package mem

import (
	"errors"
	"testing"
)

// cannedHitEngine is a test-local breadth engine that returns a FIXED hit list, ignoring the
// allowlist entirely, so QueryBreadth's authority router is exercised directly on hits the ledger
// does not own. CorpusGen 0 + a no-op Index lets QueryBreadth proceed without a real reindex.
type cannedHitEngine struct {
	name string
	hits []Hit
}

func (e *cannedHitEngine) Capabilities() EngineCapabilities {
	return EngineCapabilities{Name: e.name, Breadth: true, CorpusGen: 0}
}
func (e *cannedHitEngine) Index([]Claim, int64) error { return nil }
func (e *cannedHitEngine) Query(_, _ string, _ []string, _ int) ([]Hit, error) {
	return append([]Hit(nil), e.hits...), nil
}

// TestQueryBreadthRejectsUnownedIndexHit falsifies the old silent-drop: an index_hit (no Kind)
// into a claim the ledger does not hold is the engine serving authority it lacks, and must fail
// the WHOLE query fail-closed — never vanish.
func TestQueryBreadthRejectsUnownedIndexHit(t *testing.T) {
	s := storeWith(t, "product:focus", "positioning", "owned capture")
	eng := &cannedHitEngine{name: "rented-index", hits: []Hit{{ClaimID: "claim_not_in_ledger", Score: 9}}}
	if _, err := s.QueryBreadth("owned", 10, eng); !errors.Is(err, ErrEngineServedUnownedClaim) {
		t.Fatalf("an index_hit into an unowned claim must fail closed with ErrEngineServedUnownedClaim, got err=%v", err)
	}
}

// TestEngineDoctorFlagsUnownedIndexHitLedgerUntouched proves the doctor treats the breach as a
// hard served-authority failure (engine_violations populated) WITHOUT calling it a ledger
// mutation (ledger_unchanged stays true).
func TestEngineDoctorFlagsUnownedIndexHitLedgerUntouched(t *testing.T) {
	s := storeWith(t, "product:focus", "positioning", "owned capture")
	eng := &cannedHitEngine{name: "rented-index", hits: []Hit{{ClaimID: "claim_not_in_ledger", Score: 9}}}
	report, err := s.EngineDoctor("owned", 10, eng)
	if err == nil || !errors.Is(err, ErrEngineServedUnownedClaim) {
		t.Fatalf("engine doctor must surface the served-authority breach, got %v", err)
	}
	if len(report.EngineViolations) == 0 {
		t.Fatalf("served-authority breach must populate engine_violations: %+v", report)
	}
	if !report.LedgerUnchanged {
		t.Fatalf("a served-authority breach is not a ledger mutation; ledger must stay unchanged: %+v", report)
	}
}

// TestObservedFactRoutesToCandidateNotLedger proves an observed_fact (no ClaimID) never enters
// recall, lands only as a proposed observed-derivation candidate with engine provenance, mutates
// zero ledger bytes, and is idempotent on its content id.
func TestObservedFactRoutesToCandidateNotLedger(t *testing.T) {
	s := storeWith(t, "product:focus", "positioning", "owned capture")
	before, err := s.LedgerDigest()
	if err != nil {
		t.Fatal(err)
	}
	of := ObservedFact{Entity: "acme:widget", Predicate: "supports", Object: "single sign-on", Snippet: "bounded evidence text"}
	eng := &cannedHitEngine{name: "rented-breadth", hits: []Hit{{Kind: KindObservedFact, Observed: &of}}}

	claims, err := s.QueryBreadth("owned", 10, eng)
	if err != nil {
		t.Fatalf("an observed_fact must route to a candidate, not error: %v", err)
	}
	// (a) it must NEVER enter recall.
	for _, c := range claims {
		if c.Entity == of.Entity && c.Predicate == of.Predicate && c.Object == of.Object {
			t.Fatalf("observed_fact leaked into ambient recall claims: %+v", c)
		}
	}
	// (b) a proposed, observed-derivation candidate with engine provenance exists in the inbox.
	cand := findObservedCandidate(t, s, of.Entity, of.Predicate, of.Object)
	if cand.State != "proposed" {
		t.Fatalf("observed candidate must be proposed, got %q", cand.State)
	}
	if cand.PromotionBasis != "" {
		t.Fatalf("observed candidate must carry no promotion basis, got %q", cand.PromotionBasis)
	}
	if cand.Derivation != DerivationObserved {
		t.Fatalf("observed candidate derivation = %q, want %q", cand.Derivation, DerivationObserved)
	}
	if cand.ProvenanceLane != eng.name {
		t.Fatalf("observed candidate provenance lane = %q, want engine name %q", cand.ProvenanceLane, eng.name)
	}
	if cand.SourceTraceRef != "engine:"+eng.name+":breadth" {
		t.Fatalf("observed candidate source-trace = %q, want engine:%s:breadth", cand.SourceTraceRef, eng.name)
	}
	// (c) zero ledger bytes changed.
	after, err := s.LedgerDigest()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("observed_fact capture mutated the owned ledger: before=%s after=%s", before, after)
	}
	// (d) idempotent: re-surfacing the identical fact writes no second candidate.
	if _, err := s.QueryBreadth("owned", 10, eng); err != nil {
		t.Fatalf("second observed_fact query: %v", err)
	}
	if got := countObservedCandidates(t, s, of.Entity, of.Predicate, of.Object); got != 1 {
		t.Fatalf("observed_fact capture must be idempotent on content id, found %d candidates", got)
	}
}

func TestQueryBreadthReadOnlyValidatesButDoesNotCaptureObservedFact(t *testing.T) {
	s := storeWith(t, "product:focus", "positioning", "owned capture")
	owned, err := s.AllCurrent()
	if err != nil || len(owned) != 1 {
		t.Fatalf("owned claims = %+v, err=%v", owned, err)
	}
	of := ObservedFact{Entity: "engine-query-echo", Predicate: "query_echo", Object: "PRIVATE_QUERY_SENTINEL"}
	eng := &cannedHitEngine{name: "rented-breadth", hits: []Hit{
		{Kind: KindIndexHit, ClaimID: owned[0].ID, Score: 1},
		{Kind: KindObservedFact, Observed: &of},
	}}
	querier, ok := any(s).(interface {
		QueryBreadthReadOnly(string, int, Engine) ([]Claim, error)
	})
	if !ok {
		t.Fatal("Store does not expose read-only breadth retrieval")
	}
	before, err := s.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	claims, err := querier.QueryBreadthReadOnly("PRIVATE_QUERY_SENTINEL", 10, eng)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].ID != owned[0].ID {
		t.Fatalf("read-only breadth lost owned semantic hit or admitted observed fact: %+v", claims)
	}
	after, err := s.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("read-only breadth retrieval captured observed fact: before=%d after=%d", len(before), len(after))
	}
	malformed := &cannedHitEngine{name: "rented-breadth", hits: []Hit{{
		Kind: KindObservedFact, ClaimID: "claim_smuggled", Observed: &of,
	}}}
	if _, err := querier.QueryBreadthReadOnly("PRIVATE_QUERY_SENTINEL", 10, malformed); !errors.Is(err, ErrEngineServedUnownedClaim) {
		t.Fatalf("read-only breadth skipped malformed observed_fact validation: %v", err)
	}
}

func findObservedCandidate(t *testing.T, s *Store, entity, pred, obj string) Candidate {
	t.Helper()
	cands, err := s.AllCandidates()
	if err != nil {
		t.Fatalf("all candidates: %v", err)
	}
	for _, c := range cands {
		if c.Entity == entity && c.Predicate == pred && c.Object == obj {
			return c
		}
	}
	t.Fatalf("no candidate for %s/%s/%s in %+v", entity, pred, obj, cands)
	return Candidate{}
}

func countObservedCandidates(t *testing.T, s *Store, entity, pred, obj string) int {
	t.Helper()
	cands, err := s.AllCandidates()
	if err != nil {
		t.Fatalf("all candidates: %v", err)
	}
	n := 0
	for _, c := range cands {
		if c.Entity == entity && c.Predicate == pred && c.Object == obj {
			n++
		}
	}
	return n
}
