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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestoreArchiveRequiresAbsentTarget(t *testing.T) {
	archivePath := backupArchiveFixture(t)
	target := filepath.Join(t.TempDir(), ".compound-restored")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreArchive(archivePath, target, archiveTestNow); err == nil || !strings.Contains(err.Error(), "new absent path") {
		t.Fatalf("restore into existing target err = %v", err)
	}
}

func TestRestoreArchiveUsesOneOpenArchiveHandle(t *testing.T) {
	archivePath := backupArchiveFixture(t)
	target := filepath.Join(t.TempDir(), ".compound-restored")
	moved := archivePath + ".moved"
	oldHook := restoreArchiveOpened
	t.Cleanup(func() { restoreArchiveOpened = oldHook })
	restoreArchiveOpened = func(string) error { return os.Rename(archivePath, moved) }
	if _, err := RestoreArchive(archivePath, target, archiveTestNow); err != nil {
		t.Fatalf("restore should continue through the already-open archive handle: %v", err)
	}
	if !Doctor(target).OK {
		t.Fatalf("restored target doctor failed: %+v", Doctor(target))
	}
}

func TestRestoreArchivePreservesPrivateCustodyDirectoryModes(t *testing.T) {
	archivePath := backupArchiveFixture(t)
	target := filepath.Join(t.TempDir(), ".compound-restored")
	if _, err := RestoreArchive(archivePath, target, archiveTestNow); err != nil {
		t.Fatal(err)
	}
	report := Doctor(target)
	if !report.PrivateCustody.OK {
		t.Fatalf("restored private custody = %+v", report.PrivateCustody)
	}
	for _, rel := range privateCustodyKnownDirs {
		info, err := os.Stat(filepath.Join(target, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("restored %s mode = %04o, want 0700", rel, got)
		}
	}
}

func TestRestoreArchivePreservesRestrictivePublicDirectoryMode(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "local", archiveTestNow); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(homePath, "events"), 0o700); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := Backup(homePath, archivePath, archiveTestNow); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), ".compound-restored")
	if _, err := RestoreArchive(archivePath, target, archiveTestNow); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(target, "events"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("restored events mode = %04o, want 0700", got)
	}
}

func TestRestoreArchiveChecksumFailureLeavesTargetAbsent(t *testing.T) {
	archivePath := writeRestoreTestArchive(t,
		[]restoreTestEntry{{name: ManifestName, body: []byte("schema_version: " + SchemaVersion + "\nhome_kind: test\ncreated_at: 2025-01-01T00:00:00Z\nactive_modules: []\n")}},
		[]ArchiveEntry{{Path: ManifestName, Size: 1, SHA256: strings.Repeat("0", 64)}},
	)
	parent := t.TempDir()
	target := filepath.Join(parent, ".compound-restored")
	_, err := RestoreArchive(archivePath, target, archiveTestNow)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("restore checksum failure err = %v", err)
	}
	assertMissing(t, target)
	assertNoRestoreStaging(t, parent)
}

func TestRestoreArchiveRejectsUnsafeAndDuplicateEntries(t *testing.T) {
	cases := []struct {
		name  string
		rows  []restoreRawTarEntry
		match string
	}{
		{name: "traversal", rows: []restoreRawTarEntry{{hdr: &tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o600}, body: []byte("x")}}, match: "invalid archive path"},
		{name: "symlink", rows: []restoreRawTarEntry{{hdr: &tar.Header{Name: ManifestName, Typeflag: tar.TypeSymlink, Linkname: "outside", Mode: 0o777}}}, match: "unsupported entry"},
		{name: "hardlink", rows: []restoreRawTarEntry{{hdr: &tar.Header{Name: ManifestName, Typeflag: tar.TypeLink, Linkname: "outside", Mode: 0o777}}}, match: "unsupported entry"},
		{name: "noncanonical_directory", rows: []restoreRawTarEntry{{hdr: &tar.Header{Name: "events//", Typeflag: tar.TypeDir, Mode: 0o755}}}, match: "invalid archive path"},
		{name: "newline", rows: []restoreRawTarEntry{{hdr: &tar.Header{Name: "ledger/deceptive\nname.json", Typeflag: tar.TypeReg, Mode: 0o600}, body: []byte("x")}}, match: "invalid archive path"},
		{name: "escape", rows: []restoreRawTarEntry{{hdr: &tar.Header{Name: "ledger/deceptive\x1bname.json", Typeflag: tar.TypeReg, Mode: 0o600}, body: []byte("x")}}, match: "invalid archive path"},
		{name: "duplicate", rows: []restoreRawTarEntry{
			{hdr: &tar.Header{Name: ManifestName, Typeflag: tar.TypeReg, Mode: 0o600}, body: []byte("one")},
			{hdr: &tar.Header{Name: ManifestName, Typeflag: tar.TypeReg, Mode: 0o600}, body: []byte("two")},
		}, match: "duplicate entry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			archivePath := writeRestoreRawTarGz(t, tc.rows)
			parent := t.TempDir()
			target := filepath.Join(parent, ".compound-restored")
			_, err := RestoreArchive(archivePath, target, archiveTestNow)
			if err == nil || !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("restore err = %v, want containing %q", err, tc.match)
			}
			assertMissing(t, target)
			assertNoRestoreStaging(t, parent)
		})
	}
}

