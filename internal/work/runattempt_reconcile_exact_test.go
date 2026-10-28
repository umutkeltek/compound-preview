package work

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"compound/internal/policy"
	"compound/internal/runattempt"
)

func callerTerminalizationRequest(runAttemptID string) RunAttemptTerminalizationRequest {
	return RunAttemptTerminalizationRequest{
		RunAttemptID: runAttemptID,
		Owner:        runattempt.TerminalizationOwnerPanelApply,
		TargetStatus: runattempt.StatusFailed,
	}
}

func writeRunAttemptForReconcileTest(t *testing.T, s *Store, ra runattempt.RunAttempt) {
	t.Helper()
	body, err := json.MarshalIndent(ra, "", "  ")
	if err != nil {
		t.Fatalf("marshal RunAttempt: %v", err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(filepath.Join(s.home, "run_attempts", ra.ID+".json"), body, 0o600); err != nil {
		t.Fatalf("write RunAttempt: %v", err)
	}
}

// A process death after terminal work publication but before caller-owned
// attempt completion must be repairable only from the exact durable intent;
// reconciliation must not infer the work outcome as the attempt status.
func TestCrashAfterTerminalWorkPublicationReconcilesPreparedCallerStatus(t *testing.T) {
	s := newStore(t)
	workID, runAttemptIDs := closeTerminalizeWork(t, s, "close-crash-prepared-caller", fixedNow)
	if len(runAttemptIDs) == 0 {
		t.Fatal("setup: claim must mint a RunAttempt")
	}

	previous := closeBeforeLeaseRelease
	t.Cleanup(func() { closeBeforeLeaseRelease = previous })
	closeBeforeLeaseRelease = func() { panic("injected crash after terminal work publication") }
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("close did not reach injected post-publication crash")
			}
		}()
		_, _ = s.Close(workID, CloseInput{
			Outcome:                       "completed",
			MoveType:                      "control_plane_repair",
			Summary:                       "crash after durable close publication",
			SkipRunAttemptTerminalization: true,
			RunAttemptTerminalizations:    []RunAttemptTerminalizationRequest{callerTerminalizationRequest(runAttemptIDs[0])},
		}, fixedNow.Add(5*time.Minute))
	}()
	closeBeforeLeaseRelease = previous

	terminalWork, err := s.Get(workID)
	if err != nil {
		t.Fatalf("get post-crash work: %v", err)
	}
	if terminalWork.State != "completed" {
		t.Fatalf("injected crash was not after durable work publication: %+v", terminalWork)
	}
	pending, err := runattempt.Open(s.home).Get(runAttemptIDs[0])
	if err != nil {
		t.Fatalf("get post-crash RunAttempt: %v", err)
	}
	if runattempt.IsTerminal(pending.Status) || pending.TerminalizationIntent == nil {
		t.Fatalf("post-crash evidence gap is not durably prepared: %+v", pending)
	}

	report, err := s.ReconcileRunAttemptsForTerminalWork(workID, fixedNow.Add(6*time.Minute))
	if err != nil {
		t.Fatalf("reconcile post-crash terminalization: %v", err)
	}
	if report.Status != "ok" || len(report.Issues) != 0 {
		t.Fatalf("exact post-crash reconciliation did not converge: %+v", report)
	}
	after, err := runattempt.Open(s.home).Get(runAttemptIDs[0])
	if err != nil {
		t.Fatalf("get reconciled RunAttempt: %v", err)
	}
	if after.Status != runattempt.StatusFailed || after.TerminalizationOwner != runattempt.TerminalizationOwnerPanelApply {
		t.Fatalf("reconciliation inferred close outcome instead of prepared caller status: %+v", after)
	}
}

