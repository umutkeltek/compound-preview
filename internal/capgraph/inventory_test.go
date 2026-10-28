package capgraph

import (
	"strings"
	"testing"
)

func TestHarnessFilterKeepsGenericCapabilities(t *testing.T) {
	// A generic capability really IS offered to every harness. Filtering it out
	// of a per-harness list would make this view disagree with what the executor
	// is actually handed -- and a list that disagrees with reality is worse than
	// no list, because it gets trusted.
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	addNode(t, s, "tool:generic", "tool", false)
	addNode(t, s, "tool:mine", "tool", false)
	addNode(t, s, "tool:theirs", "tool", false)
	addEdge(t, s, EdSurfacesWith, "tool:mine", "harness:alpha")
	addEdge(t, s, EdSurfacesWith, "tool:theirs", "harness:beta")

	inv, err := s.CapabilityInventory()
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	got := inv.Filter("alpha", "", false, false, false)
	ids := map[string]bool{}
	for _, st := range got {
		ids[st.ID] = true
	}
	if !ids["tool:generic"] {
		t.Fatal("generic capability hidden from a harness view; it is offered to that harness in reality")
	}
	if !ids["tool:mine"] {
		t.Fatal("capability scoped to this harness missing from its own view")
	}
	if ids["tool:theirs"] {
		t.Fatal("capability scoped to ANOTHER harness leaked into this view")
	}
}

func TestHarnessFilterAcceptsBareAndPrefixedNames(t *testing.T) {
	// The graph stores "harness:claude_code"; an operator types "claude_code".
	// Making them type the prefix is the kind of friction that ends with the
	// command not being used.
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	addNode(t, s, "tool:mine", "tool", false)
	addEdge(t, s, EdSurfacesWith, "tool:mine", "harness:alpha")
	inv, err := s.CapabilityInventory()
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if len(inv.Filter("alpha", "", false, false, false)) != 1 {
		t.Fatal("bare harness name did not match")
	}
	if len(inv.Filter("harness:alpha", "", false, false, false)) != 1 {
		t.Fatal("prefixed harness name did not match")
	}
}

func TestRetiredCapabilitiesAreHiddenUnlessAskedFor(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	addNode(t, s, "tool:live", "tool", false)
	addNode(t, s, "tool:dead", "tool", true)
	inv, err := s.CapabilityInventory()
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if n := len(inv.Filter("", "", false, false, false)); n != 1 {
		t.Fatalf("default listing showed %d, want 1 (retired hidden)", n)
	}
	if n := len(inv.Filter("", "", false, false, true)); n != 2 {
		t.Fatalf("--include-retired showed %d, want 2", n)
	}
}

func TestPlaceholderSurfaceIsNotShownAsReachable(t *testing.T) {
	// The row view must apply the same rule as the rollup, or an operator
	// drilling into a gap finds rows that contradict the number that sent them.
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	addNode(t, s, "tool:hollow", "tool", false)
	if _, err := s.AddConnectionSurface(ConnectionSurface{
		NodeID: "tool:hollow", AssertingOwner: "someone",
	}); err != nil {
		t.Fatalf("add surface: %v", err)
	}
	inv, err := s.CapabilityInventory()
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if inv.State("tool:hollow").Reachable {
		t.Fatal("a surface with neither endpoint nor config path reported reachable in the row view")
	}
}

func TestFlagsColumnStaysFixedWidth(t *testing.T) {
	// The column is scanned vertically. A ragged one defeats the only reason it
	// exists.
	//
	// The invariant is UNIFORMITY, not a particular number, so this measures every
	// state against the empty one rather than a hardcoded width. Pinning the
	// literal width made adding a legitimate flag look like a regression while
	// still not catching the failure that matters: a new field whose glyph is
	// emitted only when set, which shortens exactly the rows that carry it and is
	// invisible in any sample set that omits that field.
	//
	// So each flag-bearing field gets its own case, and a new field belongs here
	// the day it is added.
	states := []CapabilityState{
		{},
		{Reachable: true},
		{Unlockable: true},
		{Harnesses: []string{"harness:a"}},
		{Missed: 1},
		{FalseTriggered: 1},
		{Health: "offline"},
		{Reachable: true, Unlockable: true, Harnesses: []string{"harness:a"}, Missed: 2, Health: "error"},
	}
	want := len(CapabilityState{}.Flags())
	if want == 0 {
		t.Fatal("empty state rendered no flags at all; the column has no width to keep")
	}
	for _, st := range states {
		if got := len(st.Flags()); got != want {
			t.Fatalf("flags %q width %d, want %d (ragged column)", st.Flags(), got, want)
		}
	}
}

