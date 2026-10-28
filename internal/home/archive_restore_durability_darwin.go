//go:build darwin

package home

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

var (
	streamedRestoreFsync    = unix.Fsync
	streamedRestoreFullsync = func(fd int) error {
		_, err := unix.FcntlInt(uintptr(fd), unix.F_FULLFSYNC, 0)
		return err
	}
	streamedRestoreClose   = func(file *os.File) error { return file.Close() }
	streamedRestoreControl = func(file *os.File, call func(uintptr)) error {
		raw, err := file.SyscallConn()
		if err != nil {
			return err
		}
		return raw.Control(call)
	}
)

func streamedRestoreSync(file *os.File, operation func(int) error) error {
	var syncErr error
	controlErr := streamedRestoreControl(file, func(fd uintptr) {
		for {
			syncErr = operation(int(fd))
			if !errors.Is(syncErr, syscall.EINTR) {
				break
			}
		}
	})
	return errors.Join(controlErr, syncErr)
}

func syncStreamedRestoreFile(file *os.File) error {
	// fsync sends this file's data/attributes to the device. The checked
	// F_FULLFSYNC publication barrier below persists the entire device queue.
	return streamedRestoreSync(file, streamedRestoreFsync)
}

func requireStreamedRestoreDevice(root, entry os.FileInfo) error {
	if root == nil || entry == nil {
		return fmt.Errorf("streamed restore device identity missing")
	}
	rootStat, rootOK := root.Sys().(*syscall.Stat_t)
	entryStat, entryOK := entry.Sys().(*syscall.Stat_t)
	if !rootOK || !entryOK || rootStat.Dev != entryStat.Dev {
		return fmt.Errorf("streamed restore entries must remain on the staging device")
	}
	return nil
}

func syncStreamedRestoreDirectory(root *os.Root, name string, rootInfo, expected os.FileInfo) (resultErr error) {
	if err := requireStreamedRestoreDevice(rootInfo, expected); err != nil {
		return err
	}
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !opened.IsDir() || !os.SameFile(expected, opened) {
		return fmt.Errorf("streamed restore directory identity changed: %s", name)
	}
	// Do not inherit the generic directory helper's EINVAL allowance: the
	// device barrier can only promise durability for checked fsync'd metadata.
	if err := streamedRestoreSync(file, streamedRestoreFsync); err != nil {
		return err
	}
	after, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !after.IsDir() || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) {
		return fmt.Errorf("streamed restore directory identity changed: %s", name)
	}
	return nil
}

func syncStreamedRestoreParent(file *os.File, stagingInfo os.FileInfo) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("streamed restore publication parent must be a directory")
	}
	if err := requireStreamedRestoreDevice(stagingInfo, info); err != nil {
		return err
	}
	return streamedRestoreSync(file, streamedRestoreFsync)
}

func syncStreamedRestoreBarrier(target *archiveRestoreTarget) (resultErr error) {
	before, err := target.root.Lstat(ManifestName)
	if err != nil {
		return err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return fmt.Errorf("streamed restore barrier requires a regular manifest")
	}
	if err := requireStreamedRestoreDevice(target.stagingInfo, before); err != nil {
		return err
	}
	file, err := target.root.Open(ManifestName)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, streamedRestoreClose(file)) }()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return fmt.Errorf("streamed restore barrier manifest identity changed")
	}
	// Darwin fcntl(2): F_FULLFSYNC drains the device queue, persisting all
	// previously fsync'd data on this same device. File.Sync is unsuitable
	// here because it silently falls back to fsync on ENOTSUP.
	if err := streamedRestoreSync(file, streamedRestoreFullsync); err != nil {
		return fmt.Errorf("streamed restore full-device flush: %w", err)
	}
	after, err := target.root.Lstat(ManifestName)
	if err != nil {
		return err
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		return fmt.Errorf("streamed restore barrier manifest identity changed")
	}
	return nil
}
