package tooltopology

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/capgraph"
)

var tTopo = time.Date(2026, 6, 29, 13, 0, 0, 0, time.UTC)

func TestApplyImportsToolAndMCPNodesWithFallbackRecommendation(t *testing.T) {
	home := t.TempDir()
	topo := validTopology()

	report, err := Apply(home, topo, tTopo)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || len(report.NodeIDs) != 2 || len(report.EdgeIDs) != 1 {
		t.Fatalf("report = %+v", report)
	}

	store, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]capgraph.Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	if byID["tool:browser-cli"].Kind != "tool" || byID["mcp:browser"].Kind != "mcp" {
		t.Fatalf("nodes = %+v", byID)
	}

	hits, err := store.Surface("browser screenshots")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Node.ID != "tool:browser-cli" {
		t.Fatalf("surface hits = %+v", hits)
	}

	edges := readEdges(t, home)
	if len(edges) != 1 || edges[0].Kind != capgraph.EdFallbackFor || edges[0].From != "mcp:browser" || edges[0].To != "tool:browser-cli" {
		t.Fatalf("edges = %+v", edges)
	}
}

func TestValidateRejectsUnknownNodeKind(t *testing.T) {
	topo := validTopology()
	topo.Entries[0].Kind = "plugin"
	report := Validate(topo)
	if report.Status != "fail" {
		t.Fatalf("status = %s, want fail", report.Status)
	}
}

func TestLoadOSResolverAdaptsGeneratedToolResolver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool-resolver.json")
	writeFile(t, path, `{
  "marker": "# @generated",
  "generated_from": ["registry/tool-topology.json"],
  "tools": [
    {
      "id": "mcporter",
      "title": "mcporter gateway/catalog",
      "topology": "gateway_core",
      "functions": ["tool.catalog", "tool.invoke"],
      "effect_class": ["read", "execute"],
      "preferred_model_tier": "deterministic",
      "host_availability": {"codex": "cli", "bridge": false},
      "actual_surfaces": ["tool-doctor"]
    },
    {
      "id": "context7",
      "title": "current library/framework/API documentation",
      "topology": "mcporter_catalog_on_demand",
      "mcporter_server": "context7",
      "functions": ["docs.lookup", "library.reference"],
      "effect_class": ["read"],
      "preferred_model_tier": "cheap_ok",
      "host_availability": {"codex": "on_demand", "bridge": false}
    },
    {
      "id": "ios_simulator",
      "title": "iOS simulator MCP",
      "topology": "mcporter_catalog_on_demand",
      "functions": ["ios.run", "ios.ui_automate", "screenshot.capture"],
      "effect_class": ["read", "execute"],
      "host_availability": {"codex": "on_demand"}
    },
    {
      "id": "xcodebuildmcp",
      "title": "xcodebuildmcp fallback",
      "topology": "legacy_or_last_resort",
      "functions": ["ios.build", "ios.debug"],
      "replacement": "ios_simulator",
      "host_availability": {"codex": "none"}
    }
  ]
}`)
	topo, err := LoadOSResolver(path, tTopo)
	if err != nil {
		t.Fatal(err)
	}
	if topo.SchemaVersion != SchemaVersion || topo.AdapterID != "os.tool-resolver-json" || len(topo.EvidenceRefs) != 1 {
		t.Fatalf("topology = %+v", topo)
	}
	if len(topo.Entries) != 4 || len(topo.Recommendations) != 1 {
		t.Fatalf("entries=%d recommendations=%d", len(topo.Entries), len(topo.Recommendations))
	}
	byID := map[string]Entry{}
	for _, entry := range topo.Entries {
		byID[entry.ID] = entry
	}
	if byID["adapter:mcporter"].Kind != "adapter" ||
		byID["mcp:context7"].Kind != "mcp" ||
		byID["tool:xcodebuildmcp"].Status != "fallback" {
		t.Fatalf("entries = %+v", byID)
	}
	rec := topo.Recommendations[0]
	if rec.Primary != "mcp:ios-simulator" || rec.Fallback != "tool:xcodebuildmcp" {
		t.Fatalf("recommendation = %+v", rec)
	}
	home := t.TempDir()
	report, err := Apply(home, topo, tTopo)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || len(report.NodeIDs) != 4 || len(report.EdgeIDs) != 1 {
		t.Fatalf("report = %+v", report)
	}
	store, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := store.Surface("docs lookup")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Node.ID != "mcp:context7" {
		t.Fatalf("docs lookup hits = %+v", hits)
	}
}