func TestUnscopedRowsSayGenericOutLoud(t *testing.T) {
	// "generic" is the finding, not an absence. Rendering nothing would let the
	// most common and most costly state read as unremarkable.
	st := CapabilityState{ID: "tool:x"}
	if !strings.Contains(st.Note(), "generic") {
		t.Fatalf("unscoped row note %q does not say generic", st.Note())
	}
	scoped := CapabilityState{ID: "tool:y", Harnesses: []string{"harness:alpha"}}
	if strings.Contains(scoped.Note(), "generic") {
		t.Fatalf("scoped row note %q wrongly says generic", scoped.Note())
	}
	if !strings.Contains(scoped.Note(), "alpha") {
		t.Fatalf("scoped row note %q does not name the harness", scoped.Note())
	}
}

func TestCorrectionCountsUseTheCapabilityEnd(t *testing.T) {
	// Same direction trap as the rollup: these edges run situation -> capability.
	// Getting it backwards yields a silent zero that reads as "nothing to learn".
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	addNode(t, s, "tool:corrected", "tool", false)
	addEdge(t, s, EdMissedWhen, "context:some-situation", "tool:corrected")
	addEdge(t, s, EdFalseTriggeredBy, "context:other-situation", "tool:corrected")
	inv, err := s.CapabilityInventory()
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	st := inv.State("tool:corrected")
	if st.Missed != 1 || st.FalseTriggered != 1 {
		t.Fatalf("state = %+v, want 1 missed / 1 false-triggered", st)
	}
	if !strings.Contains(st.Flags(), "!") {
		t.Fatalf("corrected capability not flagged: %q", st.Flags())
	}
}

// FALSIFIER: a degraded capability is deliberately still LISTED -- that is the
// whole point of health being separate from retirement -- so the row is the only
// thing that can distinguish it from a working one. If the projection drops
// Health, "reachable" reads as "usable" for a server that is currently down, and
// being listed becomes a trap rather than a service. Drop Health from
// CapabilityState (or from the Flags/Note renderers) and this goes red.
func TestDegradedCapabilityIsListedWithItsCondition(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Add(Node{ID: "mcp:down", Kind: "mcp", Name: "down", Weight: 1, Health: "offline"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := s.Add(Node{ID: "mcp:up", Kind: "mcp", Name: "up", Weight: 1}); err != nil {
		t.Fatalf("add: %v", err)
	}
	inv, err := s.CapabilityInventory()
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}

	down := inv.State("mcp:down")
	if down.Health != "offline" {
		t.Errorf("degraded row Health = %q, want offline", down.Health)
	}
	if !strings.Contains(down.Flags(), "D") {
		t.Errorf("degraded capability not flagged: %q", down.Flags())
	}
	// The specific condition, not merely that there is one: offline and
	// auth_required call for different responses (restart vs authenticate).
	if !strings.Contains(down.Note(), "offline") {
		t.Errorf("note does not name the condition: %q", down.Note())
	}
	// It must still be RETURNED -- degraded is not hidden. Filtering it out here
	// would reproduce retirement's bug through the list surface instead.
	if down.ID == "" {
		t.Error("degraded capability missing from the inventory entirely")
	}

	up := inv.State("mcp:up")
	if up.Health != "" || strings.Contains(up.Flags(), "D") {
		t.Errorf("healthy capability marked degraded: health=%q flags=%q", up.Health, up.Flags())
	}
}
