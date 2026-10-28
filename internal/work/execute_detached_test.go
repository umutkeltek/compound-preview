package work

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// R6P falsifiers. The detached seam must (1) leave the envelope CLAIMED and
// never call the synchronous launch, (2) never re-launch that envelope on the
// next tick, (3) close a lost door's envelope failed exactly once, (4) record
// a door that finalized without touching the envelope, (5) fail only the
// envelope whose door failed to spawn.

func installDetachedGreenGates(t *testing.T, s *Store) {
	t.Helper()
	installExecuteGrant(t, s.home, true)
	restoreAllExecuteHooks(t)
	executeStageRoutePlanFn = func(home, capability string) (string, bool) { return "claude-minimax", true }
	executeStageCapacityFn = func(home, lane string, now time.Time) (float64, bool, error) { return 30, true, nil }
	executeStagePolicyEvalFn = func(ai policyActionIntentShim, cfg policyConfigShim, now time.Time) policyDecisionShim {
		return policyDecisionShim{Outcome: "allow", ID: "pol_test_allow"}
	}
	launchStub = func(_ *Store, workID, _, _, _, _ string, _ time.Time) (string, error) {
		t.Fatalf("synchronous launch must not run in detached mode (work %s)", workID)
		return "", nil
	}
}

func TestDetachedLaunchLeavesEnvelopeClaimedAndSkipsSynchronousLaunch(t *testing.T) {
	s := newStore(t)
	seedReadyExecuteWork(t, s, "work_detached_ok")
	installDetachedGreenGates(t, s)
	var spawned DetachedLaunch
	restore := SwapExecuteStageDetachedLaunch(func(store *Store, workID, lane, prompt, actor string, now time.Time) (DetachedLaunch, error) {
		w, err := store.Get(workID)
		if err != nil {
			return DetachedLaunch{}, err
		}
		if !strings.HasPrefix(w.State, "claimed") {
			return DetachedLaunch{}, errors.New("spawner saw state " + w.State + ", want claimed")
		}
		spawned = DetachedLaunch{WorkID: workID, Lane: lane, Actor: actor, PID: 424242, PGID: 424242, LogPath: "/tmp/door.log", BudgetSeconds: 2700, StartedAt: now.UTC().Format(time.RFC3339), Outcome: DetachedOutcomeRunning}
		return spawned, WriteDetachedLaunch(store.home, spawned)
	})
	t.Cleanup(restore)
	prevAlive := detachedProcessAlive
	detachedProcessAlive = func(pid int) bool { return pid == 424242 }
	t.Cleanup(func() { detachedProcessAlive = prevAlive })

	res, err := s.Continue(fixedNow.Add(time.Minute), ContinueOptions{Actor: "claude-minimax", MaxExecuteSteps: 1})
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if res.Action != "execute" || res.Status != "claimed" {
		t.Fatalf("action=%q status=%q, want execute/claimed; human=%s", res.Action, res.Status, res.Human)
	}
	if res.Detached == nil || res.Detached.PID != 424242 || res.Detached.WorkID != "work_detached_ok" {
		t.Fatalf("detached identity missing on the result: %+v", res.Detached)
	}
	if len(res.RunAttemptIDs) != 0 {
		t.Fatalf("detached launch must not report a ratt on the launching tick: %v", res.RunAttemptIDs)
	}
	if res.Execute == nil || res.Execute.Detached == nil || res.Execute.Detached.PID != 424242 {
		t.Fatalf("verdict must carry the detached identity: %+v", res.Execute)
	}
	if !strings.Contains(res.Human, "detached governed door") || !strings.Contains(res.Human, "424242") {
		t.Fatalf("human line must name the door: %s", res.Human)
	}
	after, _ := s.Get("work_detached_ok")
	if !strings.HasPrefix(after.State, "claimed") {
		t.Fatalf("envelope state = %q, want claimed (the door owns running)", after.State)
	}
	if got, err := ReadDetachedLaunch(s.home, "work_detached_ok"); err != nil || got.PID != 424242 {
		t.Fatalf("sidecar not durable: %+v err=%v", got, err)
	}

	// Tick 2: the claimed envelope is not a candidate; nothing is re-launched,
	// and the still-alive door is reported as running.
	spawnCalls := 0
	restore2 := SwapExecuteStageDetachedLaunch(func(*Store, string, string, string, string, time.Time) (DetachedLaunch, error) {
		spawnCalls++
		return DetachedLaunch{}, errors.New("must not spawn twice")
	})
	t.Cleanup(restore2)
	res2, err := s.Continue(fixedNow.Add(2*time.Minute), ContinueOptions{Actor: "claude-minimax", MaxExecuteSteps: 1})
	if err != nil {
		t.Fatalf("continue tick 2: %v", err)
	}
	if spawnCalls != 0 {
		t.Fatalf("tick 2 re-launched the detached envelope (%d spawns)", spawnCalls)
	}
	if res2.Detached != nil {
		t.Fatalf("tick 2 must not report a new detached launch: %+v", res2.Detached)
	}
	if len(res2.DetachedReconciled) != 1 || res2.DetachedReconciled[0].Outcome != DetachedOutcomeRunning {
		t.Fatalf("tick 2 must reconcile the alive door as running: %+v", res2.DetachedReconciled)
	}
	still, _ := s.Get("work_detached_ok")
	if !strings.HasPrefix(still.State, "claimed") {
		t.Fatalf("tick 2 mutated the detached envelope: %q", still.State)
	}
}

