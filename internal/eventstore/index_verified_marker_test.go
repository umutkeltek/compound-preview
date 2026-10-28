package eventstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func openRootedStore(t *testing.T, root string, clock func() time.Time) *Store {
	t.Helper()
	store, err := openWithConfig(root, engineConfig{clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// Falsifier 1 — the memo works ACROSS processes: the first fresh store to
// load a sealed segment's index hashes the segment once and writes the
// .verified marker; a second fresh store (empty in-process caches) resolves
// the same record with ZERO content hashes. Per-tool-call hook processes are
// exactly that second store, and the re-hash was the dominant cost of every
// PreToolUse hook on a busy estate (measured 2026-08-24: 3.5–9.4s per tool
// call; past the harness kill timeout under load, hooks died with no output).
func TestVerifiedMarkerSkipsSealedRehashAcrossStores(t *testing.T) {
	root := t.TempDir()
	clk := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	store := openRootedStore(t, root, func() time.Time { return clk })
	closeCompletionGate(t, store)
	putRecord(t, store, "rec_vm_1", "compound.receipt.v0.1", clk, "v1")
	putRecord(t, store, "rec_vm_2", "compound.receipt.v0.1", clk, "v2")

	// Day 2 makes day 1 a sealed (non-hot) segment.
	clk = clk.Add(26 * time.Hour)
	putRecord(t, store, "rec_vm_3", "compound.receipt.v0.1", clk, "v3")

	// The sealed day-1 index is built lazily by the first reader. From that
	// first load on, the marker must make every LATER fresh store skip the
	// content hash entirely — fresh processes are exactly the per-tool-call
	// hook processes this memo exists for. A rebuild hashes via contentHash
	// (not sourceHash) and writes the marker, so zero sourceHash across all
	// three stores is the invariant.
	idxPath := filepath.Join(root, "events", "index", "receipt", "2026-08-23.idx")
	prev := sourceHashObserver
	for i := 0; i < 3; i++ {
		var hashes atomic.Int64
		sourceHashObserver = func(string) { hashes.Add(1) }
		fresh := openRootedStore(t, root, func() time.Time { return clk })
		closeCompletionGate(t, fresh)
		if _, err := fresh.Read("rec_vm_1"); err != nil {
			t.Fatalf("fresh store %d read: %v", i+1, err)
		}
		if got := hashes.Load(); got != 0 {
			t.Fatalf("fresh store %d rehashed %d segment(s); the .verified memo must make this zero", i+1, got)
		}
		// The memoized answer is still CORRECT: the record bytes round-trip.
		rec, err := fresh.Read("rec_vm_2")
		if err != nil {
			t.Fatalf("fresh store %d read vm_2: %v", i+1, err)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Bytes, &body); err != nil {
			t.Fatalf("record bytes unreadable: %v", err)
		}
		if body["id"] != "rec_vm_2" {
			t.Fatalf("memoized lookup returned the wrong record: %v", body["id"])
		}
	}
	sourceHashObserver = prev
	if _, err := os.Stat(idxPath + ".verified"); err != nil {
		t.Fatalf("verified marker must exist after the first lazy load: %v", err)
	}
}

// Falsifier 2 — drift keeps its teeth: rewriting a sealed segment (same
// size, new mtime) invalidates the marker; revalidation re-runs and the
// stale proof never survives.
func TestVerifiedMarkerDoesNotMaskSegmentDrift(t *testing.T) {
	root := t.TempDir()
	clk := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	store := openRootedStore(t, root, func() time.Time { return clk })
	closeCompletionGate(t, store)
	putRecord(t, store, "rec_drift_1", "compound.receipt.v0.1", clk, "v1")
	clk = clk.Add(26 * time.Hour)
	putRecord(t, store, "rec_drift_2", "compound.receipt.v0.1", clk, "v2")

	fresh := openRootedStore(t, root, func() time.Time { return clk })
	closeCompletionGate(t, fresh)
	if _, err := fresh.Read("rec_drift_1"); err != nil {
		t.Fatalf("pre-tamper read: %v", err)
	}

	// Tamper: rewrite the sealed day-1 segment with same-size different
	// bytes and a NEW mtime (a rewrite cannot keep the old mtime).
	segPath := filepath.Join(root, "events", "segments", "receipt", "2026-08-23.jsonl")
	data, err := os.ReadFile(segPath)
	if err != nil {
		t.Fatal(err)
	}
	tampered := make([]byte, len(data))
	for i, b := range data {
		tampered[i] = b ^ 0x01
	}
	if err := os.WriteFile(segPath, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	newTime := time.Now().Add(time.Hour)
	if err := os.Chtimes(segPath, newTime, newTime); err != nil {
		t.Fatal(err)
	}

	var hashes atomic.Int64
	prev := sourceHashObserver
	sourceHashObserver = func(string) { hashes.Add(1) }
	after := openRootedStore(t, root, func() time.Time { return clk })
	closeCompletionGate(t, after)
	_, _ = after.Read("rec_drift_1") // drift handling (rebuild/error) is the existing contract
	sourceHashObserver = prev

	if hashes.Load() == 0 {
		t.Fatal("drift must force at least one content hash (revalidation re-ran)")
	}
	// A stale marker left on disk by a failed rebuild is inert by
	// construction — it can never match the tampered segment's stats again —
	// but prove that directly: a THIRD store must also rehash rather than
	// trusting the old proof.
	var hashes3 atomic.Int64
	sourceHashObserver = func(string) { hashes3.Add(1) }
	third := openRootedStore(t, root, func() time.Time { return clk })
	closeCompletionGate(t, third)
	_, _ = third.Read("rec_drift_1")
	sourceHashObserver = prev
	if hashes3.Load() == 0 {
		t.Fatal("a third store must not trust the stale marker: drift state must keep forcing full validation")
	}
}

// Falsifier 3 — a rebuilt index refreshes the proof: after a repair rebuild,
// the marker reflects the NEW index stats, so a later store still skips the
// hash legitimately.
func TestVerifiedMarkerRefreshesAfterIndexRebuild(t *testing.T) {
	root := t.TempDir()
	clk := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	store := openRootedStore(t, root, func() time.Time { return clk })
	closeCompletionGate(t, store)
	putRecord(t, store, "rec_rb_1", "compound.receipt.v0.1", clk, "v1")
	clk = clk.Add(26 * time.Hour)
	putRecord(t, store, "rec_rb_2", "compound.receipt.v0.1", clk, "v2")

	// Build the sealed index first (indexes load lazily on first read).
	warm := openRootedStore(t, root, func() time.Time { return clk })
	closeCompletionGate(t, warm)
	if _, err := warm.Read("rec_rb_1"); err != nil {
		t.Fatalf("warm read: %v", err)
	}
	// Force a rebuild by removing the .idx.
	idxPath := filepath.Join(root, "events", "index", "receipt", "2026-08-23.idx")
	if err := os.Remove(idxPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(idxPath + ".verified"); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	fresh := openRootedStore(t, root, func() time.Time { return clk })
	closeCompletionGate(t, fresh)
	if _, err := fresh.Read("rec_rb_1"); err != nil {
		t.Fatalf("read after index removal: %v", err)
	}
	if _, err := os.Stat(idxPath); err != nil {
		t.Fatalf("index must have been rebuilt: %v", err)
	}
	if _, err := os.Stat(idxPath + ".verified"); err != nil {
		t.Fatalf("rebuild must refresh the verified marker: %v", err)
	}
}
