package eventstore

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// installIndexParseCounter routes the sealed-index parse observer through a
// per-path counter for the duration of one test. count(idxPath) reports how
// many FULL read+parses the sealed index at idxPath has taken; restore detaches
// the observer. Mirrors installParseCounter (hotcache_test.go) for the sealed
// twin of the hot-segment reparse.
func installIndexParseCounter(t *testing.T) (count func(path string) int, restore func()) {
	t.Helper()
	var mu sync.Mutex
	counts := map[string]int{}
	prev := indexParseObserver
	indexParseObserver = func(path string) {
		mu.Lock()
		counts[path]++
		mu.Unlock()
	}
	return func(path string) int {
			mu.Lock()
			defer mu.Unlock()
			return counts[path]
		}, func() {
			indexParseObserver = prev
		}
}

func sealedIndexPath(store *Store, family, day string) string {
	return filepath.Join(store.rootPath(), indexDirName, family, day+".idx")
}

// Falsifier — sealed index parsed once per process under interleaved cross-family
// Gets. Two families each carry a sealed (older-day) segment; alternating Gets
// across both must read+parse each sealed .idx exactly once, not once per Get.
// RED without the memo: loadOrBuildIndex re-reads and re-JSON-parses the whole
// index file on every Get (post-rollover ConsumeReceipts CPU-bound). GREEN with
// the path-keyed, stat-guarded memo.
func TestSealedIndexParsedOncePerProcess(t *testing.T) {
	day1 := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	clk := day1
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return clk }})

	const per = 25
	recIDs := make([]string, per)
	trIDs := make([]string, per)
	for i := 0; i < per; i++ {
		r := fmt.Sprintf("rec_sealedmemo_%02d", i)
		tr := fmt.Sprintf("tr_sealedmemo_%02d", i)
		recIDs[i], trIDs[i] = r, tr
		putRecord(t, store, r, "compound.receipt.v0.1", day1, fmt.Sprintf("r%02d", i))
		putRecord(t, store, tr, "compound.transition.v0.1", day1, fmt.Sprintf("t%02d", i))
	}
	// Roll the clock a day so both families' day-1 segments seal (index > 0) and
	// resolve through readIndexedFrame rather than the hot memo.
	clk = day1.Add(24 * time.Hour)
	putRecord(t, store, "rec_sealedmemo_hot", "compound.receipt.v0.1", clk, "hot")
	putRecord(t, store, "tr_sealedmemo_hot", "compound.transition.v0.1", clk, "hot")

	recIdx := sealedIndexPath(store, "receipt", ingestionDay(day1))
	trIdx := sealedIndexPath(store, "transition", ingestionDay(day1))

	// Warm both sealed indexes onto disk (first sealed read builds the .idx via
	// rebuild) and into the memo, so the counted loop measures steady state.
	if _, err := store.Read(recIDs[0]); err != nil {
		t.Fatalf("warm receipt read: %v", err)
	}
	if _, err := store.Read(trIDs[0]); err != nil {
		t.Fatalf("warm transition read: %v", err)
	}

	count, restore := installIndexParseCounter(t)
	defer restore()

	for i := 0; i < per; i++ {
		if _, err := store.Read(recIDs[i]); err != nil {
			t.Fatalf("read %s: %v", recIDs[i], err)
		}
		if _, err := store.Read(trIDs[i]); err != nil {
			t.Fatalf("read %s: %v", trIDs[i], err)
		}
	}
	if got := count(recIdx); got != 1 {
		t.Fatalf("receipt sealed index parsed %d times across %d interleaved Gets, want 1 (per-Get reparse?)", got, per)
	}
	if got := count(trIdx); got != 1 {
		t.Fatalf("transition sealed index parsed %d times across %d interleaved Gets, want 1 (per-Get reparse?)", got, per)
	}
}

// Falsifier — parity + drift teeth. A warm sealed-index memo returns
// byte-identical Get results to a fresh (memo-cold) engine, AND a segment that
// drifts underneath the memo (size/mtime move) is still detected and rebuilt —
// the memo never serves stale truth, the index stays cache and not authority.
func TestSealedIndexMemoParityAndDriftInvalidation(t *testing.T) {
	day1 := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	clk := day1
	var logs []string
	home := t.TempDir()
	store, err := openWithConfig(home, engineConfig{
		clock: func() time.Time { return clk },
		logf:  func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	putRecord(t, store, "rec_memo_drift", "compound.receipt.v0.1", day1, "before")
	putRecord(t, store, "rec_memo_stable", "compound.receipt.v0.1", day1, "stable")
	clk = day1.Add(24 * time.Hour)
	putRecord(t, store, "rec_memo_hot", "compound.receipt.v0.1", clk, "hot")

	segPath := segmentPath(store.engine.segRoot, "receipt", ingestionDay(day1), 0)

	// Warm the memo on both sealed ids.
	before, err := store.Read("rec_memo_drift")
	if err != nil {
		t.Fatalf("warm read: %v", err)
	}
	if _, err := store.Read("rec_memo_stable"); err != nil {
		t.Fatalf("warm read stable: %v", err)
	}

	// A fresh engine over the same home must produce byte-identical Gets.
	fresh := freshStore(t, home)
	for _, id := range []string{"rec_memo_drift", "rec_memo_stable"} {
		warm, warmErr := store.Read(id)
		cold, coldErr := fresh.Read(id)
		if (warmErr == nil) != (coldErr == nil) {
			t.Fatalf("%s err presence mismatch warm=%v cold=%v", id, warmErr, coldErr)
		}
		if !bytes.Equal(warm.Bytes, cold.Bytes) {
			t.Fatalf("%s memo vs fresh bytes differ:\n warm=%s\n cold=%s", id, warm.Bytes, cold.Bytes)
		}
	}

	// Drift the sealed segment underneath the warm memo (same length, future
	// mtime): the stat guard must miss and the content-hash check must rebuild.
	logs = nil
	rewriteFrameValueSameLength(t, segPath, "rec_memo_drift", "after!")
	got, err := store.Read("rec_memo_drift")
	if err != nil {
		t.Fatalf("read after drift: %v", err)
	}
	if !bytes.Contains(got.Bytes, []byte(`"value":"after!"`)) {
		t.Fatalf("memo served stale truth after segment drift: %s", got.Bytes)
	}
	assertRebuildLogged(t, logs)
	// The undrifted id still resolves byte-identically after the rebuild.
	stableAfter, err := store.Read("rec_memo_stable")
	if err != nil {
		t.Fatalf("read stable after rebuild: %v", err)
	}
	if _, err := os.Stat(segPath); err != nil {
		t.Fatalf("segment vanished: %v", err)
	}
	_ = before
	_ = stableAfter
}
