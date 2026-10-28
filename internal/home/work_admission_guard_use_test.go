package home

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWorkAdmissionGuardWithUseRejectsNilExpiredForeignAndNilCallback(t *testing.T) {
	t.Run("nil guard", func(t *testing.T) {
		called := false
		err := (*WorkAdmissionGuard)(nil).WithUse(t.TempDir(), "work_nil_guard", func() error {
			called = true
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "zero guard") {
			t.Fatalf("nil guard use error = %v, want zero-guard refusal", err)
		}
		if called {
			t.Fatal("nil guard invoked use callback")
		}
	})

	t.Run("expired guard", func(t *testing.T) {
		homePath := newWorkAdmissionTestHome(t)
		workID := "work_expired_guard"
		var escaped *WorkAdmissionGuard
		if err := WithWorkAdmissionGuard(homePath, workID, func(guard *WorkAdmissionGuard) error {
			escaped = guard
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		called := false
		err := escaped.WithUse(homePath, workID, func() error {
			called = true
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "scope expired") {
			t.Fatalf("expired guard use error = %v, want scope-expired refusal", err)
		}
		if called {
			t.Fatal("expired guard invoked use callback")
		}
	})

	t.Run("foreign identity", func(t *testing.T) {
		homePath := newWorkAdmissionTestHome(t)
		foreignHome := newWorkAdmissionTestHome(t)
		workID := "work_foreign_guard"
		called := false
		if err := WithWorkAdmissionGuard(homePath, workID, func(guard *WorkAdmissionGuard) error {
			err := guard.WithUse(foreignHome, workID, func() error {
				called = true
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "canonical home mismatch") {
				t.Fatalf("foreign guard use error = %v, want canonical-home refusal", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if called {
			t.Fatal("foreign guard invoked use callback")
		}
	})

	t.Run("nil callback", func(t *testing.T) {
		homePath := newWorkAdmissionTestHome(t)
		workID := "work_nil_use_callback"
		if err := WithWorkAdmissionGuard(homePath, workID, func(guard *WorkAdmissionGuard) error {
			err := guard.WithUse(homePath, workID, nil)
			if err == nil || !strings.Contains(err.Error(), "use callback required") {
				t.Fatalf("nil callback error = %v, want callback-required refusal", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestWorkAdmissionGuardWithUseRetainsAdmissionUntilCallbackFinishes(t *testing.T) {
	homePath := newWorkAdmissionTestHome(t)
	workID := "work_retained_use"
	guardReady := make(chan *WorkAdmissionGuard, 1)
	useEntered := make(chan struct{})
	releaseUse := make(chan struct{})
	useDone := make(chan error, 1)
	outerDone := make(chan error, 1)

	go func() {
		outerDone <- WithWorkAdmissionGuard(homePath, workID, func(guard *WorkAdmissionGuard) error {
			guardReady <- guard
			go func() {
				useDone <- guard.WithUse(homePath, workID, func() error {
					close(useEntered)
					<-releaseUse
					return nil
				})
			}()
			<-useEntered
			return nil
		})
	}()

	guard := <-guardReady
	<-useEntered
	waitForWorkAdmissionGuardInvalidation(t, guard, 1)

	select {
	case err := <-outerDone:
		t.Fatalf("outer work admission returned before retained use drained: %v", err)
	default:
	}
	contenderEntered := false
	acquired, err := TryWithWorkAdmissionGuard(homePath, workID, func(*WorkAdmissionGuard) error {
		contenderEntered = true
		return nil
	})
	if err != nil {
		t.Fatalf("contender while retained use drains: %v", err)
	}
	if acquired || contenderEntered {
		t.Fatalf("contender bypassed retained use: acquired=%v entered=%v", acquired, contenderEntered)
	}
	escapedUseEntered := false
	if err := guard.WithUse(homePath, workID, func() error {
		escapedUseEntered = true
		return nil
	}); err == nil || !strings.Contains(err.Error(), "scope expired") {
		t.Fatalf("new use after invalidation error = %v, want scope expired", err)
	}
	if escapedUseEntered {
		t.Fatal("new use entered after invalidation began")
	}

	close(releaseUse)
	if err := <-useDone; err != nil {
		t.Fatalf("retained use: %v", err)
	}
	if err := <-outerDone; err != nil {
		t.Fatalf("outer work admission: %v", err)
	}

	acquired, err = TryWithWorkAdmissionGuard(homePath, workID, func(*WorkAdmissionGuard) error {
		contenderEntered = true
		return nil
	})
	if err != nil || !acquired || !contenderEntered {
		t.Fatalf("contender after drain: acquired=%v entered=%v err=%v", acquired, contenderEntered, err)
	}
}

func TestWorkAdmissionGuardWithUsePanicDrainsBeforeAdmissionRelease(t *testing.T) {
	homePath := newWorkAdmissionTestHome(t)
	workID := "work_retained_use_panic"
	guardReady := make(chan *WorkAdmissionGuard, 1)
	useEntered := make(chan struct{})
	releaseUse := make(chan struct{})
	usePanicked := make(chan any, 1)
	outerDone := make(chan error, 1)

	go func() {
		outerDone <- WithWorkAdmissionGuard(homePath, workID, func(guard *WorkAdmissionGuard) error {
			guardReady <- guard
			go func() {
				defer func() { usePanicked <- recover() }()
				_ = guard.WithUse(homePath, workID, func() error {
					close(useEntered)
					<-releaseUse
					panic("synthetic work-admission use panic")
				})
			}()
			<-useEntered
			return nil
		})
	}()

	guard := <-guardReady
	<-useEntered
	waitForWorkAdmissionGuardInvalidation(t, guard, 1)
	select {
	case err := <-outerDone:
		t.Fatalf("outer work admission returned before panicking use drained: %v", err)
	default:
	}

	close(releaseUse)
	select {
	case recovered := <-usePanicked:
		if recovered != "synthetic work-admission use panic" {
			t.Fatalf("recovered panic = %v, want synthetic work-admission use panic", recovered)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for retained-use panic")
	}
	select {
	case err := <-outerDone:
		if err != nil {
			t.Fatalf("outer work admission after panic drain: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for work admission after panic drain")
	}
}

func waitForWorkAdmissionGuardInvalidation(t *testing.T, guard *WorkAdmissionGuard, wantInFlight int) {
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
			t.Fatal("timed out waiting for work-admission invalidation")
		}
		runtime.Gosched()
	}
}
