//go:build unix

package home

import (
	"os"

	"golang.org/x/sys/unix"
)

func openManifestNoFollow(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	success := false
	defer func() {
		if !success {
			_ = f.Close()
		}
	}()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := manifestOpenAfterStatHook(path); err != nil {
		return nil, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := validateManifestIdentity(path, before, opened, after); err != nil {
		return nil, err
	}
	success = true
	return f, nil
}
