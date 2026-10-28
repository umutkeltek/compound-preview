package eventstore

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseReferenceAcceptsLogicalBareAndHistoricalShapes(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"rec_abc123",
		"compound://events/rec_abc123",
		"events/rec_abc123.json",
	} {
		t.Run(raw, func(t *testing.T) {
			ref, err := ParseReference(raw)
			if err != nil {
				t.Fatalf("ParseReference(%q): %v", raw, err)
			}
			if got, want := ref.ID(), "rec_abc123"; got != want {
				t.Fatalf("id = %q, want %q", got, want)
			}
			if got, want := ref.String(), "compound://events/rec_abc123"; got != want {
				t.Fatalf("logical ref = %q, want %q", got, want)
			}
		})
	}
}

func TestReferenceAndScanPreserveSafeHistoricalIDVocabulary(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	for _, id := range []string{"rec:legacy@host", "réc_élan", "rec_payload.json"} {
		ref, err := ReferenceForID(id)
		if err != nil {
			t.Fatalf("ReferenceForID(%q): %v", id, err)
		}
		if ref.ID() != id {
			t.Fatalf("ReferenceForID(%q).ID = %q", id, ref.ID())
		}
		if _, err := store.PutJSON(id, map[string]string{"id": id}, " "); err != nil {
			t.Fatalf("PutJSON(%q): %v", id, err)
		}
	}

	entries, err := store.Scan(ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.ID)
	}
	want := []string{"rec:legacy@host", "rec_payload.json", "réc_élan"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("historical ids = %q, want %q", got, want)
	}
	if _, err := store.Read("events/rec_payload.json.json"); err != nil {
		t.Fatalf("historical double-extension reference: %v", err)
	}
}

func TestParseReferenceRejectsPhysicalAndUnsafeShapes(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"",
		" rec_abc123",
		"rec_abc123 ",
		".",
		"..",
		"../rec_abc123",
		"events/../rec_abc123.json",
		"events/segments/2026-07/rec_abc123.json",
		"events/index/rec_abc123.json",
		"compound://events/segments/rec_abc123",
		"compound://events/rec_abc123?raw=1",
		"compound://events/rec_abc123#fragment",
		"/events/rec_abc123.json",
		`events\rec_abc123.json`,
		"events/rec_abc123",
	} {
		t.Run(strings.ReplaceAll(raw, "/", "_"), func(t *testing.T) {
			if _, err := ParseReference(raw); err == nil {
				t.Fatalf("ParseReference(%q) unexpectedly succeeded", raw)
			}
		})
	}
}

func TestPutJSONCanonicalizesRecordAndReturnsLogicalDiagnosticRef(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	for _, tc := range []struct {
		name   string
		id     string
		indent string
	}{
		{
			name:   "one space",
			id:     "rec_one",
			indent: " ",
		},
		{
			name:   "two spaces",
			id:     "rec_two",
			indent: "  ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"id": tc.id, "meta": map[string]string{"state": "ok"}}
			result, err := store.PutJSON(tc.id, payload, tc.indent)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Created || result.Reference.String() != "compound://events/"+tc.id {
				t.Fatalf("unexpected put result: %+v", result)
			}
			record, err := store.Read(tc.id)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := string(record.Bytes), `{"id":"`+tc.id+`","meta":{"state":"ok"}}`+"\n"; got != want {
				t.Fatalf("canonical bytes = %q, want %q", got, want)
			}
			if result.DiagnosticPath == "" || record.DiagnosticPath != result.DiagnosticPath {
				t.Fatalf("diagnostic path mismatch: put=%q read=%q", result.DiagnosticPath, record.DiagnosticPath)
			}
			if result.DiagnosticPath != result.Reference.String() {
				t.Fatalf("segment diagnostic ref = %q, want logical %q", result.DiagnosticPath, result.Reference)
			}
		})
	}
}

// TestPutJSONFailsClosedOnFamilyRoutingMismatch is the store-level companion to
// frame_test.go's TestBuildFrameFailsClosedOnIDPrefixSchemaFamilyDisagreement:
// it proves the E4 mismatch through the public PutJSON/Read seam a real caller
// uses, not just the internal familyOf helper. Before the fix, this PutJSON
// would silently succeed by writing the frame under its schema-derived family
// segment (transition) while the immediately-following Read -- which can only
// route by id-prefix (receipt) since it has no record bytes yet -- would miss
// it entirely: a real caller writes a record and then cannot read back its own
// write, with no error at write time to explain why.
func TestPutJSONFailsClosedOnFamilyRoutingMismatch(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	id := "rec_mismatched_family"
	record := map[string]string{
		"id":             id,
		"schema_version": "compound.transition.v0.1",
		"created_at":     "2026-07-15T00:00:00Z",
	}

	if _, err := store.PutJSON(id, record, " "); err == nil {
		t.Fatalf("PutJSON accepted an id-prefix/schema-family disagreement for %q -- the write would be unreadable by its own id", id)
	}

	if _, err := store.Read(id); err == nil {
		t.Fatalf("record %q should never have been persisted (fail-closed), but Read found it", id)
	}
}

