package home

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveEntryDriftNamesSourceAndPreservesCause(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after string
		cause error
	}{
		{"growing", "original plus concurrent append", tar.ErrWriteTooLong},
		{"shrinking", "short", io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "runtime-events.jsonl")
			if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			// A producer changes the file after WalkDir has captured its header.
			if err := os.WriteFile(path, []byte(tc.after), 0o600); err != nil {
				t.Fatal(err)
			}
			var body bytes.Buffer
			entry, err := writeFileEntry(tar.NewWriter(&body), path, "events/runtime-events.jsonl", before)
			if !errors.Is(err, tc.cause) || !strings.Contains(err.Error(), "events/runtime-events.jsonl") {
				t.Fatalf("drift lost source or cause: entry=%+v err=%v", entry, err)
			}
			if entry.Path != "" {
				t.Fatalf("failed entry returned committed identity: %+v", entry)
			}
		})
	}
}
