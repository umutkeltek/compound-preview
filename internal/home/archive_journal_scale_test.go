package home

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// The scale pins for the bounded journal append (work_4af3e7aebe82).
//
// Defect being falsified: operationalArchiveJournalAppend used to read the
// whole index, append the row in memory, and rewrite the whole file through
// the CAS — twice per archived file (pending + committed). Archiving N files
// therefore wrote ~(2N^2+N)·rowsize index bytes: measured against the live
// home (49 MB / 119,566 rows, 2026-09-04) that is ~196 MB of index I/O per
// archived file and ~22 GB per routine 115-file hourly tick, growing without
// bound. The fix appends each row in place, so archiving N files must write
// exactly 2·N rows.
//
// Bytes are counted by operationalArchiveIndexWriteObserver at every durable
// index write (the same instrumentation point the pre-fix instrumentation
// build uses), so the pins below run unchanged against the old implementation
// and fail there: quadratic bytes blow both the absolute bound (~166x at
// N=1000) and the doubling ratio (~4.0x where linear is ~2.0x).
//
// fsync seams (operationalArchiveJournalFileSync, operationalArchiveDirSync,
// syncRootedDirectoryFile) are no-op'd here ONLY so the 20k pin runs in
// seconds; per-append fsync cost is orthogonal to write amplification and is
// exercised with real fsyncs by the crash-recovery tests.

type journalScaleMeasurement struct {
	files        int64
	bytesWritten int64
	duration     time.Duration
	peakHeap     int64
}

func scaleSeamsOff(t *testing.T) {
	t.Helper()
	oldJournalSync := operationalArchiveJournalFileSync
	oldDirSync := operationalArchiveDirSync
	oldRootedSync := syncRootedDirectoryFile
	operationalArchiveJournalFileSync = func(*os.File) error { return nil }
	operationalArchiveDirSync = func(*os.File) error { return nil }
	syncRootedDirectoryFile = func(string, *os.File) error { return nil }
	t.Cleanup(func() {
		operationalArchiveJournalFileSync = oldJournalSync
		operationalArchiveDirSync = oldDirSync
		syncRootedDirectoryFile = oldRootedSync
	})
}

func scaleObserver(t *testing.T) *int64 {
	t.Helper()
	var bytesWritten int64
	observer := operationalArchiveIndexWriteObserver
	operationalArchiveIndexWriteObserver = func(n int) { atomic.AddInt64(&bytesWritten, int64(n)) }
	t.Cleanup(func() { operationalArchiveIndexWriteObserver = observer })
	return &bytesWritten
}

