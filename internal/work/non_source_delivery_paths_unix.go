//go:build unix

package work

import (
	"os"
	"syscall"
)

func privateDeliverySingleLink(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(stat.Nlink) == 1
}
