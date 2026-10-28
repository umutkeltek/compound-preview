//go:build !darwin && !linux

package gitrepository

import "fmt"

func renameIndexLockExclusive(from int, source string, to int, destination string) error {
	return fmt.Errorf("exclusive index-lock rename unsupported on this platform")
}
