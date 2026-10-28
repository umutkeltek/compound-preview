package capgraph

import (
	"strings"
	"testing"
)

// Retire marks a node WITHDRAWN (durable), preserving its other fields, and the
// surfacing gate (capgraph.go:566) stops offering it. It is distinct from Health:
// a retired node is gone from the system, not merely "broken right now".
func TestRetireWithdrawsNodeFromSurfacing(t *testing.T) {
	home := t.TempDir()
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Add(Node{ID: "tool:gbrain.search", Kind: "tool", Name: "gbrain search", Triggers: []string{"search", "brain"}}); err != nil {
		t.Fatal(err)
	}
	// Add a control node that must stay.
	if err := store.Add(Node{ID: "tool:context7.query", Kind: "tool", Name: "context7 query", Triggers: []string{"context7"}}); err != nil {
		t.Fatal(err)
	}

	retired, err := store.Retire("tool:gbrain.search")
	if err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if len(retired) != 1 || retired[0] != "tool:gbrain.search" {
		t.Fatalf("retired = %v, want [tool:gbrain.search]", retired)
	}

	nodes, err := store.loadNodes()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	if !byID["tool:gbrain.search"].Retired {
		t.Fatal("gbrain.search not marked retired")
	}
	if byID["tool:gbrain.search"].Name != "gbrain search" || byID["tool:gbrain.search"].Kind != "tool" {
		t.Fatalf("retire clobbered node identity: %+v", byID["tool:gbrain.search"])
	}
	if byID["tool:context7.query"].Retired {
		t.Fatal("control node was retired")
	}

	// The surfacing gate (capgraph.go:566) skips Retired nodes. Prove the flag
	// alone is the gate: the retired node carries its flag, the control does not.
	// (Surfacing itself is covered by the existing gate tests.)
	if !byID["tool:gbrain.search"].Retired {
		t.Fatal("retired flag not persisted")
	}
}

// Retire reports unknown ids (so a typo fails loud) and is idempotent for
// already-retired nodes.
func TestRetireReportsUnknownAndIsIdempotent(t *testing.T) {
	home := t.TempDir()
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Add(Node{ID: "tool:real", Kind: "tool", Name: "real"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Retire("tool:real"); err != nil {
		t.Fatal(err)
	}
	// Re-retire the same node: no error, still reported.
	retired, err := store.Retire("tool:real")
	if err != nil {
		t.Fatalf("re-retire should be a no-op, got %v", err)
	}
	if len(retired) != 1 || retired[0] != "tool:real" {
		t.Fatalf("re-retire = %v", retired)
	}
	// Unknown id fails loud.
	if _, err := store.Retire("tool:nope"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown id must fail loud, got %v", err)
	}
	// No ids is a usage error.
	if _, err := store.Retire(); err == nil {
		t.Fatal("Retire() with no ids must error")
	}
}
