//go:build darwin

package home

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type appendEventOnHeader struct {
	*tar.Writer
	source   string
	appended []byte
}

func (w *appendEventOnHeader) WriteHeader(header *tar.Header) error {
	if err := w.Writer.WriteHeader(header); err != nil {
		return err
	}
	file, err := os.OpenFile(w.source, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	_, err = file.Write(w.appended)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func TestArchiveEventSnapshotSurvivesLiveAppendAtHeader(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "events"), 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(rootPath, "events", "runtime-events.jsonl")
	// Preserve the realistic 120 MiB input while bounding fixture memory.
	chunk := bytes.Repeat([]byte("synthetic event\n"), 4096)
	originalHash := sha256.New()
	file, err := os.OpenFile(source, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	originalWriter := io.MultiWriter(file, originalHash)
	for i := 0; i < 2048; i++ {
		if _, err := originalWriter.Write(chunk); err != nil {
			file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	originalSize := int64(len(chunk)) * 2048
	expected := hex.EncodeToString(originalHash.Sum(nil))
	appended := bytes.Repeat([]byte("next event\n"), 1024*1024)
	before, err := os.Lstat(source)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != originalSize {
		t.Fatalf("fixture size=%d want=%d", before.Size(), originalSize)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	archive, err := os.Create(filepath.Join(t.TempDir(), "snapshot.tar"))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	writer := &appendEventOnHeader{Writer: tar.NewWriter(archive), source: source, appended: appended}
	entry, err := writeRootedArchiveEventSnapshot(writer, root, "events/runtime-events.jsonl", before)
	if err != nil {
		t.Fatalf("live append must not invalidate immutable event snapshot: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if entry.Size != originalSize || entry.SHA256 != expected {
		t.Fatalf("snapshot entry does not seal pre-append bytes: %+v", entry)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(archive)
	header, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	size, err := io.Copy(hash, reader)
	if err != nil {
		t.Fatal(err)
	}
	if header.Mode&0o777 != 0o640 {
		t.Fatalf("snapshot lost source permission metadata: %#o", header.Mode)
	}
	if header.Name != entry.Path || size != entry.Size || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
		t.Fatal("actual tar body differs from the sealed snapshot")
	}
	live, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	livePrefixHash := sha256.New()
	if n, err := io.CopyN(livePrefixHash, live, originalSize); err != nil || n != originalSize {
		t.Fatalf("live original bytes missing: n=%d err=%v", n, err)
	}
	suffix, err := io.ReadAll(live)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(livePrefixHash.Sum(nil)) != expected || !bytes.Equal(suffix, appended) {
		t.Fatal("snapshot changed or lost original live event bytes")
	}
}

func TestEventSnapshotBackupProfileKeepsV3RestoreContract(t *testing.T) {
	source := filepath.Join(t.TempDir(), "home")
	now := time.Now().UTC()
	if err := Init(source, "test", now); err != nil {
		t.Fatal(err)
	}
	event := []byte("synthetic snapshot event\n")
	if err := os.WriteFile(filepath.Join(source, "events", "runtime-events.jsonl"), event, 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "snapshot.tar.gz")
	if _, err := BackupWithFormat(source, archive, ArchiveFormatStreamedV3EventSnapshot, now); err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyArchive(archive)
	if err != nil || !verified.OK || len(verified.ArchiveSHA256) != 64 {
		t.Fatalf("verify=%+v err=%v", verified, err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if _, err := RestoreArchive(archive, restored, now); err != nil {
		t.Fatal(err)
	}
	observed, err := os.ReadFile(filepath.Join(restored, "events", "runtime-events.jsonl"))
	if err != nil || !bytes.Equal(observed, event) {
		t.Fatalf("restore lost snapshot bytes: %q %v", observed, err)
	}
	stages, err := filepath.Glob(filepath.Join(source, "tmp", "home-event-snapshot-*"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("private snapshot stage leaked after success: %v %v", stages, err)
	}
}
