package capgraph

import (
	"testing"
	"time"
)

func addNode(t *testing.T, s *Store, id, kind string, retired bool) {
	t.Helper()
	if err := s.Add(Node{ID: id, Kind: kind, Name: id, Retired: retired}); err != nil {
		t.Fatalf("add node %s: %v", id, err)
	}
}

func kindRow(r Reachability, kind string) KindReachability {
	for _, k := range r.Kinds {
		if k.Kind == kind {
			return k
		}
	}
	return KindReachability{}
}

func TestReachabilityCountsNamedVersusUsable(t *testing.T) {
	// The whole point: being able to NAME a capability is not being able to USE
	// it, and the graph is full of the former. If these two ever collapse into
	// one number, the projection has stopped measuring the thing that hurts.
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	addNode(t, s, "tool:alpha", "tool", false)
	addNode(t, s, "tool:beta", "tool", false)
	addNode(t, s, "gateway:one", "gateway", false)

	if _, err := s.AddConnectionSurface(ConnectionSurface{
		NodeID: "gateway:one", Endpoint: "https://example.invalid/v1",
	}); err != nil {
		t.Fatalf("add surface: %v", err)
	}

	r, err := s.ComputeReachability()
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if got := kindRow(r, "tool"); got.Nodes != 2 || got.Reachable != 0 {
		t.Fatalf("tool row = %+v, want 2 named / 0 reachable", got)
	}
	if got := kindRow(r, "gateway"); got.Nodes != 1 || got.Reachable != 1 {
		t.Fatalf("gateway row = %+v, want 1 named / 1 reachable", got)
	}
	if r.Total.Nodes != 3 || r.Total.Reachable != 1 {
		t.Fatalf("total = %+v, want 3 named / 1 reachable", r.Total)
	}
}

func TestPlaceholderSurfaceDoesNotCountAsReachable(t *testing.T) {
	// A surface with no endpoint and no config path answers nothing. Counting it
	// would report the gap as closed while an agent still cannot act -- which is
	// worse than an open gap, because nobody goes looking for a closed one.
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	addNode(t, s, "harness:example", "harness", false)
	if _, err := s.AddConnectionSurface(ConnectionSurface{
		NodeID: "harness:example", AssertingOwner: "someone",
	}); err != nil {
		t.Fatalf("add surface: %v", err)
	}
	r, err := s.ComputeReachability()
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if got := kindRow(r, "harness"); got.Reachable != 0 {
		t.Fatalf("a surface carrying neither endpoint nor config path counted as reachable: %+v", got)
	}
}

func TestRetiredCapabilitiesAreNotCountedAsGaps(t *testing.T) {
	// A number an operator learns to discount is a number that stops working.
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	addNode(t, s, "tool:live", "tool", false)
	addNode(t, s, "tool:dead", "tool", true)
	r, err := s.ComputeReachability()
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if got := kindRow(r, "tool"); got.Nodes != 1 {
		t.Fatalf("retired capability counted in the gap: tool row = %+v, want 1 node", got)
	}
}

