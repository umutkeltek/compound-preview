package eventstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var migrateNow = time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

func openMigrateStore(t *testing.T) *Store {
	t.Helper()
	return openConfiguredStore(t, engineConfig{
		clock: func() time.Time { return migrateNow },
		probe: func(string) (string, bool, error) { return "apfs", true, nil },
	})
}

func schemaForID(id string) string {
	switch {
	case strings.HasPrefix(id, "tr_"):
		return "compound.transition.v0.1"
	case strings.HasPrefix(id, "pol_"):
		return "compound.policy_decision.v0.1"
	case strings.HasPrefix(id, "hook_"):
		return "compound.hook_event.v0.1"
	case strings.HasPrefix(id, "act_"):
		return "compound.action_intent.v0.1"
	case strings.HasPrefix(id, "trace_"):
		return "compound.trace_envelope.v0.1"
	default:
		return "compound.receipt.v0.1"
	}
}

func seedLoose(t *testing.T, store *Store, id, value string) {
	t.Helper()
	writeLegacyRecord(t, store, id, eventRecord(id, schemaForID(id), migrateNow, value))
}

func archivePath(store *Store, id string) string {
	shard := recordHash([]byte(id))[:2]
	return filepath.Join(store.home, archiveTopDirName, archiveGeneration, archiveFilesDirName, shard, id+".json")
}

func countSegmentFiles(t *testing.T, root string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			count++
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return count
}

