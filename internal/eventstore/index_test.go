package eventstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testIndexFile struct {
	IndexVersion int                       `json:"index_version"`
	Segment      testIndexSegment          `json:"segment"`
	Entries      map[string]testIndexEntry `json:"entries"`
}

type testIndexSegment struct {
	Name             string `json:"name"`
	Size             int64  `json:"size"`
	SHA256           string `json:"sha256"`
	ModifiedUnixNano int64  `json:"modified_unix_nano"`
}

type testIndexEntry struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

type sealedIndexFixture struct {
	store       *Store
	segmentPath string
	indexPath   string
	logs        *[]string
}

func TestIndexDirectoryDeletionRebuildsByteIdenticalGet(t *testing.T) {
	fixture := newSealedIndexFixture(t, map[string]string{"rec_index_delete": "before"})

	before, err := fixture.store.Read("rec_index_delete")
	if err != nil {
		t.Fatal(err)
	}
	assertFileExists(t, fixture.indexPath)
	if err := os.RemoveAll(filepath.Join(fixture.store.rootPath(), "index")); err != nil {
		t.Fatal(err)
	}

	after, err := fixture.store.Read("rec_index_delete")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after.Bytes, before.Bytes) {
		t.Fatalf("Get bytes changed after index rebuild\nbefore=%q\nafter=%q", before.Bytes, after.Bytes)
	}
	assertFileExists(t, fixture.indexPath)
}

func TestCorruptIndexOffsetIsDetectedAndRebuilt(t *testing.T) {
	fixture := newSealedIndexFixture(t, map[string]string{
		"rec_index_offset_a": "alpha",
		"rec_index_offset_b": "bravo",
	})
	before, err := fixture.store.Read("rec_index_offset_a")
	if err != nil {
		t.Fatal(err)
	}
	index := readTestIndex(t, fixture.indexPath)
	want := index.Entries["rec_index_offset_a"]
	corrupt := want
	corrupt.Offset++
	index.Entries["rec_index_offset_a"] = corrupt
	writeTestIndex(t, fixture.indexPath, index)
	*fixture.logs = nil

	after, err := fixture.store.Read("rec_index_offset_a")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after.Bytes, before.Bytes) {
		t.Fatalf("Get bytes changed after corrupt-offset rebuild\nbefore=%q\nafter=%q", before.Bytes, after.Bytes)
	}
	assertRebuildLogged(t, *fixture.logs)
	rebuilt := readTestIndex(t, fixture.indexPath)
	if got := rebuilt.Entries["rec_index_offset_a"]; got != want {
		t.Fatalf("rebuilt entry = %+v, want %+v", got, want)
	}
}

func TestSwappedIndexEntriesAreDetectedByValidatedHitMismatch(t *testing.T) {
	fixture := newSealedIndexFixture(t, map[string]string{
		"rec_index_swap_a": "alpha",
		"rec_index_swap_b": "bravo",
	})
	before, err := fixture.store.Read("rec_index_swap_a")
	if err != nil {
		t.Fatal(err)
	}
	index := readTestIndex(t, fixture.indexPath)
	wantA := index.Entries["rec_index_swap_a"]
	wantB := index.Entries["rec_index_swap_b"]
	index.Entries["rec_index_swap_a"] = wantB
	index.Entries["rec_index_swap_b"] = wantA
	writeTestIndex(t, fixture.indexPath, index)
	*fixture.logs = nil

	after, err := fixture.store.Read("rec_index_swap_a")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after.Bytes, before.Bytes) {
		t.Fatalf("Get bytes changed after swapped-entry rebuild\nbefore=%q\nafter=%q", before.Bytes, after.Bytes)
	}
	assertRebuildLogged(t, *fixture.logs)
	rebuilt := readTestIndex(t, fixture.indexPath)
	if got := rebuilt.Entries["rec_index_swap_a"]; got != wantA {
		t.Fatalf("rebuilt a entry = %+v, want %+v", got, wantA)
	}
	if got := rebuilt.Entries["rec_index_swap_b"]; got != wantB {
		t.Fatalf("rebuilt b entry = %+v, want %+v", got, wantB)
	}
}

