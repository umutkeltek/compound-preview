//go:build unix

package home

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestInitCreatesExactPrivateRootUnderRestrictiveUmask(t *testing.T) {
	parent := t.TempDir()
	homePath := filepath.Join(parent, ".compound")
	old := unix.Umask(0o777)
	t.Cleanup(func() { unix.Umask(old) })
	if err := Init(homePath, "global", time.Now()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("fresh home root mode under restrictive umask = %04o, want 0700", info.Mode().Perm())
	}
	for _, rel := range privateCustodyKnownDirs {
		info, err := os.Lstat(filepath.Join(homePath, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("stat %s: %v", rel, err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("private dir %s mode under restrictive umask = %04o, want 0700", rel, info.Mode().Perm())
		}
	}
}
