package eventstore

// Falsifiers for work_65efc7f8f414: engine.put's two hot-day-segment parses
// (repairTail's happy-path interior sweep and checkID's hot-branch dup scan)
// now route through the existing hotFrames memo (hotcache.go) instead of
// full-parsing on every call. installParseCounter / installIndexParseCounter
// (testmain_test.go, index_memo_test.go) and the openConfiguredStore /
// putRecord / eventRecord / hotSegmentPath / frameLine / appendFile /
// sealedIndexPath helpers are shared with the existing hotcache/repair/index
// falsifier suites in this package.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// F1 — parse-count (the predicate's falsifier). A synthetic hot segment per
// family carries ~1k frames (1000 total, interleaved rec_/tr_ like a real
// import). N=100 all-dup default-mode PutJSON calls, alternating families,
// must full-parse each family's hot segment exactly once TOTAL (the cold
// warm-up) and zero additional times across the remaining puts — matching
// the live 5000-row ALL-DUP import this packet targets.
func TestPutAllDupHotSegmentParsedOncePerFamily(t *testing.T) {
	now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	day := ingestionDay(now)

	const perFamily = 500
	trPath := hotSegmentPath(store, "transition", day)
	recPath := hotSegmentPath(store, "receipt", day)
	for i := 0; i < perFamily; i++ {
		trID := fmt.Sprintf("tr_putmemo_%04d", i)
		recID := fmt.Sprintf("rec_putmemo_%04d", i)
		appendFile(t, trPath, frameLine(t, trID, "compound.transition.v0.1", now, fmt.Sprintf("t%04d", i)))
		appendFile(t, recPath, frameLine(t, recID, "compound.receipt.v0.1", now, fmt.Sprintf("r%04d", i)))
	}

	trDupID := "tr_putmemo_0000"
	recDupID := "rec_putmemo_0000"

	hotCount, restore := installParseCounter(t)
	defer restore()

	const n = 100
	for i := 0; i < n; i++ {
		var result PutResult
		var err error
		if i%2 == 0 {
			result, err = store.PutJSON(trDupID, eventRecord(trDupID, "compound.transition.v0.1", now, "t0000"), "  ")
		} else {
			result, err = store.PutJSON(recDupID, eventRecord(recDupID, "compound.receipt.v0.1", now, "r0000"), "  ")
		}
		if err != nil {
			t.Fatalf("all-dup put %d: %v", i, err)
		}
		if result.Created {
			t.Fatalf("all-dup put %d unexpectedly created a new record", i)
		}
	}

	if got := hotCount(trPath); got != 1 {
		t.Fatalf("transition hot segment full-parsed %d times across %d all-dup puts, want 1 (cold warm-up only)", got, n/2)
	}
	if got := hotCount(recPath); got != 1 {
		t.Fatalf("receipt hot segment full-parsed %d times across %d all-dup puts, want 1 (cold warm-up only)", got, n/2)
	}
}

// F2 — dup no-op. Seed a hot segment directly (memo cold), then put the same
// id+content twice: the first call pays the unavoidable cold warm-up parse,
// the second call must be a pure memo hit (no observer fire) -- and across
// both, the segment bytes never change.
func TestPutDupIsNoOpAndSecondCallDoesNotReparse(t *testing.T) {
	now := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	id := "rec_f2dup"
	value := "dupval"
	path := hotSegmentPath(store, "receipt", ingestionDay(now))
	appendFile(t, path, frameLine(t, id, "compound.receipt.v0.1", now, value))

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	count, restore := installParseCounter(t)
	defer restore()

	first, err := store.PutJSON(id, eventRecord(id, "compound.receipt.v0.1", now, value), "  ")
	if err != nil {
		t.Fatalf("first dup put: %v", err)
	}
	if first.Created {
		t.Fatal("first dup put unexpectedly created a new record")
	}
	if got := count(path); got != 1 {
		t.Fatalf("parses after first (cold) dup put = %d, want 1", got)
	}

	second, err := store.PutJSON(id, eventRecord(id, "compound.receipt.v0.1", now, value), "  ")
	if err != nil {
		t.Fatalf("second dup put: %v", err)
	}
	if second.Created {
		t.Fatal("second dup put unexpectedly created a new record")
	}
	if got := count(path); got != 1 {
		t.Fatalf("parses after second (warm) dup put = %d, want still 1 -- no observer fire on the second put", got)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("dup puts changed segment bytes")
	}
}

