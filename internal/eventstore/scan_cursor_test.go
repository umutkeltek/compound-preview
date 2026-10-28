package eventstore

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

func entryIDs(entries []Entry) []string {
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	sort.Strings(ids)
	return ids
}

// Falsifier (a): the incremental delta plus the already-consumed prefix equals a
// full rescan of the same data, and a second run returns EXACTLY the records
// appended since the cursor — no more, no fewer. Red if ScanFrom returned the
// full set every time (delta==full would fail) or dropped a new record.
func TestScanFromDeltaEqualsFullRescan(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	closeCompletionGate(t, store)

	for i := 0; i < 5; i++ {
		putRecord(t, store, fmt.Sprintf("rec_a%d", i), "compound.receipt.v0.1", now, "v")
	}

	full, err := store.Scan(ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	page1, err := store.ScanFrom(ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(entryIDs(page1.Entries), entryIDs(full)) {
		t.Fatalf("first ScanFrom = %v, want full scan %v", entryIDs(page1.Entries), entryIDs(full))
	}
	if page1.Cursor == "" {
		t.Fatal("segment-authoritative ScanFrom must issue a non-empty cursor")
	}

	for i := 0; i < 3; i++ {
		putRecord(t, store, fmt.Sprintf("rec_b%d", i), "compound.receipt.v0.1", now, "v")
	}
	page2, err := store.ScanFrom(ScanOptions{Cursor: page1.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	wantNew := []string{"rec_b0", "rec_b1", "rec_b2"}
	if !reflect.DeepEqual(entryIDs(page2.Entries), wantNew) {
		t.Fatalf("delta = %v, want only the appended %v", entryIDs(page2.Entries), wantNew)
	}

	// The union of what the two incremental runs returned must equal a fresh full
	// rescan of all eight records: incremental consumption loses nothing.
	fullAfter, err := store.Scan(ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	union := append(entryIDs(page1.Entries), entryIDs(page2.Entries)...)
	sort.Strings(union)
	if !reflect.DeepEqual(union, entryIDs(fullAfter)) {
		t.Fatalf("incremental union %v != full rescan %v", union, entryIDs(fullAfter))
	}

	// A third run with the advanced cursor returns nothing new.
	page3, err := store.ScanFrom(ScanOptions{Cursor: page2.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(page3.Entries) != 0 {
		t.Fatalf("steady-state ScanFrom returned %d entries, want 0", len(page3.Entries))
	}
}

// Falsifier (b): the counting proof. A second consecutive run decodes ONLY the
// frames appended since the cursor, while the full Scan base re-decodes
// everything. Structural (frame count via the parse observer), not wall-clock.
// Red-first: if scanFrom ignored the cursor, incParsed would equal baseParsed.
func TestScanFromParsesOnlyFramesSinceCursor(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	closeCompletionGate(t, store)

	const seeded = 6
	for i := 0; i < seeded; i++ {
		putRecord(t, store, fmt.Sprintf("rec_c%d", i), "compound.receipt.v0.1", now, "v")
	}

	var parsed int
	scanSegmentObserver = func(_ string, frames int, _ int64) { parsed += frames }
	defer func() { scanSegmentObserver = nil }()

	parsed = 0
	page1, err := store.ScanFrom(ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if parsed != seeded {
		t.Fatalf("first full ScanFrom parsed %d frames, want %d", parsed, seeded)
	}

	const appended = 2
	for i := 0; i < appended; i++ {
		putRecord(t, store, fmt.Sprintf("rec_d%d", i), "compound.receipt.v0.1", now, "v")
	}

	parsed = 0
	if _, err := store.Scan(ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	baseParsed := parsed // full rescan re-decodes every frame

	parsed = 0
	page2, err := store.ScanFrom(ScanOptions{Cursor: page1.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	incParsed := parsed

	if incParsed != appended {
		t.Fatalf("incremental run decoded %d frames, want only the %d new-since-cursor", incParsed, appended)
	}
	if baseParsed != seeded+appended {
		t.Fatalf("base full Scan decoded %d frames, want %d", baseParsed, seeded+appended)
	}
	if incParsed >= baseParsed {
		t.Fatalf("counting falsifier has no teeth: incremental=%d base=%d", incParsed, baseParsed)
	}
	if len(page2.Entries) != appended {
		t.Fatalf("delta returned %d entries, want %d", len(page2.Entries), appended)
	}
}

// Falsifier (c): a lost, garbage, or non-boundary cursor CONVERGES to the full
// rescan answer — a cache miss can only cost work, never correctness. Red if any
// unrecoverable cursor produced a subset.
func TestScanFromStaleCursorConvergesToFullRescan(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	closeCompletionGate(t, store)

	const seeded = 6
	for i := 0; i < seeded; i++ {
		putRecord(t, store, fmt.Sprintf("rec_e%d", i), "compound.receipt.v0.1", now, "v")
	}
	full, err := store.Scan(ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := entryIDs(full)

	// (i) empty cursor after data exists → full set.
	// (ii) structurally garbage (non-base64) cursor → full set.
	// (iii) a valid token whose offset lands MID-frame (not a boundary) → the
	//       segment reparses whole, still the full set.
	rel := filepath.Join(segmentsDirName, "receipt", ingestionDay(now)+".jsonl")
	nonBoundary := encodeScanCursor(scanCursor{Version: scanCursorVersion, Segments: map[string]int64{rel: 3}})
	pastEOF := encodeScanCursor(scanCursor{Version: scanCursorVersion, Segments: map[string]int64{rel: 1 << 40}})

	for name, cursor := range map[string]string{
		"empty":        "",
		"garbage":      "!!!not base64!!!",
		"non-boundary": nonBoundary,
		"past-eof":     pastEOF,
	} {
		page, err := store.ScanFrom(ScanOptions{Cursor: cursor})
		if err != nil {
			t.Fatalf("%s cursor: %v", name, err)
		}
		if !reflect.DeepEqual(entryIDs(page.Entries), want) {
			t.Fatalf("%s cursor converged to %v, want full %v", name, entryIDs(page.Entries), want)
		}
	}
}

// Falsifier (d): a cursor survives a rotation (seq++) and a day-rollover — new
// segment files absent from the cursor parse in full exactly once, and no
// already-consumed segment is re-parsed. segmentMaxBytes=1 forces every record
// into its own segment file; advancing the clock forces a new day segment.
func TestScanFromCrossesRotationAndDayBoundary(t *testing.T) {
	clk := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{
		clock:           func() time.Time { return clk },
		segmentMaxBytes: 1, // every append rotates to a fresh seq
	})
	closeCompletionGate(t, store)

	putRecord(t, store, "rec_r0", "compound.receipt.v0.1", clk, "v")
	putRecord(t, store, "rec_r1", "compound.receipt.v0.1", clk, "v") // forces seq 1
	page1, err := store.ScanFrom(ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(entryIDs(page1.Entries), []string{"rec_r0", "rec_r1"}) {
		t.Fatalf("baseline delta = %v", entryIDs(page1.Entries))
	}

	putRecord(t, store, "rec_r2", "compound.receipt.v0.1", clk, "v") // new seq file, same day
	clk = clk.AddDate(0, 0, 1)
	putRecord(t, store, "rec_s0", "compound.receipt.v0.1", clk, "v") // new day file

	var parsed int
	scanSegmentObserver = func(_ string, frames int, _ int64) { parsed += frames }
	defer func() { scanSegmentObserver = nil }()

	parsed = 0
	page2, err := store.ScanFrom(ScanOptions{Cursor: page1.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(entryIDs(page2.Entries), []string{"rec_r2", "rec_s0"}) {
		t.Fatalf("cross-boundary delta = %v, want [rec_r2 rec_s0]", entryIDs(page2.Entries))
	}
	if parsed != 2 {
		t.Fatalf("crossing rotation+day parsed %d frames, want only the 2 new segments", parsed)
	}

	page3, err := store.ScanFrom(ScanOptions{Cursor: page2.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(page3.Entries) != 0 {
		t.Fatalf("steady state after boundary crossing returned %d, want 0", len(page3.Entries))
	}
}

// The loose-fallback (pre-migration) home never issues a cursor, so a consumer
// there always does a correct full scan. Guards the documented ScanOptions
// contract and the safety of un-migrated homes.
func TestScanFromLooseFallbackIssuesNoCursor(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	// completion gate intentionally left OPEN
	for _, id := range []string{"rec_l0", "rec_l1"} {
		writeLegacyRecord(t, store, id, eventRecord(id, "compound.receipt.v0.1", now, "loose-"+id))
	}
	page, err := store.ScanFrom(ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Cursor != "" {
		t.Fatalf("loose-fallback ScanFrom issued cursor %q, want empty", page.Cursor)
	}
	if !reflect.DeepEqual(entryIDs(page.Entries), []string{"rec_l0", "rec_l1"}) {
		t.Fatalf("loose-fallback delta = %v, want full loose set", entryIDs(page.Entries))
	}
	if page.Resumed {
		t.Fatal("loose-fallback ScanFrom must report Resumed=false (legacy delegation, not a real resume)")
	}
}

// Falsifier F3 (packet 2026-07-16-b1-backdated-reconcile-fold): ScanPage.Resumed
// distinguishes a real segment-authoritative resume from every path that still
// returns a correct answer but is NOT a precise delta a caller may trust for
// skipping its own dedup filter — empty cursor, structurally garbage cursor,
// version-mismatched cursor, and legacy-fallback delegation must all report
// false; only a usable cursor consumed on the segment-authoritative path may
// report true.
func TestScanFromResumedReportsUsableCursorOnly(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	closeCompletionGate(t, store)

	for i := 0; i < 3; i++ {
		putRecord(t, store, fmt.Sprintf("rec_f%d", i), "compound.receipt.v0.1", now, "v")
	}

	// (a) empty cursor (first-ever scan) -> false.
	first, err := store.ScanFrom(ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Resumed {
		t.Fatal("empty cursor must report Resumed=false")
	}
	if first.Cursor == "" {
		t.Fatal("segment-authoritative ScanFrom must still issue a non-empty cursor")
	}

	// (b) usable cursor from the prior call, consumed on the segment path -> true.
	putRecord(t, store, "rec_f3", "compound.receipt.v0.1", now, "v")
	second, err := store.ScanFrom(ScanOptions{Cursor: first.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Resumed {
		t.Fatal("a usable cursor on the segment-authoritative path must report Resumed=true")
	}

	// (c) structurally garbage (non-base64) cursor -> false.
	garbage, err := store.ScanFrom(ScanOptions{Cursor: "!!!not base64!!!"})
	if err != nil {
		t.Fatal(err)
	}
	if garbage.Resumed {
		t.Fatal("garbage cursor must report Resumed=false")
	}

	// (d) version-mismatched cursor -> false. Built by hand: encodeScanCursor
	// always stamps the CURRENT version, so it cannot produce a mismatch.
	staleBytes, err := json.Marshal(scanCursor{Version: scanCursorVersion + 1, Segments: map[string]int64{}})
	if err != nil {
		t.Fatal(err)
	}
	mismatched := base64.RawURLEncoding.EncodeToString(staleBytes)
	versionMismatch, err := store.ScanFrom(ScanOptions{Cursor: mismatched})
	if err != nil {
		t.Fatal(err)
	}
	if versionMismatch.Resumed {
		t.Fatal("version-mismatched cursor must report Resumed=false")
	}

	// (e) legacy-fallback delegation (migration gate open) -> false, even with a
	// structurally usable cursor in hand.
	legacyStore := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	for _, id := range []string{"rec_g0", "rec_g1"} {
		writeLegacyRecord(t, legacyStore, id, eventRecord(id, "compound.receipt.v0.1", now, "loose-"+id))
	}
	legacy, err := legacyStore.ScanFrom(ScanOptions{Cursor: first.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Resumed {
		t.Fatal("legacy-fallback ScanFrom must report Resumed=false regardless of cursor shape")
	}
}

// Falsifier F4: a STRUCTURALLY VALID cursor whose offset forces a whole-segment
// reparse (mid-frame offset, past-EOF offset, or a segment replaced by a
// shorter file) returns already-consumed frames — a superset, not a delta — so
// it MUST report Resumed=false, or a caller that skips its own dedup filter on
// Resumed=true (mem reconcile does) double-folds every re-served event. Red if
// any hazard-degraded page still claims Resumed=true.
func TestScanFromResumedReportsFalseOnHazardDegradedResume(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	closeCompletionGate(t, store)

	const seeded = 6
	for i := 0; i < seeded; i++ {
		putRecord(t, store, fmt.Sprintf("rec_h%d", i), "compound.receipt.v0.1", now, "v")
	}
	clean, err := store.ScanFrom(ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}

	rel := filepath.Join(segmentsDirName, "receipt", ingestionDay(now)+".jsonl")
	nonBoundary := encodeScanCursor(scanCursor{Version: scanCursorVersion, Segments: map[string]int64{rel: 3}})
	pastEOF := encodeScanCursor(scanCursor{Version: scanCursorVersion, Segments: map[string]int64{rel: 1 << 40}})

	for name, cursor := range map[string]string{
		"non-boundary": nonBoundary,
		"past-eof":     pastEOF,
	} {
		page, err := store.ScanFrom(ScanOptions{Cursor: cursor})
		if err != nil {
			t.Fatalf("%s cursor: %v", name, err)
		}
		if len(page.Entries) == 0 {
			t.Fatalf("%s cursor returned no entries; fixture no longer exercises the reparse path", name)
		}
		if page.Resumed {
			t.Fatalf("%s cursor degraded to a whole-segment reparse (a superset page) but still reported Resumed=true", name)
		}
	}

	// Segment replaced by a shorter file (external truncation — the CACHE-class
	// hazard the cursor contract promises to survive): the stale clean cursor
	// must converge to the fresh-scan answer AND report Resumed=false.
	segPath := filepath.Join(store.engine.eventsDir, rel)
	data, err := os.ReadFile(segPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(segPath, data[:len(data)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	fresh, err := store.ScanFrom(ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	shrunk, err := store.ScanFrom(ScanOptions{Cursor: clean.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(entryIDs(shrunk.Entries), entryIDs(fresh.Entries)) {
		t.Fatalf("shrunk-segment scan = %v, want fresh-scan convergence %v", entryIDs(shrunk.Entries), entryIDs(fresh.Entries))
	}
	if shrunk.Resumed {
		t.Fatal("shrunk-segment reparse (a superset page) still reported Resumed=true")
	}

	// Convergence: the cursor issued by the degraded scan resumes cleanly.
	steady, err := store.ScanFrom(ScanOptions{Cursor: shrunk.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(steady.Entries) != 0 || !steady.Resumed {
		t.Fatalf("post-hazard steady scan: entries=%d resumed=%v, want 0/true", len(steady.Entries), steady.Resumed)
	}
}
