package gitrepository

import "golang.org/x/sys/unix"

func renameIndexLockExclusive(from int, source string, to int, destination string) error {
	return unix.RenameatxNp(from, source, to, destination, unix.RENAME_EXCL)
}