func TestReachableIsNotTheSameAsUnlockable(t *testing.T) {
	// "I can reach it but I have no credential" is the exact state that sends an
	// agent hunting for a key by hand, so it must be visible as its own number
	// rather than hidden inside the reachable count.
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	addNode(t, s, "gateway:open", "gateway", false)
	addNode(t, s, "gateway:keyed", "gateway", false)
	if _, err := s.AddConnectionSurface(ConnectionSurface{
		NodeID: "gateway:open", Endpoint: "https://example.invalid/a",
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := s.AddConnectionSurface(ConnectionSurface{
		NodeID: "gateway:keyed", Endpoint: "https://example.invalid/b",
		CredentialHandle: CredentialHandle{
			Ref: "brokered://example-tenant", Custodian: "gateway", EnvName: "EXAMPLE_API_KEY",
		},
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	r, err := s.ComputeReachability()
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	got := kindRow(r, "gateway")
	if got.Reachable != 2 || got.Unlockable != 1 {
		t.Fatalf("gateway row = %+v, want 2 reachable / 1 unlockable", got)
	}
	// The brokered mechanism passes the value through a process we start, and an
	// operator auditing exposure needs that counted, not inferred.
	if got.ValueTransiting != 1 {
		t.Fatalf("value-transiting count = %d, want 1", got.ValueTransiting)
	}
}

func TestWorstCoverageIsReportedFirst(t *testing.T) {
	// The operator reads the top of this list and nothing else, so ordering is
	// the recommendation. If a fully-covered kind can sort above a bare one, the
	// report points at the wrong door.
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	addNode(t, s, "gateway:covered", "gateway", false)
	if _, err := s.AddConnectionSurface(ConnectionSurface{
		NodeID: "gateway:covered", Endpoint: "https://example.invalid/v1",
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	for _, id := range []string{"tool:a", "tool:b", "tool:c"} {
		addNode(t, s, id, "tool", false)
	}
	r, err := s.ComputeReachability()
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(r.Kinds) == 0 || r.Kinds[0].Kind != "tool" {
		t.Fatalf("worst-covered kind is not first: %+v", r.Kinds)
	}
	if len(r.Gaps) != 1 || r.Gaps[0].Kind != "tool" || r.Gaps[0].Missing != 3 {
		t.Fatalf("gaps = %+v, want one tool gap of 3", r.Gaps)
	}
	// A fully-covered kind must produce no gap at all, or the list fills with
	// noise and stops being read.
	for _, g := range r.Gaps {
		if g.Kind == "gateway" {
			t.Fatal("a fully-covered kind produced a gap entry")
		}
	}
}

func TestEveryGapExplainsWhatItCostsAtThePointOfUse(t *testing.T) {
	// A gap that reports only a count teaches nobody which door to build next,
	// which is how a report becomes something people scroll past.
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, kind := range []string{"tool", "mcp", "skill", "adapter", "model_profile", "lens"} {
		addNode(t, s, kind+":example", kind, false)
	}
	r, err := s.ComputeReachability()
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(r.Gaps) != 6 {
		t.Fatalf("expected a gap per kind, got %d", len(r.Gaps))
	}
	for _, g := range r.Gaps {
		if g.Consequence == "" {
			t.Fatalf("gap for %q carries no consequence", g.Kind)
		}
	}
}

func addEdge(t *testing.T, s *Store, kind, from, to string) {
	t.Helper()
	if err := s.AddEdge(Edge{Kind: kind, From: from, To: to, Confidence: 1}, time.Now()); err != nil {
		t.Fatalf("add edge %s %s->%s: %v", kind, from, to, err)
	}
}

func TestUnscopedCapabilitiesCountAsGenericForEveryHarness(t *testing.T) {
	// This is the failure that hurts. An unscoped node is not hidden from an
	// executor -- it is offered to ALL of them. So a mostly-unscoped graph hands
	// every harness the same pile, including entries it cannot run, and the
	// operator filters by hand. If Generic ever stops counting toward each
	// harness, this report would call that pile "well scoped".
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	addNode(t, s, "harness:alpha", "harness", false)
	addNode(t, s, "harness:beta", "harness", false)
	addNode(t, s, "tool:scoped", "tool", false)
	addNode(t, s, "tool:loose-a", "tool", false)
	addNode(t, s, "tool:loose-b", "tool", false)
	addEdge(t, s, EdSurfacesWith, "tool:scoped", "harness:alpha")

	r, err := s.ComputeReachability()
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(r.Harnesses) != 1 {
		t.Fatalf("expected only harnesses that appear as a scope target, got %+v", r.Harnesses)
	}
	h := r.Harnesses[0]
	if h.Harness != "harness:alpha" || h.Bound != 1 {
		t.Fatalf("harness row = %+v, want harness:alpha bound=1", h)
	}
	// 5 live nodes, 1 scoped => 4 generic, offered to everyone.
	if h.Generic != 4 {
		t.Fatalf("generic = %d, want 4; unscoped nodes must count as offered-to-all", h.Generic)
	}
	if h.ScopedShare >= 0.5 {
		t.Fatalf("scoped share %.2f suggests a curated list when the pool is mostly generic", h.ScopedShare)
	}
}

func TestOperatorCorrectionsAreCountedAndTiedToScoping(t *testing.T) {
	// false_triggered_by and missed_when are the recorded residue of "use this,
	// not that". A corrected capability that is STILL unscoped is a mistake the
	// graph was told about and has not been taught to avoid, so it stays free to
	// repeat -- which is exactly why the operator repeats themselves.
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	addNode(t, s, "harness:alpha", "harness", false)
	addNode(t, s, "tool:overeager", "tool", false)
	addNode(t, s, "tool:overlooked", "tool", false)
	addNode(t, s, "tool:taught", "tool", false)
	addNode(t, s, "lens:situation", "lens", false)

	addEdge(t, s, EdFalseTriggeredBy, "context:situation", "tool:overeager")
	addEdge(t, s, EdMissedWhen, "context:situation", "tool:overlooked")
	addEdge(t, s, EdFalseTriggeredBy, "context:situation", "tool:taught")
	addEdge(t, s, EdSurfacesWith, "tool:taught", "harness:alpha")

	r, err := s.ComputeReachability()
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	c := r.Corrections
	if c.FalseTriggeredNodes != 2 || c.MissedNodes != 1 || c.Observations != 3 {
		t.Fatalf("corrections = %+v, want 2 false-triggered / 1 missed / 3 observations", c)
	}
	// Two of the three corrected nodes learned nothing: no harness scope.
	if c.UnscopedWithCorrections != 2 {
		t.Fatalf("unscoped-with-corrections = %d, want 2; a correction that changed no scope is a lesson not taken",
			c.UnscopedWithCorrections)
	}
}

func TestRetiredNodesDoNotPolluteScopingOrCorrections(t *testing.T) {
	// Same trust argument as the coverage numbers: a retired capability's old
	// mistakes are not an outstanding routing debt.
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	addNode(t, s, "harness:alpha", "harness", false)
	addNode(t, s, "tool:dead", "tool", true)
	addNode(t, s, "lens:situation", "lens", false)
	addEdge(t, s, EdFalseTriggeredBy, "context:situation", "tool:dead")
	addEdge(t, s, EdSurfacesWith, "tool:dead", "harness:alpha")

	r, err := s.ComputeReachability()
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if r.Corrections.Observations != 0 || r.Corrections.FalseTriggeredNodes != 0 {
		t.Fatalf("retired node contributed corrections: %+v", r.Corrections)
	}
	for _, h := range r.Harnesses {
		if h.Bound != 0 {
			t.Fatalf("retired node counted as bound capability for %s", h.Harness)
		}
	}
}