func TestRestoreArchiveRequiresDirectoryBeforeChild(t *testing.T) {
	body := []byte("claim")
	sum := sha256.Sum256(body)
	archivePath := writeRestoreTestArchive(t,
		[]restoreTestEntry{{name: "ledger/claim.json", body: body}},
		[]ArchiveEntry{{Path: "ledger/claim.json", Size: int64(len(body)), SHA256: fmt.Sprintf("%x", sum[:])}},
	)
	parent := t.TempDir()
	target := filepath.Join(parent, ".compound-restored")
	_, err := RestoreArchive(archivePath, target, archiveTestNow)
	if err == nil || !strings.Contains(err.Error(), "parent must precede child") {
		t.Fatalf("restore missing directory entry err = %v", err)
	}
	assertMissing(t, target)
	assertNoRestoreStaging(t, parent)
}

func TestRestoreArchiveRejectsDuplicateManifestEntries(t *testing.T) {
	body := []byte("manifest")
	sum := sha256.Sum256(body)
	entry := ArchiveEntry{Path: ManifestName, Size: int64(len(body)), SHA256: fmt.Sprintf("%x", sum[:])}
	archivePath := writeRestoreTestArchive(t,
		[]restoreTestEntry{{name: ManifestName, body: body}},
		[]ArchiveEntry{entry, entry},
	)
	parent := t.TempDir()
	target := filepath.Join(parent, ".compound-restored")
	_, err := RestoreArchive(archivePath, target, archiveTestNow)
	if err == nil || !strings.Contains(err.Error(), "manifest contains duplicate entry") {
		t.Fatalf("restore duplicate manifest entry err = %v", err)
	}
	assertMissing(t, target)
	assertNoRestoreStaging(t, parent)
}

func TestRestoreArchiveResourceLimitsFailClosedBeforeStaging(t *testing.T) {
	four := []byte("four")
	sum := sha256.Sum256(four)
	entry := func(path string) ArchiveEntry {
		return ArchiveEntry{Path: path, Size: int64(len(four)), SHA256: fmt.Sprintf("%x", sum[:])}
	}
	cases := []struct {
		name    string
		entries []restoreTestEntry
		rows    []ArchiveEntry
		limits  func(archiveReadLimits) archiveReadLimits
		match   string
	}{
		{
			name:    "entry_bytes",
			entries: []restoreTestEntry{{name: "ledger/", directory: true}, {name: "ledger/one.json", body: four}},
			rows:    []ArchiveEntry{entry("ledger/one.json")},
			limits: func(l archiveReadLimits) archiveReadLimits {
				l.MaxEntryBytes = 3
				return l
			},
			match: "entry ledger/one.json exceeds 3 bytes",
		},
		{
			name: "expanded_bytes",
			entries: []restoreTestEntry{
				{name: "ledger/", directory: true},
				{name: "ledger/one.json", body: four},
				{name: "ledger/two.json", body: four},
			},
			rows: []ArchiveEntry{entry("ledger/one.json"), entry("ledger/two.json")},
			limits: func(l archiveReadLimits) archiveReadLimits {
				l.MaxEntryBytes = 4
				l.MaxExpandedBytes = 7
				return l
			},
			match: "expanded bytes exceed 7",
		},
		{
			name:    "entry_count",
			entries: []restoreTestEntry{{name: "ledger/", directory: true}, {name: "ledger/one.json", body: four}},
			rows:    []ArchiveEntry{entry("ledger/one.json")},
			limits: func(l archiveReadLimits) archiveReadLimits {
				l.MaxEntries = 1
				return l
			},
			match: "entry count exceeds 1",
		},
		{
			name:    "path_depth",
			entries: []restoreTestEntry{{name: "ledger/", directory: true}, {name: "ledger/deep/", directory: true}, {name: "ledger/deep/one.json", body: four}},
			rows:    []ArchiveEntry{entry("ledger/deep/one.json")},
			limits: func(l archiveReadLimits) archiveReadLimits {
				l.MaxPathDepth = 2
				return l
			},
			match: "path depth exceeds 2",
		},
		{
			name:    "path_bytes",
			entries: []restoreTestEntry{{name: "ledger/", directory: true}},
			limits: func(l archiveReadLimits) archiveReadLimits {
				l.MaxPathBytes = 6
				return l
			},
			match: "path exceeds 6 bytes",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			archivePath := writeRestoreTestArchive(t, tc.entries, tc.rows)
			parent := t.TempDir()
			target := filepath.Join(parent, ".compound-restored")
			_, err := restoreArchiveWithLimits(archivePath, target, archiveTestNow, tc.limits(defaultArchiveReadLimits))
			if err == nil || !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("restore resource limit err = %v, want containing %q", err, tc.match)
			}
			assertMissing(t, target)
			assertNoRestoreStaging(t, parent)
		})
	}
}

