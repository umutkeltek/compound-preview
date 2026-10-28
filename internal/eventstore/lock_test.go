package eventstore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

// countOpenFDs returns the number of open file descriptors in this process by
// counting the numeric entries under /dev/fd. Works on darwin and linux; the
// directory always exposes one entry per open fd. os.Open + Readdirnames is used
// (not os.ReadDir) because ReadDir's fstatat on the /dev/fd directory fails on
// darwin under go test.
func countOpenFDs(t *testing.T) int {
	t.Helper()
	f, err := os.Open("/dev/fd")
	if err != nil {
		t.Fatalf("open /dev/fd: %v", err)
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		t.Fatalf("read /dev/fd: %v", err)
	}
	n := 0
	for _, name := range names {
		if _, err := strconv.Atoi(name); err == nil {
			n++
		}
	}
	return n
}

// TestShardedAppendLockColdFamilyRaceDoesNotLeakFDs pins the fd-leak fix in
// shardedAppendLock.acquire: when two goroutines race the FIRST open of a cold
// family's lock file, the loser must close its duplicate open instead of
// leaking the fd for the process lifetime. Without the fix the process fd count
// grows by one per losing racer; with it, exactly one handle (the cached one)
// stays open per family.
//
// The race is forced by using a fresh segment root (a cold family in the
// package-level shared cache) and launching many goroutines at once so several
// see fd[family] == nil before any stores.
func TestShardedAppendLockColdFamilyRaceDoesNotLeakFDs(t *testing.T) {
	segRoot := t.TempDir()
	lock := shardedAppendLocksFor(segRoot)

	before := countOpenFDs(t)
	const racers = 32
	var wg sync.WaitGroup
	errs := make(chan error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := lock.acquire("tr_")
			if err != nil {
				errs <- err
				return
			}
			if err := lock.release("tr_", l); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent acquire/release: %v", err)
	}

	after := countOpenFDs(t)
	// Exactly one fd must remain open for the family's lock file (the cached
	// handle). Any delta beyond that is a leaked duplicate from the race.
	if got := after - before; got != 1 {
		// Guard: other package-level state (such as the hot segment cache)
		// may hold unrelated fds; but acquire of a single cold family in a fresh
		// temp root should add exactly one. Report the delta and the map size.
		t.Fatalf("open fds grew by %d after cold-family race, want 1 (one cached handle); len(fd)=%d",
			got, len(lock.fd))
	}
	if len(lock.fd) != 1 {
		t.Fatalf("family lock map has %d entries, want 1", len(lock.fd))
	}
	lockFile := filepath.Join(segRoot, appendLockName+".tr_")
	if _, err := os.Stat(lockFile); err != nil {
		t.Fatalf("lock file not created: %v", err)
	}
}

// TestShardedAppendLockAcquireAllSortedDeadlockFree covers the all-family path
// used by maintenance: acquireAll must lock every shard (sorted) and release
// them all on success and on partial failure.
func TestShardedAppendLockAcquireAllSortedDeadlockFree(t *testing.T) {
	lock := shardedAppendLocksFor(t.TempDir())
	families := []string{"tr_", "act_", "rec_", "tr_", "pol_"} // deliberate dup + unsorted
	releaseAll, err := lock.acquireAll(families)
	if err != nil {
		t.Fatalf("acquireAll: %v", err)
	}
	// Sorted unique set, all held: every family in the map is one of them.
	if len(lock.fd) != 4 {
		t.Fatalf("acquired %d unique family shards, want 4 (tr_, act_, rec_, pol_)", len(lock.fd))
	}
	if err := releaseAll(); err != nil {
		t.Fatalf("releaseAll: %v", err)
	}
	// After release the handles stay cached (not closed), so the map is intact.
	if len(lock.fd) != 4 {
		t.Fatalf("family lock map shrank after release, have %d cached handles, want 4", len(lock.fd))
	}
}

// TestShardedAppendLockReleaseKeepsHandleCached verifies release does not close
// the cached handle, so the next acquire is a cheap flock on the same fd.
func TestShardedAppendLockReleaseKeepsHandleCached(t *testing.T) {
	lock := shardedAppendLocksFor(t.TempDir())
	l1, err := lock.acquire("tr_")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := lock.release("tr_", l1); err != nil {
		t.Fatalf("release: %v", err)
	}
	l2, err := lock.acquire("tr_")
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	if l2.file != l1.file {
		t.Fatal("re-acquire returned a different handle; cached handle was not reused")
	}
}

