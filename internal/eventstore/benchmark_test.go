package eventstore

import (
	"fmt"
	"testing"
	"time"
)

// BenchmarkPutHotSegmentGrowth measures the hot-path cost of growing one
// family's hot segment via put(). With the warm hot-memo update
// (hotSegmentAppend) and the per-family append lock, each append must stay O(1)
// instead of re-parsing the whole hot segment per put — the O(n²) that made an
// 8MB hook segment a per-tool-call full parse before the 2026-08-08 rework.
func BenchmarkPutHotSegmentGrowth(b *testing.B) {
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	store, err := openWithConfig(b.TempDir(), engineConfig{clock: func() time.Time { return now }})
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		id := fmt.Sprintf("tr_bench_growth_%07d", i)
		if _, err := store.PutJSON(id, eventRecord(id, "compound.transition.v0.1", now, fmt.Sprintf("v%d", i)), "  "); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPutDurableDedup measures the durable hot-segment memo path:
// re-putting the same id+hash must prove the frame still exists without
// reparsing the segment on every retry.
func BenchmarkPutDurableDedup(b *testing.B) {
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	store, err := openWithConfig(b.TempDir(), engineConfig{clock: func() time.Time { return now }})
	if err != nil {
		b.Fatal(err)
	}
	const id = "tr_bench_dedup_0000001"
	if _, err := store.PutJSON(id, eventRecord(id, "compound.transition.v0.1", now, "seed"), "  "); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := store.PutJSONIfAbsent(id, eventRecord(id, "compound.transition.v0.1", now, "seed"), "  "); err != nil {
			b.Fatal(err)
		}
	}
}
