package eventstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

// These falsifiers pin the exact prefix enumeration contract the proof
// refresh hot path depends on (EnumerateIDs): the id set is complete against
// Scan for the same prefix, sealed segments are screened by index keys so
// unrelated history is never parsed, absent/invalid/stale indexes degrade to
// a rebuild instead of losing ids, and corruption that survives a rebuild
// fails closed. The pfrh_/pfrp_ family mapping is pinned too: an unmapped
// prefix forced every family into the walk.

// proofEnumerationStore opens a store whose engine clock tracks the returned
// *now, so a test can advance the day and actually seal segments (segment
// day selection reads the engine clock, not record created_at).
func proofEnumerationStore(t *testing.T, startTime time.Time) (*Store, *time.Time) {
	t.Helper()
	now := startTime
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	closeCompletionGate(t, store)
	return store, &now
}

// seedProofEnumerationFixture writes pfrh_ refresh outcomes and preparations
// (work-owned schemas the family switch never routes) across one sealed day
// and the current hot day, plus unrelated routed families.
func seedProofEnumerationFixture(t *testing.T, store *Store, now *time.Time) (sealedDay string) {
	t.Helper()
	putRecord(t, store, "pfrh_seed0001_outcome_a", "compound.work_proof_refresh.v0.1", *now, "sealed outcome a")
	putRecord(t, store, "pfrh_seed0001_outcome_b", "compound.work_proof_refresh.v0.1", *now, "sealed outcome b")
	putRecord(t, store, "pfrp_seed0001_prep_a", "compound.work_proof_refresh_preparation.v0.1", *now, "sealed prep")
	putRecord(t, store, "rec_seed_bystander", "compound.receipt.v0.1", *now, "unrelated receipt")
	// Advance a day: the first day's segments seal, writes land in a fresh
	// hot segment.
	sealedDay = ingestionDay(*now)
	*now = now.Add(24 * time.Hour)
	putRecord(t, store, "pfrh_seed0001_outcome_c", "compound.work_proof_refresh.v0.1", *now, "hot outcome")
	putRecord(t, store, "hook_seed_bystander", "compound.hook_event.v0.1", *now, "unrelated hook")
	return sealedDay
}

func TestEnumerateIDsMatchesScanPrefixSet(t *testing.T) {
	store, now := proofEnumerationStore(t, time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	seedProofEnumerationFixture(t, store, now)

	for _, prefix := range []string{"pfrh_", "pfrh_seed0001_", "pfrp_", "rec_", "hook_", "zzz_"} {
		got, err := store.EnumerateIDs(prefix, true)
		if err != nil {
			t.Fatalf("enumerate %q: %v", prefix, err)
		}
		entries, err := store.Scan(ScanOptions{Prefixes: []string{prefix}, RegularOnly: true})
		if err != nil {
			t.Fatalf("scan %q: %v", prefix, err)
		}
		want := make([]string, 0, len(entries))
		for _, entry := range entries {
			want = append(want, entry.ID)
		}
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("enumerate %q = %v, scan = %v", prefix, got, want)
		}
	}
}

func TestEnumerateIDSScreensSealedSegmentsByIndexKeys(t *testing.T) {
	store, now := proofEnumerationStore(t, time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	seedProofEnumerationFixture(t, store, now)

	// pfrp_ ids live only in the SEALED day; pfrh_ spans sealed and hot.
	// Enumeration must parse the hot "other" segment and the sealed segment
	// that carries pfrp_ candidates, and never the routed bystander
	// families' segments.
	touched := observeParsedFamilies(t)
	parsed := observedSegmentPaths(t)
	got, err := store.EnumerateIDs("pfrp_", true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"pfrp_seed0001_prep_a"}) {
		t.Fatalf("pfrp_ enumeration = %v", got)
	}
	if fams := touched(); !reflect.DeepEqual(fams, []string{"other"}) {
		t.Fatalf("families parsed = %v, want only other", fams)
	}
	for _, path := range parsed() {
		if fam := segmentFamily(path); fam != "other" {
			t.Fatalf("enumeration parsed %s outside the other family", path)
		}
	}
}

