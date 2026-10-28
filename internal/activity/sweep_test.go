package activity

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFalsifier7_ByteCapRefusesNoPartialWrite: at/above the byte cap, drain refuses the whole run
// with byte_cap_exceeded and writes nothing new.
func TestFalsifier7_ByteCapRefusesNoPartialWrite(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	cfg := enableSensor(t, h, now)

	// Plant a large file under observed/activity to push recursive size over the cap.
	if err := os.MkdirAll(SegmentsRoot(h), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	big := filepath.Join(SegmentsRoot(h), "2026-06-01.jsonl")
	if err := os.WriteFile(big, make([]byte, ByteCapBytes+1), 0o600); err != nil {
		t.Fatalf("plant big file: %v", err)
	}
	before, _ := os.Stat(big)

	spool := writeSpool(t, drainEvents)
	rep, err := Drain(h, spool, cfg, "work_drain", now)
	if err != nil {
		t.Fatalf("drain err: %v", err)
	}
	if !rep.Refused || rep.Reason != "byte_cap_exceeded" {
		t.Fatalf("cap drain must refuse with byte_cap_exceeded, got %+v", rep)
	}
	if rep.SegmentLinesAdded != 0 {
		t.Fatalf("cap drain must write nothing, added %d", rep.SegmentLinesAdded)
	}
	// The new day's segment must not exist (no partial write).
	if _, statErr := os.Stat(SegmentPath(h, "2026-07-15")); statErr == nil {
		t.Fatal("cap drain wrote a partial segment")
	}
	after, _ := os.Stat(big)
	if before.Size() != after.Size() {
		t.Fatal("cap drain must not truncate existing files")
	}
}

// TestFalsifier7_SweepDeletesOldAndIsIdempotent: sweep removes >30d files, reports reclaimed bytes,
// and a second run reclaims nothing.
func TestFalsifier7_SweepDeletesOldAndIsIdempotent(t *testing.T) {
	h := newTestHome(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

	if err := os.MkdirAll(SegmentsRoot(h), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(SpansRoot(h), 0o755); err != nil {
		t.Fatal(err)
	}
	// Old (>30d): 2026-05-01. Fresh (<30d): 2026-07-10.
	oldSeg := filepath.Join(SegmentsRoot(h), "2026-05-01.jsonl")
	oldSpan := filepath.Join(SpansRoot(h), "2026-05-01.json")
	freshSeg := filepath.Join(SegmentsRoot(h), "2026-07-10.jsonl")
	for _, p := range []string{oldSeg, oldSpan, freshSeg} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := Sweep(h, now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.FilesDeleted != 2 {
		t.Fatalf("sweep should delete the 2 old files, deleted %d (%v)", rep.FilesDeleted, rep.Deleted)
	}
	if rep.BytesReclaimed != 2 {
		t.Fatalf("reclaimed bytes = %d, want 2", rep.BytesReclaimed)
	}
	if _, err := os.Stat(freshSeg); err != nil {
		t.Fatal("sweep must keep the fresh file")
	}
	if _, err := os.Stat(oldSeg); err == nil {
		t.Fatal("sweep must delete the old segment")
	}

	// Idempotent: second run reclaims nothing.
	rep2, err := Sweep(h, now)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if rep2.FilesDeleted != 0 || rep2.BytesReclaimed != 0 {
		t.Fatalf("re-sweep must be a no-op, got %+v", rep2)
	}
}
