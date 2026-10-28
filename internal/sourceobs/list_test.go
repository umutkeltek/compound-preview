package sourceobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
)

func TestListSourceObservationsFreshHomeIsEmpty(t *testing.T) {
	report, err := ListSourceObservations(t.TempDir(), tSource)
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts != (SourceObservationListCounts{}) {
		t.Fatalf("fresh-home counts = %+v", report.Counts)
	}
	if report.Items == nil || len(report.Items) != 0 {
		t.Fatalf("fresh-home items = %#v, want non-nil empty slice", report.Items)
	}
	if len(report.Warnings) != 0 {
		t.Fatalf("fresh-home warnings = %v", report.Warnings)
	}
}

func TestListSourceObservationsProjectsAppliedCandidates(t *testing.T) {
	home := t.TempDir()
	obs := validObservation()
	applied, err := Apply(home, obs, tSource)
	if err != nil {
		t.Fatal(err)
	}
	report, err := ListSourceObservations(home, tSource.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != SourceObservationListSchemaVersion || report.AuthorityRef != "compound://source/observations" {
		t.Fatalf("report identity = %+v", report)
	}
	if report.Counts.TotalCandidates != 1 || report.Counts.SourceObservations != 1 || report.Counts.NoPromoteDecisions != 1 || report.Counts.PromoteDisallowed != 1 {
		t.Fatalf("counts = %+v", report.Counts)
	}
	if len(report.Items) != 1 {
		t.Fatalf("items = %+v", report.Items)
	}
	item := report.Items[0]
	if item.SourceObservationID != obs.ID || item.CandidateID != applied.CandidateIDs[0] || item.NoPromoteID != applied.NoPromoteIDs[0] {
		t.Fatalf("item = %+v applied=%+v", item, applied)
	}
}

func TestListSourceObservationsUsesCandidateCustodyAndContinuesAfterCorruption(t *testing.T) {
	home := t.TempDir()
	inbox := filepath.Join(home, candidates.InboxDir(candidates.Memory))
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	writeListCandidateFile(t, filepath.Join(inbox, "cand_blank.json"), `{"id":"","source_observation_id":"obs_blank"}`)
	writeListCandidateFile(t, filepath.Join(inbox, "cand_mismatch.json"), `{"id":"cand_other","source_observation_id":"obs_mismatch"}`)
	writeListCandidateFile(t, filepath.Join(inbox, "scratch.tmp"), `not a canonical candidate`)
	writeListCandidateFile(t, filepath.Join(home, "symlink-target.json"), `{"id":"cand_symlink","source_observation_id":"obs_symlink"}`)
	if err := os.Symlink(filepath.Join(home, "symlink-target.json"), filepath.Join(inbox, "cand_symlink.json")); err != nil {
		t.Fatal(err)
	}
	const validID = "cand_zvalid"
	if err := candidates.WriteCandidate(home, candidates.Memory, validID, CandidateRecord{
		ID:                  validID,
		Kind:                string(candidates.Memory),
		SourceObservationID: "obs_valid",
		CreatedAt:           "2026-08-30T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	report, err := ListSourceObservations(home, tSource)
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.TotalCandidates != 1 || report.Counts.SourceObservations != 1 || len(report.Items) != 1 {
		t.Fatalf("report count/items = %+v/%+v", report.Counts, report.Items)
	}
	if report.Items[0].CandidateID != validID || report.Items[0].SourceObservationID != "obs_valid" {
		t.Fatalf("valid candidate after corruption = %+v", report.Items[0])
	}
	for _, want := range []string{"cand_blank.json corrupt: blank_id", "cand_mismatch.json corrupt: id_mismatch", "cand_symlink.json corrupt: symlink", "scratch.tmp corrupt: invalid_filename"} {
		if !containsListWarning(report.Warnings, want) {
			t.Fatalf("warnings = %v, missing %q", report.Warnings, want)
		}
	}
}

func writeListCandidateFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func containsListWarning(warnings []string, want string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, want) {
			return true
		}
	}
	return false
}
