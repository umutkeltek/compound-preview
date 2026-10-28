package authority

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/capgraph"
)

// TestOSAbsorptionCapabilitySubsetAuthorityProof exercises the
// subset-scoped extension to "capability_source_authority": a row that
// declares an enumerated absorb-subset via absorb_subset_file:<path>:sha256:
// <hash> evidence_refs gets those files re-hashed live on every doctor run,
// while every other file in the same source_ref directory (out-of-row
// evidence-only churn, e.g. PORTFOLIO-MAP.md next to ETHOS.md) is never even
// read. This mirrors the real os_source_brain_solo_builder_doctrine row
// against capsrc_a84cf2259d25 (absorb-subset = ETHOS.md + instincts.md out of
// 36 files in the source tree).
func TestOSAbsorptionCapabilitySubsetAuthorityProof(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(t *testing.T, fixture capabilitySubsetFixture)
		wantStatus string
		wantDetail string
	}{
		{
			name:       "declared absorb-subset matches live files: ok from existing evidence",
			wantStatus: "ok",
		},
		{
			name: "a subset file edited after import proves pending drift naming the file",
			mutate: func(t *testing.T, fixture capabilitySubsetFixture) {
				t.Helper()
				writeCapabilitySubsetFile(t, fixture.ethosPath, []byte("# ETHOS\n\nedited after absorption, without re-import.\n"))
			},
			wantStatus: "pending",
			wantDetail: "ETHOS.md",
		},
		{
			name: "the other subset file edited after import also proves pending, naming that file",
			mutate: func(t *testing.T, fixture capabilitySubsetFixture) {
				t.Helper()
				writeCapabilitySubsetFile(t, fixture.instinctsPath, []byte("# instincts\n\nedited after absorption, without re-import.\n"))
			},
			wantStatus: "pending",
			wantDetail: "instincts.md",
		},
		{
			name: "out-of-subset churn in the same directory never touches the row",
			mutate: func(t *testing.T, fixture capabilitySubsetFixture) {
				t.Helper()
				writeCapabilitySubsetFile(t, fixture.portfolioMapPath, []byte("# PORTFOLIO-MAP\n\ndaily churn, out of row, never a capability node.\n"))
			},
			wantStatus: "ok",
		},
		{
			name: "a subset file deleted after import fails closed (not a silent pending or ok)",
			mutate: func(t *testing.T, fixture capabilitySubsetFixture) {
				t.Helper()
				if err := os.Remove(fixture.instinctsPath); err != nil {
					t.Fatal(err)
				}
			},
			wantStatus: "failed",
			wantDetail: "instincts.md",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := installCapabilitySubsetFixture(t)
			if tt.mutate != nil {
				tt.mutate(t, fixture)
			}

			doctor, err := OSAbsorptionDoctor(fixture.compoundHome)
			if err != nil {
				t.Fatalf("doctor: %v", err)
			}
			got, ok := findOSAbsorptionDoctorRow(doctor.Rows, "os_source_brain_solo_builder_doctrine")
			if !ok {
				t.Fatalf("doctor row os_source_brain_solo_builder_doctrine missing: %+v", doctor.Rows)
			}
			if got.ProofStatus != tt.wantStatus {
				t.Fatalf("proof_status=%q, want %q (detail=%q)", got.ProofStatus, tt.wantStatus, got.ProofDetail)
			}
			// Row status string must never change: this proof mode verifies a
			// declared subset, it does not assert the whole doctrine tree is
			// absorbed (34 of 36 files remain deliberately out-of-row).
			if got.Status != "pending" {
				t.Fatalf("row status string must not change: %+v", got)
			}
			switch tt.wantStatus {
			case "ok":
				if !strings.Contains(got.ProofRef, "capsrc_") || !strings.Contains(got.ProofRef, "absorb_subset=") {
					t.Fatalf("proof_ref %q missing capsrc id / absorb_subset marker", got.ProofRef)
				}
			default:
				if !strings.Contains(got.ProofDetail, tt.wantDetail) {
					t.Fatalf("proof_detail %q missing %q", got.ProofDetail, tt.wantDetail)
				}
			}
		})
	}
}

