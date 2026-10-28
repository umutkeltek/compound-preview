package executewire

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/work"
)

// Review repair 2026-09-19 follow-up (detached): a process can be descheduled
// at ANY instruction — a host sleep/resume does exactly that — so a guard that
// READS the sidecar and then starts a child only narrows the window in which
// recovery classifies the reservation lost between those two steps. This
// falsifier puts a real expired-grace reconcile INSIDE that window and asserts
// the invariant itself rather than the size of the window:
//
//	NOT (a child started for launch L  AND  launch L was classified lost).
//
// The window is entered deterministically through detachedAfterStartDecision —
// no sleep, no goroutine, no timing luck.
func TestDetachedOwnershipIsExclusiveWhenRecoveryFiresInsideTheStartWindow(t *testing.T) {
	if _, err := DetachedTimeoutBinary(); err != nil {
		t.Skip("no GNU timeout on PATH: " + err.Error())
	}
	homePath := t.TempDir()
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	seedClaimedDetachedWork(t, store, "work_detached_window", targetRoot)

	ranMarker := filepath.Join(t.TempDir(), "door-ran.txt")
	fakeDoor := filepath.Join(t.TempDir(), "fake-compound")
	if err := os.WriteFile(fakeDoor, []byte("#!/bin/sh\necho ran > "+ranMarker+"\nsleep 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prevDoor, prevBudget := DetachedDoorBinary, DefaultDetachedLaunchBudget
	DetachedDoorBinary = func() (string, error) { return fakeDoor, nil }
	DefaultDetachedLaunchBudget = 30 * time.Second
	wasInstalled := AssertLaunchContextInstalled() == nil
	SetLaunchContextInstalled(true)

	now := time.Now().UTC()
	var verdicts []work.DetachedReconcile
	prevHook := detachedAfterStartDecision
	detachedAfterStartDecision = func() {
		// The launcher is descheduled here, one step short of cmd.Start().
		// Recovery runs for real, with its grace window already expired.
		verdicts = store.ReconcileDetachedLaunches(now.Add(work.DetachedReservationGrace + time.Minute))
	}
	t.Cleanup(func() {
		detachedAfterStartDecision = prevHook
		DetachedDoorBinary = prevDoor
		DefaultDetachedLaunchBudget = prevBudget
		SetLaunchContextInstalled(wasInstalled)
	})

	launch, launchErr := LaunchDetachedViaDoor(store, "work_detached_window", "claude_minimax", "p", "claude-minimax", now)
	childStarted := launchErr == nil && launch.PID > 0

	classifiedLost := false
	for _, v := range verdicts {
		if v.Outcome == work.DetachedOutcomeDoorLost || v.Outcome == work.DetachedOutcomeDoorLostRunning {
			classifiedLost = true
		}
	}
	w, err := store.Get("work_detached_window")
	if err != nil {
		t.Fatal(err)
	}
	envelopeFailed := w.State == "failed"

	// THE INVARIANT.
	if childStarted && (classifiedLost || envelopeFailed) {
		t.Fatalf("both sides won: a child (pid %d) was started for launch %s while recovery classified it lost (verdicts %+v, envelope %s)", launch.PID, launch.LaunchID, verdicts, w.State)
	}
	// ...and exactly one side must have won: a stall where neither started nor
	// classified is its own failure, not a pass.
	if !childStarted && !classifiedLost {
		t.Fatalf("neither side won: launch err=%v, verdicts=%+v, envelope %s", launchErr, verdicts, w.State)
	}
	// The loser must have observed the WINNER's decision, not its own read.
	if childStarted {
		if len(verdicts) != 1 || verdicts[0].Outcome != work.DetachedOutcomeReserved {
			t.Fatalf("recovery could not take the lock and must leave the reservation alone; got %+v", verdicts)
		}
		if !strings.Contains(verdicts[0].Detail, "holds the ownership lock") {
			t.Fatalf("the losing verdict must name the held ownership lock, got %q", verdicts[0].Detail)
		}
		if w.State != "claimed" {
			t.Fatalf("envelope state = %q; a started child keeps its envelope claimed for the door", w.State)
		}
		logBody, _ := os.ReadFile(launch.LogPath)
		if !strings.Contains(string(logBody), work.DetachedDoorStartedLogLine) {
			t.Fatalf("a started child must leave its pid line in the log:\n%s", logBody)
		}
	}
}

// Falsifier 10 — a launcher that arrives after recovery has decided `revoke`
// refuses, through the lock path. The sidecar still reads `reserved` here, so
// the cheap pre-check passes and the refusal can only come from the decision
// taken under the lock: recovery's record is what the launcher reads.
func TestLaunchDetachedViaDoorRefusesWhenRecoveryAlreadyDecidedRevoke(t *testing.T) {
	if _, err := DetachedTimeoutBinary(); err != nil {
		t.Skip("no GNU timeout on PATH: " + err.Error())
	}
	homePath := t.TempDir()
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	seedClaimedDetachedWork(t, store, "work_detached_revoked", targetRoot)

	ranMarker := filepath.Join(t.TempDir(), "door-ran.txt")
	fakeDoor := filepath.Join(t.TempDir(), "fake-compound")
	if err := os.WriteFile(fakeDoor, []byte("#!/bin/sh\necho ran > "+ranMarker+"\nsleep 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prevDoor, prevBudget := DetachedDoorBinary, DefaultDetachedLaunchBudget
	DetachedDoorBinary = func() (string, error) { return fakeDoor, nil }
	DefaultDetachedLaunchBudget = 30 * time.Second
	wasInstalled := AssertLaunchContextInstalled() == nil
	SetLaunchContextInstalled(true)

	now := time.Now().UTC()
	prevWrite := writeDetachedLaunch
	writeDetachedLaunch = func(home string, reserved work.DetachedLaunch) error {
		if err := prevWrite(home, reserved); err != nil {
			return err
		}
		if reserved.Outcome == work.DetachedOutcomeReserved {
			// Recovery reaches the decision first, while this launcher is
			// still between its reserved write and its own decision. Only
			// the decision record is written — the sidecar stays `reserved`
			// so the pre-check cannot be what refuses.
			decision, decErr := work.TakeDetachedRevokeOwnership(home, reserved.WorkID, reserved.LaunchID, now)
			if decErr != nil || decision != work.DetachedDecisionRevoke {
				t.Fatalf("the falsifier needs recovery to win the free lock: %q %v", decision, decErr)
			}
		}
		return nil
	}
	t.Cleanup(func() {
		writeDetachedLaunch = prevWrite
		DetachedDoorBinary = prevDoor
		DefaultDetachedLaunchBudget = prevBudget
		SetLaunchContextInstalled(wasInstalled)
	})

	_, err = LaunchDetachedViaDoor(store, "work_detached_revoked", "claude_minimax", "p", "claude-minimax", now)
	if err == nil {
		t.Fatal("a launcher whose launch id was already revoked must refuse to start")
	}
	if !strings.Contains(err.Error(), "revoke") {
		t.Fatalf("the refusal must name the revoked decision, got %v", err)
	}
	if _, statErr := os.Stat(ranMarker); !os.IsNotExist(statErr) {
		t.Fatalf("a child was started for a revoked launch id (marker stat: %v)", statErr)
	}
	// The pre-check could not have refused: the sidecar still reads reserved.
	stored, readErr := work.ReadDetachedLaunch(homePath, "work_detached_revoked")
	if readErr != nil || stored.Outcome != work.DetachedOutcomeReserved {
		t.Fatalf("this falsifier only proves the lock path if the sidecar still reads reserved: %+v err=%v", stored, readErr)
	}
}
