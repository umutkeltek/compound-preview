package home

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStreamedBackupRoundTrip(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	homePath := filepath.Join(t.TempDir(), "home")
	if err := Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first.json", "second.json"} {
		if err := os.WriteFile(filepath.Join(homePath, "ledger", name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	archivePath := filepath.Join(t.TempDir(), "home.tar.gz")
	if _, err := BackupWithFormat(homePath, archivePath, "streamed-v2", now); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	hdr, err := tar.NewReader(gz).Next()
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Name != "compound-home-archive-header.json" || hdr.Size > 4<<10 {
		t.Fatalf("first entry = %q (%d bytes)", hdr.Name, hdr.Size)
	}
	if verify, err := VerifyArchive(archivePath); err != nil || !verify.OK {
		t.Fatalf("verify = %+v, %v", verify, err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if _, err := RestoreArchive(archivePath, target, now); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first.json", "second.json"} {
		got, err := os.ReadFile(filepath.Join(target, "ledger", name))
		if err != nil || string(got) != name {
			t.Fatalf("restored %s = %q, %v", name, got, err)
		}
	}
}

func TestStreamedArchiveRejectsBrokenStructureBeforeRestore(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	homePath := filepath.Join(t.TempDir(), "home")
	if err := Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "ledger", "first.json"), []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "valid.tar.gz")
	if _, err := BackupWithFormat(homePath, archivePath, ArchiveFormatStreamedV2, now); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func([]restoreRawTarEntry) []restoreRawTarEntry
	}{
		{"missing_chunk", func(rows []restoreRawTarEntry) []restoreRawTarEntry {
			for i, row := range rows {
				if strings.HasPrefix(row.hdr.Name, archiveStreamChunkPrefix) {
					return append(rows[:i], rows[i+1:]...)
				}
			}
			return rows
		}},
		{"missing_index", func(rows []restoreRawTarEntry) []restoreRawTarEntry {
			return rows[:len(rows)-1]
		}},
		{"changed_chunk", func(rows []restoreRawTarEntry) []restoreRawTarEntry {
			for i := range rows {
				if strings.HasPrefix(rows[i].hdr.Name, archiveStreamChunkPrefix) {
					rows[i].body = bytes.Replace(rows[i].body, []byte(`"sha256":"`), []byte(`"sha256":"0`), 1)
					break
				}
			}
			return rows
		}},
		{"duplicate_chunk_field", func(rows []restoreRawTarEntry) []restoreRawTarEntry {
			for i := range rows {
				if !strings.HasPrefix(rows[i].hdr.Name, archiveStreamChunkPrefix) {
					continue
				}
				needle := []byte(`"path":"` + ManifestName + `"`)
				rows[i].body = bytes.Replace(rows[i].body, needle, append(append([]byte(nil), needle...), append([]byte(","), needle...)...), 1)
				sum := sha256.Sum256(rows[i].body)
				for j := range rows {
					if rows[j].hdr.Name != archiveStreamIndexName {
						continue
					}
					var index archiveStreamIndex
					if err := json.Unmarshal(rows[j].body, &index); err != nil {
						t.Fatal(err)
					}
					index.Chunks[0].Size = int64(len(rows[i].body))
					index.Chunks[0].SHA256 = fmt.Sprintf("%x", sum)
					rows[j].body, _ = json.Marshal(index)
					break
				}
				break
			}
			return rows
		}},
		{"duplicate_across_chunk", func(rows []restoreRawTarEntry) []restoreRawTarEntry {
			for i, row := range rows {
				if strings.HasPrefix(row.hdr.Name, archiveStreamChunkPrefix) {
					dup := restoreRawTarEntry{hdr: &tar.Header{Name: "ledger/first.json", Typeflag: tar.TypeReg, Mode: 0o600}, body: []byte("first")}
					return append(rows[:i+1], append([]restoreRawTarEntry{dup}, rows[i+1:]...)...)
				}
			}
			return rows
		}},
		{"extra_after_index", func(rows []restoreRawTarEntry) []restoreRawTarEntry {
			return append(rows, restoreRawTarEntry{hdr: &tar.Header{Name: "ledger/extra.json", Typeflag: tar.TypeReg, Mode: 0o600}, body: []byte("extra")})
		}},
		{"excluded_export_habitat", func(rows []restoreRawTarEntry) []restoreRawTarEntry {
			body := []byte("excluded")
			sum := sha256.Sum256(body)
			entry := ArchiveEntry{Path: "exports/private.json", Size: int64(len(body)), SHA256: fmt.Sprintf("%x", sum)}
			for i := range rows {
				if !strings.HasPrefix(rows[i].hdr.Name, archiveStreamChunkPrefix) {
					continue
				}
				var records []ArchiveEntry
				if err := json.Unmarshal(rows[i].body, &records); err != nil {
					t.Fatal(err)
				}
				records = append(records, entry)
				rows[i].body, _ = json.Marshal(records)
				chunkSum := sha256.Sum256(rows[i].body)
				for j := range rows {
					if rows[j].hdr.Name != archiveStreamIndexName {
						continue
					}
					var index archiveStreamIndex
					if err := json.Unmarshal(rows[j].body, &index); err != nil {
						t.Fatal(err)
					}
					index.Files++
					index.Bytes += int64(len(body))
					index.Members += 2
					index.Chunks[0].Records++
					index.Chunks[0].Size = int64(len(rows[i].body))
					index.Chunks[0].SHA256 = fmt.Sprintf("%x", chunkSum)
					rows[j].body, _ = json.Marshal(index)
					break
				}
				injected := []restoreRawTarEntry{
					{hdr: &tar.Header{Name: "exports/", Typeflag: tar.TypeDir, Mode: 0o755}},
					{hdr: &tar.Header{Name: entry.Path, Typeflag: tar.TypeReg, Mode: 0o600}, body: body},
				}
				return append(rows[:i], append(injected, rows[i:]...)...)
			}
			return rows
		}},
		{"unknown_header_field", func(rows []restoreRawTarEntry) []restoreRawTarEntry {
			rows[0].body = append(rows[0].body[:len(rows[0].body)-1], []byte(`,"unexpected":true}`)...)
			return rows
		}},
		{"path_traversal", func(rows []restoreRawTarEntry) []restoreRawTarEntry {
			for i := range rows {
				if rows[i].hdr.Name == "ledger/first.json" {
					rows[i].hdr.Name = "../outside"
					break
				}
			}
			return rows
		}},
		{"symlink", func(rows []restoreRawTarEntry) []restoreRawTarEntry {
			for i := range rows {
				if rows[i].hdr.Name == "ledger/first.json" {
					rows[i].hdr.Typeflag = tar.TypeSymlink
					rows[i].hdr.Linkname = "outside"
					rows[i].body = nil
					break
				}
			}
			return rows
		}},
		{"oversized_pax_metadata", func(rows []restoreRawTarEntry) []restoreRawTarEntry {
			for i := range rows {
				if rows[i].hdr.Name == ManifestName {
					rows[i].hdr.Format = tar.FormatPAX
					rows[i].hdr.PAXRecords = map[string]string{"comment": strings.Repeat("x", 20<<10)}
					break
				}
			}
			return rows
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows := tc.mutate(readStreamedRawRows(t, archivePath))
			broken := writeRestoreRawTarGz(t, rows)
			if report, err := VerifyArchive(broken); err == nil || report.OK {
				t.Fatalf("broken archive verified: %+v, %v", report, err)
			}
			target := filepath.Join(t.TempDir(), "restore")
			if report, err := RestoreArchive(broken, target, now); err == nil || report.TargetHome != "" {
				t.Fatalf("broken archive restored: %+v, %v", report, err)
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatalf("broken archive published target: %v", err)
			}
		})
	}
}

