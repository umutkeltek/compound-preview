package work

import (
	"testing"
	"time"
)

// T2: when every work id a waiting continue-run references is terminal, the
// workbench projection reports it resolved/terminal_work — mirroring dispatch
// step-run resolution — without mutating the recorded run file.
func TestWorkbenchResolvesContinueRunBlockedByTerminalWork(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ready, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "continue run stale wait resolves"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.ContinueRun(fixedNow, ContinueRunOptions{
		ContinueOptions: ContinueOptions{Actor: "codex", ExpectWorkID: ready.ID},
		MaxSteps:        5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "waiting" || !contains(run.BlockedOn, "work.run_evidence:"+ready.ID) {
		t.Fatalf("continue run did not wait on run evidence pre-close: %+v", run)
	}
	if _, err := s.Advance(ready.ID, AdvanceInput{To: "verifying", VerificationID: "ver_stale_continue"}, fixedNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Close(ready.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "outward_delta",
		Summary:   "continue run wait resolved by terminal work",
		Changed:   true,
		ProofRefs: []string{"proof:" + ready.ID},
	}, fixedNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	report, err := s.Workbench(fixedNow.Add(3*time.Minute), WorkbenchOptions{RecentLimit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.ContinueRuns) != 1 {
		t.Fatalf("continue runs = %+v", report.ContinueRuns)
	}
	got := report.ContinueRuns[0]
	if got.Status != "resolved" || got.WaitReason != "terminal_work" || got.CanContinue {
		t.Fatalf("continue run summary not resolved: %+v", got)
	}
	if !contains(got.BlockedOn, "work.terminal:"+ready.ID) || got.NextAction != "referenced work is terminal; no continue action required" {
		t.Fatalf("continue run terminal proof not surfaced: %+v", got)
	}
}

// T2b: a continue-run with a non-terminal referenced work stays waiting. The
// projection must not resolve while any referenced work is still live.
func TestWorkbenchKeepsContinueRunWaitingWhileWorkNonTerminal(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ready, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "continue run stays waiting"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ContinueRun(fixedNow, ContinueRunOptions{
		ContinueOptions: ContinueOptions{Actor: "codex", ExpectWorkID: ready.ID},
		MaxSteps:        5,
	}); err != nil {
		t.Fatal(err)
	}

	report, err := s.Workbench(fixedNow.Add(time.Minute), WorkbenchOptions{RecentLimit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.ContinueRuns) != 1 {
		t.Fatalf("continue runs = %+v", report.ContinueRuns)
	}
	got := report.ContinueRuns[0]
	if got.Status != "waiting" || got.WaitReason != "run_evidence" {
		t.Fatalf("continue run must stay waiting while work is live: %+v", got)
	}
	if !contains(got.BlockedOn, "work.run_evidence:"+ready.ID) {
		t.Fatalf("waiting continue run should keep its live blocked_on: %+v", got)
	}
}
