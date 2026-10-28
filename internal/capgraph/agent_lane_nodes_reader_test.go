package capgraph

import (
	"testing"
)

// AgentLaneNodes is the read-only reader the doctor surfacing matrix uses to enumerate
// lane rows. It must return exactly the agent-lane nodes (never other capability kinds),
// sorted by id, with the harness parsed from the lane identity — and must skip nothing but
// non-lane nodes so the matrix and the capgraph never disagree about which lanes exist.
func TestAgentLaneNodesReturnsOnlyLaneNodesSortedWithHarness(t *testing.T) {
	home := t.TempDir()
	store, err := Open(home)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	// Two agent-lane nodes (deliberately out of id order) + one unrelated skill node.
	laneB := Node{
		ID:           "agent-lane:codex--auto-build--gpt--balanced",
		Kind:         agentLaneNodeKind,
		Name:         "codex / auto/build / gpt / balanced",
		Weight:       0.25,
		EvidenceOnly: true,
	}
	laneA := Node{
		ID:           "agent-lane:claude-code--auto-build--claude--balanced",
		Kind:         agentLaneNodeKind,
		Name:         "claude-code / auto/build / claude / balanced",
		Weight:       0.25,
		EvidenceOnly: true,
	}
	skill := Node{ID: "skill:tdd", Kind: "skill", Name: "tdd", Weight: 1}
	for _, n := range []Node{laneB, laneA, skill} {
		if err := store.Add(n); err != nil {
			t.Fatalf("add %s: %v", n.ID, err)
		}
	}

	got, err := store.AgentLaneNodes()
	if err != nil {
		t.Fatalf("AgentLaneNodes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 lane nodes (skill excluded), got %d: %+v", len(got), got)
	}
	// Sorted by id: claude-code lane sorts before codex lane.
	if got[0].ID != laneA.ID || got[1].ID != laneB.ID {
		t.Fatalf("nodes not sorted by id: %s then %s", got[0].ID, got[1].ID)
	}
	if got[0].Harness != "claude-code" {
		t.Fatalf("harness parsed from name wrong: got %q want claude-code", got[0].Harness)
	}
	if got[1].Harness != "codex" {
		t.Fatalf("harness parsed from name wrong: got %q want codex", got[1].Harness)
	}
}

// A retired lane node is still returned by the reader (the reader is a pure lister); the
// Retired flag is carried so a consumer can decide whether to show it.
func TestAgentLaneNodesCarriesRetiredFlag(t *testing.T) {
	home := t.TempDir()
	store, err := Open(home)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Add(Node{
		ID:           "agent-lane:zai--auto-fast--glm--fast",
		Kind:         agentLaneNodeKind,
		Name:         "zai / auto/fast / glm / fast",
		Weight:       0.25,
		EvidenceOnly: true,
		Retired:      true,
	}); err != nil {
		t.Fatalf("add retired lane: %v", err)
	}
	got, err := store.AgentLaneNodes()
	if err != nil {
		t.Fatalf("AgentLaneNodes: %v", err)
	}
	if len(got) != 1 || !got[0].Retired {
		t.Fatalf("want 1 retired lane node, got %+v", got)
	}
	if got[0].Harness != "zai" {
		t.Fatalf("harness parse wrong: %q", got[0].Harness)
	}
}
