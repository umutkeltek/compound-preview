//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd

package browser

import (
	"errors"
	"os"
)

func lockSelfHealFile(string, bool) (*os.File, error) {
	return nil, errors.New("browser self-heal needs an OS-supported file lock")
}

func unlockSelfHealFile(*os.File) {}
