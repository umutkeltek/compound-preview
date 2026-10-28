package harness

import (
	"context"
	"testing"
	"time"

	"compound/internal/runattempt"
)

// osEntrypointsAdapterContract is the public-facing contract of the
// OSEntrypointsAdapter: Name returns the manifest adapter id, Run drives
// a subprocess end-to-end and produces a terminal RunAttempt. Pinned by
// the four tests below.
//
// Falsifier: removing the Adapter interface from adapter.go turns every
// test in this file RED with a "undefined" build error. Removing the
// Heartbeat call inside the adapter turns TestOSEntrypointsAdapterHeartbeats
// RED with "no heartbeats recorded". Mapping the exit code wrong turns
// TestOSEntrypointsAdapterFailedExit RED with "expected failed, got
// succeeded".

// TestOSEntrypointsAdapterHappyPath: a successful subprocess (echo hello)
// produces a succeeded RunAttempt with workspace + command set and a
// transcript path. Returns nil error (subprocess exit 0 is success, not
// an infrastructure failure).
func TestOSEntrypointsAdapterHappyPath(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	a := NewOSEntrypointsAdapter(store)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	out, err := a.Run(context.Background(), RunInput{
		WorkID:    "work_happy",
		Harness:   "os-harness-entrypoints",
		Workspace: home,
		Command:   "echo",
		Args:      []string{"hello", "from", "adapter"},
		Now:       now,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.RunAttempt.Status != runattempt.StatusSucceeded {
		t.Fatalf("status = %q, want succeeded (echo exits 0)", out.RunAttempt.Status)
	}
	if out.RunAttempt.Workspace != home {
		t.Fatalf("workspace = %q, want %q", out.RunAttempt.Workspace, home)
	}
	if out.RunAttempt.Command != "echo" {
		t.Fatalf("command = %q, want %q", out.RunAttempt.Command, "echo")
	}
	if out.TranscriptPath == "" {
		t.Fatalf("transcript path must be set on success")
	}
	if out.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", out.ExitCode)
	}
	// Re-Get from disk to prove the RunAttempt is persisted (not just
	// in-memory) and that the S2 fields round-trip through atomic write.
	got, err := store.Get(out.RunAttempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != runattempt.StatusSucceeded {
		t.Fatalf("persisted status = %q, want succeeded", got.Status)
	}
	if got.TranscriptRef != out.TranscriptPath {
		t.Fatalf("transcript_ref = %q, want %q", got.TranscriptRef, out.TranscriptPath)
	}
}

// TestOSEntrypointsAdapterFailedExit: a subprocess that exits non-zero
// (the `false` builtin) produces a failed RunAttempt with the right
// exit code captured. Returns nil error -- the subprocess completed
// (even though it returned non-zero), the RunAttempt reflects the
// failed exit. Adapter only returns error for INFRASTRUCTURE failure
// (couldn't mint, couldn't spawn, etc.).
func TestOSEntrypointsAdapterFailedExit(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	a := NewOSEntrypointsAdapter(store)

	out, err := a.Run(context.Background(), RunInput{
		WorkID:    "work_fail",
		Harness:   "os-harness-entrypoints",
		Workspace: home,
		Command:   "false",
		Args:      []string{},
		Now:       time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Run returned an error: %v (want nil -- adapter only errors on infrastructure failure, not subprocess exit code)", err)
	}
	if out.RunAttempt.Status != runattempt.StatusFailed {
		t.Fatalf("status = %q, want failed (false exits 1)", out.RunAttempt.Status)
	}
	if out.ExitCode == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if out.TranscriptPath == "" {
		t.Fatalf("transcript path must be set even on failure (partial capture is the audit trail)")
	}
}

// TestOSEntrypointsAdapterTimeout: a subprocess that runs longer than
// the configured Timeout is killed; the RunAttempt terminalizes failed
// and out.TimedOut is true. Pinned: a long-running process that the
// adapter "forgets" to time out would leave a perpetually-running
// RunAttempt on disk and block operator visibility.
func TestOSEntrypointsAdapterTimeout(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	a := NewOSEntrypointsAdapter(store)

	out, err := a.Run(context.Background(), RunInput{
		WorkID:    "work_timeout",
		Harness:   "os-harness-entrypoints",
		Workspace: home,
		Command:   "sleep",
		Args:      []string{"10"},
		Timeout:   500 * time.Millisecond,
		Now:       time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("timeout must NOT surface as a Run-level error; the RunAttempt is the audit, out.TimedOut is the signal: %v", err)
	}
	if !out.TimedOut {
		t.Fatalf("out.TimedOut = false, want true (subprocess slept 10s with 500ms timeout)")
	}
	if out.RunAttempt.Status != runattempt.StatusFailed {
		t.Fatalf("timed-out RunAttempt status = %q, want failed", out.RunAttempt.Status)
	}
	// Re-Get to confirm persistence.
	got, err := store.Get(out.RunAttempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != runattempt.StatusFailed {
		t.Fatalf("persisted status = %q, want failed", got.Status)
	}
}

// TestOSEntrypointsAdapterHeartbeats: a subprocess running longer than
// one heartbeat interval produces a heartbeat on the returned RunAttempt
// (LastHeartbeatAt is non-nil). Pinned: heartbeats are the operator-visible
// signal that a long-running attempt is still progressing. The adapter
// writes the heartbeat via store.Heartbeat, which advances LastHeartbeatAt;
// the returned RunAttempt reflects the most recent persisted state.
func TestOSEntrypointsAdapterHeartbeats(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	a := NewOSEntrypointsAdapterWithInterval(store, 200*time.Millisecond)

	out, err := a.Run(context.Background(), RunInput{
		WorkID:    "work_heartbeat",
		Harness:   "os-harness-entrypoints",
		Workspace: home,
		Command:   "sleep",
		Args:      []string{"1.2"},
		Now:       time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.RunAttempt.LastHeartbeatAt == nil {
		t.Fatalf("heartbeat must be recorded for a 1.2s subprocess at 200ms cadence")
	}
}

// TestOSEntrypointsAdapterName: trivial contract pin. Pinned because
// the Adapter.Name return value is how the registry (future slice) will
// key adapters to manifest rows.
func TestOSEntrypointsAdapterName(t *testing.T) {
	a := NewOSEntrypointsAdapter(runattempt.Open(t.TempDir()))
	if a.Name() != "os-harness-entrypoints" {
		t.Fatalf("name = %q, want %q", a.Name(), "os-harness-entrypoints")
	}
}
