//go:build unix

package home

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestVerifyArchiveRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.pipe")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyArchive(path); err == nil || !strings.Contains(err.Error(), "non-symlink regular file") {
		t.Fatalf("verify FIFO err = %v", err)
	}
}

func TestVerifyArchiveRejectsSymlinkSource(t *testing.T) {
	archivePath := backupArchiveFixture(t)
	link := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := os.Symlink(archivePath, link); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyArchive(link); err == nil || !strings.Contains(err.Error(), "non-symlink regular file") {
		t.Fatalf("verify symlink err = %v", err)
	}
}
