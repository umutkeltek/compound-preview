package capgraph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportAgentLaneProfilesIdempotentNoDrift(t *testing.T) {
	dir := t.TempDir()
	registryPath := filepath.Join(dir, "registry.ts")
	overlayPath := filepath.Join(dir, "overlay.json")
	writeFile(t, registryPath, `export const LANES = [];`)
	writeFile(t, overlayPath, `{
  "roles": [
    {
      "id": "lane-alpha",
      "family": "lane",
      "description": "synthetic implementation lane",
      "capability_class": "tests-from-spec",
      "quality_tier": "needs-best",
      "emit": ["h1"],
      "models": {"h1": "m1"},
      "tendencies": ["falsifier-first"],
      "validated_by": ["eval:alpha-receipt"]
    }
  ],
  "lanes": [
    {
      "id": "lane-alpha",
      "harness": "h1",
      "route": "route one",
      "cost_class": "free"
    }
  ],
  "preference_order": ["free"]
}`)
	home, store := newStore(t)

	first, err := ImportAgentLaneProfiles(home, registryPath, overlayPath, t0)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "ok" || first.AppliedNodes != 1 || first.AppliedEdges != 3 {
		t.Fatalf("first import report = %+v, want 1 node and 3 edges", first)
	}
	nodeLines := readNonEmptyLines(t, filepath.Join(home, "registries", "capability-nodes.jsonl"))
	edgeLines := readNonEmptyLines(t, filepath.Join(home, "registries", "capability-edges.jsonl"))
	if len(nodeLines) != 1 || len(edgeLines) != 3 {
		t.Fatalf("registry rows after first import nodes=%d edges=%d", len(nodeLines), len(edgeLines))
	}

	second, err := ImportAgentLaneProfiles(home, registryPath, overlayPath, t0)
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != "ok" || second.AppliedNodes != 0 || second.AppliedEdges != 0 {
		t.Fatalf("second import report = %+v, want zero drift", second)
	}
	if got := len(readNonEmptyLines(t, filepath.Join(home, "registries", "capability-nodes.jsonl"))); got != len(nodeLines) {
		t.Fatalf("re-import appended duplicate node rows: before %d after %d", len(nodeLines), got)
	}
	if got := len(readNonEmptyLines(t, filepath.Join(home, "registries", "capability-edges.jsonl"))); got != len(edgeLines) {
		t.Fatalf("re-import appended duplicate edge rows: before %d after %d", len(edgeLines), got)
	}

	nodeID := "agent-lane:h1--route-one--m1--needs-best"
	nodes, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Node{}
	for _, node := range nodes {
		byID[node.ID] = node
	}
	if byID[nodeID].Kind != "agent-lane" {
		t.Fatalf("missing agent-lane node %s: %+v", nodeID, byID)
	}
	edges, err := store.Edges()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		kind string
		to   string
	}{
		{EdCapableOf, "task-class:tests-from-spec"},
		{EdTendsTo, "tendency:falsifier-first"},
		{EdValidatedBy, "eval:alpha-receipt"},
	} {
		if !hasEdge(edges, want.kind, nodeID, want.to) {
			t.Fatalf("missing edge %s %s -> %s in %+v", want.kind, nodeID, want.to, edges)
		}
	}
}

func TestAgentLaneTupleSlugFaithfulIdentity(t *testing.T) {
	dir := t.TempDir()
	registryPath := filepath.Join(dir, "registry.ts")
	overlayPath := filepath.Join(dir, "overlay.json")
	writeFile(t, registryPath, `export const LANES = [];`)
	writeFile(t, overlayPath, `{
  "roles": [
    {
      "id": "lane-alpha",
      "family": "lane",
      "description": "synthetic lane",
      "capability_class": "build-implement",
      "quality_tier": "the-effort",
      "emit": ["h1"],
      "models": {"h1": "m1"}
    }
  ],
  "lanes": [
    {
      "id": "lane-alpha",
      "harness": "h1",
      "route": "a to a",
      "cost_class": "free"
    }
  ]
}`)
	home, store := newStore(t)
	if _, err := ImportAgentLaneProfiles(home, registryPath, overlayPath, t0); err != nil {
		t.Fatal(err)
	}
	nodes, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	want := "agent-lane:h1--a-to-a--m1--the-effort"
	if len(nodes) != 1 || nodes[0].ID != want {
		t.Fatalf("agent-lane identity = %+v, want %s", nodes, want)
	}
	if strings.Contains(nodes[0].ID, "h1--a--m1") {
		t.Fatalf("agent-lane identity stripped stop words/repeated tokens: %s", nodes[0].ID)
	}
}

