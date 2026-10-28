package capgraph

import (
	"path/filepath"
	"testing"
)

func TestLoadOSPluginBoardImportsNonSurfacingAdapterNodes(t *testing.T) {
	manifest, err := LoadOSPluginBoard(filepath.Join("..", "..", "fixtures", "skills", "os-plugin-board.json"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.AdapterID != "os.plugin-serving-board-json" || manifest.SourceSystem != "plugin-serving" {
		t.Fatalf("unexpected manifest identity: %+v", manifest)
	}
	if len(manifest.Skills) != 3 {
		t.Fatalf("skills = %d, want 3", len(manifest.Skills))
	}
	byID := map[string]SkillManifestEntry{}
	for _, skill := range manifest.Skills {
		byID[skill.ID] = skill
		if skill.Kind != "adapter" || skill.Status != "needs_review" || !skill.Retired {
			t.Fatalf("plugin entry must be a retired adapter node: %+v", skill)
		}
	}
	browser := byID["adapter:plugin:browser-openai-bundled"]
	if browser.ID == "" {
		t.Fatalf("missing browser plugin adapter: %+v", byID)
	}
	if len(browser.CompatibleHarnesses) != 1 || browser.CompatibleHarnesses[0] != "codex" {
		t.Fatalf("browser harnesses = %+v, want codex only", browser.CompatibleHarnesses)
	}
	off := byID["adapter:plugin:spreadsheets-office-marketplace"]
	if off.ID == "" {
		t.Fatalf("missing disabled plugin adapter: %+v", byID)
	}
	if len(off.CompatibleHarnesses) != 0 {
		t.Fatalf("disabled plugin should not expose harness edges: %+v", off.CompatibleHarnesses)
	}
}

func TestApplyOSPluginBoardDoesNotSurfacePlugins(t *testing.T) {
	home, store := newStore(t)
	manifest, err := LoadOSPluginBoard(filepath.Join("..", "..", "fixtures", "skills", "os-plugin-board.json"), t0)
	if err != nil {
		t.Fatal(err)
	}
	report, err := ApplySkillManifest(home, manifest, t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || len(report.NodeIDs) != 3 {
		t.Fatalf("report = %+v", report)
	}
	nodes, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Node{}
	for _, node := range nodes {
		byID[node.ID] = node
	}
	if !byID["adapter:plugin:browser-openai-bundled"].Retired {
		t.Fatalf("plugin adapter should be retired/non-surfacing: %+v", byID["adapter:plugin:browser-openai-bundled"])
	}
	edges := readCapEdges(t, home)
	if !hasEdge(edges, EdSurfacesWith, "adapter:plugin:browser-openai-bundled", "harness:codex") {
		t.Fatalf("missing codex harness evidence edge: %+v", edges)
	}
	if hasEdge(edges, EdSurfacesWith, "adapter:plugin:spreadsheets-office-marketplace", "harness:codex") {
		t.Fatalf("disabled plugin should not gain codex harness edge: %+v", edges)
	}
	if _, ok := surfaced(store, "open the browser plugin")["adapter:plugin:browser-openai-bundled"]; ok {
		t.Fatal("plugin inventory adapter must not surface as an executable offer")
	}
}