func countManifestLines(t *testing.T, store *Store) int {
	t.Helper()
	data, err := os.ReadFile(store.engine.manifestPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func reasonsContain(reasons []string, needle string) bool {
	for _, r := range reasons {
		if strings.Contains(r, needle) {
			return true
		}
	}
	return false
}

func segmentFrameCountForID(t *testing.T, store *Store, id string) int {
	t.Helper()
	count := 0
	for _, frame := range strictSegmentFrames(t, store.engine.segRoot) {
		if frame.ID == id {
			count++
		}
	}
	return count
}

// Happy path: apply folds every loose record through the append engine, archives
// the originals, closes the gate, and the reader resolves records from segments.
func TestMigrateApplyFoldsArchivesAndClosesCompletionGate(t *testing.T) {
	store := openMigrateStore(t)
	ids := []string{"rec_a", "rec_b", "tr_c", "pol_d"}
	for _, id := range ids {
		seedLoose(t, store, id, "body-"+id)
	}

	report, err := store.Migrate(MigrateOptions{Mode: ModeApply})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !report.Completion.Complete {
		t.Fatalf("gate did not close: %v", report.Completion.Reasons)
	}
	if report.Migrated != len(ids) || report.Archived != len(ids) {
		t.Fatalf("migrated=%d archived=%d, want %d", report.Migrated, report.Archived, len(ids))
	}
	if !store.MigrationComplete() {
		t.Fatal("completion.json not written")
	}
	for _, id := range ids {
		if _, err := os.Stat(filepath.Join(store.rootPath(), id+".json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("loose %s not archived away", id)
		}
		if _, err := os.Stat(archivePath(store, id)); err != nil {
			t.Fatalf("archive missing for %s: %v", id, err)
		}
		rec, err := store.Read(id)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if !strings.Contains(string(rec.Bytes), "body-"+id) {
			t.Fatalf("read %s wrong body: %s", id, rec.Bytes)
		}
		if !strings.HasPrefix(rec.DiagnosticPath, logicalReferencePrefix) {
			t.Fatalf("read %s did not resolve to a segment: %q", id, rec.DiagnosticPath)
		}
	}
	entries, err := store.Scan(ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(ids) {
		t.Fatalf("scan returned %d entries, want %d", len(entries), len(ids))
	}
}

// Crash window (a): killed after the segment append, before the manifest row.
// On resume the record is seen already-present and its manifest row is written —
// with no second segment frame.
func TestMigrateResumeAfterSegmentCommitBeforeManifest(t *testing.T) {
	store := openMigrateStore(t)
	for _, id := range []string{"rec_a", "rec_b", "rec_c"} {
		seedLoose(t, store, id, "body-"+id)
	}
	crash := errors.New("injected crash after segment append")

	_, err := store.Migrate(MigrateOptions{Mode: ModeApply, beforeManifest: func(id string) error {
		if id == "rec_b" {
			return crash
		}
		return nil
	}})
	if !errors.Is(err, crash) {
		t.Fatalf("apply error = %v, want injected crash", err)
	}
	// rec_b is committed to a segment but has no manifest row and remains loose.
	if _, found, err := store.engine.segmentFrame("rec_b", ""); err != nil || !found {
		t.Fatalf("rec_b not segment-committed pre-resume: found=%v err=%v", found, err)
	}
	m := &migrator{store: store, engine: store.engine}
	if manifest, _ := m.loadManifest(); manifest["rec_b"].ID != "" {
		t.Fatal("rec_b manifest row present before resume (crash window not real)")
	}
	if _, err := os.Stat(filepath.Join(store.rootPath(), "rec_b.json")); err != nil {
		t.Fatalf("rec_b loose original missing pre-resume: %v", err)
	}

	report, err := store.Migrate(MigrateOptions{Mode: ModeApply})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !report.Completion.Complete {
		t.Fatalf("resume did not complete: %v", report.Completion.Reasons)
	}
	if manifest, _ := m.loadManifest(); manifest["rec_b"].ID == "" {
		t.Fatal("resume did not write the missing rec_b manifest row")
	}
	if got := segmentFrameCountForID(t, store, "rec_b"); got != 1 {
		t.Fatalf("rec_b has %d segment frames, want exactly 1 (no double append)", got)
	}
}

// Crash window (b): killed after the manifest row, before the archive rename.
// On resume the original is archived and the gate closes.
func TestMigrateResumeAfterManifestBeforeArchive(t *testing.T) {
	store := openMigrateStore(t)
	for _, id := range []string{"rec_a", "rec_b", "rec_c"} {
		seedLoose(t, store, id, "body-"+id)
	}
	crash := errors.New("injected crash after manifest")

	_, err := store.Migrate(MigrateOptions{Mode: ModeApply, beforeArchive: func(id string) error {
		if id == "rec_b" {
			return crash
		}
		return nil
	}})
	if !errors.Is(err, crash) {
		t.Fatalf("apply error = %v, want injected crash", err)
	}
	m := &migrator{store: store, engine: store.engine}
	if manifest, _ := m.loadManifest(); manifest["rec_b"].ID == "" {
		t.Fatal("rec_b manifest row absent pre-resume (crash window not real)")
	}
	if _, err := os.Stat(filepath.Join(store.rootPath(), "rec_b.json")); err != nil {
		t.Fatalf("rec_b original archived despite pre-archive crash: %v", err)
	}
	if _, err := os.Stat(archivePath(store, "rec_b")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rec_b archived before archive phase, stat err=%v", err)
	}

	report, err := store.Migrate(MigrateOptions{Mode: ModeApply})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !report.Completion.Complete {
		t.Fatalf("resume did not complete: %v", report.Completion.Reasons)
	}
	if _, err := os.Stat(archivePath(store, "rec_b")); err != nil {
		t.Fatalf("resume did not archive rec_b: %v", err)
	}
	if got := segmentFrameCountForID(t, store, "rec_b"); got != 1 {
		t.Fatalf("rec_b has %d segment frames, want exactly 1", got)
	}
}

// Crash window (c): a fully completed migration re-applied is a no-op — no new
// manifest rows, still complete.
func TestMigrateReapplyAfterCompletionIsIdempotent(t *testing.T) {
	store := openMigrateStore(t)
	for _, id := range []string{"rec_a", "rec_b"} {
		seedLoose(t, store, id, "body-"+id)
	}
	if r, err := store.Migrate(MigrateOptions{Mode: ModeApply}); err != nil || !r.Completion.Complete {
		t.Fatalf("first apply: %v %v", err, r.Completion.Reasons)
	}
	before := countManifestLines(t, store)

	r, err := store.Migrate(MigrateOptions{Mode: ModeApply})
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if !r.Completion.Complete {
		t.Fatalf("re-apply not complete: %v", r.Completion.Reasons)
	}
	if r.Migrated != 0 || r.AlreadyPresent != 0 {
		t.Fatalf("re-apply reprocessed records: %+v", r)
	}
	if after := countManifestLines(t, store); after != before {
		t.Fatalf("re-apply appended manifest rows: %d -> %d", before, after)
	}
}

// Drop a record mid-migration: the completion gate must refuse (count + digest
// mismatch against the plan) and completion.json must not be written.
func TestMigrateDropOneRecordRefusesCompletion(t *testing.T) {
	store := openMigrateStore(t)
	for _, id := range []string{"rec_a", "rec_b", "rec_c", "rec_d"} {
		seedLoose(t, store, id, "body-"+id)
	}
	crash := errors.New("stop after rec_a")
	if _, err := store.Migrate(MigrateOptions{Mode: ModeApply, beforeManifest: func(id string) error {
		if id == "rec_b" {
			return crash
		}
		return nil
	}}); !errors.Is(err, crash) {
		t.Fatalf("apply error = %v, want injected crash", err)
	}
	// Simulate data loss: an unprocessed legacy file vanishes after the plan was
	// staked (rec_c has not been folded yet).
	if err := os.Remove(filepath.Join(store.rootPath(), "rec_c.json")); err != nil {
		t.Fatalf("drop rec_c: %v", err)
	}

	report, err := store.Migrate(MigrateOptions{Mode: ModeApply})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if report.Completion.Complete {
		t.Fatal("gate closed despite a dropped legacy record")
	}
	if store.MigrationComplete() {
		t.Fatal("completion.json written despite a dropped legacy record")
	}
	if !reasonsContain(report.Completion.Reasons, "legacy_discovered") &&
		!reasonsContain(report.Completion.Reasons, "rollup digest") {
		t.Fatalf("no count/digest mismatch reason surfaced: %v", report.Completion.Reasons)
	}
}

// Same id, different content: HALT the run, preserve BOTH bodies, write nothing
// to the completion gate.
func TestMigrateConflictHaltsAndPreservesBothBodies(t *testing.T) {
	store := openMigrateStore(t)
	putRecord(t, store, "rec_x", "compound.receipt.v0.1", migrateNow, "segment-body")
	writeLegacyRecord(t, store, "rec_x", eventRecord("rec_x", "compound.receipt.v0.1", migrateNow, "loose-body"))
	seedLoose(t, store, "rec_y", "body-rec_y")

	_, err := store.Migrate(MigrateOptions{Mode: ModeApply})
	if !errors.Is(err, ErrMigrationConflict) {
		t.Fatalf("apply error = %v, want ErrMigrationConflict", err)
	}
	loose, rerr := os.ReadFile(filepath.Join(store.rootPath(), "rec_x.json"))
	if rerr != nil {
		t.Fatalf("loose original not preserved in place: %v", rerr)
	}
	if !strings.Contains(string(loose), "loose-body") {
		t.Fatalf("loose original altered: %s", loose)
	}
	rec, err := store.Read("rec_x")
	if err != nil {
		t.Fatalf("read segment record: %v", err)
	}
	if !strings.Contains(string(rec.Bytes), "segment-body") {
		t.Fatalf("segment record altered: %s", rec.Bytes)
	}
	cdir := filepath.Join(store.engine.migrationRoot(), conflictsDirName, "rec_x")
	looseCopy, err := os.ReadFile(filepath.Join(cdir, "loose.json"))
	if err != nil || !strings.Contains(string(looseCopy), "loose-body") {
		t.Fatalf("conflict loose body not preserved: %v %s", err, looseCopy)
	}
	segCopy, err := os.ReadFile(filepath.Join(cdir, "segment.json"))
	if err != nil || !strings.Contains(string(segCopy), "segment-body") {
		t.Fatalf("conflict segment body not preserved: %v %s", err, segCopy)
	}
	if _, err := os.Stat(filepath.Join(cdir, "conflict.json")); err != nil {
		t.Fatalf("conflict descriptor missing: %v", err)
	}
	if store.MigrationComplete() {
		t.Fatal("completion.json written despite an unresolved conflict")
	}
}

// After the gate closes, a NEW loose file is a legacy-writer regression the
// doctor/status check surfaces, and the hot read/scan path never serves it.
func TestMigratePostCompletionDetectsLegacyWriterAndDropsLooseFallback(t *testing.T) {
	store := openMigrateStore(t)
	for _, id := range []string{"rec_a", "rec_b"} {
		seedLoose(t, store, id, "body-"+id)
	}
	if r, err := store.Migrate(MigrateOptions{Mode: ModeApply}); err != nil || !r.Completion.Complete {
		t.Fatalf("apply: %v %v", err, r.Completion.Reasons)
	}
	seedLoose(t, store, "rec_late", "late-writer")

	active, err := store.LegacyWriterActive()
	if err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Fatal("legacy writer not detected after completion")
	}
	entries, err := store.Scan(ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.ID == "rec_late" {
			t.Fatal("post-completion scan served a loose file (fallback not dropped)")
		}
	}
	if _, err := store.Read("rec_late"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("post-completion read served a loose file: %v", err)
	}
	vr, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
	if err != nil {
		t.Fatal(err)
	}
	if !vr.Completion.LegacyWriterActive {
		t.Fatal("verify did not report degraded:legacy_writer_active")
	}
}

// TestLegacyWriterActiveDetectsFrozenStreamRegrowth is the slice-5B doctor
// falsifier: after the completion gate closes, hook-events.ndjson and
// traces.ndjson are frozen (writeHookEvent/recordTrace stop appending). A stream
// appended to AFTER the gate is a legacy-writer regression that both
// LegacyWriterActive and `migrate --verify` must surface. A stream frozen at or
// before completion (or absent entirely) must NOT flag — otherwise the frozen
// evidence file would false-red every post-migration home.
func TestLegacyWriterActiveDetectsFrozenStreamRegrowth(t *testing.T) {
	for _, streamName := range []string{hookEventsStreamName, tracesStreamName} {
		t.Run(streamName, func(t *testing.T) {
			store := openMigrateStore(t)
			seedLoose(t, store, "rec_a", "body-a")
			if r, err := store.Migrate(MigrateOptions{Mode: ModeApply}); err != nil || !r.Completion.Complete {
				t.Fatalf("apply: %v %v", err, r.Completion.Reasons)
			}
			completionInfo, err := os.Stat(store.engine.completionPath())
			if err != nil {
				t.Fatal(err)
			}

			// No stream at all: the healthy pure-segment case never flags.
			if active, err := store.LegacyWriterActive(); err != nil || active {
				t.Fatalf("absent stream must not flag legacy writer: active=%v err=%v", active, err)
			}

			streamPath := filepath.Join(store.home, "events", streamName)
			if err := os.WriteFile(streamPath, []byte(`{"id":"x"}`+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			// Frozen at/before the gate boundary: not a regression.
			frozen := completionInfo.ModTime().Add(-time.Second)
			if err := os.Chtimes(streamPath, frozen, frozen); err != nil {
				t.Fatal(err)
			}
			if active, err := store.LegacyWriterActive(); err != nil || active {
				t.Fatalf("stream frozen before the gate must not flag: active=%v err=%v", active, err)
			}

			// Written strictly after the gate boundary: a legacy-writer regression.
			future := completionInfo.ModTime().Add(time.Hour)
			if err := os.Chtimes(streamPath, future, future); err != nil {
				t.Fatal(err)
			}
			active, err := store.LegacyWriterActive()
			if err != nil {
				t.Fatal(err)
			}
			if !active {
				t.Fatalf("%s appended after completion must flag legacy writer", streamName)
			}
			vr, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
			if err != nil {
				t.Fatal(err)
			}
			if !vr.Completion.LegacyWriterActive {
				t.Fatal("verify did not report degraded:legacy_writer_active for stream regrowth")
			}
		})
	}
}

// Structural perf falsifier: after migration a scan opens O(families×days)
// segment files, never O(records), and the loose directory is no longer walked.
func TestMigratePostCompletionScanIsSegmentStructuralNotPerRecord(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	store := openMigrateStore(t)
	const records = 120
	for i := 0; i < records; i++ {
		seedLoose(t, store, fmt.Sprintf("rec_bulk_%03d", i), "b")
	}
	if r, err := store.Migrate(MigrateOptions{Mode: ModeApply}); err != nil || !r.Completion.Complete {
		t.Fatalf("apply: %v %v", err, r.Completion.Reasons)
	}
	segFiles := countSegmentFiles(t, store.engine.segRoot)
	if segFiles >= records {
		t.Fatalf("scan opens %d segment files for %d records — not sub-linear", segFiles, records)
	}
	if segFiles > 4 {
		t.Fatalf("expected O(families×days) segment files, got %d", segFiles)
	}
	entries, err := store.Scan(ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != records {
		t.Fatalf("scan returned %d, want %d", len(entries), records)
	}
	// A post-completion decoy loose file must stay invisible to the hot scan.
	seedLoose(t, store, "rec_decoy", "decoy")
	entries, err = store.Scan(ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != records {
		t.Fatalf("post-completion scan returned %d (loose fallback not dropped)", len(entries))
	}
}

// A corrupt canonical segment record fails CLOSED post-completion and is never
// silently served from the intact archived copy.
func TestMigratePostCompletionCorruptSegmentFailsClosedNotArchiveFallback(t *testing.T) {
	store := openMigrateStore(t)
	for _, id := range []string{"rec_a", "rec_b", "rec_c"} {
		seedLoose(t, store, id, "body-"+id)
	}
	if r, err := store.Migrate(MigrateOptions{Mode: ModeApply}); err != nil || !r.Completion.Complete {
		t.Fatalf("apply: %v %v", err, r.Completion.Reasons)
	}
	segPath := segmentPath(store.engine.segRoot, "receipt", ingestionDay(migrateNow), 0)
	data, err := os.ReadFile(segPath)
	if err != nil {
		t.Fatal(err)
	}
	// rec_a is the first (interior) frame; flip its value without resizing.
	corrupted := bytes.Replace(data, []byte("body-rec_a"), []byte("body-rec_Z"), 1)
	if bytes.Equal(corrupted, data) {
		t.Fatal("failed to corrupt an interior migrated frame")
	}
	if err := os.WriteFile(segPath, corrupted, 0o644); err != nil {
		t.Fatal(err)
	}
	arc, err := os.ReadFile(archivePath(store, "rec_a"))
	if err != nil {
		t.Fatalf("archived copy missing: %v", err)
	}
	if !strings.Contains(string(arc), "body-rec_a") {
		t.Fatal("archived copy unexpectedly altered")
	}
	rec, rerr := store.Read("rec_a")
	if !errors.Is(rerr, ErrCanonicalCorruption) {
		t.Fatalf("read of corrupt segment record = %v (bytes=%s); want ErrCanonicalCorruption, never the archive", rerr, rec.Bytes)
	}
}

// Legacy path-shaped references still resolve to segment records after cutover.
func TestMigratePostCompletionLegacyPathReferenceResolvesToSegment(t *testing.T) {
	store := openMigrateStore(t)
	seedLoose(t, store, "rec_ref", "ref-body")
	if r, err := store.Migrate(MigrateOptions{Mode: ModeApply}); err != nil || !r.Completion.Complete {
		t.Fatalf("apply: %v %v", err, r.Completion.Reasons)
	}
	for _, raw := range []string{"rec_ref", "compound://events/rec_ref", "events/rec_ref.json"} {
		rec, err := store.Read(raw)
		if err != nil {
			t.Fatalf("Read(%q): %v", raw, err)
		}
		if !strings.Contains(string(rec.Bytes), "ref-body") {
			t.Fatalf("Read(%q) wrong body: %s", raw, rec.Bytes)
		}
		if strings.Contains(rec.DiagnosticPath, "/segments/") || strings.Contains(rec.DiagnosticPath, ".json") {
			t.Fatalf("Read(%q) leaked a physical path: %q", raw, rec.DiagnosticPath)
		}
	}
}

// Dry-run classifies without writing; a filename/embedded id mismatch is rejected
// and blocks a real completion.
func TestMigrateDryRunClassifiesAndRejectionBlocksCompletion(t *testing.T) {
	store := openMigrateStore(t)
	seedLoose(t, store, "rec_ok", "ok")
	// Filename rec_bad, embedded id rec_other.
	writeLegacyRecord(t, store, "rec_bad", eventRecord("rec_other", "compound.receipt.v0.1", migrateNow, "x"))

	dry, err := store.Migrate(MigrateOptions{Mode: ModeDryRun})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if dry.Migrated != 1 || dry.Rejected != 1 {
		t.Fatalf("dry-run classification = migrate:%d reject:%d, want 1/1", dry.Migrated, dry.Rejected)
	}
	if _, err := os.Stat(store.engine.planPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dry-run wrote a plan")
	}
	if _, err := os.Stat(store.engine.manifestPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dry-run wrote a manifest")
	}
	if _, err := os.Stat(filepath.Join(store.rootPath(), "rec_ok.json")); err != nil {
		t.Fatalf("dry-run mutated the loose layer: %v", err)
	}

	apply, err := store.Migrate(MigrateOptions{Mode: ModeApply})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if apply.Completion.Complete {
		t.Fatal("gate closed with an unresolved rejected record")
	}
	if apply.Rejected != 1 {
		t.Fatalf("apply rejected=%d, want 1", apply.Rejected)
	}
	if !reasonsContain(apply.Completion.Reasons, "rejected") {
		t.Fatalf("no rejected-record reason surfaced: %v", apply.Completion.Reasons)
	}
}

// FALSIFIER (post-plan manifest attribution): a manifest row recorded after the
// published completion instant is runtime growth, never legacy census. The
// frozen plan comparison must exclude it; verify stays green only while the row
// keeps segment authority AND its archived original with exact source bytes,
// and reports it separately. Real-home shape: find_b642a87db247's 294 rows
// folded by a pre-refusal binary on 2026-07-17.
func TestMigrateVerifyAttributesPostPlanManifestRowsSeparately(t *testing.T) {
	store := openMigrateStore(t)
	for _, id := range []string{"rec_pa", "tr_pb"} {
		seedLoose(t, store, id, "body-"+id)
	}
	report, err := store.Migrate(MigrateOptions{Mode: ModeApply})
	if err != nil || !report.Completion.Complete {
		t.Fatalf("apply: err=%v reasons=%v", err, report.Completion.Reasons)
	}

	// Reproduce the incident fold exactly: segment append + legacy manifest row
	// + archived original, all recorded after the completion instant.
	const id = "act_postplan1"
	putRecord(t, store, id, schemaForID(id), migrateNow.Add(time.Hour), "post-plan-body")
	var canonical string
	for _, frame := range strictSegmentFrames(t, store.engine.segRoot) {
		if frame.ID == id {
			canonical = frame.SHA256
		}
	}
	if canonical == "" {
		t.Fatal("post-plan record missing from segments")
	}
	originalBytes := []byte(`{"id":"` + id + `","incident":"loose original"}` + "\n")
	manifestData, err := os.ReadFile(store.engine.manifestPath())
	if err != nil {
		t.Fatal(err)
	}
	var firstRow manifestRow
	if err := json.Unmarshal([]byte(strings.SplitN(string(manifestData), "\n", 2)[0]), &firstRow); err != nil {
		t.Fatal(err)
	}
	row := manifestRow{
		Schema: firstRow.Schema, ID: id,
		SourceSHA256: recordHash(originalBytes), CanonicalSHA256: canonical,
		Family: "action", Status: statusMigrated,
		RecordedAt: migrateNow.Add(2 * time.Hour).UTC().Format(time.RFC3339Nano),
	}
	line, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(store.engine.manifestPath(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	archive := archivePath(store, id)
	if err := os.MkdirAll(filepath.Dir(archive), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, originalBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	verify, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !verify.Completion.Complete || verify.Completion.PostPlanRows != 1 {
		t.Fatalf("post-plan row with archive+segment authority did not verify green: post_plan_rows=%d reasons=%v", verify.Completion.PostPlanRows, verify.Completion.Reasons)
	}
	if verify.Completion.DiscoveredExpected != 2 || verify.Completion.Migrated != 2 {
		t.Fatalf("post-plan row leaked into the frozen census: %+v", verify.Completion)
	}

	// A post-plan row whose archived original diverges is unrecoverable growth.
	if err := os.WriteFile(archive, []byte(`{"tampered":true}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	verify, err = store.Migrate(MigrateOptions{Mode: ModeVerify})
	if err != nil {
		t.Fatalf("verify tampered archive: %v", err)
	}
	if verify.Completion.Complete || !reasonsContain(verify.Completion.Reasons, "archived original") {
		t.Fatalf("divergent post-plan archive was not refused: %+v", verify.Completion.Reasons)
	}

	// A post-plan row with no archived original at all fails closed.
	if err := os.Remove(archive); err != nil {
		t.Fatal(err)
	}
	verify, err = store.Migrate(MigrateOptions{Mode: ModeVerify})
	if err != nil {
		t.Fatalf("verify missing archive: %v", err)
	}
	if verify.Completion.Complete || !reasonsContain(verify.Completion.Reasons, "archived original") {
		t.Fatalf("archive-less post-plan row was not refused: %+v", verify.Completion.Reasons)
	}
}
