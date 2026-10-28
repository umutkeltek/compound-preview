package eventstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNextPutRepairsOnlyTheDeterministicallyTornTail(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, path string, line []byte)
	}{
		{
			name: "truncated frame without newline",
			mutate: func(t *testing.T, path string, line []byte) {
				t.Helper()
				appendFile(t, path, line[:len(line)/2])
			},
		},
		{
			name: "complete final frame with flipped record byte",
			mutate: func(t *testing.T, path string, line []byte) {
				t.Helper()
				corrupted := bytes.Replace(line, []byte(`"value":"torn"`), []byte(`"value":"xorn"`), 1)
				if bytes.Equal(corrupted, line) {
					t.Fatalf("test did not locate record byte in %s", line)
				}
				appendFile(t, path, corrupted)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
			var repairs []string
			store := openConfiguredStore(t, engineConfig{
				clock: func() time.Time { return now },
				logf:  func(format string, args ...any) { repairs = append(repairs, fmt.Sprintf(format, args...)) },
			})
			path := segmentPath(store.engine.segRoot, "receipt", ingestionDay(now), 0)
			for i := 0; i < 3; i++ {
				id := fmt.Sprintf("rec_prior_%d", i)
				appendFile(t, path, encodedRecordFrame(t, id, "compound.receipt.v0.1", now, fmt.Sprintf("prior-%d", i)))
			}
			torn := encodedRecordFrame(t, "rec_torn", "compound.receipt.v0.1", now, "torn")
			tc.mutate(t, path, torn)

			putRecord(t, store, "rec_after_repair", "compound.receipt.v0.1", now, "after")
			if len(repairs) != 1 || !strings.Contains(strings.ToLower(repairs[0]), "repair") {
				t.Fatalf("repair log = %q, want one repair entry", repairs)
			}
			parsed, err := parseSegment(path, false)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(parsed); got != 4 {
				t.Fatalf("frames after repair = %d, want 3 prior + new", got)
			}
			for i := 0; i < 3; i++ {
				id := fmt.Sprintf("rec_prior_%d", i)
				if parsed[i].Frame.ID != id {
					t.Fatalf("prior frame %d = %s, want %s", i, parsed[i].Frame.ID, id)
				}
			}
			if parsed[3].Frame.ID != "rec_after_repair" {
				t.Fatalf("last frame = %s, want rec_after_repair", parsed[3].Frame.ID)
			}
		})
	}
}

func TestInteriorCorruptionFailsClosedWithoutChangingBytes(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	path := segmentPath(store.engine.segRoot, "receipt", ingestionDay(now), 0)
	for i, value := range []string{"first", "middle", "last"} {
		appendFile(t, path, encodedRecordFrame(t, fmt.Sprintf("rec_interior_%d", i), "compound.receipt.v0.1", now, value))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	corrupted := bytes.Replace(data, []byte(`"value":"middle"`), []byte(`"value":"niddle"`), 1)
	if bytes.Equal(corrupted, data) {
		t.Fatal("test did not corrupt the middle frame")
	}
	if err := os.WriteFile(path, corrupted, 0o644); err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), corrupted...)

	_, err = store.PutJSON("rec_refused", eventRecord("rec_refused", "compound.receipt.v0.1", now, "new"), "  ")
	if !errors.Is(err, ErrCanonicalCorruption) {
		t.Fatalf("put error = %v, want ErrCanonicalCorruption", err)
	}
	var corruption *CanonicalCorruptionError
	if !errors.As(err, &corruption) {
		t.Fatalf("error type = %T, want *CanonicalCorruptionError", err)
	}
	if corruption.Line != 2 || corruption.Offset <= 0 || !strings.Contains(corruption.Path, filepath.Base(path)) {
		t.Fatalf("corruption location = %+v, want line 2 and positive offset", corruption)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("failed-closed interior corruption path changed segment bytes")
	}
}

func TestSealedSegmentFinalCorruptionIsNeverTailRepaired(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	oldDay := "2026-07-10"
	oldPath := segmentPath(store.engine.segRoot, "receipt", oldDay, 0)
	appendFile(t, oldPath, encodedRecordFrame(t, "rec_sealed", "compound.receipt.v0.1", now.Add(-5*24*time.Hour), "sealed"))
	putRecord(t, store, "rec_hot", "compound.receipt.v0.1", now, "hot")

	data, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	corrupted := bytes.Replace(data, []byte(`"value":"sealed"`), []byte(`"value":"xealed"`), 1)
	if bytes.Equal(corrupted, data) {
		t.Fatal("test did not corrupt sealed final frame")
	}
	if err := os.WriteFile(oldPath, corrupted, 0o644); err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), corrupted...)

	if _, err := store.PutJSON(
		"rec_hot_2",
		eventRecord("rec_hot_2", "compound.receipt.v0.1", now, "hot-2"),
		"  ",
	); !errors.Is(err, ErrCanonicalCorruption) {
		t.Fatalf("put across corrupt sealed segment error = %v, want ErrCanonicalCorruption", err)
	}
	after, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("put repaired a sealed segment")
	}
	if _, err := store.Read("rec_sealed"); !errors.Is(err, ErrCanonicalCorruption) {
		t.Fatalf("sealed Get error = %v, want ErrCanonicalCorruption", err)
	}
	if _, err := store.Scan(ScanOptions{}); !errors.Is(err, ErrCanonicalCorruption) {
		t.Fatalf("sealed Scan error = %v, want ErrCanonicalCorruption", err)
	}
}

func encodedRecordFrame(t *testing.T, id, schema string, createdAt time.Time, value string) []byte {
	t.Helper()
	record, err := canonicalJSON(eventRecord(id, schema, createdAt, value))
	if err != nil {
		t.Fatal(err)
	}
	frame, _, err := buildFrame(id, record)
	if err != nil {
		t.Fatal(err)
	}
	line, err := encodeFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func canonicalJSON(value any) ([]byte, error) {
	return json.Marshal(value)
}

func appendFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