// The close event must bind the complete per-attempt intent, not only its
// shared close generation. Otherwise a post-close rewrite of a caller-owned
// reason would be accepted and promoted during reconciliation.
func TestReconcileHoldsTerminalizationReasonMismatch(t *testing.T) {
	s := newStore(t)
	workID, runAttemptIDs := closeTerminalizeWork(t, s, "reconcile-reason-mismatch", fixedNow)
	if _, err := s.Close(workID, CloseInput{
		Outcome:                       "completed",
		MoveType:                      "control_plane_repair",
		Summary:                       "caller-owned reason is exact evidence",
		SkipRunAttemptTerminalization: true,
		RunAttemptTerminalizations: []RunAttemptTerminalizationRequest{{
			RunAttemptID: runAttemptIDs[0],
			Owner:        runattempt.TerminalizationOwnerPanelApply,
			TargetStatus: runattempt.StatusFailed,
			Reason:       "target failed before close",
		}},
	}, fixedNow.Add(5*time.Minute)); err != nil {
		t.Fatalf("close with reason: %v", err)
	}
	ra, err := runattempt.Open(s.home).Get(runAttemptIDs[0])
	if err != nil || ra.TerminalizationIntent == nil {
		t.Fatalf("get prepared attempt: ra=%+v err=%v", ra, err)
	}
	tampered := *ra.TerminalizationIntent
	tampered.TerminalizationReason = "tampered after close"
	ra.TerminalizationIntent = &tampered
	writeRunAttemptForReconcileTest(t, s, ra)

	report, err := s.ReconcileRunAttemptsForTerminalWork(workID, fixedNow.Add(6*time.Minute))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if report.Status != "held" || len(report.Issues) == 0 || report.Issues[0].Code != "terminalization_intent_mismatch" {
		t.Fatalf("reason mismatch was not held: %+v", report)
	}
	after, err := runattempt.Open(s.home).Get(runAttemptIDs[0])
	if err != nil {
		t.Fatalf("get after held reconcile: %v", err)
	}
	if runattempt.IsTerminal(after.Status) {
		t.Fatalf("mismatched reason was terminalized: %+v", after)
	}
}

// A crash after intent preparation but before the first close envelope must
// reserve one exact close generation. Competing lifecycle mutations and
// changed close input are refused; an exact retry reuses the original proof.
func TestCloseResumesExactPreparedGenerationAfterCrash(t *testing.T) {
	s := newStore(t)
	workID, runAttemptIDs := closeTerminalizeWork(t, s, "close-resumes-prepared-generation", fixedNow)
	input := CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "resumable prepared close",
		Changed:   true,
		ProofRefs: []string{"check:resumable-prepared-close"},
	}

	previous := closeAfterTerminalizationPrepare
	t.Cleanup(func() { closeAfterTerminalizationPrepare = previous })
	closeAfterTerminalizationPrepare = func() { panic("injected crash after terminalization prepare") }
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("close did not reach post-prepare crash seam")
			}
		}()
		_, _ = s.Close(workID, input, fixedNow.Add(5*time.Minute))
	}()
	closeAfterTerminalizationPrepare = previous

	reserved, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if terminalStates[reserved.State] {
		t.Fatalf("pre-publication crash terminalized work: %+v", reserved)
	}
	prepared, err := runattempt.Open(s.home).Get(runAttemptIDs[0])
	if err != nil || prepared.TerminalizationIntent == nil {
		t.Fatalf("post-crash terminalization reservation missing: ra=%+v err=%v", prepared, err)
	}
	firstIntent := *prepared.TerminalizationIntent

	if _, err := s.Advance(workID, AdvanceInput{To: "running", Actor: "codex"}, fixedNow.Add(6*time.Minute)); err == nil || !strings.Contains(err.Error(), "frozen close") {
		t.Fatalf("competing lifecycle mutation error = %v, want frozen-close fence", err)
	}
	changed := input
	changed.Summary = "different retry input"
	if _, err := s.Close(workID, changed, fixedNow.Add(7*time.Minute)); err == nil || !strings.Contains(err.Error(), "retry input") {
		t.Fatalf("changed close retry error = %v, want retry-input mismatch", err)
	}

	closed, err := s.Close(workID, input, fixedNow.Add(8*time.Minute))
	if err != nil {
		t.Fatalf("resume exact prepared close: %v", err)
	}
	after, err := runattempt.Open(s.home).Get(runAttemptIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if after.TerminalizationIntent == nil || !reflect.DeepEqual(*after.TerminalizationIntent, firstIntent) {
		t.Fatalf("retry did not reuse exact prepared generation: before=%+v after=%+v", firstIntent, after.TerminalizationIntent)
	}
	if !after.CompletedAt.Equal(fixedNow.Add(8 * time.Minute)) {
		t.Fatalf("final attempt completion time = %s, want actual retry time", after.CompletedAt)
	}
	if closed.Receipt.ID != firstIntent.ReceiptID || closed.Transition.ID != firstIntent.TransitionID {
		t.Fatalf("resumed close proof ids changed: close=%+v intent=%+v", closed, firstIntent)
	}
}

