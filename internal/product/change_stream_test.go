package product

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestApplyChangeStreamCandidatesReportsFailureWhenRecordingFails(t *testing.T) {
	home := t.TempDir()
	ledgerPath := filepath.Join(home, "ledger")
	if err := os.MkdirAll(filepath.Join(ledgerPath, "no-promote-decisions.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := ChangeStreamDocument{
		SourceRef:  "change_stream_candidates:test",
		SourceHash: "sha256:test",
		Candidates: []ChangeStreamCandidate{{
			ID:           "pcs_test",
			ProductID:    "product:test",
			Surface:      "test surface",
			Summary:      "exercise the write failure path",
			RepoPath:     "/tmp/test",
			RepoBranch:   "main",
			DirtyPreview: []string{"internal/product/change_stream.go"},
		}},
	}

	report, err := ApplyChangeStreamCandidates(home, doc, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("expected candidate recording to fail")
	}
	if report.Status != "fail" {
		t.Fatalf("report status = %q, want fail when recording returns an error", report.Status)
	}
}
