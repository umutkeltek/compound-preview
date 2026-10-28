package sourceobs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCanonManifestAttestsInstructionSupplyChainSurfaces(t *testing.T) {
	root := t.TempDir()
	paths := map[string]string{
		"agents": filepath.Join(root, "AGENTS.md"),
		"claude": filepath.Join(root, "CLAUDE.md"),
		"skill":  filepath.Join(root, "skills", "security-review", "SKILL.md"),
		"plugin": filepath.Join(root, "plugins", "reviewer", "plugin.json"),
		"mcp":    filepath.Join(root, "mcp-on-demand.json"),
	}
	writeInstructionSupplyChainFixture(t, paths["agents"], "# Agent contract\n\nBody.\n")
	writeInstructionSupplyChainFixture(t, paths["claude"], "# Claude contract\n\nBody.\n")
	writeInstructionSupplyChainFixture(t, paths["skill"], "---\nname: security-review\n---\n# Review\n")
	writeInstructionSupplyChainFixture(t, paths["plugin"], `{"name":"reviewer","hooks":{"PreToolUse":"hook.ts"}}`)
	writeInstructionSupplyChainFixture(t, paths["mcp"], `{"servers":{"github":{"command":"mcporter","args":["github"]}}}`)

	pairs := make([]CanonPathNode, 0, len(paths))
	for name, path := range paths {
		pairs = append(pairs, CanonPathNode{Path: path, NodeRef: "instruction:" + name})
	}
	manifest, err := BuildCanonManifest(
		"instruction-supply-chain",
		pairs,
		time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]SourceManifestItem{}
	for _, item := range manifest.Items {
		got[instructionSupplyChainSurface(item.StructureOverlays)] = item
	}
	for _, surface := range []string{"agents", "claude", "skill", "plugin", "mcp_manifest"} {
		item, ok := got[surface]
		if !ok {
			t.Fatalf("missing surface %q; items=%+v", surface, manifest.Items)
		}
		if schema := instructionSupplyChainSchema(item.StructureOverlays); schema == "" {
			t.Fatalf("surface %q has no structural schema digest: %+v", surface, item.StructureOverlays)
		}
		if revision := instructionSupplyChainStructureRevision(item.StructureOverlays); revision != instructionSupplyChainSchemaRevision {
			t.Fatalf("surface %q schema revision = %q, want %q", surface, revision, instructionSupplyChainSchemaRevision)
		}
	}
}

