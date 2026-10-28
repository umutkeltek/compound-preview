//go:build darwin

package home

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCapturedBackupWarmCloneAllowsAdmissionWriter(t *testing.T) {
	captureTestHooks(t)
	root, target := captureTestHome(t)
	done := make(chan error, 1)
	started := false
	archiveCaptureBeforeClone = func(rel string) error {
		if rel != "ledger/a.json" || started {
			return nil
		}
		started = true
		go func() {
			done <- WithAutonomyAdmissionLock(root, func() error {
				return os.WriteFile(filepath.Join(root, "ledger", "b.json"), []byte("after!"), 0600)
			})
		}()
		select {
		case err := <-done:
			return err
		case <-time.After(500 * time.Millisecond):
			return errors.New("ordinary work blocked by warm body capture")
		}
	}
	_, err := BackupWithFormat(root, target, ArchiveFormatStreamedV3EventSnapshot, time.Now())
	if err != nil {
		if started {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("writer failed to drain")
			}
		}
		t.Fatal(err)
	}
	if !started {
		t.Fatal("warm clone not exercised")
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if _, err := RestoreArchive(target, restored, time.Now()); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"a.json": "before", "b.json": "after!"} {
		got, err := os.ReadFile(filepath.Join(restored, "ledger", name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	requireCaptureClean(t, root)
}

func TestCapturedBackupReconcilesCurrentGenerationAndMembership(t *testing.T) {
	captureTestHooks(t)
	root, target := captureTestHome(t)
	ledger := filepath.Join(root, "ledger")
	for _, name := range []string{"gone", "move"} {
		if err := os.WriteFile(filepath.Join(ledger, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("a.json", filepath.Join(ledger, "current")); err != nil {
		t.Fatal(err)
	}
	archiveWarmCaptureReady = func(*capturedArchive) error {
		return WithAutonomyAdmissionLock(root, func() error {
			for _, name := range []string{"a.json", "b.json"} {
				p := filepath.Join(ledger, name)
				info, err := os.Stat(p)
				if err != nil {
					return err
				}
				if err := os.WriteFile(p, []byte("after!"), 0600); err != nil {
					return err
				}
				// Same bytes count and restored mtime cannot hide an in-place change.
				if err := os.Chtimes(p, info.ModTime(), info.ModTime()); err != nil {
					return err
				}
			}
			if err := os.Remove(filepath.Join(ledger, "gone")); err != nil {
				return err
			}
			if err := os.Rename(filepath.Join(ledger, "move"), filepath.Join(ledger, "moved")); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(ledger, "added"), []byte("new"), 0600); err != nil {
				return err
			}
			if err := os.Remove(filepath.Join(ledger, "current")); err != nil {
				return err
			}
			return os.Symlink("b.json", filepath.Join(ledger, "current"))
		})
	}
	if _, err := BackupWithFormat(root, target, ArchiveFormatStreamedV3EventSnapshot, time.Now()); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if _, err := RestoreArchive(target, restored, time.Now()); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"a.json": "after!", "b.json": "after!", "added": "new", "moved": "move"} {
		data, err := os.ReadFile(filepath.Join(restored, "ledger", name))
		if err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v; want %q", name, data, err, want)
		}
	}
	for _, name := range []string{"gone", "move"} {
		if _, err := os.Lstat(filepath.Join(restored, "ledger", name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("deleted name retained: %s: %v", name, err)
		}
	}
	if link, err := os.Readlink(filepath.Join(restored, "ledger", "current")); err != nil || link != "b.json" {
		t.Fatalf("stale link: %s %v", link, err)
	}
	requireCaptureClean(t, root)
}

func TestCapturedBackupReconcileDeadlineReleasesAdmission(t *testing.T) {
	for _, phase := range []string{"start", "changed-clone"} {
		t.Run(phase, func(t *testing.T) {
			captureTestHooks(t)
			root, target := captureTestHome(t)
			final := false
			archiveWarmCaptureReady = func(*capturedArchive) error {
				return WithAutonomyAdmissionLock(root, func() error { return os.WriteFile(filepath.Join(root, "ledger", "a.json"), []byte("changed"), 0600) })
			}
			expire := func() {
				archiveCaptureNow = func() time.Time { return time.Now().Add(archiveCaptureReconcileBudget + time.Second) }
			}
			archiveCaptureReconcileStarted = func(*capturedArchive) error {
				final = true
				if phase == "start" {
					expire()
				}
				return nil
			}
			archiveCaptureBeforeClone = func(rel string) error {
				if final && phase == "changed-clone" && rel == "ledger/a.json" {
					expire()
				}
				return nil
			}
			_, err := BackupWithFormat(root, target, ArchiveFormatStreamedV3EventSnapshot, time.Now())
			if err == nil || !strings.Contains(err.Error(), "snapshot_busy") {
				t.Fatalf("deadline accepted: %v", err)
			}
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial archive published: %v", err)
			}
			if held, err := AutonomyAdmissionLockHeld(root); err != nil || held {
				t.Fatalf("admission retained: %v %v", held, err)
			}
			if err := WithAutonomyAdmissionLock(root, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			requireCaptureClean(t, root)
		})
	}
}

func TestCapturedBackupRejectsWarmCacheTamper(t *testing.T) {
	for _, name := range []string{"cache-index", "warm-journal", captureBodyName(1)} {
		t.Run(name, func(t *testing.T) {
			captureTestHooks(t)
			root, target := captureTestHome(t)
			archiveWarmCaptureReady = func(c *capturedArchive) error {
				// Keep size and mtime: ctime and sealed digests still must reject this.
				p := filepath.Join(root, "tmp", c.stageName, name)
				info, err := os.Stat(p)
				if err != nil {
					return err
				}
				f, err := os.OpenFile(p, os.O_RDWR, 0)
				if err != nil {
					return err
				}
				var b [1]byte
				_, err = f.ReadAt(b[:], 0)
				if err == nil {
					b[0] ^= 1
					_, err = f.WriteAt(b[:], 0)
				}
				closeErr := f.Close()
				if err != nil {
					return err
				}
				if closeErr != nil {
					return closeErr
				}
				return os.Chtimes(p, info.ModTime(), info.ModTime())
			}
			if _, err := BackupWithFormat(root, target, ArchiveFormatStreamedV3EventSnapshot, time.Now()); err == nil {
				t.Fatal("tampered warm cache accepted")
			}
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("tamper published archive: %v", err)
			}
			requireCaptureClean(t, root)
		})
	}
}
