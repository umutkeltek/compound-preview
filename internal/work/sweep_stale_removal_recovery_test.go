package work

import (
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

// TestSweepStalePendingClearResumesAfterQuarantinedRemoval pins REL-P0-004
// residual (c): a crash inside the physical removal of the frozen sweep frame
// leaves the home layer's quarantine shape (canonical gone, one
// .remove-pending residue). The rerun must reconcile that residue and finish
// the transaction -- exactly one terminal state, one new event with the
// source prefix intact, lease already released, and no orphaned quarantine.
func TestSweepStalePendingClearResumesAfterQuarantinedRemoval(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := staleEnvelope(t, s, "claimed", 30*24*time.Hour, fixedNow)
	beforeEvents, err := s.eventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Freeze the frame and drive the transaction up to (not through) the
	// pending clear, so the frozen frame is on disk in its durable shape.
	fired := false
	injected := errors.New("injected " + sweepStaleFaultBeforePendingClear)
	s.sweepStaleFault = func(stage string) error {
		if stage == sweepStaleFaultBeforePendingClear && !fired {
			fired = true
			return injected
		}
		return nil
	}
	failed, err := s.SweepStaleApply(72*time.Hour, newFakeProbe(), fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	s.sweepStaleFault = nil
	if !fired {
		t.Fatal("fault stage before_pending_clear was not reached")
	}
	if failed.Reclaimed != 0 || failed.Skipped != 1 || len(failed.Candidates) != 1 {
		t.Fatalf("failed report = %+v, want one skipped candidate", failed)
	}

	// Simulate the crash inside the physical removal: the durable frame is
	// quarantined (renamed to its .remove-pending residue) and the canonical
	// name is gone. This is the exact shape an interrupted
	// home.RemoveRootedRegularFileDurableValidated leaves behind.
	pendingPath := s.sweepStalePendingPath(w.ID)
	quarantinePath := pendingPath + ".remove-pending"
	if err := os.Rename(pendingPath, quarantinePath); err != nil {
		t.Fatal(err)
	}

	recovered, err := s.SweepStaleApply(72*time.Hour, newFakeProbe(), fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Reclaimed != 1 || len(recovered.Candidates) != 1 || !recovered.Candidates[0].Reclaimed {
		t.Fatalf("recovery report = %+v, want one exact completed reclaim", recovered)
	}
	current, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != "cancelled" {
		t.Fatalf("recovered state = %q, want cancelled", current.State)
	}
	finalEvents, err := s.eventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(finalEvents) != len(beforeEvents)+1 || !reflect.DeepEqual(finalEvents[:len(beforeEvents)], beforeEvents) {
		t.Fatalf("recovered event history = %+v, want unchanged prefix plus one event", finalEvents)
	}
	if _, err := s.readLease(w.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lease after recovery: %v", err)
	}
	if _, err := os.Stat(pendingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending frame after recovery: %v", err)
	}
	if _, err := os.Lstat(quarantinePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("quarantined pending frame was orphaned instead of reconciled by the rerun")
	}
}
