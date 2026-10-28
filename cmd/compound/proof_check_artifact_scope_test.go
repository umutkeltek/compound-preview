package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProofCheckArtifactScopeTruthTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		dryRun    bool
		status    string
		ciOnly    bool
		ciConsole bool
		want      bool
	}{
		{"full green battery records", false, "ok", false, false, true},
		{"red battery does not record", false, "failed", false, false, false},
		{"dry run does not record", true, "ok", false, false, false},
		{"ci-only green does NOT record", false, "ok", true, false, false},
		{"ci-console green does NOT record", false, "ok", false, true, false},
		{"ci-only red does not record", false, "failed", true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := proofCheckShouldRecordArtifact(tc.dryRun, tc.status, tc.ciOnly, tc.ciConsole)
			if got != tc.want {
				t.Fatalf("proofCheckShouldRecordArtifact(dryRun=%v, status=%q, ciOnly=%v, ciConsole=%v) = %v, want %v",
					tc.dryRun, tc.status, tc.ciOnly, tc.ciConsole, got, tc.want)
			}
		})
	}
}

// This is the falsifier that shows WHY the ci clauses in proofCheckShouldRecordArtifact are a
// correctness fix rather than housekeeping.
//
// recordProofCheckRuntimeArtifact writes its four "adjacent Workbench action verified" lines
// unconditionally (proof_check.go:301-305), with no reference to which gates the report actually
// contains. A `--ci` run executes only the 8 fresh-clone-portable gates — `workbench-projection`
// and `codex-pretooluse-native-gate` are NOT among them. So before the guard, a green `--ci`
// minted an artifact claiming both had been verified, and the artifact's own closing line tells
// the consumer not to rerun them. A proof that certifies work nobody did, and then suppresses the
// check that would have caught it.
//
// This test pins the writer's unconditional behaviour so that if someone later makes the body
// conditional on the steps present, they are forced to revisit the guard rather than silently
// making it redundant.
func TestProofCheckArtifactBodyClaimsGatesTheCIReportNeverRan(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()

	// A report shaped exactly like a green `--ci` run: only portable gates, no workbench and no
	// native hook gate.
	ciReport := proofCheckReport{
		SchemaVersion: proofCheckSchemaVersion,
		Status:        "ok",
		GeneratedAt:   "2026-07-26T12:00:00Z",
		CIOnly:        true,
		Steps: []proofCheckStep{
			{Name: "go-test", ProofRef: "go-test-count-1-all", Status: "ok"},
			{Name: "go-vet", ProofRef: "go-vet-all", Status: "ok"},
			{Name: "go-build", ProofRef: "go-build-all", Status: "ok"},
		},
	}

	for _, step := range ciReport.Steps {
		if step.Name == "workbench-projection" || step.Name == "codex-pretooluse-native-gate" {
			t.Fatalf("fixture is wrong: a --ci report must not contain %q", step.Name)
		}
	}

	written, err := recordProofCheckRuntimeArtifact(homePath, ciReport)
	if err != nil {
		t.Fatalf("record artifact: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(homePath, "artifacts", "refs",
		"proof-check-installed-runtime-20260726.md"))
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	text := string(body)

	// The writer claims both gates regardless of the report's contents. If this ever stops being
	// true the guard's rationale changes and this test should be revisited deliberately.
	for _, claim := range []string{"workbench-projection-refresh", "codex-pretooluse-native-gate-proof"} {
		if !strings.Contains(text, claim) {
			t.Fatalf("expected the unconditional artifact body to claim %q; if the body became "+
				"step-aware, revisit proofCheckShouldRecordArtifact's rationale", claim)
		}
	}
	if written.ArtifactRef == "" {
		t.Fatal("recording an artifact must set ArtifactRef")
	}

	// ...which is exactly why the guard must refuse to call it for a --ci run at all.
	if proofCheckShouldRecordArtifact(false, ciReport.Status, ciReport.CIOnly, ciReport.CIConsole) {
		t.Fatal("a green --ci run must not be authorized to write the certifying runtime artifact")
	}
}

// The development lane has its own, independent refusal inside the writer (proof_check.go:282).
// Both layers should hold: the guard never authorizes it, and the writer refuses even if called.
func TestProofCheckDevelopmentProfileCannotRecordCanonicalArtifact(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	devReport := proofCheckReport{
		Status:      "ok",
		GeneratedAt: "2026-07-26T12:00:00Z",
		ProfileKind: proofCheckProfileDevelopment,
		Steps: []proofCheckStep{
			{Name: "go-test", ProofRef: "development:go-test-short-cached-all", Status: "ok"},
		},
	}
	if _, err := recordProofCheckRuntimeArtifact(homePath, devReport); err == nil {
		t.Fatal("the development profile must be refused the canonical installed-runtime artifact")
	}
	if _, err := os.Stat(filepath.Join(homePath, "artifacts", "refs")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(homePath, "artifacts", "refs"))
		if len(entries) > 0 {
			t.Fatalf("a refused development record must leave no artifact behind, found %d", len(entries))
		}
	}
}
