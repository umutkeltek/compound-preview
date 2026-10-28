package release

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

var errInstallerFixtureTemplateBuildAborted = errors.New("installer fixture template builder stopped before returning")

type installerFixtureTemplate struct {
	once        sync.Once
	cleanupOnce sync.Once
	builder     func(string) error
	rootDir     string
	templateDir string
	initErr     error
	cleanupErr  error
}

func newInstallerFixtureTemplate(builder func(string) error) *installerFixtureTemplate {
	return &installerFixtureTemplate{builder: builder}
}

func (template *installerFixtureTemplate) cloneTo(target string) error {
	return template.cloneToWithBuilder(target, template.builder)
}

func (template *installerFixtureTemplate) cloneToWithBuilder(target string, builder func(string) error) error {
	template.once.Do(func() {
		completed := false
		defer func() {
			if !completed && template.initErr == nil {
				template.initErr = errInstallerFixtureTemplateBuildAborted
			}
		}()

		root, err := os.MkdirTemp("", "compound-installer-fixture-template-")
		if err != nil {
			template.initErr = fmt.Errorf("create installer fixture template root: %w", err)
			completed = true
			return
		}
		template.rootDir = root
		template.templateDir = filepath.Join(root, "dist")
		if builder == nil {
			template.initErr = errors.New("installer fixture template builder is nil")
			completed = true
			return
		}
		if err := builder(template.templateDir); err != nil {
			template.initErr = fmt.Errorf("build installer fixture template: %w", err)
			completed = true
			return
		}
		if err := validateInstallerFixtureTemplate(template.templateDir); err != nil {
			template.initErr = err
			completed = true
			return
		}
		completed = true
	})
	if template.initErr != nil {
		return template.initErr
	}
	return copyInstallerFixtureTemplate(template.templateDir, target)
}

func (template *installerFixtureTemplate) close() error {
	template.cleanupOnce.Do(func() {
		if template.rootDir != "" {
			template.cleanupErr = os.RemoveAll(template.rootDir)
		}
	})
	return template.cleanupErr
}

func validateInstallerFixtureTemplate(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read installer fixture template: %w", err)
	}
	if len(entries) == 0 {
		return errors.New("installer fixture template is empty")
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect installer fixture template entry %s: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("installer fixture template entry %s is not a regular file", entry.Name())
		}
	}
	return nil
}

