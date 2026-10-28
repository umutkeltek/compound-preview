//go:build darwin

package home

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// Run explicitly with -benchtime=1x. The default is a quick cardinality sample;
// COMPOUND_CAPTURE_BENCH_FILES=1000000 exercises the production failure scale.
// A successful archive AND restore are required; a fast refusal is not success.
func BenchmarkCapturedBackupCardinality(b *testing.B) {
	if b.N != 1 {
		b.Fatal("use -benchtime=1x for the file-backed cardinality fixture")
	}
	count := 10000
	if raw := os.Getenv("COMPOUND_CAPTURE_BENCH_FILES"); raw != "" {
		var err error
		count, err = strconv.Atoi(raw)
		if err != nil || count < 1 || count > 1000000 {
			b.Fatal("invalid cardinality fixture size")
		}
	}
	b.StopTimer()
	fixture := b.TempDir()
	retained := os.Getenv("COMPOUND_CAPTURE_BENCH_RETAIN")
	if retained != "" {
		var err error
		fixture, err = createCaptureBenchmarkFixture(retained)
		if err != nil {
			b.Fatal(err)
		}
		b.Logf("caller-owned synthetic fixture retained at %s; cleanup is a separate operation", fixture)
	}
	root := filepath.Join(fixture, "home")
	if err := Init(root, "test", time.Now()); err != nil {
		b.Fatal(err)
	}
	base := filepath.Join(root, "context", "cardinality")
	for part := 0; part < 256; part++ {
		if err := os.MkdirAll(filepath.Join(base, fmt.Sprintf("%03d", part)), 0700); err != nil {
			b.Fatal(err)
		}
	}
	for i := 0; i < count; i++ {
		p := filepath.Join(base, fmt.Sprintf("%03d", i%256), fmt.Sprintf("%09d.json", i))
		if err := os.WriteFile(p, []byte(fmt.Sprintf("{\"fixture\":%d}\n", i)), 0600); err != nil {
			b.Fatal(err)
		}
		if i > 0 && i%100000 == 0 {
			b.Logf("fixture-created=%d", i)
		}
	}
	for _, name := range []string{"a.json", "b.json"} {
		if err := os.WriteFile(filepath.Join(root, "ledger", name), []byte("initial"), 0600); err != nil {
			b.Fatal(err)
		}
	}
	before, ready, warm, started := archiveCaptureBeforeClone, archiveStreamCaptureReady, archiveWarmCaptureReady, archiveCaptureReconcileStarted
	oldClone, oldReconciled := archiveCaptureClone, archiveCaptureReconciled
	defer func() {
		archiveCaptureBeforeClone, archiveStreamCaptureReady, archiveWarmCaptureReady, archiveCaptureReconcileStarted = before, ready, warm, started
		archiveCaptureClone, archiveCaptureReconciled = oldClone, oldReconciled
	}()
	target := filepath.Join(fixture, "archive.tar.gz")
	var writerCount atomic.Int64
	stop := make(chan struct{})
	done := make(chan error, 1)
	writerStarted := false
	writerStopped := false
	var expected string
	start := time.Now()
	var warmElapsed, lockElapsed time.Duration
	cloneCount := 0
	clone := archiveCaptureClone
	archiveCaptureClone = func(src, dst int, name string, flags int) error {
		err := clone(src, dst, name, flags)
		if err == nil {
			cloneCount++
			if cloneCount%100000 == 0 {
				b.Logf("warm-cloned=%d elapsed=%s", cloneCount, time.Since(start))
			}
		}
		return err
	}
	archiveCaptureReconciled = func(c *capturedArchive, err error) {
		b.Logf("reconcile-finished elapsed=%s error=%v", c.admissionDuration, err)
	}
	archiveCaptureBeforeClone = func(rel string) error {
		if writerStarted || rel == "ledger/a.json" || rel == "ledger/b.json" {
			return nil
		}
		writerStarted = true
		go func() {
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					done <- nil
					return
				case <-ticker.C:
					err := WithAutonomyAdmissionLock(root, func() error {
						n := writerCount.Add(1)
						for _, name := range []string{"a.json", "b.json"} {
							if err := os.WriteFile(filepath.Join(root, "ledger", name), []byte(fmt.Sprintf("generation:%d", n)), 0600); err != nil {
								return err
							}
						}
						return nil
					})
					if err != nil {
						done <- err
						return
					}
				}
			}
		}()
		return nil
	}
	defer func() {
		if writerStarted && !writerStopped {
			close(stop)
			<-done
		}
	}()
	archiveWarmCaptureReady = func(*capturedArchive) error {
		warmElapsed = time.Since(start)
		if writerStarted {
			close(stop)
			err := <-done
			writerStopped = true
			if err != nil {
				return err
			}
		}
		expected = fmt.Sprintf("generation:%d", writerCount.Load())
		if writerCount.Load() < 2 {
			return fmt.Errorf("ordinary writers did not make progress during warm capture")
		}
		b.Logf("warm-complete files=%d elapsed=%s writer_transactions=%d", count, warmElapsed, writerCount.Load())
		return nil
	}
	archiveStreamCaptureReady = func(c *capturedArchive) error {
		lockElapsed = c.admissionDuration
		b.Logf("reconciliation-complete elapsed=%s", lockElapsed)
		return nil
	}
	b.StartTimer()
	if _, err := BackupWithFormat(root, target, ArchiveFormatStreamedV3EventSnapshot, time.Now()); err != nil {
		b.Fatal(err)
	}
	b.Log("archive-published")
	report, err := VerifyArchive(target)
	if err != nil || !report.OK {
		b.Fatalf("verify: %+v %v", report, err)
	}
	restored := filepath.Join(fixture, "restored")
	if _, err := RestoreArchive(target, restored, time.Now()); err != nil {
		b.Fatal(err)
	}
	for _, name := range []string{"a.json", "b.json"} {
		data, err := os.ReadFile(filepath.Join(restored, "ledger", name))
		if err != nil || string(data) != expected {
			b.Fatalf("mixed generation: %s=%q %v; want %q", name, data, err, expected)
		}
	}
	for i := 0; i < count; i++ {
		p := filepath.Join(restored, "context", "cardinality", fmt.Sprintf("%03d", i%256), fmt.Sprintf("%09d.json", i))
		data, err := os.ReadFile(p)
		if err != nil || string(data) != fmt.Sprintf("{\"fixture\":%d}\n", i) {
			b.Fatalf("body %d: %q %v", i, data, err)
		}
	}
	if retained != "" {
		outcome := map[string]any{
			"schema": "compound.synthetic.capture-outcome.v1", "archive": target,
			"archive_sha256": report.ArchiveSHA256, "restored": filepath.Join(fixture, "restore-recheck"),
			"captured_restored": restored, "files": count, "writer_transactions": writerCount.Load(),
			"warm_ns": warmElapsed.Nanoseconds(), "reconciliation_ns": lockElapsed.Nanoseconds(),
		}
		body, err := json.MarshalIndent(outcome, "", "  ")
		if err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixture, "outcome.json"), body, 0600); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(count), "files")
	b.ReportMetric(warmElapsed.Seconds(), "warm-s")
	b.ReportMetric(lockElapsed.Seconds(), "reconcile-s")
	b.ReportMetric(float64(writerCount.Load()), "writer-transactions")
	b.Logf("restore-verified files=%d", count)
}

// This optional benchmark-owned output is never removed by testing cleanup.
// Exclusive creation refuses reuse; callers retain the exact path and outcome.
func createCaptureBenchmarkFixture(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("retained fixture requires absolute path")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	if parent != filepath.Dir(path) {
		return "", fmt.Errorf("retained fixture parent must be canonical")
	}
	if err := os.Mkdir(path, 0700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(path, "fixture-owner"), []byte("compound.synthetic.capture-benchmark.v1\n"), 0600); err != nil {
		return "", err
	}
	return path, nil
}

func TestCapturedBenchmarkRetainedFixtureRefusesExistingPath(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "retained")
	if _, err := createCaptureBenchmarkFixture(path); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(path, "sentinel")
	if err := os.WriteFile(marker, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := createCaptureBenchmarkFixture(path); err == nil {
		t.Fatal("existing fixture reused")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "preserve" {
		t.Fatalf("existing body changed: %q %v", data, err)
	}
}