func TestCloseResumesLegacyPreparedActionIdentityAfterUpgrade(t *testing.T) {
	s := newStore(t)
	workID, runAttemptIDs := closeTerminalizeWork(t, s, "close-resumes-legacy-prepared-identity", fixedNow)
	input := CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "resume legacy prepared close identity",
		Changed:   true,
		ProofRefs: []string{"check:legacy-prepared-close-identity"},
	}
	closeAt := fixedNow.Add(5 * time.Minute)

	previous := closeAfterTerminalizationPrepare
	t.Cleanup(func() { closeAfterTerminalizationPrepare = previous })
	closeAfterTerminalizationPrepare = func() { panic("injected pre-upgrade crash after terminalization prepare") }
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("close did not reach post-prepare crash seam")
			}
		}()
		_, _ = s.Close(workID, input, closeAt)
	}()
	closeAfterTerminalizationPrepare = previous

	prepared, err := runattempt.Open(s.home).Get(runAttemptIDs[0])
	if err != nil || prepared.TerminalizationIntent == nil {
		t.Fatalf("post-crash terminalization reservation missing: ra=%+v err=%v", prepared, err)
	}
	intent := *prepared.TerminalizationIntent
	plan, err := s.ClosePlan(workID, intent.PreparedAt, ClosePlanOptions{
		Actor:     input.Actor,
		Outcome:   input.Outcome,
		MoveType:  input.MoveType,
		ProofRefs: input.ProofRefs,
	})
	if err != nil {
		t.Fatal(err)
	}
	action := plan.ActionIntent
	legacyActionID := idFor(
		"act_",
		action.Capability,
		action.Action,
		action.Target.Kind,
		action.Target.ID,
		action.Target.Path,
		action.MutationClass,
		action.Reversibility,
		action.BlastRadius,
		action.Actor,
		input.Outcome,
		input.MoveType,
		"false",
		"",
		"",
		ts(intent.PreparedAt),
	)
	if legacyActionID == action.ID {
		t.Fatal("test did not construct distinct legacy and framed close identities")
	}
	// A real home prepared by the legacy binary could not also carry the modern
	// close-pending frame. Remove the injected modern crash residue before
	// installing the legacy intent; otherwise the fixture combines two mutually
	// exclusive generations and correctly trips the exact-intent fence.
	pendingClose, present, err := s.loadClosePending(workID)
	if err != nil || !present {
		t.Fatalf("load modern close frame before simulating legacy home: present=%v err=%v", present, err)
	}
	if err := s.clearClosePending(pendingClose); err != nil {
		t.Fatalf("clear modern close frame before simulating legacy home: %v", err)
	}
	legacyAction := action
	legacyAction.ID = legacyActionID
	cfg, _, err := policy.LoadConfig(s.home)
	if err != nil {
		t.Fatal(err)
	}
	legacyDecision := policy.Evaluate(legacyAction, cfg, intent.PreparedAt)
	if !policy.Allowed(legacyDecision) {
		t.Fatalf("legacy close decision unexpectedly refused: %+v", legacyDecision)
	}
	intent.CloseActionIntentID = legacyAction.ID
	intent.ClosePolicyDecisionID = legacyDecision.ID
	prepared.TerminalizationIntent = &intent
	writeRunAttemptForReconcileTest(t, s, prepared)

	closed, err := s.Close(workID, input, closeAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("resume pre-upgrade prepared close: %v", err)
	}
	if closed.Receipt.ID != intent.ReceiptID || closed.Transition.ID != intent.TransitionID {
		t.Fatalf("legacy retry changed frozen proof identity: close=%+v intent=%+v", closed, intent)
	}
	after, err := runattempt.Open(s.home).Get(runAttemptIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if after.TerminalizationIntent == nil || !reflect.DeepEqual(*after.TerminalizationIntent, intent) {
		t.Fatalf("legacy retry rewrote prepared authority: before=%+v after=%+v", intent, after.TerminalizationIntent)
	}
}

func TestCloseResumesAfterFirstCloseEnvelopeWrite(t *testing.T) {
	s := newStore(t)
	workID, runAttemptIDs := closeTerminalizeWork(t, s, "close-resumes-first-envelope", fixedNow)
	input := CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "resume partial close envelopes",
		ProofRefs: []string{"check:resume-first-envelope"},
	}
	previous := closeAfterFirstEnvelopeWrite
	t.Cleanup(func() { closeAfterFirstEnvelopeWrite = previous })
	closeAfterFirstEnvelopeWrite = func() { panic("injected crash after first close envelope") }
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("close did not reach first-envelope crash seam")
			}
		}()
		_, _ = s.Close(workID, input, fixedNow.Add(5*time.Minute))
	}()
	closeAfterFirstEnvelopeWrite = previous

	prepared, err := runattempt.Open(s.home).Get(runAttemptIDs[0])
	if err != nil || prepared.TerminalizationIntent == nil {
		t.Fatalf("partial close lost prepared generation: ra=%+v err=%v", prepared, err)
	}
	closed, err := s.Close(workID, input, fixedNow.Add(9*time.Minute))
	if err != nil {
		t.Fatalf("resume after partial envelope publication: %v", err)
	}
	if closed.Receipt.ID != prepared.TerminalizationIntent.ReceiptID || closed.Transition.ID != prepared.TerminalizationIntent.TransitionID {
		t.Fatalf("partial-publication retry minted a new proof generation: close=%+v intent=%+v", closed, prepared.TerminalizationIntent)
	}
}

