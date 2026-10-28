package home

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestHomeMutationGuardPanicDrainsInFlightUseBeforeReleasingExclusion(t *testing.T) {
	homePath := t.TempDir()
	guardReady := make(chan *HomeMutationGuard, 1)
	useEntered := make(chan struct{})
	releaseUse := make(chan struct{})
	usePanicked := make(chan any, 1)
	outerCallbackReturned := make(chan struct{})
	outerDone := make(chan error, 1)

	go func() {
		outerDone <- WithHomeMutationGuard(homePath, func(guard *HomeMutationGuard) error {
			guardReady <- guard
			go func() {
				defer func() { usePanicked <- recover() }()
				_ = guard.WithUse(homePath, func() error {
					close(useEntered)
					<-releaseUse
					panic("synthetic in-flight mutation panic")
				})
			}()
			<-useEntered
			close(outerCallbackReturned)
			return nil
		})
	}()

	guard := <-guardReady
	<-outerCallbackReturned
	waitForHomeMutationGuardInvalidation(t, guard, 1)

	select {
	case err := <-outerDone:
		t.Fatalf("outer guard returned before panicking in-flight use drained: %v", err)
	default:
	}

	contenderEntered := false
	acquired, err := TryWithHomeMutationGuard(homePath, func(*HomeMutationGuard) error {
		contenderEntered = true
		return nil
	})
	if err != nil {
		t.Fatalf("contender while in-flight use drains: %v", err)
	}
	if acquired || contenderEntered {
		t.Fatalf("contender bypassed draining guard: acquired=%v entered=%v", acquired, contenderEntered)
	}

	escapedUseEntered := false
	if err := guard.WithUse(homePath, func() error {
		escapedUseEntered = true
		return nil
	}); err == nil || !strings.Contains(err.Error(), "scope expired") {
		t.Fatalf("new use after invalidation error = %v, want scope expired", err)
	}
	if escapedUseEntered {
		t.Fatal("new use entered after invalidation began")
	}

	close(releaseUse)
	select {
	case recovered := <-usePanicked:
		if recovered != "synthetic in-flight mutation panic" {
			t.Fatalf("recovered panic = %v, want synthetic in-flight mutation panic", recovered)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for in-flight use panic")
	}
	select {
	case err := <-outerDone:
		if err != nil {
			t.Fatalf("outer guard after panic drain: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for outer guard after panic drain")
	}

	acquired, err = TryWithHomeMutationGuard(homePath, func(*HomeMutationGuard) error {
		contenderEntered = true
		return nil
	})
	if err != nil || !acquired || !contenderEntered {
		t.Fatalf("contender after panic drain: acquired=%v entered=%v err=%v", acquired, contenderEntered, err)
	}
}

func waitForHomeMutationGuardInvalidation(t *testing.T, guard *HomeMutationGuard, wantInFlight int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		guard.scope.mu.Lock()
		active := guard.scope.active
		inFlight := guard.scope.inFlight
		guard.scope.mu.Unlock()
		if !active {
			if inFlight != wantInFlight {
				t.Fatalf("guard invalidated with in-flight uses = %d, want %d", inFlight, wantInFlight)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for home mutation guard invalidation")
		}
		runtime.Gosched()
	}
}
