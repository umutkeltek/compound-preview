//go:build darwin

package home

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func requireArchiveEventSnapshotSupport() error { return nil }

// The source fd identifies the owned live file; clonefile pins immutable extents.
// Only this backup's private temporary clone is removed.
func writeRootedArchiveEventSnapshot(tw archiveEntryWriter, root *os.Root, rel string, before fs.FileInfo) (entry ArchiveEntry, resultErr error) {
	defer func() {
		if resultErr != nil {
			entry = ArchiveEntry{}
			resultErr = fmt.Errorf("archive event snapshot %q: %w", rel, resultErr)
		}
	}()
	if !before.Mode().IsRegular() {
		return ArchiveEntry{}, fmt.Errorf("source must be regular")
	}
	source, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		return ArchiveEntry{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, source.Close()) }()
	opened, err := source.Stat()
	if err != nil {
		return ArchiveEntry{}, err
	}
	current, err := root.Lstat(filepath.FromSlash(rel))
	if err != nil {
		return ArchiveEntry{}, err
	}
	if !opened.Mode().IsRegular() || current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(before, opened) || !os.SameFile(before, current) {
		return ArchiveEntry{}, fmt.Errorf("source identity changed before snapshot")
	}
	if err := root.Mkdir("tmp", 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return ArchiveEntry{}, err
	}
	tmpBefore, err := root.Lstat("tmp")
	if err != nil {
		return ArchiveEntry{}, err
	}
	if !tmpBefore.IsDir() || tmpBefore.Mode()&os.ModeSymlink != 0 {
		return ArchiveEntry{}, fmt.Errorf("owned temporary root must be a regular directory")
	}
	tmpRoot, err := root.OpenRoot("tmp")
	if err != nil {
		return ArchiveEntry{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, tmpRoot.Close()) }()
	tmpDir, err := tmpRoot.Open(".")
	if err != nil {
		return ArchiveEntry{}, err
	}
	tmpOpened, statErr := tmpDir.Stat()
	closeErr := tmpDir.Close()
	if err := errors.Join(statErr, closeErr); err != nil {
		return ArchiveEntry{}, err
	}
	if !os.SameFile(tmpBefore, tmpOpened) {
		return ArchiveEntry{}, fmt.Errorf("temporary root identity changed")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ArchiveEntry{}, err
	}
	stageName := "home-event-snapshot-" + hex.EncodeToString(nonce[:])
	if err := tmpRoot.Mkdir(stageName, 0o700); err != nil {
		return ArchiveEntry{}, err
	}
	stageBefore, err := tmpRoot.Lstat(stageName)
	if err != nil {
		return ArchiveEntry{}, err
	}
	stage, err := tmpRoot.OpenRoot(stageName)
	if err != nil {
		return ArchiveEntry{}, err
	}
	defer func() {
		removeErr := stage.Remove("body")
		if errors.Is(removeErr, fs.ErrNotExist) {
			removeErr = nil
		}
		resultErr = errors.Join(resultErr, removeErr, stage.Close())
		after, err := tmpRoot.Lstat(stageName)
		if err != nil || !os.SameFile(stageBefore, after) {
			resultErr = errors.Join(resultErr, fmt.Errorf("temporary snapshot directory identity changed"), err)
			return
		}
		resultErr = errors.Join(resultErr, tmpRoot.Remove(stageName))
	}()
	dir, err := stage.Open(".")
	if err != nil {
		return ArchiveEntry{}, err
	}
	cloneErr := unix.Fclonefileat(int(source.Fd()), int(dir.Fd()), "body", 0)
	if err := errors.Join(cloneErr, dir.Close()); err != nil {
		return ArchiveEntry{}, fmt.Errorf("immutable native clone unavailable: %w", err)
	}
	snapshotMetadata, err := stage.Lstat("body")
	if err != nil {
		return ArchiveEntry{}, err
	}
	if err := stage.Chmod("body", 0o600); err != nil {
		return ArchiveEntry{}, err
	}
	snapshotInfo, err := stage.Lstat("body")
	if err != nil {
		return ArchiveEntry{}, err
	}
	entry, err = writeRootedArchiveFileAt(tw, stage, "body", rel, snapshotInfo, snapshotMetadata)
	if err != nil {
		return ArchiveEntry{}, err
	}
	after, err := root.Lstat(filepath.FromSlash(rel))
	if err != nil {
		return ArchiveEntry{}, err
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return ArchiveEntry{}, fmt.Errorf("live source identity changed during snapshot export")
	}
	return entry, nil
}
