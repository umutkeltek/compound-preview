//go:build darwin

package home

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStreamedRestoreDarwinFlushFailures(t *testing.T) {
	archive := streamedDurabilityArchive(t)
	for _, phase := range []string{"fsync", "fullsync_eio", "fullsync_unsupported", "fullsync_invalid", "control", "close"} {
		t.Run(phase, func(t *testing.T) {
			fsync, fullsync, control, closeFile := streamedRestoreFsync, streamedRestoreFullsync, streamedRestoreControl, streamedRestoreClose
			t.Cleanup(func() {
				streamedRestoreFsync, streamedRestoreFullsync, streamedRestoreControl, streamedRestoreClose = fsync, fullsync, control, closeFile
			})
			want := error(syscall.EIO)
			switch phase {
			case "fsync":
				streamedRestoreFsync = func(int) error { return want }
			case "fullsync_eio":
				streamedRestoreFullsync = func(int) error { return want }
			case "fullsync_unsupported":
				want = syscall.ENOTSUP
				streamedRestoreFullsync = func(int) error { return want }
			case "fullsync_invalid":
				want = syscall.EINVAL
				streamedRestoreFullsync = func(int) error { return want }
			case "control":
				streamedRestoreControl = func(*os.File, func(uintptr)) error { return want }
			case "close":
				streamedRestoreClose = func(file *os.File) error { return errors.Join(file.Close(), want) }
			}
			target := filepath.Join(t.TempDir(), "restored")
			_, err := RestoreArchive(archive, target, archiveTestNow)
			if !errors.Is(err, want) {
				t.Fatalf("lost %s error: %v", phase, err)
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatalf("failed flush published target: %v", err)
			}
		})
	}
}

func TestStreamedRestoreDarwinRetriesOnlyEINTR(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	for _, terminal := range []error{nil, syscall.EIO, syscall.ENOTSUP} {
		calls := 0
		err := streamedRestoreSync(file, func(int) error {
			calls++
			if calls == 1 {
				return syscall.EINTR
			}
			return terminal
		})
		if calls != 2 || !errors.Is(err, terminal) {
			t.Fatalf("calls=%d err=%v want=%v", calls, err, terminal)
		}
	}
}

type streamedDeviceInfo struct {
	os.FileInfo
	dev int32
}

func (info streamedDeviceInfo) Sys() any {
	stat := *info.FileInfo.Sys().(*syscall.Stat_t)
	stat.Dev = info.dev
	return &stat
}