func TestShardedAppendLockReleaseRetriesInterruptedUnlock(t *testing.T) {
	lock := shardedAppendLocksFor(t.TempDir())
	held, err := lock.acquire("tr_")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	unlockCalls := 0
	lock.rawFlock = func(fd int, how int) error {
		if how == syscall.LOCK_UN {
			unlockCalls++
			if unlockCalls == 1 {
				return syscall.EINTR
			}
		}
		return syscall.Flock(fd, how)
	}
	if err := lock.release("tr_", held); err != nil {
		t.Fatalf("release after interrupted unlock: %v", err)
	}
	if unlockCalls != 2 {
		t.Fatalf("raw unlock calls = %d, want 2 (EINTR then success)", unlockCalls)
	}

	lock.mu.Lock()
	cached := lock.fd["tr_"]
	lock.mu.Unlock()
	if cached != held.file {
		t.Fatal("successful unlock retry retired the healthy cached descriptor")
	}
}

func TestShardedAppendLockPersistentUnlockFailureRetiresExactCachedFD(t *testing.T) {
	segRoot := t.TempDir()
	lock := shardedAppendLocksFor(segRoot)
	t.Cleanup(func() {
		lock.rawFlock = syscall.Flock
		lock.mu.Lock()
		files := make([]*os.File, 0, len(lock.fd))
		for family, file := range lock.fd {
			delete(lock.fd, family)
			files = append(files, file)
		}
		lock.mu.Unlock()
		for _, file := range files {
			_ = file.Close()
		}
	})

	other, err := lock.acquire("act_")
	if err != nil {
		t.Fatalf("acquire unrelated family: %v", err)
	}
	if err := lock.release("act_", other); err != nil {
		t.Fatalf("release unrelated family: %v", err)
	}
	otherCached := other.file

	held, err := lock.acquire("tr_")
	if err != nil {
		t.Fatalf("acquire target family: %v", err)
	}
	heldFD := int(held.file.Fd())
	assertChildFlock(t, filepath.Join(segRoot, appendLockName+".tr_"), false)

	unlockErr := errors.New("persistent raw unlock failure")
	unlockAttempted := make(chan struct{})
	var unlockOnce sync.Once
	lock.rawFlock = func(fd int, how int) error {
		if fd == heldFD && how == syscall.LOCK_UN {
			unlockOnce.Do(func() { close(unlockAttempted) })
			return unlockErr
		}
		return syscall.Flock(fd, how)
	}

	// Hold the cache mutex so a correct release is forced to pause at exact-fd
	// eviction. During that pause both processMu and the ordinary-family gate
	// must still exclude local writers.
	lock.mu.Lock()
	releaseDone := make(chan error, 1)
	go func() { releaseDone <- lock.release("tr_", held) }()
	select {
	case <-unlockAttempted:
	case <-time.After(5 * time.Second):
		lock.mu.Unlock()
		t.Fatal("release did not attempt the injected raw unlock")
	}

	processHeld := !held.processMu.TryLock()
	if !processHeld {
		held.processMu.Unlock()
	}
	gateHeld := !lock.gate.TryLock()
	if !gateHeld {
		lock.gate.Unlock()
	}
	var prematureErr error
	select {
	case prematureErr = <-releaseDone:
	default:
	}
	lock.mu.Unlock()
	if prematureErr != nil {
		t.Fatalf("release returned before exact-fd retirement could run: %v", prematureErr)
	}
	if !processHeld || !gateHeld {
		t.Fatalf("local exclusion released before fd retirement: processMu held=%t gate held=%t", processHeld, gateHeld)
	}

	var releaseErr error
	select {
	case releaseErr = <-releaseDone:
	case <-time.After(5 * time.Second):
		t.Fatal("release did not finish after cache eviction was unblocked")
	}
	if !errors.Is(releaseErr, unlockErr) {
		t.Fatalf("release error = %v, want persistent raw unlock failure", releaseErr)
	}
	lock.rawFlock = syscall.Flock

	lock.mu.Lock()
	_, staleTarget := lock.fd["tr_"]
	gotOther := lock.fd["act_"]
	lock.mu.Unlock()
	if staleTarget {
		t.Fatal("failed unlock left the retired target descriptor in the cache")
	}
	if gotOther != otherCached {
		t.Fatal("failed target unlock evicted an unrelated family's cached descriptor")
	}
	if _, err := held.file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("retired descriptor Stat error = %v, want os.ErrClosed", err)
	}
	if _, err := otherCached.Stat(); err != nil {
		t.Fatalf("unrelated cached descriptor was closed: %v", err)
	}

	assertChildFlock(t, filepath.Join(segRoot, appendLockName+".tr_"), true)
	fresh, err := lock.acquire("tr_")
	if err != nil {
		t.Fatalf("local acquire after retirement: %v", err)
	}
	if fresh.file == held.file {
		t.Fatal("local acquire reused the retired descriptor")
	}
	lock.mu.Lock()
	cachedFresh := lock.fd["tr_"]
	lock.mu.Unlock()
	if cachedFresh != fresh.file {
		t.Fatal("fresh local descriptor was not cached after retirement")
	}
	if err := lock.release("tr_", fresh); err != nil {
		t.Fatalf("release fresh descriptor: %v", err)
	}
}