func TestCrashGapReconcileMapsEveryWorkCloseOutcome(t *testing.T) {
	tests := []struct {
		outcome string
		want    string
		setup   func(*testing.T, *Store) (string, []string)
	}{
		{"completed", runattempt.StatusSucceeded, func(t *testing.T, s *Store) (string, []string) {
			return closeTerminalizeWork(t, s, "crash-map-completed", fixedNow)
		}},
		{"failed", runattempt.StatusFailed, func(t *testing.T, s *Store) (string, []string) {
			return closeTerminalizeWork(t, s, "crash-map-failed", fixedNow)
		}},
		{"cancelled", runattempt.StatusAbandoned, func(t *testing.T, s *Store) (string, []string) {
			return closeTerminalizeWorkClaimed(t, s, "crash-map-cancelled", fixedNow)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.outcome, func(t *testing.T) {
			s := newStore(t)
			workID, runAttemptIDs := tt.setup(t, s)
			previous := closeBeforeLeaseRelease
			t.Cleanup(func() { closeBeforeLeaseRelease = previous })
			closeBeforeLeaseRelease = func() { panic("injected post-publication crash") }
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("close did not reach injected crash")
					}
				}()
				_, _ = s.Close(workID, CloseInput{
					Outcome:  tt.outcome,
					MoveType: map[bool]string{true: "audit_only", false: "control_plane_repair"}[tt.outcome == "cancelled"],
					Summary:  "crash-gap outcome mapping",
				}, fixedNow.Add(5*time.Minute))
			}()
			closeBeforeLeaseRelease = previous

			attemptPath := filepath.Join(s.home, "run_attempts", runAttemptIDs[0]+".json")
			attemptBodyBeforeReconcile, err := os.ReadFile(attemptPath)
			if err != nil {
				t.Fatalf("read post-crash RunAttempt for %s: %v", tt.outcome, err)
			}
			pending, err := runattempt.Open(s.home).Get(runAttemptIDs[0])
			if err != nil || !runattempt.IsTerminal(pending.Status) || pending.Status != tt.want || pending.TerminalizationIntent == nil || pending.TerminalizationIntent.TargetStatus != tt.want || pending.TerminalizationOwner != runattempt.TerminalizationOwnerWorkClose || pending.TerminalizationReceiptID == "" || pending.TerminalizationTransitionID == "" {
				t.Fatalf("post-crash terminalization for %s is wrong: ra=%+v err=%v", tt.outcome, pending, err)
			}
			if _, present, err := s.loadClosePending(workID); err != nil || !present {
				t.Fatalf("post-crash close frame for %s: present=%v err=%v", tt.outcome, present, err)
			}
			if _, err := os.Stat(s.leasePath(workID)); err != nil {
				t.Fatalf("post-crash lease for %s was not retained: %v", tt.outcome, err)
			}
			report, err := s.ReconcileRunAttemptsForTerminalWork(workID, fixedNow.Add(6*time.Minute))
			if err != nil || report.Status != "ok" || len(report.Issues) != 0 || !reflect.DeepEqual(report.AlreadyConsistentIDs, runAttemptIDs) || len(report.ReconciledIDs) != 0 {
				t.Fatalf("reconcile %s: report=%+v err=%v", tt.outcome, report, err)
			}
			after, err := runattempt.Open(s.home).Get(runAttemptIDs[0])
			if err != nil || after.Status != tt.want || after.TerminalizationOwner != runattempt.TerminalizationOwnerWorkClose || after.TerminalizationReceiptID != pending.TerminalizationReceiptID || after.TerminalizationTransitionID != pending.TerminalizationTransitionID {
				t.Fatalf("reconciled %s attempt=%+v err=%v", tt.outcome, after, err)
			}
			attemptBodyAfterReconcile, err := os.ReadFile(attemptPath)
			if err != nil {
				t.Fatalf("read reconciled RunAttempt for %s: %v", tt.outcome, err)
			}
			if !reflect.DeepEqual(attemptBodyBeforeReconcile, attemptBodyAfterReconcile) {
				t.Fatalf("already-consistent RunAttempt for %s changed during cleanup reconciliation", tt.outcome)
			}
			if _, present, err := s.loadClosePending(workID); err != nil || present {
				t.Fatalf("reconcile left close frame for %s: present=%v err=%v", tt.outcome, present, err)
			}
			if _, err := os.Stat(s.leasePath(workID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("reconcile left lease for %s: %v", tt.outcome, err)
			}
			again, err := s.ReconcileRunAttemptsForTerminalWork(workID, fixedNow.Add(7*time.Minute))
			if err != nil || again.Status != "ok" || len(again.Issues) != 0 || !reflect.DeepEqual(again.AlreadyConsistentIDs, runAttemptIDs) {
				t.Fatalf("idempotent reconcile %s: report=%+v err=%v", tt.outcome, again, err)
			}
		})
	}
}

