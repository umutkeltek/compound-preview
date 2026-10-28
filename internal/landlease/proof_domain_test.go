package landlease

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func proofDomainFixture(t *testing.T) ([]string, string, string) {
	t.Helper()
	backing, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	homes := []string{t.TempDir(), t.TempDir()}
	for i, home := range homes {
		homes[i], err = filepath.EvalSymlinks(home)
		if err != nil {
			t.Fatal(err)
		}
		if err := MigrateLegacyLayout(homes[i], backing, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(homes)
	generation := ProofDomainMigrationGeneration(backing, homes)
	if err := MigrateProofDomain(backing, homes, func() error { return nil }, nil, ProofDomainFencedV3); err != nil {
		t.Fatal(err)
	}
	return homes, backing, generation
}
func beginDomainTicket(t *testing.T, home string, class ProofCapacityClass, repo string) *ProofAdmission {
	t.Helper()
	identity, err := ProofCoordinationFor(home, class, strings.Repeat(repo, 64))
	if err != nil {
		t.Fatal(err)
	}
	if identity == nil {
		t.Fatal("fixture not coordinated")
	}
	ticket, err := BeginProofAdmission(home, *identity, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ticket.Cancel(); err != nil {
			t.Error(err)
		}
	})
	return ticket
}
func acquireDomainProof(t *testing.T, ticket *ProofAdmission, holder string) (*Resource, *ExecutionBarrier) {
	t.Helper()
	var resource *Resource
	var barrier *ExecutionBarrier
	err := ticket.WithTurn(context.Background(), false, func(r *Resource) error {
		resource = r
		_, b, err := r.AcquireWithExecutionBarrierPrepared(holder, time.Minute, time.Now(), func(AcquireOutcome) error { return nil })
		barrier = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ticket.Cancel(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = barrier.Close() })
	return resource, barrier
}
func TestProofDomainTwoClassesAcrossHomesAndSameRepositoryExclusion(t *testing.T) {
	homes, _, _ := proofDomainFixture(t)
	first := beginDomainTicket(t, homes[0], ProofCapacitySystemFull, "2")
	resource, barrier := acquireDomainProof(t, first, "capacity-first")
	independent := beginDomainTicket(t, homes[1], ProofCapacityNativeConsumer, "3")
	other, otherBarrier := acquireDomainProof(t, independent, "capacity-independent")
	// Operator-approved capacity: system_full has two slots, so a second
	// repository is admitted into slot 1 and a third is refused.
	sameClass := beginDomainTicket(t, homes[1], ProofCapacitySystemFull, "4")
	sameRepo := beginDomainTicket(t, homes[1], ProofCapacityNativeConsumer, "2")
	secondSlot, secondSlotBarrier := acquireDomainProof(t, sameClass, "capacity-second-slot")
	if identity := secondSlot.ProofIdentity(); identity == nil || identity.Slot != 1 {
		t.Fatalf("second system_full repository not in slot 1: %+v", identity)
	}
	thirdClass := beginDomainTicket(t, homes[0], ProofCapacitySystemFull, "5")
	for _, ticket := range []*ProofAdmission{thirdClass, sameRepo} {
		called := false
		err := ticket.WithTurn(context.Background(), false, func(*Resource) error { called = true; return nil })
		var busy *ProofAdmissionBusyError
		if called || !errors.As(err, &busy) {
			t.Fatalf("conflict admitted: called=%v err=%v", called, err)
		}
	}
	if err := otherBarrier.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Release("capacity-independent", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Same repository remains occupied even when the other capacity is free.
	called := false
	err := sameRepo.WithTurn(context.Background(), false, func(*Resource) error { called = true; return nil })
	if err == nil || called {
		t.Fatalf("cross-class repository exclusion lost: %v", err)
	}
	if err := barrier.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := resource.Release("capacity-first", time.Now()); err != nil {
		t.Fatal(err)
	}
	releaseDomainProof(t, secondSlot, secondSlotBarrier, "capacity-second-slot")
	if err := thirdClass.Cancel(); err != nil {
		t.Fatal(err)
	}
	_, _ = acquireDomainProof(t, sameRepo, "repository-after-release")
}
func TestProofDomainExpiredOtherClassStillNeedsExactRecovery(t *testing.T) {
	homes, backing, generation := proofDomainFixture(t)
	ticket := beginDomainTicket(t, homes[0], ProofCapacitySystemFull, "2")
	resource, barrier := acquireDomainProof(t, ticket, "expired-first")
	if err := barrier.Close(); err != nil {
		t.Fatal(err)
	}
	root := proofClassRoot(backing, generation, ProofCapacitySystemFull)
	path, _ := statePath(root)
	lease, _, err := readLease(path)
	if err != nil {
		t.Fatal(err)
	}
	lease.AcquiredAt = baseNow.Format(time.RFC3339Nano)
	lease.ExpiresAt = baseNow.Add(time.Minute).Format(time.RFC3339Nano)
	if err := writeLeaseAtomic(path, *lease); err != nil {
		t.Fatal(err)
	}
	next := beginDomainTicket(t, homes[1], ProofCapacityNativeConsumer, "2")
	called := false
	if err := next.WithTurn(context.Background(), false, func(*Resource) error { called = true; return nil }); err == nil || called {
		t.Fatalf("expired repository owner bypassed: %v", err)
	}
	if _, err := resource.Release("expired-first", time.Now()); err != nil {
		t.Fatal(err)
	}
	_, _ = acquireDomainProof(t, next, "after-exact-recovery")
}
func TestProofDomainPreparationFailureClosesBothBarriersAndTicket(t *testing.T) {
	homes, _, _ := proofDomainFixture(t)
	first := beginDomainTicket(t, homes[0], ProofCapacitySystemFull, "2")
	failure := errors.New("prepare refused")
	err := first.WithTurn(context.Background(), false, func(r *Resource) error {
		_, barrier, err := r.AcquireWithExecutionBarrierPrepared("failed", time.Minute, time.Now(), func(AcquireOutcome) error { return failure })
		if barrier != nil {
			t.Error("failed preparation returned barrier")
			_ = barrier.Close()
		}
		return err
	})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := first.Cancel(); err != nil {
		t.Fatal(err)
	}
	next := beginDomainTicket(t, homes[1], ProofCapacityNativeConsumer, "2")
	_, _ = acquireDomainProof(t, next, "after-failed-prepare")
}
func TestProofDomainEscapedAdmissionCannotAcquire(t *testing.T) {
	homes, _, _ := proofDomainFixture(t)
	ticket := beginDomainTicket(t, homes[0], ProofCapacitySystemFull, "2")
	var escaped *Resource
	if err := ticket.WithTurn(context.Background(), false, func(r *Resource) error { escaped = r; return nil }); err != nil {
		t.Fatal(err)
	}
	called := false
	_, barrier, err := escaped.AcquireWithExecutionBarrierPrepared("escaped", time.Minute, time.Now(), func(AcquireOutcome) error { called = true; return nil })
	if err == nil || called || barrier != nil {
		t.Fatalf("escaped callback acquired: %v", err)
	}
	if _, err := escaped.Acquire("barrierless", time.Minute, time.Now()); err == nil {
		t.Fatal("barrierless coordinated acquisition admitted")
	}
}
func TestProofDomainLifecycleRejectsChangedAndMissingIdentity(t *testing.T) {
	for _, missing := range []bool{false, true} {
		name := "changed"
		if missing {
			name = "missing"
		}
		t.Run(name, func(t *testing.T) {
			homes, backing, generation := proofDomainFixture(t)
			ticket := beginDomainTicket(t, homes[0], ProofCapacitySystemFull, "2")
			resource, barrier := acquireDomainProof(t, ticket, "owner")
			if err := barrier.Close(); err != nil {
				t.Fatal(err)
			}
			path, _ := statePath(proofClassRoot(backing, generation, ProofCapacitySystemFull))
			lease, _, err := readLease(path)
			if err != nil {
				t.Fatal(err)
			}
			if missing {
				lease.ProofCoordination = nil
			} else {
				identity, _ := NewProofCoordinationIdentity(generation, ProofCapacitySystemFull, strings.Repeat("3", 64))
				lease.ProofCoordination = &identity
			}
			if err := writeLeaseAtomic(path, *lease); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			checks := []func() error{
				func() error { _, err := resource.Status(time.Now()); return err },
				func() error { _, err := resource.StatusReadOnly(time.Now()); return err },
				func() error { _, err := resource.Refresh("owner", time.Minute, time.Now()); return err },
				func() error { _, err := resource.Release("owner", time.Now()); return err },
				func() error {
					_, err := resource.ReleaseAfter("owner", time.Now(), func() error { t.Error("mismatched finalizer entered"); return nil })
					return err
				},
				func() error { _, err := resource.AttestExecutionTerminated("owner"); return err },
				func() error { _, err := resource.ExecutionPending("owner"); return err },
				func() error {
					_, err := resource.RequestExecutionTermination("owner", "requester", "test", time.Now())
					return err
				},
			}
			for i, check := range checks {
				if err := check(); err == nil {
					t.Errorf("lifecycle operation %d accepted changed identity", i)
				}
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Fatal("identity refusal mutated lease")
			}
		})
	}
}
func TestProofDomainReleaseRequiresRepositoryDescriptorExit(t *testing.T) {
	homes, _, _ := proofDomainFixture(t)
	ticket := beginDomainTicket(t, homes[0], ProofCapacitySystemFull, "2")
	resource, barrier := acquireDomainProof(t, ticket, "owner")
	if len(barrier.additional) != 1 {
		t.Fatal("repository barrier missing")
	}
	fd, err := syscall.Dup(int(barrier.additional[0].Fd()))
	if err != nil {
		t.Fatal(err)
	}
	inherited := os.NewFile(uintptr(fd), "inherited-repository")
	defer inherited.Close()
	if err := barrier.Close(); err != nil {
		t.Fatal(err)
	}
	if pending, err := resource.ExecutionPending("owner"); err != nil || !pending {
		t.Fatalf("repository descendant not reported live: %v", err)
	}
	if _, err := resource.Release("owner", time.Now()); err == nil {
		t.Fatal("repository descendant lost exclusion")
	}
	if _, err := resource.AttestExecutionTerminated("owner"); err == nil {
		t.Fatal("repository descendant falsely attested terminated")
	}
	if err := inherited.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := resource.Release("owner", time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestProofDomainWaitsForAcquisitionStartedInsideCallback(t *testing.T) {
	homes, _, _ := proofDomainFixture(t)
	ticket := beginDomainTicket(t, homes[0], ProofCapacitySystemFull, "2")
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	acquisitionDone := make(chan error, 1)
	turnDone := make(chan error, 1)
	var escaped *Resource
	go func() {
		turnDone <- ticket.WithTurn(context.Background(), false, func(r *Resource) error {
			escaped = r
			go func() {
				_, barrier, err := r.AcquireWithExecutionBarrierPrepared("in-flight", time.Minute, time.Now(), func(AcquireOutcome) error {
					close(entered)
					<-release
					return nil
				})
				if barrier != nil {
					_ = barrier.Close()
				}
				acquisitionDone <- err
			}()
			<-entered
			return nil
		})
	}()
	<-entered
	// Observe the guard enter draining state under its mutex. A callback return
	// must retain domain ownership until the admitted acquisition completes.
	deadline := time.Now().Add(5 * time.Second)
	for {
		escaped.proofAdmission.mu.Lock()
		draining := escaped.proofAdmission.draining
		escaped.proofAdmission.mu.Unlock()
		if draining {
			break
		}
		select {
		case err := <-turnDone:
			t.Fatalf("domain released before acquisition drain: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("callback did not enter drain")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-turnDone:
		t.Fatalf("domain released during prepare: %v", err)
	default:
	}
	unblock()
	if err := <-acquisitionDone; err != nil {
		t.Fatal(err)
	}
	if err := <-turnDone; err != nil {
		t.Fatal(err)
	}
}

func TestProofDomainIndependentClassAdmissionDuringFinalizer(t *testing.T) {
	homes, backing, generation := proofDomainFixture(t)
	first := beginDomainTicket(t, homes[0], ProofCapacitySystemFull, "2")
	resource, barrier := acquireDomainProof(t, first, "first")
	if err := barrier.Close(); err != nil {
		t.Fatal(err)
	}
	entered, unblock, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := resource.ReleaseAfter("first", time.Now(), func() error { close(entered); <-unblock; return nil })
		done <- err
	}()
	<-entered
	defer func() {
		close(unblock)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	other := beginDomainTicket(t, homes[1], ProofCapacityNativeConsumer, "3")
	admitted := make(chan error, 1)
	go func() {
		err := other.WithTurn(context.Background(), false, func(r *Resource) error {
			_, b, err := r.AcquireWithExecutionBarrierPrepared("independent", time.Minute, time.Now(), func(AcquireOutcome) error { return nil })
			if b != nil {
				_ = b.Close()
			}
			return err
		})
		admitted <- err
	}()
	select {
	case err := <-admitted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("independent class admission waited for unrelated finalizer")
	}
	// Even if an idle class flock is held by lifecycle work, acquisition must
	// return to the queue rather than block with the domain lock retained.
	root := proofClassRoot(backing, generation, ProofCapacityNativeConsumer)
	held, release := make(chan struct{}), make(chan struct{})
	lockedDone := make(chan error, 1)
	go func() { lockedDone <- withLock(root, func() error { close(held); <-release; return nil }) }()
	<-held
	released := false
	defer func() {
		if !released {
			close(release)
		}
		if err := <-lockedDone; err != nil {
			t.Error(err)
		}
	}()
	b := &leaseBinding{identity: other.Identity(), acquisition: true, nonblocking: true}
	busyResult := make(chan error, 1)
	go func() { busyResult <- withLeaseBindingLock(root, b, func() error { return nil }) }()
	select {
	case err := <-busyResult:
		if _, ok := err.(*ProofAdmissionBusyError); !ok {
			t.Fatalf("busy class lock should promptly defer: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("class acquisition blocked instead of returning to queue")
	}

	close(release)
	released = true
}
func TestProofDomainHistoricalEvidenceDoesNotBlockSuccessorRepository(t *testing.T) {
	homes, _, _ := proofDomainFixture(t)
	first := beginDomainTicket(t, homes[0], ProofCapacitySystemFull, "2")
	original, barrier := acquireDomainProof(t, first, "original")
	if err := barrier.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := original.Release("original", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := first.Cancel(); err != nil {
		t.Fatal(err)
	}
	successor := beginDomainTicket(t, homes[1], ProofCapacitySystemFull, "3")
	current, barrier := acquireDomainProof(t, successor, "successor")
	defer barrier.Close()
	status, err := original.ObserveHistoricalHolder("original", time.Now())
	if err != nil || status.Holder != "successor" {
		t.Fatalf("historical holder observation: %+v %v", status, err)
	}
	pending, err := original.HistoricalExecutionPending("original")
	if err != nil || pending {
		t.Fatalf("old execution pending=%v %v", pending, err)
	}
	if _, err := original.AttestHistoricalExecutionTerminated("original"); err != nil {
		t.Fatal(err)
	}
	if _, err := original.Release("successor", time.Now()); err == nil {
		t.Fatal("historical observation relaxed exact mutation")
	}
	if status, err := current.Status(time.Now()); err != nil || status.Holder != "successor" {
		t.Fatalf("successor changed: %+v %v", status, err)
	}
}

func TestProofDomainWaitingSnapshotIsForeignAwareAndDoesNotBlockFinalizer(t *testing.T) {
	homes, backing, generation := proofDomainFixture(t)
	first := beginDomainTicket(t, homes[0], ProofCapacitySystemFull, "2")
	_, barrier := acquireDomainProof(t, first, "waiting-snapshot-holder")
	defer barrier.Close()
	// One foreign holder leaves the second system_full slot free: not Held.
	early := beginDomainTicket(t, homes[1], ProofCapacitySystemFull, "3")
	if status, err := early.WaitingStatus(time.Now()); err != nil || status.Held {
		t.Fatalf("free second slot reported Held: %+v %v", status, err)
	}
	if err := early.Cancel(); err != nil {
		t.Fatal(err)
	}
	// Both slots held by foreign repositories: Held, reporting the slot-0 holder.
	_, secondBarrier := acquireDomainProof(t, beginDomainTicket(t, homes[1], ProofCapacitySystemFull, "4"), "waiting-snapshot-second")
	defer secondBarrier.Close()
	waiter := beginDomainTicket(t, homes[1], ProofCapacitySystemFull, "3")
	held, release, unlocked := make(chan struct{}), make(chan struct{}), make(chan error, 2)
	for slot := 0; slot < proofClassCapacity(ProofCapacitySystemFull); slot++ {
		root := proofSlotRoot(backing, generation, ProofCapacitySystemFull, slot)
		locked := make(chan struct{})
		go func() { unlocked <- withLock(root, func() error { close(locked); <-release; return nil }) }()
		<-locked
	}
	close(held)
	<-held
	type result struct {
		status StatusResult
		err    error
	}
	done := make(chan result, 1)
	go func() { status, err := waiter.WaitingStatus(time.Now()); done <- result{status, err} }()
	var observed result
	blocked := false
	select {
	case observed = <-done:
	case <-time.After(2 * time.Second):
		blocked = true
	}
	close(release)
	for slot := 0; slot < proofClassCapacity(ProofCapacitySystemFull); slot++ {
		if err := <-unlocked; err != nil {
			t.Fatal(err)
		}
	}
	if blocked {
		observed = <-done
		t.Fatal("advisory waiting snapshot blocked on finalizer class lock")
	}
	if observed.err != nil || !observed.status.Held || observed.status.Holder != "waiting-snapshot-holder" {
		t.Fatalf("foreign capacity snapshot: %+v %v", observed.status, observed.err)
	}
	if observed.status.ProofCoordination == nil || observed.status.ProofCoordination.RepositoryFingerprint != first.identity.RepositoryFingerprint {
		t.Fatal("waiting snapshot replaced the actual holder identity")
	}
	waiter.identity.Generation = strings.Repeat("9", 64)
	if _, err := waiter.WaitingStatus(time.Now()); err == nil {
		t.Fatal("waiting snapshot accepted stale generation")
	}
}