func TestRestoreArchiveRejectsOversizedSourceBeforeStaging(t *testing.T) {
	archivePath := backupArchiveFixture(t)
	parent := t.TempDir()
	target := filepath.Join(parent, ".compound-restored")
	limits := defaultArchiveReadLimits
	limits.MaxSourceBytes = 1
	_, err := restoreArchiveWithLimits(archivePath, target, archiveTestNow, limits)
	if err == nil || !strings.Contains(err.Error(), "archive source exceeds 1 bytes") {
		t.Fatalf("restore source limit err = %v", err)
	}
	assertMissing(t, target)
	assertNoRestoreStaging(t, parent)
}

func TestRestoreArchiveRejectsSourceGrowthBeyondActualReadLimit(t *testing.T) {
	archivePath := backupArchiveFixture(t)
	info, err := os.Stat(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	oldHook := restoreArchiveOpened
	t.Cleanup(func() { restoreArchiveOpened = oldHook })
	restoreArchiveOpened = func(string) error {
		file, err := os.OpenFile(archivePath, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(bytes.Repeat([]byte("x"), 1<<20))
		return errors.Join(writeErr, file.Close())
	}
	parent := t.TempDir()
	target := filepath.Join(parent, ".compound-restored")
	limits := defaultArchiveReadLimits
	limits.MaxSourceBytes = info.Size()
	_, err = restoreArchiveWithLimits(archivePath, target, archiveTestNow, limits)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("archive source exceeds %d bytes while reading", info.Size())) {
		t.Fatalf("restore growing source err = %v", err)
	}
	assertMissing(t, target)
	assertNoRestoreStaging(t, parent)
}

func TestRestoreArchiveRejectsOpenSourceMutationBetweenPreflightAndExtraction(t *testing.T) {
	archivePath := backupArchiveFixture(t)
	oldHook := restoreArchivePreflighted
	t.Cleanup(func() { restoreArchivePreflighted = oldHook })
	restoreArchivePreflighted = func(string) error {
		file, err := os.OpenFile(archivePath, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer file.Close()
		var header [10]byte
		if _, err := file.ReadAt(header[:], 0); err != nil {
			return err
		}
		if header[3]&0x02 != 0 {
			return fmt.Errorf("fixture unexpectedly enables gzip FHCRC")
		}
		header[9] ^= 0xff
		if _, err := file.WriteAt(header[9:10], 9); err != nil {
			return err
		}
		return file.Sync()
	}
	parent := t.TempDir()
	target := filepath.Join(parent, ".compound-restored")
	_, err := RestoreArchive(archivePath, target, archiveTestNow)
	wantErr := fmt.Sprintf("archive source changed between preflight and restore extraction: %s", archivePath)
	if err == nil {
		t.Fatal("restore accepted archive source mutation")
	}
	gotErr := err
	if joined, ok := err.(interface{ Unwrap() []error }); ok && len(joined.Unwrap()) > 0 {
		gotErr = joined.Unwrap()[0]
	}
	if gotErr.Error() != wantErr {
		t.Fatalf("restore source mutation err = %v", err)
	}
	assertMissing(t, target)
	assertRetainedEmptyRestoreStaging(t, parent, err)
}

func TestRestoreArchiveRejectsDecodedTarMetadataExpansion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format tar.Format
		pax    map[string]string
	}{
		{name: "pax", format: tar.FormatPAX, pax: map[string]string{"comment": strings.Repeat("z", 64<<10)}},
		{name: "gnu_longname", format: tar.FormatGNU},
	} {
		t.Run(tc.name, func(t *testing.T) {
			longName := strings.Repeat("x", 32<<10)
			archivePath := writeRestoreRawTarGz(t, []restoreRawTarEntry{
				{hdr: &tar.Header{Name: longName, Typeflag: tar.TypeReg, Mode: 0o600, Format: tc.format, PAXRecords: tc.pax}, body: []byte("x")},
			})
			parent := t.TempDir()
			target := filepath.Join(parent, ".compound-restored")
			limits := defaultArchiveReadLimits
			limits.MaxPathBytes = len(longName) + 1
			limits.MaxEntryBytes = 1
			limits.MaxExpandedBytes = 1
			limits.MaxDecodedBytes = 1024
			_, err := restoreArchiveWithLimits(archivePath, target, archiveTestNow, limits)
			if err == nil || !strings.Contains(err.Error(), "decoded archive exceeds 1024 bytes while reading") {
				t.Fatalf("restore decoded metadata limit err = %v", err)
			}
			assertMissing(t, target)
			assertNoRestoreStaging(t, parent)
		})
	}
}