// TestOSAbsorptionCapabilityLegacyWholeDirRowUnaffectedBySubsetLogic is the
// explicit backward-compatibility falsifier: a row with no absorb_subset_file
// evidence_refs (e.g. the real os_source_brain_solo_builder_os recipes row)
// must keep proving through the original exact (SourcePath, SourceRevision)
// whole-directory match, completely untouched by the new subset code path.
func TestOSAbsorptionCapabilityLegacyWholeDirRowUnaffectedBySubsetLogic(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	recipesDir := filepath.Join(t.TempDir(), "recipes")
	if err := os.MkdirAll(recipesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Content is irrelevant here: the legacy path matches on (SourcePath,
	// SourceRevision) string equality only and never reads live files.
	revision := "sha256:" + digest12(recipesDir)
	rec, err := capgraph.RecordSourceAuthority(homePath, capgraph.SourceAuthorityInput{
		SourcePath:     recipesDir,
		SourceRevision: revision,
		ImportedVia:    "test import",
		NodeIDs:        []string{"procedure:example"},
		EvidenceRefs:   []string{"fixture:recipes"},
	}, now)
	if err != nil {
		t.Fatalf("record cap source authority: %v", err)
	}

	matrixPath := filepath.Join(t.TempDir(), "matrix.json")
	r := row("os_source_brain_solo_builder_os", recipesDir, "absorbed", capgraph.CapabilityGraphAuthorityRef, "compound capability authority", osAbsorptionProofModeCapability)
	r.SourceRevision = revision
	writeJSONFixture(t, matrixPath, OSAbsorptionSourceMatrix{
		SchemaVersion: OSAbsorptionMatrixSchema,
		Rows:          []OSAbsorptionMatrixRow{r},
	})
	if _, err := ImportOSAbsorptionMatrix(homePath, matrixPath, now); err != nil {
		t.Fatalf("import matrix: %v", err)
	}

	doctor, err := OSAbsorptionDoctor(homePath)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	got, ok := findOSAbsorptionDoctorRow(doctor.Rows, "os_source_brain_solo_builder_os")
	if !ok {
		t.Fatalf("doctor row missing: %+v", doctor.Rows)
	}
	if got.ProofStatus != "ok" || got.ProofRef != rec.ID {
		t.Fatalf("legacy whole-dir row must prove via exact (SourcePath, SourceRevision) match with proof_ref==record id, unaffected by subset logic: %+v (want proof_ref=%s)", got, rec.ID)
	}
	if strings.Contains(got.ProofRef, "absorb_subset=") {
		t.Fatalf("legacy row must never carry an absorb_subset marker: %+v", got)
	}
}

type capabilitySubsetFixture struct {
	compoundHome     string
	sourceDir        string
	ethosPath        string
	instinctsPath    string
	portfolioMapPath string
}

func installCapabilitySubsetFixture(t *testing.T) capabilitySubsetFixture {
	t.Helper()
	root := t.TempDir()
	compoundHome := filepath.Join(root, "compound-home")
	sourceDir := filepath.Join(root, "solo-builder-os")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source dir: %v", err)
	}

	ethosPath := filepath.Join(sourceDir, "ETHOS.md")
	instinctsPath := filepath.Join(sourceDir, "instincts.md")
	portfolioMapPath := filepath.Join(sourceDir, "PORTFOLIO-MAP.md")
	ethosBody := []byte("# ETHOS\n\nHow to approach a task.\n")
	instinctsBody := []byte("# instincts\n\nINSTINCT-001: check upstream freshness.\n")
	writeCapabilitySubsetFile(t, ethosPath, ethosBody)
	writeCapabilitySubsetFile(t, instinctsPath, instinctsBody)
	// Out-of-row files: churn here must never be read by the subset proof.
	writeCapabilitySubsetFile(t, portfolioMapPath, []byte("# PORTFOLIO-MAP\n\nchurns daily, never a capability node.\n"))
	writeCapabilitySubsetFile(t, filepath.Join(sourceDir, "COHERENCE-CHARTER.md"), []byte("# CHARTER\n\nevidence-only, not in the declared subset.\n"))

	ethosHash := digestBytes(ethosBody)
	instinctsHash := digestBytes(instinctsBody)
	subsetRefs := []string{
		"absorb_subset_file:ETHOS.md:sha256:" + ethosHash,
		"absorb_subset_file:instincts.md:sha256:" + instinctsHash,
		"brain_git:03a9c7e",
		"manifest:docs_private/instructions/solo-builder-doctrine.yaml",
	}

	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	rec, err := capgraph.RecordSourceAuthority(compoundHome, capgraph.SourceAuthorityInput{
		SourcePath:     sourceDir,
		SourceRevision: "doctrine-subset-sha256:" + ethosHash + "+" + instinctsHash,
		ImportedVia:    "compound capability import-skills",
		NodeIDs:        []string{"instruction_rule:three-layers-of-knowledge", "instruction_rule:upstream-artifact-freshness-check"},
		EvidenceRefs:   subsetRefs,
	}, now)
	if err != nil {
		t.Fatalf("record cap source authority: %v", err)
	}

	matrixPath := filepath.Join(root, "matrix.json")
	r := row("os_source_brain_solo_builder_doctrine", sourceDir, "pending", capgraph.CapabilityGraphAuthorityRef, "compound orient constitution/doctrine nodes", osAbsorptionProofModeCapability)
	// Deliberately does not match the capsrc's doctrine-subset-sha256 revision:
	// this row's source_revision is a whole-tree pin (evidence only), while the
	// subset proof matches on the declared absorb_subset_file evidence, not on
	// row.SourceRevision.
	r.SourceRevision = "git:03a9c7e"
	r.EvidenceRefs = append([]string{"source:brain", rec.ID}, subsetRefs...)
	writeJSONFixture(t, matrixPath, OSAbsorptionSourceMatrix{
		SchemaVersion: OSAbsorptionMatrixSchema,
		Rows:          []OSAbsorptionMatrixRow{r},
	})
	if _, err := ImportOSAbsorptionMatrix(compoundHome, matrixPath, now.Add(time.Minute)); err != nil {
		t.Fatalf("import matrix: %v", err)
	}

	return capabilitySubsetFixture{
		compoundHome:     compoundHome,
		sourceDir:        sourceDir,
		ethosPath:        ethosPath,
		instinctsPath:    instinctsPath,
		portfolioMapPath: portfolioMapPath,
	}
}

func writeCapabilitySubsetFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write capability subset fixture %s: %v", path, err)
	}
}
