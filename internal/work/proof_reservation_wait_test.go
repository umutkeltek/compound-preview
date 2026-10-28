package work

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/runattempt"
)

const queuedProofOtherHolder = "claude_code:other-lane:work_other"

func queuedProofInput(t *testing.T) ProofResourceReserveInput {
	t.Helper()
	profile, err := NewProofResourceProfile(ProofResourceProfileCanonicalFull, "/synthetic/repo-alpha", "", []ProofResourceGateProfile{{
		Name:     "synthetic-gate-a",
		Command:  []string{"go", "test", "./internal/synthetic-a"},
		ProofRef: "synthetic-proof-a",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       5 * time.Minute,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --wait 30s --out /synthetic/report-alpha.json",
		Profile:   profile,
	}
}

// A plain reserve against this lease refuses and leaves an abandoned RunAttempt.
// Queued, the reserve waits out the other lane and ends with exactly one attempt,
// running, and names the holder it waited on.
func TestReserveProofResourceWhenFreeQueuesInsteadOfAbandoningAnAttempt(t *testing.T) {
	s := newStore(t)
	now := time.Now().UTC()
	w := claimedProofWork(t, s, "queued proof work", proofActorAlpha, now)
	if _, err := landlease.Acquire(s.home, queuedProofOtherHolder, time.Hour, now); err != nil {
		t.Fatal(err)
	}
	before := runAttemptCount(t, s, w.ID)
	released := make(chan error, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		_, err := landlease.Release(s.home, queuedProofOtherHolder, time.Now())
		released <- err
	}()

	var progress strings.Builder
	result, err := s.ReserveProofResourceWhenFree(context.Background(), w.ID, queuedProofInput(t), 30*time.Second, landlease.RealClock{}, &progress)
	if releaseErr := <-released; releaseErr != nil {
		t.Fatal(releaseErr)
	}
	if err != nil {
		t.Fatalf("queued reserve: %v", err)
	}
	if err := closeProofFixtureExecutionBarrier(result.ExecutionBarrier); err != nil {
		t.Fatal(err)
	}
	if result.Reservation.Status != ProofResourceAcquired {
		t.Fatalf("reservation = %+v, want acquired", result.Reservation)
	}
	assertProofRunAttempt(t, s, result.Reservation, runattempt.StatusRunning)
	if added := runAttemptCount(t, s, w.ID) - before; added != 1 {
		t.Fatalf("reserve added %d run attempts, want only the acquired one", added)
	}
	samples, timingErr := ReadProofResourceTimings(s.home, "", 20)
	if timingErr != nil || len(samples) != 1 || samples[0].QueueWaitMS == nil || *samples[0].QueueWaitMS <= 0 || samples[0].AdmissionMS == nil || *samples[0].AdmissionMS < *samples[0].QueueWaitMS {
		t.Fatalf("queued admission timings = %+v, error %v", samples, timingErr)
	}
	if !strings.Contains(progress.String(), queuedProofOtherHolder) {
		t.Fatalf("progress %q does not name the holder it waited on", progress.String())
	}
}

func TestReserveProofResourceWhenFreeTimesOutWithoutMintingAnAttempt(t *testing.T) {
	s := newStore(t)
	now := time.Now().UTC()
	w := claimedProofWork(t, s, "queued proof work", proofActorAlpha, now)
	if _, err := landlease.Acquire(s.home, queuedProofOtherHolder, time.Hour, now); err != nil {
		t.Fatal(err)
	}
	before := runAttemptCount(t, s, w.ID)
	_, err := s.ReserveProofResourceWhenFree(context.Background(), w.ID, queuedProofInput(t), time.Second, landlease.RealClock{}, nil)
	if err == nil || !strings.Contains(err.Error(), queuedProofOtherHolder) {
		t.Fatalf("want a wait timeout naming the holder, got %v", err)
	}
	if added := runAttemptCount(t, s, w.ID) - before; added != 0 {
		t.Fatalf("a timed-out wait minted %d run attempts", added)
	}
}

// Lived 2026-09-13: another battery was mid-reservation (holding the admission lock,
// lease not yet written) when the waiter looked, so the lease read free; the waiter
// then queued on that lock and minted an attempt that was refused. The lease must be
// re-checked under the lock, so the only attempt is the one that finally acquires.
func TestReserveProofResourceWhenFreeWaitsOutAReservationInFlight(t *testing.T) {
	s := newStore(t)
	now := time.Now().UTC()
	w := claimedProofWork(t, s, "queued proof work", proofActorAlpha, now)
	before := runAttemptCount(t, s, w.ID)

	inside := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		err := home.WithAutonomyAdmissionLock(s.home, func() error {
			close(inside)
			time.Sleep(300 * time.Millisecond) // the waiter reads the lease as free meanwhile
			_, err := landlease.Acquire(s.home, queuedProofOtherHolder, time.Hour, time.Now())
			return err
		})
		if err == nil {
			time.Sleep(300 * time.Millisecond)
			_, err = landlease.Release(s.home, queuedProofOtherHolder, time.Now())
		}
		done <- err
	}()
	<-inside

	var progress strings.Builder
	result, err := s.ReserveProofResourceWhenFree(context.Background(), w.ID, queuedProofInput(t), 30*time.Second, landlease.RealClock{}, &progress)
	if otherErr := <-done; otherErr != nil {
		t.Fatal(otherErr)
	}
	if err != nil {
		t.Fatalf("queued reserve: %v", err)
	}
	if err := closeProofFixtureExecutionBarrier(result.ExecutionBarrier); err != nil {
		t.Fatal(err)
	}
	if added := runAttemptCount(t, s, w.ID) - before; added != 1 {
		t.Fatalf("reserve added %d run attempts, want only the acquired one", added)
	}
	assertProofRunAttempt(t, s, result.Reservation, runattempt.StatusRunning)
	if !strings.Contains(progress.String(), queuedProofOtherHolder) {
		t.Fatalf("progress %q does not name the in-flight holder", progress.String())
	}
}

// A queued battery must stop on SIGTERM/Ctrl-C (its cancelled context) without
// reserving; otherwise it takes the lease the moment it frees and records a run.
func TestReserveProofResourceWhenFreeStopsWhenCancelled(t *testing.T) {
	s := newStore(t)
	now := time.Now().UTC()
	w := claimedProofWork(t, s, "queued proof work", proofActorAlpha, now)
	if _, err := landlease.Acquire(s.home, queuedProofOtherHolder, time.Hour, now); err != nil {
		t.Fatal(err)
	}
	before := runAttemptCount(t, s, w.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := s.ReserveProofResourceWhenFree(ctx, w.ID, queuedProofInput(t), time.Hour, landlease.RealClock{}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled queue returned %v, want the context error", err)
	}
	if waited := time.Since(started); waited > 2*proofResourceWaitPoll {
		t.Fatalf("cancellation took %s", waited)
	}
	if added := runAttemptCount(t, s, w.ID) - before; added != 0 {
		t.Fatalf("a cancelled wait minted %d run attempts", added)
	}
}

func runAttemptCount(t *testing.T, s *Store, workID string) int {
	t.Helper()
	w, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	return len(w.RunAttemptIDs)
}

func TestReserveProofResourceWhenFreeCancelledVacancyMintsNothing(t *testing.T) {
	s := newStore(t)
	w := claimedProofWork(t, s, "cancelled free proof resource", proofActorAlpha, time.Now().UTC())
	before := runAttemptCount(t, s, w.ID)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := s.ReserveProofResourceWhenFree(ctx, w.ID, queuedProofInput(t), time.Minute, landlease.RealClock{}, nil)
	if result.ExecutionBarrier != nil {
		_ = closeProofFixtureExecutionBarrier(result.ExecutionBarrier)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled free queue returned %v", err)
	}
	if added := runAttemptCount(t, s, w.ID) - before; added != 0 {
		t.Errorf("cancelled queue minted %d attempts", added)
	}
}
