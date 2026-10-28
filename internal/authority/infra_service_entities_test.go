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

var infraServiceEntitiesTestNow = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

func writeInfraServicesEntityFixture(t *testing.T, path string) {
	t.Helper()
	body := strings.Join([]string{
		"services:",
		"  example-svc.internal:",
		"    role: example_role_a",
		"    authority: example_authority_a",
		"    status: active",
		"  other-svc.example:",
		"    role: example_role_b",
		"    authority: example_authority_b",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestInfraServiceEntitiesRegisterFromInstalledRegistryAndResolve is F1/F2 at
// the authority-function level: after ImportInfraServicesRegistry installs
// services.yaml, RegisterInfraServiceEntities registers one mem.Entity per
// service that resolves by both its full key (identifier) and its bare alias.
func TestInfraServiceEntitiesRegisterFromInstalledRegistryAndResolve(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "services.yaml")
	writeInfraServicesEntityFixture(t, source)

	if _, err := ImportInfraServicesRegistry(home, source, infraServiceEntitiesTestNow); err != nil {
		t.Fatal(err)
	}
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}

	report, err := RegisterInfraServiceEntities(store, home, infraServiceEntitiesTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.RegisteredSlugs) != 2 || len(report.SkippedSlugs) != 0 {
		t.Fatalf("report = %+v", report)
	}

	byIdentifier, err := store.ResolveEntity("example-svc.internal")
	if err != nil {
		t.Fatal(err)
	}
	if byIdentifier.Status != "resolved" || byIdentifier.Slug != "service:example-svc" || byIdentifier.Via != "identifier" {
		t.Fatalf("identifier resolution = %+v", byIdentifier)
	}

	byAlias, err := store.ResolveEntity("example-svc")
	if err != nil {
		t.Fatal(err)
	}
	if byAlias.Status != "resolved" || byAlias.Slug != "service:example-svc" || byAlias.Via != "alias" {
		t.Fatalf("alias resolution = %+v", byAlias)
	}

	unknown, err := store.ResolveEntity("nosuch.example")
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Status != "unresolved" {
		t.Fatalf("unknown token should stay unresolved, got %+v", unknown)
	}
}

// TestInfraServiceEntitiesRegisterIsIdempotentAcrossReimport is F4: importing
// the same services.yaml twice and re-registering must not append duplicate
// entity rows for the same slug, and resolution must stay identical.
func TestInfraServiceEntitiesRegisterIsIdempotentAcrossReimport(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "services.yaml")
	writeInfraServicesEntityFixture(t, source)

	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if _, err := ImportInfraServicesRegistry(home, source, infraServiceEntitiesTestNow.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := RegisterInfraServiceEntities(store, home, infraServiceEntitiesTestNow.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	lines := readNonEmptyFileLines(t, filepath.Join(home, "ledger", "entities.jsonl"))
	serviceLines := 0
	for _, line := range lines {
		if strings.Contains(line, `"slug":"service:example-svc"`) || strings.Contains(line, `"slug": "service:example-svc"`) {
			serviceLines++
		}
	}
	if serviceLines != 1 {
		t.Fatalf("re-import should not duplicate the entity row, got %d matching lines in %v", serviceLines, lines)
	}

	res, err := store.ResolveEntity("example-svc.internal")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "resolved" || res.Slug != "service:example-svc" {
		t.Fatalf("resolution after re-import = %+v", res)
	}
}

// TestInfraServiceCapabilitiesEnrollMintsServiceNodesAndSurfaces is F6/F7 at
// the authority-function level: enrollment mints a "service" kind node per
// entry, a task with overlapping tokens surfaces it, and a task with no
// overlap surfaces nothing.
func TestInfraServiceCapabilitiesEnrollMintsServiceNodesAndSurfaces(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "services.yaml")
	writeInfraServicesEntityFixture(t, source)

	if _, err := ImportInfraServicesRegistry(home, source, infraServiceEntitiesTestNow); err != nil {
		t.Fatal(err)
	}
	report, err := EnrollInfraServiceCapabilities(home, infraServiceEntitiesTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.NodeIDs) != 2 || report.SourceAuthorityID == "" {
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
		if n.ID == "service:example-svc" {
			found = true
			if n.Kind != "service" {
				t.Fatalf("node kind = %q, want service", n.Kind)
			}
		}
	}
	if !found {
		t.Fatalf("service:example-svc node missing: %+v", nodes)
	}

	hits, err := store.Surface("deploy a service to example-svc.internal")
	if err != nil {
		t.Fatal(err)
	}
	hit := false
	for _, h := range hits {
		if h.Node.ID == "service:example-svc" {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("surface for deploy task did not offer service:example-svc: %+v", hits)
	}

	noHits, err := store.Surface("write a poem about the ocean")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range noHits {
		if h.Node.ID == "service:example-svc" || h.Node.ID == "service:other-svc" {
			t.Fatalf("unrelated task should not surface service nodes, got %+v", noHits)
		}
	}

	// Re-enrolling must not duplicate node rows (capgraph.Store.Add already
	// no-ops an identical replay by ID).
	if _, err := EnrollInfraServiceCapabilities(home, infraServiceEntitiesTestNow); err != nil {
		t.Fatal(err)
	}
	nodeLines := readNonEmptyFileLines(t, filepath.Join(home, "registries", "capability-nodes.jsonl"))
	count := 0
	for _, line := range nodeLines {
		if strings.Contains(line, `"id":"service:example-svc"`) || strings.Contains(line, `"id": "service:example-svc"`) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("re-enroll should not duplicate node rows, got %d in %v", count, nodeLines)
	}
}

// TestInfraServiceProvenanceResolvesRoleAuthorityStatusAndRevision is F1's
// provenance half: role/authority/status/source_revision are readable for a
// resolved service entity, and non-service slugs report found=false with no
// error.
func TestInfraServiceProvenanceResolvesRoleAuthorityStatusAndRevision(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "services.yaml")
	writeInfraServicesEntityFixture(t, source)

	if _, err := ImportInfraServicesRegistry(home, source, infraServiceEntitiesTestNow); err != nil {
		t.Fatal(err)
	}

	prov, found, err := ResolveInfraServiceProvenance(home, "service:example-svc")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected provenance to be found for service:example-svc")
	}
	if prov.Role != "example_role_a" || prov.Authority != "example_authority_a" || prov.Status != "active" || prov.SourceRevision == "" {
		t.Fatalf("provenance = %+v", prov)
	}

	_, found, err = ResolveInfraServiceProvenance(home, "repo:some-repo")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("non-service slug must not resolve provenance")
	}
}

func readNonEmptyFileLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