func copyInstallerFixtureTemplate(source, target string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return fmt.Errorf("read installer fixture template: %w", err)
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		return fmt.Errorf("create installer fixture clone: %w", err)
	}
	for _, entry := range entries {
		sourcePath := filepath.Join(source, entry.Name())
		info, err := os.Lstat(sourcePath)
		if err != nil {
			return fmt.Errorf("inspect installer fixture template entry %s: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("installer fixture template entry %s is not a regular file", entry.Name())
		}
		if err := copyInstallerFixtureRegularFile(sourcePath, filepath.Join(target, entry.Name()), info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

func copyInstallerFixtureRegularFile(source, target string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open installer fixture template file %s: %w", filepath.Base(source), err)
	}
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = input.Close()
		return fmt.Errorf("create installer fixture clone file %s: %w", filepath.Base(target), err)
	}
	_, copyErr := io.Copy(output, input)
	inputCloseErr := input.Close()
	outputCloseErr := output.Close()
	if copyErr != nil {
		return fmt.Errorf("copy installer fixture file %s: %w", filepath.Base(source), copyErr)
	}
	if inputCloseErr != nil {
		return fmt.Errorf("close installer fixture template file %s: %w", filepath.Base(source), inputCloseErr)
	}
	if outputCloseErr != nil {
		return fmt.Errorf("close installer fixture clone file %s: %w", filepath.Base(target), outputCloseErr)
	}
	if err := os.Chmod(target, mode); err != nil {
		return fmt.Errorf("preserve installer fixture mode for %s: %w", filepath.Base(target), err)
	}
	return nil
}

var installerFixtureDistributionTemplate = newInstallerFixtureTemplate(nil)

func cloneInstallerFixtureDistribution(t *testing.T, target string) error {
	t.Helper()
	return installerFixtureDistributionTemplate.cloneToWithBuilder(target, func(templateDir string) error {
		repo := newReleaseBuildFixture(t)
		output, err := fixtureReleaseBuild(repo, templateDir, "0.1.0")
		if err != nil {
			return fmt.Errorf("release build failed: %w\n%s", err, output)
		}
		return nil
	})
}

func TestInstallerFixtureTemplateClonesRegularFilesWithIndependentInodesAndModes(t *testing.T) {
	var builds atomic.Int32
	template := newInstallerFixtureTemplate(installerFixtureTestBuilder(&builds))
	t.Cleanup(func() {
		if err := template.close(); err != nil {
			t.Fatal(err)
		}
	})

	first := filepath.Join(t.TempDir(), "dist")
	second := filepath.Join(t.TempDir(), "dist")
	if err := template.cloneTo(first); err != nil {
		t.Fatal(err)
	}
	if err := template.cloneTo(second); err != nil {
		t.Fatal(err)
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("template build count = %d, want 1", got)
	}

	for name, mode := range map[string]os.FileMode{
		"install-release.sh": 0o700,
		"SHA256SUMS":         0o600,
	} {
		templateInfo := assertRegularFileMode(t, filepath.Join(template.templateDir, name), mode)
		firstInfo := assertRegularFileMode(t, filepath.Join(first, name), mode)
		secondInfo := assertRegularFileMode(t, filepath.Join(second, name), mode)
		if os.SameFile(templateInfo, firstInfo) || os.SameFile(templateInfo, secondInfo) || os.SameFile(firstInfo, secondInfo) {
			t.Fatalf("%s clone shares an inode with the template or another clone", name)
		}
	}
}

func TestInstallerFixtureTemplateCloneMutationAndDeletionDoNotAliasLaterClone(t *testing.T) {
	template := newInstallerFixtureTemplate(installerFixtureTestBuilder(nil))
	t.Cleanup(func() {
		if err := template.close(); err != nil {
			t.Fatal(err)
		}
	})

	first := filepath.Join(t.TempDir(), "dist")
	if err := template.cloneTo(first); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "install-release.sh"), []byte("mutated\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(first, "SHA256SUMS")); err != nil {
		t.Fatal(err)
	}

	second := filepath.Join(t.TempDir(), "dist")
	if err := template.cloneTo(second); err != nil {
		t.Fatal(err)
	}
	assertInstallerFixtureFileBody(t, filepath.Join(second, "install-release.sh"), "installer\n")
	assertInstallerFixtureFileBody(t, filepath.Join(second, "SHA256SUMS"), "checksums\n")
	assertInstallerFixtureFileBody(t, filepath.Join(template.templateDir, "install-release.sh"), "installer\n")
	assertInstallerFixtureFileBody(t, filepath.Join(template.templateDir, "SHA256SUMS"), "checksums\n")
}

func TestInstallerFixtureTemplateSurvivesFirstSubtestCleanup(t *testing.T) {
	var builds atomic.Int32
	template := newInstallerFixtureTemplate(installerFixtureTestBuilder(&builds))
	t.Cleanup(func() {
		if err := template.close(); err != nil {
			t.Fatal(err)
		}
	})

	var first string
	t.Run("first caller", func(t *testing.T) {
		first = filepath.Join(t.TempDir(), "dist")
		if err := template.cloneTo(first); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := os.Stat(first); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("first subtest clone still exists after cleanup: %v", err)
	}
	if _, err := os.Stat(template.templateDir); err != nil {
		t.Fatalf("first subtest cleanup removed process template: %v", err)
	}

	second := filepath.Join(t.TempDir(), "dist")
	if err := template.cloneTo(second); err != nil {
		t.Fatal(err)
	}
	assertInstallerFixtureFileBody(t, filepath.Join(second, "install-release.sh"), "installer\n")
	if got := builds.Load(); got != 1 {
		t.Fatalf("template build count = %d, want 1", got)
	}
}

func TestInstallerFixtureTemplateBuildsOnceForConcurrentCallers(t *testing.T) {
	var builds atomic.Int32
	template := newInstallerFixtureTemplate(installerFixtureTestBuilder(&builds))
	t.Cleanup(func() {
		if err := template.close(); err != nil {
			t.Fatal(err)
		}
	})

	const callers = 16
	targets := make([]string, callers)
	for i := range targets {
		targets[i] = filepath.Join(t.TempDir(), "dist")
	}
	errs := make(chan error, callers)
	var callersDone sync.WaitGroup
	callersDone.Add(callers)
	for _, target := range targets {
		target := target
		go func() {
			defer callersDone.Done()
			errs <- template.cloneTo(target)
		}()
	}
	callersDone.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("template build count = %d, want 1", got)
	}
	for _, target := range targets {
		assertRegularFileMode(t, filepath.Join(target, "install-release.sh"), 0o700)
	}
}

