package authority

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/capgraph"
	"compound/internal/mem"
)

var modelCatalogEntitiesTestNow = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

// writeModelCatalogFixture writes a small synthetic provider+model catalog:
// 4 model rows across 2 providers (examplecorp, othercorp) — one shared
// alias ("shared-model" registered under both providers) and one deprecated
// row ("legacy-model", retired for examplecorp). Synthetic names only, per
// the repo's public/private boundary — never the real provider registry.
func writeModelCatalogFixture(t *testing.T, path string) {
	t.Helper()
	body := `{
  "schema": "examplecorp/provider-registry@3",
  "models": [
    {
      "provider_id": "examplecorp",
      "model_id": "alpha-flash",
      "display_name": "Alpha Flash",
      "upstream_model_ids": ["alpha-flash-001"],
      "input_micros_per_mtok": 140000,
      "output_micros_per_mtok": 560000,
      "context_window": 128000,
      "tier": "standard",
      "source_url": "https://catalog.invalid/alpha-flash",
      "workload_kind": "chat"
    },
    {
      "provider_id": "examplecorp",
      "model_id": "shared-model",
      "display_name": "Shared Model",
      "upstream_model_ids": [],
      "input_micros_per_mtok": 100000,
      "output_micros_per_mtok": 300000,
      "context_window": 32000,
      "tier": "value",
      "source_url": "",
      "workload_kind": "chat"
    },
    {
      "provider_id": "othercorp",
      "model_id": "shared-model",
      "display_name": "Shared Model Prime",
      "upstream_model_ids": [],
      "input_micros_per_mtok": 90000,
      "output_micros_per_mtok": 270000,
      "context_window": 64000,
      "tier": "value",
      "source_url": "https://catalog.invalid/shared-model-prime",
      "workload_kind": "chat"
    },
    {
      "provider_id": "examplecorp",
      "model_id": "legacy-model",
      "display_name": "Legacy Model",
      "upstream_model_ids": [],
      "input_micros_per_mtok": 50000,
      "output_micros_per_mtok": 150000,
      "context_window": 8000,
      "tier": "legacy",
      "source_url": "",
      "workload_kind": "chat"
    }
  ],
  "deprecated": [
    {
      "model": "legacy-model",
      "providers": ["examplecorp"],
      "status": "deprecated",
      "note": "superseded by alpha-flash"
    }
  ]
}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestModelCatalogImportInstallsRegistryAndCountsDeprecated is F1 (install
// half) + F3 (deprecated count in the report): ImportModelCatalog installs a
// verbatim copy, cites the EXISTING model-router authority ref, and counts
// the one deprecated row without enrolling it.
func TestModelCatalogImportInstallsRegistryAndCountsDeprecated(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "provider-registry.json")
	writeModelCatalogFixture(t, source)

	report, err := ImportModelCatalog(home, source, modelCatalogEntitiesTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if report.AuthorityRef != ModelCatalogAuthorityRef {
		t.Fatalf("authority_ref = %q, want %q", report.AuthorityRef, ModelCatalogAuthorityRef)
	}
	if report.ModelCount != 4 || report.ProviderCount != 2 || report.DeprecatedCount != 1 {
		t.Fatalf("report = %+v", report)
	}
	if _, err := os.Stat(ModelCatalogRegistryPath(home)); err != nil {
		t.Fatalf("installed catalog missing: %v", err)
	}
	records, err := LoadModelCatalogSourceAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].AuthorityRef != ModelCatalogAuthorityRef {
		t.Fatalf("source authority records = %+v", records)
	}
}

// TestModelCatalogEntitiesRegisterAndResolveWithSharedAlias is F1 (entity
// half) + F2: each enrollable row registers as a resolvable model:* entity;
// a unique alias resolves cleanly, and the alias shared across two providers
// resolves to BOTH — status "multi" — which is correct, not a bug.
func TestModelCatalogEntitiesRegisterAndResolveWithSharedAlias(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "provider-registry.json")
	writeModelCatalogFixture(t, source)

	if _, err := ImportModelCatalog(home, source, modelCatalogEntitiesTestNow); err != nil {
		t.Fatal(err)
	}
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}

	report, err := RegisterModelCatalogEntities(store, home, modelCatalogEntitiesTestNow)
	if err != nil {
		t.Fatal(err)
	}
	// 3 enrollable rows (alpha-flash, examplecorp/shared-model,
	// othercorp/shared-model); legacy-model is deprecated and skipped.
	if len(report.RegisteredSlugs) != 3 || len(report.SkippedSlugs) != 0 {
		t.Fatalf("report = %+v", report)
	}

	bySlug, err := store.ResolveEntity("model:examplecorp/alpha-flash")
	if err != nil {
		t.Fatal(err)
	}
	if bySlug.Status != "resolved" || bySlug.Slug != "model:examplecorp/alpha-flash" || bySlug.Via != "slug" {
		t.Fatalf("slug resolution = %+v", bySlug)
	}

	byUniqueAlias, err := store.ResolveEntity("alpha-flash")
	if err != nil {
		t.Fatal(err)
	}
	if byUniqueAlias.Status != "resolved" || byUniqueAlias.Slug != "model:examplecorp/alpha-flash" || byUniqueAlias.Via != "alias" {
		t.Fatalf("unique alias resolution = %+v", byUniqueAlias)
	}

	bySharedAlias, err := store.ResolveEntity("shared-model")
	if err != nil {
		t.Fatal(err)
	}
	if bySharedAlias.Status != "multi" {
		t.Fatalf("shared alias should resolve multi, got %+v", bySharedAlias)
	}
	for _, want := range []string{"model:examplecorp/shared-model", "model:othercorp/shared-model"} {
		if !strings.Contains(bySharedAlias.Slug, want) {
			t.Fatalf("multi resolution %+v missing %q", bySharedAlias, want)
		}
	}

	deprecated, err := store.ResolveEntity("model:examplecorp/legacy-model")
	if err != nil {
		t.Fatal(err)
	}
	if deprecated.Status != "unresolved" {
		t.Fatalf("deprecated row must not register, got %+v", deprecated)
	}
}

// TestModelCatalogEntitiesReimportIsIdempotent is F4: importing the same
// catalog twice and re-registering must not append duplicate entity rows for
// the same slug, and resolution must stay identical.
func TestModelCatalogEntitiesReimportIsIdempotent(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "provider-registry.json")
	writeModelCatalogFixture(t, source)

	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		now := modelCatalogEntitiesTestNow.Add(time.Duration(i) * time.Minute)
		if _, err := ImportModelCatalog(home, source, now); err != nil {
			t.Fatal(err)
		}
		if _, err := RegisterModelCatalogEntities(store, home, now); err != nil {
			t.Fatal(err)
		}
	}

	lines := readNonEmptyFileLines(t, filepath.Join(home, "ledger", "entities.jsonl"))
	count := 0
	for _, line := range lines {
		if strings.Contains(line, `"slug":"model:examplecorp/alpha-flash"`) || strings.Contains(line, `"slug": "model:examplecorp/alpha-flash"`) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("re-import should not duplicate the entity row, got %d matching lines in %v", count, lines)
	}

	res, err := store.ResolveEntity("model:examplecorp/alpha-flash")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "resolved" || res.Slug != "model:examplecorp/alpha-flash" {
		t.Fatalf("resolution after re-import = %+v", res)
	}
}

// TestModelCatalogCapabilitiesEnrollMintsModelProfileNodesAndSurfaces is F6:
// enrollment mints a "model_profile" kind node per enrollable row, a task
// with overlapping tokens surfaces it, and an unrelated task surfaces none
// of the fixture nodes. Also proves the deprecated row is never minted.
func TestModelCatalogCapabilitiesEnrollMintsModelProfileNodesAndSurfaces(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "provider-registry.json")
	writeModelCatalogFixture(t, source)

	if _, err := ImportModelCatalog(home, source, modelCatalogEntitiesTestNow); err != nil {
		t.Fatal(err)
	}
	report, err := EnrollModelCatalogCapabilities(home, modelCatalogEntitiesTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.NodeIDs) != 3 || report.SourceAuthorityID == "" {
		t.Fatalf("capability report = %+v", report)
	}

	store, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range nodes {
		if n.ID == "model:examplecorp/legacy-model" {
			t.Fatalf("deprecated row must not mint a capability node: %+v", n)
		}
		if n.ID == "model:examplecorp/alpha-flash" {
			found = true
			if n.Kind != "model_profile" {
				t.Fatalf("node kind = %q, want model_profile", n.Kind)
			}
		}
	}
	if !found {
		t.Fatalf("model:examplecorp/alpha-flash node missing: %+v", nodes)
	}

	hits, err := store.Surface("what does alpha-flash cost")
	if err != nil {
		t.Fatal(err)
	}
	hit := false
	for _, h := range hits {
		if h.Node.ID == "model:examplecorp/alpha-flash" {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("surface for alpha-flash task did not offer model:examplecorp/alpha-flash: %+v", hits)
	}

	noHits, err := store.Surface("write a poem about the ocean")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range noHits {
		if h.Node.ID == "model:examplecorp/alpha-flash" || h.Node.ID == "model:examplecorp/shared-model" || h.Node.ID == "model:othercorp/shared-model" {
			t.Fatalf("unrelated task should not surface fixture model nodes, got %+v", noHits)
		}
	}

	// Re-enrolling must not duplicate node rows.
	if _, err := EnrollModelCatalogCapabilities(home, modelCatalogEntitiesTestNow); err != nil {
		t.Fatal(err)
	}
	nodeLines := readNonEmptyFileLines(t, filepath.Join(home, "registries", "capability-nodes.jsonl"))
	nodeCount := 0
	for _, line := range nodeLines {
		if strings.Contains(line, `"id":"model:examplecorp/alpha-flash"`) || strings.Contains(line, `"id": "model:examplecorp/alpha-flash"`) {
			nodeCount++
		}
	}
	if nodeCount != 1 {
		t.Fatalf("re-enroll should not duplicate node rows, got %d in %v", nodeCount, nodeLines)
	}
}

// TestModelCatalogProvenanceConvertsMicrosToUSDExactly is F7: micros-per-Mtok
// prices convert to an exact decimal USD string (140000 -> "0.14") with no
// float drift, and the rest of the provenance block (provider, context
// window, tier, source_revision) resolves for a real row while a non-model
// slug reports found=false.
func TestModelCatalogProvenanceConvertsMicrosToUSDExactly(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "provider-registry.json")
	writeModelCatalogFixture(t, source)

	if _, err := ImportModelCatalog(home, source, modelCatalogEntitiesTestNow); err != nil {
		t.Fatal(err)
	}

	prov, found, err := ResolveModelCatalogProvenance(home, "model:examplecorp/alpha-flash")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected provenance to be found for model:examplecorp/alpha-flash")
	}
	if prov.Provider != "examplecorp" {
		t.Fatalf("provider = %q", prov.Provider)
	}
	if prov.PriceInUSDPerMtok != "0.14" {
		t.Fatalf("price_in_usd_per_mtok = %q, want 0.14 (input_micros_per_mtok=140000)", prov.PriceInUSDPerMtok)
	}
	if prov.PriceOutUSDPerMtok != "0.56" {
		t.Fatalf("price_out_usd_per_mtok = %q, want 0.56 (output_micros_per_mtok=560000)", prov.PriceOutUSDPerMtok)
	}
	if prov.ContextWindow != 128000 || prov.Tier != "standard" || prov.SourceRevision == "" {
		t.Fatalf("provenance = %+v", prov)
	}

	_, found, err = ResolveModelCatalogProvenance(home, "repo:some-repo")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("non-model slug must not resolve provenance")
	}
}

func TestFormatMicrosPerMtokUSDExactStrings(t *testing.T) {
	cases := []struct {
		micros int64
		want   string
	}{
		{140000, "0.14"},
		{560000, "0.56"},
		{1000000, "1"},
		{0, "0"},
		{1500000, "1.5"},
		{1, "0.000001"},
	}
	for _, c := range cases {
		if got := formatMicrosPerMtokUSD(c.micros); got != c.want {
			t.Fatalf("formatMicrosPerMtokUSD(%d) = %q, want %q", c.micros, got, c.want)
		}
	}
}
