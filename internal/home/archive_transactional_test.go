package home

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBackupRejectsExistingTarget(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "test", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "home.tar.gz")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Backup(homePath, target, time.Unix(1, 0))
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected existing target error, got %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "keep" {
		t.Fatalf("existing target was modified: %q", got)
	}
}

func TestBackupRejectsIncludedHomeTarget(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "test", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(homePath, "ledger", "backup.tar.gz")
	_, err := Backup(homePath, target, time.Unix(1, 0))
	if err == nil || !strings.Contains(err.Error(), "included home root") {
		t.Fatalf("expected included target rejection, got %v", err)
	}
	if _, statErr := os.Lstat(target); !os.IsNotExist(statErr) {
		t.Fatalf("target should not have been created, stat=%v", statErr)
	}
}

func TestBackupRejectsSymlinkedExports(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "test", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(homePath, "exports")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(homePath, "exports")); err != nil {
		t.Fatal(err)
	}
	_, err := Backup(homePath, "", time.Unix(1, 0))
	if err == nil || !strings.Contains(err.Error(), "exports directory must not be a symlink") {
		t.Fatalf("expected symlinked exports rejection, got %v", err)
	}
}

func TestBackupRejectsSymlinkedExportsDescendant(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	outside := t.TempDir()
	if err := Init(homePath, "test", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(homePath, "exports", "link")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(homePath, "exports", "link", "nested", "home.tar.gz")
	_, err := Backup(homePath, target, time.Unix(1, 0))
	if err == nil || !strings.Contains(err.Error(), "must not contain symlink") {
		t.Fatalf("expected symlinked exports descendant rejection, got %v", err)
	}
	if entries, readErr := os.ReadDir(outside); readErr != nil {
		t.Fatal(readErr)
	} else if len(entries) != 0 {
		t.Fatalf("outside symlink target should stay empty, got %v", entries)
	}
}

func TestBackupRejectsExportParentSwapRace(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	outside := t.TempDir()
	if err := Init(homePath, "test", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	raceDir := filepath.Join(homePath, "exports", "race")
	if err := os.MkdirAll(raceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := archiveBeforeOpenExportParent
	archiveBeforeOpenExportParent = func(rel string) {
		if rel != filepath.Join("exports", "race") {
			return
		}
		if err := os.Remove(raceDir); err != nil {
			t.Fatalf("remove race dir: %v", err)
		}
		if err := os.Symlink(outside, raceDir); err != nil {
			t.Fatalf("swap race dir: %v", err)
		}
	}
	defer func() { archiveBeforeOpenExportParent = old }()
	target := filepath.Join(raceDir, "home.tar.gz")
	_, err := Backup(homePath, target, time.Unix(1, 0))
	if err == nil || (!strings.Contains(err.Error(), "must not contain symlink") && !strings.Contains(err.Error(), "path escapes from parent")) {
		t.Fatalf("expected component swap rejection, got %v", err)
	}
	if entries, readErr := os.ReadDir(outside); readErr != nil {
		t.Fatal(readErr)
	} else if len(entries) != 0 {
		t.Fatalf("outside symlink target should stay empty, got %v", entries)
	}
}

func TestBackupSyncFailureLeavesNoPartialFinal(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "test", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "home.tar.gz")
	old := archiveFileSync
	archiveFileSync = func(*os.File) error { return errors.New("sync boom") }
	defer func() { archiveFileSync = old }()
	_, err := Backup(homePath, target, time.Unix(1, 0))
	if err == nil || !strings.Contains(err.Error(), "sync boom") {
		t.Fatalf("expected sync error, got %v", err)
	}
	if _, statErr := os.Lstat(target); !os.IsNotExist(statErr) {
		t.Fatalf("final target should not exist after failed sync, stat=%v", statErr)
	}
}

func TestBackupSameTargetOneWinner(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "test", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "home.tar.gz")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = Backup(homePath, target, time.Unix(1, 0))
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("expected exactly one winner, got %d errs=%v", wins, errs)
	}
	if _, err := VerifyArchive(target); err != nil {
		t.Fatalf("winner archive should verify: %v", err)
	}
}

func TestExportPublicFixtureTransactionalVerify(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "test", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "fixture.tar.gz")
	if _, err := ExportPublicFixture(homePath, target, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	verify, err := VerifyArchive(target)
	if err != nil {
		t.Fatal(err)
	}
	if verify.Kind != ArchiveKindPublicFixture || !verify.OK {
		t.Fatalf("unexpected verify report: %+v", verify)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(homePath)) {
		t.Fatalf("public fixture compressed bytes contain source home %q", homePath)
	}
	f, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(gz)
	if closeErr := errors.Join(gz.Close(), f.Close()); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(decoded, []byte(homePath)) {
		t.Fatalf("public fixture decoded bytes contain source home %q", homePath)
	}
	manifest, _, err := readArchive(target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SourceHome != "" {
		t.Fatalf("public fixture source_home = %q, want omitted", manifest.SourceHome)
	}
}
