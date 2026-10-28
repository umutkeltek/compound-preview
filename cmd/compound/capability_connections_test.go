package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/contract"
	"compound/internal/envelope"
	"compound/internal/harness"
	"compound/internal/home"
	"compound/internal/routeevidence"
)

// writeConnectionsPermissionFixture writes one harness-permission authority plus
// a live target settings file into homePath and returns the target path. It
// builds a synthetic home with a fixture authority (the pattern
// writeDoctorPermissionPostureFixture uses) so nothing operator-specific is
// tracked -- every path is read from the home under test.
func writeConnectionsPermissionFixture(t *testing.T, homePath, harnessName string, allow ...string) string {
	t.Helper()
	if len(allow) == 0 {
		allow = []string{"Read", "mcp__gbrain__*"}
	}
	targetPath := filepath.Join(t.TempDir(), harnessName+"-settings.json")
	writeHarnessDriftObserveJSON(t, targetPath, map[string]any{"theme": "dark"})
	policy := harness.PermissionAuthority{
		SchemaVersion: harness.PermissionAuthoritySchemaVersion,
		Harness:       harnessName,
		AuthorityRef:  harness.PermissionAuthorityRef(harnessName),
		TargetPath:    targetPath,
		Permissions: harness.PermissionRules{
			Allow: allow, Deny: []string{"Bash(rm:*)"}, Ask: []string{"WebFetch"}, DefaultMode: "default",
		},
		AutoMode: harness.AutoModePolicy{Allow: []string{"$defaults", "Read"}},
	}
	writeHarnessDriftObserveJSON(t, harness.PermissionAuthorityPath(homePath, harnessName), policy)
	return targetPath
}

func connectionSurfaceLineCount(t *testing.T, homePath string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(homePath, "registries", "capability-connection-surfaces.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

func findNodeByID(t *testing.T, store *capgraph.Store, id string) (capgraph.Node, bool) {
	t.Helper()
	nodes, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.ID == id {
			return n, true
		}
	}
	return capgraph.Node{}, false
}

func findEnvelopeCapabilityOffer(offers []envelope.CapabilityOffer, ref string) *envelope.CapabilityOffer {
	for i := range offers {
		if offers[i].Ref == ref {
			return &offers[i]
		}
	}
	return nil
}

func offerConstraintHasRef(values []envelope.CapabilityOfferConstraint, kind, ref string) bool {
	for _, c := range values {
		if c.Kind == kind && c.Ref == ref {
			return true
		}
	}
	return false
}

// F1: import mints exactly one ConnectionSurface (config paths include the
// target path; asserting owner + source revision set); a second run adds
// nothing; changing the authority content (new revision) updates the record.
func TestCapabilityImportConnections_F1_MintsIdempotentUpdatable(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	targetPath := writeConnectionsPermissionFixture(t, homePath, "claude_code")

	out := captureStdout(t, func() {
		cmdCapability([]string{"import-connections"})
	})
	for _, want := range []string{
		"# capability import-connections ok",
		"- harness: claude_code node=claude_code",
		"- source_observation: srcobs_conn_",
		"- source_no_promote:",
		"- source_authority: capsrc_",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("import-connections output missing %q:\n%s", want, out)
		}
	}

	store, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	surfaces, err := store.ConnectionSurfaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(surfaces) != 1 {
		t.Fatalf("want exactly 1 connection surface, got %d: %+v", len(surfaces), surfaces)
	}
	cs := surfaces[0]
	if cs.NodeID != "claude_code" {
		t.Fatalf("connection surface node id = %q, want claude_code", cs.NodeID)
	}
	if !stringSliceContains(cs.ConfigPaths, targetPath) {
		t.Fatalf("config paths %+v missing target path %q", cs.ConfigPaths, targetPath)
	}
	if cs.AssertingOwner != harness.PermissionAuthorityRef("claude_code") {
		t.Fatalf("asserting owner = %q, want %q", cs.AssertingOwner, harness.PermissionAuthorityRef("claude_code"))
	}
	if !strings.HasPrefix(cs.SourceRevision, "sha256:") {
		t.Fatalf("source revision = %q, want sha256: prefix", cs.SourceRevision)
	}
	firstRevision := cs.SourceRevision
	if lines := connectionSurfaceLineCount(t, homePath); lines != 1 {
		t.Fatalf("sidecar line count = %d after first run, want 1", lines)
	}

	// Second run with unchanged sources is a no-op (no new record line).
	captureStdout(t, func() {
		cmdCapability([]string{"import-connections"})
	})
	if lines := connectionSurfaceLineCount(t, homePath); lines != 1 {
		t.Fatalf("sidecar line count = %d after idempotent re-run, want 1 (no append)", lines)
	}
	surfaces2, err := store.ConnectionSurfaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(surfaces2) != 1 {
		t.Fatalf("re-run changed logical record count: %d", len(surfaces2))
	}

	// Changing the authority content bumps its revision -> the record updates
	// (last-wins by node id; still one logical record).
	writeConnectionsPermissionFixture(t, homePath, "claude_code", "Read", "mcp__gbrain__*", "Write")
	captureStdout(t, func() {
		cmdCapability([]string{"import-connections"})
	})
	surfaces3, err := store.ConnectionSurfaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(surfaces3) != 1 {
		t.Fatalf("update should keep exactly one logical record, got %d", len(surfaces3))
	}
	if surfaces3[0].SourceRevision == firstRevision {
		t.Fatalf("revision did not update after authority content changed: %q", surfaces3[0].SourceRevision)
	}
}