func TestReconcileClosesLostDoorThatNeverReachedRunning(t *testing.T) {
	s := newStore(t)
	seedReadyExecuteWork(t, s, "work_door_lost")
	if _, err := s.Claim("work_door_lost", "claude-minimax", fixedNow); err != nil {
		t.Fatal(err)
	}
	if err := WriteDetachedLaunch(s.home, DetachedLaunch{WorkID: "work_door_lost", Lane: "claude-minimax", Actor: "claude-minimax", PID: 777, PGID: 777, LogPath: "/tmp/door.log", StartedAt: "2026-09-11T19:00:00Z", Outcome: DetachedOutcomeRunning}); err != nil {
		t.Fatal(err)
	}
	prevAlive := detachedProcessAlive
	detachedProcessAlive = func(int) bool { return false }
	t.Cleanup(func() { detachedProcessAlive = prevAlive })

	verdicts := s.ReconcileDetachedLaunches(fixedNow.Add(time.Hour))
	if len(verdicts) != 1 || verdicts[0].Outcome != DetachedOutcomeDoorLost {
		t.Fatalf("verdicts = %+v, want one door_lost", verdicts)
	}
	after, _ := s.Get("work_door_lost")
	if after.State != "failed" {
		t.Fatalf("lost door must close its envelope failed, got %q (%s)", after.State, verdicts[0].Detail)
	}
	sidecar, _ := ReadDetachedLaunch(s.home, "work_door_lost")
	if sidecar.Outcome != DetachedOutcomeDoorLost || sidecar.ExitObservedAt == "" {
		t.Fatalf("sidecar not stamped: %+v", sidecar)
	}
	// Second reconcile is a no-op: nothing is closed twice, nothing re-reported.
	if again := s.ReconcileDetachedLaunches(fixedNow.Add(2 * time.Hour)); len(again) != 0 {
		t.Fatalf("second reconcile must be a no-op, got %+v", again)
	}
}

