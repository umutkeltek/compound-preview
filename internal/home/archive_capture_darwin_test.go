//go:build darwin

package home

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func captureTestHome(t *testing.T) (string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "home")
	if err := Init(root, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.json", "b.json"} {
		if err := os.WriteFile(filepath.Join(root, "ledger", name), []byte("before"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "events", "sample.jsonl"), []byte("prefix\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(t.TempDir(), "backup.tar.gz")
}
func captureTestHooks(t *testing.T) {
	t.Helper()
	before, clone, ready := archiveCaptureBeforeClone, archiveCaptureClone, archiveStreamCaptureReady
	warm, reconcile, clock := archiveWarmCaptureReady, archiveCaptureReconcileStarted, archiveCaptureNow
	reconciled := archiveCaptureReconciled
	t.Cleanup(func() {
		archiveCaptureBeforeClone, archiveCaptureClone, archiveStreamCaptureReady = before, clone, ready
		archiveWarmCaptureReady, archiveCaptureReconcileStarted, archiveCaptureNow = warm, reconcile, clock
		archiveCaptureReconciled = reconciled
	})
}
func requireCaptureClean(t *testing.T, root string) {
	t.Helper()
	stages, err := filepath.Glob(filepath.Join(root, "tmp", "home-backup-snapshot-*"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("capture stage leak: %v, %v", stages, err)
	}
}

// A file-backed, incompressible body exercises real capture and gzip I/O.
// The admission writer starts during final reconciliation, then must finish while the
// serializer is deliberately held at its first boundary.
func TestCapturedBackupReleasesAdmissionAndSealsInputs(t *testing.T) {
	captureTestHooks(t)
	root, target := captureTestHome(t)
	modified := time.Now().Add(-time.Hour).Truncate(time.Second)
	a := filepath.Join(root, "ledger", "a.json")
	if err := os.Chtimes(a, modified, modified); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.json", filepath.Join(root, "ledger", "current")); err != nil {
		t.Fatal(err)
	}
	large, err := os.Create(filepath.Join(root, "events", "large.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	buffer := make([]byte, 64<<10)
	for n := 0; n < 1024; n++ {
		if _, err := rand.Read(buffer); err != nil {
			t.Fatal(err)
		}
		if _, err := io.MultiWriter(large, hash).Write(buffer); err != nil {
			t.Fatal(err)
		}
	}
	if err := large.Close(); err != nil {
		t.Fatal(err)
	}
	expected := hex.EncodeToString(hash.Sum(nil))
	// More than one directory batch, without an in-memory file list.
	for n := 0; n < archiveWalkBatch+1; n++ {
		if err := os.WriteFile(filepath.Join(root, "ledger", fmt.Sprintf("extra-%03d.json", n)), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writerDone := make(chan error, 1)
	started, writerJoined := false, false
	t.Cleanup(func() {
		if started && !writerJoined {
			select {
			case <-writerDone:
			case <-time.After(5 * time.Second):
				t.Error("admission writer did not terminate")
			}
		}
	})
	archiveCaptureReconcileStarted = func(*capturedArchive) error {
		held, err := AutonomyAdmissionLockHeld(root)
		if err != nil || !held {
			return fmt.Errorf("capture is not admission fenced: held=%v err=%v", held, err)
		}
		started = true
		go func() {
			writerDone <- WithAutonomyAdmissionLock(root, func() error {
				for _, name := range []string{"a.json", "b.json"} {
					if err := os.WriteFile(filepath.Join(root, "ledger", name), []byte("after!"), 0o600); err != nil {
						return err
					}
				}
				return nil
			})
		}()
		return nil
	}
	start := time.Now()
	var captureDuration time.Duration
	archiveStreamCaptureReady = func(c *capturedArchive) error {
		captureDuration = time.Since(start)
		if !started {
			return errors.New("writer never started")
		}
		select {
		case err := <-writerDone:
			writerJoined = true
			if err != nil {
				return err
			}
		case <-time.After(5 * time.Second):
			return errors.New("admission writer blocked while compression was paused")
		}
		if err := os.Rename(filepath.Join(root, "events", "large.jsonl"), filepath.Join(root, "events", "renamed.jsonl")); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(root, "events", "large.jsonl"), []byte("replacement"), 0o600); err != nil {
			return err
		}
		for _, dir := range []string{filepath.Join(root, "tmp", c.stageName)} {
			info, err := os.Stat(dir)
			if err != nil || info.Mode().Perm() != 0o700 {
				return fmt.Errorf("capture permissions: %v %v", info, err)
			}
		}
		return nil
	}
	_, err = BackupWithFormat(root, target, ArchiveFormatStreamedV3EventSnapshot, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	total := time.Since(start)
	t.Logf("64 MiB plus batched metadata: warm+reconciliation=%s total=%s", captureDuration, total)
	if captureDuration <= 0 || total <= captureDuration {
		t.Fatal("missing separate capture and compression observations")
	}
	report, err := VerifyArchive(target)
	if err != nil || !report.OK {
		t.Fatalf("verify: %+v %v", report, err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if _, err := RestoreArchive(target, restored, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.json", "b.json"} {
		data, err := os.ReadFile(filepath.Join(restored, "ledger", name))
		if err != nil || string(data) != "before" {
			t.Fatalf("capture interleaved with canonical writer: %s %q %v", name, data, err)
		}
	}
	info, err := os.Stat(filepath.Join(restored, "ledger", "a.json"))
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("metadata changed: %v %v", info, err)
	}

	// Restore's existing contract restores mode, not source timestamps.
	// The archived header must still preserve the captured source mtime.
	archive, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	gz, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	found := false
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "ledger/a.json" {
			found = true
			if !header.ModTime.Equal(modified) || header.Mode != 0o640 {
				t.Fatalf("captured header metadata changed: %+v", header)
			}
			break
		}
	}
	if !found {
		t.Fatal("captured header missing")
	}
	link, err := os.Readlink(filepath.Join(restored, "ledger", "current"))
	if err != nil || link != "a.json" {
		t.Fatalf("link changed: %s %v", link, err)
	}
	body, err := os.Open(filepath.Join(restored, "events", "large.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	observed := sha256.New()
	n, err := io.Copy(observed, body)
	body.Close()
	if err != nil || n != 64<<20 || hex.EncodeToString(observed.Sum(nil)) != expected {
		t.Fatal("post-capture replacement affected archive")
	}
	requireCaptureClean(t, root)
}

func TestCapturedBackupEventAppendDuringClone(t *testing.T) {
	captureTestHooks(t)
	root, target := captureTestHome(t)
	native := archiveCaptureClone
	archiveCaptureClone = func(src, dst int, name string, flags int) error {
		if err := native(src, dst, name, flags); err != nil {
			return err
		}
		// Append after every clone: some captures see longer prefixes, all
		// must remain immutable and the original source inode stays pinned.
		f, err := os.OpenFile(filepath.Join(root, "events", "sample.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		_, err = f.WriteString("append\n")
		return errors.Join(err, f.Close())
	}
	if _, err := BackupWithFormat(root, target, ArchiveFormatStreamedV3EventSnapshot, time.Now()); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if _, err := RestoreArchive(target, restored, time.Now()); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(filepath.Join(restored, "events", "sample.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	live, err := os.ReadFile(filepath.Join(root, "events", "sample.jsonl"))
	if err != nil || !bytes.HasPrefix(live, saved) || len(live) <= len(saved) {
		t.Fatalf("not an immutable captured prefix: %q / %q (%v)", saved, live, err)
	}
	requireCaptureClean(t, root)
}

func TestCapturedBackupRejectsSourceDriftAndCloneFailure(t *testing.T) {
	for _, kind := range []string{"replace", "symlink", "in-place", "event-replace", "clone-error"} {
		t.Run(kind, func(t *testing.T) {
			captureTestHooks(t)
			root, target := captureTestHome(t)
			rel := "ledger/a.json"
			if kind == "event-replace" {
				rel = "events/sample.jsonl"
			}
			if kind == "clone-error" {
				archiveCaptureClone = func(int, int, string, int) error { return syscall.ENOTSUP }
			} else {
				archiveCaptureBeforeClone = func(name string) error {
					if name != rel {
						return nil
					}
					path := filepath.Join(root, filepath.FromSlash(rel))
					switch kind {
					case "replace", "event-replace":
						if err := os.Rename(path, path+".old"); err != nil {
							return err
						}
						return os.WriteFile(path, []byte("replacement"), 0o600)
					case "symlink":
						if err := os.Rename(path, path+".old"); err != nil {
							return err
						}
						return os.Symlink(filepath.Base(path)+".old", path)
					default:
						info, err := os.Stat(path)
						if err != nil {
							return err
						}
						if err := os.WriteFile(path, []byte("after!"), 0o640); err != nil {
							return err
						}
						return os.Chtimes(path, info.ModTime().Add(time.Second), info.ModTime().Add(time.Second))
					}
				}
			}
			_, err := BackupWithFormat(root, target, ArchiveFormatStreamedV3EventSnapshot, time.Now())
			if err == nil {
				t.Fatal("unstable source or unavailable clone accepted")
			}
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed capture published target: %v", err)
			}
			requireCaptureClean(t, root)
		})
	}
}

func TestCapturedBackupRejectsPrivateInputDrift(t *testing.T) {
	for _, kind := range []string{"body-replace", "body-append", "body-rewrite", "body-mode", "journal-replace", "journal-edit", "stage-replace"} {
		t.Run(kind, func(t *testing.T) {
			captureTestHooks(t)
			root, target := captureTestHome(t)
			var replacement string
			archiveStreamCaptureReady = func(c *capturedArchive) error {
				stage := filepath.Join(root, "tmp", c.stageName)
				body := filepath.Join(stage, captureBodyName(1))
				switch kind {
				case "body-replace":
					if err := os.Remove(body); err != nil {
						return err
					}
					return os.WriteFile(body, []byte("replacement"), 0o600)
				case "body-append":
					f, err := os.OpenFile(body, os.O_APPEND|os.O_WRONLY, 0)
					if err != nil {
						return err
					}
					_, err = f.WriteString("changed")
					return errors.Join(err, f.Close())
				case "body-rewrite":
					info, err := os.Stat(body)
					if err != nil {
						return err
					}
					data, err := os.ReadFile(body)
					if err != nil {
						return err
					}
					if len(data) == 0 {
						return errors.New("body fixture is empty")
					}
					data[0] ^= 1
					if err := os.WriteFile(body, data, info.Mode().Perm()); err != nil {
						return err
					}
					return os.Chtimes(body, info.ModTime(), info.ModTime())
				case "body-mode":
					return os.Chmod(body, 0o644)
				case "journal-replace":
					if err := os.Remove(filepath.Join(stage, "journal")); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(stage, "journal"), []byte("{}\n"), 0o600)
				case "journal-edit":
					path := filepath.Join(stage, "journal")
					data, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					// Same-size valid JSON edit must fail the sealed digest,
					// even when the mtime is deliberately restored.
					info, err := os.Stat(path)
					if err != nil {
						return err
					}
					changed := bytes.Replace(data, []byte("ledger/a.json"), []byte("ledger/z.json"), 1)
					if bytes.Equal(changed, data) {
						return errors.New("journal fixture path absent")
					}
					if err := os.WriteFile(path, changed, 0o600); err != nil {
						return err
					}
					return os.Chtimes(path, info.ModTime(), info.ModTime())
				default:
					if err := os.Rename(stage, stage+"-moved"); err != nil {
						return err
					}
					if err := os.Mkdir(stage, 0o700); err != nil {
						return err
					}
					replacement = filepath.Join(stage, "foreign")
					return os.WriteFile(replacement, []byte("preserve"), 0o600)
				}
			}
			_, err := BackupWithFormat(root, target, ArchiveFormatStreamedV3EventSnapshot, time.Now())
			if err == nil {
				t.Fatal("tampered capture accepted")
			}
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed replay published target: %v", err)
			}
			if replacement != "" {
				data, err := os.ReadFile(replacement)
				if err != nil || string(data) != "preserve" {
					t.Fatalf("cleanup touched replacement: %q %v", data, err)
				}
			} else {
				requireCaptureClean(t, root)
			}
		})
	}
}

type mutateCaptureOnHeader struct {
	archiveEntryWriter
	mutate func() error
}

func (w mutateCaptureOnHeader) WriteHeader(h *tar.Header) error {
	if err := w.archiveEntryWriter.WriteHeader(h); err != nil {
		return err
	}
	return w.mutate()
}
func TestCapturedBodyRejectsDriftDuringReplay(t *testing.T) {
	root, _ := captureTestHome(t)
	c, err := captureStreamedArchive(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	err = c.walk(func(item archiveStreamItem) error {
		if item.rel != "ledger/a.json" {
			return nil
		}
		writer := mutateCaptureOnHeader{archiveEntryWriter: tar.NewWriter(io.Discard), mutate: func() error {
			for n := 1; n <= c.bodies; n++ {
				if err := c.stage.Chmod(captureBodyName(n), 0o644); err != nil {
					return err
				}
			}
			return nil
		}}
		_, err := item.write(writer)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "capture body changed") {
		t.Fatalf("replay mutation accepted: %v", err)
	}
}

// Test metadata admission directly: the host filesystem may reject opaque-byte
// names before the walker can observe them, while mounted filesystems may not.
func TestCapturedJournalPathEncoding(t *testing.T) {
	t.Run("invalid directory name", func(t *testing.T) {
		record := archiveCaptureRecord{Header: tar.Header{Name: "ledger/opaque-\xff", Typeflag: tar.TypeDir}}
		if data, err := marshalArchiveCaptureRecord(record); err == nil || !strings.Contains(err.Error(), "UTF-8") || len(data) != 0 {
			t.Fatalf("invalid path admitted: bytes=%q err=%v", data, err)
		}
	})
	t.Run("Unicode directory round trip", func(t *testing.T) {
		record := archiveCaptureRecord{Header: tar.Header{Name: "ledger/目录/é-Δ", Typeflag: tar.TypeDir}}
		data, err := marshalArchiveCaptureRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) == 0 || data[len(data)-1] != '\n' {
			t.Fatalf("missing journal line terminator: %q", data)
		}
		var decoded archiveCaptureRecord
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Header.Name != record.Header.Name || decoded.Header.Typeflag != tar.TypeDir {
			t.Fatalf("path changed during encoding: %#v", decoded.Header)
		}
	})
}
