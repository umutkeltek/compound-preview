//go:build !darwin && !linux

package home

import (
	"fmt"
	"os"
	"runtime"
)

func restoreRenameNoReplace(*os.File, string, string) error {
	return fmt.Errorf("atomic no-replace restore publication is unsupported on %s", runtime.GOOS)
}
