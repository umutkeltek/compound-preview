//go:build !darwin && !linux

package home

import (
	"fmt"
	"runtime"
)

func exchangeRootedCASAt(_ int, _, _ string) error {
	return fmt.Errorf("atomic rooted file exchange is unsupported on %s", runtime.GOOS)
}
