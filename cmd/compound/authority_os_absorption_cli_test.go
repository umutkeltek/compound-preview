package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"compound/internal/authority"
	"compound/internal/capgraph"
	"compound/internal/home"
)

func TestAuthorityOSAbsorptionCLICommands(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	capSources := []string{
		"/synthetic/example-os/spine/views/skill-manifest.json",
		"/synthetic/example-os/registry/tool-topology.json",
		"/synthetic/example-os/spine/views/skills-selection-board.json",
	}
	for _, source := range capSources {
		rev := "sha256:" + testDigest(source)
		if _, err := capgraph.RecordSourceAuthority(homePath, capgraph.SourceAuthorityInput{
			SourcePath:     source,
			SourceRevision: rev,
			ImportedVia:    "cli test import",
			NodeIDs:        []string{"skill:" + filepath.Base(source)},
			EvidenceRefs:   []string{"fixture:" + filepath.Base(source)},
		}, cliTestNow); err != nil {
			t.Fatalf("record cap source authority: %v", err)
		}
	}

	matrix := authority.OSAbsorptionSourceMatrix{
		SchemaVersion: authority.OSAbsorptionMatrixSchema,
		Rows: []authority.OSAbsorptionMatrixRow{
			osAbsorptionCLIRow("skill_manifest", capSources[0], "sha256:"+testDigest(capSources[0]), "absorbed", capgraph.CapabilityGraphAuthorityRef, "compound capability authority", "capability_source_authority"),
			osAbsorptionCLIRow("tool_topology", capSources[1], "sha256:"+testDigest(capSources[1]), "absorbed", capgraph.CapabilityGraphAuthorityRef, "compound capability authority", "capability_source_authority"),
			osAbsorptionCLIRow("skills_selection_board", capSources[2], "sha256:"+testDigest(capSources[2]), "absorbed", capgraph.CapabilityGraphAuthorityRef, "compound capability authority", "capability_source_authority"),
			osAbsorptionCLIRow("brain", "/synthetic/brain/projects/compound.md", "", "absorbed", "compound://ledger/memory", "compound memory recall", "manual"),
			osAbsorptionCLIRow("gbrain", "gbrain://synthetic-store", "", "absorbed", "compound://adapters/memory-engine/gbrain", "compound memory recall", "manual"),
			osAbsorptionCLIRow("infra_os", "/synthetic/infra-os/project-registry.yaml", "", "absorbed", "compound://registries/projects", "compound system map", "manual"),
			osAbsorptionCLIRow("packets", "/synthetic/example-os/packets", "", "absorbed", "compound://work/packets", "compound workbench", "manual"),
		},
	}
	matrixPath := filepath.Join(t.TempDir(), "matrix.json")
	writeCmdJSONFixture(t, matrixPath, matrix)

	importOut := captureStdout(t, func() {
		cmdAuthority([]string{"import-os-absorption", matrixPath})
	})
	var importReport authority.OSAbsorptionImportReport
	if err := json.Unmarshal([]byte(importOut), &importReport); err != nil {
		t.Fatalf("parse import report: %v\n%s", err, importOut)
	}
	if importReport.RowCount != 7 || importReport.AbsorbedCount != 7 || importReport.PendingCount != 0 {
		t.Fatalf("import report = %+v", importReport)
	}

	showOut := captureStdout(t, func() {
		cmdAuthority([]string{"os-absorption", "--json"})
	})
	var loaded authority.OSAbsorptionSourceMatrix
	if err := json.Unmarshal([]byte(showOut), &loaded); err != nil {
		t.Fatalf("parse matrix: %v\n%s", err, showOut)
	}
	if len(loaded.Rows) != 7 || loaded.AuthorityRef != authority.OSAbsorptionAuthorityRef {
		t.Fatalf("loaded matrix = %+v", loaded)
	}

	doctorOut := captureStdout(t, func() {
		cmdAuthority([]string{"os-absorption-doctor", "--json"})
	})
	var doctor authority.OSAbsorptionDoctorReport
	if err := json.Unmarshal([]byte(doctorOut), &doctor); err != nil {
		t.Fatalf("parse doctor: %v\n%s", err, doctorOut)
	}
	if doctor.Status != "ok" || doctor.AutoProofCount != 3 || doctor.PendingWithGateCount != 0 {
		t.Fatalf("doctor = %+v", doctor)
	}
}

func osAbsorptionCLIRow(id, source, revision, status, owner, consumer, proofMode string) authority.OSAbsorptionMatrixRow {
	return authority.OSAbsorptionMatrixRow{
		ID:             id,
		SourceRef:      source,
		SourceRevision: revision,
		OldOwner:       "legacy",
		OldRole:        "legacy source",
		CurrentRole:    "evidence_only_input",
		Privacy:        "private",
		CompoundOwner:  owner,
		Consumer:       consumer,
		Proof:          consumer + " proof",
		ProofMode:      proofMode,
		RetirementGate: "consumer reads Compound authority",
		Status:         status,
	}
}

func writeCmdJSONFixture(t *testing.T, path string, v any) {
	t.Helper()
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
