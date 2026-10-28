//go:build unix

package home

import (
	"os"
	"syscall"
)

func fileOwnerUID(info os.FileInfo) (uint32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

func ownedByCurrent(info os.FileInfo) bool {
	uid, ok := fileOwnerUID(info)
	return ok && uid == uint32(os.Getuid())
}

func singleLinkPrivateObject(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(st.Nlink) == 1
}

func privateObjectLinkCount(info os.FileInfo) (uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Nlink), true
}
