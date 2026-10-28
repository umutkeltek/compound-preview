package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/home"
)

// writeGatewayRegistryFixture writes a SYNTHETIC switchback/provider-registry@3
// file into dir (outside COMPOUND_HOME, mirroring a real gateway registry) and
// returns its path. It never copies the real registry: fake .invalid/.example
// hosts + real auth_scheme vocabulary, plus one provider with an empty base_url
// to exercise the skip path. Re-calling with the same dir overwrites the same
// file so a base_url edit changes the file bytes (new whole-file revision).
func writeGatewayRegistryFixture(t *testing.T, dir, alphaBaseURL string) string {
	t.Helper()
	registry := map[string]any{
		"schema":    "switchback/provider-registry@3",
		"generated": "2026-07-16T00:00:00Z",
		"providers": []map[string]any{
			{"id": "alpha", "name": "Alpha Provider", "base_url": alphaBaseURL, "auth_scheme": "bearer"},
			{"id": "beta", "name": "Beta Provider", "base_url": "https://api.beta.example/v1", "auth_scheme": "header:x-api-key"},
			{"id": "gamma", "name": "Gamma (no endpoint)", "base_url": "", "auth_scheme": "none"},
		},
		"models": []any{},
	}
	data, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "provider-registry.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func gatewayMustOpenStore(t *testing.T, homePath string) *capgraph.Store {
	t.Helper()
	store, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func gatewaySidecarLineCount(t *testing.T, homePath string) int {
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

func gatewaySurfaceByNode(t *testing.T, store *capgraph.Store, id string) capgraph.ConnectionSurface {
	t.Helper()
	surfaces, err := store.ConnectionSurfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, cs := range surfaces {
		if cs.NodeID == id {
			return cs
		}
	}
	t.Fatalf("connection surface for node %q not found", id)
	return capgraph.ConnectionSurface{}
}

func gatewayFindNode(t *testing.T, store *capgraph.Store, id string) (capgraph.Node, bool) {
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

// gatewaySnapshotDir hashes every file directly under dir so a fail-closed
// import can be proven to have written nothing (before == after).
func gatewaySnapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out
		}
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		out[e.Name()] = hex.EncodeToString(sum[:])
	}
	return out
}

// G1: importing a synthetic schema@3 fixture mints a gateway root + one node per
// connectable provider (kind "gateway", NO triggers, low weight) carrying the
// right Endpoint/AuthMode/schema/owner/SourceRevision; a provider with an empty
// base_url is skipped and counted (never silent-dropped).
//
// DEVIATION (documented): the packet's G1 says "retrievable via
// `capability surface --json`", but decision 2 mandates gateway nodes have NO
// triggers so they can "never surface by task-match alone" -- the two clauses
// are in tension. Decision 2 is the explicit hard constraint, so retrieval here
// is proven through the import report + the persisted sidecar/node list (the
// channels a no-trigger evidence node is actually reachable through), and this
// test additionally asserts gateway nodes do NOT appear in `Surface`.
func TestCapabilityImportGateway_G1_MintsGatewayNodesWithConnectivity(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	regDir := t.TempDir()
	registryPath := writeGatewayRegistryFixture(t, regDir, "https://api.alpha.invalid/v1")

	out := captureStdout(t, func() {
		cmdCapability([]string{"import-gateway", registryPath})
	})
	for _, want := range []string{
		"# capability import-gateway ok",
		"- gateway: switchback",
		"- registry: " + registryPath + " (schema switchback/provider-registry@3)",
		"gateway:switchback/provider/alpha",
		"endpoint=https://api.alpha.invalid/v1",
		"auth_mode=bearer",
		"role=root",
		"- skipped_providers: 1",
		"- skipped_provider: gamma",
		"- source_observation: srcobs_gw_",
		"- source_authority: capsrc_",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("import-gateway output missing %q:\n%s", want, out)
		}
	}

	store := gatewayMustOpenStore(t, homePath)
	rootID := capgraph.GatewayRootNodeID("switchback")
	alphaID := capgraph.GatewayProviderNodeID("switchback", "alpha")
	betaID := capgraph.GatewayProviderNodeID("switchback", "beta")
	gammaID := capgraph.GatewayProviderNodeID("switchback", "gamma")

	for _, id := range []string{rootID, alphaID, betaID} {
		n, ok := gatewayFindNode(t, store, id)
		if !ok {
			t.Fatalf("gateway node %q not minted", id)
		}
		if n.Kind != "gateway" {
			t.Fatalf("node %q kind = %q, want gateway", id, n.Kind)
		}
		if len(n.Triggers) != 0 {
			t.Fatalf("gateway node %q has triggers %v; must be empty (never surface by task-match)", id, n.Triggers)
		}
	}
	if _, ok := gatewayFindNode(t, store, gammaID); ok {
		t.Fatalf("provider gamma (empty base_url) must be skipped, not minted")
	}

	alpha := gatewaySurfaceByNode(t, store, alphaID)
	if alpha.Endpoint != "https://api.alpha.invalid/v1" {
		t.Fatalf("alpha endpoint = %q", alpha.Endpoint)
	}
	if alpha.AuthMode != "bearer" {
		t.Fatalf("alpha auth mode = %q", alpha.AuthMode)
	}
	if alpha.SettingsSchemaRef != "switchback/provider-registry@3" {
		t.Fatalf("alpha schema ref = %q", alpha.SettingsSchemaRef)
	}
	if alpha.AssertingOwner != "switchback://provider-registry" {
		t.Fatalf("alpha asserting owner = %q", alpha.AssertingOwner)
	}
	if !strings.HasPrefix(alpha.SourceRevision, "sha256:") {
		t.Fatalf("alpha source revision = %q, want sha256: prefix", alpha.SourceRevision)
	}
	if !stringSliceContains(alpha.ConfigPaths, registryPath) {
		t.Fatalf("alpha config paths %v missing registry path %q", alpha.ConfigPaths, registryPath)
	}

	// Root surface carries schema/owner/revision but NO endpoint/auth.
	root := gatewaySurfaceByNode(t, store, rootID)
	if root.Endpoint != "" || root.AuthMode != "" {
		t.Fatalf("root surface must not carry endpoint/auth: %+v", root)
	}
	if root.SourceRevision != alpha.SourceRevision {
		t.Fatalf("root revision %q != alpha revision %q (same source file)", root.SourceRevision, alpha.SourceRevision)
	}

	// Decision-2 guarantee: gateway nodes never surface by task-match.
	surfaced, err := store.Surface("alpha bearer gateway switchback provider endpoint connectivity")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range surfaced {
		if s.Node.Kind == "gateway" {
			t.Fatalf("gateway node %q surfaced by task-match; must stay evidence-only", s.Node.ID)
		}
	}
}

