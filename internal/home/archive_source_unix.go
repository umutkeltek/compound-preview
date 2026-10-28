//go:build unix

package home

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openArchiveNoFollow(path string, maxBytes int64) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, fmt.Errorf("archive source must be a non-symlink regular file: %s", path)
	}
	if before.Size() > maxBytes {
		return nil, fmt.Errorf("archive source exceeds %d bytes: %s", maxBytes, path)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	success := false
	defer func() {
		if !success {
			_ = file.Close()
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if after.Mode()&os.ModeSymlink != 0 || !opened.Mode().IsRegular() || !after.Mode().IsRegular() || !os.SameFile(before, opened) || !os.SameFile(before, after) {
		return nil, fmt.Errorf("archive source identity changed while opening: %s", path)
	}
	if opened.Size() > maxBytes {
		return nil, fmt.Errorf("archive source exceeds %d bytes: %s", maxBytes, path)
	}
	success = true
	return file, nil
}