func TestRestoreArchiveRejectsPublicFixtureKindBeforePublication(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "test", archiveTestNow); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "fixture.tar.gz")
	if _, err := ExportPublicFixture(homePath, archivePath, archiveTestNow); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	target := filepath.Join(parent, ".compound-restored")
	_, err := RestoreArchive(archivePath, target, archiveTestNow)
	if err == nil || !strings.Contains(err.Error(), "restore requires archive kind backup") {
		t.Fatalf("restore public fixture kind err = %v", err)
	}
	assertMissing(t, target)
	assertNoRestoreStaging(t, parent)
}

func TestVerifyArchiveManifestAppliesLimitsBeforeExpectedMapConstruction(t *testing.T) {
	digest := strings.Repeat("0", sha256.Size*2)
	base := ArchiveManifest{SchemaVersion: ArchiveSchemaVersion, Kind: ArchiveKindBackup, HomeSchemaVersion: SchemaVersion}
	for _, tc := range []struct {
		name    string
		entries []ArchiveEntry
		limits  func(archiveReadLimits) archiveReadLimits
		match   string
	}{
		{name: "entry_count", entries: make([]ArchiveEntry, 2), limits: func(l archiveReadLimits) archiveReadLimits { l.MaxEntries = 1; return l }, match: "manifest entry count exceeds 1"},
		{name: "path_bytes", entries: []ArchiveEntry{{Path: "ledger/one", SHA256: digest}}, limits: func(l archiveReadLimits) archiveReadLimits { l.MaxPathBytes = 5; return l }, match: "manifest path exceeds 5 bytes"},
		{name: "path_depth", entries: []ArchiveEntry{{Path: "ledger/deep/one", SHA256: digest}}, limits: func(l archiveReadLimits) archiveReadLimits { l.MaxPathDepth = 2; return l }, match: "manifest path depth exceeds 2"},
		{name: "entry_bytes", entries: []ArchiveEntry{{Path: "ledger/one", Size: 2, SHA256: digest}}, limits: func(l archiveReadLimits) archiveReadLimits { l.MaxEntryBytes = 1; return l }, match: "invalid metadata"},
		{name: "expanded_bytes", entries: []ArchiveEntry{{Path: "ledger/one", Size: 1, SHA256: digest}, {Path: "ledger/two", Size: 1, SHA256: digest}}, limits: func(l archiveReadLimits) archiveReadLimits {
			l.MaxEntryBytes = 1
			l.MaxExpandedBytes = 1
			l.MaxDecodedBytes = 1
			return l
		}, match: "manifest expanded bytes exceed 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := base
			manifest.Entries = tc.entries
			if _, err := verifyArchiveManifestWithLimits("fixture", manifest, nil, tc.limits(defaultArchiveReadLimits)); err == nil || !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("manifest resource limit err = %v, want %q", err, tc.match)
			}
		})
	}
}

