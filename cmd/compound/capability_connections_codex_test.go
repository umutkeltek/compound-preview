package main

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"compound/internal/capgraph"
	"compound/internal/contract"
	"compound/internal/harness"
	"compound/internal/home"
)

func TestCapabilityImportConnectionsCodex_MintsSurfaceAndRerunsIdempotently(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	targetPath := writeConnectionsPermissionFixture(t, homePath, "codex")
	authorityPath := harness.PermissionAuthorityPath(homePath, "codex")
	hookRoot := t.TempDir()
	hookRefs := []string{
		filepath.Join(hookRoot, "pre-tool-use.hook.ts"),
		filepath.Join(hookRoot, "session-start.hook.ts"),
	}
	writeHarnessDriftObserveJSON(t, contract.InstalledHooksPath(homePath), map[string]any{
		"schema_version": "compound.installed_hooks.v0.1",
		"hooks": []map[string]any{
			{"harness": "codex", "name": "PreToolUse", "event": "PreToolUse", "ref": hookRefs[0], "status": "current"},
			{"harness": "codex", "name": "SessionStart", "event": "SessionStart", "ref": hookRefs[1], "status": "current"},
		},
	})

	firstOut := captureStdout(t, func() {
		cmdCapability([]string{"import-connections", "--harness", "codex"})
	})
	for _, want := range []string{
		"# capability import-connections ok",
		"harness: codex node=codex minted=true",
		"connection_surface_added=true",
	} {
		if !strings.Contains(firstOut, want) {
			t.Fatalf("first codex import output missing %q:\n%s", want, firstOut)
		}
	}

	store, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := findNodeByID(t, store, "codex")
	if !ok {
		t.Fatal("codex harness node was not minted")
	}
	if node.Kind != "harness" {
		t.Fatalf("codex node kind = %q, want harness", node.Kind)
	}
	surfaces, err := store.ConnectionSurfaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(surfaces) != 1 {
		t.Fatalf("connection surfaces = %d, want 1: %+v", len(surfaces), surfaces)
	}
	wantPaths := append([]string{targetPath, authorityPath}, hookRefs...)
	if !reflect.DeepEqual(surfaces[0].ConfigPaths, wantPaths) {
		t.Fatalf("codex config paths = %+v, want %+v", surfaces[0].ConfigPaths, wantPaths)
	}
	if surfaces[0].NodeID != "codex" || surfaces[0].AssertingOwner != harness.PermissionAuthorityRef("codex") {
		t.Fatalf("codex connection surface identity = %+v", surfaces[0])
	}
	if lines := connectionSurfaceLineCount(t, homePath); lines != 1 {
		t.Fatalf("sidecar line count after first codex import = %d, want 1", lines)
	}

	secondOut := captureStdout(t, func() {
		cmdCapability([]string{"import-connections", "--harness", "codex"})
	})
	for _, want := range []string{
		"harness: codex node=codex minted=false",
		"connection_surface_added=false",
	} {
		if !strings.Contains(secondOut, want) {
			t.Fatalf("second codex import output missing %q:\n%s", want, secondOut)
		}
	}
	if lines := connectionSurfaceLineCount(t, homePath); lines != 1 {
		t.Fatalf("sidecar line count after idempotent codex import = %d, want 1", lines)
	}
}
