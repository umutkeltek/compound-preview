package eventstore

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMigrateBatchRecensusesPresenceAfterConcurrentRemove(t *testing.T) {
	store := openMigrateStore(t)
	id := "rec_batch_recensus"
	putRecord(t, store, id, schemaForID(id), migrateNow, "same")
	seedLoose(t, store, id, "same")
	path := hotSegmentPath(store, "receipt", ingestionDay(migrateNow))

	// Hold the all-family mutation boundary while migration reaches its batch.
	// Removing segment truth only after the waiter is observed proves that the
	// presence census belongs under the boundary, rather than to an earlier run
	// phase that can go stale while acquisition blocks.
	releaseAll, err := store.engine.lockAllFamilyShards()
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			if err := releaseAll(); err != nil {
				t.Errorf("release all-family boundary during cleanup: %v", err)
			}
		}
	}()
	waiting := make(chan struct{})
	var waitingOnce sync.Once
	store.engine.appendShards.waiting = func(scope string) {
		if scope == allFamiliesLockScope {
			waitingOnce.Do(func() { close(waiting) })
		}
	}
	type migrateResult struct {
		report MigrateReport
		err    error
	}
	done := make(chan migrateResult, 1)
	go func() {
		report, migrateErr := store.Migrate(MigrateOptions{Mode: ModeApply, BatchSize: 1})
		done <- migrateResult{report: report, err: migrateErr}
	}()
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("migration batch did not wait for the held all-family boundary")
	}
	if err := truncateAndSync(path, 0, true); err != nil {
		t.Fatalf("remove segment truth while migration waits: %v", err)
	}
	invalidateHotSegment(path)
	if err := releaseAll(); err != nil {
		t.Fatalf("release held all-family boundary: %v", err)
	}
	released = true

	var got migrateResult
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("migration did not resume after the all-family boundary released")
	}
	if got.err != nil {
		t.Fatalf("migrate after concurrent remove: %v", got.err)
	}
	if !got.report.Completion.Complete {
		t.Fatalf("migration did not rebuild removed segment truth from the loose body: %v", got.report.Completion.Reasons)
	}
	if got.report.Migrated != 1 || got.report.AlreadyPresent != 0 {
		t.Fatalf("batch result after recensus = migrated:%d already_present:%d, want 1/0", got.report.Migrated, got.report.AlreadyPresent)
	}
	if _, err := store.Read(id); err != nil {
		t.Fatalf("rebuilt record is unreadable: %v", err)
	}
	if got := segmentFrameCountForID(t, store, id); got != 1 {
		t.Fatalf("segment contains %d frames for %s, want one rebuilt frame", got, id)
	}
}

func TestMigrateCompletionPublishesInsideFinalAllFamilyBoundary(t *testing.T) {
	store := openMigrateStore(t)
	id := "rec_completion_boundary"
	seedLoose(t, store, id, "body")

	waiting := make(chan struct{})
	var waitingOnce sync.Once
	store.engine.appendShards.waiting = func(scope string) {
		if scope == allFamiliesLockScope {
			waitingOnce.Do(func() { close(waiting) })
		}
	}

	type removeResult struct {
		err               error
		completionVisible bool
	}
	removeDone := make(chan removeResult, 1)
	var callbackSaw removeResult
	callbackConsumed := false

	report, err := store.Migrate(MigrateOptions{
		Mode: ModeApply,
		afterFinalPresence: func() error {
			go func() {
				removeErr := store.Remove(id)
				_, statErr := os.Stat(store.engine.completionPath())
				removeDone <- removeResult{err: removeErr, completionVisible: statErr == nil}
			}()
			select {
			case callbackSaw = <-removeDone:
				callbackConsumed = true
				return callbackSaw.err
			case <-waiting:
				return nil
			}
		},
	})
	if err != nil {
		t.Fatalf("migrate completion boundary: %v", err)
	}
	if !report.Completion.Complete {
		t.Fatalf("completion did not publish: %v", report.Completion.Reasons)
	}

	removed := callbackSaw
	if !callbackConsumed {
		removed = <-removeDone
	}
	if removed.err != nil && !errors.Is(removed.err, os.ErrNotExist) {
		t.Fatalf("concurrent remove: %v", removed.err)
	}
	if !removed.completionVisible {
		t.Fatal("concurrent Remove completed after final census but before completion.json publication")
	}
}

