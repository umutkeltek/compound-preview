package work

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
)

// A stale sweep may win after Close freezes a pending claim reservation but
// before Close prepares that reservation for supersession. The later, real
// harness execution already owns the failure outcome, so the sweep must not
// wedge the frozen close or be rewritten as close-owned evidence.
func TestCloseConvergesWhenSweepWinsBeforePrepare(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{
		Owner:            "sweep-before-close-prepare",
		SuccessPredicate: "failed governed execution closes despite a swept claim reservation",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(w.ID, "executor", fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed.RunAttemptIDs) != 1 {
		t.Fatalf("claim attempts = %v, want one reservation", claimed.RunAttemptIDs)
	}
	claimAttemptID := claimed.RunAttemptIDs[0]
	running, err := s.Advance(w.ID, AdvanceInput{
		To:    "running",
		Actor: "executor",
		Note:  "dispatch governed execution",
	}, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	attempts := runattempt.Open(s.home)
	execution, err := attempts.MintWithActionIntent(
		w.ID,
		"executor",
		"os-harness-entrypoints",
		latestActionIntentID(running),
		fixedNow.Add(3*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if execution, err = attempts.SetOrigin(execution.ID, runattempt.OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	if execution, err = attempts.SetExecutionPosture(execution.ID, runattempt.ExecutionPosture{
		PermissionMode:   "plan",
		PolicyDecisionID: running.PolicyDecisionID,
	}); err != nil {
		t.Fatal(err)
	}
	if execution, err = attempts.CommitLaunch(execution.ID, fixedNow.Add(3*time.Minute+time.Second), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if execution, err = attempts.Complete(execution.ID, runattempt.StatusFailed, fixedNow.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	executionBefore := execution
	executionBeforeBytes, err := os.ReadFile(filepath.Join(s.home, "run_attempts", execution.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}

	closeAt := fixedNow.Add(5 * time.Minute)
	input := CloseInput{
		Outcome:       "failed",
		MoveType:      "audit_only",
		Actor:         "executor",
		Summary:       "governed execution failed",
		ProofRefs:     []string{"failure:harness-exit"},
		RunAttemptIDs: []string{execution.ID},
	}
	plan, err := s.ClosePlan(w.ID, closeAt, ClosePlanOptions{
		Actor:     input.Actor,
		Outcome:   input.Outcome,
		MoveType:  input.MoveType,
		ProofRefs: input.ProofRefs,
	})
	if err != nil {
		t.Fatal(err)
	}

	previousPrepare := prepareRunAttemptTerminalization
	t.Cleanup(func() { prepareRunAttemptTerminalization = previousPrepare })
	var sweptClaim runattempt.RunAttempt
	var sweptClaimBytes []byte
	prepareCalls := 0
	prepareRunAttemptTerminalization = func(store *runattempt.Store, id string, intent runattempt.TerminalizationIntent) (runattempt.RunAttempt, error) {
		prepareCalls++
		if prepareCalls == 1 {
			if id != claimAttemptID {
				t.Fatalf("first prepared attempt = %s, want frozen claim %s", id, claimAttemptID)
			}
			report, sweepErr := attempts.SweepStale(30*time.Minute, closeAt.Add(time.Hour))
			if sweepErr != nil {
				t.Fatal(sweepErr)
			}
			if report.AutoClosed != 1 || !reflect.DeepEqual(report.AutoClosedIDs, []string{claimAttemptID}) {
				t.Fatalf("sweep report = %+v, want exact claim auto-close", report)
			}
			sweptClaim, sweepErr = attempts.Get(claimAttemptID)
			if sweepErr != nil {
				t.Fatal(sweepErr)
			}
			sweptClaimBytes, sweepErr = os.ReadFile(filepath.Join(s.home, "run_attempts", claimAttemptID+".json"))
			if sweepErr != nil {
				t.Fatal(sweepErr)
			}
		}
		return previousPrepare(store, id, intent)
	}

	if _, closeErr := s.Close(w.ID, input, closeAt); closeErr == nil || !strings.Contains(closeErr.Error(), "recovery_required") {
		t.Fatalf("first close error = %v, want recovery_required", closeErr)
	}
	current, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != "running" {
		t.Fatalf("failed first close changed work state to %q", current.State)
	}
	frozen, present, err := s.loadClosePending(w.ID)
	if err != nil || !present {
		t.Fatalf("failed first close recovery frame: present=%v err=%v", present, err)
	}
	if frozen.Action.ID != plan.ActionIntent.ID || frozen.Decision.ID != plan.Decision.ID ||
		frozen.Receipt.ID != idFor("rec_", w.ID, ts(closeAt)) ||
		frozen.Transition.ID != idFor("tr_", w.ID, ts(closeAt)) {
		t.Fatalf("frozen close generation differs plan: pending=%+v plan=%+v", frozen, plan)
	}
	frozenFrameBytes, err := os.ReadFile(s.closePendingPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}

	// A failed replacement stage must leave the original recovery frame and
	// both RunAttempts byte-identical. The next exact retry then resumes the
	// same frozen generation rather than minting a substitute close.
	previousStageWriter := writeStagedJSONFile
	t.Cleanup(func() { writeStagedJSONFile = previousStageWriter })
	injectedStageFailure := errors.New("injected swept-claim frame stage failure")
	writeStagedJSONFile = func(*os.File, []byte) error { return injectedStageFailure }
	if _, refreshErr := s.Close(w.ID, input, closeAt.Add(30*time.Minute)); refreshErr == nil ||
		!strings.Contains(refreshErr.Error(), "recovery_required") ||
		!strings.Contains(refreshErr.Error(), injectedStageFailure.Error()) {
		t.Fatalf("refresh stage failure = %v, want durable recovery_required", refreshErr)
	}
	writeStagedJSONFile = previousStageWriter
	frameAfterStageFailure, err := os.ReadFile(s.closePendingPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(frameAfterStageFailure, frozenFrameBytes) {
		t.Fatal("failed refresh stage changed frozen close frame")
	}
	claimAfterStageFailure, err := os.ReadFile(filepath.Join(s.home, "run_attempts", claimAttemptID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(claimAfterStageFailure, sweptClaimBytes) {
		t.Fatal("failed refresh stage changed swept claim bytes")
	}
	executionAfterStageFailure, err := os.ReadFile(filepath.Join(s.home, "run_attempts", execution.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(executionAfterStageFailure, executionBeforeBytes) {
		t.Fatal("failed refresh stage changed execution bytes")
	}

	closed, closeErr := s.Close(w.ID, input, closeAt.Add(time.Hour))
	if closeErr != nil {
		t.Fatalf("exact frozen close did not converge after sweep: %v", closeErr)
	}
	if closed.Work.State != "failed" {
		t.Fatalf("closed state = %q, want failed", closed.Work.State)
	}
	if closed.Work.PolicyDecisionID != plan.Decision.ID ||
		countCloseActionID(closed.Work.ActionIntentIDs, plan.ActionIntent.ID) != 1 {
		t.Fatalf("close changed frozen action/policy generation: work=%+v plan=%+v", closed.Work, plan)
	}
	if closed.Receipt.ID != idFor("rec_", w.ID, ts(closeAt)) ||
		closed.Transition.ID != idFor("tr_", w.ID, ts(closeAt)) {
		t.Fatalf("close changed frozen proof generation: receipt=%s transition=%s", closed.Receipt.ID, closed.Transition.ID)
	}
	if !reflect.DeepEqual(closed.Work.RunAttemptIDs, []string{claimAttemptID, execution.ID}) {
		t.Fatalf("closed attempt lineage = %v, want swept claim then execution", closed.Work.RunAttemptIDs)
	}

	claimAfter, err := attempts.Get(claimAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(claimAfter, sweptClaim) {
		t.Fatalf("close rewrote swept claim evidence:\nbefore=%+v\nafter=%+v", sweptClaim, claimAfter)
	}
	claimAfterBytes, err := os.ReadFile(filepath.Join(s.home, "run_attempts", claimAttemptID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(claimAfterBytes, sweptClaimBytes) {
		t.Fatal("close changed swept claim bytes")
	}
	executionAfter, err := attempts.Get(execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(executionAfter, executionBefore) {
		t.Fatalf("close rewrote actual execution:\nbefore=%+v\nafter=%+v", executionBefore, executionAfter)
	}
	executionAfterBytes, err := os.ReadFile(filepath.Join(s.home, "run_attempts", execution.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(executionAfterBytes, executionBeforeBytes) {
		t.Fatal("close changed actual execution bytes")
	}

	actualAction := frozen.Action
	if err := s.readEventJSON(frozen.Action.ID, &actualAction); err != nil || !reflect.DeepEqual(actualAction, frozen.Action) {
		t.Fatalf("persisted ActionIntent differs frozen frame: action=%+v err=%v", actualAction, err)
	}
	actualDecision := frozen.Decision
	if err := s.readEventJSON(frozen.Decision.ID, &actualDecision); err != nil || !reflect.DeepEqual(actualDecision, frozen.Decision) {
		t.Fatalf("persisted PolicyDecision differs frozen frame: decision=%+v err=%v", actualDecision, err)
	}
	actualReceipt := frozen.Receipt
	if err := s.readEventJSON(frozen.Receipt.ID, &actualReceipt); err != nil || !reflect.DeepEqual(actualReceipt, frozen.Receipt) {
		t.Fatalf("persisted Receipt differs frozen frame: receipt=%+v err=%v", actualReceipt, err)
	}
	actualTransition := frozen.Transition
	if err := s.readEventJSON(frozen.Transition.ID, &actualTransition); err != nil || !reflect.DeepEqual(actualTransition, frozen.Transition) {
		t.Fatalf("persisted Transition differs frozen frame: transition=%+v err=%v", actualTransition, err)
	}

	events, err := s.eventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	closeEvents := 0
	for _, event := range events {
		if event.To != "failed" {
			continue
		}
		closeEvents++
		for _, digest := range event.RunAttemptTerminalizationDigests {
			if digest.RunAttemptID == claimAttemptID {
				t.Fatalf("close event retained stale terminalization digest for swept claim: %+v", event)
			}
		}
	}
	if closeEvents != 1 {
		t.Fatalf("failed close event count = %d, want one", closeEvents)
	}
	if _, present, err := s.loadClosePending(w.ID); err != nil || present {
		t.Fatalf("successful close left pending frame: present=%v err=%v", present, err)
	}
	if _, err := os.Stat(s.leasePath(w.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful close left lease: %v", err)
	}

	replayed, err := s.Close(w.ID, input, closeAt.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("idempotent terminal replay: %v", err)
	}
	if replayed.Receipt.ID != closed.Receipt.ID || replayed.Transition.ID != closed.Transition.ID {
		t.Fatalf("terminal replay changed proof pair: first=%s/%s replay=%s/%s",
			closed.Receipt.ID, closed.Transition.ID, replayed.Receipt.ID, replayed.Transition.ID)
	}
}

// A frame with more than one terminalization plan cannot be rewritten after
// an earlier plan has installed its immutable intent. Even when a later claim
// reservation is swept in the same narrow shape as the recoverable race, the
// original frame and prepared sibling intent must remain recovery evidence.
func TestCloseDoesNotRebuildSweptPlanAfterEarlierPrepare(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{
		Owner:            "multi-plan-sweep-before-prepare",
		SuccessPredicate: "partial close preparation remains recovery_required",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(w.ID, "executor", fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	claimAttemptID := claimed.RunAttemptIDs[0]
	running, err := s.Advance(w.ID, AdvanceInput{
		To:    "running",
		Actor: "executor",
		Note:  "dispatch governed execution",
	}, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	attempts := runattempt.Open(s.home)
	firstPlan, err := attempts.MintWithActionIntent(
		w.ID,
		"executor",
		"os-harness-entrypoints",
		latestActionIntentID(running),
		fixedNow.Add(2*time.Minute+10*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if firstPlan, err = attempts.SetOrigin(firstPlan.ID, runattempt.OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	if firstPlan, err = attempts.SetExecutionPosture(firstPlan.ID, runattempt.ExecutionPosture{
		PermissionMode:   "plan",
		PolicyDecisionID: running.PolicyDecisionID,
	}); err != nil {
		t.Fatal(err)
	}
	// This plan exercises interrupted terminalization, not a subprocess.
	// Do not invent launch evidence for a still-active execution: census
	// correctly requires such effects to be reconciled before close.
	if firstPlan, err = attempts.MarkRunning(firstPlan.ID, fixedNow.Add(2*time.Minute+20*time.Second)); err != nil {
		t.Fatal(err)
	}
	// Put the nonterminal execution before the claim reservation so prepare
	// installs its intent first. This constructs the crash-recovery state that
	// cannot be reached deterministically through concurrent public calls.
	withFirstPlan := running
	withFirstPlan.RunAttemptIDs = []string{firstPlan.ID, claimAttemptID}
	if err := s.writeJSON(s.workPath(w.ID), &withFirstPlan); err != nil {
		t.Fatal(err)
	}

	execution, err := attempts.MintWithActionIntent(
		w.ID,
		"executor",
		"os-harness-entrypoints",
		latestActionIntentID(withFirstPlan),
		fixedNow.Add(3*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if execution, err = attempts.SetOrigin(execution.ID, runattempt.OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	if execution, err = attempts.SetExecutionPosture(execution.ID, runattempt.ExecutionPosture{
		PermissionMode:   "plan",
		PolicyDecisionID: withFirstPlan.PolicyDecisionID,
	}); err != nil {
		t.Fatal(err)
	}
	if execution, err = attempts.CommitLaunch(execution.ID, fixedNow.Add(3*time.Minute+time.Second), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if execution, err = attempts.Complete(execution.ID, runattempt.StatusFailed, fixedNow.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}

	closeAt := fixedNow.Add(5 * time.Minute)
	input := CloseInput{
		Outcome:       "failed",
		MoveType:      "audit_only",
		Actor:         "executor",
		Summary:       "governed execution failed after partial close preparation",
		ProofRefs:     []string{"failure:harness-exit"},
		RunAttemptIDs: []string{execution.ID},
	}
	previousPrepare := prepareRunAttemptTerminalization
	t.Cleanup(func() { prepareRunAttemptTerminalization = previousPrepare })
	prepareCalls := 0
	var sweptClaim runattempt.RunAttempt
	prepareRunAttemptTerminalization = func(store *runattempt.Store, id string, intent runattempt.TerminalizationIntent) (runattempt.RunAttempt, error) {
		prepareCalls++
		if prepareCalls == 1 && id != firstPlan.ID {
			t.Fatalf("first prepared plan = %s, want %s", id, firstPlan.ID)
		}
		if prepareCalls == 2 {
			if id != claimAttemptID {
				t.Fatalf("second prepared plan = %s, want claim %s", id, claimAttemptID)
			}
			report, sweepErr := attempts.SweepStale(30*time.Minute, closeAt.Add(time.Hour))
			if sweepErr != nil {
				t.Fatal(sweepErr)
			}
			if report.AutoClosed != 1 || !reflect.DeepEqual(report.AutoClosedIDs, []string{claimAttemptID}) {
				t.Fatalf("sweep report = %+v, want only claim auto-close", report)
			}
			sweptClaim, sweepErr = attempts.Get(claimAttemptID)
			if sweepErr != nil {
				t.Fatal(sweepErr)
			}
		}
		return previousPrepare(store, id, intent)
	}

	if _, err := s.Close(w.ID, input, closeAt); err == nil || !strings.Contains(err.Error(), "recovery_required") {
		t.Fatalf("partially prepared close error = %v, want recovery_required", err)
	}
	frameAfterFirst, err := os.ReadFile(s.closePendingPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	preparedFirst, err := attempts.Get(firstPlan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preparedFirst.TerminalizationIntent == nil {
		t.Fatal("first plan did not retain its prepared close intent")
	}

	if _, err := s.Close(w.ID, input, closeAt.Add(2*time.Hour)); err == nil || !strings.Contains(err.Error(), "recovery_required") {
		t.Fatalf("unsafe multi-plan retry error = %v, want recovery_required", err)
	}
	frameAfterRetry, err := os.ReadFile(s.closePendingPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(frameAfterRetry, frameAfterFirst) {
		t.Fatal("multi-plan retry rewrote frozen close frame after earlier intent preparation")
	}
	preparedAfterRetry, err := attempts.Get(firstPlan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(preparedAfterRetry, preparedFirst) {
		t.Fatal("multi-plan retry changed earlier prepared intent")
	}
	claimAfterRetry, err := attempts.Get(claimAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(claimAfterRetry, sweptClaim) {
		t.Fatal("multi-plan retry rewrote swept claim evidence")
	}
	current, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != "running" {
		t.Fatalf("unsafe multi-plan retry changed work state to %q", current.State)
	}
}
