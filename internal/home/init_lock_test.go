package home

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestInitRootLockRetriesEINTR(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	oldFlock := initRootFlock
	t.Cleanup(func() { initRootFlock = oldFlock })
	var exclusiveCalls atomic.Int32
	initRootFlock = func(fd int, how int) error {
		if how == syscall.LOCK_EX && exclusiveCalls.Add(1) == 1 {
			return syscall.EINTR
		}
		return oldFlock(fd, how)
	}

	if err := Init(homePath, HomeKindGlobal, time.Unix(0, 0)); err != nil {
		t.Fatalf("Init after interrupted directory lock: %v", err)
	}
	if got := exclusiveCalls.Load(); got != 2 {
		t.Fatalf("exclusive lock calls = %d, want one interrupted call plus one retry", got)
	}
}

func TestInitRootLockFailureLeavesHomeUninitialized(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected directory lock failure")
	oldFlock := initRootFlock
	t.Cleanup(func() { initRootFlock = oldFlock })
	initRootFlock = func(fd int, how int) error {
		if how == syscall.LOCK_EX {
			return injected
		}
		return oldFlock(fd, how)
	}

	err := Init(homePath, HomeKindGlobal, time.Unix(0, 0))
	if !errors.Is(err, injected) || !strings.Contains(err.Error(), "lock compound home root") {
		t.Fatalf("Init lock failure = %v", err)
	}
	entries, readErr := os.ReadDir(homePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("lock failure populated home: %+v", entries)
	}
}

func instrumentInitRootLockContention(t *testing.T) <-chan struct{} {
	t.Helper()
	oldFlock := initRootFlock
	contended := make(chan struct{}, 2)
	t.Cleanup(func() { initRootFlock = oldFlock })
	initRootFlock = func(fd int, how int) error {
		if how != syscall.LOCK_EX {
			return oldFlock(fd, how)
		}
		err := oldFlock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		contended <- struct{}{}
		return oldFlock(fd, syscall.LOCK_EX)
	}
	return contended
}