func TestPutModesPreserveConflictExclusiveAndSkipIfPresent(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	if _, err := store.PutJSON("rec_mode", map[string]string{"id": "rec_mode", "value": "first"}, " "); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON("rec_mode", map[string]string{"id": "rec_mode", "value": "overwritten"}, " "); !errors.Is(err, ErrIDConflict) {
		t.Fatalf("conflict error = %v, want ErrIDConflict", err)
	}
	first, err := store.Read("compound://events/rec_mode")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(first.Bytes, []byte("first")) || bytes.Contains(first.Bytes, []byte("overwritten")) {
		t.Fatalf("conflict changed stored bytes: %s", first.Bytes)
	}

	if _, err := store.PutJSONExclusive("rec_mode", map[string]string{"id": "rec_mode", "value": "exclusive"}, " "); !errors.Is(err, os.ErrExist) {
		t.Fatalf("exclusive existing error = %v, want os.ErrExist", err)
	}

	created, err := store.PutJSONIfAbsent("rec_mode", map[string]string{"id": "rec_mode", "value": "skipped"}, " ")
	if err != nil {
		t.Fatal(err)
	}
	if created.Created {
		t.Fatal("if-absent existing record reported created")
	}
	stable, err := store.Read("events/rec_mode.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stable.Bytes, first.Bytes) {
		t.Fatalf("if-absent changed stable bytes\nbefore=%q\nafter=%q", first.Bytes, stable.Bytes)
	}

	created, err = store.PutJSONIfAbsent("trace_new", map[string]string{"id": "trace_new", "value": "created"}, "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !created.Created {
		t.Fatal("if-absent missing record did not report created")
	}
}

func TestExclusiveWriteParityUsesOneCallAndIgnoresNilErrorShortCount(t *testing.T) {
	t.Parallel()

	writer := &shortCountingWriter{}
	if err := writeOnce(writer, []byte("complete record")); err != nil {
		t.Fatal(err)
	}
	if writer.calls != 1 {
		t.Fatalf("exclusive writer calls = %d, want exactly one", writer.calls)
	}
}

type shortCountingWriter struct{ calls int }

func (w *shortCountingWriter) Write(data []byte) (int, error) {
	w.calls++
	return len(data) / 2, nil
}