func TestLoadMCPorterCatalogMapsServersAndToolsWithoutExecuting(t *testing.T) {
	topo, err := LoadMCPorterCatalog(filepath.Join("..", "..", "fixtures", "tooltopology", "mcporter-list.json"), tTopo)
	if err != nil {
		t.Fatal(err)
	}
	if topo.AdapterID != "mcporter.list-json" || topo.SourceSystem != "mcporter" || topo.SourceRef.SourceClass != "mcporter-json" {
		t.Fatalf("topology source = %+v", topo)
	}
	byID := map[string]Entry{}
	for _, entry := range topo.Entries {
		byID[entry.ID] = entry
	}
	for _, id := range []string{
		"mcp:context7",
		"tool:context7.get-library-docs",
		"tool:context7.resolve-library-id",
		"mcp:cloudflare-api",
	} {
		if byID[id].ID == "" {
			t.Fatalf("missing entry %s in %+v", id, byID)
		}
	}
	// An auth-blocked server must not surface as AVAILABLE. That guarantee is
	// unchanged; what changed is how it is enforced.
	//
	// It used to be enforced by retiring the node, which also deleted it from the
	// graph's offers entirely -- so "needs auth" and "does not exist" became the
	// same state, and a server that was merely down at probe time disappeared the
	// same way. Availability is now carried by Health plus the degraded weight,
	// and the node keeps existing.
	//
	// The assertions below are deliberately STRONGER than the Retired check they
	// replace: Retired was an internal flag, while a consumer's actual exposure is
	// the offer it receives. So this pins the consumer-visible surface -- the
	// capability still reaches a session, and it arrives carrying a required
	// health constraint that makes it unmistakable for a working one.
	blocked := byID["mcp:cloudflare-api"]
	if blocked.Retired {
		t.Fatalf("auth-blocked server was RETIRED; needing auth is not the same as not existing: %+v", blocked)
	}
	if blocked.Status != "auth_required" || blocked.Health != "auth_required" {
		t.Fatalf("auth-blocked server did not carry its condition: %+v", blocked)
	}
	if blocked.Weight >= 1.0 {
		t.Fatalf("auth-blocked server kept a full weight, so it ranks like a working one: %+v", blocked)
	}
	if byID["tool:context7.get-library-docs"].Interface != "mcp_tool" ||
		!containsString(byID["tool:context7.get-library-docs"].Capabilities, "documentation") {
		t.Fatalf("tool entry did not preserve tool capability: %+v", byID["tool:context7.get-library-docs"])
	}

	home := t.TempDir()
	report, err := Apply(home, topo, tTopo)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || len(report.NodeIDs) != 4 {
		t.Fatalf("report = %+v", report)
	}
	store, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := store.Surface("current library docs")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Node.ID != "tool:context7.get-library-docs" {
		t.Fatalf("library docs surface hits = %+v", hits)
	}
	authHits, err := store.Surface("cloudflare api")
	if err != nil {
		t.Fatal(err)
	}
	if len(authHits) != 0 {
		t.Fatalf("auth-blocked server should stay out of offers: %+v", authHits)
	}
}

func TestDoctorBoundaryReportsNonExecutingMCPAndPluginInventory(t *testing.T) {
	home := t.TempDir()
	store, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range []capgraph.Node{
		{ID: "mcp:context7", Kind: "mcp", Name: "context7", Triggers: []string{"docs"}, Weight: 1},
		{ID: "tool:agent-browser", Kind: "tool", Name: "agent-browser", Triggers: []string{"browser"}, Weight: 1},
		{ID: "adapter:plugin:browser-openai-bundled", Kind: "adapter", Name: "browser plugin adapter", Triggers: []string{"plugin", "browser"}, Weight: 1, Retired: true},
	} {
		if err := store.Add(node); err != nil {
			t.Fatal(err)
		}
	}
	report, err := DoctorBoundary(home)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || report.MCPCount != 1 || report.ToolCount != 1 || report.PluginAdapterCount != 1 || report.PluginSurfaceCount != 0 {
		t.Fatalf("report = %+v", report)
	}
	if !hasBoundaryCheck(report, "plugin_adapters_not_surfacing", "ok") {
		t.Fatalf("missing non-surfacing check: %+v", report.Checks)
	}
}

func TestDoctorBoundaryWarnsOnStaleCapabilitySourceRecords(t *testing.T) {
	home := t.TempDir()
	store, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	addBoundaryDoctorNodes(t, store)
	staleImport := time.Now().UTC().Add(-15 * 24 * time.Hour)
	if _, err := capgraph.RecordSourceAuthority(home, capgraph.SourceAuthorityInput{
		SourcePath:     "fixtures/imports/mcporter-list-20260701T000000Z.json",
		SourceKind:     "catalog",
		SourceOwner:    "tool-topology",
		SourceClass:    "mcporter-json",
		SourceRevision: "sha256:stale",
		ImportedVia:    "compound capability import-topology",
		NodeIDs:        []string{"mcp:context7", "tool:agent-browser"},
		EvidenceRefs:   []string{"mcporter_json:stale"},
	}, staleImport); err != nil {
		t.Fatal(err)
	}

	report, err := DoctorBoundary(home)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "warn" {
		t.Fatalf("status = %s, want warn: %+v", report.Status, report.Checks)
	}
	if !hasBoundaryCheck(report, "source_freshness", "warn") {
		t.Fatalf("missing stale source_freshness warning: %+v", report.Checks)
	}
	if detail := boundaryCheckDetail(report, "source_freshness"); !strings.Contains(detail, "mcporter list --json") || !strings.Contains(detail, "compound capability import-topology") {
		t.Fatalf("freshness detail should include exact refresh command, got %q", detail)
	}
}