func TestCanonDriftAllowsContentChangeButHoldsInstructionSchemaChange(t *testing.T) {
	homePath := t.TempDir()
	sourcePath := filepath.Join(t.TempDir(), "AGENTS.md")
	writeInstructionSupplyChainFixture(t, sourcePath, "# Contract\n\nOriginal guidance.\n")
	canonBaseline(
		t,
		homePath,
		tCanon,
		[]CanonPathNode{{Path: sourcePath, NodeRef: "harness:codex"}},
	)

	writeInstructionSupplyChainFixture(t, sourcePath, "# Contract\n\nUpdated guidance.\n")
	contentReport, contentManifest, err := ObserveCanonDrift(
		homePath,
		"harness-canon",
		tCanon.Add(time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(contentReport.AdmissionHolds) != 0 {
		t.Fatalf("content-only change should remain automatic: %+v", contentReport.AdmissionHolds)
	}
	if got := contentManifest.Items[0].FileHash; got == "" || got == loadCanonItem(t, homePath, sourcePath).FileHash {
		t.Fatalf("content-only candidate was not rebaselined in returned manifest: %+v", contentManifest.Items[0])
	}

	writeInstructionSupplyChainFixture(t, sourcePath, "# Contract\n\n## New authority surface\n\nUpdated guidance.\n")
	structureReport, structureManifest, err := ObserveCanonDrift(
		homePath,
		"harness-canon",
		tCanon.Add(2*time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(structureReport.AdmissionHolds) != 1 {
		t.Fatalf("instruction schema change must hold exactly once: %+v", structureReport)
	}
	hold := structureReport.AdmissionHolds[0]
	if hold.Surface != "agents" || hold.PriorSchema == "" || hold.LiveSchema == "" || hold.PriorSchema == hold.LiveSchema {
		t.Fatalf("bad structural hold: %+v", hold)
	}
	if got, want := structureManifest.Items[0].FileHash, loadCanonItem(t, homePath, sourcePath).FileHash; got != want {
		t.Fatalf("held structural change advanced baseline: got %s want %s", got, want)
	}
}

func TestCanonDriftHoldsMCPManifestSchemaExpansionButNotValueRotation(t *testing.T) {
	homePath := t.TempDir()
	sourcePath := filepath.Join(t.TempDir(), "mcp-on-demand.json")
	writeInstructionSupplyChainFixture(
		t,
		sourcePath,
		`{"servers":{"github":{"command":"mcporter","args":["github"],"env":{"TOKEN":"redacted"}}}}`,
	)
	canonBaseline(
		t,
		homePath,
		tCanon,
		[]CanonPathNode{{Path: sourcePath, NodeRef: "registry:mcp"}},
	)

	writeInstructionSupplyChainFixture(
		t,
		sourcePath,
		`{"servers":{"github":{"command":"mcporter","args":["github-v2"],"env":{"TOKEN":"rotated"}}}}`,
	)
	valueReport, _, err := ObserveCanonDrift(homePath, "harness-canon", tCanon.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(valueReport.AdmissionHolds) != 0 {
		t.Fatalf("same-schema value rotation should remain automatic: %+v", valueReport.AdmissionHolds)
	}

	writeInstructionSupplyChainFixture(
		t,
		sourcePath,
		`{"servers":{"github":{"command":"mcporter","args":["github-v2"],"env":{"TOKEN":"rotated"},"tools":{"create_issue":{"inputSchema":{"type":"object"}}}}}}`,
	)
	schemaReport, _, err := ObserveCanonDrift(homePath, "harness-canon", tCanon.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(schemaReport.AdmissionHolds) != 1 || schemaReport.AdmissionHolds[0].Surface != "mcp_manifest" {
		t.Fatalf("MCP schema expansion must hold: %+v", schemaReport.AdmissionHolds)
	}
}

func TestCanonDriftHoldsMissingInstructionSurfaceAndRejectsInvalidManifest(t *testing.T) {
	homePath := t.TempDir()
	sourcePath := filepath.Join(t.TempDir(), "CLAUDE.md")
	writeInstructionSupplyChainFixture(t, sourcePath, "# Contract\n")
	canonBaseline(
		t,
		homePath,
		tCanon,
		[]CanonPathNode{{Path: sourcePath, NodeRef: "harness:claude_code"}},
	)
	if err := os.Remove(sourcePath); err != nil {
		t.Fatal(err)
	}
	report, _, err := ObserveCanonDrift(homePath, "harness-canon", tCanon.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.AdmissionHolds) != 1 ||
		report.AdmissionHolds[0].Surface != "claude" ||
		report.AdmissionHolds[0].Reason != "instruction surface unavailable" {
		t.Fatalf("missing instruction surface must hold: %+v", report.AdmissionHolds)
	}

	invalidPath := filepath.Join(t.TempDir(), "mcp-on-demand.json")
	writeInstructionSupplyChainFixture(t, invalidPath, `{"servers":{}} {"second":"document"}`)
	if _, err := BuildCanonManifest(
		"invalid-instruction-canon",
		[]CanonPathNode{{Path: invalidPath, NodeRef: "registry:mcp"}},
		tCanon,
	); err == nil {
		t.Fatal("multiple JSON documents were admitted as one MCP manifest")
	}
}

func writeInstructionSupplyChainFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func loadCanonItem(t *testing.T, homePath, sourcePath string) SourceManifestItem {
	t.Helper()
	manifest, err := LoadSourceManifest(homePath, "harness-canon")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range manifest.Items {
		if item.SourceRef == sourcePath {
			return item
		}
	}
	t.Fatalf("missing canon item %s", sourcePath)
	return SourceManifestItem{}
}
