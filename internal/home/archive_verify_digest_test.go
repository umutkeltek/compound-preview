package home

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVerifyArchiveReturnsExactCompressedSHA256(t *testing.T) {
	for _, format := range []string{"v1", ArchiveFormatStreamedV2, ArchiveFormatStreamedV3} {
		t.Run(format, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "home")
			now := time.Now().UTC()
			if err := Init(source, "test", now); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, "events", "runtime-events.jsonl"), []byte("synthetic event\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(t.TempDir(), "backup.tar.gz")
			if _, err := BackupWithFormat(source, archive, format, now); err != nil {
				t.Fatal(err)
			}
			report, err := VerifyArchive(archive)
			if err != nil || !report.OK {
				t.Fatalf("verify=%+v err=%v", report, err)
			}
			compressed, err := os.ReadFile(archive)
			if err != nil {
				t.Fatal(err)
			}
			expected := sha256.Sum256(compressed)
			if report.ArchiveSHA256 != hex.EncodeToString(expected[:]) {
				t.Fatalf("digest=%q want compressed archive digest", report.ArchiveSHA256)
			}
		})
	}
}

func TestVerifiedArchiveDigestRejectsSourceDrift(t *testing.T) {
	for _, mode := range []string{"append", "shrink", "replacement", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "archive-fixture")
			original := bytes.Repeat([]byte("synthetic archived bytes"), 1024)
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			before, err := file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "append":
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := f.Write([]byte("new bytes"))
				closeErr := f.Close()
				if writeErr != nil || closeErr != nil {
					t.Fatal(writeErr, closeErr)
				}
			case "shrink":
				if err := os.Truncate(path, 3); err != nil {
					t.Fatal(err)
				}
			case "replacement", "symlink":
				held := path + ".held"
				if err := os.Rename(path, held); err != nil {
					t.Fatal(err)
				}
				if mode == "replacement" {
					err = os.WriteFile(path, original, 0o600)
				} else {
					err = os.Symlink(held, path)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			digest, err := verifiedArchiveSourceSHA256(file, before, path)
			if err == nil || digest != "" {
				t.Fatalf("drift returned trusted checksum: digest=%q err=%v", digest, err)
			}
		})
	}
}
