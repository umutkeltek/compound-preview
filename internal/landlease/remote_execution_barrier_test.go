package landlease

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func allowRemoteFixture() error { return nil }
func remoteFixture(t *testing.T) ([]string, *Resource, *ExecutionBarrier, RemoteExecutionRequest) {
	t.Helper()
	homes, _, _ := proofDomainFixture(t)
	resource, barrier := acquireDomainProof(t, beginDomainTicket(t, homes[0], ProofCapacitySystemFull, "2"), "remote-proof")
	return homes, resource, barrier, RemoteExecutionRequest{ID: "request_one", Digest: strings.Repeat("a", 64)}
}
func requireExecutionPending(t *testing.T, err error) {
	t.Helper()
	var pending *ExecutionPendingError
	if !errors.As(err, &pending) {
		t.Fatalf("want execution-pending refusal, got %v", err)
	}
}

func TestRemoteFenceSurvivesLocalExitAcrossAllLifecyclePaths(t *testing.T) {
	homes, resource, barrier, request := remoteFixture(t)
	if err := resource.PrepareRemoteExecution("remote-proof", request, allowRemoteFixture); err != nil {
		t.Fatal(err)
	}
	if err := barrier.Close(); err != nil {
		t.Fatal(err)
	}
	pending, err := resource.ExecutionPending("remote-proof")
	if err != nil || !pending {
		t.Fatalf("local exit cleared remote fence: %v %v", pending, err)
	}
	_, err = resource.Release("remote-proof", time.Now())
	requireExecutionPending(t, err)
	finalized := false
	_, err = resource.ReleaseAfter("remote-proof", time.Now(), func() error { finalized = true; return nil })
	requireExecutionPending(t, err)
	if finalized {
		t.Fatal("finalizer ran while remote pending")
	}
	_, err = resource.AttestExecutionTerminated("remote-proof")
	requireExecutionPending(t, err)
	_, err = resource.AttestHistoricalExecutionTerminated("remote-proof")
	requireExecutionPending(t, err)
	_, err = resource.TryAttestHistoricalExecutionTerminated("remote-proof")
	requireExecutionPending(t, err)
	if pending, err = resource.HistoricalExecutionPending("remote-proof"); err != nil || !pending {
		t.Fatalf("historical fence lost: %v %v", pending, err)
	}
	later := time.Now().Add(2 * time.Minute)
	expired, err := resource.StatusReadOnly(later)
	if err != nil {
		t.Fatal(err)
	}
	_, err = resource.RearmExpiredExact(expired, later)
	requireExecutionPending(t, err)
	root := proofSlotRoot(homesBacking(t, homes[0]), resource.proofIdentity.Generation, resource.proofIdentity.Class, resource.proofIdentity.Slot)
	binding := resource.binding(root, true)
	for _, mode := range []string{"ordinary", "expected", "observed"} {
		t.Run(mode, func(t *testing.T) {
			var observed *StatusResult
			if mode == "observed" {
				observed = &expired
			}
			_, err := stealWithProofIdentityExpected(root, "successor", "remote-proof", "verified local exit", later, mode != "ordinary", nil, observed, resource.proofIdentity, binding)
			requireExecutionPending(t, err)
		})
	}
	_, err = acquireWithProofIdentityPrepared(root, "successor", time.Minute, later, nil, resource.proofIdentity, binding)
	requireExecutionPending(t, err)
	ticket := beginDomainTicket(t, homes[1], ProofCapacitySystemFull, "2")
	err = ticket.WithTurn(context.Background(), true, func(r *Resource) error {
		_, b, err := r.StealExpectedWithExecutionBarrierPrepared("successor", "remote-proof", "local exit", later, func(Lease) error { return nil })
		if b != nil {
			_ = b.Close()
		}
		return err
	})
	requireExecutionPending(t, err)
	if err := ticket.Cancel(); err != nil {
		t.Fatal(err)
	}
	// A different repository can still use the other capacity slot.
	foreign, foreignBarrier := acquireDomainProof(t, beginDomainTicket(t, homes[1], ProofCapacitySystemFull, "3"), "foreign")
	requireProofSlot(t, foreign, 1)
	releaseDomainProof(t, foreign, foreignBarrier, "foreign")
}

