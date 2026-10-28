package lanebeliefs

import (
	"strings"

	"compound/internal/envelope"
	"compound/internal/mem"
)

// Lane attribution is the B0 identity door and NOTHING else. A receipt's declared
// Producer.LaneID resolves to a canonical agent-lane node id ONLY when that node is imported in
// the home's capability graph (mem.LaneResolver, satisfied by *capgraph.Store.ResolveLaneRef);
// a raw actor string, a bare harness, or an unimported node id all resolve to nothing and yield
// ZERO evidence (binding taxonomy rule 1 + falsifier l). There is no short-id-to-synthetic-node
// guess here — that path is retired.

const (
	LaneResolutionResolved   = "resolved"
	LaneResolutionUnresolved = "unresolved"
)

// LaneResolution is the thin outcome the fold reads: either a receipt attributes to an imported
// lane node, or it is an explicit unresolved (first-class zero-evidence) outcome the fold counts.
type LaneResolution struct {
	Outcome string
	LaneID  string
	NodeID  string
}

// Resolved reports whether the receipt attributed to an imported agent-lane node.
func (r LaneResolution) Resolved() bool { return r.Outcome == LaneResolutionResolved }

// resolveReceiptLane attributes a receipt to a lane through the B0 store door only. The declared
// lane must resolve to an imported agent-lane node; anything else is unresolved and contributes
// no evidence — the receipt is counted, never guessed onto a synthetic node.
func resolveReceiptLane(resolver mem.LaneResolver, receipt envelope.Receipt) LaneResolution {
	laneID := strings.TrimSpace(receipt.Producer.LaneID)
	if laneID == "" {
		return LaneResolution{Outcome: LaneResolutionUnresolved}
	}
	nodeID, ok := resolver.ResolveLaneRef(laneID)
	if !ok {
		return LaneResolution{Outcome: LaneResolutionUnresolved, LaneID: laneID}
	}
	return LaneResolution{Outcome: LaneResolutionResolved, LaneID: laneID, NodeID: nodeID}
}