// journalScaleWorkspace seeds a fresh home with n fixed-width terminal work
// files, then measures peak heap on a sampling goroutine.
func journalScaleWorkspace(t *testing.T, n int) (string, *int64) {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "global", archiveTestNow); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		rel := filepath.ToSlash(filepath.Join("work", "work", scaleWorkName(i)))
		writeHomeFile(t, h, rel, `{"id":"`+scaleWorkName(i)+`","state":"completed"}`)
	}
	var peakHeap int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var mem runtime.MemStats
		for {
			select {
			case <-stop:
				return
			default:
				// ReadMemStats stops the world; sampling often enough to
				// catch the peak, rarely enough not to BE the workload.
				runtime.ReadMemStats(&mem)
				if int64(mem.HeapAlloc) > atomic.LoadInt64(&peakHeap) {
					atomic.StoreInt64(&peakHeap, int64(mem.HeapAlloc))
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
	return h, &peakHeap
}

func scaleWorkName(i int) string {
	return fmt.Sprintf("work_scale_a%06d.json", i)
}

// journalScaleCommitLoop drives commitJournaledOperationalArchive directly —
// the exact per-file journal unit the defect lives in (one pending append +
// one committed append per file).
func journalScaleCommitLoop(t *testing.T, h string, n int, peakHeap *int64) journalScaleMeasurement {
	t.Helper()
	var files int64
	started := time.Now()
	for i := 0; i < n; i++ {
		rel := filepath.ToSlash(filepath.Join("work", "work", scaleWorkName(i)))
		cand := operationalCandidate{Kind: "work", Status: "completed", OriginalPath: rel}
		entry, err := archiveOperationalFile(h, cand, archiveTestNow, false)
		if err != nil {
			t.Fatalf("archiveOperationalFile %s: %v", rel, err)
		}
		if _, err := commitJournaledOperationalArchive(h, entry); err != nil {
			t.Fatalf("commitJournaledOperationalArchive %s: %v", rel, err)
		}
		files++
	}
	return journalScaleMeasurement{files: files, duration: time.Since(started), peakHeap: atomic.LoadInt64(peakHeap)}
}

func scaleRowSize(t *testing.T, h string, rows int64) int64 {
	t.Helper()
	info, err := os.Stat(OperationalArchiveIndexPath(h))
	if err != nil {
		t.Fatal(err)
	}
	return info.Size() / rows
}

// TestOperationalArchiveJournalBytesLinear1k is the falsifier: 1,000 and
// 2,000 archived entries must show total index bytes written growing LINEARLY
// with N, within a small constant of the two-rows-per-file truth. Against the
// pre-fix whole-index-rewrite implementation this fails on both the absolute
// bound (quadratic ≈ N·(N+1)·rowsize) and the doubling ratio (≈4.0).
func TestOperationalArchiveJournalBytesLinear1k(t *testing.T) {
	scaleSeamsOff(t)
	bytesWritten := scaleObserver(t)

	h1, peak1 := journalScaleWorkspace(t, 1000)
	m1 := journalScaleCommitLoop(t, h1, 1000, peak1)
	b1 := atomic.LoadInt64(bytesWritten)
	row1 := scaleRowSize(t, h1, 2000)

	h2, peak2 := journalScaleWorkspace(t, 2000)
	m2 := journalScaleCommitLoop(t, h2, 2000, peak2)
	b2 := atomic.LoadInt64(bytesWritten) - b1
	row2 := scaleRowSize(t, h2, 4000)

	t.Logf("1k: files=%d bytes=%d rowSize=%d (%.1f rows/file) duration=%s peakHeap=%dMB",
		m1.files, b1, row1, float64(b1)/float64(row1)/1000, m1.duration, m1.peakHeap>>20)
	t.Logf("2k: files=%d bytes=%d rowSize=%d (%.1f rows/file) duration=%s peakHeap=%dMB",
		m2.files, b2, row2, float64(b2)/float64(row2)/2000, m2.duration, m2.peakHeap>>20)

	// Absolute: two rows per file plus modest headroom. The pre-fix code
	// writes ~N·(N+1)/2 whole-file images per journal step.
	const headroom = 3
	if want := int64(headroom) * 2 * row1 * 1000; b1 > want {
		t.Fatalf("1k pass wrote %d index bytes; linear bound is %d (rowSize=%d). Write amplification is super-linear — see work_4af3e7aebe82.", b1, want, row1)
	}
	if want := int64(headroom) * 2 * row2 * 2000; b2 > want {
		t.Fatalf("2k pass wrote %d index bytes; linear bound is %d (rowSize=%d)", b2, want, row2)
	}
	// Linearity: doubling N must ~double the bytes (quadratic quadruples).
	ratio := float64(b2) / float64(b1)
	if ratio > 3 {
		t.Fatalf("bytes-written grew %.2fx when N doubled; linear growth is ~2x, quadratic ~4x (1k=%d 2k=%d)", ratio, b1, b2)
	}
	if m1.duration > 60*time.Second || m2.duration > 120*time.Second {
		t.Fatalf("journal commit loop too slow: 1k=%s 2k=%s", m1.duration, m2.duration)
	}
	if m1.peakHeap > 256<<20 || m2.peakHeap > 256<<20 {
		t.Fatalf("journal commit loop peak heap too high: 1k=%dMB 2k=%dMB", m1.peakHeap>>20, m2.peakHeap>>20)
	}
	// The append-only shape: total bytes written == final file size. A
	// whole-file rewrite per row writes far more than the file it leaves.
	if info, err := os.Stat(OperationalArchiveIndexPath(h2)); err != nil {
		t.Fatal(err)
	} else if info.Size() != b2 {
		t.Fatalf("append-only shape violated: 2k index file is %d bytes but the pass wrote %d", info.Size(), b2)
	}
	// And the index must be loadable, fully committed, newline-terminated.
	active, err := LoadOperationalArchiveIndex(h2)
	if err != nil {
		t.Fatalf("load 2k index: %v", err)
	}
	if len(active) != 2000 {
		t.Fatalf("2k index active entries = %d, want 2000", len(active))
	}
	for _, entry := range active {
		if entry.Operation != "archived" {
			t.Fatalf("2k index has non-committed entry %+v", entry)
		}
	}
}

// TestOperationalArchiveJournalBytes20k pins the 20,000-entry point: the
// measured bytes/duration/peak-memory a real drain costs once the append is
// bounded. It also re-proves the append-only shape (bytes written == final
// file size) at a scale where the pre-fix implementation would have written
// ~344 GB.
func TestOperationalArchiveJournalBytes20k(t *testing.T) {
	scaleSeamsOff(t)
	bytesWritten := scaleObserver(t)

	h, peak := journalScaleWorkspace(t, 20000)
	m := journalScaleCommitLoop(t, h, 20000, peak)
	b := atomic.LoadInt64(bytesWritten)
	row := scaleRowSize(t, h, 40000)

	t.Logf("20k: files=%d bytes=%d rowSize=%d (%.1f rows/file) duration=%s peakHeap=%dMB",
		m.files, b, row, float64(b)/float64(row)/20000, m.duration, m.peakHeap>>20)

	if want := int64(3) * 2 * row * 20000; b > want {
		t.Fatalf("20k pass wrote %d index bytes; linear bound is %d (rowSize=%d)", b, want, row)
	}
	if info, err := os.Stat(OperationalArchiveIndexPath(h)); err != nil {
		t.Fatal(err)
	} else if info.Size() != b {
		t.Fatalf("append-only shape violated: 20k index file is %d bytes but the pass wrote %d", info.Size(), b)
	}
	// Keep the deadline unconditional alongside the structural bounds. A slow
	// run is evidence to investigate, not permission to skip the assertion.
	if m.duration > 120*time.Second {
		t.Fatalf("20k journal commit loop took %s; ceiling is 120s", m.duration)
	}
	if m.peakHeap > 256<<20 {
		t.Fatalf("20k journal commit loop peak heap = %dMB; ceiling is 256MB", m.peakHeap>>20)
	}
}

// TestOperationalArchiveEndToEndBytesLinear1k runs the production pass
// (ArchiveOperationalHistory) over 1,000 candidates and pins the same linear
// write bound end to end: listing, reconcile, per-file journal commits.
func TestOperationalArchiveEndToEndBytesLinear1k(t *testing.T) {
	scaleSeamsOff(t)
	bytesWritten := scaleObserver(t)

	h, peak := journalScaleWorkspace(t, 1000)
	started := time.Now()
	report, err := ArchiveOperationalHistory(h, OperationalArchiveOptions{Kinds: []string{"work"}}, archiveTestNow)
	if err != nil {
		t.Fatal(err)
	}
	duration := time.Since(started)
	peakHeap := atomic.LoadInt64(peak)
	b := atomic.LoadInt64(bytesWritten)
	row := scaleRowSize(t, h, 2000)

	t.Logf("e2e 1k: archived=%d bytes=%d rowSize=%d (%.1f rows/file) duration=%s peakHeap=%dMB",
		report.ArchivedCount, b, row, float64(b)/float64(row)/1000, duration, peakHeap>>20)

	if report.ArchivedCount != 1000 {
		t.Fatalf("e2e archived = %d, want 1000", report.ArchivedCount)
	}
	if want := int64(3) * 2 * row * 1000; b > want {
		t.Fatalf("e2e 1k pass wrote %d index bytes; linear bound is %d (rowSize=%d)", b, want, row)
	}
	if duration > 60*time.Second {
		t.Fatalf("e2e 1k pass took %s; ceiling is 60s", duration)
	}
	if peakHeap > 256<<20 {
		t.Fatalf("e2e 1k peak heap = %dMB; ceiling is 256MB", peakHeap>>20)
	}
}
