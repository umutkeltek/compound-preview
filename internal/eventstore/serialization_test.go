package eventstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestShardedAppendLocksCanonicalizeMissingRootAcrossAliases(t *testing.T) {
	base := t.TempDir()
	realHome := filepath.Join(base, "real-home")
	if err := os.Mkdir(realHome, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasHome := filepath.Join(base, "alias-home")
	if err := os.Symlink(realHome, aliasHome); err != nil {
		t.Fatal(err)
	}

	realRoot := filepath.Join(realHome, eventsDirName, segmentsDirName)
	aliasRoot := filepath.Join(aliasHome, eventsDirName, segmentsDirName)
	if _, err := os.Stat(realRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("segments root exists before lock binding: %v", err)
	}

	realLocks := shardedAppendLocksFor(realRoot)
	aliasLocks := shardedAppendLocksFor(aliasRoot)
	if realLocks != aliasLocks {
		t.Fatal("real and symlink-alias homes received different in-process lock state")
	}
}

func TestShardedAppendLockSerializesSameFamilyInProcess(t *testing.T) {
	locks := shardedAppendLocksFor(t.TempDir())
	first, err := locks.acquire("receipt")
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	acquired := make(chan *appendLock, 1)
	errCh := make(chan error, 1)
	go func() {
		close(started)
		second, acquireErr := locks.acquire("receipt")
		if acquireErr != nil {
			errCh <- acquireErr
			return
		}
		acquired <- second
	}()
	<-started

	select {
	case second := <-acquired:
		_ = locks.release("receipt", second)
		_ = locks.release("receipt", first)
		t.Fatal("second same-family critical section entered before the first released")
	case acquireErr := <-errCh:
		_ = locks.release("receipt", first)
		t.Fatalf("second acquire failed: %v", acquireErr)
	case <-time.After(100 * time.Millisecond):
	}

	if err := locks.release("receipt", first); err != nil {
		t.Fatal(err)
	}
	select {
	case second := <-acquired:
		if err := locks.release("receipt", second); err != nil {
			t.Fatal(err)
		}
	case acquireErr := <-errCh:
		t.Fatalf("second acquire failed after release: %v", acquireErr)
	case <-time.After(5 * time.Second):
		t.Fatal("second same-family critical section did not enter after release")
	}
}

func TestStorePutRevalidatesAfterBeforePublish(t *testing.T) {
	now := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	id := "rec_before_revalidate"
	value := eventRecord(id, "compound.receipt.v0.1", now, "same")

	result, err := store.PutJSONAtomic(id, value, "  ", AtomicOptions{
		BeforePublish: func() error {
			nested, nestedErr := store.PutJSON(id, value, "  ")
			if nestedErr != nil {
				return nestedErr
			}
			if !nested.Created {
				return errors.New("nested publisher did not create the record")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("outer put: %v", err)
	}
	if result.Created {
		t.Fatal("outer put reported Created after the callback already published the same id")
	}
	if got := segmentFrameCountForID(t, store, id); got != 1 {
		t.Fatalf("segment contains %d frames for %s, want exactly 1", got, id)
	}
}

func TestStorePutSecondRevalidationRepairsTailTornByBeforePublish(t *testing.T) {
	now := time.Date(2026, 8, 14, 9, 2, 0, 0, time.UTC)
	var repairs []string
	store := openConfiguredStore(t, engineConfig{
		clock: func() time.Time { return now },
		logf:  func(format string, args ...any) { repairs = append(repairs, fmt.Sprintf(format, args...)) },
	})
	seedID := "rec_second_revalidation_seed"
	targetID := "rec_second_revalidation_target"
	putRecord(t, store, seedID, "compound.receipt.v0.1", now, "seed")
	path := hotSegmentPath(store, "receipt", ingestionDay(now))

	result, err := store.PutJSONAtomic(
		targetID,
		eventRecord(targetID, "compound.receipt.v0.1", now, "target"),
		"  ",
		AtomicOptions{BeforePublish: func() error {
			file, openErr := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if openErr != nil {
				return openErr
			}
			if _, writeErr := file.Write([]byte(`{"torn":`)); writeErr != nil {
				_ = file.Close()
				return writeErr
			}
			return file.Close()
		}},
	)
	if err != nil {
		t.Fatalf("put after callback tears the hot tail: %v", err)
	}
	if !result.Created {
		t.Fatal("put after repaired callback tail did not report Created")
	}

	frames, err := parseSegment(path, false)
	if err != nil {
		t.Fatalf("strict parse after second-phase tail repair: %v", err)
	}
	if len(frames) != 2 || frames[0].Frame.ID != seedID || frames[1].Frame.ID != targetID {
		t.Fatalf("strict frames = %+v, want exactly seed %s then target %s", frames, seedID, targetID)
	}
	if len(repairs) != 1 {
		t.Fatalf("tail repair logs = %q, want exactly one repair", repairs)
	}
}

func TestStorePutRevalidationDetectsCallbackConflict(t *testing.T) {
	now := time.Date(2026, 8, 14, 9, 5, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	id := "rec_before_conflict"

	result, err := store.PutJSONAtomic(
		id,
		eventRecord(id, "compound.receipt.v0.1", now, "outer"),
		"  ",
		AtomicOptions{BeforePublish: func() error {
			_, nestedErr := store.PutJSON(id, eventRecord(id, "compound.receipt.v0.1", now, "nested"), "  ")
			return nestedErr
		}},
	)
	if !errors.Is(err, ErrIDConflict) {
		t.Fatalf("outer put error = %v, want ErrIDConflict", err)
	}
	if result.Created {
		t.Fatal("conflicting outer put reported Created")
	}
	if got := segmentFrameCountForID(t, store, id); got != 1 {
		t.Fatalf("segment contains %d frames for %s, want nested publisher only", got, id)
	}
}

func TestStorePutCallbacksCanReenterSameFamily(t *testing.T) {
	now := time.Date(2026, 8, 14, 9, 10, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	var once sync.Once

	result, err := store.PutJSONAtomic(
		"rec_callback_outer",
		eventRecord("rec_callback_outer", "compound.receipt.v0.1", now, "outer"),
		"  ",
		AtomicOptions{
			Write: func(file *os.File, data []byte) error {
				if _, nestedErr := store.PutJSON("rec_callback_write", eventRecord("rec_callback_write", "compound.receipt.v0.1", now, "write"), "  "); nestedErr != nil {
					return nestedErr
				}
				return writeFull(file, data)
			},
			BeforePublish: func() error {
				_, nestedErr := store.PutJSON("rec_callback_before", eventRecord("rec_callback_before", "compound.receipt.v0.1", now, "before"), "  ")
				return nestedErr
			},
			AfterPublish: func() error {
				var nestedErr error
				once.Do(func() {
					_, nestedErr = store.PutJSON("rec_callback_after", eventRecord("rec_callback_after", "compound.receipt.v0.1", now, "after"), "  ")
				})
				return nestedErr
			},
		},
	)
	if err != nil {
		t.Fatalf("outer put with reentrant callbacks: %v", err)
	}
	if !result.Created {
		t.Fatal("outer put was not created")
	}
	for _, id := range []string{"rec_callback_write", "rec_callback_before", "rec_callback_outer", "rec_callback_after"} {
		if _, err := store.Read(id); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
	}
}

func TestShardedAppendLockDifferentFamiliesProgressIndependently(t *testing.T) {
	locks := shardedAppendLocksFor(t.TempDir())
	receipt, err := locks.acquire("receipt")
	if err != nil {
		t.Fatal(err)
	}
	defer locks.release("receipt", receipt)

	transition, err := locks.acquire("transition")
	if err != nil {
		t.Fatalf("different-family acquire blocked or failed: %v", err)
	}
	if err := locks.release("transition", transition); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRetryAfterForeignDurableRemoveDoesNotTrustRecentWriteMemo(t *testing.T) {
	now := time.Date(2026, 8, 14, 9, 15, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	id := "rec_foreign_remove_retry"
	value := eventRecord(id, "compound.receipt.v0.1", now, "same")

	first, err := store.PutJSON(id, value, "  ")
	if err != nil || !first.Created {
		t.Fatalf("initial put: result=%+v err=%v", first, err)
	}
	path := hotSegmentPath(store, "receipt", ingestionDay(now))
	frames, err := parseSegment(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || frames[0].Frame.ID != id {
		t.Fatalf("initial segment frames = %+v, want only %s", frames, id)
	}

	// Simulate a cooperating foreign process removing the tail under its own
	// lock. It cannot invalidate this process's package-local recent-write memo.
	if err := truncateAndSync(path, frames[0].Offset, true); err != nil {
		t.Fatalf("foreign durable remove: %v", err)
	}

	retry, err := store.PutJSON(id, value, "  ")
	if err != nil {
		t.Fatalf("retry after foreign removal: %v", err)
	}
	if !retry.Created {
		t.Fatal("retry trusted the stale recent-write memo and failed to recreate the missing durable record")
	}
	if got := segmentFrameCountForID(t, store, id); got != 1 {
		t.Fatalf("segment contains %d frames for %s after retry, want exactly 1", got, id)
	}
}

func TestStoreRemoveLocksKnownAndDiscoveredFamilies(t *testing.T) {
	store := openConfiguredStore(t, engineConfig{})
	discovered := "future-family"
	if err := os.MkdirAll(filepath.Join(store.engine.segRoot, discovered), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := store.Remove("rec_missing_remove_lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove missing record error = %v, want os.ErrNotExist", err)
	}

	want := map[string]bool{"other": true, discovered: true}
	for _, definition := range idPrefixFamilies {
		want[definition.family] = true
	}
	for family := range want {
		path := filepath.Join(store.engine.segRoot, appendLockName+"."+family)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("Remove did not acquire %q family lock %s: %v", family, path, err)
		}
	}
}

func TestStoreRemoveRecensusesFamiliesAfterAcquiringAllFamilyBoundary(t *testing.T) {
	now := time.Date(2026, 8, 14, 9, 20, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	id := "rec_remove_recensus"
	record, err := marshalJSONLine(eventRecord(id, "compound.receipt.v0.1", now, "body"), "  ")
	if err != nil {
		t.Fatal(err)
	}
	frame, _, err := buildFrame(id, record)
	if err != nil {
		t.Fatal(err)
	}
	line, err := encodeFrame(frame)
	if err != nil {
		t.Fatal(err)
	}

	// Hold the mutation boundary while Remove takes its pre-lock view. The
	// serialized predecessor then creates a previously absent family before
	// handing over the boundary.
	releaseAll, err := store.engine.lockAllFamilyShards()
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	var waitingOnce sync.Once
	store.engine.appendShards.waiting = func(scope string) {
		if scope == allFamiliesLockScope {
			waitingOnce.Do(func() { close(waiting) })
		}
	}
	removeDone := make(chan error, 1)
	go func() { removeDone <- store.Remove(id) }()
	<-waiting
	if _, _, err := store.engine.appendFrameAt(frame.Family, ingestionDay(now), line); err != nil {
		t.Fatalf("serialized predecessor append: %v", errors.Join(err, releaseAll()))
	}
	if err := releaseAll(); err != nil {
		t.Fatalf("release serialized predecessor boundary: %v", err)
	}

	if err := <-removeDone; err != nil {
		t.Fatalf("Remove missed the family created before it acquired the boundary: %v", err)
	}
	if _, err := store.Read(id); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record remains after serialized Remove: %v", err)
	}
}

func TestStoreRemoveSurfacesAllFamilyReleaseFailure(t *testing.T) {
	now := time.Date(2026, 8, 14, 9, 22, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	id := "rec_remove_release_failure"
	putRecord(t, store, id, "compound.receipt.v0.1", now, "body")

	shards := store.engine.appendShards
	shards.mu.Lock()
	target := shards.fd["receipt"]
	shards.mu.Unlock()
	if target == nil {
		t.Fatal("put did not cache the receipt family descriptor")
	}
	unlockErr := errors.New("all-family raw unlock failure")
	shards.rawFlock = func(fd int, how int) error {
		if how == syscall.LOCK_UN && fd == int(target.Fd()) {
			return unlockErr
		}
		return syscall.Flock(fd, how)
	}
	t.Cleanup(func() { shards.rawFlock = syscall.Flock })

	err := store.Remove(id)
	shards.rawFlock = syscall.Flock
	if !errors.Is(err, unlockErr) {
		t.Fatalf("Remove error = %v, want all-family release failure", err)
	}
	if _, readErr := store.Read(id); !errors.Is(readErr, os.ErrNotExist) {
		t.Fatalf("record remains after Remove published before release failure: %v", readErr)
	}
	shards.mu.Lock()
	_, stale := shards.fd["receipt"]
	shards.mu.Unlock()
	if stale {
		t.Fatal("Remove release failure left the receipt descriptor cached")
	}
}

func TestStoreConcurrentSameFamilyPutCreatesOneFrame(t *testing.T) {
	now := time.Date(2026, 8, 14, 9, 25, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	id := "rec_same_family_race"
	value := eventRecord(id, "compound.receipt.v0.1", now, "same")

	const writers = 24
	start := make(chan struct{})
	results := make(chan PutResult, writers)
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := store.PutJSON(id, value, "  ")
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent put: %v", err)
	}
	created := 0
	for result := range results {
		if result.Created {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("Created=true outcomes = %d, want exactly 1", created)
	}
	if got := segmentFrameCountForID(t, store, id); got != 1 {
		t.Fatalf("segment contains %d frames for %s, want exactly 1", got, id)
	}
}

func TestStoreCrossedFamilyCallbacksDoNotDeadlock(t *testing.T) {
	now := time.Date(2026, 8, 14, 9, 30, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	entered := make(chan struct{}, 2)
	releaseCallbacks := make(chan struct{})
	done := make(chan error, 2)

	go func() {
		_, err := store.PutJSONAtomic(
			"rec_cross_outer",
			eventRecord("rec_cross_outer", "compound.receipt.v0.1", now, "outer"),
			"  ",
			AtomicOptions{BeforePublish: func() error {
				entered <- struct{}{}
				<-releaseCallbacks
				_, nestedErr := store.PutJSON("tr_cross_nested", eventRecord("tr_cross_nested", "compound.transition.v0.1", now, "nested"), "  ")
				return nestedErr
			}},
		)
		done <- err
	}()
	go func() {
		_, err := store.PutJSONAtomic(
			"tr_cross_outer",
			eventRecord("tr_cross_outer", "compound.transition.v0.1", now, "outer"),
			"  ",
			AtomicOptions{BeforePublish: func() error {
				entered <- struct{}{}
				<-releaseCallbacks
				_, nestedErr := store.PutJSON("rec_cross_nested", eventRecord("rec_cross_nested", "compound.receipt.v0.1", now, "nested"), "  ")
				return nestedErr
			}},
		)
		done <- err
	}()

	<-entered
	<-entered
	close(releaseCallbacks)
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("crossed callback put: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("crossed receipt→transition / transition→receipt callbacks deadlocked")
		}
	}
	for _, id := range []string{"rec_cross_outer", "tr_cross_nested", "tr_cross_outer", "rec_cross_nested"} {
		if _, err := store.Read(id); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
	}
}

func TestStorePutJoinsUnlockAndAfterPublishErrors(t *testing.T) {
	now := time.Date(2026, 8, 14, 9, 35, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	unlockErr := errors.New("injected raw unlock failure")
	afterErr := errors.New("injected after-publish failure")
	callbackOrderErr := errors.New("AfterPublish ran before lock retirement")
	shards := store.engine.appendShards
	var heldFile *os.File
	shards.rawFlock = func(fd int, how int) error {
		if how == syscall.LOCK_EX {
			if err := syscall.Flock(fd, how); err != nil {
				return err
			}
			shards.mu.Lock()
			heldFile = shards.fd["receipt"]
			shards.mu.Unlock()
			return nil
		}
		if how == syscall.LOCK_UN && heldFile != nil && fd == int(heldFile.Fd()) {
			return unlockErr
		}
		return syscall.Flock(fd, how)
	}
	t.Cleanup(func() { shards.rawFlock = syscall.Flock })
	callbackRan := false

	result, err := store.PutJSONAtomic(
		"rec_join_release_callback",
		eventRecord("rec_join_release_callback", "compound.receipt.v0.1", now, "body"),
		"  ",
		AtomicOptions{AfterPublish: func() error {
			callbackRan = true
			shards.mu.Lock()
			_, stale := shards.fd["receipt"]
			shards.mu.Unlock()
			closed := heldFile != nil
			if closed {
				_, statErr := heldFile.Stat()
				closed = errors.Is(statErr, os.ErrClosed)
			}
			processMu := shards.mutexFor("receipt")
			processReleased := processMu.TryLock()
			if processReleased {
				processMu.Unlock()
			}
			gateReleased := shards.gate.TryLock()
			if gateReleased {
				shards.gate.Unlock()
			}
			if stale || !closed || !processReleased || !gateReleased {
				return errors.Join(afterErr, callbackOrderErr)
			}
			return afterErr
		}},
	)
	shards.rawFlock = syscall.Flock
	if !result.Created {
		t.Fatal("durably published record lost Created=true when post-publication errors occurred")
	}
	if !callbackRan {
		t.Fatal("AfterPublish did not run after the unlock failure")
	}
	if !errors.Is(err, unlockErr) || !errors.Is(err, afterErr) {
		t.Fatalf("put error = %v, want joined unlock and AfterPublish errors", err)
	}
	if errors.Is(err, callbackOrderErr) {
		t.Fatalf("put error = %v; AfterPublish observed an unretired lock", err)
	}
	if _, readErr := store.Read("rec_join_release_callback"); readErr != nil {
		t.Fatalf("published record is unreadable: %v", readErr)
	}
}