// G2: re-import of an unchanged registry adds nothing (idempotent); mutating one
// provider's base_url changes the whole-file bytes -> a new SourceRevision.
//
// DEVIATION (documented): the packet's G2 says "others untouched". Decision 3
// mandates SourceRevision = sha256 of the WHOLE file, so any edit bumps the
// shared revision on every surface record (they re-append; last-wins keeps the
// logical count stable). "Others untouched" is therefore honored at the
// CONNECTIVITY-FACT level: a non-mutated provider's endpoint/auth stay
// byte-identical, even though its record carries the new shared revision.
func TestCapabilityImportGateway_G2_IdempotentAndUpdatable(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	regDir := t.TempDir()
	registryPath := writeGatewayRegistryFixture(t, regDir, "https://api.alpha.invalid/v1")

	captureStdout(t, func() { cmdCapability([]string{"import-gateway", registryPath}) })
	firstLines := gatewaySidecarLineCount(t, homePath)
	if firstLines == 0 {
		t.Fatalf("no connection surfaces written on first import")
	}

	// Re-import unchanged: all added=false, minted=false; no new sidecar lines.
	reOut := captureStdout(t, func() { cmdCapability([]string{"import-gateway", registryPath, "--json"}) })
	var reReport gatewayImportReport
	if err := json.Unmarshal([]byte(reOut), &reReport); err != nil {
		t.Fatalf("import-gateway JSON did not parse: %v\n%s", err, reOut)
	}
	if len(reReport.Nodes) == 0 {
		t.Fatalf("re-import reported no nodes: %s", reOut)
	}
	for _, n := range reReport.Nodes {
		if n.ConnectionSurfaceAdded {
			t.Fatalf("re-import of unchanged registry re-added surface for %q; want idempotent", n.NodeID)
		}
		if n.NodeMinted {
			t.Fatalf("re-import re-minted node %q; want idempotent", n.NodeID)
		}
	}
	if lines := gatewaySidecarLineCount(t, homePath); lines != firstLines {
		t.Fatalf("sidecar line count %d != %d after idempotent re-import", lines, firstLines)
	}

	store := gatewayMustOpenStore(t, homePath)
	alphaID := capgraph.GatewayProviderNodeID("switchback", "alpha")
	betaID := capgraph.GatewayProviderNodeID("switchback", "beta")
	origAlpha := gatewaySurfaceByNode(t, store, alphaID)
	origBeta := gatewaySurfaceByNode(t, store, betaID)

	// Mutate alpha's base_url -> new whole-file revision.
	writeGatewayRegistryFixture(t, regDir, "https://api.alpha-2.invalid/v2")
	captureStdout(t, func() { cmdCapability([]string{"import-gateway", registryPath}) })

	newAlpha := gatewaySurfaceByNode(t, store, alphaID)
	newBeta := gatewaySurfaceByNode(t, store, betaID)
	if newAlpha.Endpoint != "https://api.alpha-2.invalid/v2" {
		t.Fatalf("alpha endpoint did not update: %q", newAlpha.Endpoint)
	}
	if newAlpha.SourceRevision == origAlpha.SourceRevision {
		t.Fatalf("alpha revision did not change after the file was edited")
	}
	// Non-mutated provider's connectivity facts stay byte-identical.
	if newBeta.Endpoint != origBeta.Endpoint || newBeta.AuthMode != origBeta.AuthMode {
		t.Fatalf("beta connectivity facts changed: before endpoint=%q auth=%q, after endpoint=%q auth=%q",
			origBeta.Endpoint, origBeta.AuthMode, newBeta.Endpoint, newBeta.AuthMode)
	}
	// Logical record count is stable (last-wins by node id).
	surfaces, err := store.ConnectionSurfaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(surfaces) != firstLines {
		t.Fatalf("logical surface record count %d != %d after update", len(surfaces), firstLines)
	}
}