func TestRemoteFenceReconciliationIsExactAndPreventsDelayedStart(t *testing.T) {
	_, r, b, request := remoteFixture(t)
	if err := r.PrepareRemoteExecution("remote-proof", request, allowRemoteFixture); err != nil {
		t.Fatal(err)
	}
	if err := r.PrepareRemoteExecution("remote-proof", request, allowRemoteFixture); err != nil {
		t.Fatalf("exact pending retry: %v", err)
	}
	wrong := request
	wrong.Digest = strings.Repeat("b", 64)
	if err := r.PrepareRemoteExecution("remote-proof", wrong, allowRemoteFixture); err == nil {
		t.Fatal("conflicting request accepted")
	}
	receipt := strings.Repeat("c", 64)
	foreignIdentity, err := r.proofIdentity.WithSlot(1)
	if err != nil {
		t.Fatal(err)
	}
	foreignResource := ProofResourceForIdentity(r.home, &foreignIdentity)
	if err := foreignResource.ResolveRemoteExecution("remote-proof", request, receipt, allowRemoteFixture); err == nil {
		t.Fatal("foreign slot reconciled the repository marker")
	}

	for _, check := range []func() error{nil, func() error { return errors.New("remote child still live") }} {
		if err := r.ResolveRemoteExecution("remote-proof", request, receipt, check); err == nil {
			t.Fatal("unvalidated receipt accepted")
		}
	}
	if err := r.ResolveRemoteExecution("remote-proof", wrong, receipt, allowRemoteFixture); err == nil {
		t.Fatal("foreign request resolved")
	}
	if err := r.ResolveRemoteExecution("other-holder", request, receipt, allowRemoteFixture); err == nil {
		t.Fatal("foreign holder resolved")
	}
	if err := r.ResolveRemoteExecution("remote-proof", request, receipt, allowRemoteFixture); err != nil {
		t.Fatal(err)
	}
	if err := r.ResolveRemoteExecution("remote-proof", request, receipt, allowRemoteFixture); err != nil {
		t.Fatal(err)
	}
	if err := r.ResolveRemoteExecution("remote-proof", request, strings.Repeat("d", 64), allowRemoteFixture); err == nil {
		t.Fatal("changed terminal receipt accepted")
	}
	// This includes cancel-before-start: a terminal tombstone prevents late dispatch.
	if err := r.PrepareRemoteExecution("remote-proof", request, allowRemoteFixture); err == nil {
		t.Fatal("delayed start reopened terminal request")
	}
	releaseDomainProof(t, r, b, "remote-proof")
	if pending, err := r.HistoricalExecutionPending("remote-proof"); err != nil || pending {
		t.Fatalf("terminal receipt did not clear exact fence: %v %v", pending, err)
	}
	if err := r.ResolveRemoteExecution("remote-proof", request, receipt, allowRemoteFixture); err != nil {
		t.Fatalf("released exact retry: %v", err)
	}
}

func TestRemoteFencePartialPreparationAndResolutionStayBlocking(t *testing.T) {
	for _, stage := range []string{"prepare", "resolve"} {
		t.Run(stage, func(t *testing.T) {
			homes, r, b, request := remoteFixture(t)
			interrupt := func() error { return errors.New("interrupted") }
			if stage == "prepare" {
				if err := r.prepareRemoteExecution("remote-proof", request, allowRemoteFixture, interrupt); err == nil {
					t.Fatal("fault missing")
				}
			} else {
				if err := r.PrepareRemoteExecution("remote-proof", request, allowRemoteFixture); err != nil {
					t.Fatal(err)
				}
				if err := r.resolveRemoteExecution("remote-proof", request, strings.Repeat("c", 64), allowRemoteFixture, interrupt); err == nil {
					t.Fatal("fault missing")
				}
			}
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			_, err := r.Release("remote-proof", time.Now())
			requireExecutionPending(t, err)
			// Even loss of the capacity lease cannot free the repository fence.
			root := proofSlotRoot(homesBacking(t, homes[0]), r.proofIdentity.Generation, r.proofIdentity.Class, r.proofIdentity.Slot)
			path, _ := statePath(root)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			ticket := beginDomainTicket(t, homes[1], ProofCapacitySystemFull, "2")
			called := false
			err = ticket.WithTurn(context.Background(), false, func(*Resource) error { called = true; return nil })
			if err == nil || called {
				t.Fatal("partial marker did not fence another slot")
			}
			// Steal admission may inspect the resource, but its barrier cannot launch.
			err = ticket.WithTurn(context.Background(), true, func(next *Resource) error {
				_, child, err := next.AcquireWithExecutionBarrierPrepared("next", time.Minute, time.Now(), func(AcquireOutcome) error { return nil })
				if child != nil {
					_ = child.Close()
				}
				return err
			})
			requireExecutionPending(t, err)
			if err := r.ResolveRemoteExecution("remote-proof", request, strings.Repeat("c", 64), allowRemoteFixture); err != nil {
				t.Fatal(err)
			}
			if pending, err := r.HistoricalExecutionPending("remote-proof"); err != nil || pending {
				t.Fatalf("exact recovery: %v %v", pending, err)
			}
		})
	}
}

func TestRemoteFenceCorruptionAndUnknownRequestFailClosed(t *testing.T) {
	_, r, _, request := remoteFixture(t)
	if err := r.ResolveRemoteExecution("remote-proof", request, strings.Repeat("c", 64), allowRemoteFixture); err == nil {
		t.Fatal("unprepared request resolved")
	}
	if err := r.PrepareRemoteExecution("remote-proof", request, allowRemoteFixture); err != nil {
		t.Fatal(err)
	}
	root := proofSlotRoot(homesBacking(t, r.home), r.proofIdentity.Generation, r.proofIdentity.Class, r.proofIdentity.Slot)
	path := filepath.Join(remoteFenceDir(root, "remote-proof"), remoteFenceName(request.ID))
	if err := os.WriteFile(path, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ExecutionPending("remote-proof"); err == nil {
		t.Fatal("corrupt marker accepted")
	}
	if err := r.ResolveRemoteExecution("remote-proof", request, strings.Repeat("c", 64), allowRemoteFixture); err == nil {
		t.Fatal("corrupt marker reconciled")
	}
}
