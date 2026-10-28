package eventstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The hot-segment memo must never serve stale truth: an append moves the
// segment's size/mtime, and the very next point read must see the new record.
// This fails if hotFrames trusts its memo without the stat guard — and it
// also fails if the memo path cannot find records at all (the cache must be
// a pure accelerator, invisible to Get semantics).
func TestHotSegmentCacheServesFreshAppends(t *testing.T) {
	store := openTestStore(t)

	first := mustPutTestRecord(t, store, "tr_hotcache00001")
	rec, err := store.Read("tr_hotcache00001")
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	assertSameRecord(t, rec.Bytes, first)

	// Append a second record: size changes, memo must invalidate.
	second := mustPutTestRecord(t, store, "tr_hotcache00002")
	rec2, err := store.Read("tr_hotcache00002")
	if err != nil {
		t.Fatalf("get after append must see the new record (stale memo?): %v", err)
	}
	assertSameRecord(t, rec2.Bytes, second)

	// And the first record still resolves through the refreshed memo.
	recAgain, err := store.Read("tr_hotcache00001")
	if err != nil {
		t.Fatalf("re-get of first record: %v", err)
	}
	assertSameRecord(t, recAgain.Bytes, first)
}

func mustPutTestRecord(t *testing.T, store *Store, id string) []byte {
	t.Helper()
	record, err := json.Marshal(map[string]string{
		"id":             id,
		"schema_version": "compound.transition.v0.1",
		"created_at":     "2026-07-16T00:00:00Z",
		"note":           "hotcache falsifier " + id,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON(id, json.RawMessage(record), ""); err != nil {
		t.Fatalf("put %s: %v", id, err)
	}
	return record
}

func assertSameRecord(t *testing.T, got []byte, want []byte) {
	t.Helper()
	var g, w map[string]any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	if g["id"] != w["id"] || g["note"] != w["note"] {
		t.Fatalf("record mismatch: got %v want %v", g, w)
	}
}

// Interleaved point reads across FAMILIES must each keep their memo: a proof
// chain reads act_/pol_/rec_/tr_ in sequence per item, and a single-slot
// cache thrashes to a 100% miss rate on that pattern (the live 158s
// regression). This fails if hotFrames evicts one family's memo when another
// family is read.
func TestHotSegmentCacheSurvivesCrossFamilyInterleaving(t *testing.T) {
	store := openTestStore(t)
	mustPutTestRecord(t, store, "tr_interleave001")
	if _, err := store.PutJSON("pol_interleave01", json.RawMessage(`{"id":"pol_interleave01","schema_version":"compound.policy_decision.v0.1","created_at":"2026-07-16T00:00:00Z"}`), ""); err != nil {
		t.Fatal(err)
	}
	// Prime both family memos.
	if _, err := store.Read("tr_interleave001"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read("pol_interleave01"); err != nil {
		t.Fatal(err)
	}
	eng := store.engine
	live := liveHotEntriesUnder(eng.segRoot)
	if live < 2 {
		t.Fatalf("cross-family reads must keep one memo entry per family, got %d live entries (single-slot thrash)", live)
	}
}

// liveHotEntriesUnder counts sharedHotCache entries belonging to one store's
// segment root. The cache is package-level (keys are absolute family dirs), so
// bounded-size assertions must scope to their own home — the per-family
// eviction property they guard is unchanged.
func liveHotEntriesUnder(segRoot string) int {
	sharedHotCache.mu.Lock()
	defer sharedHotCache.mu.Unlock()
	live := 0
	for key := range sharedHotCache.entries {
		if strings.HasPrefix(key, segRoot+string(os.PathSeparator)) {
			live++
		}
	}
	return live
}

// --- salvaged from hotcache-repair-20260716 (parallel land superseded by
// 6aa94840; parse-count exactness is the added value over the two tests
// above, which prove correctness but not call-count) ---

// installParseCounter routes the hot-segment parse observer through a
// per-path counter for the duration of one test. count(path) reports how
// many FULL parses the hot segment at path has taken; restore detaches the
// observer.
func installParseCounter(t *testing.T) (count func(path string) int, restore func()) {
	t.Helper()
	var mu sync.Mutex
	counts := map[string]int{}
	prev := segmentParseObserver
	segmentParseObserver = func(path string) {
		mu.Lock()
		counts[path]++
		mu.Unlock()
	}
	return func(path string) int {
			mu.Lock()
			defer mu.Unlock()
			return counts[path]
		}, func() {
			segmentParseObserver = prev
		}
}

func hotSegmentPath(store *Store, family, day string) string {
	return filepath.Join(store.engine.segRoot, family, day+".jsonl")
}

func frameLine(t *testing.T, id, schema string, createdAt time.Time, value string) []byte {
	t.Helper()
	body, err := json.Marshal(eventRecord(id, schema, createdAt, value))
	if err != nil {
		t.Fatal(err)
	}
	frame, _, err := buildFrame(id, body)
	if err != nil {
		t.Fatalf("build frame %s: %v", id, err)
	}
	line, err := encodeFrame(frame)
	if err != nil {
		t.Fatalf("encode frame %s: %v", id, err)
	}
	return line
}

func closeCompletionGate(t *testing.T, store *Store) {
	t.Helper()
	if err := os.MkdirAll(store.engine.migrationRoot(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.engine.completionPath(), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func freshStore(t *testing.T, home string) *Store {
	t.Helper()
	store, err := Open(home)
	if err != nil {
		t.Fatalf("open fresh store: %v", err)
	}
	return store
}

// Falsifier a — parse-once. 50 puts then 50 Gets against one hot segment
// full-parse it exactly once: the first put cold-parses (empty file → one
// parse), and the warm-memo update keeps every later put AND read served from
// the in-process memo with zero further parses. This pins the improvement over
// the pre-2026-08-08 behavior where every append invalidated the memo and the
// next read re-parsed the whole hot segment per put.
func TestHotSegmentParsedOncePerProcess(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	hot := hotSegmentPath(store, "transition", "2026-07-15")

	count, restore := installParseCounter(t)
	defer restore()

	const records = 50
	ids := make([]string, records)
	for i := 0; i < records; i++ {
		id := fmt.Sprintf("tr_parseonce_%02d", i)
		ids[i] = id
		putRecord(t, store, id, "compound.transition.v0.1", now, fmt.Sprintf("v%02d", i))
	}
	for _, id := range ids {
		if _, err := store.Read(id); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
	}
	if got := count(hot); got != 1 {
		t.Fatalf("hot segment parsed %d times across %d puts + %d Gets, want 1 (one cold parse)", got, records, records)
	}
}

// Falsifier b — interleave-thrash. Alternating Gets across two families' hot
// segments parse each exactly once (2 total), not once per family switch.
// Adds call-count exactness on top of
// TestHotSegmentCacheSurvivesCrossFamilyInterleaving's live-entry-count proof.
func TestHotSegmentInterleaveDoesNotThrash(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	hotA := hotSegmentPath(store, "transition", "2026-07-15")
	hotB := hotSegmentPath(store, "receipt", "2026-07-15")

	count, restore := installParseCounter(t)
	defer restore()

	const per = 25
	aIDs := make([]string, per)
	bIDs := make([]string, per)
	for i := 0; i < per; i++ {
		a := fmt.Sprintf("tr_thrash_%02d", i)
		b := fmt.Sprintf("rec_thrash_%02d", i)
		aIDs[i], bIDs[i] = a, b
		putRecord(t, store, a, "compound.transition.v0.1", now, fmt.Sprintf("a%02d", i))
		putRecord(t, store, b, "compound.receipt.v0.1", now, fmt.Sprintf("b%02d", i))
	}
	for i := 0; i < per; i++ {
		if _, err := store.Read(aIDs[i]); err != nil {
			t.Fatalf("read %s: %v", aIDs[i], err)
		}
		if _, err := store.Read(bIDs[i]); err != nil {
			t.Fatalf("read %s: %v", bIDs[i], err)
		}
	}
	if got := count(hotA); got != 1 {
		t.Fatalf("family A hot parsed %d times under interleave, want 1 (one cold parse)", got)
	}
	if got := count(hotB); got != 1 {
		t.Fatalf("family B hot parsed %d times under interleave, want 1 (one cold parse)", got)
	}
}

// Falsifier c — warm-on-own-append, reparse-on-foreign-append. A warm memo is
// NOT discarded when THIS process appends (hotSegmentAppend updates it in place
// under the family lock — the pre-2026-08-08 behavior reparsed the whole hot
// segment on every own append, the O(n²) that made a hook-event write a
// per-tool-call full parse). A FOREIGN writer's append still moves the stat
// tuple the memo cannot account for, so exactly one reparse occurs and the
// appended id is found while prior ids stay byte-correct.
func TestHotSegmentStalenessReparsesOnAppend(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	oldID := "tr_stale_old"
	putRecord(t, store, oldID, "compound.transition.v0.1", now, "old")
	hot := hotSegmentPath(store, "transition", "2026-07-15")

	count, restore := installParseCounter(t)
	defer restore()

	first, err := store.Read(oldID)
	if err != nil {
		t.Fatalf("warm read: %v", err)
	}
	if got := count(hot); got != 1 {
		t.Fatalf("parses after warm read = %d, want 1", got)
	}
	if _, err := store.Read(oldID); err != nil {
		t.Fatalf("second warm read: %v", err)
	}
	if got := count(hot); got != 1 {
		t.Fatalf("parses after memo hit = %d, want 1 (served from memo)", got)
	}

	// Own-process append keeps the memo warm in place: the appended id is
	// found with NO reparse (count stays 1), and the prior id stays intact.
	newID := "tr_stale_new"
	putRecord(t, store, newID, "compound.transition.v0.1", now, "new")
	got, err := store.Read(newID)
	if err != nil {
		t.Fatalf("read appended id after own append: %v", err)
	}
	if !bytes.Contains(got.Bytes, []byte(`"value": "new"`)) && !bytes.Contains(got.Bytes, []byte(`"value":"new"`)) {
		t.Fatalf("appended record body wrong: %s", got.Bytes)
	}
	if c := count(hot); c != 1 {
		t.Fatalf("parses after own append + read = %d, want 1 (memo kept warm in place, no reparse)", c)
	}
	if again, err := store.Read(oldID); err != nil {
		t.Fatalf("re-read prior id: %v", err)
	} else if !bytes.Equal(first.Bytes, again.Bytes) {
		t.Fatalf("prior id bytes drifted after warm own-append:\n before=%s\n after =%s", first.Bytes, again.Bytes)
	}

	// Foreign append (direct file write, not through this store) moves the
	// stat tuple the memo cannot account for → exactly one reparse, and both
	// the foreign id and the prior ids are correct afterward.
	foreignLine := frameLine(t, "tr_foreign", "compound.transition.v0.1", now, "foreign")
	f, err := os.OpenFile(hot, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open hot segment for foreign append: %v", err)
	}
	if _, err := f.Write(foreignLine); err != nil {
		_ = f.Close()
		t.Fatalf("foreign append: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	fr, err := store.Read("tr_foreign")
	if err != nil {
		t.Fatalf("read foreign-appended id: %v", err)
	}
	if !bytes.Contains(fr.Bytes, []byte(`"value": "foreign"`)) && !bytes.Contains(fr.Bytes, []byte(`"value":"foreign"`)) {
		t.Fatalf("foreign record body wrong: %s", fr.Bytes)
	}
	if c := count(hot); c != 2 {
		t.Fatalf("parses after foreign append + read = %d, want 2 (exactly one reparse for foreign interference)", c)
	}
	if again, err := store.Read(oldID); err != nil {
		t.Fatalf("re-read prior id after foreign append: %v", err)
	} else if !bytes.Equal(first.Bytes, again.Bytes) {
		t.Fatalf("prior id bytes drifted after foreign reparse:\n before=%s\n after =%s", first.Bytes, again.Bytes)
	}
}

// Falsifier d — regular bypass / no poisoning. ReadRegular full-parses the hot
// segment per call and neither populates nor serves the memo; interleaving
// Read and ReadRegular yields byte-identical records.
func TestRegularReadBypassesHotMemo(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	id := "tr_regular"
	putRecord(t, store, id, "compound.transition.v0.1", now, "x")
	hot := hotSegmentPath(store, "transition", "2026-07-15")

	count, restore := installParseCounter(t)
	defer restore()

	reg1, err := store.ReadRegular(id, "event record")
	if err != nil {
		t.Fatalf("first ReadRegular: %v", err)
	}
	if _, err := store.ReadRegular(id, "event record"); err != nil {
		t.Fatalf("second ReadRegular: %v", err)
	}
	if c := count(hot); c != 2 {
		t.Fatalf("ReadRegular parses = %d, want 2 (per call, never memoized)", c)
	}

	// First memoized Read must be a MISS — ReadRegular never populated the memo.
	memo1, err := store.Read(id)
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if c := count(hot); c != 3 {
		t.Fatalf("parses after first Read = %d, want 3 (memo not populated by ReadRegular)", c)
	}
	if _, err := store.Read(id); err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if c := count(hot); c != 3 {
		t.Fatalf("parses after memo hit = %d, want 3 (served from memo)", c)
	}

	// A subsequent ReadRegular must still bypass the now-warm memo.
	if _, err := store.ReadRegular(id, "event record"); err != nil {
		t.Fatalf("third ReadRegular: %v", err)
	}
	if c := count(hot); c != 4 {
		t.Fatalf("parses after post-warm ReadRegular = %d, want 4 (regular never serves memo)", c)
	}

	if !bytes.Equal(reg1.Bytes, memo1.Bytes) {
		t.Fatalf("regular vs memoized bytes differ:\n regular=%s\n memo   =%s", reg1.Bytes, memo1.Bytes)
	}
}

// Falsifier e — parity. Across a hot segment with duplicate-id occurrences, a
// sealed segment, and an absent id under a closed migration gate, results
// (bytes, offset, relPath, error text) are byte-identical whether served
// from a warm memo or a fresh (memo-cold) engine. Newest-occurrence-wins is
// preserved.
func TestHotMemoParityWithFullParse(t *testing.T) {
	home := t.TempDir()
	day14 := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	day15 := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	clock := day14
	store, err := openWithConfig(home, engineConfig{clock: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}

	// Sealed segment (older day) holds tr_sealed.
	putRecord(t, store, "tr_sealed", "compound.transition.v0.1", day14, "sealed")

	// Hot segment (newer day), hand-written with a DUPLICATE id: tr_dup appears
	// twice (A then B); newest-occurrence-wins must resolve to B.
	clock = day15
	hot := hotSegmentPath(store, "transition", "2026-07-15")
	var body []byte
	body = append(body, frameLine(t, "tr_dup", "compound.transition.v0.1", day15, "A")...)
	body = append(body, frameLine(t, "tr_dup", "compound.transition.v0.1", day15, "B")...)
	body = append(body, frameLine(t, "tr_other", "compound.transition.v0.1", day15, "C")...)
	if err := os.WriteFile(hot, body, 0o644); err != nil {
		t.Fatal(err)
	}

	closeCompletionGate(t, store)

	// Warm the memo store on every id it will serve.
	for _, id := range []string{"tr_dup", "tr_other", "tr_sealed"} {
		if _, err := store.Read(id); err != nil {
			t.Fatalf("warm read %s: %v", id, err)
		}
	}

	// Newest-occurrence-wins: tr_dup resolves to B, never A.
	dup, err := store.Read("tr_dup")
	if err != nil {
		t.Fatalf("read tr_dup: %v", err)
	}
	var decoded struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(dup.Bytes, &decoded); err != nil {
		t.Fatalf("decode tr_dup: %v", err)
	}
	if decoded.Value != "B" {
		t.Fatalf("tr_dup resolved to value %q, want newest occurrence B", decoded.Value)
	}

	for _, id := range []string{"tr_dup", "tr_other", "tr_sealed"} {
		fresh := freshStore(t, home)
		warmHit, warmFound, warmErr := store.engine.segmentFrame(id, "")
		freshHit, freshFound, freshErr := fresh.engine.segmentFrame(id, "")
		if warmErr != nil || freshErr != nil {
			t.Fatalf("%s segmentFrame err: warm=%v fresh=%v", id, warmErr, freshErr)
		}
		if warmFound != freshFound || !warmFound {
			t.Fatalf("%s found mismatch: warm=%v fresh=%v", id, warmFound, freshFound)
		}
		if warmHit.offset != freshHit.offset {
			t.Fatalf("%s offset mismatch: warm=%d fresh=%d", id, warmHit.offset, freshHit.offset)
		}
		if warmHit.relPath != freshHit.relPath {
			t.Fatalf("%s relPath mismatch: warm=%q fresh=%q", id, warmHit.relPath, freshHit.relPath)
		}
		if !bytes.Equal(warmHit.frame.Record, freshHit.frame.Record) {
			t.Fatalf("%s record bytes mismatch:\n warm =%s\n fresh=%s", id, warmHit.frame.Record, freshHit.frame.Record)
		}

		warmRec, warmReadErr := store.Read(id)
		freshRec, freshReadErr := fresh.Read(id)
		if (warmReadErr == nil) != (freshReadErr == nil) {
			t.Fatalf("%s Read err presence mismatch: warm=%v fresh=%v", id, warmReadErr, freshReadErr)
		}
		if !bytes.Equal(warmRec.Bytes, freshRec.Bytes) {
			t.Fatalf("%s Read bytes mismatch:\n warm =%s\n fresh=%s", id, warmRec.Bytes, freshRec.Bytes)
		}
	}

	// Absent id under the closed gate: identical migration-complete error text.
	warmRec, warmErr := store.Read("tr_absent")
	freshRec, freshErr := freshStore(t, home).Read("tr_absent")
	if warmErr == nil || freshErr == nil {
		t.Fatalf("absent id must error: warm=%v fresh=%v (%q/%q)", warmErr, freshErr, warmRec.Bytes, freshRec.Bytes)
	}
	if warmErr.Error() != freshErr.Error() {
		t.Fatalf("absent-id error text mismatch:\n warm =%q\n fresh=%q", warmErr.Error(), freshErr.Error())
	}
	if !bytes.Contains([]byte(warmErr.Error()), []byte("migration complete")) {
		t.Fatalf("absent-id error missing migration-complete wording: %q", warmErr.Error())
	}
}

// Falsifier f — foreign-writer visibility. A frame appended to the hot
// segment by a direct file write (not through the engine) is seen by the
// next Get, because the stat guard — also the cross-process mechanism —
// reparses on the size/mtime move.
func TestHotSegmentForeignAppendVisible(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	seed := "tr_foreign_seed"
	putRecord(t, store, seed, "compound.transition.v0.1", now, "seed")
	hot := hotSegmentPath(store, "transition", "2026-07-15")

	// Warm the memo before the foreign write.
	if _, err := store.Read(seed); err != nil {
		t.Fatalf("warm read: %v", err)
	}

	foreign := "tr_foreign_appended"
	line := frameLine(t, foreign, "compound.transition.v0.1", now, "foreign")
	f, err := os.OpenFile(hot, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := store.Read(foreign)
	if err != nil {
		t.Fatalf("foreign-appended record not visible after warm memo: %v", err)
	}
	if !bytes.Contains(got.Bytes, []byte("foreign")) {
		t.Fatalf("foreign record body wrong: %s", got.Bytes)
	}
	if _, err := store.Read(seed); err != nil {
		t.Fatalf("seed record lost after reparse: %v", err)
	}
}

// Falsifier g — rollover eviction. A long-lived process crossing a day (or seq)
// rollover must not strand the prior segment's parsed frames. The memo is keyed
// by family DIRECTORY, so the new day's hot segment REPLACES the prior day's
// entry — one live entry per family, always. Without eviction the entry map
// grows by one dead ~day-of-frames entry per family per rollover (the console
// supervisor RSS creep noted in the hot-memo salvage follow-up). RED on the
// path-keyed base (two entries survive the rollover); GREEN with dir keying.
// Reads stay correct across the boundary.
func TestHotSegmentMemoEvictsPriorDayOnRollover(t *testing.T) {
	day1 := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	clk := day1
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return clk }})

	putRecord(t, store, "tr_rollover_day1", "compound.transition.v0.1", day1, "d1")
	// Warm the day-1 hot memo while day 1 is the hot segment.
	d1, err := store.Read("tr_rollover_day1")
	if err != nil {
		t.Fatalf("day-1 warm read: %v", err)
	}

	// Cross the day rollover: the new segment becomes hot, day 1 seals.
	clk = day1.Add(24 * time.Hour)
	putRecord(t, store, "tr_rollover_day2", "compound.transition.v0.1", clk, "d2")
	if _, err := store.Read("tr_rollover_day2"); err != nil {
		t.Fatalf("day-2 read: %v", err)
	}

	eng := store.engine
	live := liveHotEntriesUnder(eng.segRoot)
	if live != 1 {
		t.Fatalf("hot memo holds %d entries for one family after a day rollover, want 1 (prior-day entry not evicted -> unbounded RSS creep)", live)
	}

	// Reads stay correct across the boundary: the day-1 id now resolves through
	// the sealed index, the day-2 id through the fresh hot memo.
	d1again, err := store.Read("tr_rollover_day1")
	if err != nil {
		t.Fatalf("re-read day-1 after rollover: %v", err)
	}
	if !bytes.Equal(d1.Bytes, d1again.Bytes) {
		t.Fatalf("day-1 bytes drifted across rollover:\n before=%s\n after =%s", d1.Bytes, d1again.Bytes)
	}
	d2, err := store.Read("tr_rollover_day2")
	if err != nil {
		t.Fatalf("re-read day-2: %v", err)
	}
	if !bytes.Contains(d2.Bytes, []byte(`"value":"d2"`)) && !bytes.Contains(d2.Bytes, []byte(`"value": "d2"`)) {
		t.Fatalf("day-2 record body wrong: %s", d2.Bytes)
	}

	// A within-day seq rotation also evicts: force a new seq segment and confirm
	// the family still holds exactly one entry.
	clk = day1.Add(24 * time.Hour)
	rotated := segmentPath(eng.segRoot, "transition", ingestionDay(clk), 1)
	line := frameLine(t, "tr_rollover_seq", "compound.transition.v0.1", clk, "seq")
	if err := os.WriteFile(rotated, line, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read("tr_rollover_seq"); err != nil {
		t.Fatalf("read seq-rotated record: %v", err)
	}
	liveAfterSeq := liveHotEntriesUnder(eng.segRoot)
	if liveAfterSeq != 1 {
		t.Fatalf("hot memo holds %d entries after a seq rotation, want 1", liveAfterSeq)
	}
}