func TestSweepBeforeReconcileDoesNotDestroyPreparedCallerStatus(t *testing.T) {
	s := newStore(t)
	workID, runAttemptIDs := closeTerminalizeWork(t, s, "sweep-before-reconcile", fixedNow)
	if _, err := s.Close(workID, CloseInput{
		Outcome:                       "completed",
		MoveType:                      "control_plane_repair",
		Summary:                       "sweep preserves prepared caller",
		SkipRunAttemptTerminalization: true,
		RunAttemptTerminalizations:    []RunAttemptTerminalizationRequest{callerTerminalizationRequest(runAttemptIDs[0])},
	}, fixedNow.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	sweep, err := runattempt.Open(s.home).SweepStale(30*time.Minute, fixedNow.Add(2*time.Hour))
	if err != nil || sweep.Swept != 0 {
		t.Fatalf("sweep changed exactly prepared attempt: report=%+v err=%v", sweep, err)
	}
	report, err := s.ReconcileRunAttemptsForTerminalWork(workID, fixedNow.Add(3*time.Hour))
	if err != nil || report.Status != "ok" {
		t.Fatalf("reconcile after sweep: report=%+v err=%v", report, err)
	}
	after, err := runattempt.Open(s.home).Get(runAttemptIDs[0])
	if err != nil || after.Status != runattempt.StatusFailed || after.TerminalizationOwner != runattempt.TerminalizationOwnerPanelApply {
		t.Fatalf("prepared caller status was lost: ra=%+v err=%v", after, err)
	}
}

func TestReconcilePreparedTerminalizationIsConcurrentAndIdempotent(t *testing.T) {
	s := newStore(t)
	workID, runAttemptIDs := closeTerminalizeWork(t, s, "reconcile-concurrent-idempotent", fixedNow)
	if _, err := s.Close(workID, CloseInput{
		Outcome:                       "completed",
		MoveType:                      "control_plane_repair",
		Summary:                       "concurrent reconcile",
		SkipRunAttemptTerminalization: true,
		RunAttemptTerminalizations:    []RunAttemptTerminalizationRequest{callerTerminalizationRequest(runAttemptIDs[0])},
	}, fixedNow.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}

	const callers = 2
	reports := make([]RunAttemptReconcileReport, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			reports[index], errs[index] = s.ReconcileRunAttemptsForTerminalWork(workID, fixedNow.Add(time.Duration(6+index)*time.Minute))
		}(i)
	}
	wg.Wait()
	for i := 0; i < callers; i++ {
		if errs[i] != nil || reports[i].Status != "ok" || len(reports[i].Issues) != 0 {
			t.Fatalf("concurrent reconcile %d: report=%+v err=%v", i, reports[i], errs[i])
		}
	}
	after, err := runattempt.Open(s.home).Get(runAttemptIDs[0])
	if err != nil || after.Status != runattempt.StatusFailed {
		t.Fatalf("concurrent reconcile did not converge: ra=%+v err=%v", after, err)
	}
	again, err := s.ReconcileRunAttemptsForTerminalWork(workID, fixedNow.Add(10*time.Minute))
	if err != nil || again.Status != "ok" || len(again.Issues) != 0 {
		t.Fatalf("idempotent reconcile: report=%+v err=%v", again, err)
	}
}