// F3 — conflict newest. An existing id put with different content must
// return ErrIDConflict (default/idempotent mode) and os.ErrExist (exclusive
// mode) -- both still returned, and neither call appends to the segment.
func TestPutConflictNewestReturnsBothErrorForms(t *testing.T) {
	now := time.Date(2026, 7, 17, 11, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	id := "rec_f3conflict"
	putRecord(t, store, id, "compound.receipt.v0.1", now, "original")

	path := hotSegmentPath(store, "receipt", ingestionDay(now))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.PutJSON(id, eventRecord(id, "compound.receipt.v0.1", now, "different"), "  "); !errors.Is(err, ErrIDConflict) {
		t.Fatalf("default-mode conflicting put error = %v, want ErrIDConflict", err)
	}
	if _, err := store.PutJSONExclusive(id, eventRecord(id, "compound.receipt.v0.1", now, "different"), "  "); !errors.Is(err, os.ErrExist) {
		t.Fatalf("exclusive-mode conflicting put error = %v, want os.ErrExist", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("conflicting puts appended to the segment")
	}
}

// F4 — conflict via intra-divergent duplicates. Hand-craft a hot segment
// where id X has an OLDER frame with hash H1 and a NEWER frame with hash H2
// (put() itself refuses to create this state -- write the raw frame lines
// directly). Base checkID's per-occurrence reverse scan returns idConflict
// here because the OLDER occurrence's hash differs from the incoming hash,
// even though the NEWEST occurrence matches exactly. A bare byID[id] lookup
// (ignoring divergence) would only see the newest occurrence and wrongly
// report idAlreadyPresent -- this is the exact base-vs-map semantics gap the
// divergent set exists to close.
func TestPutConflictViaIntraDivergentDuplicates(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	id := "rec_f4divergent"
	path := hotSegmentPath(store, "receipt", ingestionDay(now))
	appendFile(t, path, frameLine(t, id, "compound.receipt.v0.1", now, "H1-older"))
	appendFile(t, path, frameLine(t, id, "compound.receipt.v0.1", now, "H2-newer"))

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.PutJSON(id, eventRecord(id, "compound.receipt.v0.1", now, "H2-newer"), "  ")
	if !errors.Is(err, ErrIDConflict) {
		t.Fatalf("put matching only the newest divergent occurrence error = %v, want ErrIDConflict", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("conflicting divergent put appended to the segment")
	}
}

// F5 — torn-tail heal preserved. A memo warmed BEFORE a torn (newline-less)
// final frame is appended must not serve stale (pre-truncation) content
// after the next put heals the tail: the heal restores the cached size and
// some filesystems can reuse the cached mtime, so repair must invalidate the
// memo explicitly. The heal itself (byte-for-byte truncation) must
// match base behavior exactly -- this test only adds the memo-interaction
// angle on top of repair_test.go's existing (memo-untouched) heal coverage.
func TestPutTornTailHealInvalidatesWarmMemo(t *testing.T) {
	now := time.Date(2026, 7, 17, 13, 0, 0, 0, time.UTC)
	var repairs []string
	store := openConfiguredStore(t, engineConfig{
		clock: func() time.Time { return now },
		logf:  func(format string, args ...any) { repairs = append(repairs, fmt.Sprintf(format, args...)) },
	})
	path := hotSegmentPath(store, "receipt", ingestionDay(now))
	putRecord(t, store, "rec_f5prior0", "compound.receipt.v0.1", now, "prior0")
	putRecord(t, store, "rec_f5prior1", "compound.receipt.v0.1", now, "prior1")

	warm, err := store.Read("rec_f5prior0")
	if err != nil {
		t.Fatalf("warm read: %v", err)
	}

	torn := frameLine(t, "rec_f5torn", "compound.receipt.v0.1", now, "torn")
	appendFile(t, path, torn[:len(torn)/2]) // no trailing newline: torn tail

	// Install the counter only now: the heal branch itself (truncateAndSync)
	// truncates the torn tail back to exactly the pre-append size. Even when
	// the filesystem reuses the cached mtime, checkID's post-heal memo consult
	// below MUST genuinely reparse rather than serve the stale map.
	count, restore := installParseCounter(t)
	defer restore()

	if _, err := store.PutJSON("rec_f5after", eventRecord("rec_f5after", "compound.receipt.v0.1", now, "after"), "  "); err != nil {
		t.Fatalf("put after torn tail: %v", err)
	}

	if len(repairs) != 1 || !strings.Contains(strings.ToLower(repairs[0]), "repair") {
		t.Fatalf("repair log = %q, want exactly one repair entry", repairs)
	}
	if got := count(path); got != 1 {
		t.Fatalf("parses during the post-heal put = %d, want 1 (repair must explicitly invalidate the pre-torn-append warm entry even when truncate restores the cached size+mtime tuple)", got)
	}

	parsed, err := parseSegment(path, false)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{"rec_f5prior0", "rec_f5prior1", "rec_f5after"}
	if len(parsed) != len(wantIDs) {
		t.Fatalf("frames after heal = %d, want %d", len(parsed), len(wantIDs))
	}
	for i, id := range wantIDs {
		if parsed[i].Frame.ID != id {
			t.Fatalf("frame %d = %s, want %s", i, parsed[i].Frame.ID, id)
		}
	}

	again, err := store.Read("rec_f5prior0")
	if err != nil {
		t.Fatalf("re-read after heal: %v", err)
	}
	if !bytes.Equal(warm.Bytes, again.Bytes) {
		t.Fatalf("prior record bytes drifted after heal (stale memo served?):\n before=%s\n after =%s", warm.Bytes, again.Bytes)
	}
}

// F6 — cross-process staleness. After a warm memo, a frame appended
// OUT-OF-BAND (a direct file write, simulating another process under its own
// lock) must be visible to the very next put: the stat guard misses on the
// size/mtime move, triggers exactly one re-parse, and correctly dup-detects
// against the now-present foreign frame instead of re-appending it.
func TestPutSeesForeignAppendExactlyOneReparse(t *testing.T) {
	now := time.Date(2026, 7, 17, 14, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	path := hotSegmentPath(store, "receipt", ingestionDay(now))
	putRecord(t, store, "rec_f6seed", "compound.receipt.v0.1", now, "seed")

	if _, err := store.Read("rec_f6seed"); err != nil {
		t.Fatalf("warm read: %v", err)
	}

	foreignID := "rec_f6foreign"
	foreign := frameLine(t, foreignID, "compound.receipt.v0.1", now, "foreignval")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(foreign); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	count, restore := installParseCounter(t)
	defer restore()

	result, err := store.PutJSON(foreignID, eventRecord(foreignID, "compound.receipt.v0.1", now, "foreignval"), "  ")
	if err != nil {
		t.Fatalf("put against foreign-appended id: %v", err)
	}
	if result.Created {
		t.Fatal("put re-appended an id a foreign writer already wrote (dup-detect against the foreign append failed)")
	}
	if got := count(path); got != 1 {
		t.Fatalf("parses after foreign append + put = %d, want exactly 1 (stat-guarded miss, one re-parse)", got)
	}
}

// F7 — sealed segments untouched. An id living only in a sealed (index>0)
// segment must still resolve through the sidecar index: notifyIndexParse
// fires, notifyHotParse never fires for the sealed segment itself, and the
// sealed segment's bytes never change.
func TestPutSealedIDResolvesViaIndexNotHotMemo(t *testing.T) {
	day1 := time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)
	clk := day1
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return clk }})

	sealedID := "rec_f7sealed"
	putRecord(t, store, sealedID, "compound.receipt.v0.1", day1, "sealedval")

	day2 := day1.Add(24 * time.Hour)
	clk = day2
	putRecord(t, store, "rec_f7hot", "compound.receipt.v0.1", day2, "hotval")

	sealedPath := hotSegmentPath(store, "receipt", ingestionDay(day1))
	hotPath := hotSegmentPath(store, "receipt", ingestionDay(day2))
	idxPath := sealedIndexPath(store, "receipt", ingestionDay(day1))

	// Prime the first-ever resolution of the sealed id (builds the sidecar
	// .idx on disk via rebuild, which does not itself remember the index
	// in-process -- see loadOrBuildIndex/rebuildIndex) and the day-2 hot
	// segment, so the counted window below measures steady state rather than
	// one-time index-build / hot-memo cold-start noise.
	if _, err := store.Read(sealedID); err != nil {
		t.Fatalf("priming read: %v", err)
	}

	sealedBefore, err := os.ReadFile(sealedPath)
	if err != nil {
		t.Fatal(err)
	}

	hotCount, restoreHot := installParseCounter(t)
	defer restoreHot()
	idxCount, restoreIdx := installIndexParseCounter(t)
	defer restoreIdx()

	result, err := store.PutJSON(sealedID, eventRecord(sealedID, "compound.receipt.v0.1", day1, "sealedval"), "  ")
	if err != nil {
		t.Fatalf("dup put against sealed-only id: %v", err)
	}
	if result.Created {
		t.Fatal("dup put against sealed-only id unexpectedly created a new record")
	}

	if got := idxCount(idxPath); got != 1 {
		t.Fatalf("sealed index parses during the dup put = %d, want 1 (notifyIndexParse must fire)", got)
	}
	if got := hotCount(sealedPath); got != 0 {
		t.Fatalf("hot-parse observer fired %d times for the SEALED segment, want 0 (a sealed id must resolve via the index only)", got)
	}
	if got := hotCount(hotPath); got != 0 {
		t.Fatalf("hot segment reparsed %d times during the dup put, want 0 (already warm from priming, nothing changed since)", got)
	}

	sealedAfter, err := os.ReadFile(sealedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sealedBefore, sealedAfter) {
		t.Fatal("dup put against sealed-only id mutated the sealed segment")
	}
}