func TestClaudeVariantLaneUsesClaudeRolesWithoutCollapsingHarnessIdentity(t *testing.T) {
	dir := t.TempDir()
	registryPath := filepath.Join(dir, "registry.ts")
	overlayPath := filepath.Join(dir, "overlay.json")
	writeFile(t, registryPath, `export const LANES = [];`)
	writeFile(t, overlayPath, `{
  "roles": [{
    "id": "executor",
    "family": "lane",
    "description": "synthetic Claude implementation lane",
    "capability_class": "build-implement",
    "quality_tier": "balanced",
    "emit": ["claude"],
    "models": {"claude": "sonnet"}
  }],
  "lanes": [{
    "id": "claude-zai",
    "harness": "claude-zai",
    "route": "sb run claude --with zai-direct",
    "cost_class": "api-marginal"
  }]
}`)
	home, store := newStore(t)
	if _, err := ImportAgentLaneProfiles(home, registryPath, overlayPath, t0); err != nil {
		t.Fatal(err)
	}
	nodes, err := store.AgentLaneNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("claude variant import nodes = %+v, want one lane from the shared Claude role", nodes)
	}
	if nodes[0].Harness != "claude-zai" || !strings.HasPrefix(nodes[0].ID, "agent-lane:claude-zai--") {
		t.Fatalf("claude variant identity collapsed into native Claude: %+v", nodes[0])
	}
}

func TestImportAgentLaneProfilesAbsentSeedNoop(t *testing.T) {
	home, _ := newStore(t)
	report, err := ImportAgentLaneProfiles(home, filepath.Join(t.TempDir(), "registry.ts"), filepath.Join(t.TempDir(), "overlay.json"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "skipped" || report.AppliedNodes != 0 || report.AppliedEdges != 0 {
		t.Fatalf("absent seed report = %+v, want skipped zero-change", report)
	}
	nodePath := filepath.Join(home, "registries", "capability-nodes.jsonl")
	if got := readOptionalNonEmptyLines(t, nodePath); len(got) != 0 {
		t.Fatalf("absent seed wrote nodes: %v", got)
	}
	edgePath := filepath.Join(home, "registries", "capability-edges.jsonl")
	if got := readOptionalNonEmptyLines(t, edgePath); len(got) != 0 {
		t.Fatalf("absent seed wrote edges: %v", got)
	}
}

func TestImportAgentLaneProfilesRejectsUnknownTendency(t *testing.T) {
	dir := t.TempDir()
	registryPath := filepath.Join(dir, "registry.ts")
	overlayPath := filepath.Join(dir, "overlay.json")
	writeFile(t, registryPath, `export const LANES = [];`)
	writeFile(t, overlayPath, `{
  "roles": [
    {
      "id": "lane-alpha",
      "family": "lane",
      "description": "synthetic lane",
      "capability_class": "build-implement",
      "quality_tier": "balanced",
      "emit": ["h1"],
      "models": {"h1": "m1"},
      "tendencies": ["unknown-tendency"]
    }
  ],
  "lanes": [
    {
      "id": "lane-alpha",
      "harness": "h1",
      "route": "route one",
      "cost_class": "free"
    }
  ]
}`)
	home, _ := newStore(t)
	_, err := ImportAgentLaneProfiles(home, registryPath, overlayPath, t0)
	if err == nil || !strings.Contains(err.Error(), "unknown tendency") {
		t.Fatalf("err = %v, want unknown tendency rejection", err)
	}
}

func readOptionalNonEmptyLines(t *testing.T, path string) []string {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return readNonEmptyLines(t, path)
}
