package home

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkStreamedHomeRecovery820k is opt-in resource acceptance, not a unit-test gate.
// Run with -benchtime=1x so the fixture is not duplicated.
func BenchmarkStreamedHomeRecovery820k(b *testing.B) {
	const files = 820_000
	now := time.Unix(1_700_000_000, 0).UTC()
	base := b.TempDir()
	homePath := filepath.Join(base, "source")
	if err := Init(homePath, "test", now); err != nil {
		b.Fatal(err)
	}
	ledgerPath := filepath.Join(homePath, "ledger")
	fixtureStart := time.Now()
	for i := range files {
		path := filepath.Join(ledgerPath, fmt.Sprintf("synthetic-%06d.json", i))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			b.Fatalf("create synthetic file %d: %v", i, err)
		}
		if err := f.Close(); err != nil {
			b.Fatalf("close synthetic file %d: %v", i, err)
		}
	}
	fixtureDuration := time.Since(fixtureStart)
	fmt.Printf("recovery phase=fixture_complete files=%d elapsed=%s\n", files, fixtureDuration)
	archivePath := filepath.Join(base, "backup.tar.gz")
	targetPath := filepath.Join(base, "restored")
	b.ResetTimer()
	backupStart := time.Now()
	if _, err := BackupWithFormat(homePath, archivePath, ArchiveFormatStreamedV2, now); err != nil {
		b.Fatal(err)
	}
	backupDuration := time.Since(backupStart)
	fmt.Printf("recovery phase=backup_complete elapsed=%s\n", backupDuration)
	verifyStart := time.Now()
	if report, err := VerifyArchive(archivePath); err != nil || !report.OK {
		b.Fatalf("verify: report=%+v err=%v", report, err)
	}
	verifyDuration := time.Since(verifyStart)
	fmt.Printf("recovery phase=verify_complete elapsed=%s\n", verifyDuration)
	restoreStart := time.Now()
	if _, err := RestoreArchive(archivePath, targetPath, now); err != nil {
		b.Fatal(err)
	}
	restoreDuration := time.Since(restoreStart)
	fmt.Printf("recovery phase=restore_complete elapsed=%s\n", restoreDuration)
	b.StopTimer()
	b.ReportMetric(fixtureDuration.Seconds(), "fixture-s")
	b.ReportMetric(backupDuration.Seconds(), "backup-s")
	b.ReportMetric(verifyDuration.Seconds(), "verify-s")
	b.ReportMetric(restoreDuration.Seconds(), "restore-s")
	b.ReportMetric(float64(files), "synthetic-files")
}
