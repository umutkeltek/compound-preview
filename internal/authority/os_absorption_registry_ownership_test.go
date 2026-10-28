package authority

import (
	"os"
	"strings"
	"testing"
	"time"

	"compound/internal/policy"
)

// TestOSAbsorptionRegistryOwnershipAuthorityProof exercises the generic
// "registry_ownership_authority" proof mode reused across all seven
// already-live compound_primary registry rows added in this change. It uses
// os_source_job_roster as the concrete subject because JobRosterAuthority
// lives in this same package, so the test needs no cross-package fixture
// plumbing; the proof function itself (registryOwnershipProof) is identical
// for every row id in registryOwnershipSpecs.
func TestOSAbsorptionRegistryOwnershipAuthorityProof(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(t *testing.T, homePath string)
		wantStatus string
		wantDetail string
	}{
		{
			name:       "installed registry loads through its own authority command",
			wantStatus: "ok",
		},
		{
			name: "installed registry file deleted fails closed (ownership, not just health)",
			mutate: func(t *testing.T, homePath string) {
				t.Helper()
				if err := os.Remove(JobRosterAuthorityPath(homePath)); err != nil {
					t.Fatal(err)
				}
			},
			wantStatus: "failed",
			wantDetail: "installed registry file",
		},
		{
			name: "installed registry file corrupted fails closed",
			mutate: func(t *testing.T, homePath string) {
				t.Helper()
				if err := os.WriteFile(JobRosterAuthorityPath(homePath), []byte("not valid json"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantStatus: "failed",
			wantDetail: "authority command",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := installRegistryOwnershipFixture(t)
			if tt.mutate != nil {
				tt.mutate(t, homePath)
			}

			doctor, err := OSAbsorptionDoctor(homePath)
			if err != nil {
				t.Fatalf("doctor: %v", err)
			}
			got, ok := findOSAbsorptionDoctorRow(doctor.Rows, "os_source_job_roster")
			if !ok {
				t.Fatalf("doctor row os_source_job_roster missing: %+v", doctor.Rows)
			}
			if got.ProofStatus != tt.wantStatus {
				t.Fatalf("proof_status=%q, want %q (detail=%q)", got.ProofStatus, tt.wantStatus, got.ProofDetail)
			}
			if tt.wantStatus == "ok" {
				if !strings.Contains(got.ProofRef, osAbsorptionProofModeRegistryOwnership) {
					t.Fatalf("proof_ref %q missing proof mode", got.ProofRef)
				}
				return
			}
			if !strings.Contains(got.ProofDetail, tt.wantDetail) {
				t.Fatalf("proof_detail %q missing %q", got.ProofDetail, tt.wantDetail)
			}
		})
	}
}

// TestOSAbsorptionRegistryOwnershipProofIsOwnershipNotHealth proves the
// falsifier the envelope calls out explicitly: an installed registry whose
// own projected content reports a degraded/failed status must still
// machine-prove ownership. Health and ownership are different axes, and the
// doctor must not conflate them.
func TestOSAbsorptionRegistryOwnershipProofIsOwnershipNotHealth(t *testing.T) {
	homePath := installRegistryOwnershipFixture(t)

	doctor, err := OSAbsorptionDoctor(homePath)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	got, ok := findOSAbsorptionDoctorRow(doctor.Rows, "os_source_operator_approval_frontier")
	if !ok {
		t.Fatalf("doctor row os_source_operator_approval_frontier missing: %+v", doctor.Rows)
	}
	if got.ProofStatus != "ok" {
		t.Fatalf("a degraded-but-well-formed frontier should still machine-prove ownership, got proof_status=%q detail=%q", got.ProofStatus, got.ProofDetail)
	}

	frontier, err := policy.LoadOperatorApprovalFrontierFromHome(homePath)
	if err != nil {
		t.Fatalf("load frontier: %v", err)
	}
	if frontier.Status != "operator_approval_frontier_failed" {
		t.Fatalf("fixture frontier status = %q, want the degraded status this test exercises", frontier.Status)
	}
}

func installRegistryOwnershipFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	homePath := root + "/compound-home"

	// os_source_job_roster subject used by the table test above.
	jobsSource := root + "/cron-jobs.json"
	writeJobRosterFixture(t, jobsSource)
	if _, err := ImportJobRoster(homePath, jobsSource, time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("import job roster: %v", err)
	}

	// os_source_operator_approval_frontier subject used by the
	// ownership-vs-health test: a *degraded* but well-formed frontier
	// (status=operator_approval_frontier_failed) must still load cleanly.
	frontierSource := root + "/operator-approval-frontier-summary.json"
	writeDegradedOperatorApprovalFrontierFixture(t, frontierSource)
	if _, err := policy.ImportOperatorApprovalFrontier(homePath, frontierSource, time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("import operator approval frontier: %v", err)
	}

	matrixPath := root + "/matrix.json"
	rows := []OSAbsorptionMatrixRow{
		row("os_source_job_roster", jobsSource, "absorbed", JobRosterAuthorityRef, "compound authority job-roster-authority", osAbsorptionProofModeRegistryOwnership),
		row("os_source_operator_approval_frontier", frontierSource, "absorbed", policy.OperatorApprovalFrontierAuthorityRef, "compound policy operator-approval-frontier-authority", osAbsorptionProofModeRegistryOwnership),
	}
	writeJSONFixture(t, matrixPath, OSAbsorptionSourceMatrix{
		SchemaVersion: OSAbsorptionMatrixSchema,
		Rows:          rows,
	})
	if _, err := ImportOSAbsorptionMatrix(homePath, matrixPath, time.Date(2026, 7, 12, 12, 5, 0, 0, time.UTC)); err != nil {
		t.Fatalf("import matrix: %v", err)
	}
	return homePath
}

func writeDegradedOperatorApprovalFrontierFixture(t *testing.T, path string) {
	t.Helper()
	body := `{
  "schema_version": 1,
  "generated_at": "2026-07-12T00:00:00Z",
  "run_id": "run_degraded_fixture",
  "status": "operator_approval_frontier_failed",
  "families": []
}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