func TestConcurrentInitSerializesLexicalAliasesOfSameHome(t *testing.T) {
	parent := t.TempDir()
	homePath := filepath.Join(parent, ".compound")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(parent, "alias")
	if err := os.Mkdir(aliasParent, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasPath := aliasParent + string(filepath.Separator) + ".." + string(filepath.Separator) + ".compound"
	lockContended := instrumentInitRootLockContention(t)

	ready := make(chan struct{}, 1)
	release := make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	oldBeforePublish := initManifestBeforePublish
	t.Cleanup(func() { initManifestBeforePublish = oldBeforePublish })
	initManifestBeforePublish = func(*os.Root, string, []byte) error {
		ready <- struct{}{}
		<-release
		return nil
	}
	type result struct {
		kind string
		err  error
	}
	results := make(chan result, 2)
	go func() {
		results <- result{kind: HomeKindGlobal, err: Init(homePath, HomeKindGlobal, time.Unix(0, 0))}
	}()
	go func() {
		results <- result{kind: HomeKindLocal, err: Init(aliasPath, HomeKindLocal, time.Unix(1, 0))}
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("neither lexical alias reached manifest publication")
	}
	select {
	case <-lockContended:
	case <-time.After(5 * time.Second):
		t.Fatal("lexical aliases did not contend on the same physical directory lock")
	}
	close(release)
	released = true

	var succeeded, failed []result
	for range 2 {
		select {
		case got := <-results:
			if got.err == nil {
				succeeded = append(succeeded, got)
			} else {
				failed = append(failed, got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("lexical alias Init did not return")
		}
	}
	if len(succeeded) != 1 || len(failed) != 1 {
		t.Fatalf("lexical alias Init results: succeeded=%+v failed=%+v", succeeded, failed)
	}
	manifest, err := LoadManifest(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.HomeKind != succeeded[0].kind {
		t.Fatalf("published kind = %q, successful alias initializer = %q", manifest.HomeKind, succeeded[0].kind)
	}
}

func TestInitRevalidatesForeignPopulationAfterWaitingForRootLock(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(homePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	dir, err := root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	locked := true
	t.Cleanup(func() {
		if locked {
			_ = syscall.Flock(int(dir.Fd()), syscall.LOCK_UN)
		}
		_ = dir.Close()
	})
	if err := syscall.Flock(int(dir.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	lockContended := instrumentInitRootLockContention(t)

	afterSecured := make(chan struct{}, 1)
	oldAfterSecured := initAfterRootSecured
	t.Cleanup(func() { initAfterRootSecured = oldAfterSecured })
	initAfterRootSecured = func(string) error {
		afterSecured <- struct{}{}
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- Init(homePath, HomeKindGlobal, time.Unix(0, 0)) }()
	select {
	case <-afterSecured:
	case <-time.After(5 * time.Second):
		t.Fatal("Init did not reach the directory lock boundary")
	}
	select {
	case <-lockContended:
	case <-time.After(5 * time.Second):
		t.Fatal("Init did not contend on the held directory lock")
	}
	markerPath := filepath.Join(homePath, "foreign-marker")
	markerBody := []byte("must survive")
	if err := os.WriteFile(markerPath, markerBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(dir.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	locked = false

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "populated path has no manifest") {
			t.Fatalf("Init after foreign population = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Init did not return after directory lock release")
	}
	if got, err := os.ReadFile(markerPath); err != nil || !bytes.Equal(got, markerBody) {
		t.Fatalf("foreign marker changed: body=%q err=%v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(homePath, ManifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Init published beside foreign marker: %v", err)
	}
}

func TestInitDirectoryLockSerializesAcrossProcesses(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(homePath)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := root.Open(".")
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	locked := true
	t.Cleanup(func() {
		if locked {
			_ = syscall.Flock(int(dir.Fd()), syscall.LOCK_UN)
		}
		_ = dir.Close()
		_ = root.Close()
	})
	if err := syscall.Flock(int(dir.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestInitDirectoryLockProcessHelper$")
	cmd.Env = append(os.Environ(), "COMPOUND_INIT_LOCK_HELPER_HOME="+homePath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	childDone := make(chan error, 1)
	go func() { childDone <- cmd.Wait() }()
	childFinished := false
	t.Cleanup(func() {
		if !childFinished {
			_ = cmd.Process.Kill()
			<-childDone
		}
	})
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "attempting" {
		t.Fatalf("child did not announce Init attempt: line=%q stderr=%s", scanner.Text(), stderr.String())
	}
	contentionLine := make(chan string, 1)
	go func() {
		if scanner.Scan() {
			contentionLine <- scanner.Text()
			return
		}
		contentionLine <- ""
	}()
	select {
	case line := <-contentionLine:
		if line != "contended" {
			t.Fatalf("child lock evidence = %q, want contended; stderr=%s", line, stderr.String())
		}
	case err := <-childDone:
		childFinished = true
		locked = false
		_ = syscall.Flock(int(dir.Fd()), syscall.LOCK_UN)
		t.Fatalf("child Init never attempted the held directory lock: err=%v stderr=%s", err, stderr.String())
	case <-time.After(5 * time.Second):
		t.Fatal("child did not report directory-lock contention")
	}
	if _, err := os.Lstat(filepath.Join(homePath, ManifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blocked child published a manifest: %v", err)
	}
	if err := syscall.Flock(int(dir.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	locked = false
	select {
	case err := <-childDone:
		childFinished = true
		if err != nil {
			t.Fatalf("child Init after directory lock release: %v stderr=%s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child Init did not finish after directory lock release")
	}
	if _, err := LoadManifest(homePath); err != nil {
		t.Fatalf("child Init did not publish a valid manifest: %v", err)
	}
}

func TestInitDirectoryLockProcessHelper(t *testing.T) {
	homePath := os.Getenv("COMPOUND_INIT_LOCK_HELPER_HOME")
	if homePath == "" {
		return
	}
	oldFlock := initRootFlock
	var attempted atomic.Bool
	initRootFlock = func(fd int, how int) error {
		if how != syscall.LOCK_EX || !attempted.CompareAndSwap(false, true) {
			return oldFlock(fd, how)
		}
		err := oldFlock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			_ = oldFlock(fd, syscall.LOCK_UN)
			return errors.New("expected parent process to hold the home init directory lock")
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		fmt.Fprintln(os.Stdout, "contended")
		return oldFlock(fd, syscall.LOCK_EX)
	}
	fmt.Fprintln(os.Stdout, "attempting")
	if err := Init(homePath, HomeKindGlobal, time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
}
