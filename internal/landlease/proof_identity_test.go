package landlease

import (
	"os"
	"strings"
	"testing"
	"time"
)

func coordinatedFixtureLease(t *testing.T, root string) Lease {
	t.Helper()
	lease, err := Acquire(root, laneAlpha, time.Minute, baseNow)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewProofCoordinationIdentity(strings.Repeat("1", 64), ProofCapacitySystemFull, strings.Repeat("2", 64))
	if err != nil {
		t.Fatal(err)
	}
	lease.ProofCoordination = &identity
	path, _ := statePath(root)
	// Deliberately seed future identity through the persistence seam so the
	// regression can expose existing constructors that would erase it.
	if err := writeLeaseAtomic(path, lease); err != nil {
		t.Fatal(err)
	}
	return lease
}

func TestProofCoordinationSurvivesRefreshAndRenew(t *testing.T) {
	for _, renew := range []bool{false, true} {
		name := "refresh"
		if renew {
			name = "renew"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			before := coordinatedFixtureLease(t, root)
			var after Lease
			var err error
			if renew {
				after, err = Acquire(root, laneAlpha, time.Minute, baseNow.Add(time.Second))
			} else {
				after, err = Refresh(root, laneAlpha, time.Minute, baseNow.Add(time.Second))
			}
			if err != nil {
				t.Fatal(err)
			}
			if !sameProofCoordination(before.ProofCoordination, after.ProofCoordination) {
				t.Fatalf("lease renewal erased exact identity: %+v", after)
			}
		})
	}
}

func TestProofCoordinationUntypedTakeoverRefused(t *testing.T) {
	for _, steal := range []bool{false, true} {
		name := "expired-acquire"
		if steal {
			name = "steal"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			coordinatedFixtureLease(t, root)
			path, _ := statePath(root)
			before, _ := os.ReadFile(path)
			var err error
			if steal {
				_, err = Steal(root, laneBeta, "synthetic takeover", baseNow.Add(time.Second))
			} else {
				_, err = Acquire(root, laneBeta, time.Minute, baseNow.Add(2*time.Minute))
			}
			if err == nil {
				t.Fatal("untyped takeover reinterpreted coordinated lease")
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Fatal("refusal changed lease")
			}
		})
	}
}

func TestProofCoordinationChangedIdentityFencesExactRecovery(t *testing.T) {
	root := t.TempDir()
	lease := coordinatedFixtureLease(t, root)
	at := baseNow.Add(2 * time.Minute)
	observed, err := StatusReadOnly(root, at)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := NewProofCoordinationIdentity(lease.ProofCoordination.Generation, ProofCapacitySystemFull, strings.Repeat("3", 64))
	if err != nil {
		t.Fatal(err)
	}
	lease.ProofCoordination = &changed
	path, _ := statePath(root)
	if err := writeLeaseAtomic(path, lease); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if _, err := RearmExpiredExact(root, observed, at); err == nil {
		t.Fatal("exact recovery ignored changed coordination identity")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("stale exact recovery changed lease")
	}
}