func readStreamedRawRows(t *testing.T, path string) []restoreRawTarEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var rows []restoreRawTarEntry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		copyHdr := *hdr
		rows = append(rows, restoreRawTarEntry{hdr: &copyHdr, body: body})
	}
	return rows
}

func TestRestoreCleanupRemovesEachFileDuringTraversal(t *testing.T) {
	path := t.TempDir()
	if err := os.WriteFile(filepath.Join(path, "item"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	prior := restoreCleanupWalk
	defer func() { restoreCleanupWalk = prior }()
	restoreCleanupWalk = func(root *os.Root, fn fs.WalkDirFunc) error {
		info, err := root.Lstat("item")
		if err != nil {
			return err
		}
		if err := fn("item", fs.FileInfoToDirEntry(info), nil); err != nil {
			return err
		}
		if _, err := root.Lstat("item"); !os.IsNotExist(err) {
			return fmt.Errorf("cleanup retained traversed file: %v", err)
		}
		return nil
	}
	if err := clearRestoreStaging(root, path); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreCleanupRemovesMoreThanOneDirectoryBatch(t *testing.T) {
	path := t.TempDir()
	for i := 0; i < 2*archiveWalkBatch+3; i++ {
		name := filepath.Join(path, fmt.Sprintf("item-%04d", i))
		if err := os.WriteFile(name, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := clearRestoreStaging(root, path); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cleanup retained %d entries: %v", len(entries), err)
	}
}

func TestStreamedBackupPinsManifestToOpenedHomeRoot(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	base := t.TempDir()
	homePath := filepath.Join(base, "home")
	otherPath := filepath.Join(base, "other")
	if err := Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	if err := Init(otherPath, "test", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(homePath, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := os.ReadFile(filepath.Join(otherPath, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(original, replacement) {
		t.Fatal("test manifests must differ")
	}
	prior := archiveStreamRootOpened
	defer func() { archiveStreamRootOpened = prior }()
	archiveStreamRootOpened = func(string) error {
		if err := os.Rename(homePath, filepath.Join(base, "pinned")); err != nil {
			return err
		}
		return os.Rename(otherPath, homePath)
	}
	archivePath := filepath.Join(t.TempDir(), "archive.tar.gz")
	if _, err := BackupWithFormat(homePath, archivePath, ArchiveFormatStreamedV2, now); err != nil {
		t.Fatal(err)
	}
	var archived []byte
	for _, row := range readStreamedRawRows(t, archivePath) {
		if row.hdr.Name == ManifestName {
			archived = row.body
		}
	}
	if !bytes.Equal(archived, original) {
		t.Fatalf("archive mixed pinned root with replacement manifest")
	}
}

func TestStreamedPrivateCustodyScansMultipleBatchesAndRejectsLateDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "home")
	if err := Init(path, "test", time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(path, "private", "artifacts", "sha256")
	var last string
	for i := 0; i < archiveWalkBatch+2; i++ {
		body := []byte(fmt.Sprintf("artifact-%04d", i))
		sum := sha256.Sum256(body)
		last = filepath.Join(dir, fmt.Sprintf("%x", sum))
		if err := os.WriteFile(last, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := validateStreamedPrivateCustody(root, path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(last, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateStreamedPrivateCustody(root, path); err == nil {
		t.Fatal("private custody accepted mode drift")
	}
}

func TestStreamedRestoreRejectsEmptyDirectoryMutationBeforePublish(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	homePath := filepath.Join(t.TempDir(), "home")
	if err := Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(homePath, "ledger", "empty-original"), 0o700); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "archive.tar.gz")
	if _, err := BackupWithFormat(homePath, archivePath, ArchiveFormatStreamedV2, now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(string) error
	}{
		{"missing", func(staging string) error { return os.Remove(filepath.Join(staging, "ledger", "empty-original")) }},
		{"extra", func(staging string) error { return os.Mkdir(filepath.Join(staging, "ledger", "empty-added"), 0o700) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prior := restoreBeforePublish
			defer func() { restoreBeforePublish = prior }()
			restoreBeforePublish = func(staging, _ string) error { return tc.mutate(staging) }
			target := filepath.Join(t.TempDir(), "restored")
			if report, err := RestoreArchive(archivePath, target, now); err == nil || report.TargetHome != "" {
				t.Fatalf("directory mutation published: %+v, %v", report, err)
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatalf("restore target exists after refusal: %v", err)
			}
		})
	}
}

func TestLegacyArchiveWithLargeFirstPAXHeaderStillVerifies(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	homePath := filepath.Join(t.TempDir(), "home")
	if err := Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "legacy.tar.gz")
	if _, err := Backup(homePath, archivePath, now); err != nil {
		t.Fatal(err)
	}
	rows := readStreamedRawRows(t, archivePath)
	rows[0].hdr.Format = tar.FormatPAX
	rows[0].hdr.PAXRecords = map[string]string{"comment": strings.Repeat("x", 20<<10)}
	withPAX := writeRestoreRawTarGz(t, rows)
	if report, err := VerifyArchive(withPAX); err != nil || !report.OK {
		t.Fatalf("v0.1 archive with first PAX metadata rejected: %+v, %v", report, err)
	}
}

func TestStreamedArchiveRejectsRegularFileTrailingSlash(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	homePath := filepath.Join(t.TempDir(), "home")
	if err := Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "ledger", "first.json"), []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "valid.tar.gz")
	if _, err := BackupWithFormat(homePath, archivePath, ArchiveFormatStreamedV2, now); err != nil {
		t.Fatal(err)
	}
	rows := readStreamedRawRows(t, archivePath)
	for i := range rows {
		if !strings.HasPrefix(rows[i].hdr.Name, archiveStreamChunkPrefix) {
			continue
		}
		var records []ArchiveEntry
		if err := json.Unmarshal(rows[i].body, &records); err != nil {
			t.Fatal(err)
		}
		for j := range records {
			if records[j].Path == "ledger/first.json" {
				records[j].Path += "/"
			}
		}
		rows[i].body, _ = json.Marshal(records)
		sum := sha256.Sum256(rows[i].body)
		for j := range rows {
			if rows[j].hdr.Name == archiveStreamIndexName {
				var index archiveStreamIndex
				if err := json.Unmarshal(rows[j].body, &index); err != nil {
					t.Fatal(err)
				}
				index.Chunks[0].Size = int64(len(rows[i].body))
				index.Chunks[0].SHA256 = fmt.Sprintf("%x", sum)
				rows[j].body, _ = json.Marshal(index)
			}
		}
		break
	}
	crafted := patchRegularTarHeaderName(t, writeRestoreRawTarGz(t, rows), "ledger/first.json", "ledger/first.json/")
	if report, err := VerifyArchive(crafted); err == nil || report.OK {
		t.Fatalf("regular trailing-slash entry verified: %+v, %v", report, err)
	}
}

func patchRegularTarHeaderName(t *testing.T, path, from, to string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	decoded, readErr := io.ReadAll(gz)
	closeErr := errors.Join(gz.Close(), f.Close())
	if err := errors.Join(readErr, closeErr); err != nil {
		t.Fatal(err)
	}
	changed := false
	for offset := 0; offset+512 <= len(decoded); offset += 512 {
		hdr := decoded[offset : offset+512]
		if string(bytes.TrimRight(hdr[:100], "\x00")) != from || string(hdr[257:262]) != "ustar" || hdr[156] != tar.TypeReg {
			continue
		}
		clear(hdr[:100])
		copy(hdr[:100], to)
		for i := 148; i < 156; i++ {
			hdr[i] = ' '
		}
		var sum int64
		for _, b := range hdr {
			sum += int64(b)
		}
		copy(hdr[148:156], fmt.Sprintf("%06o\x00 ", sum))
		changed = true
		break
	}
	if !changed {
		t.Fatal("regular tar header not found")
	}
	outPath := filepath.Join(t.TempDir(), "crafted.tar.gz")
	out, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	zip := gzip.NewWriter(out)
	_, writeErr := zip.Write(decoded)
	if err := errors.Join(writeErr, zip.Close(), out.Close()); err != nil {
		t.Fatal(err)
	}
	return outPath
}

func TestStreamedRestoreCompletesAbsentPrivateCustodyDirectories(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	homePath := filepath.Join(t.TempDir(), "home")
	if err := Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	for i := len(privateCustodyKnownDirs) - 1; i >= 0; i-- {
		if err := os.Remove(filepath.Join(homePath, privateCustodyKnownDirs[i])); err != nil {
			t.Fatal(err)
		}
	}
	archivePath := filepath.Join(t.TempDir(), "archive.tar.gz")
	if _, err := BackupWithFormat(homePath, archivePath, ArchiveFormatStreamedV2, now); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if _, err := RestoreArchive(archivePath, target, now); err != nil {
		t.Fatal(err)
	}
	for _, rel := range privateCustodyKnownDirs {
		if info, err := os.Lstat(filepath.Join(target, rel)); err != nil || !info.IsDir() {
			t.Fatalf("private custody directory %s missing: %v", rel, err)
		}
	}
}

func TestStreamedRestoreHonorsInjectedResourceLimits(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	homePath := filepath.Join(t.TempDir(), "home")
	if err := Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "archive.tar.gz")
	if _, err := BackupWithFormat(homePath, archivePath, ArchiveFormatStreamedV2, now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		limit func(*archiveReadLimits)
	}{
		{"entries", func(l *archiveReadLimits) { l.MaxEntries = 1 }},
		{"expanded", func(l *archiveReadLimits) { l.MaxEntryBytes = 1; l.MaxExpandedBytes = 1 }},
		{"source", func(l *archiveReadLimits) { l.MaxSourceBytes = 1 }},
		{"decoded", func(l *archiveReadLimits) { l.MaxEntryBytes = 1; l.MaxExpandedBytes = 1; l.MaxDecodedBytes = 1 }},
		{"path_bytes", func(l *archiveReadLimits) { l.MaxPathBytes = 1 }},
		{"path_depth", func(l *archiveReadLimits) { l.MaxPathDepth = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits := defaultArchiveReadLimits
			tc.limit(&limits)
			target := filepath.Join(t.TempDir(), "restored")
			if _, err := restoreArchiveWithLimits(archivePath, target, now, limits); err == nil {
				t.Fatal("restore accepted lower resource limit")
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatalf("limited restore published target: %v", err)
			}
		})
	}
}

func TestStreamedReaderRejectsEachMetadataBudget(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	homePath := filepath.Join(t.TempDir(), "home")
	if err := Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "archive.tar.gz")
	if _, err := BackupWithFormat(homePath, archivePath, ArchiveFormatStreamedV2, now); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		lower func(*archiveStreamMetadataLimits)
	}{
		{"header_bytes", func(l *archiveStreamMetadataLimits) { l.HeaderBytes = 1 }},
		{"chunk_bytes", func(l *archiveStreamMetadataLimits) { l.ChunkBytes = 1 }},
		{"index_bytes", func(l *archiveStreamMetadataLimits) { l.IndexBytes = 1 }},
		{"total_metadata", func(l *archiveStreamMetadataLimits) { l.TotalBytes = 1 }},
		{"chunk_records", func(l *archiveStreamMetadataLimits) { l.ChunkRecords = 1 }},
		{"header_overhead", func(l *archiveStreamMetadataLimits) { l.HeaderOverhead = 512 }},
		{"total_header_overhead", func(l *archiveStreamMetadataLimits) { l.TotalHeaderOverhead = 512 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.Open(archivePath)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			limits := defaultArchiveStreamMetadataLimits
			tc.lower(&limits)
			if report, err := readStreamedArchiveWithMetadataLimits(archivePath, f, nil, nil, nil, archiveStreamTarLimits(), limits); err == nil || report.OK {
				t.Fatalf("archive passed lowered %s budget: %+v, %v", tc.name, report, err)
			}
		})
	}
}

func TestStreamedWriterMetadataRefusalLeavesNoArchive(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	homePath := filepath.Join(t.TempDir(), "home")
	if err := Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	prior := defaultArchiveStreamMetadataLimits
	defer func() { defaultArchiveStreamMetadataLimits = prior }()
	for _, tc := range []struct {
		name  string
		lower func(*archiveStreamMetadataLimits)
	}{
		{"header", func(l *archiveStreamMetadataLimits) { l.HeaderBytes = 1 }},
		{"chunk", func(l *archiveStreamMetadataLimits) { l.ChunkBytes = 1 }},
		{"index", func(l *archiveStreamMetadataLimits) { l.IndexBytes = 1 }},
		{"total", func(l *archiveStreamMetadataLimits) { l.TotalBytes = 1 }},
		{"chunks", func(l *archiveStreamMetadataLimits) { l.Chunks = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defaultArchiveStreamMetadataLimits = prior
			tc.lower(&defaultArchiveStreamMetadataLimits)
			target := filepath.Join(t.TempDir(), "archive.tar.gz")
			if _, err := BackupWithFormat(homePath, target, ArchiveFormatStreamedV2, now); err == nil {
				t.Fatal("writer published archive outside metadata budget")
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatalf("writer left final archive after refusal: %v", err)
			}
		})
	}
}

func TestStreamedChunkCountBudget(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	homePath := filepath.Join(t.TempDir(), "home")
	if err := Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "ledger", "extra.json"), []byte("extra"), 0o600); err != nil {
		t.Fatal(err)
	}
	prior := defaultArchiveStreamMetadataLimits
	defer func() { defaultArchiveStreamMetadataLimits = prior }()
	defaultArchiveStreamMetadataLimits.ChunkRecords = 1
	archivePath := filepath.Join(t.TempDir(), "archive.tar.gz")
	if _, err := BackupWithFormat(homePath, archivePath, ArchiveFormatStreamedV2, now); err != nil {
		t.Fatal(err)
	}
	defaultArchiveStreamMetadataLimits = prior
	f, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	metadata := prior
	metadata.Chunks = 1
	if report, err := readStreamedArchiveWithMetadataLimits(archivePath, f, nil, nil, nil, archiveStreamTarLimits(), metadata); err == nil || report.OK {
		t.Fatalf("multiple chunks passed one-chunk bound: %+v, %v", report, err)
	}
	defaultArchiveStreamMetadataLimits.ChunkRecords = 1
	defaultArchiveStreamMetadataLimits.Chunks = 1
	target := filepath.Join(t.TempDir(), "refused.tar.gz")
	if _, err := BackupWithFormat(homePath, target, ArchiveFormatStreamedV2, now); err == nil {
		t.Fatal("writer exceeded one-chunk bound")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("writer published over-budget archive: %v", err)
	}
}

func TestStreamedBackupChunkBoundaryBeforeNextFile(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	homePath := filepath.Join(t.TempDir(), "home")
	if err := Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	for i := range archiveStreamChunkRecords + 1 {
		path := filepath.Join(homePath, "ledger", fmt.Sprintf("synthetic-%04d.json", i))
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	archivePath := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := BackupWithFormat(homePath, archivePath, ArchiveFormatStreamedV2, now); err != nil {
		t.Fatal(err)
	}
	if report, err := VerifyArchive(archivePath); err != nil || !report.OK {
		t.Fatalf("verify after record boundary: %+v, %v", report, err)
	}
}

func TestStreamedBackupChunkByteBoundaryBeforeNextFile(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	homePath := filepath.Join(t.TempDir(), "home")
	if err := Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		path := filepath.Join(homePath, "ledger", fmt.Sprintf("synthetic-%04d.json", i))
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prior := defaultArchiveStreamMetadataLimits
	defer func() { defaultArchiveStreamMetadataLimits = prior }()
	defaultArchiveStreamMetadataLimits.ChunkBytes = 200
	archivePath := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := BackupWithFormat(homePath, archivePath, ArchiveFormatStreamedV2, now); err != nil {
		t.Fatal(err)
	}
	if report, err := VerifyArchive(archivePath); err != nil || !report.OK {
		t.Fatalf("verify after byte boundary: %+v, %v", report, err)
	}
}

func TestStreamedRestoreSyncsFlatParentOnlyAtTreeBarrier(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	homePath := filepath.Join(t.TempDir(), "home")
	if err := Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err := os.WriteFile(filepath.Join(homePath, "ledger", fmt.Sprintf("item-%d.json", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	archivePath := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := BackupWithFormat(homePath, archivePath, ArchiveFormatStreamedV2, now); err != nil {
		t.Fatal(err)
	}
	prior := restoreSyncStagingDirectory
	defer func() { restoreSyncStagingDirectory = prior }()
	ledgerSyncs := 0
	restoreSyncStagingDirectory = func(root *os.Root, rel, display, operation string) error {
		if rel == "ledger" {
			ledgerSyncs++
		}
		return prior(root, rel, display, operation)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if _, err := RestoreArchive(archivePath, target, now); err != nil {
		t.Fatal(err)
	}
	if ledgerSyncs != 1 {
		t.Fatalf("flat streamed restore synced ledger parent %d times, want one final tree sync", ledgerSyncs)
	}
}

func TestStreamedRestoreFinalDirectorySyncFailureLeavesTargetAbsent(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	homePath := filepath.Join(t.TempDir(), "home")
	if err := Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "ledger", "item.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := BackupWithFormat(homePath, archivePath, ArchiveFormatStreamedV2, now); err != nil {
		t.Fatal(err)
	}
	prior := restoreSyncStagingDirectory
	defer func() { restoreSyncStagingDirectory = prior }()
	restoreSyncStagingDirectory = func(root *os.Root, rel, display, operation string) error {
		if rel == "ledger" {
			return errors.New("injected final ledger sync failure")
		}
		return prior(root, rel, display, operation)
	}
	parent := t.TempDir()
	target := filepath.Join(parent, "restored")
	_, restoreErr := RestoreArchive(archivePath, target, now)
	if restoreErr == nil || !strings.Contains(restoreErr.Error(), "injected final ledger sync failure") {
		t.Fatalf("restore did not surface final directory sync failure: %v", restoreErr)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("failed final sync published target: %v", err)
	}
	stages, err := filepath.Glob(filepath.Join(parent, restoreStagingPrefix+"*"))
	if err != nil || len(stages) != 1 || !strings.Contains(restoreErr.Error(), "retained empty") {
		t.Fatalf("failed final sync did not retain one empty staging directory: %v, %v; restore error: %v", stages, err, restoreErr)
	}
	entries, err := os.ReadDir(stages[0])
	if err != nil || len(entries) != 0 {
		t.Fatalf("retained staging is not empty: %v, %v", entries, err)
	}
}

func TestLegacyRestoreRetainsPerFileParentSync(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	homePath := filepath.Join(t.TempDir(), "home")
	if err := Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err := os.WriteFile(filepath.Join(homePath, "ledger", fmt.Sprintf("item-%d.json", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	archivePath := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := Backup(homePath, archivePath, now); err != nil {
		t.Fatal(err)
	}
	prior := restoreSyncStagingDirectory
	defer func() { restoreSyncStagingDirectory = prior }()
	ledgerSyncs := 0
	restoreSyncStagingDirectory = func(root *os.Root, rel, display, operation string) error {
		if rel == "ledger" {
			ledgerSyncs++
		}
		return prior(root, rel, display, operation)
	}
	if _, err := RestoreArchive(archivePath, filepath.Join(t.TempDir(), "restored"), now); err != nil {
		t.Fatal(err)
	}
	if ledgerSyncs != 4 {
		t.Fatalf("legacy restore synced ledger parent %d times, want three files plus final tree sync", ledgerSyncs)
	}
}
