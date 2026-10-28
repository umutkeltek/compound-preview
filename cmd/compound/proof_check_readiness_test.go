package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// waitForProofCheckProcessReady does not charge durable admission work to a
// subprocess-readiness budget. Early admission/start/exit errors stay visible;
// the caller owns cancellation and joining before deleting fixture directories.
func waitForProofCheckProcessReady(ctx context.Context, started <-chan struct{}, done <-chan error, path string, timeout time.Duration) error {
	ended := func(phase string, err error) error {
		if err == nil {
			return fmt.Errorf("predecessor completed before %s", phase)
		}
		return fmt.Errorf("predecessor failed before %s: %w", phase, err)
	}
	select {
	case <-started:
	case err := <-done:
		return ended("process start", err)
	case <-ctx.Done():
		return fmt.Errorf("waiting for predecessor process start: %w", ctx.Err())
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for predecessor process readiness: %w", err)
		}
		select {
		case err := <-done:
			return ended("process readiness", err)
		default:
		}
		if _, err := os.Stat(path); err == nil {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("waiting for predecessor process readiness: %w", err)
			}
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat predecessor readiness: %w", err)
		}
		select {
		case err := <-done:
			return ended("process readiness", err)
		case <-ctx.Done():
			return fmt.Errorf("waiting for predecessor process readiness: %w", ctx.Err())
		case <-timer.C:
			return fmt.Errorf("timed out waiting for predecessor process readiness at %s", path)
		case <-ticker.C:
		}
	}
}

func TestProofCheckReadinessSurfacesEarlyAdmissionFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("synthetic admission refusal")
	done := make(chan error, 1)
	done <- want
	err := waitForProofCheckProcessReady(context.Background(), make(chan struct{}), done, filepath.Join(t.TempDir(), "ready"), time.Second)
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "before process start") {
		t.Fatalf("readiness error = %v, want original admission error", err)
	}
}

func TestProofCheckReadinessDoesNotAcceptMarkerAfterEarlyExit(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(path, []byte("started"), 0o600); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	close(started)
	want := errors.New("synthetic process failure")
	done := make(chan error, 1)
	done <- want
	if err := waitForProofCheckProcessReady(context.Background(), started, done, path, time.Second); !errors.Is(err, want) {
		t.Fatalf("readiness error = %v, want early process failure despite marker", err)
	}
}

func TestProofCheckReadinessCancellationIsNotSuccess(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := waitForProofCheckProcessReady(ctx, make(chan struct{}), make(chan error), filepath.Join(t.TempDir(), "ready"), time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("readiness error = %v, want cancellation", err)
	}
}

func TestProofCheckReadinessCancellationAfterStartRejectsExistingMarker(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(path, []byte("started"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	close(started)
	cancel()
	for range 100 {
		err := waitForProofCheckProcessReady(ctx, started, make(chan error), path, time.Second)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("readiness error = %v, want cancellation despite marker", err)
		}
	}
}

func TestProofCheckReadinessEarlySuccessIsNotReadiness(t *testing.T) {
	t.Parallel()
	for _, afterStart := range []bool{false, true} {
		started := make(chan struct{})
		if afterStart {
			close(started)
		}
		done := make(chan error, 1)
		done <- nil
		err := waitForProofCheckProcessReady(context.Background(), started, done, filepath.Join(t.TempDir(), "ready"), time.Second)
		if err == nil || !strings.Contains(err.Error(), "predecessor completed before") {
			t.Fatalf("afterStart=%t: readiness error = %v, want premature completion", afterStart, err)
		}
	}
}

func TestProofCheckReadinessMissingMarkerFailsAndWorkerJoins(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "never-created")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := proofCheckCommandContext(ctx, "sh", "-c", "while :; do sleep 1; done")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	close(started)
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		done <- cmd.Wait()
		close(finished)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Error("worker did not join after cancellation")
		}
	})
	err := waitForProofCheckProcessReady(ctx, started, done, path, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for predecessor process readiness") {
		t.Fatalf("readiness error = %v, want missing marker failure", err)
	}
}
