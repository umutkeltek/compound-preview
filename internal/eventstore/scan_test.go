package eventstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestScanRetainsDecodedSegmentBytesButKeepsLooseEntriesLazy(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	writeLegacyRecord(t, store, "rec_loose", eventRecord("rec_loose", "compound.receipt.v0.1", now, "loose"))
	appendFile(t,
		segmentPath(store.engine.segRoot, "receipt", ingestionDay(now), 0),
		encodedRecordFrame(t, "rec_segment", "compound.receipt.v0.1", now, "segment"),
	)

	entries, err := store.Scan(ScanOptions{Prefixes: []string{"rec_"}})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Entry{}
	for _, entry := range entries {
		byID[entry.ID] = entry
	}
	if byID["rec_segment"].cachedBytes == nil {
		t.Fatal("segment scan discarded bytes it had already decoded")
	}
	if byID["rec_loose"].cachedBytes != nil {
		t.Fatal("legacy loose entry must stay lazy so unreadable files surface on Entry.Read")
	}

	// A segment entry reads the immutable snapshot Scan already validated; it must
	// not need a second point lookup through the segment engine.
	if err := os.RemoveAll(store.engine.segRoot); err != nil {
		t.Fatal(err)
	}
	record, err := byID["rec_segment"].Read()
	if err != nil {
		t.Fatalf("read cached segment entry after segment removal: %v", err)
	}
	if !strings.Contains(string(record.Bytes), "segment") {
		t.Fatalf("cached record = %s", record.Bytes)
	}
}

func TestGetAndScanUnionSegmentsBeforeLegacyLooseFiles(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})

	for _, id := range []string{"rec_l1", "rec_l2", "rec_l3", "rec_dual"} {
		writeLegacyRecord(t, store, id, eventRecord(id, "compound.receipt.v0.1", now, "legacy-"+id))
	}
	for _, id := range []string{"rec_s1", "rec_s2", "rec_s3", "rec_dual"} {
		appendFile(t,
			segmentPath(store.engine.segRoot, "receipt", ingestionDay(now), 0),
			encodedRecordFrame(t, id, "compound.receipt.v0.1", now, "segment-"+id),
		)
	}

	for _, raw := range []string{
		"rec_dual",
		"compound://events/rec_dual",
		"events/rec_dual.json",
	} {
		record, err := store.Read(raw)
		if err != nil {
			t.Fatalf("Read(%q): %v", raw, err)
		}
		if !strings.Contains(string(record.Bytes), "segment-rec_dual") || strings.Contains(string(record.Bytes), "legacy-rec_dual") {
			t.Fatalf("Read(%q) did not prefer segment: %s", raw, record.Bytes)
		}
		if strings.Contains(record.DiagnosticPath, "/segments/") {
			t.Fatalf("segment path escaped as durable diagnostic reference: %q", record.DiagnosticPath)
		}
	}

	entries, err := store.Scan(ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.ID)
		record, err := entry.Read()
		if err != nil {
			t.Fatalf("read scan entry %s: %v", entry.ID, err)
		}
		if entry.ID == "rec_dual" && !strings.Contains(string(record.Bytes), "segment-rec_dual") {
			t.Fatalf("scan duplicate did not resolve to segment: %s", record.Bytes)
		}
	}
	want := []string{"rec_dual", "rec_l1", "rec_l2", "rec_l3", "rec_s1", "rec_s2", "rec_s3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scan ids = %v, want %v", got, want)
	}
}

func writeLegacyRecord(t *testing.T, store *Store, id string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store.rootPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.rootPath(), id+".json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