func TestEnumerateIDsStopsParsingSealedSegmentsWithoutCandidates(t *testing.T) {
	store, now := proofEnumerationStore(t, time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	putRecord(t, store, "pfrh_nocand_outcome_a", "compound.work_proof_refresh.v0.1", *now, "sealed")
	sealedDay := ingestionDay(*now)
	*now = now.Add(24 * time.Hour)
	putRecord(t, store, "pfrh_nocand_outcome_b", "compound.work_proof_refresh.v0.1", *now, "hot")

	// A DIFFERENT reservation's prefix: index keys of the sealed day hold no
	// candidates, so the sealed segment must never be parsed — only the hot
	// segment is allowed to appear.
	sealedPath := segmentPath(store.engine.segRoot, "other", sealedDay, 0)
	// Establish the complete inventory once before measuring the warm path.
	if _, err := store.EnumerateIDs("pfrh_nocand_", true); err != nil {
		t.Fatal(err)
	}
	parsed := observedSegmentPaths(t)
	got, err := store.EnumerateIDs("pfrh_othereservation_", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("unrelated prefix enumerated %v, want none", got)
	}
	for _, path := range parsed() {
		if path == sealedPath {
			t.Fatal("enumeration parsed a sealed segment whose index keys held no candidates")
		}
	}
	if len(parsed()) == 0 {
		t.Fatal("falsifier is not armed: nothing was parsed at all")
	}
}

func TestEnumerateIDsRebuildsDeletedIndex(t *testing.T) {
	store, now := proofEnumerationStore(t, time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	sealedDay := seedProofEnumerationFixture(t, store, now)
	// Build the sealed day's sidecar index first (indexes are lazy), using a
	// prefix with no candidates so nothing is parsed while priming.
	if _, err := store.EnumerateIDs("zzz_prime_", true); err != nil {
		t.Fatal(err)
	}

	indexPath := filepath.Join(store.rootPath(), "index", "other", sealedDay+".idx")
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(indexPath + ".verified"); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	got, err := store.EnumerateIDs("pfrp_", true)
	if err != nil {
		t.Fatalf("enumeration after index deletion: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"pfrp_seed0001_prep_a"}) {
		t.Fatalf("post-deletion enumeration = %v", got)
	}
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("index was not rebuilt: %v", err)
	}
}

func TestEnumerateIDsRebuildsCorruptIndex(t *testing.T) {
	store, now := proofEnumerationStore(t, time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	sealedDay := seedProofEnumerationFixture(t, store, now)
	if _, err := store.EnumerateIDs("zzz_prime_", true); err != nil {
		t.Fatal(err)
	}

	indexPath := filepath.Join(store.rootPath(), "index", "other", sealedDay+".idx")
	if err := os.WriteFile(indexPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := store.EnumerateIDs("pfrh_", true)
	if err != nil {
		t.Fatalf("enumeration after index corruption: %v", err)
	}
	want := []string{"pfrh_seed0001_outcome_a", "pfrh_seed0001_outcome_b", "pfrh_seed0001_outcome_c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("post-corruption enumeration = %v, want %v", got, want)
	}
}

func TestEnumerateIDsRebuildsStaleIndexAfterSealedAppend(t *testing.T) {
	store, now := proofEnumerationStore(t, time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	putRecord(t, store, "pfrh_stale_outcome_a", "compound.work_proof_refresh.v0.1", *now, "sealed")
	sealedDay := ingestionDay(*now)
	*now = now.Add(24 * time.Hour)
	putRecord(t, store, "pfrh_stale_outcome_b", "compound.work_proof_refresh.v0.1", *now, "hot")

	// Force-build the sealed day's index, then append a frame to the sealed
	// segment behind the index's back. The stat drift must trigger a rebuild
	// so the appended outcome is still enumerated (a valid-but-stale subset
	// can never be authoritative).
	sealedPath := segmentPath(store.engine.segRoot, "other", sealedDay, 0)
	sealedIndex, err := store.engine.loadOrBuildIndex("other", segmentFile{path: sealedPath, day: sealedDay})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sealedIndex.Entries["pfrh_stale_appended"]; ok {
		t.Fatal("appended id unexpectedly pre-indexed")
	}
	frame := encodeTestFrame(t, "pfrh_stale_appended", "compound.work_proof_refresh.v0.1", *now, "appended behind the index")
	f, err := os.OpenFile(sealedPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(frame); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := store.EnumerateIDs("pfrh_stale_", true)
	if err != nil {
		t.Fatalf("enumeration after sealed append: %v", err)
	}
	want := []string{"pfrh_stale_appended", "pfrh_stale_outcome_a", "pfrh_stale_outcome_b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("post-append enumeration = %v, want %v", got, want)
	}
}

func TestEnumerateIDsFailsClosedOnCorruptSealedSegment(t *testing.T) {
	store, now := proofEnumerationStore(t, time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	putRecord(t, store, "pfrh_corrupt_outcome_a", "compound.work_proof_refresh.v0.1", *now, "sealed")
	sealedDay := ingestionDay(*now)
	*now = now.Add(24 * time.Hour)
	putRecord(t, store, "pfrh_corrupt_outcome_b", "compound.work_proof_refresh.v0.1", *now, "hot")

	// Corrupt the sealed segment so both the index rebuild and the strict
	// validated parse fail. Enumeration must refuse, never silently shrink.
	sealedPath := segmentPath(store.engine.segRoot, "other", sealedDay, 0)
	if err := os.WriteFile(sealedPath, []byte("this line is not an event frame\nneither is this one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnumerateIDs("pfrh_corrupt_", true); err == nil {
		t.Fatal("enumeration tolerated a corrupt sealed segment")
	}
}

func TestEnumerateIDsUnionsLooseLegacyRecords(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	// Migration gate OPEN (no completion marker): loose records stay
	// authority and must be unioned.
	putRecord(t, store, "pfrh_loose_outcome_a", "compound.work_proof_refresh.v0.1", now, "segmented")

	loosePath := filepath.Join(store.rootPath(), "pfrh_loose_legacy.json")
	if err := os.WriteFile(loosePath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := store.EnumerateIDs("pfrh_loose_", true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pfrh_loose_legacy", "pfrh_loose_outcome_a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loose-union enumeration = %v, want %v", got, want)
	}
}

func TestEnumerateIDsFailsClosedOnInvalidLooseMatch(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	// A loose filename matching the prefix that is not a valid record id
	// must surface, exactly like Scan.
	bad := filepath.Join(store.rootPath(), "pfrh_bad#id.json")
	if err := os.WriteFile(bad, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnumerateIDs("pfrh_bad", true); err == nil {
		t.Fatal("enumeration ignored an invalid loose entry matching the prefix")
	}
}

// The pfrh_/pfrp_ family mapping is load-bearing for scan routing too: a
// prefix scan must stop walking every family once the prefix maps.
func TestProofRefreshPrefixesRouteScansToOtherFamily(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	closeCompletionGate(t, store)
	seedAllFamilies(t, store, now)
	putRecord(t, store, "pfrh_route_outcome_a", "compound.work_proof_refresh.v0.1", now, "outcome")

	touched := observeParsedFamilies(t)
	entries, err := store.Scan(ScanOptions{Prefixes: []string{"pfrh_"}, RegularOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "pfrh_route_outcome_a" {
		t.Fatalf("scan entries = %+v, want only the pfrh_ record", entries)
	}
	if fams := touched(); !reflect.DeepEqual(fams, []string{"other"}) {
		t.Fatalf("families parsed = %v, want only other", fams)
	}
}

// observedSegmentPaths records the exact segment paths parsed during the test.
func observedSegmentPaths(t *testing.T) func() []string {
	t.Helper()
	var paths []string
	previous := scanSegmentObserver
	scanSegmentObserver = func(path string, frames int, bytes int64) {
		paths = append(paths, path)
		if previous != nil {
			previous(path, frames, bytes)
		}
	}
	t.Cleanup(func() { scanSegmentObserver = previous })
	return func() []string { return paths }
}

func encodeTestFrame(t *testing.T, id, schema string, createdAt time.Time, value string) []byte {
	t.Helper()
	record := eventRecord(id, schema, createdAt, value)
	frame, _, err := buildFrame(id, mustRecordBytes(t, record))
	if err != nil {
		t.Fatal(err)
	}
	line, err := encodeFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func mustRecordBytes(t *testing.T, record map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
