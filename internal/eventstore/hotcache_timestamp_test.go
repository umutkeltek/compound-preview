package eventstore

import (
	"path/filepath"
	"testing"
	"time"
)

// Supply an identical timestamp directly: the invariant must hold on a coarse
// filesystem even when the test runner's own filesystem has a finer clock.
func TestHotSegmentAppendSameTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hot.jsonl")
	stamp := time.Unix(100, 0)
	old := hotSegmentEntry{path: path, size: 10, mtime: stamp, byID: map[string]parsedFrame{}}
	sharedHotCache.mu.Lock()
	if sharedHotCache.entries == nil {
		sharedHotCache.entries = map[string]hotSegmentEntry{}
	}
	sharedHotCache.entries[filepath.Dir(path)] = old
	sharedHotCache.mu.Unlock()
	t.Cleanup(func() { invalidateHotSegment(path) })
	frame := parsedFrame{Frame: Frame{ID: "tr_tick"}, Offset: 10, End: 20}
	hotSegmentAppend(path, 20, stamp, frame)
	sharedHotCache.mu.Lock()
	got := sharedHotCache.entries[filepath.Dir(path)]
	sharedHotCache.mu.Unlock()
	if got.size != 20 || got.byID["tr_tick"].End != 20 {
		t.Fatalf("known append with unchanged mtime lost: %+v", got)
	}
	if len(old.byID) != 0 {
		t.Fatal("append mutated a map held by an earlier reader")
	}
}

func TestHotSegmentAppendRefusesUnaccountedBytes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset int64
		end    int64
		size   int64
	}{
		{"foreign prefix", 11, 20, 20},
		{"foreign suffix", 10, 20, 21},
		{"empty append", 10, 10, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hot.jsonl")
			stamp := time.Unix(100, 0)
			sharedHotCache.mu.Lock()
			if sharedHotCache.entries == nil {
				sharedHotCache.entries = map[string]hotSegmentEntry{}
			}
			sharedHotCache.entries[filepath.Dir(path)] = hotSegmentEntry{path: path, size: 10, mtime: stamp, byID: map[string]parsedFrame{}}
			sharedHotCache.mu.Unlock()
			t.Cleanup(func() { invalidateHotSegment(path) })
			hotSegmentAppend(path, tc.size, stamp.Add(time.Second), parsedFrame{Frame: Frame{ID: "tr_tick"}, Offset: tc.offset, End: tc.end})
			sharedHotCache.mu.Lock()
			got := sharedHotCache.entries[filepath.Dir(path)]
			sharedHotCache.mu.Unlock()
			if got.size != 10 || len(got.byID) != 0 {
				t.Fatalf("unaccounted append entered memo: %+v", got)
			}
		})
	}
}
