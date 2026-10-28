package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"compound/internal/home"
)

type initializedTestHomeTemplate struct {
	once sync.Once
	path string
	err  error
}

func (c *initializedTestHomeTemplate) ensure(root string, initialize func(string) error) (string, error) {
	c.once.Do(func() {
		if strings.TrimSpace(root) == "" {
			c.err = errors.New("test home template root required")
			return
		}
		if initialize == nil {
			c.err = errors.New("test home template initializer required")
			return
		}
		c.path = filepath.Join(root, "initialized-home-template")
		c.err = initialize(c.path)
	})
	if c.err != nil {
		return "", c.err
	}
	return c.path, nil
}

var (
	cachedInitializedTestHome initializedTestHomeTemplate
	testHomeTemplateInitCount atomic.Int32
)

func initializedTestHome(t *testing.T) string {
	t.Helper()
	template, err := cachedInitializedTestHome.ensure(testBinDir, func(path string) error {
		testHomeTemplateInitCount.Add(1)
		return home.Init(path, "test", cliTestNow)
	})
	if err != nil {
		t.Fatalf("initialize cached test home: %v", err)
	}
	destination := t.TempDir()
	if err := cloneTestHome(template, destination); err != nil {
		t.Fatalf("clone initialized test home: %v", err)
	}
	return destination
}

func cloneTestHome(source, destination string) error {
	sourceInfo, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("inspect test home template: %w", err)
	}
	if sourceInfo.Mode()&os.ModeSymlink != 0 || !sourceInfo.IsDir() {
		return fmt.Errorf("test home template must be a non-symlink directory: %s", source)
	}
	destinationInfo, err := os.Lstat(destination)
	if err != nil {
		return fmt.Errorf("inspect test home clone destination: %w", err)
	}
	if destinationInfo.Mode()&os.ModeSymlink != 0 || !destinationInfo.IsDir() {
		return fmt.Errorf("test home clone destination must be a non-symlink directory: %s", destination)
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		return fmt.Errorf("read test home clone destination: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("test home clone destination must be empty: %s", destination)
	}
	if err := cloneTestHomeContents(source, destination); err != nil {
		return err
	}
	if err := os.Chmod(destination, sourceInfo.Mode().Perm()); err != nil {
		return fmt.Errorf("apply test home root mode: %w", err)
	}
	return nil
}

func cloneTestHomeContents(source, destination string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return fmt.Errorf("read test home template: %w", err)
	}
	for _, entry := range entries {
		sourcePath := filepath.Join(source, entry.Name())
		destinationPath := filepath.Join(destination, entry.Name())
		info, err := os.Lstat(sourcePath)
		if err != nil {
			return fmt.Errorf("inspect test home template entry %s: %w", sourcePath, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("test home template entry must not be a symlink: %s", sourcePath)
		}
		switch {
		case info.IsDir():
			if err := os.Mkdir(destinationPath, 0o700); err != nil {
				return fmt.Errorf("create test home clone directory %s: %w", destinationPath, err)
			}
			if err := cloneTestHomeContents(sourcePath, destinationPath); err != nil {
				return err
			}
			if err := os.Chmod(destinationPath, info.Mode().Perm()); err != nil {
				return fmt.Errorf("apply test home clone directory mode %s: %w", destinationPath, err)
			}
		case info.Mode().IsRegular():
			data, err := os.ReadFile(sourcePath)
			if err != nil {
				return fmt.Errorf("read test home template file %s: %w", sourcePath, err)
			}
			if err := os.WriteFile(destinationPath, data, 0o600); err != nil {
				return fmt.Errorf("write test home clone file %s: %w", destinationPath, err)
			}
			if err := os.Chmod(destinationPath, info.Mode().Perm()); err != nil {
				return fmt.Errorf("apply test home clone file mode %s: %w", destinationPath, err)
			}
		default:
			return fmt.Errorf("test home template entry must be regular: %s (%s)", sourcePath, info.Mode())
		}
	}
	return nil
}

func TestInitializedTestHomeCloneIsolationAndModes(t *testing.T) {
	first := initializedTestHome(t)
	second := initializedTestHome(t)
	if first == second {
		t.Fatalf("initialized home clones share a root: %s", first)
	}
	template := cachedInitializedTestHome.path
	for _, relative := range []string{home.ManifestName, filepath.Join("ledger")} {
		templateInfo, err := os.Stat(filepath.Join(template, relative))
		if err != nil {
			t.Fatal(err)
		}
		for _, clone := range []string{first, second} {
			cloneInfo, err := os.Stat(filepath.Join(clone, relative))
			if err != nil {
				t.Fatal(err)
			}
			if cloneInfo.Mode().Perm() != templateInfo.Mode().Perm() {
				t.Fatalf("%s mode = %o, want %o", relative, cloneInfo.Mode().Perm(), templateInfo.Mode().Perm())
			}
		}
	}
	templateManifest, err := os.ReadFile(filepath.Join(template, home.ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	for _, clone := range []string{first, second} {
		manifest, err := os.ReadFile(filepath.Join(clone, home.ManifestName))
		if err != nil {
			t.Fatal(err)
		}
		if string(manifest) != string(templateManifest) {
			t.Fatalf("clone manifest differs from template")
		}
	}
	marker := filepath.Join(first, "ledger", "clone-only")
	if err := os.WriteFile(marker, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, untouched := range []string{second, template} {
		if _, err := os.Lstat(filepath.Join(untouched, "ledger", "clone-only")); !os.IsNotExist(err) {
			t.Fatalf("clone mutation reached %s: %v", untouched, err)
		}
	}
	if got := testHomeTemplateInitCount.Load(); got != 1 {
		t.Fatalf("template initialized %d times, want 1", got)
	}
}

func TestInitializedTestHomeTemplateConcurrentAndFailureSticky(t *testing.T) {
	var cache initializedTestHomeTemplate
	var calls atomic.Int32
	root := t.TempDir()
	const workers = 8
	paths := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path, err := cache.ensure(root, func(path string) error {
				calls.Add(1)
				return home.Init(path, "test", cliTestNow)
			})
			paths <- path
			errs <- err
		}()
	}
	wg.Wait()
	close(paths)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent template init: %v", err)
		}
	}
	var wantPath string
	for path := range paths {
		if wantPath == "" {
			wantPath = path
		}
		if path != wantPath {
			t.Fatalf("template paths differ: got %q want %q", path, wantPath)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent template initializer called %d times, want 1", got)
	}

	sentinel := errors.New("template init failed")
	var failed initializedTestHomeTemplate
	var failedCalls atomic.Int32
	for range 2 {
		path, err := failed.ensure(t.TempDir(), func(path string) error {
			failedCalls.Add(1)
			if err := os.MkdirAll(path, 0o700); err != nil {
				return err
			}
			return sentinel
		})
		if path != "" || !errors.Is(err, sentinel) {
			t.Fatalf("failed template returned path=%q err=%v", path, err)
		}
	}
	if got := failedCalls.Load(); got != 1 {
		t.Fatalf("failed template initializer called %d times, want 1", got)
	}
}

func TestCloneTestHomeRejectsSymlink(t *testing.T) {
	source := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink("missing", filepath.Join(source, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := cloneTestHome(source, target); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("clone symlink error = %v", err)
	}
}
