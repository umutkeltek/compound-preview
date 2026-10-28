package sourceobs

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"compound/internal/home"
)

func TestSourceManifestConcurrentMaterializersShareOneExactTarget(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", time.Date(2026, 7, 14, 15, 30, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(t.TempDir(), "large-source.bin")
	body := bytes.Repeat([]byte("compound-custody\n"), 512*1024)
	if err := os.WriteFile(sourcePath, body, 0o640); err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildOwnedSourceManifest(homePath, "concurrent_materialize", []string{sourcePath}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordSourceManifest(homePath, manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sourcePath); err != nil {
		t.Fatal(err)
	}

	targetPath := filepath.Join(t.TempDir(), "restored", "large-source.bin")
	const callers = 12
	start := make(chan struct{})
	reports := make(chan SourceManifestMaterializeReport, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			report, err := MaterializeSourceManifestItem(homePath, manifest.SourceFamily, manifest.Items[0].ID, targetPath)
			reports <- report
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(reports)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent materializer failed: %v", err)
		}
	}
	created := 0
	for report := range reports {
		if !report.AlreadyExists {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("materializers reporting target creation = %d, want exactly 1", created)
	}
	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("materialized target differs: got=%d bytes want=%d", len(got), len(body))
	}
	info, err := os.Stat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("materialized mode = %04o, want 0640", info.Mode().Perm())
	}
}

func TestSourceManifestMaterializeRefusesByteIdenticalSymlinkTarget(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", time.Date(2026, 7, 14, 15, 30, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(t.TempDir(), "source.bin")
	body := []byte("compound-owned-body\n")
	if err := os.WriteFile(sourcePath, body, 0o640); err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildOwnedSourceManifest(homePath, "symlink_materialize", []string{sourcePath}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordSourceManifest(homePath, manifest); err != nil {
		t.Fatal(err)
	}

	targetDir := t.TempDir()
	realTarget := filepath.Join(targetDir, "real.bin")
	if err := os.WriteFile(realTarget, body, 0o640); err != nil {
		t.Fatal(err)
	}
	symlinkTarget := filepath.Join(targetDir, "target.bin")
	if err := os.Symlink(realTarget, symlinkTarget); err != nil {
		t.Fatal(err)
	}

	_, err = MaterializeSourceManifestItem(homePath, manifest.SourceFamily, manifest.Items[0].ID, symlinkTarget)
	if err == nil || !strings.Contains(err.Error(), "non-regular target") {
		t.Fatalf("materialize symlink error = %v, want refusal of non-regular target", err)
	}
}

func TestSourceManifestCustodySurvivesHomeBackupRestoreWithoutSourceOrLegacy(t *testing.T) {
	now := time.Now().UTC()
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(t.TempDir(), "source.sh")
	body := []byte("#!/bin/sh\nprintf 'compound custody\\n'\n")
	if err := os.WriteFile(sourcePath, body, 0o750); err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildOwnedSourceManifest(homePath, "backup_restore_custody", []string{sourcePath}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordSourceManifest(homePath, manifest); err != nil {
		t.Fatal(err)
	}
	item := manifest.Items[0]
	if err := os.Remove(sourcePath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(LegacySourceManifestPath(homePath, manifest.SourceFamily)); !os.IsNotExist(err) {
		t.Fatalf("legacy manifest exists before backup: %v", err)
	}

	archivePath := filepath.Join(t.TempDir(), "compound-home.tar.gz")
	if _, err := home.Backup(homePath, archivePath, now); err != nil {
		t.Fatal(err)
	}
	verify, err := home.VerifyArchive(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if !verify.OK {
		t.Fatalf("archive verification = %+v", verify)
	}

	restore := func(t *testing.T) string {
		t.Helper()
		restoredHome := filepath.Join(t.TempDir(), ".compound-restored")
		if _, err := home.RestoreArchive(archivePath, restoredHome, now); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(SourceManifestPath(restoredHome, manifest.SourceFamily)); err != nil {
			t.Fatalf("canonical manifest missing after restore: %v", err)
		}
		if _, err := os.Stat(LegacySourceManifestPath(restoredHome, manifest.SourceFamily)); !os.IsNotExist(err) {
			t.Fatalf("restore synthesized legacy manifest: %v", err)
		}
		return restoredHome
	}

	restoredHome := restore(t)
	doctor, err := DoctorSourceManifest(restoredHome, manifest.SourceFamily)
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "ok" || doctor.StorageStatus != SourceManifestStorageCanonical || !doctor.CustodyComplete {
		t.Fatalf("restored manifest doctor = %+v", doctor)
	}
	targetPath := filepath.Join(t.TempDir(), "materialized", "source.sh")
	report, err := MaterializeSourceManifestItem(restoredHome, manifest.SourceFamily, item.ID, targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.Bytes != int64(len(body)) || report.AlreadyExists {
		t.Fatalf("materialize report = %+v", report)
	}
	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("materialized bytes = %q, want %q", got, body)
	}
	info, err := os.Stat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o750 {
		t.Fatalf("materialized mode = %04o, want 0750", info.Mode().Perm())
	}

	hexDigest, err := home.ArtifactHex(item.BodyRef)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		mutate     func(t *testing.T, path string)
		bodyStatus string
	}{
		{
			name: "missing artifact",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
			bodyStatus: "body_unavailable",
		},
		{
			name: "corrupt artifact",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("corrupt\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			bodyStatus: "body_hash_mismatch",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failureHome := restore(t)
			artifactPath := filepath.Join(failureHome, "artifacts", "sha256", hexDigest)
			tc.mutate(t, artifactPath)
			doctor, err := DoctorSourceManifest(failureHome, manifest.SourceFamily)
			if err != nil {
				t.Fatal(err)
			}
			if doctor.Status != "fail" || doctor.HashMismatchCount != 1 || len(doctor.Items) != 1 || doctor.Items[0].BodyStatus != tc.bodyStatus {
				t.Fatalf("doctor after %s = %+v", tc.name, doctor)
			}
			if _, err := MaterializeSourceManifestItem(failureHome, manifest.SourceFamily, item.ID, filepath.Join(t.TempDir(), "target.sh")); err == nil {
				t.Fatalf("materialization succeeded after %s", tc.name)
			}
		})
	}
}
