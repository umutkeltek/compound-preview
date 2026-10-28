package mem

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/candidates"
)

func TestAdjudicatePendingReusesPageRecordsWithoutPointReads(t *testing.T) {
	const (
		candidateCount = 257
		scanLimit      = 31
	)
	store := newStore(t)
	if err := os.MkdirAll(store.candDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < candidateCount; index++ {
		id := fmt.Sprintf("batch-%04d", index)
		body, err := json.Marshal(Candidate{
			ID:         id,
			Entity:     "entity:" + id,
			Predicate:  "batch-custody",
			Object:     id,
			State:      "proposed",
			Visibility: "candidate",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(store.candDir(), id+".json"), append(body, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	pointReads := 0
	store.candidatePointReader = func(id string) (Candidate, error) {
		pointReads++
		var candidate Candidate
		err := candidates.ReadCandidate(store.home, candidates.Memory, id, &candidate)
		return candidate, err
	}

	var cursor candidates.EntryCursor
	changed := map[string]bool{}
	for pass := 0; pass < 16; pass++ {
		report := store.AdjudicatePending(AdjudicatePendingOptions{
			AfterCursor: cursor,
			ScanLimit:   scanLimit,
		}, time.Date(2026, 8, 29, 12, pass, 0, 0, time.UTC))
		if len(report.Errors) != 0 {
			t.Fatalf("pass %d errors = %+v", pass, report.Errors)
		}
		for _, id := range report.ChangedIDs {
			if changed[id] {
				t.Fatalf("candidate %s changed twice", id)
			}
			changed[id] = true
		}
		if report.NextCursor == "" {
			break
		}
		if report.NextCursor <= cursor {
			t.Fatalf("cursor did not advance: before=%q after=%q", cursor, report.NextCursor)
		}
		cursor = report.NextCursor
	}
	if len(changed) != candidateCount {
		t.Fatalf("changed = %d, want %d", len(changed), candidateCount)
	}
	if pointReads != 0 {
		t.Fatalf("point reads = %d, want 0; batch must reuse descriptor-rooted page records", pointReads)
	}
}