// G3: a pre-existing node whose id collides with a provider node is NOT
// rewritten (reflect.DeepEqual before/after).
func TestCapabilityImportGateway_G3_PreExistingNodeNotRewritten(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	store := gatewayMustOpenStore(t, homePath)
	collideID := capgraph.GatewayProviderNodeID("switchback", "alpha")
	pre := capgraph.Node{ID: collideID, Kind: "skill", Name: "Pre-existing collider", Triggers: []string{"alpha", "collide"}, Weight: 4}
	if err := store.Add(pre); err != nil {
		t.Fatal(err)
	}
	before, ok := gatewayFindNode(t, store, collideID)
	if !ok {
		t.Fatalf("setup node missing")
	}

	regDir := t.TempDir()
	registryPath := writeGatewayRegistryFixture(t, regDir, "https://api.alpha.invalid/v1")
	out := captureStdout(t, func() { cmdCapability([]string{"import-gateway", registryPath, "--json"}) })
	var report gatewayImportReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("import-gateway JSON parse: %v\n%s", err, out)
	}
	for _, n := range report.Nodes {
		if n.NodeID == collideID && n.NodeMinted {
			t.Fatalf("colliding node %q reported minted; must be left untouched", collideID)
		}
	}
	after, ok := gatewayFindNode(t, store, collideID)
	if !ok {
		t.Fatalf("node vanished after import")
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("pre-existing node rewritten:\nbefore=%+v\nafter=%+v", before, after)
	}
}

// G4: a surfacing node with NO connection surface produces output byte-identical
// to base -- no "connection_surface" key in JSON and no "connection_surfaces:"
// line in human text (the additive renderer line stays gated on real records).
func TestCapabilityImportGateway_G4_AbsentSurfaceByteIdentical(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	store := gatewayMustOpenStore(t, homePath)
	if err := store.Add(capgraph.Node{ID: "tool:plain", Kind: "tool", Name: "plain tool", Triggers: []string{"widget", "plainrender"}}); err != nil {
		t.Fatal(err)
	}
	task := "widget plainrender"

	jsonOut := captureStdout(t, func() { cmdCapability([]string{"surface", "--json", task}) })
	if strings.Contains(jsonOut, "connection_surface") {
		t.Fatalf("surface JSON leaked a connection_surface field with no records:\n%s", jsonOut)
	}
	textOut := captureStdout(t, func() { cmdCapability([]string{"surface", task}) })
	if !strings.Contains(textOut, "tool:plain") {
		t.Fatalf("expected the plain node to surface:\n%s", textOut)
	}
	if strings.Contains(textOut, "connection_surfaces:") {
		t.Fatalf("human text printed a connection_surfaces line with no records:\n%s", textOut)
	}
}

