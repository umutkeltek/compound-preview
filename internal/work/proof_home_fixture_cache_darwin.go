package work

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

var homeFixtureSharedParentStat = os.Lstat

// A harness can atomically replace files directly under HOME. Keep the
// accelerator outside that observable ancestor without dropping any seal input.
func defaultHomeFixtureCacheBase() (string, error) {
	parent := filepath.Join(string(filepath.Separator), "Users", "Shared")
	info, err := homeFixtureSharedParentStat(parent)
	if err != nil {
		return "", err
	}
	if !homeFixtureSharedParentSafe(info) {
		return "", fmt.Errorf("home fixture shared parent is not a safe root-owned directory")
	}
	return filepath.Join(parent, ".compound-proof-cache-"+strconv.Itoa(os.Getuid())), nil
}

func homeFixtureSharedParentSafe(info os.FileInfo) bool {
	if info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0 && (info.Mode().Perm()&0022 == 0 || info.Mode()&os.ModeSticky != 0)
}
