package capgraph

import "testing"

func TestAgentLaneNodeIDExportMatchesImportedTupleIdentity(t *testing.T) {
	tuple := AgentLaneTuple{
		Harness: "fixture-harness",
		Route:   "fixture/route",
		Model:   "fixture-model",
		Effort:  "fixture-effort",
	}
	got := AgentLaneNodeID(tuple)
	want := "agent-lane:fixture-harness--fixture-route--fixture-model--fixture-effort"
	if got != want {
		t.Fatalf("AgentLaneNodeID() = %q, want %q", got, want)
	}
}

func TestShortLaneIDMappingDeterministicAndTotalForRoutePanelVocabularyShapes(t *testing.T) {
	shortLaneIDs := []string{
		"fixture/judge",
		"fixture/extract",
		"local/test-judge",
		"panel/cheap-judge",
		"lane/base",
		"lane/candidate",
		"standalone",
	}
	seen := map[string]bool{}
	for _, laneID := range shortLaneIDs {
		first, ok := AgentLaneNodeIDFromShortLaneID(laneID)
		if !ok {
			t.Fatalf("AgentLaneNodeIDFromShortLaneID(%q) unresolved, want total mapping", laneID)
		}
		second, ok := AgentLaneNodeIDFromShortLaneID(laneID)
		if !ok || second != first {
			t.Fatalf("AgentLaneNodeIDFromShortLaneID(%q) not deterministic: first=%q second=%q ok=%v", laneID, first, second, ok)
		}
		if seen[first] {
			t.Fatalf("short lane id %q collided at %q", laneID, first)
		}
		seen[first] = true
	}
}

func TestShortLaneIDMappingRejectsEmptyAndWildcardSentinels(t *testing.T) {
	for _, laneID := range []string{"", " ", "*"} {
		if got, ok := AgentLaneNodeIDFromShortLaneID(laneID); ok || got != "" {
			t.Fatalf("AgentLaneNodeIDFromShortLaneID(%q) = %q,%v; want unresolved", laneID, got, ok)
		}
	}
}