// G5: import records a no-promote SourceObservation candidate + a
// capability-source-authority row (imported_via "capability import-gateway"),
// and claims.jsonl stays UNTOUCHED (no promotion).
func TestCapabilityImportGateway_G5_LineageNoPromote(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	regDir := t.TempDir()
	registryPath := writeGatewayRegistryFixture(t, regDir, "https://api.alpha.invalid/v1")
	captureStdout(t, func() { cmdCapability([]string{"import-gateway", registryPath}) })

	candidateDir := filepath.Join(homePath, candidates.InboxDir(candidates.CapabilityObs))
	entries, err := os.ReadDir(candidateDir)
	if err != nil {
		t.Fatalf("read capability observations: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("capability observation entries = %d, want 1", len(entries))
	}
	rows := readNoPromoteDecisionRows(t, homePath)
	if len(rows) != 1 || rows[0].OwnerTarget.Kind != "source_observation" {
		t.Fatalf("no-promote rows = %+v", rows)
	}
	records, err := capgraph.LoadSourceAuthority(homePath)
	if err != nil {
		t.Fatal(err)
	}
	foundVia := false
	for _, rec := range records {
		if rec.ImportedVia == "capability import-gateway" {
			foundVia = true
			if rec.AuthorityRef != capgraph.CapabilityGraphAuthorityRef {
				t.Fatalf("authority ref = %q, want %q", rec.AuthorityRef, capgraph.CapabilityGraphAuthorityRef)
			}
			if len(rec.NodeIDs) == 0 {
				t.Fatalf("source authority row has no node ids")
			}
		}
	}
	if !foundVia {
		t.Fatalf("no capability-source-authority record for import-gateway: %+v", records)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("gateway import must not promote claims, claims ledger stat err=%v", err)
	}
}

// G6: `capability surface` human text for a task matching a node WITH a
// connection surface prints a "connection_surfaces:" line (closes the renderer
// gap decision 7). Gateway nodes never surface, so this exercises the general
// renderer fix with a trigger-bearing node that carries a surface.
func TestCapabilityImportGateway_G6_HumanTextPrintsConnectionSurfaces(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	store := gatewayMustOpenStore(t, homePath)
	if err := store.Add(capgraph.Node{ID: "tool:reachable", Kind: "tool", Name: "reachable tool", Triggers: []string{"widget", "reachnow"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddConnectionSurface(capgraph.ConnectionSurface{
		NodeID:         "tool:reachable",
		Endpoint:       "https://reach.invalid/v1",
		AuthMode:       "bearer",
		AssertingOwner: "switchback://provider-registry",
		SourceRevision: "sha256:deadbeefcafe",
	}); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { cmdCapability([]string{"surface", "widget reachnow"}) })
	if !strings.Contains(out, "tool:reachable") {
		t.Fatalf("node did not surface:\n%s", out)
	}
	if !strings.Contains(out, "connection_surfaces:") {
		t.Fatalf("human text missing connection_surfaces line (renderer gap not closed):\n%s", out)
	}
	if !strings.Contains(out, "https://reach.invalid/v1") {
		t.Fatalf("connection_surfaces line missing endpoint:\n%s", out)
	}
}

// G7: an unreadable file, a missing schema field, or an unknown schema fails
// closed -- an error is returned and NOTHING is persisted (no partial import).
func TestCapabilityImportGateway_G7_FailsClosedNoPartialImport(t *testing.T) {
	assertNothing := func(t *testing.T, homePath string, before map[string]string) {
		t.Helper()
		after := gatewaySnapshotDir(t, filepath.Join(homePath, "registries"))
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("fail-closed import mutated registries:\nbefore=%v\nafter=%v", before, after)
		}
		candidateDir := filepath.Join(homePath, candidates.InboxDir(candidates.CapabilityObs))
		if entries, err := os.ReadDir(candidateDir); err == nil && len(entries) != 0 {
			t.Fatalf("fail-closed import wrote %d candidate observations", len(entries))
		}
	}

	cases := []struct {
		name    string
		prepare func(t *testing.T) string // returns the registry path to import
	}{
		{
			name: "unreadable file",
			prepare: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "does-not-exist.json")
			},
		},
		{
			name: "missing schema field",
			prepare: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "no-schema.json")
				if err := os.WriteFile(p, []byte(`{"providers":[{"id":"x","base_url":"https://x.invalid"}]}`), 0o644); err != nil {
					t.Fatal(err)
				}
				return p
			},
		},
		{
			name: "unknown schema",
			prepare: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "wrong-schema.json")
				if err := os.WriteFile(p, []byte(`{"schema":"switchback/model-registry@1","providers":[]}`), 0o644); err != nil {
					t.Fatal(err)
				}
				return p
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			homePath := t.TempDir()
			t.Setenv("COMPOUND_HOME", homePath)
			if err := home.Init(homePath, "test", cliTestNow); err != nil {
				t.Fatal(err)
			}
			store := gatewayMustOpenStore(t, homePath)
			before := gatewaySnapshotDir(t, filepath.Join(homePath, "registries"))
			registryPath := tc.prepare(t)
			_, err := importCapabilityGateway(store, homePath, registryPath, "", cliTestNow)
			if err == nil {
				t.Fatalf("expected fail-closed error for %s", tc.name)
			}
			assertNothing(t, homePath, before)
		})
	}
}
