package routeevidence

import (
	"testing"

	"compound/internal/capgraph"
)

func TestPromotionShortLaneIDsMapIntoCapgraphCanonicalIdentity(t *testing.T) {
	for _, laneID := range []string{"fixture/judge", "local/test-judge", "panel/cheap-judge"} {
		got, ok := canonicalPromotionLaneNodeID(laneID)
		if !ok {
			t.Fatalf("canonicalPromotionLaneNodeID(%q) unresolved", laneID)
		}
		want, ok := capgraph.AgentLaneNodeIDFromShortLaneID(laneID)
		if !ok {
			t.Fatalf("capgraph mapping for %q unresolved", laneID)
		}
		if got != want {
			t.Fatalf("promotion lane %q canonical id = %q, want capgraph id %q", laneID, got, want)
		}
	}
}