// F2: no harness node -> door mints kind:harness node; a pre-existing node is
// left untouched (DeepEqual proves no rewrite).
func TestCapabilityImportConnections_F2_EnsuresHarnessNodeWithoutRewrite(t *testing.T) {
	t.Run("mints when absent", func(t *testing.T) {
		homePath := t.TempDir()
		t.Setenv("COMPOUND_HOME", homePath)
		if err := home.Init(homePath, "test", cliTestNow); err != nil {
			t.Fatal(err)
		}
		writeConnectionsPermissionFixture(t, homePath, "claude_code")
		out := captureStdout(t, func() {
			cmdCapability([]string{"import-connections"})
		})
		if !strings.Contains(out, "node=claude_code minted=true") {
			t.Fatalf("expected minted harness node, output:\n%s", out)
		}
		store, err := capgraph.Open(homePath)
		if err != nil {
			t.Fatal(err)
		}
		node, ok := findNodeByID(t, store, "claude_code")
		if !ok {
			t.Fatalf("harness node not minted")
		}
		if node.Kind != "harness" {
			t.Fatalf("minted node kind = %q, want harness", node.Kind)
		}
	})

	t.Run("leaves a pre-existing node untouched", func(t *testing.T) {
		homePath := t.TempDir()
		t.Setenv("COMPOUND_HOME", homePath)
		if err := home.Init(homePath, "test", cliTestNow); err != nil {
			t.Fatal(err)
		}
		store, err := capgraph.Open(homePath)
		if err != nil {
			t.Fatal(err)
		}
		pre := capgraph.Node{ID: "claude_code", Kind: "skill", Name: "Preexisting Claude Node", Triggers: []string{"claude", "code"}, Weight: 3}
		if err := store.Add(pre); err != nil {
			t.Fatal(err)
		}
		before, ok := findNodeByID(t, store, "claude_code")
		if !ok {
			t.Fatalf("setup node missing")
		}
		writeConnectionsPermissionFixture(t, homePath, "claude_code")
		out := captureStdout(t, func() {
			cmdCapability([]string{"import-connections"})
		})
		if !strings.Contains(out, "node=claude_code minted=false") {
			t.Fatalf("expected pre-existing node left unminted, output:\n%s", out)
		}
		after, ok := findNodeByID(t, store, "claude_code")
		if !ok {
			t.Fatalf("node vanished after import")
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("pre-existing node was rewritten:\nbefore=%+v\nafter=%+v", before, after)
		}
	})
}