func TestDoctorBoundaryAcceptsFreshCapabilitySourceRecords(t *testing.T) {
	home := t.TempDir()
	store, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	addBoundaryDoctorNodes(t, store)
	if _, err := capgraph.RecordSourceAuthority(home, capgraph.SourceAuthorityInput{
		SourcePath:     "fixtures/imports/mcporter-list-20260704T000000Z.json",
		SourceKind:     "catalog",
		SourceOwner:    "tool-topology",
		SourceClass:    "mcporter-json",
		SourceRevision: "sha256:fresh",
		ImportedVia:    "compound capability import-topology",
		NodeIDs:        []string{"mcp:context7", "tool:agent-browser"},
		EvidenceRefs:   []string{"mcporter_json:fresh"},
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	report, err := DoctorBoundary(home)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" {
		t.Fatalf("status = %s, want ok: %+v", report.Status, report.Checks)
	}
	if !hasBoundaryCheck(report, "source_freshness", "ok") {
		t.Fatalf("missing fresh source_freshness check: %+v", report.Checks)
	}
}

func TestDoctorBoundaryFailsWhenPluginAdapterSurfaces(t *testing.T) {
	home := t.TempDir()
	store, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range []capgraph.Node{
		{ID: "mcp:context7", Kind: "mcp", Name: "context7", Triggers: []string{"docs"}, Weight: 1},
		{ID: "tool:agent-browser", Kind: "tool", Name: "agent-browser", Triggers: []string{"browser"}, Weight: 1},
		{ID: "adapter:plugin:unsafe", Kind: "adapter", Name: "unsafe plugin adapter", Triggers: []string{"hook adapter"}, Weight: 1},
	} {
		if err := store.Add(node); err != nil {
			t.Fatal(err)
		}
	}
	report, err := DoctorBoundary(home)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "fail" || report.PluginSurfaceCount == 0 {
		t.Fatalf("report should fail on active plugin adapter surfacing: %+v", report)
	}
	if !hasBoundaryCheck(report, "plugin_adapters_retired", "fail") || !hasBoundaryCheck(report, "plugin_adapters_not_surfacing", "fail") {
		t.Fatalf("missing failed plugin checks: %+v", report.Checks)
	}
}

func hasBoundaryCheck(report BoundaryDoctorReport, name, status string) bool {
	for _, check := range report.Checks {
		if check.Name == name && check.Status == status {
			return true
		}
	}
	return false
}

func boundaryCheckDetail(report BoundaryDoctorReport, name string) string {
	for _, check := range report.Checks {
		if check.Name == name {
			return check.Detail
		}
	}
	return ""
}

func addBoundaryDoctorNodes(t *testing.T, store *capgraph.Store) {
	t.Helper()
	for _, node := range []capgraph.Node{
		{ID: "mcp:context7", Kind: "mcp", Name: "context7", Triggers: []string{"docs"}, Weight: 1},
		{ID: "tool:agent-browser", Kind: "tool", Name: "agent-browser", Triggers: []string{"browser"}, Weight: 1},
		{ID: "adapter:plugin:browser-openai-bundled", Kind: "adapter", Name: "browser plugin adapter", Triggers: []string{"plugin", "browser"}, Weight: 1, Retired: true},
	} {
		if err := store.Add(node); err != nil {
			t.Fatal(err)
		}
	}
}

func validTopology() Topology {
	return Topology{
		SchemaVersion: SchemaVersion,
		ID:            "topo_fixture_browser",
		AdapterID:     "fixture.tool-topology",
		SourceSystem:  "fixture-os",
		ObservedAt:    "2026-06-29T00:00:00Z",
		SourceRef: SourceRef{
			Path:        "fixtures/generated/tool-resolver.md",
			Kind:        "generated_view",
			Owner:       "fixture-spine",
			SourceClass: "generated",
			Revision:    "sha256:def456",
		},
		Entries: []Entry{
			{
				ID:           "tool:browser-cli",
				Kind:         "tool",
				Name:         "Browser CLI",
				Interface:    "cli",
				Status:       "active",
				Triggers:     []string{"browser", "screenshots"},
				Capabilities: []string{"web automation"},
				EvidenceRefs: []string{"artifact_sha256:def456"},
				Weight:       2,
			},
			{
				ID:           "mcp:browser",
				Kind:         "mcp",
				Name:         "Browser MCP",
				Interface:    "mcp",
				Status:       "fallback",
				Triggers:     []string{"browser"},
				Capabilities: []string{"web automation"},
				EvidenceRefs: []string{"artifact_sha256:def456"},
			},
		},
		Recommendations: []Recommendation{
			{Primary: "tool:browser-cli", Fallback: "mcp:browser", Reason: "prefer CLI for low-context browser work", Confidence: 0.9},
		},
		EvidenceRefs: []string{"artifact_sha256:def456"},
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readEdges(t *testing.T, home string) []capgraph.Edge {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "registries", "capability-edges.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var edges []capgraph.Edge
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var edge capgraph.Edge
		if err := json.Unmarshal([]byte(line), &edge); err != nil {
			t.Fatalf("parse edge %q: %v", line, err)
		}
		edges = append(edges, edge)
	}
	return edges
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