func TestReconcileRecordsDoorFinalizedWithoutTouchingTheEnvelope(t *testing.T) {
	s := newStore(t)
	seedReadyExecuteWork(t, s, "work_door_done")
	if _, err := s.Claim("work_door_done", "claude-minimax", fixedNow); err != nil {
		t.Fatal(err)
	}
	// The door's own lifecycle: running then verifying.
	if _, err := s.Advance("work_door_done", AdvanceInput{To: "running", Actor: "claude-minimax", Note: "door: running"}, fixedNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance("work_door_done", AdvanceInput{To: "verifying", Actor: "claude-minimax", Note: "door: verifying"}, fixedNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := WriteDetachedLaunch(s.home, DetachedLaunch{WorkID: "work_door_done", Lane: "claude-minimax", Actor: "claude-minimax", PID: 778, PGID: 778, StartedAt: "2026-09-11T19:00:00Z", Outcome: DetachedOutcomeRunning}); err != nil {
		t.Fatal(err)
	}
	prevAlive := detachedProcessAlive
	detachedProcessAlive = func(int) bool { return false }
	t.Cleanup(func() { detachedProcessAlive = prevAlive })
	before, _ := s.Get("work_door_done")

	verdicts := s.ReconcileDetachedLaunches(fixedNow.Add(time.Hour))
	if len(verdicts) != 1 || verdicts[0].Outcome != DetachedOutcomeDoorFinalized {
		t.Fatalf("verdicts = %+v, want one door_finalized", verdicts)
	}
	after, _ := s.Get("work_door_done")
	if after.State != before.State || after.State != "verifying" {
		t.Fatalf("finalized door's envelope was touched: before=%q after=%q", before.State, after.State)
	}
}

func TestDetachedSpawnFailureFailsOnlyThatEnvelope(t *testing.T) {
	s := newStore(t)
	seedReadyExecuteWork(t, s, "work_spawn_fail")
	seedReadyExecuteWork(t, s, "work_spawn_other")
	installDetachedGreenGates(t, s)
	restore := SwapExecuteStageDetachedLaunch(func(_ *Store, workID, _, _, _ string, _ time.Time) (DetachedLaunch, error) {
		return DetachedLaunch{}, errors.New("no GNU timeout wrapper on PATH")
	})
	t.Cleanup(restore)

	res, err := s.Continue(fixedNow.Add(time.Minute), ContinueOptions{Actor: "claude-minimax", MaxExecuteSteps: 1})
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if res.Action != "execute" || res.Skipped != "launch_failed" {
		t.Fatalf("action=%q skipped=%q, want execute/launch_failed; human=%s", res.Action, res.Skipped, res.Human)
	}
	if !strings.Contains(res.Human, "failed to spawn") {
		t.Fatalf("human line must say the door failed to spawn: %s", res.Human)
	}
	failed, _ := s.Get(res.WorkID)
	if failed.State != "failed" {
		t.Fatalf("spawn failure must fail that envelope, got %q", failed.State)
	}
	otherID := "work_spawn_other"
	if res.WorkID == otherID {
		otherID = "work_spawn_fail"
	}
	other, _ := s.Get(otherID)
	if other.State != "ready" {
		t.Fatalf("the other envelope must be untouched, got %q", other.State)
	}
}

// Start/record contract 2026-09-15: a typed record failure is
// recovery_required — the door started and owns the envelope, so the stage
// must NOT fail-close it (the pre-2026-09-15 bug closed the work as
// "failed to spawn" while the child ran on).
func TestDetachedPostStartRecordFailureKeepsEnvelopeClaimed(t *testing.T) {
	s := newStore(t)
	seedReadyExecuteWork(t, s, "work_record_recovery")
	installDetachedGreenGates(t, s)
	restore := SwapExecuteStageDetachedLaunch(func(_ *Store, workID, lane, _, actor string, now time.Time) (DetachedLaunch, error) {
		launch := DetachedLaunch{WorkID: workID, Lane: lane, Actor: actor, PID: 424243, PGID: 424243, LogPath: "/tmp/door.log", BudgetSeconds: 2700, StartedAt: now.UTC().Format(time.RFC3339), Outcome: DetachedOutcomeRunning}
		return launch, &DetachedLaunchRecordError{Launch: launch, Err: errors.New("disk full: write detached launch")}
	})
	t.Cleanup(restore)

	res, err := s.Continue(fixedNow.Add(time.Minute), ContinueOptions{Actor: "claude-minimax", MaxExecuteSteps: 1})
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if res.Action != "execute" || res.Skipped != "launch_record_recovery" {
		t.Fatalf("action=%q skipped=%q, want execute/launch_record_recovery; human=%s", res.Action, res.Skipped, res.Human)
	}
	if res.Detached == nil || res.Detached.PID != 424243 {
		t.Fatalf("recovery result must carry the started identity: %+v", res.Detached)
	}
	after, _ := s.Get("work_record_recovery")
	if !strings.HasPrefix(after.State, "claimed") {
		t.Fatalf("recovery_required must leave the envelope claimed for the door, got %q", after.State)
	}
	if !strings.Contains(res.Human, "pid 424243") || !strings.Contains(res.Human, "start evidence could not be durably recorded") {
		t.Fatalf("human line must name the pid and the recovery: %s", res.Human)
	}
}