// F3: with a connection-surface record present, all three shared surfaces carry
// it -- (a) orient/contract CapabilityOffer, (b) workbench --capability-task
// JSON offer summary, (c) capability resolve JSON.
func TestCapabilityImportConnections_F3_RendersAcrossOrientWorkbenchResolve(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	targetPath := writeConnectionsPermissionFixture(t, homePath, "claude_code")
	if _, err := routeevidence.ImportModelCapabilitiesYAML(homePath, filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml"), cliTestNow); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() {
		cmdCapability([]string{"import-connections"})
	})

	// (a) orient / EffectiveContract CapabilityOffer via the shared pack path.
	pack, err := contract.PackWithCapabilities(contract.Input{
		Harness: "claude_code", Cwd: "/repo/x", Task: "claude code permissions", RepoKind: "product", RepoKindVia: "flag", Mode: "plan",
	}, contract.DefaultPolicy(), homePath, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := pack.Validate(); err != nil {
		t.Fatalf("pack invalid with connection surface offer: %v", err)
	}
	offer := findEnvelopeCapabilityOffer(pack.CapabilityOffers, "capability://claude_code")
	if offer == nil {
		t.Fatalf("orient pack missing claude_code offer: %+v", pack.CapabilityOffers)
	}
	if !offerConstraintHasRef(offer.ConnectionSurfaces, "config_path", targetPath) {
		t.Fatalf("orient offer connection surfaces missing config path %q: %+v", targetPath, offer.ConnectionSurfaces)
	}
	if !offerConstraintHasRef(offer.ConnectionSurfaces, "asserting_owner", harness.PermissionAuthorityRef("claude_code")) {
		t.Fatalf("orient offer connection surfaces missing asserting owner: %+v", offer.ConnectionSurfaces)
	}

	// (b) workbench --capability-task JSON offer summary.
	wbOut := captureStdout(t, func() {
		cmdWorkbench([]string{"--json", "--capability-task", "claude code permissions", "--capability-harness", "claude_code"})
	})
	var wb workbenchCLIOutput
	if err := json.Unmarshal([]byte(wbOut), &wb); err != nil {
		t.Fatalf("workbench JSON did not parse: %v\n%s", err, wbOut)
	}
	if wb.CapabilitySurface == nil {
		t.Fatalf("workbench capability surface missing")
	}
	if !surfaceReportHasConnection(t, *wb.CapabilitySurface, "claude_code", targetPath) {
		t.Fatalf("workbench surface offer missing connection surface: %+v", wb.CapabilitySurface.Offers)
	}

	// (c) capability resolve JSON (same SurfaceReport, joined once).
	store, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	report, err := buildCapabilityResolutionReport(store, homePath, "extract claude code config export", "claude_code", false, false, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if !surfaceReportHasConnection(t, report.CapabilitySurface, "claude_code", targetPath) {
		t.Fatalf("resolve surface offer missing connection surface: %+v", report.CapabilitySurface.Offers)
	}
}

func surfaceReportHasConnection(t *testing.T, report capgraph.SurfaceReport, nodeID, wantConfigPath string) bool {
	t.Helper()
	foundNode := false
	for _, offer := range report.Offers {
		if offer.Node.ID != nodeID {
			continue
		}
		foundNode = true
		for _, c := range offer.Constraints.ConnectionSurfaces {
			if c.Kind == "config_path" && c.Ref == wantConfigPath {
				// Also confirm the flat offer-summary refs carry it.
				for _, summary := range report.OfferSummaries {
					if summary.ID == nodeID && stringSliceContains(summary.ConnectionSurfaceRefs, wantConfigPath) {
						return true
					}
				}
				t.Fatalf("offer constraint carried config path but offer summary did not: %+v", report.OfferSummaries)
			}
		}
	}
	if !foundNode {
		t.Fatalf("node %q did not surface in report", nodeID)
	}
	return false
}

// F4: a home with nodes but NO connection-surface records emits offer JSON with
// no new empty fields -- the "connection_surface" key is absent everywhere
// (byte-identical to base behavior).
func TestCapabilityImportConnections_F4_AbsentRecordsByteIdentical(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	store, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Add(capgraph.Node{ID: "tool:reach", Kind: "tool", Name: "reach tool", Triggers: []string{"extract", "claude", "code", "config"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := routeevidence.ImportModelCapabilitiesYAML(homePath, filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml"), cliTestNow); err != nil {
		t.Fatal(err)
	}
	task := "extract claude code config export"

	surfaceOut := captureStdout(t, func() {
		cmdCapability([]string{"surface", "--json", task})
	})
	if strings.Contains(surfaceOut, "connection_surface") {
		t.Fatalf("surface JSON leaked a connection_surface field with no records:\n%s", surfaceOut)
	}

	wbOut := captureStdout(t, func() {
		cmdWorkbench([]string{"--json", "--capability-task", task})
	})
	if strings.Contains(wbOut, "connection_surface") {
		t.Fatalf("workbench JSON leaked a connection_surface field with no records:\n%s", wbOut)
	}

	report, err := buildCapabilityResolutionReport(store, homePath, task, "", false, false, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	resolveJSON, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(resolveJSON), "connection_surface") {
		t.Fatalf("resolve JSON leaked a connection_surface field with no records:\n%s", resolveJSON)
	}

	pack, err := contract.PackWithCapabilities(contract.Input{
		Harness: "claude_code", Cwd: "/repo/x", Task: task, RepoKind: "product", RepoKindVia: "flag", Mode: "plan",
	}, contract.DefaultPolicy(), homePath, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	packJSON, err := json.Marshal(pack)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(packJSON), "connection_surface") {
		t.Fatalf("orient pack JSON leaked a connection_surface field with no records:\n%s", packJSON)
	}
}

// F5: import-connections leaves sourceobs no-promote markers + a
// capability-source-authority record, and claims.jsonl is UNTOUCHED (mirrors
// TestCapabilityImportTopologyRecordsSourceObservationNoPromote).
func TestCapabilityImportConnections_F5_LineageNoPromote(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	writeConnectionsPermissionFixture(t, homePath, "claude_code")
	captureStdout(t, func() {
		cmdCapability([]string{"import-connections"})
	})

	candidateDir := filepath.Join(homePath, candidates.InboxDir(candidates.CapabilityObs))
	entries, err := os.ReadDir(candidateDir)
	if err != nil {
		t.Fatalf("read capability observations: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("capability observation entries = %d, want 1", len(entries))
	}
	noPromoteRows := readNoPromoteDecisionRows(t, homePath)
	if len(noPromoteRows) != 1 || noPromoteRows[0].OwnerTarget.Kind != "source_observation" {
		t.Fatalf("no-promote rows = %+v", noPromoteRows)
	}
	records, err := capgraph.LoadSourceAuthority(homePath)
	if err != nil {
		t.Fatal(err)
	}
	foundVia := false
	for _, rec := range records {
		if rec.ImportedVia == "capability import-connections" {
			foundVia = true
		}
	}
	if !foundVia {
		t.Fatalf("no capability-source-authority record for import-connections: %+v", records)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("connection import must not promote claims, claims ledger stat err=%v", err)
	}
}

// F6: the additive CapabilityOffer field keeps the schema battery green -- the
// context_pack schema still exports (with capability_offers) and now reflects
// the additive connection_surfaces field.
func TestCapabilityImportConnections_F6_SchemaStable(t *testing.T) {
	out := captureStdout(t, func() {
		cmdSchema([]string{"export", "context-pack"})
	})
	if !strings.Contains(out, `"capability_offers"`) {
		t.Fatalf("schema export lost capability_offers:\n%s", out)
	}
	if !strings.Contains(out, `"connection_surfaces"`) {
		t.Fatalf("schema export missing additive connection_surfaces field:\n%s", out)
	}
}
