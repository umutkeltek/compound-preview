package capgraph

import (
	"testing"
	"time"
)

// AgentLaneCards joins each agent-lane node with its outgoing capable_of/tends_to/validated_by
// edges into a read-only display card: capability domains and tendencies come back as clean
// slugs (prefixes stripped), sorted deterministically; validated_by is a lane-level tally; and
// non-lane nodes are excluded. Cards are sorted by id so the peers section renders stably.
func TestAgentLaneCardsJoinsEdgesIntoSortedSlugs(t *testing.T) {
	home := t.TempDir()
	store, err := Open(home)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)

	laneB := Node{ID: "agent-lane:codex--auto-build--gpt--balanced", Kind: agentLaneNodeKind,
		Name: "codex / auto/build / gpt / balanced", Weight: 0.25, EvidenceOnly: true}
	laneA := Node{ID: "agent-lane:claude-code--auto-build--claude--balanced", Kind: agentLaneNodeKind,
		Name: "claude-code / auto/build / claude / balanced", Weight: 0.25, EvidenceOnly: true}
	skill := Node{ID: "skill:tdd", Kind: "skill", Name: "tdd", Weight: 1}
	for _, n := range []Node{laneB, laneA, skill} {
		if err := store.Add(n); err != nil {
			t.Fatalf("add %s: %v", n.ID, err)
		}
	}
	// laneA: two capabilities (out of slug order), two tendencies, two validated_by receipts.
	// A capable_of edge onto the skill node must NOT count as a lane capability.
	for _, e := range []Edge{
		{Kind: EdCapableOf, From: laneA.ID, To: "task-class:tests-from-spec", Confidence: 1},
		{Kind: EdCapableOf, From: laneA.ID, To: "task-class:build-implement", Confidence: 1},
		{Kind: EdTendsTo, From: laneA.ID, To: "tendency:receipt-faithful", Confidence: 1},
		{Kind: EdTendsTo, From: laneA.ID, To: "tendency:falsifier-first", Confidence: 1},
		{Kind: EdValidatedBy, From: laneA.ID, To: "eval:alpha-receipt", Confidence: 1},
		{Kind: EdValidatedBy, From: laneA.ID, To: "receipt:rec_beta", Confidence: 1},
		{Kind: EdCapableOf, From: skill.ID, To: "task-class:noise", Confidence: 1},
	} {
		if err := store.AddEdge(e, now); err != nil {
			t.Fatalf("add edge %+v: %v", e, err)
		}
	}

	cards, err := store.AgentLaneCards()
	if err != nil {
		t.Fatalf("AgentLaneCards: %v", err)
	}
	if len(cards) != 2 {
		t.Fatalf("want 2 lane cards (skill excluded), got %d: %+v", len(cards), cards)
	}
	// Sorted by id: claude-code lane first.
	if cards[0].ID != laneA.ID || cards[1].ID != laneB.ID {
		t.Fatalf("cards not sorted by id: %s then %s", cards[0].ID, cards[1].ID)
	}
	a := cards[0]
	if a.Harness != "claude-code" {
		t.Fatalf("harness parsed wrong: %q", a.Harness)
	}
	// Capabilities come back as sorted slugs, prefixes stripped.
	if got := a.Capabilities; len(got) != 2 || got[0] != "build-implement" || got[1] != "tests-from-spec" {
		t.Fatalf("capabilities wrong (want sorted stripped slugs): %v", got)
	}
	if got := a.Tendencies; len(got) != 2 || got[0] != "falsifier-first" || got[1] != "receipt-faithful" {
		t.Fatalf("tendencies wrong (want sorted stripped slugs): %v", got)
	}
	if a.ValidatedByCount != 2 {
		t.Fatalf("validated_by tally wrong: got %d want 2", a.ValidatedByCount)
	}
	// laneB has no edges: empty slices, zero tally, but still a card.
	b := cards[1]
	if len(b.Capabilities) != 0 || len(b.Tendencies) != 0 || b.ValidatedByCount != 0 {
		t.Fatalf("edgeless lane should have empty card content: %+v", b)
	}
}
