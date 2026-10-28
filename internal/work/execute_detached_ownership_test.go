package work

import (
	"encoding/json"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Review repair 2026-09-19 follow-up (detached): recovery may not classify a
// reservation lost on evidence it has not made durable, and it may not decide
// ownership on elapsed time.
//
// The lost path used to fail the envelope FIRST and then write the sidecar
// with the error discarded. A failed envelope behind a sidecar that still
// reads `reserved` is the delayed-owner hole read from the other end. The
// order is now revoke-durably, stamp-durably, then close.
//
// And the launcher's ownership is proven by a HELD kernel lock rather than by
// a window: a flock survives the holder being suspended and is released by the
// kernel when the holder dies, which is exactly the distinction a timeout
// cannot draw.

// reserveDetachedLaunchForOwnership claims an envelope and leaves a reserved
// sidecar with no started pid: the shape a delayed launcher leaves behind.
func reserveDetachedLaunchForOwnership(t *testing.T, s *Store, workID, launchID string) {
	t.Helper()
	seedReadyExecuteWork(t, s, workID)
	if _, err := s.Claim(workID, "claude-minimax", fixedNow); err != nil {
		t.Fatal(err)
	}
	if err := WriteDetachedLaunch(s.home, DetachedLaunch{
		WorkID:    workID,
		LaunchID:  launchID,
		Lane:      "claude-minimax",
		Actor:     "claude-minimax",
		LogPath:   t.TempDir() + "/" + workID + ".door.log",
		StartedAt: fixedNow.UTC().Format(time.RFC3339),
		Outcome:   DetachedOutcomeReserved,
	}); err != nil {
		t.Fatal(err)
	}
}

// writeDetachedDecisionRecord lays down the decision a lock holder would have
// written, so these falsifiers read the same bytes production reads.
func writeDetachedDecisionRecord(t *testing.T, home, workID, launchID, decision string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(DetachedLaunchDir(home), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(detachedOwnershipToken{
		LaunchID:  launchID,
		WorkID:    workID,
		Decision:  decision,
		DecidedAt: at.UTC().Format(time.RFC3339Nano),
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(detachedOwnershipTokenPath(home, launchID), append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readDetachedDecisionRecord(t *testing.T, home, launchID string) detachedOwnershipToken {
	t.Helper()
	body, err := os.ReadFile(detachedOwnershipTokenPath(home, launchID))
	if err != nil {
		t.Fatalf("decision record unreadable: %v", err)
	}
	var token detachedOwnershipToken
	if err := json.Unmarshal(body, &token); err != nil {
		t.Fatalf("decision record unparseable: %v", err)
	}
	return token
}

// holdDetachedOwnershipLock takes the launch id's lock from a SEPARATE open
// file description — which is what makes flock exclude the reconciler even
// inside one test process, so these falsifiers need no second process, no
// sleep and no goroutine. Released when the test ends.
func holdDetachedOwnershipLock(t *testing.T, home, launchID string) {
	t.Helper()
	if err := os.MkdirAll(DetachedLaunchDir(home), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(detachedOwnershipTokenPath(home, launchID), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		t.Fatalf("the falsifier could not take the ownership lock it needs to hold: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })
}

// Falsifier 8 — a HELD lock is not overridable by any clock. The launcher
// decided `start` and is suspended inside its critical section (a host
// sleep/resume, a SIGSTOP, a wedged disk). No amount of elapsed time makes it
// safe to classify: recovery must report the held lock and touch nothing.
func TestRecoveryClassifiesNothingWhileTheLauncherHoldsTheOwnershipLock(t *testing.T) {
	s := newStore(t)
	reserveDetachedLaunchForOwnership(t, s, "work_lock_held", "dl_lock_held")
	writeDetachedDecisionRecord(t, s.home, "work_lock_held", "dl_lock_held", DetachedDecisionStart, fixedNow)
	holdDetachedOwnershipLock(t, s.home, "dl_lock_held")
	prevAlive := detachedProcessAlive
	detachedProcessAlive = func(int) bool { return false }
	t.Cleanup(func() { detachedProcessAlive = prevAlive })

	// A full day past the reservation grace, with no started pid in the log.
	verdicts := s.ReconcileDetachedLaunches(fixedNow.Add(24 * time.Hour))
	if len(verdicts) != 1 || verdicts[0].Outcome != DetachedOutcomeReserved {
		t.Fatalf("a held ownership lock must not be overridden by elapsed time; got %+v", verdicts)
	}
	if !strings.Contains(verdicts[0].Detail, "holds the ownership lock") {
		t.Fatalf("the verdict must name the held lock, got %q", verdicts[0].Detail)
	}
	if w, _ := s.Get("work_lock_held"); w.State != "claimed" {
		t.Fatalf("envelope state = %q; a launcher inside its critical section keeps its envelope", w.State)
	}
	sidecar, _ := ReadDetachedLaunch(s.home, "work_lock_held")
	if sidecar.Outcome != DetachedOutcomeReserved {
		t.Fatalf("sidecar outcome = %q; nothing may be stamped past a held lock", sidecar.Outcome)
	}
}

// A released launcher lock is not proof of a side-effect-free attempt: the
// owner can die immediately before OR immediately after Start.
func TestRecoveryRetainsUnrecordedStartWhenTheOwnershipLockIsFree(t *testing.T) {
	s := newStore(t)
	reserveDetachedLaunchForOwnership(t, s, "work_lock_free", "dl_lock_free")
	writeDetachedDecisionRecord(t, s.home, "work_lock_free", "dl_lock_free", DetachedDecisionStart, fixedNow)
	verdicts := s.ReconcileDetachedLaunches(fixedNow.Add(DetachedReservationGrace + time.Minute))
	if len(verdicts) != 1 || verdicts[0].Outcome != DetachedOutcomeRecoveryRequired {
		t.Fatalf("released lock cannot settle whether a child started: %+v", verdicts)
	}
	if w, _ := s.Get("work_lock_free"); w.State != "claimed" {
		t.Fatalf("uncertain start changed envelope to %s", w.State)
	}
	if _, owned := DetachedDoorOwns(s.home, "work_lock_free"); !owned {
		t.Fatal("uncertain start lost its reservation")
	}
}

// Falsifier 6 — an undurable revoke must classify nothing. Both durable steps
// are exercised: the decision itself, and the sidecar stamp that makes the
// decision readable to every other party.
func TestReconcileClassifiesNothingOnAnUndurableRevoke(t *testing.T) {
	t.Run("the decision cannot be taken", func(t *testing.T) {
		s := newStore(t)
		reserveDetachedLaunchForOwnership(t, s, "work_undurable_decision", "dl_undurable_decision")
		prevAlive := detachedProcessAlive
		detachedProcessAlive = func(int) bool { return false }
		prevOpen := detachedOwnershipOpenFn
		detachedOwnershipOpenFn = func(string) (*os.File, error) { return nil, os.ErrPermission }
		t.Cleanup(func() {
			detachedProcessAlive = prevAlive
			detachedOwnershipOpenFn = prevOpen
		})

		verdicts := s.ReconcileDetachedLaunches(fixedNow.Add(24 * time.Hour))
		if len(verdicts) != 1 || verdicts[0].Outcome != DetachedOutcomeReserved {
			t.Fatalf("a revoke that cannot be made durable must classify nothing; got %+v", verdicts)
		}
		if !strings.Contains(verdicts[0].Detail, "could not be made durable") {
			t.Fatalf("the verdict must report the undurable revoke, got %q", verdicts[0].Detail)
		}
		w, _ := s.Get("work_undurable_decision")
		if w.State != "claimed" {
			t.Fatalf("envelope state = %q; an unclassified reservation must stay claimed", w.State)
		}
		sidecar, _ := ReadDetachedLaunch(s.home, "work_undurable_decision")
		if sidecar.Outcome != DetachedOutcomeReserved {
			t.Fatalf("sidecar outcome = %q; nothing may be stamped without a durable revoke", sidecar.Outcome)
		}
	})

	t.Run("the sidecar stamp cannot be made durable", func(t *testing.T) {
		s := newStore(t)
		reserveDetachedLaunchForOwnership(t, s, "work_undurable_stamp", "dl_undurable_stamp")
		prevAlive := detachedProcessAlive
		detachedProcessAlive = func(int) bool { return false }
		prevWrite := reconcileWriteDetachedLaunch
		reconcileWriteDetachedLaunch = func(string, DetachedLaunch) error { return os.ErrPermission }
		t.Cleanup(func() {
			detachedProcessAlive = prevAlive
			reconcileWriteDetachedLaunch = prevWrite
		})

		verdicts := s.ReconcileDetachedLaunches(fixedNow.Add(24 * time.Hour))
		if len(verdicts) != 1 || verdicts[0].Outcome != DetachedOutcomeReserved {
			t.Fatalf("an undurable sidecar stamp must classify nothing; got %+v", verdicts)
		}
		if !strings.Contains(verdicts[0].Detail, "sidecar stamp is not") {
			t.Fatalf("the verdict must report the undurable stamp, got %q", verdicts[0].Detail)
		}
		w, _ := s.Get("work_undurable_stamp")
		if w.State != "claimed" {
			t.Fatalf("envelope state = %q; the envelope must NOT be failed behind an unstamped sidecar", w.State)
		}
		// The revoke itself IS durable, and it was taken before any close was
		// attempted — that ordering is what a later tick resumes from.
		if token := readDetachedDecisionRecord(t, s.home, "dl_undurable_stamp"); token.Decision != DetachedDecisionRevoke {
			t.Fatalf("token decision = %q, want revoke", token.Decision)
		}
	})
}

// A legacy id-less reservation has no launch id to name a lock with, so no
// ownership guarantee is claimed for it: it keeps its pre-2026-09-19 path and
// leaves no decision record behind. Stated here so the guarantee is not read
// as covering sidecars it cannot cover.
func TestLegacyIdlessReservationTakesNoOwnershipDecision(t *testing.T) {
	s := newStore(t)
	seedReadyExecuteWork(t, s, "work_legacy_no_token")
	if _, err := s.Claim("work_legacy_no_token", "claude-minimax", fixedNow); err != nil {
		t.Fatal(err)
	}
	if err := WriteDetachedLaunch(s.home, DetachedLaunch{
		WorkID:    "work_legacy_no_token",
		Lane:      "claude-minimax",
		Actor:     "claude-minimax",
		LogPath:   t.TempDir() + "/work_legacy_no_token.door.log",
		StartedAt: fixedNow.UTC().Format(time.RFC3339),
		Outcome:   DetachedOutcomeReserved,
	}); err != nil {
		t.Fatal(err)
	}
	prevAlive := detachedProcessAlive
	detachedProcessAlive = func(int) bool { return false }
	prevOpen := detachedOwnershipOpenFn
	detachedOwnershipOpenFn = func(string) (*os.File, error) {
		t.Fatal("a legacy id-less reservation must not take an ownership decision")
		return nil, nil
	}
	t.Cleanup(func() {
		detachedProcessAlive = prevAlive
		detachedOwnershipOpenFn = prevOpen
	})

	verdicts := s.ReconcileDetachedLaunches(fixedNow.Add(24 * time.Hour))
	if len(verdicts) != 1 || verdicts[0].Outcome != DetachedOutcomeRecoveryRequired {
		t.Fatalf("legacy absence of pid cannot prove no start: %+v", verdicts)
	}
	if w, _ := s.Get("work_legacy_no_token"); w.State != "claimed" {
		t.Fatalf("legacy uncertainty changed envelope to %s", w.State)
	}
}
