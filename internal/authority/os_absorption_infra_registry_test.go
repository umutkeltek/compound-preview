package authority

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestOSAbsorptionInfraRegistryAuthorityProof(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(t *testing.T, fixture infraRegistryFixture)
		wantStatus string
		wantDetail string
	}{
		{
			name:       "installed registry, ledger record, and live source all agree",
			wantStatus: "ok",
		},
		{
			name: "installed registry deleted fails closed",
			mutate: func(t *testing.T, fixture infraRegistryFixture) {
				t.Helper()
				if err := os.Remove(InfraDatabaseRegistryPath(fixture.compoundHome)); err != nil {
					t.Fatal(err)
				}
			},
			wantStatus: "failed",
			wantDetail: "installed infra-database registry",
		},
		{
			name: "live source drift proves pending, not ok and not a silent failure",
			mutate: func(t *testing.T, fixture infraRegistryFixture) {
				t.Helper()
				writeInfraDatabaseFixtureVariant(t, fixture.sourcePath, "changed")
			},
			wantStatus: "pending",
			wantDetail: "has drifted from imported record",
		},
		{
			name: "row missing source_authority evidence ref fails closed",
			mutate: func(t *testing.T, fixture infraRegistryFixture) {
				t.Helper()
				mutateOSAbsorptionMatrixRow(t, fixture.compoundHome, "os_source_infra_database", func(row *OSAbsorptionMatrixRow) {
					row.EvidenceRefs = []string{InfraDatabaseAuthorityRef}
				})
			},
			wantStatus: "failed",
			wantDetail: "missing source_authority ref",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := installInfraRegistryFixture(t)
			if tt.mutate != nil {
				tt.mutate(t, fixture)
			}

			doctor, err := OSAbsorptionDoctor(fixture.compoundHome)
			if err != nil {
				t.Fatalf("doctor: %v", err)
			}
			got, ok := findOSAbsorptionDoctorRow(doctor.Rows, "os_source_infra_database")
			if !ok {
				t.Fatalf("doctor row os_source_infra_database missing: %+v", doctor.Rows)
			}
			if got.ProofStatus != tt.wantStatus {
				t.Fatalf("proof_status=%q, want %q (detail=%q)", got.ProofStatus, tt.wantStatus, got.ProofDetail)
			}
			switch tt.wantStatus {
			case "ok":
				if !strings.Contains(got.ProofRef, osAbsorptionProofModeInfraRegistry) {
					t.Fatalf("proof_ref %q missing proof mode", got.ProofRef)
				}
			default:
				if !strings.Contains(got.ProofDetail, tt.wantDetail) {
					t.Fatalf("proof_detail %q missing %q", got.ProofDetail, tt.wantDetail)
				}
			}
		})
	}
}

type infraRegistryFixture struct {
	compoundHome string
	sourcePath   string
}

func installInfraRegistryFixture(t *testing.T) infraRegistryFixture {
	t.Helper()
	root := t.TempDir()
	compoundHome := root + "/compound-home"
	sourcePath := root + "/database.yaml"
	writeInfraDatabaseFixture(t, sourcePath)

	report, err := ImportInfraDatabaseRegistry(compoundHome, sourcePath, time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("import infra database: %v", err)
	}

	matrixPath := root + "/matrix.json"
	r := row("os_source_infra_database", sourcePath, "pending", InfraDatabaseAuthorityRef, "compound authority os-absorption-doctor", osAbsorptionProofModeInfraRegistry)
	r.EvidenceRefs = []string{"source:infra-os", InfraDatabaseAuthorityRef, "source_authority:" + report.SourceAuthorityID}
	writeJSONFixture(t, matrixPath, OSAbsorptionSourceMatrix{
		SchemaVersion: OSAbsorptionMatrixSchema,
		Rows:          []OSAbsorptionMatrixRow{r},
	})
	if _, err := ImportOSAbsorptionMatrix(compoundHome, matrixPath, time.Date(2026, 7, 12, 12, 5, 0, 0, time.UTC)); err != nil {
		t.Fatalf("import matrix: %v", err)
	}
	return infraRegistryFixture{compoundHome: compoundHome, sourcePath: sourcePath}
}

func writeInfraDatabaseFixtureVariant(t *testing.T, path, variant string) {
	t.Helper()
	body := strings.Join([]string{
		"authority:",
		"  primary_postgres: example-db.internal",
		"databases:",
		"  ops:",
		"    purpose: controller_jobs",
		"    status: active",
		"  knowledge:",
		"    purpose: crawl_metadata_" + variant,
		"    status: provisioned",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mutateOSAbsorptionMatrixRow loads the installed matrix, mutates one row by
// id, and re-installs it directly (bypassing ImportOSAbsorptionMatrix's
// re-hash/normalize path) so tests can construct an intentionally malformed
// installed row without also having to forge a fresh source file.
func mutateOSAbsorptionMatrixRow(t *testing.T, homePath, id string, mutate func(*OSAbsorptionMatrixRow)) {
	t.Helper()
	matrix, err := LoadOSAbsorptionMatrix(homePath)
	if err != nil {
		t.Fatalf("load matrix: %v", err)
	}
	found := false
	for i := range matrix.Rows {
		if matrix.Rows[i].ID == id {
			mutate(&matrix.Rows[i])
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("matrix row %s not found", id)
	}
	writeJSONFixture(t, OSAbsorptionMatrixPath(homePath), matrix)
}
