package eventstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const membershipTestSchema = "compound.receipt.v0.1"

var membershipTestNow = time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)

func membershipStore(t *testing.T) *Store {
	t.Helper()
	return openConfiguredStore(t, engineConfig{
		clock: func() time.Time { return membershipTestNow },
		logf:  func(string, ...any) {},
	})
}

// seedSealed writes one sealed (non-newest) receipt segment named by seq with
// the given ids, each carrying value "v-<id>".
func seedSealed(t *testing.T, store *Store, seq int, ids ...string) string {
	t.Helper()
	dir := filepath.Join(store.engine.segRoot, "receipt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := segmentPath(store.engine.segRoot, "receipt", "2026-01-01", seq)
	for _, id := range ids {
		appendFile(t, path, frameLine(t, id, membershipTestSchema, membershipTestNow, "v-"+id))
	}
	return path
}

func countMembership(t *testing.T) (full, extend func() int, restore func()) {
	t.Helper()
	var mu sync.Mutex
	var f, x int
	prev := membershipObserver
	membershipObserver = func(_ string, isFull bool) {
		mu.Lock()
		defer mu.Unlock()
		if isFull {
			f++
		} else {
			x++
		}
	}
	return func() int { mu.Lock(); defer mu.Unlock(); return f },
		func() int { mu.Lock(); defer mu.Unlock(); return x },
		func() { membershipObserver = prev }
}

func putReceipt(store *Store, id, value string) (PutResult, error) {
	return store.PutJSON(id, eventRecord(id, membershipTestSchema, membershipTestNow, value), "  ")
}

// (1) A sealed segment that grew behind the filter's back (a writer that never
// updated it) must not let a conflicting or duplicate id through: the stale
// fingerprint forces a rebuild, and the full index scan then decides.
func TestMembershipStaleFilterCannotHideForeignAppend(t *testing.T) {
	store := membershipStore(t)
	seg := seedSealed(t, store, 1, "rec_a", "rec_b")
	seedSealed(t, store, 2, "rec_c")
	if _, err := putReceipt(store, "rec_warm", "w"); err != nil { // builds the filter
		t.Fatal(err)
	}
	if _, err := os.Stat(store.engine.membershipPath("receipt")); err != nil {
		t.Fatalf("filter not persisted: %v", err)
	}

	// Foreign writer appends rec_foreign to a sealed segment; the filter is not told.
	appendFile(t, seg, frameLine(t, "rec_foreign", membershipTestSchema, membershipTestNow, "v-rec_foreign"))

	if _, err := putReceipt(store, "rec_foreign", "DIFFERENT"); !errors.Is(err, ErrIDConflict) {
		t.Fatalf("stale filter hid a conflicting id; err=%v", err)
	}
	res, err := putReceipt(store, "rec_foreign", "v-rec_foreign")
	if err != nil {
		t.Fatal(err)
	}
	if res.Created {
		t.Fatal("stale filter let a duplicate id be appended again")
	}
}

// (2) A missing or torn filter is rebuilt (logged via observer) and the rebuilt
// filter still reaches the right answers.
func TestMembershipMissingAndCorruptFilterRebuilt(t *testing.T) {
	store := membershipStore(t)
	seedSealed(t, store, 1, "rec_a", "rec_b")
	seedSealed(t, store, 2, "rec_c")
	full, _, restore := countMembership(t)
	defer restore()

	if _, err := putReceipt(store, "rec_new1", "x"); err != nil {
		t.Fatal(err)
	}
	if full() != 1 {
		t.Fatalf("first put should build the filter once, got %d", full())
	}
	path := store.engine.membershipPath("receipt")

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := putReceipt(store, "rec_a", "DIFFERENT"); !errors.Is(err, ErrIDConflict) {
		t.Fatalf("conflict lost after filter removal: %v", err)
	}
	if full() != 2 {
		t.Fatalf("missing filter should rebuild once, got %d rebuilds", full())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("filter not rewritten: %v", err)
	}
	if err := os.WriteFile(path, raw[:len(raw)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := putReceipt(store, "rec_c", "DIFFERENT"); !errors.Is(err, ErrIDConflict) {
		t.Fatalf("conflict lost after filter truncation: %v", err)
	}
	if full() != 3 {
		t.Fatalf("torn filter should rebuild once, got %d rebuilds", full())
	}
	// Steady state: no further rebuilds.
	if _, err := putReceipt(store, "rec_new2", "y"); err != nil {
		t.Fatal(err)
	}
	if full() != 3 {
		t.Fatalf("healthy filter rebuilt again: %d", full())
	}
}

// (3) Conflict and idempotence semantics for an id living in a cold segment
// are exactly those of the unfiltered scan.
func TestMembershipColdConflictAndDuplicateUnchanged(t *testing.T) {
	store := membershipStore(t)
	seedSealed(t, store, 1, "rec_cold")
	seedSealed(t, store, 2, "rec_other")

	if _, err := putReceipt(store, "rec_cold", "DIFFERENT"); !errors.Is(err, ErrIDConflict) {
		t.Fatalf("different hash in cold segment must conflict: %v", err)
	}
	res, err := putReceipt(store, "rec_cold", "v-rec_cold")
	if err != nil || res.Created {
		t.Fatalf("same hash in cold segment must be already-present: created=%v err=%v", res.Created, err)
	}
	if _, err := store.PutJSONExclusive("rec_cold", eventRecord("rec_cold", membershipTestSchema, membershipTestNow, "v-rec_cold"), "  "); !errors.Is(err, os.ErrExist) {
		t.Fatalf("exclusive put of cold id must be ErrExist: %v", err)
	}
	res, err = putReceipt(store, "rec_brand_new", "n")
	if err != nil || !res.Created {
		t.Fatalf("brand-new id must append: created=%v err=%v", res.Created, err)
	}
}

// A rollover (new sealed segment) extends the filter instead of rebuilding it,
// and an id that lives only in the newly sealed segment is still found.
func TestMembershipRolloverExtendsWithoutRebuild(t *testing.T) {
	store := membershipStore(t)
	seedSealed(t, store, 1, "rec_a")
	seedSealed(t, store, 2, "rec_b")
	full, extend, restore := countMembership(t)
	defer restore()
	if _, err := putReceipt(store, "rec_n1", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := putReceipt(store, "rec_n1b", "x"); err != nil { // today's segment is hot, both seeds cold
		t.Fatal(err)
	}
	base := full()
	// The hot (today) segment now exists; seal it by moving the clock a day.
	store.engine.clock = func() time.Time { return membershipTestNow.Add(24 * time.Hour) }
	if _, err := putReceipt(store, "rec_n2", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := putReceipt(store, "rec_n3", "x"); err != nil {
		t.Fatal(err)
	}
	if extend() == 0 || full() != base {
		t.Fatalf("rollover should extend (extends=%d) not rebuild (rebuilds %d -> %d)", extend(), base, full())
	}
	if _, err := putReceipt(store, "rec_n1", "DIFFERENT"); !errors.Is(err, ErrIDConflict) {
		t.Fatalf("id in newly sealed segment lost: %v", err)
	}
}

// (4) Put cost no longer scales with sealed-segment count: with ~1000 sealed
// segments and a warm filter, new-id puts parse zero sealed indexes and
// rebuild nothing. RED against the unfiltered checkID, which parses (or at
// least loads) every sealed index per put.
func TestMembershipPutDoesNotScaleWithSealedSegments(t *testing.T) {
	store := membershipStore(t)
	const segments = 1000
	for i := 1; i <= segments; i++ {
		seedSealed(t, store, i, fmt.Sprintf("rec_seg%04d_a", i), fmt.Sprintf("rec_seg%04d_b", i))
	}
	for i := 0; i < 2; i++ { // first put seals the last seed; second absorbs it
		if _, err := putReceipt(store, fmt.Sprintf("rec_warm%d", i), "w"); err != nil {
			t.Fatal(err)
		}
	}

	// Fresh-process behavior: drop the in-process index memo so any index load
	// shows up as a parse.
	sharedIndexCache.mu.Lock()
	sharedIndexCache.entries = nil
	sharedIndexCache.mu.Unlock()
	parses, restoreParse := installIndexParseCounter(t)
	defer restoreParse()
	full, extend, restoreM := countMembership(t)
	defer restoreM()

	const puts = 20
	start := time.Now()
	for i := 0; i < puts; i++ {
		if res, err := putReceipt(store, fmt.Sprintf("rec_fresh_%02d", i), "f"); err != nil || !res.Created {
			t.Fatalf("put %d: created=%v err=%v", i, res.Created, err)
		}
	}
	elapsed := time.Since(start)

	total := 0
	for i := 1; i <= segments; i++ {
		total += parses(sealedIndexPath(store, "receipt", fmt.Sprintf("2026-01-01.%d", i)))
	}
	if total != 0 || full() != 0 || extend() != 0 {
		t.Fatalf("warm-filter puts touched sealed state: index parses=%d filter rebuilds=%d extends=%d", total, full(), extend())
	}
	if per := elapsed / puts; per > 150*time.Millisecond {
		t.Fatalf("put over %d sealed segments took %v each; filter fast path should be a few ms", segments, per)
	}
	t.Logf("%d sealed segments: %v per put", segments, elapsed/puts)
}
