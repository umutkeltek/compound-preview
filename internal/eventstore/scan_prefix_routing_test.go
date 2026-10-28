package eventstore

import (
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

// familySeed is one id/schema pair whose family is unambiguous under
// familyOf's id-prefix vocabulary (frame.go).
type familySeed struct {
	family string
	id     string
	schema string
}

// allFamilySeeds covers every family the segment layout partitions by,
// including both action canonical prefixes (act_ and act_live_ resolve to
// the same family) so multi-canonical-prefix routing gets exercised too.
var allFamilySeeds = []familySeed{
	{"transition", "tr_a", "compound.transition.v0.1"},
	{"policy", "pol_a", "compound.policy_decision.v0.1"},
	{"hook", "hook_a", "compound.hook_event.v0.1"},
	{"action", "act_a", "compound.action_intent.v0.1"},
	{"action", "act_live_a", "compound.action_intent.v0.1"},
	{"trace", "trace_a", "compound.trace_envelope.v0.1"},
	{"receipt", "rec_a", "compound.receipt.v0.1"},
	{"approval", "approval_grant_a", "compound.exact_action_approval.v0.1"},
}

func seedAllFamilies(t *testing.T, store *Store, now time.Time) {
	t.Helper()
	for _, seed := range allFamilySeeds {
		putRecord(t, store, seed.id, seed.schema, now, "v")
	}
}

// segmentFamily recovers the family directory name a segment path was
// parsed from: segmentPath joins segRoot/family/day[.seq].jsonl, so the
// family is the parent directory's basename.
func segmentFamily(path string) string {
	return filepath.Base(filepath.Dir(path))
}

// observeParsedFamilies installs scanSegmentObserver for the duration of the
// test and returns a function reporting the sorted, deduplicated set of
// families any segment was ACTUALLY parsed from.
func observeParsedFamilies(t *testing.T) func() []string {
	t.Helper()
	seen := map[string]bool{}
	scanSegmentObserver = func(path string, _ int, _ int64) {
		seen[segmentFamily(path)] = true
	}
	t.Cleanup(func() { scanSegmentObserver = nil })
	return func() []string {
		out := make([]string, 0, len(seen))
		for family := range seen {
			out = append(out, family)
		}
		sort.Strings(out)
		return out
	}
}

// S1 (core, RED at base): a Prefixes-scoped Scan parses ONLY the matching
// family's segments. At base, scanAll always walked every family directory
// on disk regardless of opts.Prefixes (post-filtering in memory only), so
// this observer-counted assertion failed RED there (all six seeded families
// would appear in `touched`); routing (scanFamilyRestriction, store.go)
// makes it GREEN by skipping the non-matching family directories' readdir
// and parse entirely.
func TestScanPrefixRoutingParsesOnlyMatchingFamilySegments(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	closeCompletionGate(t, store)
	seedAllFamilies(t, store, now)

	touched := observeParsedFamilies(t)

	entries, err := store.Scan(ScanOptions{Prefixes: []string{"hook_"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "hook_a" {
		t.Fatalf("scan entries = %+v, want only hook_a", entries)
	}
	if got, want := touched(), []string{"hook"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("families parsed by Scan = %v, want %v (routing must skip non-matching family dirs)", got, want)
	}
}

// S1 sibling: the same falsifier against the ScanFrom path (scan_cursor.go),
// the other function the packet names as I/O-blind at base.
func TestScanFromPrefixRoutingParsesOnlyMatchingFamilySegments(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	closeCompletionGate(t, store)
	seedAllFamilies(t, store, now)

	touched := observeParsedFamilies(t)

	page, err := store.ScanFrom(ScanOptions{Prefixes: []string{"hook_"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || page.Entries[0].ID != "hook_a" {
		t.Fatalf("scanFrom entries = %+v, want only hook_a", page.Entries)
	}
	if got, want := touched(), []string{"hook"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("families parsed by ScanFrom = %v, want %v (routing must skip non-matching family dirs)", got, want)
	}
}

// S2 (equality): for several prefix sets (single family, multi family, an
// unmapped garbage prefix, and multi-canonical-prefix-same-family), a
// prefix-routed scan returns EXACTLY the same ids as an unrestricted scan
// post-filtered by the same prefixes -- on both Scan and ScanFrom. Routing
// is a pure I/O optimization; matchesPrefix stays the sole authority on
// which records the caller receives.
func TestScanPrefixRoutingMatchesUnprefixedPostFilter(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	closeCompletionGate(t, store)
	seedAllFamilies(t, store, now)

	full, err := store.Scan(ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		prefixes []string
	}{
		{"single_family", []string{"hook_"}},
		{"multi_family", []string{"hook_", "rec_"}},
		{"unmapped_garbage", []string{"zzz_"}},
		{"unmapped_mixed_with_mapped", []string{"hook_", "zzz_"}},
		{"multi_canonical_same_family", []string{"act_", "act_live_"}},
		{"extended_canonical_prefix", []string{"rec_a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := []string{}
			for _, e := range full {
				if matchesPrefix(e.ID, tc.prefixes) {
					want = append(want, e.ID)
				}
			}
			sort.Strings(want)

			scanned, err := store.Scan(ScanOptions{Prefixes: tc.prefixes})
			if err != nil {
				t.Fatal(err)
			}
			if got := entryIDs(scanned); !reflect.DeepEqual(got, want) {
				t.Fatalf("Scan(Prefixes=%v) ids = %v, want %v", tc.prefixes, got, want)
			}

			page, err := store.ScanFrom(ScanOptions{Prefixes: tc.prefixes})
			if err != nil {
				t.Fatal(err)
			}
			if got := entryIDs(page.Entries); !reflect.DeepEqual(got, want) {
				t.Fatalf("ScanFrom(Prefixes=%v) ids = %v, want %v", tc.prefixes, got, want)
			}
		})
	}
}

// S4 (fail-safe): an unmapped prefix in the set forces ALL families to be
// parsed (observer-counted) even though the post-filter still narrows the
// returned entries correctly. Mixing one mapped and one unmapped prefix must
// not narrow I/O to only the mapped prefix's family -- the whole Prefixes
// set has to map cleanly or routing backs off entirely.
func TestScanPrefixRoutingFailSafeOnUnmappedPrefixParsesAllFamilies(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	closeCompletionGate(t, store)
	seedAllFamilies(t, store, now)

	wantFamilies := []string{"action", "approval", "hook", "policy", "receipt", "trace", "transition"}

	t.Run("wholly_unmapped", func(t *testing.T) {
		touched := observeParsedFamilies(t)
		entries, err := store.Scan(ScanOptions{Prefixes: []string{"zzz_"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("unmapped-prefix scan returned %+v, want none", entries)
		}
		if got := touched(); !reflect.DeepEqual(got, wantFamilies) {
			t.Fatalf("families parsed = %v, want fail-safe superset %v", got, wantFamilies)
		}
	})

	t.Run("mixed_mapped_and_unmapped", func(t *testing.T) {
		touched := observeParsedFamilies(t)
		entries, err := store.Scan(ScanOptions{Prefixes: []string{"hook_", "zzz_"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].ID != "hook_a" {
			t.Fatalf("mixed-prefix scan entries = %+v, want only hook_a", entries)
		}
		if got := touched(); !reflect.DeepEqual(got, wantFamilies) {
			t.Fatalf("families parsed = %v, want fail-safe superset %v (one unmapped prefix must disable routing entirely)", got, wantFamilies)
		}
	})
}

// S5 (hot-path regression): the approval_ prefix MUST route to its own family.
// This is the one prefix the pre-tool-use hook scans on EVERY tool call
// (policy.ExactActionApprovalStore.ApplyMatching), so falling into the S4
// fail-safe here is not a slow scan — it is a full parse of the entire event
// store per tool call in every concurrent lane. Observed live before the
// mapping existed: 2-5 GB RSS and 30-40s per hook invocation against a 3 GB
// store. The mapping is safe to introduce without migration because approval
// records expire in <=15m and none exist outside their own family.
func TestScanApprovalPrefixRoutingParsesOnlyApprovalFamily(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	closeCompletionGate(t, store)
	seedAllFamilies(t, store, now)

	touched := observeParsedFamilies(t)

	entries, err := store.Scan(ScanOptions{Prefixes: []string{"approval_"}, RegularOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "approval_grant_a" {
		t.Fatalf("scan entries = %+v, want only approval_grant_a", entries)
	}
	if got, want := touched(), []string{"approval"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("families parsed by approval_ Scan = %v, want %v (an unmapped approval_ prefix regresses every tool call on the machine to a full-store parse)", got, want)
	}
}

// S3 (cursor law): a cursor minted under one Prefixes intent, then consumed
// by ScanFrom under a DIFFERENT Prefixes intent, must never skip a record --
// worst case it re-parses more than the ideal minimum, never less. The
// per-segment byte-cursor only records progress for families a given call
// actually visited (see scanFrom's restrictFamilies doc, scan_cursor.go); a
// family missing from that map on a later call falls back to a full reparse
// via the existing zero-value convergence law, so switching intent can only
// broaden, never narrow, what gets returned.
func TestScanFromPrefixIntentSwitchNeverSkipsRecords(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

	t.Run("narrow_then_broad_recovers_untracked_family", func(t *testing.T) {
		store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
		closeCompletionGate(t, store)
		putRecord(t, store, "hook_a", "compound.hook_event.v0.1", now, "v")
		putRecord(t, store, "hook_b", "compound.hook_event.v0.1", now, "v")
		putRecord(t, store, "rec_a", "compound.receipt.v0.1", now, "v")
		putRecord(t, store, "rec_b", "compound.receipt.v0.1", now, "v")

		// Mint under a narrow (hook-only) intent -- the cursor this issues only
		// tracks the hook family's segment; receipt is never enumerated.
		page1, err := store.ScanFrom(ScanOptions{Prefixes: []string{"hook_"}})
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"hook_a", "hook_b"}; !reflect.DeepEqual(entryIDs(page1.Entries), want) {
			t.Fatalf("narrow mint entries = %v, want %v", entryIDs(page1.Entries), want)
		}

		// Reuse that cursor under a BROAD intent. hook is already fully
		// consumed (zero new there); receipt was never tracked, so it must get
		// a full reparse -- not skipped.
		page2, err := store.ScanFrom(ScanOptions{Prefixes: nil, Cursor: page1.Cursor})
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"rec_a", "rec_b"}; !reflect.DeepEqual(entryIDs(page2.Entries), want) {
			t.Fatalf("broad reuse after narrow mint entries = %v, want %v (receipt family must not be skipped)", entryIDs(page2.Entries), want)
		}

		full, err := store.Scan(ScanOptions{})
		if err != nil {
			t.Fatal(err)
		}
		union := append(append([]string{}, entryIDs(page1.Entries)...), entryIDs(page2.Entries)...)
		sort.Strings(union)
		if got := entryIDs(full); !reflect.DeepEqual(union, got) {
			t.Fatalf("union of intent-switched incremental scans = %v, want full scan %v (zero skipped records)", union, got)
		}
	})

	t.Run("broad_then_narrow_still_detects_new_record", func(t *testing.T) {
		store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
		closeCompletionGate(t, store)
		putRecord(t, store, "hook_a", "compound.hook_event.v0.1", now, "v")
		putRecord(t, store, "rec_a", "compound.receipt.v0.1", now, "v")

		// Mint under a broad intent -- both families tracked in the cursor.
		pageA, err := store.ScanFrom(ScanOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"hook_a", "rec_a"}; !reflect.DeepEqual(entryIDs(pageA.Entries), want) {
			t.Fatalf("broad mint entries = %v, want %v", entryIDs(pageA.Entries), want)
		}

		// A new hook record appears, then the SAME cursor is reused under a
		// narrower (hook-only) intent. It must find exactly the new record --
		// neither re-emitting the already-seen one nor skipping the new one.
		putRecord(t, store, "hook_b", "compound.hook_event.v0.1", now, "v")
		pageB, err := store.ScanFrom(ScanOptions{Prefixes: []string{"hook_"}, Cursor: pageA.Cursor})
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"hook_b"}; !reflect.DeepEqual(entryIDs(pageB.Entries), want) {
			t.Fatalf("narrow reuse after broad mint entries = %v, want %v", entryIDs(pageB.Entries), want)
		}
	})
}
