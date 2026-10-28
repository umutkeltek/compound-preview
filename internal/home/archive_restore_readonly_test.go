package home

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestArchiveRestoreReadOnlyDirectories(t *testing.T) {
	for _, format := range []string{"v1", ArchiveFormatStreamedV2, ArchiveFormatStreamedV3} {
		t.Run(format, func(t *testing.T) {
			base := t.TempDir()
			source := filepath.Join(base, "source")
			target := filepath.Join(base, "restored")
			now := time.Now().UTC()
			if err := Init(source, "test", now); err != nil {
				t.Fatal(err)
			}
			parent := filepath.Join("private", "readonly-fixture")
			child := filepath.Join(parent, "nested")
			body := filepath.Join(child, "body.txt")
			flatBody := filepath.Join(parent, "body.txt")
			// Only synthetic fixture trees are made writable for TempDir cleanup.
			t.Cleanup(func() {
				for _, root := range []string{source, target} {
					for _, rel := range []string{parent, child} {
						if err := os.Chmod(filepath.Join(root, rel), 0o700); err != nil && !os.IsNotExist(err) {
							t.Errorf("fixture cleanup permissions: %v", err)
						}
					}
				}
			})
			if err := os.MkdirAll(filepath.Join(source, child), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, rel := range []string{flatBody, body} {
				if err := os.WriteFile(filepath.Join(source, rel), []byte("readonly payload"), 0o555); err != nil {
					t.Fatal(err)
				}
			}
			for rel, mode := range map[string]os.FileMode{parent: 0o555, child: 0o500} {
				if err := os.Chmod(filepath.Join(source, rel), mode); err != nil {
					t.Fatal(err)
				}
			}
			archive := filepath.Join(base, "backup.tar.gz")
			if _, err := BackupWithFormat(source, archive, format, now); err != nil {
				t.Fatal(err)
			}
			offline := filepath.Join(base, "source-offline")
			if err := os.Rename(source, offline); err != nil {
				t.Fatal(err)
			}
			source = offline
			if _, err := RestoreArchive(archive, target, now); err != nil {
				t.Fatalf("restore readonly directory children: %v", err)
			}
			for _, rel := range []string{flatBody, body} {
				got, err := os.ReadFile(filepath.Join(target, rel))
				if err != nil || string(got) != "readonly payload" {
					t.Fatalf("restored %s = %q, error = %v", rel, got, err)
				}
			}
			for rel, want := range map[string]os.FileMode{parent: 0o555, child: 0o500, flatBody: 0o555, body: 0o555} {
				info, err := os.Lstat(filepath.Join(target, rel))
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != want {
					t.Errorf("restored %s mode = %v; want %04o without a symlink", rel, info.Mode(), want)
				}
			}
		})
	}
}
