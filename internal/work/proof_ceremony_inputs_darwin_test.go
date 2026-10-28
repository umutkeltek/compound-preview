package work

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// APFS clones retain source birth/access times. A temporary-directory cleaner
// can therefore consider a new snapshot old while a long proof still uses it.
func TestCopyProofCeremonyInputsRefreshesClonedAccessTimes(t *testing.T) {
	source, sealed := t.TempDir(), t.TempDir()
	old := time.Now().Add(-7 * 24 * time.Hour).Truncate(time.Second)
	paths := []string{
		"docs_private/leak-tokens.txt",
		"docs_private/profiles/overlay.json",
		"console/node_modules/typescript/bin/tsc",
	}
	for _, relative := range paths {
		path := filepath.Join(source, filepath.FromSlash(relative))
		writeCeremonyInput(t, path, "synthetic proof input\n")
		if err := os.Chmod(path, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	linkRelative := "console/node_modules/.bin/tsc"
	link := filepath.Join(source, filepath.FromSlash(linkRelative))
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../typescript/bin/tsc", link); err != nil {
		t.Fatal(err)
	}
	started := time.Now().Truncate(time.Second)
	if err := CopyProofCeremonyInputs(context.Background(), source, sealed, exec.CommandContext); err != nil {
		t.Fatal(err)
	}
	for _, relative := range paths {
		t.Run(relative, func(t *testing.T) {
			from := filepath.Join(source, filepath.FromSlash(relative))
			to := filepath.Join(sealed, filepath.FromSlash(relative))
			// Inspect before reading bytes: a read could itself refresh atime.
			for _, path := range []string{from, to} {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				stat := info.Sys().(*syscall.Stat_t)
				access := time.Unix(stat.Atimespec.Sec, stat.Atimespec.Nsec)
				if path == to && access.Before(started) {
					t.Errorf("copied access time = %v, want at least %v", access, started)
				}
				if path == from && !access.Equal(old) {
					t.Errorf("source access time changed: got %v, want %v", access, old)
				}
				if !info.ModTime().Equal(old) || info.Mode().Perm() != 0o750 {
					t.Errorf("metadata changed: mtime %v, mode %v", info.ModTime(), info.Mode())
				}
			}
			for _, path := range []string{from, to} {
				body, err := os.ReadFile(path)
				if err != nil || string(body) != "synthetic proof input\n" {
					t.Fatalf("input bytes changed: %q, %v", body, err)
				}
			}
		})
	}
	for _, root := range []string{source, sealed} {
		target, err := os.Readlink(filepath.Join(root, filepath.FromSlash(linkRelative)))
		if err != nil || target != "../typescript/bin/tsc" {
			t.Fatalf("rooted dependency symlink changed: %q, %v", target, err)
		}
	}
}
