package main

import (
	"encoding/json"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/sourceobs"
)

func TestSourceObservationsJSONCommand(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	rec := sourceobs.CandidateRecord{
		SchemaVersion:       sourceobs.CandidateSchemaVersion,
		ID:                  "src_cand_cli",
		Kind:                string(candidates.CapabilityObs),
		Subject:             "projection",
		Statement:           "projection needs review",
		SourceObservationID: "srcobs_cli",
		SourceRef:           "projection://cli",
		Verdict:             candidates.NoPromote,
		PromoteAllowed:      false,
		CreatedAt:           time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
		EvidenceRefs:        []string{"source:projection://cli#sha256:abc"},
	}
	if err := candidates.WriteCandidate(homePath, candidates.CapabilityObs, rec.ID, rec); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdSource([]string{"observations", "--json"})
	})
	var report sourceobs.SourceObservationListReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse source observations json: %v\n%s", err, out)
	}
	if report.SchemaVersion != sourceobs.SourceObservationListSchemaVersion || report.Counts.SourceObservations != 1 {
		t.Fatalf("report = %+v", report)
	}
	if report.Items[0].SourceObservationID != "srcobs_cli" || report.Items[0].CandidateID != "src_cand_cli" {
		t.Fatalf("item = %+v", report.Items[0])
	}
}
