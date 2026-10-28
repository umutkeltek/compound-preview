//go:build linux

package home

import (
	"os"

	"golang.org/x/sys/unix"
)

func restoreRenameNoReplace(parent *os.File, from, to string) error {
	fd := int(parent.Fd())
	return unix.Renameat2(fd, from, fd, to, unix.RENAME_NOREPLACE)
}
