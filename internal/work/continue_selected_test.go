package work

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSelectedCloseRejectsChangedVerificationGeneration(t *testing.T) {
	s := newStore(t)
	w := runningWork(t, s, "selected close requires the verified generation")
	trace := writeTrace(t, s, "trace_selected_close", w.ID)
	observed, err := s.ObserveRun(w.ID, ObserveRunInput{Actor: "codex", TraceID: trace.ID}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	frozen := observed.Work
	newer, err := s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "codex", Note: "concurrent generation"}, fixedNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DispatchClose(fixedNow.Add(2*time.Second), DispatchCloseOptions{
		WorkID: w.ID, Actor: "codex", ExpectedWorkState: frozen.State,
		ExpectedPolicyDecisionID: frozen.PolicyDecisionID, ExpectedActionIntentID: selectedLastAction(frozen),
	})
	if !errors.Is(err, ErrExecutionReservationChanged) {
		t.Fatalf("stale verification accepted: %v", err)
	}
	after, err := s.Get(w.ID)
	if err != nil || !reflect.DeepEqual(newer, after) {
		t.Fatalf("newer generation changed: %v", err)
	}
}

func TestSelectedVerificationRequiresVerifyingAndHonorsDryRun(t *testing.T) {
	s := newStore(t)
	seedReadyExecuteWork(t, s, "work_selected_verify")
	w, err := s.Claim("work_selected_verify", "executor", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, opts := range []ContinueOptions{
		{Actor: "executor", VerificationRef: "check:unselected"},
		{Selected: true, Actor: "executor", VerificationRef: "check:claimed"},
		{Selected: true, Resume: true, Actor: "executor", VerificationRef: "check:resume"},
	} {
		if _, err := s.Continue(fixedNow, opts); err == nil {
			t.Fatalf("invalid verification accepted: %+v", opts)
		}
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "executor"}, fixedNow); err != nil {
		t.Fatal(err)
	}
	before, err := s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "executor"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Continue(fixedNow, ContinueOptions{Selected: true, Actor: "executor", VerificationRef: "check:dry", DryRun: true}); err != nil {
		t.Fatal(err)
	}
	after, err := s.Get(w.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("dry run changed verification: %v", err)
	}
}

func TestSelectedContinueRefusesWrongActorMissingSelectionAndAmbiguity(t *testing.T) {
	s := newStore(t)
	seedReadyExecuteWork(t, s, "work_selected_one")
	if _, err := s.Claim("work_selected_one", "executor", fixedNow); err != nil {
		t.Fatal(err)
	}
	before, err := s.Get("work_selected_one")
	if err != nil {
		t.Fatal(err)
	}
	for _, opts := range []ContinueOptions{
		{Selected: true, Actor: "intruder", ExpectWorkID: before.ID},
		{Selected: true, Actor: "executor", ExpectWorkID: before.ID},
		{Selected: true, ExpectWorkID: before.ID},
		{Resume: true, Actor: "executor", ExpectWorkID: before.ID},
	} {
		if _, err := s.Continue(fixedNow.Add(time.Minute), opts); err == nil {
			t.Fatalf("unsafe options accepted: %+v", opts)
		}
		after, err := s.Get(before.ID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("refusal changed work: %v %+v", err, after)
		}
	}
	seedReadyExecuteWork(t, s, "work_selected_two")
	if _, err := s.Claim("work_selected_two", "executor", fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Continue(fixedNow.Add(time.Minute), ContinueOptions{Selected: true, Actor: "executor"}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguity was queue-picked: %v", err)
	}
}

func TestSelectedContinueUnreadableLaunchNeverRedispatches(t *testing.T) {
	s := newStore(t)
	seedReadyExecuteWork(t, s, "work_selected_corrupt")
	if _, err := s.Claim("work_selected_corrupt", "executor", fixedNow); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(DetachedLaunchDir(s.home), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(detachedLaunchPath(s.home, "work_selected_corrupt"), []byte("unreadable"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Continue(fixedNow, ContinueOptions{Selected: true, Actor: "executor", ExpectWorkID: "work_selected_corrupt"}); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("corrupt launch accepted: %v", err)
	}
}

func TestSelectedReconcileDoesNotMistakePriorVerifyingForNewExecution(t *testing.T) {
	s := newStore(t)
	seedReadyExecuteWork(t, s, "work_selected_resume")
	seedReadyExecuteWork(t, s, "work_unrelated_door")
	for _, id := range []string{"work_selected_resume", "work_unrelated_door"} {
		if _, err := s.Claim(id, "executor", fixedNow); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Advance("work_selected_resume", AdvanceInput{To: "running", Actor: "executor"}, fixedNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance("work_selected_resume", AdvanceInput{To: "verifying", Actor: "executor"}, fixedNow.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	before, err := s.Get("work_selected_resume")
	if err != nil {
		t.Fatal(err)
	}
	launch := DetachedLaunch{WorkID: before.ID, LaunchID: "dl_selected_resume", Explicit: true, SourceRevision: ExecutionSourceRevision(before), SourceRunAttemptIDs: before.RunAttemptIDs, Actor: "executor", Lane: "fixture", PID: 123, Outcome: DetachedOutcomeRunning}
	if err := WriteDetachedLaunch(s.home, launch); err != nil {
		t.Fatal(err)
	}
	unrelated := DetachedLaunch{WorkID: "work_unrelated_door", Actor: "executor", PID: 124, Outcome: DetachedOutcomeRunning}
	if err := WriteDetachedLaunch(s.home, unrelated); err != nil {
		t.Fatal(err)
	}
	oldAlive := detachedProcessAlive
	detachedProcessAlive = func(int) bool { return false }
	t.Cleanup(func() { detachedProcessAlive = oldAlive })
	verdict := s.ReconcileDetachedLaunch(before.ID, fixedNow.Add(time.Minute))
	if len(verdict) != 1 || verdict[0].Outcome != DetachedOutcomeDoorLost {
		t.Fatalf("old verifying state certified a new execution: %+v", verdict)
	}
	after, err := s.Get(before.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("explicit no-start destroyed source work: %v %+v", err, after)
	}
	other, err := ReadDetachedLaunch(s.home, unrelated.WorkID)
	if err != nil || other.Outcome != DetachedOutcomeRunning {
		t.Fatalf("targeted reconcile touched unrelated launch: %v %+v", err, other)
	}
	if err := s.archiveSelectedDoor(before.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.archiveSelectedDoor(before.ID); err != nil {
		t.Fatalf("archive retry: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(DetachedLaunchDir(s.home), "history"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("lost trial not retained exactly once: %v %v", entries, err)
	}
}