// F8 — consumer lifecycle (the live-flow falsifier this package's F1 could
// not see). The real import path (routeevidence.RecordTransition) calls
// eventstore.Open PER ROW, so an engine-scoped memo dies with every row and
// the hot segment is still full-parsed once per put — the 2026-07-17 live
// run proved F1 green while the real 5000-row all-dup import stayed slow.
// The hot-segment and sealed-index memos are therefore package-level and
// path-keyed (absolute paths; distinct homes cannot collide): N all-dup puts
// through N FRESH stores on the same home must still full-parse the hot
// segment exactly once process-wide.
func TestPutAllDupAcrossFreshStoreOpensParsesOnce(t *testing.T) {
	now := time.Date(2026, 7, 17, 11, 0, 0, 0, time.UTC)
	config := engineConfig{clock: func() time.Time { return now }}
	first := openConfiguredStore(t, config)
	home := first.engine.home
	day := ingestionDay(now)

	trPath := hotSegmentPath(first, "transition", day)
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("tr_freshopen_%04d", i)
		appendFile(t, trPath, frameLine(t, id, "compound.transition.v0.1", now, fmt.Sprintf("v%04d", i)))
	}

	dupID := "tr_freshopen_0000"
	hotCount, restore := installParseCounter(t)
	defer restore()

	const n = 50
	for i := 0; i < n; i++ {
		store, err := openWithConfig(home, config)
		if err != nil {
			t.Fatalf("fresh open %d: %v", i, err)
		}
		result, err := store.PutJSON(dupID, eventRecord(dupID, "compound.transition.v0.1", now, "v0000"), "  ")
		if err != nil {
			t.Fatalf("all-dup put %d via fresh store: %v", i, err)
		}
		if result.Created {
			t.Fatalf("all-dup put %d via fresh store unexpectedly created a record", i)
		}
	}

	if got := hotCount(trPath); got != 1 {
		t.Fatalf("hot segment full-parsed %d times across %d all-dup puts through fresh store opens, want 1 (memo must be package-level, not engine-scoped)", got, n)
	}
}
