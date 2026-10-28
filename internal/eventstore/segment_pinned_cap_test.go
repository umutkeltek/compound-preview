package eventstore

import (
	"testing"
	"time"
)

// The segment size cap is first a hot-path bound: every put in a short-lived
// process parses the family's hot segment before appending, so the cap is the
// ceiling on per-put cost for the agent hook path. 64 MiB let a busy estate's
// PreToolUse hook spend 4.5–9.4s per tool call (measured 2026-08-24) and blow
// the harness's 60s hook timeout under load — hooks died with no output at
// all. This pins the ceiling so it cannot quietly grow back.
func TestDefaultSegmentCapBoundsHotSegmentParse(t *testing.T) {
	if maxBytes > 8<<20 {
		t.Fatalf("default segment cap = %d bytes; must stay <= 8MiB (it bounds the per-put hot-segment parse every hook process pays)", maxBytes)
	}
	if maxBytes <= 0 {
		t.Fatal("default segment cap must be positive")
	}
}

// A busy family must roll to the next seq at the cap, so the hot segment a
// fresh process parses is never the day's whole accumulation.
func TestFamilyRollsAtCap(t *testing.T) {
	clk := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{
		clock:           func() time.Time { return clk },
		segmentMaxBytes: 256, // every few appends rotate to a fresh seq
	})
	closeCompletionGate(t, store)
	for i := 0; i < 12; i++ {
		putRecord(t, store, "rec_cap_"+string(rune('a'+i)), "compound.receipt.v0.1", clk, "v")
	}
	seg, ok, err := store.engine.lastWritten("receipt")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected a segment")
	}
	if seg.seq == 0 {
		t.Fatalf("family did not roll at cap: still on seq 0 (%s)", seg.path)
	}
}