func TestVerifyArchiveRejectsTrailingCompressedMember(t *testing.T) {
	archivePath := backupArchiveFixture(t)
	file, err := os.OpenFile(archivePath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	if _, err := gz.Write([]byte("trailing member")); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyArchive(archivePath); err == nil || !strings.Contains(err.Error(), "trailing compressed data") {
		t.Fatalf("verify trailing gzip member err = %v", err)
	}
}

func TestRejectTrailingDecodedArchiveDataReadsAtMostOneByte(t *testing.T) {
	reader := &countingReader{reader: bytes.NewReader(bytes.Repeat([]byte("x"), 1<<20))}
	if err := rejectTrailingDecodedArchiveData(reader); err == nil || !strings.Contains(err.Error(), "trailing decoded data") {
		t.Fatalf("trailing decoded data err = %v", err)
	}
	if reader.bytes != 1 {
		t.Fatalf("trailing decoded bytes consumed = %d, want 1", reader.bytes)
	}
}

func TestVerifyArchiveRejectsTruncatedGzipFooter(t *testing.T) {
	archivePath := backupArchiveFixture(t)
	raw, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 8 {
		t.Fatalf("archive too small: %d", len(raw))
	}
	if err := os.WriteFile(archivePath, raw[:len(raw)-4], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyArchive(archivePath); err == nil || !strings.Contains(err.Error(), "gzip archive footer") {
		t.Fatalf("verify truncated gzip footer err = %v", err)
	}
}

func TestRestoreArchivePublishRaceDoesNotReplaceTarget(t *testing.T) {
	archivePath := backupArchiveFixture(t)
	parent := t.TempDir()
	target := filepath.Join(parent, ".compound-restored")
	sentinel := []byte("foreign")
	oldHook := restoreBeforePublish
	t.Cleanup(func() { restoreBeforePublish = oldHook })
	restoreBeforePublish = func(_, target string) error {
		if err := os.Mkdir(target, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(target, "sentinel"), sentinel, 0o600)
	}
	_, restoreErr := RestoreArchive(archivePath, target, archiveTestNow)
	if restoreErr == nil {
		t.Fatal("restore replaced a target created before publication")
	}
	got, err := os.ReadFile(filepath.Join(target, "sentinel"))
	if err != nil || !bytes.Equal(got, sentinel) {
		t.Fatalf("foreign target changed: got=%q err=%v", got, err)
	}
	assertRetainedEmptyRestoreStaging(t, parent, restoreErr)
}

func TestRestoreArchivePostPublishSyncFailureIsAmbiguousAndKeepsTarget(t *testing.T) {
	archivePath := backupArchiveFixture(t)
	parent := t.TempDir()
	target := filepath.Join(parent, ".compound-restored")
	oldSync := restoreSyncParent
	t.Cleanup(func() { restoreSyncParent = oldSync })
	calls := 0
	restoreSyncParent = func(root *os.File) error {
		calls++
		if calls >= 2 {
			return errors.New("parent sync failed")
		}
		return syncRestoreDirectory(root)
	}
	if _, err := RestoreArchive(archivePath, target, archiveTestNow); err == nil || !strings.Contains(err.Error(), "published but parent sync failed") {
		t.Fatalf("restore post-publish sync err = %v", err)
	}
	if !Doctor(target).OK {
		t.Fatalf("ambiguous published target should remain inspectable: %+v", Doctor(target))
	}
}

func TestRestoreArchivePublishSuccessThenErrorPreservesPublishedTarget(t *testing.T) {
	archivePath := backupArchiveFixture(t)
	parent := t.TempDir()
	target := filepath.Join(parent, ".compound-restored")
	oldRename := restoreNoReplace
	t.Cleanup(func() { restoreNoReplace = oldRename })
	restoreNoReplace = func(parentFD *os.File, from, to string) error {
		if err := oldRename(parentFD, from, to); err != nil {
			return err
		}
		return errors.New("injected ambiguous rename result")
	}
	_, err := RestoreArchive(archivePath, target, archiveTestNow)
	if err == nil || !strings.Contains(err.Error(), "target acquired the staged identity") || !strings.Contains(err.Error(), "target preserved") || !strings.Contains(err.Error(), "injected ambiguous rename result") {
		t.Fatalf("restore ambiguous publish err = %v", err)
	}
	if report := Doctor(target); !report.OK || !report.PrivateCustody.OK {
		t.Fatalf("ambiguously published target was not preserved intact: %+v", report)
	}
	assertNoRestoreStaging(t, parent)
}

func TestRestoreArchiveRejectsStagedMutationBeforePublish(t *testing.T) {
	archivePath := backupArchiveFixture(t)
	parent := t.TempDir()
	target := filepath.Join(parent, ".compound-restored")
	oldHook := restoreBeforePublish
	t.Cleanup(func() { restoreBeforePublish = oldHook })
	restoreBeforePublish = func(staging, _ string) error {
		return os.WriteFile(filepath.Join(staging, "ledger", "claims.json"), []byte(`{"claim":"tampered"}`), 0o600)
	}
	_, err := RestoreArchive(archivePath, target, archiveTestNow)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("restore staged mutation err = %v", err)
	}
	assertMissing(t, target)
	assertRetainedEmptyRestoreStaging(t, parent, err)
}

func TestRestoreArchiveRejectsPrivateCustodyDriftBeforePublish(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "local", archiveTestNow); err != nil {
		t.Fatal(err)
	}
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutPrivate([]byte("sensitive fixture")); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := Backup(homePath, archivePath, archiveTestNow); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	target := filepath.Join(parent, ".compound-restored")
	oldHook := restoreBeforePublish
	t.Cleanup(func() { restoreBeforePublish = oldHook })
	restoreBeforePublish = func(staging, _ string) error {
		digestRoot := filepath.Join(staging, "private", "artifacts", "sha256")
		entries, err := os.ReadDir(digestRoot)
		if err != nil {
			return err
		}
		if len(entries) != 1 {
			return fmt.Errorf("private digest entries = %d, want 1", len(entries))
		}
		return os.Chmod(filepath.Join(digestRoot, entries[0].Name()), 0o644)
	}
	_, err = RestoreArchive(archivePath, target, archiveTestNow)
	if err == nil || !strings.Contains(err.Error(), "private custody") {
		t.Fatalf("restore private drift err = %v", err)
	}
	assertMissing(t, target)
	assertRetainedEmptyRestoreStaging(t, parent, err)
}

