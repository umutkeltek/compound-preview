package mem

import (
	"testing"
)

// build a small entity graph: app -> auth -> jwt, and app -> db (a separate hop), plus an
// unrelated island. Returns the store.
func graphStore(t *testing.T) *Store {
	s := newStore(t)
	mk := func(e, p, o string) {
		c, _ := s.Capture([]Candidate{cand(e, p, o, "")}, t0)
		_, _ = promoteForTest(t, s, c[0].ID, "", t0)
	}
	mk("app", "lang", "go")
	mk("auth", "lang", "go")
	mk("jwt", "kind", "library")
	mk("db", "engine", "postgres")
	mk("island", "x", "y") // unconnected
	s.Relate("app", "depends_on", "auth", 0.9, t0)
	s.Relate("auth", "depends_on", "jwt", 0.9, t0)
	s.Relate("app", "depends_on", "db", 0.4, t0) // low confidence
	return s
}

// Task-shaped recall: seed -> bounded traversal -> connected subgraph + coverage. Depth bounds
// how far it walks; the unconnected island is never reached.
func TestRecallGraphTraversal(t *testing.T) {
	s := graphStore(t)
	// depth 2 from "app": app -> auth (1) -> jwt (2), plus app -> db (1). island excluded.
	sg, err := s.RecallGraph("app", 2, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range sg.Entities {
		got[e] = true
	}
	for _, want := range []string{"app", "auth", "jwt", "db"} {
		if !got[want] {
			t.Errorf("depth-2 subgraph from app should reach %q; entities=%v", want, sg.Entities)
		}
	}
	if got["island"] {
		t.Errorf("the unconnected island must not be reached; entities=%v", sg.Entities)
	}
	if len(sg.Claims) != 4 {
		t.Errorf("expected 4 current claims in the subgraph, got %d", len(sg.Claims))
	}
}

// Depth bounds the walk: depth 1 from app reaches only direct neighbors, not jwt (2 hops).
func TestRecallGraphDepthBound(t *testing.T) {
	s := graphStore(t)
	sg, _ := s.RecallGraph("app", 1, 20, 0)
	got := map[string]bool{}
	for _, e := range sg.Entities {
		got[e] = true
	}
	if !got["auth"] || !got["db"] {
		t.Errorf("depth 1 should reach auth and db; got %v", sg.Entities)
	}
	if got["jwt"] {
		t.Errorf("depth 1 must NOT reach jwt (2 hops); got %v", sg.Entities)
	}
}

// Confidence gates relations: a min-conf above a weak edge prunes that branch.
func TestRecallGraphConfidenceGate(t *testing.T) {
	s := graphStore(t)
	// app->db is conf 0.4; require 0.5 so that branch is pruned.
	sg, _ := s.RecallGraph("app", 2, 20, 0.5)
	for _, e := range sg.Entities {
		if e == "db" {
			t.Errorf("the low-confidence app->db edge should be gated out at min-conf 0.5; got %v", sg.Entities)
		}
	}
}

// Budget truncates a large subgraph and SAYS so (no silent cap).
func TestRecallGraphBudget(t *testing.T) {
	s := graphStore(t)
	sg, _ := s.RecallGraph("app", 5, 2, 0) // budget 2 entities
	if !sg.BudgetHit {
		t.Errorf("a budget of 2 should be hit and reported; coverage=%q", sg.Coverage)
	}
}

// No fabrication: a seed that matches nothing says so plainly.
func TestRecallGraphSeedMiss(t *testing.T) {
	s := graphStore(t)
	sg, _ := s.RecallGraph("nonexistent-entity", 2, 20, 0)
	if len(sg.SeedEntities) != 0 || len(sg.Claims) != 0 {
		t.Errorf("an unmatched seed must return nothing, got seeds=%v claims=%d", sg.SeedEntities, len(sg.Claims))
	}
}

// A relation between two reached entities at the depth FRONTIER (never traversed because the BFS
// stops expanding there) must still be reported — relations are "all edges among reached entities".
func TestRecallGraphFrontierRelation(t *testing.T) {
	s := newStore(t)
	mk := func(e string) {
		c, _ := s.Capture([]Candidate{cand(e, "k", "v", "")}, t0)
		_, _ = promoteForTest(t, s, c[0].ID, "", t0)
	}
	mk("app")
	mk("x")
	mk("y")
	s.Relate("app", "relates", "x", 0.9, t0)
	s.Relate("app", "relates", "y", 0.9, t0)
	s.Relate("x", "relates", "y", 0.9, t0)  // boundary cross-edge: x and y are reached but never expanded
	sg, _ := s.RecallGraph("app", 1, 20, 0) // depth 1 reaches app,x,y; x and y are frontier (not expanded)
	hasXY := false
	for _, r := range sg.Relations {
		if (r.From == "x" && r.To == "y") || (r.From == "y" && r.To == "x") {
			hasXY = true
		}
	}
	if !hasXY {
		t.Errorf("the x-y boundary relation (both ends reached) must be reported; relations=%v", sg.Relations)
	}
}

// The structured extractor parses both a bare candidate array and an {candidates, relations} object.
func TestStructuredExtractor(t *testing.T) {
	bare := []byte(`[{"entity":"e","predicate":"p","object":"o","derivation":"extracted","source_trace_ref":"t","confidence":0.9}]`)
	ext, err := StructuredExtractor{}.Extract(bare)
	if err != nil || len(ext.Candidates) != 1 || len(ext.Relations) != 0 {
		t.Errorf("bare array should yield 1 candidate, 0 relations; got %d/%d (%v)", len(ext.Candidates), len(ext.Relations), err)
	}
	obj := []byte(`{"candidates":[{"entity":"a","predicate":"p","object":"o","derivation":"extracted","source_trace_ref":"t","confidence":0.9}],"relations":[{"from":"a","to":"b","kind":"relates","confidence":0.8}]}`)
	ext2, err := StructuredExtractor{}.Extract(obj)
	if err != nil || len(ext2.Candidates) != 1 || len(ext2.Relations) != 1 {
		t.Errorf("object form should yield 1 candidate, 1 relation; got %d/%d (%v)", len(ext2.Candidates), len(ext2.Relations), err)
	}
}

func TestRecallGraphDirectionalPartOfZoom(t *testing.T) {
	s := newStore(t)
	mk := func(e string) {
		c, _ := s.Capture([]Candidate{cand(e, "kind", "industry", "")}, t0)
		if _, err := promoteForTest(t, s, c[0].ID, "", t0); err != nil {
			t.Fatalf("promote %s: %v", e, err)
		}
	}
	for _, entity := range []string{"industry:us-eu-saas", "industry:saas", "industry:tech-2026", "industry:fintech", "noise:benchmark"} {
		mk(entity)
	}
	if _, err := s.Relate("industry:us-eu-saas", "part_of", "industry:saas", 0.9, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Relate("industry:saas", "part_of", "industry:tech-2026", 0.9, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Relate("industry:fintech", "part_of", "industry:tech-2026", 0.9, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Relate("industry:us-eu-saas", "relates", "noise:benchmark", 0.9, t0); err != nil {
		t.Fatal(err)
	}

	up, err := s.RecallGraphOpts("industry:us-eu-saas", GraphOpts{Depth: 2, Budget: 20, Direction: "up", Kinds: []string{"part_of"}})
	if err != nil {
		t.Fatal(err)
	}
	assertEntities(t, up.Entities, []string{"industry:saas", "industry:tech-2026", "industry:us-eu-saas"})
	assertMissingEntities(t, up.Entities, []string{"industry:fintech", "noise:benchmark"})

	down, err := s.RecallGraphOpts("industry:tech-2026", GraphOpts{Depth: 2, Budget: 20, Direction: "down", Kinds: []string{"part_of"}})
	if err != nil {
		t.Fatal(err)
	}
	assertEntities(t, down.Entities, []string{"industry:fintech", "industry:saas", "industry:tech-2026", "industry:us-eu-saas"})
	assertMissingEntities(t, down.Entities, []string{"noise:benchmark"})
}

func assertEntities(t *testing.T, got []string, want []string) {
	t.Helper()
	seen := map[string]bool{}
	for _, e := range got {
		seen[e] = true
	}
	for _, e := range want {
		if !seen[e] {
			t.Fatalf("missing entity %q in %v", e, got)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("entities = %v, want exactly %v", got, want)
	}
}

func assertMissingEntities(t *testing.T, got []string, forbidden []string) {
	t.Helper()
	seen := map[string]bool{}
	for _, e := range got {
		seen[e] = true
	}
	for _, e := range forbidden {
		if seen[e] {
			t.Fatalf("unexpected entity %q in %v", e, got)
		}
	}
}
