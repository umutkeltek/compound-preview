package runattempt

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestListFreshlyAggregatesNewCorruptionAfterSuccessfulRead(t *testing.T) {
	home := t.TempDir()
	writer := Open(home)
	first, err := writer.Mint("work_first", "test", "os-harness-entrypoints", time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	second, err := writer.Mint("work_second", "test", "os-harness-entrypoints", time.Date(2026, 10, 14, 0, 0, 1, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	reader := Open(home)
	if got, err := reader.List(); err != nil || len(got) != 2 {
		t.Fatalf("initial List = %d attempts, %v; want 2 valid attempts", len(got), err)
	}
	for _, id := range []string{first.ID, second.ID} {
		if err := os.WriteFile(filepath.Join(home, "run_attempts", id+".json"), []byte("{broken"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := reader.List()
	if len(got) != 0 {
		t.Fatalf("corrupt attempts surfaced as readable: %d", len(got))
	}
	var unreadable *UnreadableRunAttemptsError
	if !errors.As(err, &unreadable) {
		t.Fatalf("List error = %v; want aggregate unreadable error", err)
	}
	want := []string{first.ID, second.ID}
	sort.Strings(want)
	if ids := UnreadableRunAttemptIDs(err); !reflect.DeepEqual(ids, want) {
		t.Fatalf("unreadable ids = %v; want %v", ids, want)
	}
}

func TestListInvalidatesCachedGetAfterExternalChangeOrCorruption(t *testing.T) {
	home := t.TempDir()
	writer := Open(home)
	createdAt := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	attempt, err := writer.Mint("work_cache", "test", "os-harness-entrypoints", createdAt)
	if err != nil {
		t.Fatal(err)
	}
	reader := Open(home)
	if before, err := reader.Get(attempt.ID); err != nil || before.Status != StatusPending {
		t.Fatalf("initial Get status = %q, %v; want pending", before.Status, err)
	}
	if _, err := writer.Complete(attempt.ID, StatusSucceeded, createdAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	listed, err := reader.List()
	if err != nil || len(listed) != 1 || listed[0].Status != StatusSucceeded {
		t.Fatalf("List after external completion = %+v, %v; want succeeded", listed, err)
	}
	if after, err := reader.Get(attempt.ID); err != nil || after.Status != StatusSucceeded {
		t.Fatalf("Get after List status = %q, %v; want succeeded", after.Status, err)
	}
	if err := os.WriteFile(filepath.Join(home, "run_attempts", attempt.ID+".json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.List(); err == nil {
		t.Fatal("List accepted corrupt record")
	}
	if _, err := reader.Get(attempt.ID); err == nil {
		t.Fatal("Get resurrected cached record after List found corruption")
	}
}
