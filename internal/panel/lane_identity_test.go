package panel

import (
	"testing"

	"compound/internal/capgraph"
)

func TestComparisonShortLaneIDsMapIntoCapgraphCanonicalIdentity(t *testing.T) {
	for _, laneID := range []string{"lane/base", "lane/candidate", "panel/cheap-judge"} {
		got, ok := canonicalComparisonLaneNodeID(laneID)
		if !ok {
			t.Fatalf("canonicalComparisonLaneNodeID(%q) unresolved", laneID)
		}
		want, ok := capgraph.AgentLaneNodeIDFromShortLaneID(laneID)
		if !ok {
			t.Fatalf("capgraph mapping for %q unresolved", laneID)
		}
		if got != want {
			t.Fatalf("comparison lane %q canonical id = %q, want capgraph id %q", laneID, got, want)
		}
	}
}