func TestRestoreArchiveCleanupDoesNotDeleteReplacementStaging(t *testing.T) {
	archivePath := backupArchiveFixture(t)
	parent := t.TempDir()
	target := filepath.Join(parent, ".compound-restored")
	oldHook := restoreBeforePublish
	t.Cleanup(func() { restoreBeforePublish = oldHook })
	var replacement string
	restoreBeforePublish = func(staging, _ string) error {
		if err := os.Rename(staging, staging+".moved"); err != nil {
			return err
		}
		if err := os.Mkdir(staging, 0o700); err != nil {
			return err
		}
		replacement = filepath.Join(staging, "sentinel")
		if err := os.WriteFile(replacement, []byte("foreign"), 0o600); err != nil {
			return err
		}
		return errors.New("stop after staging replacement")
	}
	_, err := RestoreArchive(archivePath, target, archiveTestNow)
	if err == nil || !strings.Contains(err.Error(), "staging replacement") || !strings.Contains(err.Error(), "replacement was left untouched") {
		t.Fatalf("restore replacement cleanup err = %v", err)
	}
	if got, err := os.ReadFile(replacement); err != nil || string(got) != "foreign" {
		t.Fatalf("replacement staging was changed: got=%q err=%v", got, err)
	}
	assertMissing(t, target)
}

func TestRestoreArchiveRetainsFailedStagingEmptyAtExactPrivatePath(t *testing.T) {
	archivePath := backupArchiveFixture(t)
	parent := t.TempDir()
	target := filepath.Join(parent, ".compound-restored")
	oldHook := restoreBeforePublish
	t.Cleanup(func() { restoreBeforePublish = oldHook })
	restoreBeforePublish = func(string, string) error { return errors.New("stop before publication") }
	_, err := RestoreArchive(archivePath, target, archiveTestNow)
	if err == nil || !strings.Contains(err.Error(), "stop before publication") {
		t.Fatalf("retained failed staging err = %v", err)
	}
	assertRetainedEmptyRestoreStaging(t, parent, err)
	assertMissing(t, target)
}

func TestRestoreArchiveReportsPinnedClearFailureAtExactPath(t *testing.T) {
	archivePath := backupArchiveFixture(t)
	parent := t.TempDir()
	target := filepath.Join(parent, ".compound-restored")
	oldHook := restoreBeforePublish
	t.Cleanup(func() { restoreBeforePublish = oldHook })
	restoreBeforePublish = func(string, string) error { return errors.New("stop before publication") }
	oldRemove := restoreCleanupRemove
	t.Cleanup(func() { restoreCleanupRemove = oldRemove })
	restoreCleanupRemove = func(*os.Root, string) error {
		return errors.New("pinned clear failure injected")
	}
	_, err := RestoreArchive(archivePath, target, archiveTestNow)
	paths := restoreStagingPaths(t, parent)
	if err == nil || !strings.Contains(err.Error(), "stop before publication") || !strings.Contains(err.Error(), "pinned clear failure injected") || len(paths) != 1 || !strings.Contains(err.Error(), paths[0]) || !strings.Contains(err.Error(), "private data may remain") {
		t.Fatalf("pinned clear failure paths=%v err=%v", paths, err)
	}
	entries, readErr := os.ReadDir(paths[0])
	if readErr != nil || len(entries) == 0 {
		t.Fatalf("failed pinned clear should retain inspectable private data: entries=%v err=%v", entries, readErr)
	}
	assertMissing(t, target)
}

