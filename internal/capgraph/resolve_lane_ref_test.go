package capgraph

import "testing"

// ResolveLaneRef is the single no-guessing lane resolver (B0). A ref only resolves when it
// names EXACTLY ONE imported agent-lane node: either an existing node id (passthrough) or a
// short lane id whose deterministic mapping lands on an imported node. Everything that could
// name many lanes (a bare harness) or that is not a lane at all (an actor string, an owner,
// empty) resolves to nothing — falsifier (l): an unresolvable lane is ZERO evidence, never a
// guess. Store membership is the discriminator that makes "short-id resolves" and
// "harness/actor refuse" true at the same time, mirroring routeevidence's existing pattern.
func TestResolveLaneRefFalsifiers(t *testing.T) {
	home := t.TempDir()
	store, err := Open(home)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	// Seed one imported agent-lane node whose id is exactly the short-id mapping of
	// "fixture/judge" — so the short-id path has an imported node to land on.
	shortID := "fixture/judge"
	shortNodeID, ok := AgentLaneNodeIDFromShortLaneID(shortID)
	if !ok {
		t.Fatalf("AgentLaneNodeIDFromShortLaneID(%q) unresolved in fixture setup", shortID)
	}
	// And a second lane seeded directly by node id, to prove node-id passthrough.
	directNodeID := AgentLaneNodeID(AgentLaneTuple{Harness: "fixture", Route: "fixture/build", Model: "fixture-model", Effort: "balanced"})
	for _, id := range []string{shortNodeID, directNodeID} {
		if err := store.Add(Node{ID: id, Kind: agentLaneNodeKind, Name: id, Weight: 0.25, EvidenceOnly: true}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	t.Run("node-id passthrough resolves to itself", func(t *testing.T) {
		got, ok := store.ResolveLaneRef(directNodeID)
		if !ok || got != directNodeID {
			t.Fatalf("ResolveLaneRef(%q) = %q,%v; want %q,true", directNodeID, got, ok, directNodeID)
		}
	})

	t.Run("short-id resolves byte-identical to AgentLaneNodeIDFromShortLaneID", func(t *testing.T) {
		want, _ := AgentLaneNodeIDFromShortLaneID(shortID)
		got, ok := store.ResolveLaneRef(shortID)
		if !ok || got != want {
			t.Fatalf("ResolveLaneRef(%q) = %q,%v; want %q,true (byte-identical to the exported mapper)", shortID, got, ok, want)
		}
	})

	t.Run("bare harness refuses (no-guessing: many lanes share one harness)", func(t *testing.T) {
		// "fixture" is the harness of the seeded lane, but a bare harness names many lanes.
		if got, ok := store.ResolveLaneRef("fixture"); ok || got != "" {
			t.Fatalf("ResolveLaneRef(%q) = %q,%v; want \"\",false", "fixture", got, ok)
		}
	})

	t.Run("actor string refuses", func(t *testing.T) {
		for _, actor := range []string{"agent-harness:0f0f0f0f", "operator:x", "codex"} {
			if got, ok := store.ResolveLaneRef(actor); ok || got != "" {
				t.Fatalf("ResolveLaneRef(%q) = %q,%v; want \"\",false", actor, got, ok)
			}
		}
	})

	t.Run("empty refuses", func(t *testing.T) {
		for _, ref := range []string{"", "   "} {
			if got, ok := store.ResolveLaneRef(ref); ok || got != "" {
				t.Fatalf("ResolveLaneRef(%q) = %q,%v; want \"\",false", ref, got, ok)
			}
		}
	})

	t.Run("node-id-shaped ref not in the store refuses (membership, not shape)", func(t *testing.T) {
		absent := AgentLaneNodeID(AgentLaneTuple{Harness: "ghost", Route: "ghost/route", Model: "ghost", Effort: "unknown"})
		if got, ok := store.ResolveLaneRef(absent); ok || got != "" {
			t.Fatalf("ResolveLaneRef(%q) = %q,%v; want \"\",false (never imported)", absent, got, ok)
		}
	})

	t.Run("short-id whose mapped node is not imported refuses", func(t *testing.T) {
		if got, ok := store.ResolveLaneRef("unseeded/lane"); ok || got != "" {
			t.Fatalf("ResolveLaneRef(%q) = %q,%v; want \"\",false", "unseeded/lane", got, ok)
		}
	})
}