func TestShardedAppendLockReleaseJoinsUnlockAndCloseErrors(t *testing.T) {
	lock := shardedAppendLocksFor(t.TempDir())
	held, err := lock.acquire("tr_")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := held.file.Close(); err != nil {
		t.Fatalf("pre-close held descriptor: %v", err)
	}

	unlockErr := errors.New("raw unlock failure")
	lock.rawFlock = func(_ int, how int) error {
		if how == syscall.LOCK_UN {
			return unlockErr
		}
		return nil
	}
	t.Cleanup(func() { lock.rawFlock = syscall.Flock })
	releaseErr := lock.release("tr_", held)
	if !errors.Is(releaseErr, unlockErr) || !errors.Is(releaseErr, os.ErrClosed) {
		t.Fatalf("release error = %v, want joined unlock and close errors", releaseErr)
	}
	lock.mu.Lock()
	_, stale := lock.fd["tr_"]
	lock.mu.Unlock()
	if stale {
		t.Fatal("unlock+close failure left the exact descriptor cached")
	}
}

func TestShardedAppendLockAcquireAllReleaseAggregatesUnlockFailures(t *testing.T) {
	lock := shardedAppendLocksFor(t.TempDir())
	t.Cleanup(func() {
		lock.rawFlock = syscall.Flock
		lock.mu.Lock()
		files := make([]*os.File, 0, len(lock.fd))
		for family, file := range lock.fd {
			delete(lock.fd, family)
			files = append(files, file)
		}
		lock.mu.Unlock()
		for _, file := range files {
			_ = file.Close()
		}
	})

	releaseAll, err := lock.acquireAll([]string{"tr_", "act_", "rec_"})
	if err != nil {
		t.Fatalf("acquireAll: %v", err)
	}
	lock.mu.Lock()
	actFile := lock.fd["act_"]
	recFile := lock.fd["rec_"]
	trFile := lock.fd["tr_"]
	lock.mu.Unlock()
	recErr := errors.New("receipt unlock failure")
	trErr := errors.New("transition unlock failure")
	lock.rawFlock = func(fd int, how int) error {
		if how == syscall.LOCK_UN {
			switch fd {
			case int(recFile.Fd()):
				return recErr
			case int(trFile.Fd()):
				return trErr
			}
		}
		return syscall.Flock(fd, how)
	}

	releaseErr := releaseAll()
	if !errors.Is(releaseErr, recErr) || !errors.Is(releaseErr, trErr) {
		t.Fatalf("releaseAll error = %v, want both family unlock failures", releaseErr)
	}
	lock.rawFlock = syscall.Flock

	lock.mu.Lock()
	gotAct := lock.fd["act_"]
	_, staleRec := lock.fd["rec_"]
	_, staleTr := lock.fd["tr_"]
	lock.mu.Unlock()
	if gotAct != actFile {
		t.Fatal("successful all-family unlock retired the healthy descriptor")
	}
	if staleRec || staleTr {
		t.Fatalf("failed all-family unlock left stale descriptors: rec=%t tr=%t", staleRec, staleTr)
	}
	for family, file := range map[string]*os.File{"rec_": recFile, "tr_": trFile} {
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("retired %s descriptor Stat error = %v, want os.ErrClosed", family, err)
		}
	}

	// Every process mutex and the exclusive gate must be released even when
	// more than one kernel unlock failed.
	for _, family := range []string{"act_", "rec_", "tr_"} {
		held, err := lock.acquire(family)
		if err != nil {
			t.Fatalf("acquire %s after partial release failure: %v", family, err)
		}
		if err := lock.release(family, held); err != nil {
			t.Fatalf("release %s after partial release failure: %v", family, err)
		}
	}
}

