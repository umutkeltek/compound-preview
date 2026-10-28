package work

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Audit 2026-09-16, finding 5 (detached recovery): a reserved sidecar
// with no PID and a still-claimed envelope was classified as "lost door"
// by ReconcileDetachedLaunches and the envelope was closed failed. The
// reconciliation did not first establish that the original launcher was
// gone or that its reservation had expired.
//
// TestReconcileDoesNotClassifyReservedLiveEnvelopeAsLostBeforeGraceWindow
// is the red falsifier: a fresh reserved sidecar whose launcher is in
// flight (the sidecar was just written, the door is being spawned this
// very moment) must NOT be classified door_lost on the same tick the
// reservation landed. The chosen verdict shape is a reserved outcome
// carrying the grace explanation; the audit contract is that the
// envelope stays claimed and the sidecar stays reserved.
func TestReconcileDoesNotClassifyReservedLiveEnvelopeAsLostBeforeGraceWindow(t *testing.T) {
	s := newStore(t)
	seedReadyExecuteWork(t, s, "work_reserved_live")
	if _, err := s.Claim("work_reserved_live", "claude-minimax", fixedNow); err != nil {
		t.Fatal(err)
	}
	if err := WriteDetachedLaunch(s.home, DetachedLaunch{
		WorkID:    "work_reserved_live",
		Lane:      "claude-minimax",
		Actor:     "claude-minimax",
		LogPath:   "/tmp/empty-door.log",
		StartedAt: fixedNow.UTC().Format(time.RFC3339),
		Outcome:   DetachedOutcomeReserved,
	}); err != nil {
		t.Fatal(err)
	}
	verdicts := s.ReconcileDetachedLaunches(fixedNow.Add(time.Minute))
	if len(verdicts) != 1 || verdicts[0].Outcome != DetachedOutcomeReserved || verdicts[0].State != "claimed" {
		t.Fatalf("a still-claimed envelope with a fresh reserved sidecar must surface a reserved verdict, NOT door_lost; got %+v", verdicts)
	}
	sidecar, _ := ReadDetachedLaunch(s.home, "work_reserved_live")
	if sidecar.Outcome != DetachedOutcomeReserved {
		t.Fatalf("sidecar outcome mutated to %q on a within-grace reconcile; reserved state must persist", sidecar.Outcome)
	}
	w, _ := s.Get("work_reserved_live")
	if w.State != "claimed" {
		t.Fatalf("envelope state mutated to %q; a still-claimed reserved launch must NOT close the envelope", w.State)
	}
}

// Audit 2026-09-16, finding 5 (detached recovery): the existing recovery
// read the first matching "started pid=" marker from a per-work log.
// That marker is appended to a log opened with O_APPEND, so an EARLIER
// launch attempt's marker survives in the log and a LATER reconcile
// adopts it as the new launch's pid. The newer launch ends up holding
// the older launch's pid in the sidecar.
//
// TestReconcileBindsRecoveredPidToExactLaunchAttemptNotFirstLogMarker
// is the red falsifier: when the sidecar has NO pid and the log
// contains a stale "started pid=" line from a prior launch, the
// reconciler must NOT adopt that pid as this launch's. The recovery
// either binds to the exact launch attempt (e.g. by stamping the
// log line with the launch's marker line) or refuses to recover.
func TestReconcileBindsRecoveredPidToExactLaunchAttemptNotFirstLogMarker(t *testing.T) {
	s := newStore(t)
	seedReadyExecuteWork(t, s, "work_pid_bind")
	if _, err := s.Claim("work_pid_bind", "claude-minimax", fixedNow); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "work_pid_bind.door.log")
	// Two launches wrote to this log (O_APPEND, so both survive).
	// A stale prior launch's pid (99999) precedes a fresh launch's
	// marker + new pid (888). The reconciler must adopt 888 — the
	// marker scopes the recovery to the most recent launch.
	logBody := "2026-09-11T18:00:00Z detached door start work=work_pid_bind lane=claude-minimax actor=claude-minimax\n" +
		"2026-09-11T18:00:00Z " + DetachedDoorStartedLogLine + "99999 pgid=99999\n" +
		"2026-09-11T18:30:00Z detached door launch marker work=work_pid_bind started_at=2026-09-11T18:30:00.000Z\n" +
		"2026-09-11T18:30:00Z " + DetachedDoorStartedLogLine + "888 pgid=888\n"
	if err := os.WriteFile(logPath, []byte(logBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteDetachedLaunch(s.home, DetachedLaunch{
		WorkID:    "work_pid_bind",
		Lane:      "claude-minimax",
		Actor:     "claude-minimax",
		LogPath:   logPath,
		StartedAt: fixedNow.UTC().Format(time.RFC3339),
		Outcome:   DetachedOutcomeReserved,
	}); err != nil {
		t.Fatal(err)
	}
	prevAlive := detachedProcessAlive
	detachedProcessAlive = func(pid int) bool { return pid == 888 }
	t.Cleanup(func() { detachedProcessAlive = prevAlive })

	verdicts := s.ReconcileDetachedLaunches(fixedNow.Add(time.Minute))
	// The reconcile MUST adopt the NEW pid (888), not the stale one
	// (99999). The chosen verdict shape is running (the door is alive).
	if len(verdicts) != 1 || verdicts[0].Outcome != DetachedOutcomeRunning || verdicts[0].PID != 888 {
		t.Fatalf("reconcile must adopt the new launch's pid (888), not the stale prior-launch pid (99999); got %+v", verdicts)
	}
	sidecar, _ := ReadDetachedLaunch(s.home, "work_pid_bind")
	if sidecar.PID != 888 {
		t.Fatalf("sidecar PID = %d, want 888 (the current launch's pid)", sidecar.PID)
	}
	if sidecar.Outcome != DetachedOutcomeRunning {
		t.Fatalf("sidecar outcome = %q, want running", sidecar.Outcome)
	}
}
func TestReconcileRevokesUnstartedExpiredReservation(t *testing.T) {
	s := newStore(t)
	seedReadyExecuteWork(t, s, "work_reserved_expired")
	if _, err := s.Claim("work_reserved_expired", "claude-minimax", fixedNow); err != nil {
		t.Fatal(err)
	}
	if err := WriteDetachedLaunch(s.home, DetachedLaunch{
		WorkID:    "work_reserved_expired",
		LaunchID:  "dl_reserved_never_started",
		Lane:      "claude-minimax",
		Actor:     "claude-minimax",
		LogPath:   "/tmp/empty-expired-door.log",
		StartedAt: fixedNow.UTC().Format(time.RFC3339),
		Outcome:   DetachedOutcomeReserved,
	}); err != nil {
		t.Fatal(err)
	}
	prevAlive := detachedProcessAlive
	detachedProcessAlive = func(int) bool { return false }
	t.Cleanup(func() { detachedProcessAlive = prevAlive })

	verdicts := s.ReconcileDetachedLaunches(fixedNow.Add(24 * time.Hour))
	if len(verdicts) != 1 || verdicts[0].Outcome != DetachedOutcomeDoorLost {
		t.Fatalf("an expired reserved launch with no PID and no log marker must classify as door_lost; got %+v", verdicts)
	}
}
