package eventstore

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// These tests prove the frozen-stream doctor's byte-size baseline (see
// stream-baseline.json / legacyStreamRegrew in migrate.go): a stream's mtime
// alone must no longer flip the doctor degraded once a baseline is recorded,
// growth/shrink of the recorded size must, a governed re-baseline accepts a
// legitimate size change as the new trusted baseline, and a home that
// completed migration before this change (no sidecar) keeps behaving exactly
// as it did before (mtime boundary).

// FALSIFIER F1 (size-robust): a completed home with a sidecar recording a
// nonzero stream size must NOT flag when that stream's mtime is bumped after
// the completion boundary but its byte size is unchanged — a mtime-only
// regression (e.g. a backup restore, a filesystem migration touching mtimes)
// no longer false-reds the doctor.
func TestLegacyStreamRegrewSizeRobustAgainstMTimeBump(t *testing.T) {
	for _, streamName := range []string{hookEventsStreamName, tracesStreamName} {
		t.Run(streamName, func(t *testing.T) {
			store := openMigrateStore(t)
			seedLoose(t, store, "rec_f1", "body-f1")

			streamPath := filepath.Join(store.home, "events", streamName)
			if err := os.WriteFile(streamPath, []byte(`{"id":"pre-existing"}`+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			r, err := store.Migrate(MigrateOptions{Mode: ModeApply})
			if err != nil || !r.Completion.Complete {
				t.Fatalf("apply: %v %v", err, r.Completion.Reasons)
			}

			baseline, ok, err := readStreamBaseline(store.engine.streamBaselinePath())
			if err != nil {
				t.Fatalf("read sidecar: %v", err)
			}
			if !ok {
				t.Fatal("expected writeCompletion to have written the initial sidecar")
			}
			if baseline.Reason != streamBaselineReasonCompletion {
				t.Fatalf("expected reason=%q, got %q", streamBaselineReasonCompletion, baseline.Reason)
			}
			size, ok := baseline.Streams[streamName]
			if !ok || size == 0 {
				t.Fatalf("expected a nonzero recorded size for %s, got streams=%v", streamName, baseline.Streams)
			}

			completionInfo, err := os.Stat(store.engine.completionPath())
			if err != nil {
				t.Fatal(err)
			}
			future := completionInfo.ModTime().Add(time.Hour)
			if err := os.Chtimes(streamPath, future, future); err != nil {
				t.Fatal(err)
			}

			active, err := store.LegacyWriterActive()
			if err != nil {
				t.Fatal(err)
			}
			if active {
				t.Fatalf("%s: mtime bump alone (size unchanged) must not flag once a sidecar records its size", streamName)
			}
			vr, err := store.Migrate(MigrateOptions{Mode: ModeVerify})
			if err != nil {
				t.Fatal(err)
			}
			if !vr.Completion.Complete || vr.Completion.LegacyWriterActive {
				t.Fatalf("verify should report ok despite the mtime bump: complete=%v legacy_writer_active=%v reasons=%v",
					vr.Completion.Complete, vr.Completion.LegacyWriterActive, vr.Completion.Reasons)
			}
		})
	}
}

// FALSIFIER F5 (explicit fallback): a home whose completion gate closed
// BEFORE this change has no stream-baseline.json sidecar at all. It must
// behave exactly as it did before this change: mtime-boundary comparison,
// nothing more.
func TestLegacyStreamRegrewFallsBackToMtimeWhenSidecarAbsent(t *testing.T) {
	store := openMigrateStore(t)
	seedLoose(t, store, "rec_f5", "body-f5")
	if r, err := store.Migrate(MigrateOptions{Mode: ModeApply}); err != nil || !r.Completion.Complete {
		t.Fatalf("apply: %v %v", err, r.Completion.Reasons)
	}

	// Simulate a home that completed before the sidecar existed at all.
	if err := os.Remove(store.engine.streamBaselinePath()); err != nil {
		t.Fatalf("remove sidecar to simulate a pre-change home: %v", err)
	}
	if _, ok, err := readStreamBaseline(store.engine.streamBaselinePath()); err != nil || ok {
		t.Fatalf("sidecar should be absent now: ok=%v err=%v", ok, err)
	}

	completionInfo, err := os.Stat(store.engine.completionPath())
	if err != nil {
		t.Fatal(err)
	}
	streamPath := filepath.Join(store.home, "events", hookEventsStreamName)
	if err := os.WriteFile(streamPath, []byte(`{"id":"x"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	frozen := completionInfo.ModTime().Add(-time.Second)
	if err := os.Chtimes(streamPath, frozen, frozen); err != nil {
		t.Fatal(err)
	}
	if active, err := store.LegacyWriterActive(); err != nil || active {
		t.Fatalf("sidecar-absent + frozen-before-boundary must not flag: active=%v err=%v", active, err)
	}

	future := completionInfo.ModTime().Add(time.Hour)
	if err := os.Chtimes(streamPath, future, future); err != nil {
		t.Fatal(err)
	}
	active, err := store.LegacyWriterActive()
	if err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Fatal("sidecar-absent + written-after-boundary must flag (unchanged mtime behavior)")
	}
}

// FALSIFIERS F2 (re-baseline path) + F3 (teeth after re-baseline), exercised
// directly against the eventstore transaction primitive: a
// completed home whose stream legitimately grew reports degraded; re-baselining
// records the grown size as the new trusted baseline and verify goes back to
// ok; a further append after THAT flips degraded again — re-baselining is not
// a one-time escape hatch, it re-arms the same doctor at the new size.
func TestRebaselineTransactionAcceptsGrowthThenDetectsFurtherGrowth(t *testing.T) {
	store := openMigrateStore(t)
	seedLoose(t, store, "rec_f2", "body-f2")

	streamPath := filepath.Join(store.home, "events", hookEventsStreamName)
	if err := os.WriteFile(streamPath, []byte(`{"id":"initial"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r, err := store.Migrate(MigrateOptions{Mode: ModeApply}); err != nil || !r.Completion.Complete {
		t.Fatalf("apply: %v %v", err, r.Completion.Reasons)
	}

	// Legitimate growth between completion and "now" (e.g. a slow operator
	// catching up a backlog before the freeze truly lands).
	f := openForAppend(t, streamPath)
	if _, err := f.WriteString(`{"id":"grew"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	active, err := store.LegacyWriterActive()
	if err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Fatal("grown stream must flag degraded BEFORE re-baselining")
	}

	applyTestRebaseline(t, store, migrateNow.Add(time.Hour))
	baseline, ok, err := readStreamBaseline(store.engine.streamBaselinePath())
	if err != nil || !ok {
		t.Fatalf("expected a rebaselined sidecar: ok=%v err=%v", ok, err)
	}
	if baseline.Reason != streamBaselineReasonRebaseline {
		t.Fatalf("expected reason=%q after rebaseline, got %q", streamBaselineReasonRebaseline, baseline.Reason)
	}
	info, err := os.Stat(streamPath)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Streams[hookEventsStreamName] != info.Size() {
		t.Fatalf("rebaselined size %d does not match on-disk size %d", baseline.Streams[hookEventsStreamName], info.Size())
	}

	active, err = store.LegacyWriterActive()
	if err != nil {
		t.Fatal(err)
	}
	if active {
		t.Fatal("verify must report ok immediately after accepting the grown size as the new baseline")
	}

	// F3 teeth: a genuine post-rebaseline append flips degraded again — the
	// governed accept is not a permanent silence switch.
	f = openForAppend(t, streamPath)
	if _, err := f.WriteString(`{"id":"post-rebaseline"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	active, err = store.LegacyWriterActive()
	if err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Fatal("an append AFTER the re-baseline must flag degraded again")
	}
}

// The physical transaction refuses (and touches nothing) when migration is not complete.
func TestRebaselineTransactionRefusesWhenMigrationIncomplete(t *testing.T) {
	store := openMigrateStore(t)
	if err := store.WithRebaselineTransaction(func(tx *RebaselineTransaction) error {
		_, _, err := tx.Plan(migrateNow)
		return err
	}); err == nil {
		t.Fatal("expected an error re-baselining a home with no completed migration")
	}
	if _, err := os.Stat(store.engine.streamBaselinePath()); !os.IsNotExist(err) {
		t.Fatalf("no sidecar should have been written: stat err=%v", err)
	}
}

func TestRebaselineTransactionHoldsLockAcrossCallback(t *testing.T) {
	store := openTestStore(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- store.WithRebaselineTransaction(func(*RebaselineTransaction) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	err := store.WithRebaselineTransaction(func(*RebaselineTransaction) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second transaction err=%v, want lock refusal", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPendingRebaselineFailsClosedForBaselineReaders(t *testing.T) {
	store := openMigrateStore(t)
	seedLoose(t, store, "rec_pending_reader", "body")
	if report, err := store.Migrate(MigrateOptions{Mode: ModeApply}); err != nil || !report.Completion.Complete {
		t.Fatalf("apply: report=%+v err=%v", report, err)
	}
	if err := store.WithRebaselineTransaction(func(tx *RebaselineTransaction) error {
		prior, existed, err := tx.ReadSidecar()
		if err != nil {
			return err
		}
		journal := newTestPhysicalRebaselineJournal(t, tx, prior, existed, []byte("post"))
		return tx.WritePending(journal.Raw)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PreviewStreamBaseline(); !errors.Is(err, ErrRebaselineFinalizationPending) {
		t.Fatalf("preview err=%v, want pending sentinel", err)
	}
	if _, err := store.engine.legacyStreamRegrew(); !errors.Is(err, ErrRebaselineFinalizationPending) {
		t.Fatalf("legacy reader err=%v, want pending sentinel", err)
	}
	if _, err := store.Migrate(MigrateOptions{Mode: ModeVerify}); !errors.Is(err, ErrRebaselineFinalizationPending) {
		t.Fatalf("verify err=%v, want pending sentinel", err)
	}
}

func TestRebaselineTransactionCASRollbackRefusesConcurrentState(t *testing.T) {
	store := openTestStore(t)
	path := store.engine.streamBaselinePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("pre"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := store.WithRebaselineTransaction(func(tx *RebaselineTransaction) error {
		journal := newTestPhysicalRebaselineJournal(t, tx, []byte("pre"), true, []byte("post"))
		if err := tx.WritePending(journal.Raw); err != nil {
			return err
		}
		if err := tx.WriteSidecarCAS([]byte("pre"), true, []byte("post")); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte("foreign"), 0o644); err != nil {
			return err
		}
		return tx.RollbackSidecarCAS([]byte("post"), []byte("pre"), true)
	})
	if !errors.Is(err, ErrRebaselineStateConflict) {
		t.Fatalf("rollback err=%v, want conflict", err)
	}
	body, readErr := os.ReadFile(path)
	if readErr != nil || string(body) != "foreign" {
		t.Fatalf("CAS rollback overwrote foreign state: body=%q err=%v", body, readErr)
	}
}

func TestRebaselineTransactionEscapedHandleCannotMutateAfterCallback(t *testing.T) {
	store := openTestStore(t)
	var escaped *RebaselineTransaction
	if err := store.WithRebaselineTransaction(func(tx *RebaselineTransaction) error {
		escaped = tx
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := escaped.WritePending([]byte("escaped")); !errors.Is(err, ErrRebaselineTransactionClosed) {
		t.Fatalf("escaped write err=%v, want closed sentinel", err)
	}
	if _, err := os.Stat(escaped.PendingPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("escaped transaction wrote marker: %v", err)
	}
}

func TestRebaselineTransactionCopiedHandleCannotMutateAfterCallback(t *testing.T) {
	store := openTestStore(t)
	var escaped RebaselineTransaction
	if err := store.WithRebaselineTransaction(func(tx *RebaselineTransaction) error {
		escaped = *tx
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := escaped.WritePending([]byte("copied escape")); !errors.Is(err, ErrRebaselineTransactionClosed) {
		t.Fatalf("copied escaped write err=%v, want closed sentinel", err)
	}
	if _, err := os.Stat(escaped.PendingPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("copied escaped transaction wrote marker: %v", err)
	}
}

func TestRebaselineTransactionMutationsRequireExactPendingMarker(t *testing.T) {
	operations := []struct {
		name    string
		initial string
		run     func(*RebaselineTransaction) error
	}{
		{name: "write", initial: "pre", run: func(tx *RebaselineTransaction) error {
			return tx.WriteSidecarCAS([]byte("pre"), true, []byte("post"))
		}},
		{name: "rollback", initial: "post", run: func(tx *RebaselineTransaction) error {
			return tx.RollbackSidecarCAS([]byte("post"), []byte("pre"), true)
		}},
		{name: "clear", initial: "pre", run: func(tx *RebaselineTransaction) error {
			return tx.ClearPending()
		}},
	}
	markers := []struct {
		name     string
		replaced bool
	}{
		{name: "missing"},
		{name: "replaced", replaced: true},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			for _, marker := range markers {
				t.Run(marker.name, func(t *testing.T) {
					store := openTestStore(t)
					path := store.engine.streamBaselinePath()
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(operation.initial), 0o644); err != nil {
						t.Fatal(err)
					}
					err := store.WithRebaselineTransaction(func(tx *RebaselineTransaction) error {
						if marker.replaced {
							journal := newTestPhysicalRebaselineJournal(t, tx, []byte("pre"), true, []byte("post"))
							if err := tx.WritePending(journal.Raw); err != nil {
								return err
							}
							if err := os.WriteFile(tx.PendingPath(), []byte("foreign pending marker\n"), 0o644); err != nil {
								return err
							}
						}
						return operation.run(tx)
					})
					if !errors.Is(err, ErrRebaselineStateConflict) {
						t.Fatalf("%s err=%v, want pending-marker conflict", operation.name, err)
					}
					body, readErr := os.ReadFile(path)
					if readErr != nil || string(body) != operation.initial {
						t.Fatalf("unjournaled %s mutated sidecar: body=%q err=%v", operation.name, body, readErr)
					}
					pending, exists, readErr := store.ReadRebaselinePending()
					if marker.replaced {
						if readErr != nil || !exists || string(pending) != "foreign pending marker\n" {
							t.Fatalf("%s erased foreign marker: pending=%q exists=%t err=%v", operation.name, pending, exists, readErr)
						}
					} else if readErr != nil || exists {
						t.Fatalf("%s created missing marker: exists=%t err=%v", operation.name, exists, readErr)
					}
				})
			}
		})
	}
}

func TestClearPendingRestoresExactMarkerWhenDirectorySyncFails(t *testing.T) {
	store := openTestStore(t)
	original := syncRebaselineDirectoryFn
	fail := true
	syncRebaselineDirectoryFn = func(path string) error {
		if fail {
			fail = false
			return errors.New("forced pending clear directory sync failure")
		}
		return original(path)
	}
	t.Cleanup(func() { syncRebaselineDirectoryFn = original })
	if err := store.WithRebaselineTransaction(func(tx *RebaselineTransaction) error {
		journal := newTestPhysicalRebaselineJournal(t, tx, nil, false, []byte("post"))
		if err := tx.WritePending(journal.Raw); err != nil {
			return err
		}
		if err := tx.ClearPending(); err == nil {
			t.Fatal("clear unexpectedly succeeded")
		}
		got, exists, err := tx.ReadPending()
		if err != nil || !exists || !reflect.DeepEqual(got, journal.Raw) {
			t.Fatalf("restored marker got=%q exists=%t err=%v", got, exists, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRebaselinePhysicalJournalRejectsArbitraryMarkerBeforeMutation(t *testing.T) {
	store := openTestStore(t)
	path := store.engine.streamBaselinePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("pre"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := store.WithRebaselineTransaction(func(tx *RebaselineTransaction) error {
		if err := tx.WritePending([]byte(`{"schema_version":"arbitrary"}` + "\n")); err != nil {
			return err
		}
		return tx.WriteSidecarCAS([]byte("pre"), true, []byte("post"))
	})
	if err == nil {
		t.Fatal("arbitrary non-empty marker authorized sidecar mutation")
	}
	body, readErr := os.ReadFile(path)
	if readErr != nil || string(body) != "pre" {
		t.Fatalf("arbitrary marker changed sidecar: body=%q err=%v", body, readErr)
	}
}

func TestRebaselinePhysicalJournalWriteIsCreateOnlyAcrossCrash(t *testing.T) {
	store := openTestStore(t)
	var first []byte
	if err := store.WithRebaselineTransaction(func(tx *RebaselineTransaction) error {
		journal := newTestPhysicalRebaselineJournal(t, tx, []byte("pre"), true, []byte("post-one"))
		first = append([]byte(nil), journal.Raw...)
		return tx.WritePending(journal.Raw)
	}); err != nil {
		t.Fatal(err)
	}

	err := store.WithRebaselineTransaction(func(tx *RebaselineTransaction) error {
		replacement := newTestPhysicalRebaselineJournal(t, tx, []byte("pre"), true, []byte("post-two"))
		return tx.WritePending(replacement.Raw)
	})
	if !errors.Is(err, ErrRebaselineStateConflict) {
		t.Fatalf("second journal write err=%v, want state conflict", err)
	}
	got, exists, readErr := store.ReadRebaselinePending()
	if readErr != nil || !exists || !reflect.DeepEqual(got, first) {
		t.Fatalf("crash journal was replaced: got=%q exists=%t err=%v want=%q", got, exists, readErr, first)
	}
}

func TestRebaselinePhysicalJournalRequiresExactProofHash(t *testing.T) {
	store := openTestStore(t)
	err := store.WithRebaselineTransaction(func(tx *RebaselineTransaction) error {
		journal := newTestPhysicalRebaselineJournal(t, tx, []byte("pre"), true, []byte("post"))
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(journal.Raw, &wire); err != nil {
			return err
		}
		var proofs []RebaselinePendingProof
		if err := json.Unmarshal(wire["proofs"], &proofs); err != nil {
			return err
		}
		proofs[0].RecordSHA256 = ""
		proofRaw, err := json.Marshal(proofs)
		if err != nil {
			return err
		}
		wire["proofs"] = proofRaw
		forged, err := json.MarshalIndent(wire, "", "  ")
		if err != nil {
			return err
		}
		return tx.WritePending(append(forged, '\n'))
	})
	if err == nil {
		t.Fatal("physical journal accepted a proof with no exact record hash")
	}
	if _, exists, readErr := store.ReadRebaselinePending(); readErr != nil || exists {
		t.Fatalf("hashless physical journal reached disk: exists=%t err=%v", exists, readErr)
	}
}

func TestRebaselinePhysicalJournalBindsImageArguments(t *testing.T) {
	for _, tc := range []struct {
		name            string
		journalPrior    []byte
		journalExisted  bool
		journalPost     []byte
		expected        []byte
		expectedExisted bool
		replacement     []byte
	}{
		{name: "prior existence", journalPrior: nil, journalExisted: false, journalPost: []byte("post"), expected: []byte("pre"), expectedExisted: true, replacement: []byte("post")},
		{name: "pre image", journalPrior: []byte("other-pre"), journalExisted: true, journalPost: []byte("post"), expected: []byte("pre"), expectedExisted: true, replacement: []byte("post")},
		{name: "post image", journalPrior: []byte("pre"), journalExisted: true, journalPost: []byte("other-post"), expected: []byte("pre"), expectedExisted: true, replacement: []byte("post")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openTestStore(t)
			path := store.engine.streamBaselinePath()
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("pre"), 0o644); err != nil {
				t.Fatal(err)
			}
			err := store.WithRebaselineTransaction(func(tx *RebaselineTransaction) error {
				journal := newTestPhysicalRebaselineJournal(t, tx, tc.journalPrior, tc.journalExisted, tc.journalPost)
				if err := tx.WritePending(journal.Raw); err != nil {
					return err
				}
				return tx.WriteSidecarCAS(tc.expected, tc.expectedExisted, tc.replacement)
			})
			if !errors.Is(err, ErrRebaselineStateConflict) {
				t.Fatalf("image mismatch err=%v, want conflict", err)
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil || string(body) != "pre" {
				t.Fatalf("image mismatch changed sidecar: body=%q err=%v", body, readErr)
			}
		})
	}
}

func TestRebaselinePhysicalJournalRequiresProofAbsenceForMutation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		initial   []byte
		putProof  func(*testing.T, *Store, testPhysicalRebaselineJournal)
		operation func(*RebaselineTransaction) error
	}{
		{name: "write receipt present", initial: []byte("pre"), putProof: func(t *testing.T, store *Store, journal testPhysicalRebaselineJournal) {
			putTestPhysicalProof(t, store, journal.ReceiptID, journal.ReceiptRaw)
		}, operation: func(tx *RebaselineTransaction) error { return tx.WriteSidecarCAS([]byte("pre"), true, []byte("post")) }},
		{name: "rollback transition present", initial: []byte("post"), putProof: func(t *testing.T, store *Store, journal testPhysicalRebaselineJournal) {
			putTestPhysicalProof(t, store, journal.TransitionID, journal.TransitionRaw)
		}, operation: func(tx *RebaselineTransaction) error {
			return tx.RollbackSidecarCAS([]byte("post"), []byte("pre"), true)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openTestStore(t)
			path := store.engine.streamBaselinePath()
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, tc.initial, 0o644); err != nil {
				t.Fatal(err)
			}
			err := store.WithRebaselineTransaction(func(tx *RebaselineTransaction) error {
				journal := newTestPhysicalRebaselineJournal(t, tx, []byte("pre"), true, []byte("post"))
				if err := tx.WritePending(journal.Raw); err != nil {
					return err
				}
				tc.putProof(t, store, journal)
				return tc.operation(tx)
			})
			if !errors.Is(err, ErrRebaselineStateConflict) {
				t.Fatalf("proof-bearing mutation err=%v, want conflict", err)
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil || !bytesEqual(body, tc.initial) {
				t.Fatalf("proof-bearing mutation changed sidecar: body=%q err=%v", body, readErr)
			}
		})
	}
}

func TestRebaselinePhysicalJournalClearRequiresTerminalState(t *testing.T) {
	for _, tc := range []struct {
		name          string
		current       []byte
		putReceipt    bool
		putTransition bool
		wantOK        bool
	}{
		{name: "pre with receipt", current: []byte("pre"), putReceipt: true},
		{name: "post missing transition", current: []byte("post"), putReceipt: true},
		{name: "post missing receipt", current: []byte("post"), putTransition: true},
		{name: "post exact proofs", current: []byte("post"), putReceipt: true, putTransition: true, wantOK: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openTestStore(t)
			path := store.engine.streamBaselinePath()
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, tc.current, 0o644); err != nil {
				t.Fatal(err)
			}
			var marker []byte
			err := store.WithRebaselineTransaction(func(tx *RebaselineTransaction) error {
				journal := newTestPhysicalRebaselineJournal(t, tx, []byte("pre"), true, []byte("post"))
				marker = append([]byte(nil), journal.Raw...)
				if err := tx.WritePending(journal.Raw); err != nil {
					return err
				}
				if tc.putReceipt {
					putTestPhysicalProof(t, store, journal.ReceiptID, journal.ReceiptRaw)
				}
				if tc.putTransition {
					putTestPhysicalProof(t, store, journal.TransitionID, journal.TransitionRaw)
				}
				return tx.ClearPending()
			})
			if tc.wantOK {
				if err != nil {
					t.Fatalf("exact terminal clear: %v", err)
				}
				if _, exists, readErr := store.ReadRebaselinePending(); readErr != nil || exists {
					t.Fatalf("exact terminal clear retained marker: exists=%t err=%v", exists, readErr)
				}
				return
			}
			if !errors.Is(err, ErrRebaselineStateConflict) {
				t.Fatalf("nonterminal clear err=%v, want conflict", err)
			}
			got, exists, readErr := store.ReadRebaselinePending()
			if readErr != nil || !exists || !reflect.DeepEqual(got, marker) {
				t.Fatalf("nonterminal clear lost marker: got=%q exists=%t err=%v", got, exists, readErr)
			}
		})
	}
}

func TestRebaselinePhysicalJournalMalformedReadPreservesRaw(t *testing.T) {
	store := openTestStore(t)
	raw := []byte(`{"schema_version":"broken","receipt":`)
	if err := store.WithRebaselineTransaction(func(tx *RebaselineTransaction) error {
		if err := os.WriteFile(tx.PendingPath(), raw, 0o644); err != nil {
			return err
		}
		got, exists, err := tx.ReadPending()
		if err == nil || !exists || !reflect.DeepEqual(got, raw) {
			t.Fatalf("malformed read got=%q exists=%t err=%v", got, exists, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationCompletionPublishesBaselineBeforeCompletionUnderRebaselineLock(t *testing.T) {
	store := openMigrateStore(t)
	seedLoose(t, store, "rec_completion_barrier", "body")
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := store.Migrate(MigrateOptions{Mode: ModeApply, beforeInitialBaseline: func() error {
			close(entered)
			<-release
			return nil
		}})
		done <- err
	}()
	<-entered
	if store.MigrationComplete() {
		t.Fatal("completion became visible before initial baseline publication")
	}
	if err := store.WithRebaselineTransaction(func(*RebaselineTransaction) error { return nil }); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second rebaseline transaction err=%v, want lock refusal", err)
	}
	appendDone := make(chan error, 1)
	go func() {
		appendDone <- store.AppendHookEvent(map[string]string{"kind": "in-flight-before-completion"})
	}()
	select {
	case err := <-appendDone:
		t.Fatalf("frozen-stream append crossed the completion boundary lock before release: %v", err)
	case <-time.After(100 * time.Millisecond):
		// The append must block on the same boundary lock until completion is visible.
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-appendDone; err != nil {
		t.Fatalf("blocked append should return successfully after completion: %v", err)
	}
	if !store.MigrationComplete() {
		t.Fatal("completion not published after baseline initialization")
	}
	if _, ok, err := readStreamBaseline(store.engine.streamBaselinePath()); err != nil || !ok {
		t.Fatalf("initial baseline missing after completion: ok=%t err=%v", ok, err)
	}
	if _, err := os.Stat(filepath.Join(store.home, "events", hookEventsStreamName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("frozen stream created after completion: %v", err)
	}
}

func TestMigrationCompletionRetryRefreshesProvisionalCompletionBaseline(t *testing.T) {
	store := openMigrateStore(t)
	seedLoose(t, store, "rec_completion_retry", "body")
	streamPath := filepath.Join(store.home, "events", hookEventsStreamName)
	if err := os.WriteFile(streamPath, []byte(`{"id":"before-provisional"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.engine.writeInitialStreamBaseline(migrateNow.Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("write provisional completion baseline: %v", err)
	}
	if store.MigrationComplete() {
		t.Fatal("provisional baseline unexpectedly published completion")
	}
	if err := store.AppendHookEvent(map[string]string{"kind": "grew-before-completion-retry"}); err != nil {
		t.Fatal(err)
	}
	wantInfo, err := os.Stat(streamPath)
	if err != nil {
		t.Fatal(err)
	}
	report, err := store.Migrate(MigrateOptions{Mode: ModeApply})
	if err != nil || !report.Completion.Complete {
		t.Fatalf("completion retry: report=%+v err=%v", report, err)
	}
	baseline, ok, err := readStreamBaseline(store.engine.streamBaselinePath())
	if err != nil || !ok {
		t.Fatalf("read refreshed completion baseline: ok=%t err=%v", ok, err)
	}
	if got := baseline.Streams[hookEventsStreamName]; got != wantInfo.Size() {
		t.Fatalf("retry left stale provisional baseline size=%d, want current=%d", got, wantInfo.Size())
	}
	if active, err := store.LegacyWriterActive(); err != nil || active {
		t.Fatalf("freshly completed retry must remain healthy: active=%t err=%v", active, err)
	}
}

func TestMigrationCompletionRefusesNonCompletionBaseline(t *testing.T) {
	for _, tc := range []struct {
		name     string
		baseline StreamBaseline
		wantErr  string
	}{
		{name: "governed reason", baseline: StreamBaseline{Schema: streamBaselineSchema, Reason: streamBaselineReasonRebaseline, Streams: map[string]int64{}}, wantErr: "reason"},
		{name: "wrong schema", baseline: StreamBaseline{Schema: "unexpected.stream-baseline", Reason: streamBaselineReasonCompletion, Streams: map[string]int64{}}, wantErr: "schema"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openMigrateStore(t)
			seedLoose(t, store, "rec_completion_refuse", "body")
			path := store.engine.streamBaselinePath()
			tc.baseline.RecordedAt = migrateNow.Format(time.RFC3339Nano)
			if err := writeStreamBaselineAtomic(path, tc.baseline); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Migrate(MigrateOptions{Mode: ModeApply}); err == nil || !errors.Is(err, ErrRebaselineStateConflict) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("completion accepted conflicting baseline: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("completion overwrote conflicting baseline: err=%v", err)
			}
			if store.MigrationComplete() {
				t.Fatal("completion published despite baseline conflict")
			}
		})
	}
}

func applyTestRebaseline(t *testing.T, store *Store, now time.Time) {
	t.Helper()
	if err := store.WithRebaselineTransaction(func(tx *RebaselineTransaction) error {
		prior, existed, err := tx.ReadSidecar()
		if err != nil {
			return err
		}
		_, post, err := tx.Plan(now)
		if err != nil {
			return err
		}
		journal := newTestPhysicalRebaselineJournal(t, tx, prior, existed, post)
		if err := tx.WritePending(journal.Raw); err != nil {
			return err
		}
		if err := tx.WriteSidecarCAS(prior, existed, post); err != nil {
			return err
		}
		putTestPhysicalProof(t, store, journal.ReceiptID, journal.ReceiptRaw)
		putTestPhysicalProof(t, store, journal.TransitionID, journal.TransitionRaw)
		return tx.ClearPending()
	}); err != nil {
		t.Fatalf("rebaseline primitive: %v", err)
	}
}

type testPhysicalRebaselineJournal struct {
	Raw           []byte
	ReceiptID     string
	ReceiptRaw    []byte
	TransitionID  string
	TransitionRaw []byte
}

func newTestPhysicalRebaselineJournal(t *testing.T, tx *RebaselineTransaction, prior []byte, priorExisted bool, post []byte) testPhysicalRebaselineJournal {
	t.Helper()
	preRef := fmt.Sprintf("artifact:sha256:%x", sha256.Sum256(prior))
	postRef := fmt.Sprintf("artifact:sha256:%x", sha256.Sum256(post))
	const workID = "work_physical_rebaseline"
	const actionID = "act_physical_rebaseline"
	const receiptID = "rec_physical_rebaseline"
	const transitionID = "tr_physical_rebaseline"
	receiptRaw, err := json.Marshal(struct {
		SchemaVersion string `json:"schema_version"`
		ID            string `json:"id"`
		IDs           struct {
			WorkID string `json:"work_id"`
		} `json:"ids"`
		StateDelta struct {
			Artifacts []string `json:"artifacts"`
		} `json:"state_delta"`
		ProofRefs []string `json:"proof_refs"`
	}{SchemaVersion: "compound.receipt.v0.1", ID: receiptID, IDs: struct {
		WorkID string `json:"work_id"`
	}{WorkID: workID}, StateDelta: struct {
		Artifacts []string `json:"artifacts"`
	}{Artifacts: []string{preRef, postRef}}, ProofRefs: []string{preRef, postRef}})
	if err != nil {
		t.Fatal(err)
	}
	transitionRaw, err := json.Marshal(struct {
		SchemaVersion string `json:"schema_version"`
		ID            string `json:"id"`
		IDs           struct {
			WorkID string `json:"work_id"`
		} `json:"ids"`
		BeforeRefs []string `json:"before_refs"`
		ActionRefs []string `json:"action_refs"`
		AfterRefs  []string `json:"after_refs"`
	}{SchemaVersion: "compound.transition.v0.1", ID: transitionID, IDs: struct {
		WorkID string `json:"work_id"`
	}{WorkID: workID}, BeforeRefs: []string{preRef}, ActionRefs: []string{actionID}, AfterRefs: []string{postRef}})
	if err != nil {
		t.Fatal(err)
	}
	receiptProof, err := NewRebaselinePendingProof(receiptID, receiptRaw)
	if err != nil {
		t.Fatal(err)
	}
	transitionProof, err := NewRebaselinePendingProof(transitionID, transitionRaw)
	if err != nil {
		t.Fatal(err)
	}
	authorityBundle, err := NewRebaselinePendingAuthorityBundle([]byte(`{"opaque":"test authority"}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(struct {
		SchemaVersion     string                           `json:"schema_version"`
		SidecarPath       string                           `json:"sidecar_path"`
		PriorExisted      bool                             `json:"prior_existed"`
		PreImageRef       string                           `json:"pre_image_ref"`
		PostImageRef      string                           `json:"post_image_ref"`
		BeforeEvidenceRef string                           `json:"before_evidence_ref"`
		AfterEvidenceRef  string                           `json:"after_evidence_ref"`
		AuthorityBundle   RebaselinePendingAuthorityBundle `json:"authority_bundle"`
		Proofs            []RebaselinePendingProof         `json:"proofs"`
	}{SchemaVersion: "compound.eventstore.rebaseline_pending.v0.1", SidecarPath: tx.SidecarPath(), PriorExisted: priorExisted, PreImageRef: preRef, PostImageRef: postRef, BeforeEvidenceRef: "eventstore-baseline-before:test", AfterEvidenceRef: "eventstore-baseline-after:test", AuthorityBundle: authorityBundle, Proofs: []RebaselinePendingProof{receiptProof, transitionProof}}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return testPhysicalRebaselineJournal{Raw: append(raw, '\n'), ReceiptID: receiptID, ReceiptRaw: receiptRaw, TransitionID: transitionID, TransitionRaw: transitionRaw}
}

func putTestPhysicalProof(t *testing.T, store *Store, id string, raw []byte) {
	t.Helper()
	if _, err := store.PutJSON(id, json.RawMessage(raw), " "); err != nil {
		t.Fatal(err)
	}
}

func bytesEqual(a, b []byte) bool { return reflect.DeepEqual(a, b) }

// FALSIFIER (R5B predicate): the rebaseline pending journal must persist
// (a) whether the prior sidecar existed, and (b) an artifact ref whose body
// is the EXACT prior sidecar bytes (so a next-invocation "resume rollback"
// can execute tx.RollbackSidecarCAS against the restored prior without any
// in-memory prior bytes from the original caller). The predicate is RED if
// either PriorExisted is wrong or rebaselineArtifactRef(prior) != journal
// PreImageRef. The migrated store below provides the completion gate Plan
// requires; openTestStore alone fails this gate.
func TestRebaselineJournalCapturesPreImageForExecutableRollback(t *testing.T) {
	store := openMigrateStore(t)
	seedLoose(t, store, "rec_prerebase", "body")
	if r, err := store.Migrate(MigrateOptions{Mode: ModeApply}); err != nil || !r.Completion.Complete {
		t.Fatalf("apply: %v %v", err, r.Completion.Reasons)
	}
	prior := []byte(`{"prior":"bytes","timestamp":"2026-07-16T00:00:00Z"}` + "\n")
	path := store.engine.streamBaselinePath()
	if err := os.WriteFile(path, prior, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.WithRebaselineTransaction(func(tx *RebaselineTransaction) error {
		_, post, err := tx.Plan(migrateNow)
		if err != nil {
			return err
		}
		journal := newTestPhysicalRebaselineJournal(t, tx, prior, true, post)
		return tx.WritePending(journal.Raw)
	}); err != nil {
		t.Fatal(err)
	}
	raw, exists, err := store.ReadRebaselinePending()
	if err != nil || !exists {
		t.Fatalf("journal missing: exists=%v err=%v", exists, err)
	}
	var wire rebaselinePhysicalJournalWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if !wire.PriorExisted {
		t.Fatalf("journal PriorExisted=false, want true (a prior sidecar was written)")
	}
	if wire.PreImageRef == "" || wire.PreImageRef == rebaselineArtifactRef(nil) {
		t.Fatalf("journal PreImageRef is empty/nil: %q", wire.PreImageRef)
	}
	if wire.PreImageRef != rebaselineArtifactRef(prior) {
		t.Fatalf("journal PreImageRef=%q does not match rebaselineArtifactRef(prior)=%q\nthe pre-image must round-trip via the journal alone", wire.PreImageRef, rebaselineArtifactRef(prior))
	}
}

// FALSIFIER (R5B predicate): rollback from the journal alone — given only the
// pending journal + tx, executing tx.RollbackSidecarCAS(post, prior, true)
// against a sidecar that holds the post-image must restore the prior bytes
// (or, if priorExisted=false, remove the sidecar). The test verifies the
// existing primitive already powers this — green-at-birth. The predicate
// marker the packet names is "RollbackPlan is executable from the journal
// alone": the rebaselinePhysicalJournal binds the prior and post refs so
// requireImageBinding enforces (post, prior, priorExisted) exactness.
func TestRebaselineRollsBackSidecarWhenReceiptRecordingFails(t *testing.T) {
	store := openMigrateStore(t)
	seedLoose(t, store, "rec_prerebase_rollback", "body")
	if r, err := store.Migrate(MigrateOptions{Mode: ModeApply}); err != nil || !r.Completion.Complete {
		t.Fatalf("apply: %v %v", err, r.Completion.Reasons)
	}
	prior := []byte(`{"prior":"rollback-baseline"}` + "\n")
	path := store.engine.streamBaselinePath()
	if err := os.WriteFile(path, prior, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.WithRebaselineTransaction(func(tx *RebaselineTransaction) error {
		_, post, err := tx.Plan(migrateNow)
		if err != nil {
			return err
		}
		journal := newTestPhysicalRebaselineJournal(t, tx, prior, true, post)
		if err := tx.WritePending(journal.Raw); err != nil {
			return err
		}
		if err := tx.WriteSidecarCAS(prior, true, post); err != nil {
			return err
		}
		// The post-write receipt-recording failure is injected at the verb
		// boundary (cmd/compound/failReceiptRecordsBeforePut). The physical
		// rollback from the journal-only inputs is the eventstore-side path
		// exercised here.
		if err := tx.RollbackSidecarCAS(post, prior, true); err != nil {
			return err
		}
		return tx.ClearPending()
	}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(prior) {
		t.Fatalf("journal-only rollback did not restore prior bytes: got=%q want=%q", body, prior)
	}
}

func openForAppend(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