func TestMigrateFinalizationAcquiresRebaselineBeforeAllFamilies(t *testing.T) {
	store := openMigrateStore(t)
	seedLoose(t, store, "rec_finalization_order", "migration")
	liveID := "tr_rebaseline_transaction_put"

	var finalizationPhase atomic.Bool
	finalizationStarted := make(chan struct{})
	finalizationAllAcquired := make(chan struct{})
	putWaitingOnAllFamilies := make(chan struct{})
	releaseFinalizationAll := make(chan struct{}, 1)
	beginTransactionPut := make(chan struct{}, 1)
	finishTransaction := make(chan struct{}, 1)
	defer func() {
		select {
		case releaseFinalizationAll <- struct{}{}:
		default:
		}
		select {
		case beginTransactionPut <- struct{}{}:
		default:
		}
		select {
		case finishTransaction <- struct{}{}:
		default:
		}
	}()

	var acquiredOnce sync.Once
	store.engine.appendShards.acquired = func(scope string) {
		if scope != allFamiliesLockScope || !finalizationPhase.Load() {
			return
		}
		acquiredOnce.Do(func() {
			close(finalizationAllAcquired)
			<-releaseFinalizationAll
		})
	}
	var waitingOnce sync.Once
	store.engine.appendShards.waiting = func(scope string) {
		if scope == "transition" && finalizationPhase.Load() {
			waitingOnce.Do(func() { close(putWaitingOnAllFamilies) })
		}
	}

	transactionHolding := make(chan struct{})
	transactionPutDone := make(chan error, 1)
	transactionDone := make(chan error, 1)
	go func() {
		transactionDone <- store.WithRebaselineTransaction(func(*RebaselineTransaction) error {
			close(transactionHolding)
			<-beginTransactionPut
			_, err := store.PutJSON(
				liveID,
				eventRecord(liveID, "compound.transition.v0.1", migrateNow, "transaction"),
				"  ",
			)
			transactionPutDone <- err
			<-finishTransaction
			return nil
		})
	}()
	select {
	case <-transactionHolding:
	case err := <-transactionDone:
		t.Fatalf("rebaseline transaction ended before holding its lock: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("rebaseline transaction did not acquire its lock")
	}

	type migrateResult struct {
		report MigrateReport
		err    error
	}
	migrationDone := make(chan migrateResult, 1)
	go func() {
		report, err := store.Migrate(MigrateOptions{
			Mode:      ModeApply,
			BatchSize: 1,
			beforeFinalization: func() error {
				finalizationPhase.Store(true)
				close(finalizationStarted)
				return nil
			},
		})
		migrationDone <- migrateResult{report: report, err: err}
	}()
	select {
	case <-finalizationStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("migration did not reach finalization")
	}

	var first migrateResult
	allFamiliesWon := false
	select {
	case <-finalizationAllAcquired:
		allFamiliesWon = true
	case first = <-migrationDone:
	case <-time.After(5 * time.Second):
		t.Fatal("finalization neither refused the held rebaseline lock nor acquired all families")
	}
	beginTransactionPut <- struct{}{}
	if allFamiliesWon {
		select {
		case <-putWaitingOnAllFamilies:
		case err := <-transactionPutDone:
			t.Fatalf("transaction put completed while finalization held every family: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("transaction put neither waited on nor crossed the finalization boundary")
		}
		select {
		case err := <-transactionPutDone:
			t.Fatalf("transaction put crossed the held all-family boundary early: %v", err)
		default:
		}
		releaseFinalizationAll <- struct{}{}
		select {
		case first = <-migrationDone:
		case <-time.After(5 * time.Second):
			t.Fatal("migration did not release the reversed all-family boundary")
		}
	}
	if first.err == nil {
		t.Fatal("migration finalized while a rebaseline transaction held the boundary")
	}
	select {
	case err := <-transactionPutDone:
		if err != nil {
			t.Fatalf("put inside rebaseline transaction: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("put inside rebaseline transaction did not complete")
	}
	finishTransaction <- struct{}{}
	select {
	case err := <-transactionDone:
		if err != nil {
			t.Fatalf("rebaseline transaction: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rebaseline transaction did not release its lock")
	}
	finalizationPhase.Store(false)

	retry, err := store.Migrate(MigrateOptions{Mode: ModeApply, BatchSize: 1})
	if err != nil {
		t.Fatalf("retry migration after rebaseline transaction: %v", err)
	}
	if !retry.Completion.Complete {
		t.Fatalf("retry did not complete migration: %v", retry.Completion.Reasons)
	}
	if _, err := store.Read(liveID); err != nil {
		t.Fatalf("transaction put is unreadable after migration retry: %v", err)
	}
	if allFamiliesWon {
		t.Fatal("migration finalization acquired all families before refusing the held rebaseline lock")
	}
}