func TestRestoreArchiveReportsPreDeferStagingFailures(t *testing.T) {
	cases := []struct {
		name    string
		install func(parent string)
		restore func()
		match   string
		check   func(t *testing.T, parent string, err error)
	}{
		{
			name: "initial_parent_sync",
			install: func(string) {
				restoreSyncParent = func(*os.File) error { return errors.New("initial staging sync injected") }
			},
			restore: func() { restoreSyncParent = syncRestoreDirectory },
			match:   "initial staging sync injected",
			check:   assertRetainedEmptyRestoreStaging,
		},
		{
			name: "open_root",
			install: func(string) {
				restoreOpenStaging = func(*os.Root, string) (*os.Root, error) { return nil, errors.New("open staging injected") }
			},
			restore: func() {
				restoreOpenStaging = func(parent *os.Root, name string) (*os.Root, error) { return parent.OpenRoot(name) }
			},
			match: "open staging injected",
			check: func(t *testing.T, parent string, err error) {
				paths := restoreStagingPaths(t, parent)
				if len(paths) != 1 || !strings.Contains(err.Error(), paths[0]) || !strings.Contains(err.Error(), "not safely pinned") {
					t.Fatalf("unopened staging path not reported: paths=%v err=%v", paths, err)
				}
				assertPrivateEmptyDirectory(t, paths[0])
			},
		},
		{
			name: "pinned_cleanup_sync_failure",
			install: func(string) {
				restoreSyncParent = func(*os.File) error { return errors.New("initial staging sync injected") }
				restoreCleanupSync = func(*os.Root, string) error { return errors.New("pinned cleanup sync injected") }
			},
			restore: func() {
				restoreSyncParent = syncRestoreDirectory
				restoreCleanupSync = func(root *os.Root, display string) error {
					return syncRootedDirectory(root, ".", display, "archive restore cleanup")
				}
			},
			match: "pinned cleanup sync injected",
			check: func(t *testing.T, parent string, err error) {
				paths := restoreStagingPaths(t, parent)
				if len(paths) != 1 || !strings.Contains(err.Error(), paths[0]) || !strings.Contains(err.Error(), "private data may remain") {
					t.Fatalf("cleanup sync failure path not reported: paths=%v err=%v", paths, err)
				}
				assertPrivateEmptyDirectory(t, paths[0])
			},
		},
		{
			name: "validation_replacement",
			install: func(string) {
				restoreBeforeStagingValidation = func(staging string) error {
					if err := os.Rename(staging, staging+".moved"); err != nil {
						return err
					}
					if err := os.Mkdir(staging, 0o700); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(staging, "sentinel"), []byte("foreign"), 0o600)
				}
			},
			restore: func() { restoreBeforeStagingValidation = func(string) error { return nil } },
			match:   "restore staging directory must remain stable and private",
			check: func(t *testing.T, parent string, err error) {
				paths := restoreStagingPaths(t, parent)
				var replacement string
				for _, path := range paths {
					if !strings.HasSuffix(path, ".moved") {
						replacement = path
					}
				}
				if replacement == "" {
					t.Fatalf("replacement staging not found: %v", paths)
				}
				if got, err := os.ReadFile(filepath.Join(replacement, "sentinel")); err != nil || string(got) != "foreign" {
					t.Fatalf("replacement staging changed: got=%q err=%v", got, err)
				}
				if !strings.Contains(err.Error(), replacement) || !strings.Contains(err.Error(), "replacement was left untouched") {
					t.Fatalf("replacement identity drift not reported: replacement=%s err=%v", replacement, err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			archivePath := backupArchiveFixture(t)
			parent := t.TempDir()
			target := filepath.Join(parent, ".compound-restored")
			tc.install(parent)
			t.Cleanup(tc.restore)
			_, err := RestoreArchive(archivePath, target, archiveTestNow)
			if err == nil || !strings.Contains(err.Error(), filepath.Join(parent, restoreStagingPrefix)) || !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("pre-defer staging failure did not name exact staging prefix: %v", err)
			}
			assertMissing(t, target)
			if tc.check != nil {
				tc.check(t, parent, err)
			}
		})
	}
}

