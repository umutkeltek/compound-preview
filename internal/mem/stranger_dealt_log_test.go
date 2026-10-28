package mem

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadStrangerDealtLogMissingFileIsEmptyNotError(t *testing.T) {
	homePath := t.TempDir()
	rows, err := ReadStrangerDealtLog(homePath)
	if err != nil {
		t.Fatalf("missing dealt-log must not error: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %v, want empty", rows)
	}
}

func TestAppendStrangerDealtRowThenReadRoundTrips(t *testing.T) {
	homePath := t.TempDir()
	want := []StrangerDealtRow{
		{Ts: "2026-07-17T10:00:00Z", CardID: "stranger:cand_a", CandidateRef: "cand_a", Topic: "topic-a"},
		{Ts: "2026-07-17T11:00:00Z", CardID: "stranger:cand_b", CandidateRef: "cand_b", Topic: "topic-b"},
	}
	for _, row := range want {
		if err := appendStrangerDealtRow(homePath, row); err != nil {
			t.Fatalf("append %+v: %v", row, err)
		}
	}
	got, err := ReadStrangerDealtLog(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestReadStrangerDealtLogCorruptLineErrors(t *testing.T) {
	homePath := t.TempDir()
	logPath := filepath.Join(homePath, filepath.FromSlash(StrangerDealtLogRelPath))
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("{\"ts\":\"2026-07-17T10:00:00Z\"}\nnot json at all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadStrangerDealtLog(homePath); err == nil {
		t.Fatal("corrupt dealt-log line must surface an error so callers fail closed")
	}
}

func TestReadStrangerDealtLogRejectsSemanticallyInvalidRows(t *testing.T) {
	tests := []struct {
		name string
		row  string
	}{
		{name: "missing timestamp", row: `{"card_id":"stranger:cand_a","candidate_ref":"cand_a"}`},
		{name: "invalid timestamp", row: `{"ts":"not-a-time","card_id":"stranger:cand_a","candidate_ref":"cand_a"}`},
		{name: "missing card id", row: `{"ts":"2026-07-17T10:00:00Z","candidate_ref":"cand_a"}`},
		{name: "missing candidate ref", row: `{"ts":"2026-07-17T10:00:00Z","card_id":"stranger:cand_a"}`},
		{name: "mismatched card identity", row: `{"ts":"2026-07-17T10:00:00Z","card_id":"stranger:cand_b","candidate_ref":"cand_a"}`},
		{name: "path shaped candidate ref", row: `{"ts":"2026-07-17T10:00:00Z","card_id":"stranger:../cand_a","candidate_ref":"../cand_a"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := t.TempDir()
			logPath := filepath.Join(homePath, filepath.FromSlash(StrangerDealtLogRelPath))
			if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(logPath, []byte(tt.row+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if rows, err := ReadStrangerDealtLog(homePath); err == nil {
				t.Fatalf("semantically invalid cooldown row was accepted: %+v", rows)
			}
		})
	}
}

func TestAppendStrangerDealtRowWritesOnlyTheOneFile(t *testing.T) {
	homePath := t.TempDir()
	if _, err := Open(homePath); err != nil {
		t.Fatal(err)
	}
	before := snapshotFiles(t, homePath)
	if err := appendStrangerDealtRow(homePath, StrangerDealtRow{Ts: "2026-07-17T10:00:00Z", CardID: "stranger:cand_x", CandidateRef: "cand_x"}); err != nil {
		t.Fatal(err)
	}
	after := snapshotFiles(t, homePath)
	var added []string
	for f := range after {
		if !before[f] {
			added = append(added, f)
		}
	}
	wantPath := filepath.Join(homePath, filepath.FromSlash(StrangerDealtLogRelPath))
	if len(added) != 1 || added[0] != wantPath {
		t.Fatalf("unexpected new files: %v (want exactly [%s])", added, wantPath)
	}
}

func TestFinalizeStrangerDealRevalidatesCandidateState(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	store, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := store.Capture([]Candidate{cand("entity:finalize-state", "status", "pending", "2026-07-17")}, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	context, err := store.NewStrangerDealContext("")
	if err != nil {
		t.Fatal(err)
	}
	preparation, err := store.PrepareStrangerDeal(captured[0].ID, context, now.Add(-2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if preparation == nil {
		t.Fatal("live proposed candidate did not prepare")
	}
	if _, err := store.Adjudicate(captured[0].ID, "reject", "", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	finalized, err := store.FinalizeStrangerDeal(*preparation, now, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if finalized {
		t.Fatal("a candidate rejected after preparation must not finalize a deal")
	}
	rows, err := ReadStrangerDealtLog(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("rejected candidate produced dealt-log rows: %+v", rows)
	}
}

func TestFinalizeStrangerDealRevalidatesCandidateExpiry(t *testing.T) {
	homePath := t.TempDir()
	preparedAt := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	store, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	candidate := cand("entity:finalize-expiry", "status", "pending", "2026-07-17")
	candidate.ExpiresAt = preparedAt.Add(time.Hour).UTC().Format(time.RFC3339)
	captured, err := store.Capture([]Candidate{candidate}, preparedAt)
	if err != nil {
		t.Fatal(err)
	}
	context, err := store.NewStrangerDealContext("")
	if err != nil {
		t.Fatal(err)
	}
	preparation, err := store.PrepareStrangerDeal(captured[0].ID, context, preparedAt)
	if err != nil {
		t.Fatal(err)
	}
	if preparation == nil {
		t.Fatal("candidate did not prepare before expiry")
	}
	finalizeAt := preparedAt.Add(2 * time.Hour)
	finalized, err := store.FinalizeStrangerDeal(*preparation, finalizeAt, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if finalized {
		t.Fatal("a candidate that expired after preparation must not finalize a deal")
	}
	rows, err := ReadStrangerDealtLog(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("expired candidate produced dealt-log rows: %+v", rows)
	}
}