func TestShardedAppendLockAcquireAllJoinsPartialCleanupFailure(t *testing.T) {
	lock := shardedAppendLocksFor(t.TempDir())
	t.Cleanup(func() {
		lock.rawFlock = syscall.Flock
		lock.mu.Lock()
		files := make([]*os.File, 0, len(lock.fd))
		for family, file := range lock.fd {
			delete(lock.fd, family)
			files = append(files, file)
		}
		lock.mu.Unlock()
		for _, file := range files {
			_ = file.Close()
		}
	})

	acquireErr := errors.New("second family acquire failure")
	cleanupErr := errors.New("first family cleanup unlock failure")
	acquireCalls := 0
	var firstFile *os.File
	lock.rawFlock = func(fd int, how int) error {
		switch how {
		case syscall.LOCK_EX:
			acquireCalls++
			if acquireCalls == 2 {
				return acquireErr
			}
			if err := syscall.Flock(fd, how); err != nil {
				return err
			}
			lock.mu.Lock()
			firstFile = lock.fd["act_"]
			lock.mu.Unlock()
			return nil
		case syscall.LOCK_UN:
			if firstFile != nil && fd == int(firstFile.Fd()) {
				return cleanupErr
			}
		}
		return syscall.Flock(fd, how)
	}

	releaseAll, err := lock.acquireAll([]string{"rec_", "act_"})
	if releaseAll != nil {
		t.Fatal("failed acquireAll returned a release closure")
	}
	if !errors.Is(err, acquireErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("acquireAll error = %v, want acquisition and partial-cleanup failures", err)
	}
	lock.rawFlock = syscall.Flock
	if firstFile == nil {
		t.Fatal("first family descriptor was not captured")
	}
	if _, statErr := firstFile.Stat(); !errors.Is(statErr, os.ErrClosed) {
		t.Fatalf("partial-cleanup descriptor Stat error = %v, want os.ErrClosed", statErr)
	}
	lock.mu.Lock()
	_, staleFirst := lock.fd["act_"]
	lock.mu.Unlock()
	if staleFirst {
		t.Fatal("partial acquire cleanup left its failed-unlock descriptor cached")
	}

	// The failed acquisition and failed cleanup must still release the process
	// mutexes and exclusive gate so a later family can make progress.
	held, err := lock.acquire("act_")
	if err != nil {
		t.Fatalf("acquire after partial cleanup failure: %v", err)
	}
	if err := lock.release("act_", held); err != nil {
		t.Fatalf("release after partial cleanup failure: %v", err)
	}
}

func TestShardedAppendLockChildProcess(t *testing.T) {
	path := os.Getenv("COMPOUND_EVENTSTORE_TEST_LOCK_PATH")
	if path == "" {
		return
	}
	wantAcquire := os.Getenv("COMPOUND_EVENTSTORE_TEST_LOCK_ACQUIRE") == "1"
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("child open lock: %v", err)
	}
	defer file.Close()
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if wantAcquire {
		if err != nil {
			t.Fatalf("child acquire after retirement: %v", err)
		}
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
			t.Fatalf("child unlock: %v", err)
		}
		return
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
		if err == nil {
			_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		}
		t.Fatalf("child lock while parent holds it = %v, want would-block", err)
	}
}

func assertChildFlock(t *testing.T, path string, wantAcquire bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestShardedAppendLockChildProcess$")
	want := "0"
	if wantAcquire {
		want = "1"
	}
	cmd.Env = append(os.Environ(),
		"COMPOUND_EVENTSTORE_TEST_LOCK_PATH="+path,
		"COMPOUND_EVENTSTORE_TEST_LOCK_ACQUIRE="+want,
	)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("child flock timed out: %v", ctx.Err())
	}
	if err != nil {
		t.Fatalf("child flock failed: %v\n%s", err, output)
	}
}
