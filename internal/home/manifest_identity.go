package home

import (
	"fmt"
	"os"
)

var manifestOpenAfterStatHook = func(string) error { return nil }

func validateManifestIdentity(path string, before, opened, after os.FileInfo) error {
	if before == nil || opened == nil || after == nil {
		return fmt.Errorf("manifest %s identity cannot be established", path)
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("manifest %s is a symlink", path)
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("manifest %s is not a regular file", path)
	}
	if after.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("manifest %s is a symlink", path)
	}
	if !after.Mode().IsRegular() {
		return fmt.Errorf("manifest %s is not a regular file", path)
	}
	if !opened.Mode().IsRegular() {
		return fmt.Errorf("manifest %s is not a regular file", path)
	}
	if !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		return fmt.Errorf("manifest %s identity changed while opening", path)
	}
	return nil
}