func TestStaleIndexSegmentHashIsRebuilt(t *testing.T) {
	fixture := newSealedIndexFixture(t, map[string]string{"rec_index_stale": "before"})
	if _, err := fixture.store.Read("rec_index_stale"); err != nil {
		t.Fatal(err)
	}
	stale := readTestIndex(t, fixture.indexPath)
	rewriteFrameValueSameLength(t, fixture.segmentPath, "rec_index_stale", "after!")
	changedInfo, err := os.Stat(fixture.segmentPath)
	if err != nil {
		t.Fatal(err)
	}
	// Match the cheap metadata guard so only the required content-hash check
	// can distinguish this stale index from its changed segment truth.
	stale.Segment.ModifiedUnixNano = changedInfo.ModTime().UnixNano()
	writeTestIndex(t, fixture.indexPath, stale)
	*fixture.logs = nil

	record, err := fixture.store.Read("rec_index_stale")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(record.Bytes, []byte(`"value":"after!"`)) {
		t.Fatalf("Get did not return changed segment truth: %s", record.Bytes)
	}
	assertRebuildLogged(t, *fixture.logs)
	rebuilt := readTestIndex(t, fixture.indexPath)
	if rebuilt.Segment.SHA256 == stale.Segment.SHA256 {
		t.Fatalf("stale source hash was retained: %s", rebuilt.Segment.SHA256)
	}
	segmentBytes, err := os.ReadFile(fixture.segmentPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rebuilt.Segment.SHA256, bytesHash(segmentBytes); got != want {
		t.Fatalf("rebuilt source hash = %s, want %s", got, want)
	}
}

func TestHotSegmentWritesNeverConsultStaleIndex(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	var logs []string
	store := openConfiguredStore(t, engineConfig{
		clock: func() time.Time { return now },
		logf:  func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
	})
	putRecord(t, store, "rec_hot_index", "compound.receipt.v0.1", now, "original")
	indexPath := filepath.Join(store.rootPath(), "index", "receipt", ingestionDay(now)+".idx")
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := []byte("{definitely-not-a-valid-index\n")
	if err := os.WriteFile(indexPath, stale, 0o644); err != nil {
		t.Fatal(err)
	}

	putRecord(t, store, "rec_hot_index_next", "compound.receipt.v0.1", now, "next")
	conflict := eventRecord("rec_hot_index", "compound.receipt.v0.1", now, "changed!")
	if _, err := store.PutJSON("rec_hot_index", conflict, "  "); !errors.Is(err, ErrIDConflict) {
		t.Fatalf("hot-segment conflict error = %v, want ErrIDConflict", err)
	}
	after, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, stale) {
		t.Fatalf("hot write consulted or rewrote stale index\nbefore=%q\nafter=%q", stale, after)
	}
	for _, entry := range logs {
		if strings.Contains(strings.ToLower(entry), "index") {
			t.Fatalf("hot write logged index activity: %q", logs)
		}
	}
}

func TestPutDetectsDelayedConflictBeyondTwentyFourHours(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	putRecord(t, store, "rec_delayed_conflict", "compound.receipt.v0.1", now, "original")

	now = now.Add(24 * time.Hour)
	putRecord(t, store, "rec_delayed_filler", "compound.receipt.v0.1", now, "filler")
	now = now.Add(48 * time.Hour)
	before := readFamilyBytes(t, store.engine.segRoot, "receipt")
	conflict := eventRecord("rec_delayed_conflict", "compound.receipt.v0.1", now, "different")
	if _, err := store.PutJSON("rec_delayed_conflict", conflict, "  "); !errors.Is(err, ErrIDConflict) {
		t.Fatalf("delayed conflict error = %v, want ErrIDConflict", err)
	}
	if after := readFamilyBytes(t, store.engine.segRoot, "receipt"); !bytes.Equal(after, before) {
		t.Fatal("delayed conflict mutated segment truth")
	}
}

func newSealedIndexFixture(t *testing.T, records map[string]string) sealedIndexFixture {
	t.Helper()
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	var logs []string
	store := openConfiguredStore(t, engineConfig{
		clock: func() time.Time { return now },
		logf:  func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
	})
	for id, value := range records {
		putRecord(t, store, id, "compound.receipt.v0.1", now, value)
	}
	sealedDay := ingestionDay(now)
	now = now.Add(24 * time.Hour)
	putRecord(t, store, "rec_index_hot", "compound.receipt.v0.1", now, "hot")
	return sealedIndexFixture{
		store:       store,
		segmentPath: segmentPath(store.engine.segRoot, "receipt", sealedDay, 0),
		indexPath:   filepath.Join(store.rootPath(), "index", "receipt", sealedDay+".idx"),
		logs:        &logs,
	}
}

func readTestIndex(t *testing.T, path string) testIndexFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var index testIndexFile
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	return index
}

func writeTestIndex(t *testing.T, path string, index testIndexFile) {
	t.Helper()
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertFileExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected index file %s: %v", path, err)
	}
}

func assertRebuildLogged(t *testing.T, logs []string) {
	t.Helper()
	for _, entry := range logs {
		if strings.Contains(strings.ToLower(entry), "rebuilt") && strings.Contains(strings.ToLower(entry), "index") {
			return
		}
	}
	t.Fatalf("index rebuild was not logged: %q", logs)
}

func rewriteFrameValueSameLength(t *testing.T, path, id, value string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	frames, err := parseSegmentBytes(path, data, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, parsed := range frames {
		if parsed.Frame.ID != id {
			continue
		}
		createdAt, err := time.Parse(time.RFC3339Nano, parsed.Frame.CreatedAt)
		if err != nil {
			t.Fatal(err)
		}
		replacement := encodedRecordFrame(t, id, parsed.Frame.SchemaVersion, createdAt, value)
		if got, want := int64(len(replacement)), parsed.End-parsed.Offset; got != want {
			t.Fatalf("replacement frame length = %d, want unchanged %d", got, want)
		}
		copy(data[parsed.Offset:parsed.End], replacement)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		future := time.Now().Add(time.Hour)
		if err := os.Chtimes(path, future, future); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("frame %s not found in %s", id, path)
}

func bytesHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