func TestRestoreArchiveDetectsPublishedTargetReplacement(t *testing.T) {
	archivePath := backupArchiveFixture(t)
	parent := t.TempDir()
	target := filepath.Join(parent, ".compound-restored")
	oldRename := restoreNoReplace
	t.Cleanup(func() { restoreNoReplace = oldRename })
	restoreNoReplace = func(parentFD *os.File, from, to string) error {
		if err := oldRename(parentFD, from, to); err != nil {
			return err
		}
		if err := os.Rename(target, target+".moved"); err != nil {
			return err
		}
		if err := os.Mkdir(target, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(target, "sentinel"), []byte("foreign"), 0o600)
	}
	if _, err := RestoreArchive(archivePath, target, archiveTestNow); err == nil || !strings.Contains(err.Error(), "target identity changed") {
		t.Fatalf("restore published replacement err = %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "sentinel")); err != nil || string(got) != "foreign" {
		t.Fatalf("replacement target changed: got=%q err=%v", got, err)
	}
}

func TestRestoreArchiveReportsCleanupFailuresAndPrivateStagingPath(t *testing.T) {
	archivePath := backupArchiveFixture(t)
	parent := t.TempDir()
	target := filepath.Join(parent, ".compound-restored")
	oldHook := restoreBeforePublish
	t.Cleanup(func() { restoreBeforePublish = oldHook })
	restoreBeforePublish = func(string, string) error { return errors.New("stop before publication") }
	oldRemove := restoreCleanupRemove
	t.Cleanup(func() { restoreCleanupRemove = oldRemove })
	restoreCleanupRemove = func(*os.Root, string) error { return errors.New("cleanup remove injected") }
	_, err := RestoreArchive(archivePath, target, archiveTestNow)
	if err == nil || !strings.Contains(err.Error(), "stop before publication") || !strings.Contains(err.Error(), "restore cleanup incomplete for private staging") || !strings.Contains(err.Error(), "cleanup remove injected") || !strings.Contains(err.Error(), "private data may remain") {
		t.Fatalf("restore cleanup err = %v", err)
	}
	paths := restoreStagingPaths(t, parent)
	if len(paths) != 1 || !strings.Contains(err.Error(), paths[0]) {
		t.Fatalf("retained cleanup path mismatch: paths=%v err=%v", paths, err)
	}
}

func TestArchiveRestoreFileCommitDoesNotDeleteReplacement(t *testing.T) {
	parent := t.TempDir()
	root, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	target := &archiveRestoreTarget{root: root, displayPath: parent}
	file, err := target.createFile("entry", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.file.Write([]byte("opened")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "entry")
	if err := os.Rename(path, path+".opened"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := file.commit(); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("replacement commit err = %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "foreign" {
		t.Fatalf("commit deleted replacement: got=%q err=%v", got, err)
	}
}

func TestArchiveRestoreFileAbortDoesNotDeleteReplacement(t *testing.T) {
	parent := t.TempDir()
	root, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	target := &archiveRestoreTarget{root: root, displayPath: parent}
	file, err := target.createFile("entry", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "entry")
	if err := os.Rename(path, path+".opened"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("abort injected")
	if err := file.abort(cause); !errors.Is(err, cause) {
		t.Fatalf("abort err = %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "foreign" {
		t.Fatalf("abort deleted replacement: got=%q err=%v", got, err)
	}
}

func TestRestoreArchiveAtomicRoundTrip(t *testing.T) {
	archivePath := backupArchiveFixture(t)
	target := filepath.Join(t.TempDir(), ".compound-restored")
	report, err := RestoreArchive(archivePath, target, archiveTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if report.TargetHome != target || report.Kind != ArchiveKindBackup || report.Files == 0 {
		t.Fatalf("restore report = %+v", report)
	}
	if got := mustReadHomeFile(t, target, "ledger/claims.json"); got != `{"claim":"durable"}` {
		t.Fatalf("restored ledger = %q", got)
	}
}

func backupArchiveFixture(t *testing.T) string {
	t.Helper()
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "local", archiveTestNow); err != nil {
		t.Fatal(err)
	}
	writeHomeFile(t, homePath, "ledger/claims.json", `{"claim":"durable"}`)
	archivePath := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := Backup(homePath, archivePath, archiveTestNow); err != nil {
		t.Fatal(err)
	}
	return archivePath
}

func assertRetainedEmptyRestoreStaging(t *testing.T, parent string, restoreErr error) {
	t.Helper()
	paths := restoreStagingPaths(t, parent)
	if len(paths) != 1 || restoreErr == nil || !strings.Contains(restoreErr.Error(), paths[0]) || !strings.Contains(restoreErr.Error(), "retained empty") || !strings.Contains(restoreErr.Error(), "explicit cleanup") {
		t.Fatalf("retained empty staging contract failed: paths=%v err=%v", paths, restoreErr)
	}
	assertPrivateEmptyDirectory(t, paths[0])
}

func assertNoRestoreStaging(t *testing.T, parent string) {
	t.Helper()
	if paths := restoreStagingPaths(t, parent); len(paths) != 0 {
		t.Fatalf("restore staging unexpectedly allocated: %v", paths)
	}
}

func assertPrivateEmptyDirectory(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 || !ownedByCurrent(info) {
		t.Fatalf("retained staging is not a current-user-owned private directory: path=%s mode=%v", path, info.Mode())
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("retained staging is not empty: path=%s entries=%v", path, entries)
	}
}

func restoreStagingPaths(t *testing.T, parent string) []string {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), restoreStagingPrefix) {
			paths = append(paths, filepath.Join(parent, entry.Name()))
		}
	}
	return paths
}

type restoreTestEntry struct {
	name      string
	body      []byte
	directory bool
}

func writeRestoreTestArchive(t *testing.T, entries []restoreTestEntry, manifestEntries []ArchiveEntry) string {
	t.Helper()
	manifest := ArchiveManifest{SchemaVersion: ArchiveSchemaVersion, Kind: ArchiveKindBackup, CreatedAt: archiveTestNow.UTC().Format("2006-01-02T15:04:05Z"), HomeSchemaVersion: SchemaVersion, Entries: manifestEntries}
	manifestRaw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]restoreRawTarEntry, 0, len(entries)+1)
	for _, entry := range entries {
		typeflag := byte(tar.TypeReg)
		mode := int64(0o600)
		if entry.directory {
			typeflag = tar.TypeDir
			mode = 0o700
		}
		raw = append(raw, restoreRawTarEntry{hdr: &tar.Header{Name: entry.name, Typeflag: typeflag, Mode: mode}, body: entry.body})
	}
	raw = append(raw, restoreRawTarEntry{hdr: &tar.Header{Name: ArchiveManifestName, Typeflag: tar.TypeReg, Mode: 0o600}, body: manifestRaw})
	return writeRestoreRawTarGz(t, raw)
}

type restoreRawTarEntry struct {
	hdr  *tar.Header
	body []byte
}

type countingReader struct {
	reader io.Reader
	bytes  int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytes += n
	return n, err
}

func writeRestoreRawTarGz(t *testing.T, entries []restoreRawTarEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		hdr := *entry.hdr
		if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA {
			hdr.Size = int64(len(entry.body))
		}
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(tw, bytes.NewReader(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