func TestProofCoordinationInvalidIdentityRefusesRead(t *testing.T) {
	root := t.TempDir()
	lease := coordinatedFixtureLease(t, root)
	lease.ProofCoordination.RepositoryBarrier = strings.Repeat("4", 64)
	path, _ := statePath(root)
	// Invalid authority cannot be produced by a validated constructor.
	if err := writeLeaseAtomic(path, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := StatusReadOnly(root, baseNow); err == nil {
		t.Fatal("invalid repository barrier identity accepted")
	}
}

func TestProofCoordinationTypedTakeoverKeepsPredecessor(t *testing.T) {
	root := t.TempDir()
	before := coordinatedFixtureLease(t, root)
	next, err := NewProofCoordinationIdentity(before.ProofCoordination.Generation, ProofCapacitySystemFull, strings.Repeat("3", 64))
	if err != nil {
		t.Fatal(err)
	}
	result, err := acquireWithProofIdentityPrepared(root, laneBeta, time.Minute, baseNow.Add(2*time.Minute), nil, &next)
	if err != nil {
		t.Fatal(err)
	}
	if !sameProofCoordination(result.Lease.ProofCoordination, &next) {
		t.Fatal("new exact identity lost")
	}
	if result.ExpiredPredecessor == nil || !sameProofCoordination(result.ExpiredPredecessor.ProofCoordination, before.ProofCoordination) {
		t.Fatal("predecessor identity lost")
	}
}

func TestProofCoordinationCannotReinterpretHistoricalLease(t *testing.T) {
	root := t.TempDir()
	if _, err := Acquire(root, laneAlpha, time.Minute, baseNow); err != nil {
		t.Fatal(err)
	}
	identity, err := NewProofCoordinationIdentity(strings.Repeat("1", 64), ProofCapacitySystemFull, strings.Repeat("2", 64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireWithProofIdentityPrepared(root, laneBeta, time.Minute, baseNow.Add(2*time.Minute), nil, &identity); err == nil {
		t.Fatal("historical identity reinterpreted")
	}
}

func TestProofCoordinationTypedIdentityChangesRefuseBeforePrepare(t *testing.T) {
	for _, kind := range []string{"holder-repository", "class", "generation", "historical-same-holder"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			before := coordinatedFixtureLease(t, root)
			next := *before.ProofCoordination
			holder := laneBeta
			switch kind {
			case "holder-repository":
				holder = laneAlpha
				next, _ = NewProofCoordinationIdentity(next.Generation, next.Class, strings.Repeat("3", 64))
			case "class":
				next.Class = ProofCapacityNativeConsumer
			case "generation":
				next.Generation = strings.Repeat("4", 64)
			case "historical-same-holder":
				holder = laneAlpha
				before.ProofCoordination = nil
				path, _ := statePath(root)
				if err := writeLeaseAtomic(path, before); err != nil {
					t.Fatal(err)
				}
			}
			path, _ := statePath(root)
			bytesBefore, _ := os.ReadFile(path)
			called := false
			_, barrier, err := acquireWithProofIdentityAndBarrierPrepared(root, holder, time.Minute, baseNow.Add(2*time.Minute), func(AcquireOutcome) error { called = true; return nil }, &next)
			if barrier != nil {
				barrier.Close()
				t.Fatal("refusal returned barrier")
			}
			if err == nil || called {
				t.Fatalf("identity mutation admitted or prepare called: %v, %v", err, called)
			}
			bytesAfter, _ := os.ReadFile(path)
			if string(bytesBefore) != string(bytesAfter) {
				t.Fatal("refusal changed lease")
			}
		})
	}
}
func TestProofCoordinationTypedEmptyAcquireAndStealBarriers(t *testing.T) {
	for _, withBarrier := range []bool{false, true} {
		name := "ordinary"
		if withBarrier {
			name = "barrier"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			identity, _ := NewProofCoordinationIdentity(strings.Repeat("1", 64), ProofCapacitySystemFull, strings.Repeat("2", 64))
			result, barrier, err := acquireWithProofIdentityAndBarrierPrepared(root, laneAlpha, time.Minute, baseNow, func(AcquireOutcome) error { return nil }, &identity)
			if err != nil {
				t.Fatal(err)
			}
			defer barrier.Close()
			if !sameProofCoordination(result.Lease.ProofCoordination, &identity) {
				t.Fatal("acquisition lost identity")
			}
			next, _ := NewProofCoordinationIdentity(identity.Generation, identity.Class, strings.Repeat("3", 64))
			called := false
			steal := func() (Lease, *ExecutionBarrier, error) {
				prepare := func(Lease) error { called = true; return nil }
				if withBarrier {
					return stealWithProofIdentityAndBarrierPrepared(root, laneBeta, laneAlpha, "test", baseNow.Add(time.Second), prepare, &next)
				}
				lease, err := stealWithProofIdentityExpected(root, laneBeta, laneAlpha, "test", baseNow.Add(time.Second), true, prepare, nil, &next)
				return lease, nil, err
			}
			if _, child, err := steal(); err == nil || called || child != nil {
				t.Fatalf("live predecessor barrier bypassed: %v", err)
			}
			if err := barrier.Close(); err != nil {
				t.Fatal(err)
			}
			lease, child, err := steal()
			if err != nil {
				t.Fatal(err)
			}
			if child != nil {
				defer child.Close()
			}
			if !called || !sameProofCoordination(lease.ProofCoordination, &next) {
				t.Fatal("typed steal lost identity or prepare")
			}
		})
	}
}
