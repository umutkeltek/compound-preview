//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd

package browser

import (
	"errors"
	"os"
)

func platformLockCredential(string) (*os.File, error) {
	return nil, errors.New("credential mutation needs an OS-supported private file lock")
}

func platformUnlockCredential(*os.File) {}