func TestPutJSONAtomicPreservesPublicationHooksAndCleansTemp(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	var phases []string
	result, err := store.PutJSONAtomic("tr_atomic", map[string]string{"id": "tr_atomic", "state": "ok"}, "  ", AtomicOptions{
		SyncDirectory: true,
		Write: func(file *os.File, data []byte) error {
			phases = append(phases, "write")
			for len(data) > 0 {
				n, err := file.Write(data)
				if err != nil {
					return err
				}
				if n == 0 {
					return io.ErrShortWrite
				}
				data = data[n:]
			}
			return nil
		},
		BeforePublish: func() error {
			phases = append(phases, "before_publish")
			if exists, err := store.Exists("tr_atomic"); err != nil || exists {
				t.Fatalf("record visible before publish: exists=%v err=%v", exists, err)
			}
			return nil
		},
		AfterPublish: func() error {
			phases = append(phases, "after_publish")
			if exists, err := store.Exists("tr_atomic"); err != nil || !exists {
				t.Fatalf("record not visible after publish: exists=%v err=%v", exists, err)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created {
		t.Fatal("atomic put did not report created")
	}
	if want := []string{"write", "before_publish", "after_publish"}; !reflect.DeepEqual(phases, want) {
		t.Fatalf("phases = %v, want %v", phases, want)
	}
	if err := filepath.WalkDir(store.engine.segRoot, func(path string, entry os.DirEntry, err error) error {
		if err == nil && strings.Contains(entry.Name(), ".tmp-") {
			t.Errorf("atomic temp leaked: %s", path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenIsReadOnlyAndPreflightCleansItsProbe(t *testing.T) {
	t.Parallel()

	homePath := filepath.Join(t.TempDir(), "absent")
	store, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.rootPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open created the event root: %v", err)
	}
	if err := store.Preflight(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(store.rootPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != segmentsDirName || !entries[0].IsDir() {
		t.Fatalf("preflight artifacts = %v, want only segment infrastructure", entries)
	}
	segmentEntries, err := os.ReadDir(store.engine.segRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range segmentEntries {
		if strings.Contains(entry.Name(), "preflight") {
			t.Fatalf("preflight probe leaked: %s", entry.Name())
		}
	}
}

func TestCountAndRemoveUseResolvedRecords(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	for _, id := range []string{"rec_a", "rec_b", "pol_c"} {
		if _, err := store.PutJSON(id, map[string]string{"id": id}, " "); err != nil {
			t.Fatal(err)
		}
	}
	count, err := store.Count(ScanOptions{Prefixes: []string{"rec_"}})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}
	if err := store.Remove("compound://events/rec_b"); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.Exists("events/rec_b.json"); err != nil || exists {
		t.Fatalf("removed record exists=%v err=%v", exists, err)
	}
	missing, err := Open(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missing.Count(ScanOptions{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing count error = %v, want os.ErrNotExist", err)
	}
}

func TestPutJSONAtomicFailureBoundariesPreserveVisibility(t *testing.T) {
	t.Parallel()

	boom := errors.New("injected failure")
	for _, tc := range []struct {
		name          string
		opts          AtomicOptions
		wantPublished bool
	}{
		{
			name: "write",
			opts: AtomicOptions{Write: func(*os.File, []byte) error {
				return boom
			}},
		},
		{
			name: "before publish",
			opts: AtomicOptions{BeforePublish: func() error {
				return boom
			}},
		},
		{
			name: "after publish",
			opts: AtomicOptions{AfterPublish: func() error {
				return boom
			}},
			wantPublished: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openTestStore(t)
			_, err := store.PutJSONAtomic("tr_failure", map[string]string{"id": "tr_failure", "state": "new"}, "  ", tc.opts)
			if !errors.Is(err, boom) {
				t.Fatalf("error = %v, want injected failure", err)
			}
			exists, err := store.Exists("tr_failure")
			if err != nil {
				t.Fatal(err)
			}
			if exists != tc.wantPublished {
				t.Fatalf("published = %v, want %v", exists, tc.wantPublished)
			}
			entries, err := os.ReadDir(store.rootPath())
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.Contains(entry.Name(), ".tmp-") {
					t.Fatalf("atomic temp leaked: %s", entry.Name())
				}
			}
		})
	}
}

func TestScanIsDeterministicLazyAndPrefixFilteredBeforeOpen(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	for _, id := range []string{"rec_z", "act_ignored", "rec_a"} {
		if _, err := store.PutJSON(id, map[string]string{"id": id}, " "); err != nil {
			t.Fatal(err)
		}
	}
	broken := filepath.Join(t.TempDir(), "missing.json")
	if err := os.Symlink(broken, filepath.Join(store.rootPath(), "trace_broken.json")); err != nil {
		t.Fatal(err)
	}

	entries, err := store.Scan(ScanOptions{Prefixes: []string{"rec_"}, RegularOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.ID)
		if _, err := entry.Read(); err != nil {
			t.Fatalf("read %s: %v", entry.ID, err)
		}
	}
	if want := []string{"rec_a", "rec_z"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("scan ids = %v, want %v", got, want)
	}

	missingStore, err := Open(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missingStore.Scan(ScanOptions{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing scan error = %v, want os.ErrNotExist", err)
	}

	if err := os.Symlink(broken, filepath.Join(store.rootPath(), "rec_broken.json")); err != nil {
		t.Fatal(err)
	}
	lazy, err := store.Scan(ScanOptions{Prefixes: []string{"rec_b"}})
	if err != nil {
		t.Fatalf("matching unreadable scan entry was opened eagerly: %v", err)
	}
	if len(lazy) != 1 || lazy[0].ID != "rec_broken" {
		t.Fatalf("lazy entries = %+v", lazy)
	}
	if _, err := lazy[0].Read(); err == nil {
		t.Fatal("matching unreadable entry unexpectedly read")
	}
}

func TestScanFailsClosedOnMatchingInvalidLooseEventReference(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	if err := os.WriteFile(filepath.Join(store.rootPath(), "trace_bad#.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := store.Scan(ScanOptions{Prefixes: []string{"rec_"}})
	if err != nil {
		t.Fatalf("nonmatching invalid loose record affected scan: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("nonmatching invalid loose record produced entries: %+v", entries)
	}

	const invalidName = "rec_bad#.json"
	if err := os.WriteFile(filepath.Join(store.rootPath(), invalidName), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err = store.Scan(ScanOptions{Prefixes: []string{"rec_"}})
	if err == nil {
		t.Fatal("matching invalid loose event reference was silently skipped")
	}
	if entries != nil {
		t.Fatalf("failed scan returned partial entries: %+v", entries)
	}
	if !strings.Contains(err.Error(), invalidName) {
		t.Fatalf("scan error %q does not identify invalid loose filename %q", err, invalidName)
	}
}

func TestNamedStreamsRemainPhysicallyDistinct(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	if err := store.AppendHookEvent(map[string]string{"kind": "hook"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTrace(map[string]string{"kind": "trace"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendRetrievalTraffic(map[string]string{"kind": "retrieval"}); err != nil {
		t.Fatal(err)
	}

	checks := []struct {
		name string
		open func() (*os.File, error)
		want string
	}{
		{"hook", store.OpenHookEvents, `{"kind":"hook"}` + "\n"},
		{"trace", store.OpenTraces, `{"kind":"trace"}` + "\n"},
		{"retrieval", store.OpenRetrievalTraffic, `{"kind":"retrieval"}` + "\n"},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			file, err := check.open()
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			got, err := io.ReadAll(file)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != check.want {
				t.Fatalf("stream bytes = %q, want %q", got, check.want)
			}
		})
	}
	if _, err := store.WorkLogSize(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("compatibility stream write touched work log: %v", err)
	}
}

func TestWorkLogOperationsPreserveLockingPublicationAndReadOnlyTail(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	first := []byte("{\"work_id\":\"work_a\"}\n")
	if err := store.WithWorkLogLock(func() error {
		return store.AppendWorkLog(first, nil)
	}); err != nil {
		t.Fatal(err)
	}
	data, err := store.ReadWorkLog()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, first) {
		t.Fatalf("work log = %q, want %q", data, first)
	}
	if size, err := store.WorkLogSize(); err != nil || size != int64(len(first)) {
		t.Fatalf("work log size = %d, err=%v", size, err)
	}
	chunk, err := store.ReadWorkLogAt(2, 7)
	if err != nil {
		t.Fatal(err)
	}
	if want := first[2:9]; !bytes.Equal(chunk, want) {
		t.Fatalf("read-at = %q, want %q", chunk, want)
	}
	file, err := store.OpenWorkLog()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("forbidden")); err == nil {
		_ = file.Close()
		t.Fatal("read-only work log handle accepted a write")
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	second := []byte("{\"work_id\":\"work_b\"}\n")
	var phases []string
	if err := store.WithWorkLogLock(func() error {
		return store.ReplaceWorkLog(first, second, WorkLogReplaceOptions{
			WriteSuffix: func(w io.Writer, data []byte) (int, error) {
				phases = append(phases, "write")
				return w.Write(data)
			},
			BeforePublish: func() error {
				phases = append(phases, "before_publish")
				return nil
			},
			AfterPublish: func() error {
				phases = append(phases, "after_publish")
				return nil
			},
		})
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"write", "before_publish", "after_publish"}; !reflect.DeepEqual(phases, want) {
		t.Fatalf("replace phases = %v, want %v", phases, want)
	}
	data, err = store.ReadWorkLogRegular("work event ledger")
	if err != nil {
		t.Fatal(err)
	}
	if want := append(append([]byte(nil), first...), second...); !bytes.Equal(data, want) {
		t.Fatalf("replaced work log = %q, want %q", data, want)
	}
}

func TestReadRegularRejectsSymlinkedRecordAndEventsAncestor(t *testing.T) {
	t.Parallel()

	t.Run("record", func(t *testing.T) {
		store := openTestStore(t)
		outside := filepath.Join(t.TempDir(), "rec_link.json")
		if err := os.WriteFile(outside, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(store.rootPath(), "rec_link.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReadRegular("rec_link", "receipt"); err == nil {
			t.Fatal("ReadRegular followed a symlinked event record")
		}
	})

	t.Run("events ancestor", func(t *testing.T) {
		home := t.TempDir()
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "rec_link.json"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(home, "events")); err != nil {
			t.Fatal(err)
		}
		store, err := Open(home)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReadRegular("rec_link", "receipt"); err == nil {
			t.Fatal("ReadRegular followed a symlinked events ancestor")
		}
	})
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	return store
}