func TestInstallerFixtureTemplatePropagatesBuilderFailureToEveryCaller(t *testing.T) {
	wantErr := errors.New("fixture builder failed")
	var builds atomic.Int32
	template := newInstallerFixtureTemplate(func(string) error {
		builds.Add(1)
		return wantErr
	})
	t.Cleanup(func() {
		if err := template.close(); err != nil {
			t.Fatal(err)
		}
	})

	for i := 0; i < 2; i++ {
		err := template.cloneTo(filepath.Join(t.TempDir(), "dist"))
		if !errors.Is(err, wantErr) {
			t.Fatalf("clone %d error = %v, want wrapped builder error", i+1, err)
		}
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("template build count = %d, want 1", got)
	}
}

func TestInstallerFixtureTemplateRecordsBuilderGoexitForLaterCallers(t *testing.T) {
	var builds atomic.Int32
	template := newInstallerFixtureTemplate(func(string) error {
		builds.Add(1)
		runtime.Goexit()
		return nil
	})
	t.Cleanup(func() {
		if err := template.close(); err != nil {
			t.Fatal(err)
		}
	})

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_ = template.cloneTo(filepath.Join(t.TempDir(), "first"))
	}()
	<-firstDone
	err := template.cloneTo(filepath.Join(t.TempDir(), "second"))
	if !errors.Is(err, errInstallerFixtureTemplateBuildAborted) {
		t.Fatalf("later clone error = %v, want builder-aborted error", err)
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("template build count = %d, want 1", got)
	}
}

func TestInstallerFixtureTemplateCleanupIsProcessOwned(t *testing.T) {
	template := newInstallerFixtureTemplate(installerFixtureTestBuilder(nil))
	cloneRoot := t.TempDir()
	if err := template.cloneTo(filepath.Join(cloneRoot, "dist")); err != nil {
		t.Fatal(err)
	}
	templateRoot := template.rootDir
	if err := os.RemoveAll(cloneRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(templateRoot); err != nil {
		t.Fatalf("caller cleanup removed process template: %v", err)
	}
	if err := template.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(templateRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("template root exists after process cleanup: %v", err)
	}
	if err := template.close(); err != nil {
		t.Fatalf("second process cleanup failed: %v", err)
	}
}

func TestInstallerFixtureTemplateRejectsNonRegularTemplateEntry(t *testing.T) {
	template := newInstallerFixtureTemplate(func(dir string) error {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		return os.Symlink("missing", filepath.Join(dir, "release-link"))
	})
	t.Cleanup(func() {
		if err := template.close(); err != nil {
			t.Fatal(err)
		}
	})

	err := template.cloneTo(filepath.Join(t.TempDir(), "dist"))
	if err == nil {
		t.Fatal("clone accepted a non-regular template entry")
	}
}

func installerFixtureTestBuilder(builds *atomic.Int32) func(string) error {
	return func(dir string) error {
		if builds != nil {
			builds.Add(1)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "install-release.sh"), []byte("installer\n"), 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte("checksums\n"), 0o600)
	}
}

func assertRegularFileMode(t *testing.T, path string, want os.FileMode) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s mode = %v, want regular non-symlink", path, info.Mode())
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %v, want %v", path, got, want)
	}
	return info
}

func assertInstallerFixtureFileBody(t *testing.T, path, want string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != want {
		t.Fatalf("%s body = %q, want %q", path, got, want)
	}
}