func TestCloseReportsWorkOwnedRunAttemptTerminalizationFailure(t *testing.T) {
	s := newStore(t)
	workID, runAttemptIDs := closeTerminalizeWork(t, s, "close-reports-terminalization-failure", fixedNow)
	wantID := runAttemptIDs[0]
	injected := errors.New("injected post-close RunAttempt terminalization failure")
	previous := completePreparedTerminalization
	t.Cleanup(func() { completePreparedTerminalization = previous })
	completePreparedTerminalization = func(store *runattempt.Store, id string, intent runattempt.TerminalizationIntent, at time.Time) (runattempt.RunAttempt, error) {
		if id == wantID {
			return runattempt.RunAttempt{}, injected
		}
		return previous(store, id, intent, at)
	}

	input := CloseInput{
		Outcome: "completed", MoveType: "control_plane_repair",
		Summary: "close reports incomplete RunAttempt as recovery required",
	}
	closed, err := s.Close(workID, input, fixedNow.Add(5*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "close recovery_required") {
		t.Fatalf("incomplete RunAttempt close error = %v, want recovery_required", err)
	}
	if !reflect.DeepEqual(closed.UnterminalizedRunAttemptIDs, []string{wantID}) {
		t.Fatalf("unterminalized attempts = %v, want [%s]", closed.UnterminalizedRunAttemptIDs, wantID)
	}
	prepared, err := runattempt.Open(s.home).Get(wantID)
	if err != nil || runattempt.IsTerminal(prepared.Status) || prepared.TerminalizationIntent == nil {
		t.Fatalf("failed completion did not preserve exact repair intent: attempt=%+v err=%v", prepared, err)
	}
	if _, present, err := s.loadClosePending(workID); err != nil || !present {
		t.Fatalf("failed completion did not retain exact close frame: present=%v err=%v", present, err)
	}
	if _, err := os.Stat(s.leasePath(workID)); err != nil {
		t.Fatalf("failed completion did not retain lease: %v", err)
	}

	completePreparedTerminalization = previous
	retryAt := fixedNow.Add(6 * time.Minute)
	closed, err = s.Close(workID, input, retryAt)
	if err != nil || len(closed.UnterminalizedRunAttemptIDs) != 0 {
		t.Fatalf("exact retry did not finish terminal close: result=%+v err=%v", closed, err)
	}
	after, err := runattempt.Open(s.home).Get(wantID)
	if err != nil || after.Status != runattempt.StatusSucceeded || !after.CompletedAt.Equal(retryAt) {
		t.Fatalf("exact retry did not terminalize at actual retry time %s: attempt=%+v err=%v", retryAt, after, err)
	}
	if _, present, err := s.loadClosePending(workID); err != nil || present {
		t.Fatalf("exact retry left close frame: present=%v err=%v", present, err)
	}
	if _, err := os.Stat(s.leasePath(workID)); !os.IsNotExist(err) {
		t.Fatalf("exact retry left lease: %v", err)
	}
}

func TestReconcileFinishesClosePendingFrameAndLeaseCleanup(t *testing.T) {
	s := newStore(t)
	workID, runAttemptIDs := closeTerminalizeWork(t, s, "reconcile-finishes-close-cleanup", fixedNow)
	wantID := runAttemptIDs[0]
	previous := completePreparedTerminalization
	t.Cleanup(func() { completePreparedTerminalization = previous })
	completePreparedTerminalization = func(store *runattempt.Store, id string, intent runattempt.TerminalizationIntent, at time.Time) (runattempt.RunAttempt, error) {
		if id == wantID {
			return runattempt.RunAttempt{}, errors.New("injected terminalization failure before reconcile")
		}
		return previous(store, id, intent, at)
	}
	input := CloseInput{
		Outcome: "completed", MoveType: "control_plane_repair",
		Summary: "reconciler owns terminal close cleanup",
	}
	if _, err := s.Close(workID, input, fixedNow.Add(5*time.Minute)); err == nil || !strings.Contains(err.Error(), "close recovery_required") {
		t.Fatalf("incomplete close error = %v, want recovery_required", err)
	}

	completePreparedTerminalization = previous
	reconcileAt := fixedNow.Add(7 * time.Minute)
	report, err := s.ReconcileRunAttemptsForTerminalWork(workID, reconcileAt)
	if err != nil || report.Status != "ok" || !reflect.DeepEqual(report.ReconciledIDs, []string{wantID}) {
		t.Fatalf("reconcile incomplete close: report=%+v err=%v", report, err)
	}
	after, err := runattempt.Open(s.home).Get(wantID)
	if err != nil || after.Status != runattempt.StatusSucceeded || !after.CompletedAt.Equal(reconcileAt) {
		t.Fatalf("reconcile did not terminalize at actual recovery time %s: attempt=%+v err=%v", reconcileAt, after, err)
	}
	if _, present, err := s.loadClosePending(workID); err != nil || present {
		t.Fatalf("reconcile left close frame: present=%v err=%v", present, err)
	}
	if _, err := os.Stat(s.leasePath(workID)); !os.IsNotExist(err) {
		t.Fatalf("reconcile left lease: %v", err)
	}
}

func TestReconcileReportsPartialMutationWhenLaterCASLoses(t *testing.T) {
	s := newStore(t)
	workID, _ := closeTerminalizeWork(t, s, "reconcile-reports-partial", fixedNow)
	w, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	actionID := latestActionIntentID(w)
	attempts := runattempt.Open(s.home)
	var callerIDs []string
	for i := 0; i < 2; i++ {
		ra, mintErr := attempts.MintWithActionIntent(workID, "compound-panel", runattempt.HarnessPlaceholder, actionID, fixedNow.Add(time.Duration(20+i)*time.Second))
		if mintErr != nil {
			t.Fatal(mintErr)
		}
		callerIDs = append(callerIDs, ra.ID)
	}
	requests := []RunAttemptTerminalizationRequest{
		callerTerminalizationRequest(callerIDs[0]),
		callerTerminalizationRequest(callerIDs[1]),
	}
	if _, err := s.Close(workID, CloseInput{
		Outcome: "completed", MoveType: "control_plane_repair",
		Summary:                       "leave two exact caller-owned attempts prepared",
		RunAttemptIDs:                 callerIDs,
		SkipRunAttemptTerminalization: true,
		RunAttemptTerminalizations:    requests,
	}, fixedNow.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}

	previous := completePreparedTerminalization
	t.Cleanup(func() { completePreparedTerminalization = previous })
	injected := errors.New("injected second reconciliation CAS loss")
	completePreparedTerminalization = func(store *runattempt.Store, id string, intent runattempt.TerminalizationIntent, at time.Time) (runattempt.RunAttempt, error) {
		if id == callerIDs[1] {
			return runattempt.RunAttempt{}, injected
		}
		return previous(store, id, intent, at)
	}
	report, err := s.ReconcileRunAttemptsForTerminalWork(workID, fixedNow.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "partially_reconciled" || !reflect.DeepEqual(report.ReconciledIDs, callerIDs[:1]) || !reflect.DeepEqual(report.HeldIDs, callerIDs[1:]) {
		t.Fatalf("partial reconcile report = %+v", report)
	}
	if len(report.Issues) != 1 || report.Issues[0].Code != "terminalization_cas_lost" {
		t.Fatalf("partial reconcile issues = %+v", report.Issues)
	}
}

func TestReconcileHoldsSweptAbandonedPreparedAttemptAsTerminalConflict(t *testing.T) {
	s := newStore(t)
	workID, runAttemptIDs := closeTerminalizeWork(t, s, "reconcile-refuses-swept-abandoned", fixedNow)
	if _, err := s.Close(workID, CloseInput{
		Outcome: "completed", MoveType: "control_plane_repair",
		Summary:                       "leave exact caller-owned terminalization prepared",
		SkipRunAttemptTerminalization: true,
		RunAttemptTerminalizations:    []RunAttemptTerminalizationRequest{callerTerminalizationRequest(runAttemptIDs[0])},
	}, fixedNow.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	store := runattempt.Open(s.home)
	ra, err := store.Get(runAttemptIDs[0])
	if err != nil || ra.TerminalizationIntent == nil {
		t.Fatalf("load prepared attempt: attempt=%+v err=%v", ra, err)
	}
	ra.Status = runattempt.StatusAbandoned
	ra.CompletedAt = fixedNow.Add(6 * time.Minute)
	ra.SweepReason = "synthetic stale sweep from an incompatible writer"
	ra.SweepClassification = runattempt.SweepClassificationAmbiguous
	writeRunAttemptForReconcileTest(t, s, ra)

	report, err := s.ReconcileRunAttemptsForTerminalWork(workID, fixedNow.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "held" || len(report.ReconciledIDs) != 0 || !reflect.DeepEqual(report.HeldIDs, runAttemptIDs[:1]) {
		t.Fatalf("swept-abandoned reconcile report = %+v", report)
	}
	if len(report.Issues) != 1 || report.Issues[0].Code != "terminal_status_conflict" {
		t.Fatalf("swept-abandoned reconcile issues = %+v, want terminal_status_conflict", report.Issues)
	}
}

func TestReconcileHoldsExactAuthorityAndProofMismatches(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*runattempt.TerminalizationIntent)
	}{
		{"receipt", func(in *runattempt.TerminalizationIntent) { in.ReceiptID = "rec_missing_exact" }},
		{"transition", func(in *runattempt.TerminalizationIntent) { in.TransitionID = "tr_missing_exact" }},
		{"close_action", func(in *runattempt.TerminalizationIntent) { in.CloseActionIntentID = "act_missing_exact" }},
		{"close_policy", func(in *runattempt.TerminalizationIntent) { in.ClosePolicyDecisionID = "pol_missing_exact" }},
		{"attempt_authority_action", func(in *runattempt.TerminalizationIntent) { in.AuthorityActionIntentID = "act_missing_exact_authority" }},
		{"attempt_authority_policy", func(in *runattempt.TerminalizationIntent) {
			in.AuthorityPolicyDecisionID = "pol_missing_exact_authority"
		}},
		{"expected_work_generation", func(in *runattempt.TerminalizationIntent) {
			in.ExpectedWorkSHA256 = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}},
		{"expected_action_generation", func(in *runattempt.TerminalizationIntent) { in.ExpectedActionIntentID = "act_stale_generation" }},
		{"expected_policy_generation", func(in *runattempt.TerminalizationIntent) { in.ExpectedPolicyDecisionID = "pol_stale_generation" }},
		{"close_input_digest", func(in *runattempt.TerminalizationIntent) {
			in.CloseInputSHA256 = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		}},
		{"prepared_at", func(in *runattempt.TerminalizationIntent) { in.PreparedAt = in.PreparedAt.Add(time.Second) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t)
			workID, runAttemptIDs := closeTerminalizeWork(t, s, "reconcile-holds-"+tt.name, fixedNow)
			if _, err := s.Close(workID, CloseInput{
				Outcome:                       "completed",
				MoveType:                      "control_plane_repair",
				Summary:                       "mismatch stays held",
				SkipRunAttemptTerminalization: true,
				RunAttemptTerminalizations:    []RunAttemptTerminalizationRequest{callerTerminalizationRequest(runAttemptIDs[0])},
			}, fixedNow.Add(5*time.Minute)); err != nil {
				t.Fatal(err)
			}
			ra, err := runattempt.Open(s.home).Get(runAttemptIDs[0])
			if err != nil || ra.TerminalizationIntent == nil {
				t.Fatalf("get prepared attempt: ra=%+v err=%v", ra, err)
			}
			original := *ra.TerminalizationIntent
			tampered := original
			tt.mutate(&tampered)
			if reflect.DeepEqual(tampered, original) {
				t.Fatal("test mutator did not change intent")
			}
			ra.TerminalizationIntent = &tampered
			writeRunAttemptForReconcileTest(t, s, ra)
			report, err := s.ReconcileRunAttemptsForTerminalWork(workID, fixedNow.Add(6*time.Minute))
			if err != nil || report.Status != "held" || len(report.Issues) == 0 {
				t.Fatalf("mismatch %s was not held: report=%+v err=%v", tt.name, report, err)
			}
			after, err := runattempt.Open(s.home).Get(runAttemptIDs[0])
			if err != nil || runattempt.IsTerminal(after.Status) {
				t.Fatalf("held mismatch %s terminalized: ra=%+v err=%v", tt.name, after, err)
			}
		})
	}
}

func TestReconcileHoldsTamperedClosingReceiptOrTransition(t *testing.T) {
	tests := []struct {
		name   string
		tamper func(*testing.T, *Store, CloseResult)
	}{
		{"receipt", func(t *testing.T, s *Store, closed CloseResult) {
			receipt := closed.Receipt
			receipt.Outcome = "failed"
			if err := s.replaceEvent(receipt.ID, &receipt); err != nil {
				t.Fatal(err)
			}
		}},
		{"transition", func(t *testing.T, s *Store, closed CloseResult) {
			transition := closed.Transition
			transition.AfterRefs = []string{"artifact:synthetic-tamper"}
			if err := s.replaceEvent(transition.ID, &transition); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t)
			workID, runAttemptIDs := closeTerminalizeWork(t, s, "reconcile-tampered-close-"+tt.name, fixedNow)
			closed, err := s.Close(workID, CloseInput{
				Outcome:                       "completed",
				MoveType:                      "control_plane_repair",
				Summary:                       "tampered close evidence",
				SkipRunAttemptTerminalization: true,
				RunAttemptTerminalizations:    []RunAttemptTerminalizationRequest{callerTerminalizationRequest(runAttemptIDs[0])},
			}, fixedNow.Add(5*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			tt.tamper(t, s, closed)
			report, err := s.ReconcileRunAttemptsForTerminalWork(workID, fixedNow.Add(6*time.Minute))
			if err != nil || report.Status != "held" || len(report.Issues) == 0 || report.Issues[0].Code != "close_evidence_mismatch" {
				t.Fatalf("tampered close %s not held: report=%+v err=%v", tt.name, report, err)
			}
		})
	}
}
