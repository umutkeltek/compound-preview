package lanebeliefs

import (
	"testing"

	"compound/internal/envelope"
)

// stubResolver is a minimal mem.LaneResolver: it resolves exactly the refs in its map and
// refuses everything else, so the identity unit test does not depend on capgraph internals (the
// store-backed membership behaviour is proven end-to-end by the fold falsifiers).
type stubResolver map[string]string

func (s stubResolver) ResolveLaneRef(ref string) (string, bool) {
	nodeID, ok := s[ref]
	return nodeID, ok
}

func receiptWithLane(laneID string) envelope.Receipt {
	return envelope.Receipt{
		Header: envelope.Header{
			SchemaVersion: envelope.SVReceipt,
			ID:            "rec_fixture",
			Producer:      envelope.Producer{Kind: "test", Name: "fixture", LaneID: laneID},
		},
	}
}

func TestReceiptLaneUnresolvedWithoutStampedLaneID(t *testing.T) {
	res := resolveReceiptLane(stubResolver{}, receiptWithLane(""))
	if res.Resolved() || res.Outcome != LaneResolutionUnresolved {
		t.Fatalf("outcome = %q, want %q", res.Outcome, LaneResolutionUnresolved)
	}
	if res.NodeID != "" {
		t.Fatalf("a laneless receipt produced a node id %q", res.NodeID)
	}
}

func TestReceiptLaneResolvesThroughStoreDoorOnly(t *testing.T) {
	resolver := stubResolver{"agent-lane:codex--auto-build--gpt--balanced": "agent-lane:codex--auto-build--gpt--balanced"}
	res := resolveReceiptLane(resolver, receiptWithLane("agent-lane:codex--auto-build--gpt--balanced"))
	if !res.Resolved() {
		t.Fatalf("outcome = %q, want resolved", res.Outcome)
	}
	if res.NodeID != "agent-lane:codex--auto-build--gpt--balanced" {
		t.Fatalf("node id = %q, want the resolver's canonical id", res.NodeID)
	}
}

func TestReceiptLaneNeverGuessesUnimportedLane(t *testing.T) {
	// The resolver refuses these refs (they name no imported node). The retired guessing path
	// would have mapped them onto synthetic node ids anyway — resolveReceiptLane must not.
	resolver := stubResolver{}
	for _, laneID := range []string{"codex", "some-actor:abcdef", "fixture/judge", "*"} {
		res := resolveReceiptLane(resolver, receiptWithLane(laneID))
		if res.Resolved() {
			t.Fatalf("lane %q resolved without store membership (node %q)", laneID, res.NodeID)
		}
		if res.NodeID != "" {
			t.Fatalf("lane %q guessed a node id %q", laneID, res.NodeID)
		}
	}
}
