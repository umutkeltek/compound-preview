//go:build darwin

package home

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCaptureParallelMetadataBoundAndParentOrder(t *testing.T) {
	rootPath := t.TempDir()
	for i := 0; i < 32; i++ {
		dir := filepath.Join(rootPath, fmt.Sprintf("d%02d", i), "nested")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "body"), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	original := captureReadBulk
	defer func() { captureReadBulk = original }()
	var active, maximum atomic.Int32
	captureReadBulk = func(dir *os.File, buf []byte) (int, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); old < n && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		time.Sleep(time.Millisecond)
		return original(dir, buf)
	}
	seen := map[string]bool{".": true}
	err = walkCaptureRootConcurrent(root, ".", func(path string, info fs.FileInfo) error {
		if seen[path] {
			return fmt.Errorf("duplicate path %s", path)
		}
		if !seen[filepath.ToSlash(filepath.Dir(path))] {
			return fmt.Errorf("child before parent: %s", path)
		}
		seen[path] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 97 {
		t.Fatalf("incomplete walk: %d", len(seen))
	}
	if maximum.Load() < 2 || maximum.Load() > captureDirectoryWorkers {
		t.Fatalf("metadata concurrency = %d", maximum.Load())
	}
	if active.Load() != 0 {
		t.Fatal("walker returned with active readers")
	}
}

func TestCaptureParallelFailureJoinsReadersAndHonorsSkip(t *testing.T) {
	rootPath := t.TempDir()
	for i := 0; i < 12; i++ {
		p := filepath.Join(rootPath, fmt.Sprintf("d%02d", i))
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "body"), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	original := captureReadBulk
	defer func() { captureReadBulk = original }()
	var active atomic.Int32
	captureReadBulk = func(dir *os.File, buf []byte) (int, error) {
		active.Add(1)
		defer active.Add(-1)
		return original(dir, buf)
	}
	sentinel := errors.New("visit refused")
	err = walkCaptureRootConcurrent(root, ".", func(path string, info fs.FileInfo) error {
		if path == "d00" {
			return fs.SkipDir
		}
		if strings.HasPrefix(path, "d00/") {
			t.Error("walked skipped directory")
		}
		if !info.IsDir() {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("lost error: %v", err)
	}
	if active.Load() != 0 {
		t.Fatal("reader survived return")
	}
}

func TestCaptureParallelRejectsDelayedDirectoryReplacement(t *testing.T) {
	rootPath := t.TempDir()
	path := filepath.Join(rootPath, "child")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	expected, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, filepath.Join(rootPath, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	w := &captureBulkTraversal{root: root, visit: func(string, fs.FileInfo) error { return nil }, slots: make(chan struct{}, 3), stopped: make(chan struct{})}
	if err := w.walk("child", expected); err == nil {
		t.Fatal("delayed subtree accepted replacement identity")
	}
}

func TestCaptureParallelChecksAncestorAfterSubtree(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "parent", "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "parent", "child", "body"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	original := captureReadBulk
	defer func() { captureReadBulk = original }()
	var replaced atomic.Bool
	captureReadBulk = func(dir *os.File, buf []byte) (int, error) {
		n, err := original(dir, buf)
		if filepath.Base(dir.Name()) == "child" && replaced.CompareAndSwap(false, true) {
			if e := os.Rename(filepath.Join(rootPath, "parent"), filepath.Join(rootPath, "moved")); e != nil {
				return 0, e
			}
			if e := os.Mkdir(filepath.Join(rootPath, "parent"), 0700); e != nil {
				return 0, e
			}
		}
		return n, err
	}
	err = walkCaptureRootConcurrent(root, ".", func(string, fs.FileInfo) error { return nil })
	if !replaced.Load() {
		t.Fatal("ancestor replacement control did not run")
	}
	if err == nil {
		t.Fatal("ancestor replacement during child traversal was accepted")
	}
}
