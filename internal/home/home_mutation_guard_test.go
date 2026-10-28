package home

import (
	"strings"
	"testing"
	"time"
)

func TestHomeMutationGuardWithUseRejectsNilExpiredAndForeignGuard(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		called := false
		err := (*HomeMutationGuard)(nil).WithUse(t.TempDir(), func() error {
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

	t.Run("expired", func(t *testing.T) {
		homePath := t.TempDir()
		var escaped *HomeMutationGuard
		if err := WithHomeMutationGuard(homePath, func(guard *HomeMutationGuard) error {
			escaped = guard
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		called := false
		err := escaped.WithUse(homePath, func() error {
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

	t.Run("foreign", func(t *testing.T) {
		selectedHome := t.TempDir()
		foreignHome := t.TempDir()
		called := false
		err := WithHomeMutationGuard(selectedHome, func(guard *HomeMutationGuard) error {
			useErr := guard.WithUse(foreignHome, func() error {
				called = true
				return nil
			})
			if useErr == nil || !strings.Contains(useErr.Error(), "identity changed") {
				t.Fatalf("foreign guard use error = %v, want identity refusal", useErr)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if called {
			t.Fatal("foreign guard invoked use callback")
		}
	})
}

func TestHomeMutationGuardWithUseRetainsScopeUntilCallbackFinishes(t *testing.T) {
	homePath := t.TempDir()
	useEntered := make(chan struct{})
	releaseUse := make(chan struct{})
	useDone := make(chan error, 1)
	outerDone := make(chan error, 1)
	go func() {
		outerDone <- WithHomeMutationGuard(homePath, func(guard *HomeMutationGuard) error {
			go func() {
				useDone <- guard.WithUse(homePath, func() error {
					close(useEntered)
					<-releaseUse
					return nil
				})
			}()
			<-useEntered
			return nil
		})
	}()
	<-useEntered

	var outerErr error
	returnedEarly := false
	timer := time.NewTimer(200 * time.Millisecond)
	select {
	case outerErr = <-outerDone:
		returnedEarly = true
	case <-timer.C:
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	close(releaseUse)
	useErr := <-useDone
	if !returnedEarly {
		outerErr = <-outerDone
	}
	if returnedEarly {
		t.Fatalf("outer guard returned while retained use was active: outer=%v use=%v", outerErr, useErr)
	}
	if useErr != nil {
		t.Fatalf("retained use: %v", useErr)
	}
	if outerErr != nil {
		t.Fatalf("outer guard: %v", outerErr)
	}
}