func TestStreamedRestoreDarwinBarrierIdentity(t *testing.T) {
	for _, mutation := range []string{"symlink", "replacement_during_flush", "different_device", "closed_root"} {
		t.Run(mutation, func(t *testing.T) {
			path := t.TempDir()
			manifest := filepath.Join(path, ManifestName)
			if err := os.WriteFile(manifest, []byte("synthetic"), 0o600); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { root.Close() })
			info, err := root.Stat(".")
			if err != nil {
				t.Fatal(err)
			}
			target := &archiveRestoreTarget{root: root, stagingInfo: info, deferFileParentSync: true}
			old := streamedRestoreFullsync
			t.Cleanup(func() { streamedRestoreFullsync = old })
			switch mutation {
			case "symlink":
				if err := os.Rename(manifest, manifest+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(ManifestName+".original", manifest); err != nil {
					t.Fatal(err)
				}
			case "replacement_during_flush":
				streamedRestoreFullsync = func(fd int) error {
					replacement := manifest + ".replacement"
					if err := os.WriteFile(replacement, []byte("synthetic"), 0o600); err != nil {
						return err
					}
					if err := os.Rename(replacement, manifest); err != nil {
						return err
					}
					return old(fd)
				}
			case "different_device":
				target.stagingInfo = streamedDeviceInfo{FileInfo: info, dev: info.Sys().(*syscall.Stat_t).Dev + 1}
			case "closed_root":
				if err := root.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if err := syncStreamedRestoreBarrier(target); err == nil {
				t.Fatal("barrier accepted changed identity/device")
			}
		})
	}
}

func TestStreamedRestoreDarwinFinalVerificationChecksDevice(t *testing.T) {
	path := t.TempDir()
	if err := os.WriteFile(filepath.Join(path, "data"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	info, err := root.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	foreign := streamedDeviceInfo{FileInfo: info, dev: info.Sys().(*syscall.Stat_t).Dev + 1}
	target := &archiveRestoreTarget{root: root, stagingInfo: foreign, deferFileParentSync: true}
	err = verifyStreamedStagedFile(target, ArchiveEntry{Path: "data"})
	if err == nil || !strings.Contains(err.Error(), "device") {
		t.Fatalf("final verification lost device check: %v", err)
	}
	if _, err := target.createFile("new", 0o600); err == nil || !strings.Contains(err.Error(), "device") {
		t.Fatalf("create lost device check: %v", err)
	}
}

func TestStreamedRestoreDarwinRealFilesystemDurability(t *testing.T) {
	path := t.TempDir()
	dir, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	file, err := os.Create(filepath.Join(path, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("synthetic durable fixture"); err != nil {
		t.Fatal(err)
	}
	if err := syncStreamedRestoreFile(file); err != nil {
		t.Fatalf("real file fsync: %v", err)
	}
	if err := streamedRestoreSync(dir, unix.Fsync); err != nil {
		t.Fatalf("real directory fsync: %v", err)
	}
	dirFullsyncErr := streamedRestoreSync(dir, streamedRestoreFullsync)
	t.Logf("real directory F_FULLFSYNC: %v (not used as the sole publication guarantee)", dirFullsyncErr)
	if err := streamedRestoreSync(file, streamedRestoreFullsync); err != nil {
		t.Fatalf("real regular-file F_FULLFSYNC: %v", err)
	}
}

func TestStreamedRestoreDarwinStrictDirectorySyncFailure(t *testing.T) {
	archive := streamedDurabilityArchive(t)
	for _, injected := range []error{syscall.EIO, syscall.EINVAL} {
		t.Run(injected.Error(), func(t *testing.T) {
			previous := streamedRestoreFsync
			t.Cleanup(func() { streamedRestoreFsync = previous })
			directories := 0
			streamedRestoreFsync = func(fd int) error {
				var info unix.Stat_t
				if err := unix.Fstat(fd, &info); err != nil {
					return err
				}
				if info.Mode&unix.S_IFMT == unix.S_IFDIR {
					directories++
					return injected
				}
				return previous(fd)
			}
			target := filepath.Join(t.TempDir(), "restored")
			_, err := RestoreArchive(archive, target, archiveTestNow)
			if !errors.Is(err, injected) || directories != 1 {
				t.Fatalf("directory sync error ignored: err=%v calls=%d", err, directories)
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatalf("failed directory sync published target: %v", err)
			}
		})
	}
}

func TestStreamedRestoreDarwinStrictDirectoriesBeforeBarrier(t *testing.T) {
	archive := streamedDurabilityArchive(t)
	previousSync, previousBarrier := streamedRestoreFsync, restoreStreamedDeviceBarrier
	t.Cleanup(func() { streamedRestoreFsync, restoreStreamedDeviceBarrier = previousSync, previousBarrier })
	synced := map[string]bool{}
	streamedRestoreFsync = func(fd int) error {
		var info unix.Stat_t
		if err := unix.Fstat(fd, &info); err != nil {
			return err
		}
		if err := previousSync(fd); err != nil {
			return err
		}
		if info.Mode&unix.S_IFMT == unix.S_IFDIR {
			synced[fmt.Sprintf("%d:%d", info.Dev, info.Ino)] = true
		}
		return nil
	}
	calls := 0
	restoreStreamedDeviceBarrier = func(target *archiveRestoreTarget) error {
		calls++
		if calls == 1 {
			directories := 0
			if err := fs.WalkDir(target.root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if !entry.IsDir() {
					return nil
				}
				info, err := target.root.Stat(path)
				if err != nil {
					return err
				}
				stat := info.Sys().(*syscall.Stat_t)
				if !synced[fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)] {
					return fmt.Errorf("directory %s not strictly synced before barrier", path)
				}
				directories++
				return nil
			}); err != nil {
				return err
			}
			if directories == 0 {
				return errors.New("no staged directories were checked")
			}
		}
		return previousBarrier(target)
	}
	if _, err := RestoreArchive(archive, filepath.Join(t.TempDir(), "restored"), archiveTestNow); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("barrier calls=%d", calls)
	}
}
