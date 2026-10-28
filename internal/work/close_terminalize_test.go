package work

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/runattempt"
)

func attachTerminalCloseHistoryAttempt(
	t *testing.T,
	s *Store,
	workID string,
	predecessorID string,
	status string,
	commitLaunch bool,
	at time.Time,
) runattempt.RunAttempt {
	t.Helper()
	attempts := runattempt.Open(s.home)
	ra, err := attempts.Mint(workID, "codex", "claude", at)
	if err != nil {
		t.Fatal(err)
	}
	if ra, err = attempts.SetOrigin(ra.ID, runattempt.OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	if predecessorID != "" {
		if ra, err = attempts.SetPredecessor(ra.ID, predecessorID); err != nil {
			t.Fatal(err)
		}
	}
	if commitLaunch {
		if ra, err = attempts.CommitLaunch(ra.ID, at.Add(500*time.Millisecond), func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Advance(workID, AdvanceInput{
		To:            "verifying",
		Actor:         "codex",
		RunAttemptIDs: []string{ra.ID},
	}, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	ra, err = attempts.Complete(ra.ID, status, at.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return ra
}

func runAttemptBytesForCloseTest(t *testing.T, home, id string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(home, "run_attempts", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestCloseVerificationRejectedWithoutAcknowledgementRefuses(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	id, _ := closeTerminalizeWork(t, s, "verification-rejected-regression", fixedNow)
	ra := attachTerminalCloseHistoryAttempt(t, s, id, "", runattempt.StatusSucceeded, true, fixedNow.Add(4*time.Minute))
	before := runAttemptBytesForCloseTest(t, s.home, ra.ID)
	_, err := s.Close(id, CloseInput{Outcome: "failed", MoveType: "audit_only", Actor: "codex", ProofRefs: []string{"check:verification-rejected"}}, fixedNow.Add(10*time.Minute))
	want := "terminal runattempt " + ra.ID + " status succeeded contradicts close outcome failed (want failed)"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if !reflect.DeepEqual(before, runAttemptBytesForCloseTest(t, s.home, ra.ID)) {
		t.Fatal("refusal rewrote succeeded attempt")
	}
}

func verificationRejectedCloseInput(id, reason string) CloseInput {
	return CloseInput{Outcome: "failed", MoveType: "audit_only", Actor: "codex", ProofRefs: []string{"check:verification-rejected"}, RunAttemptTerminalizations: []RunAttemptTerminalizationRequest{{RunAttemptID: id, Owner: "work.close.verification_rejected", TargetStatus: runattempt.StatusSucceeded, Reason: reason}}}
}

func TestCloseVerificationRejectedPreservesSucceededAttempt(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	id, _ := closeTerminalizeWork(t, s, "verification-rejected", fixedNow)
	ra := attachTerminalCloseHistoryAttempt(t, s, id, "", runattempt.StatusSucceeded, true, fixedNow.Add(4*time.Minute))
	before := runAttemptBytesForCloseTest(t, s.home, ra.ID)
	reason := "independent verifier rejected the output: unsupported claim"
	res, err := s.Close(id, verificationRejectedCloseInput(ra.ID, reason), fixedNow.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if res.Work.State != "failed" || res.Receipt.Outcome != "failed" {
		t.Fatalf("work/receipt outcome = %s/%s, want failed/failed", res.Work.State, res.Receipt.Outcome)
	}
	if !reflect.DeepEqual(before, runAttemptBytesForCloseTest(t, s.home, ra.ID)) {
		t.Fatal("verification acknowledgement rewrote succeeded attempt")
	}
	want := "verification_rejected:" + ra.ID + ":" + reason
	found := false
	for _, ref := range res.Receipt.ProofRefs {
		found = found || ref == want
	}
	if !found {
		t.Fatalf("proof_refs = %v, want %q", res.Receipt.ProofRefs, want)
	}
}

func TestCloseVerificationRejectedRefusals(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, state, outcome, status, reason, want string
		unattached                                 bool
	}{
		{name: "running", state: "running", want: "requires work state verifying"},
		{name: "empty reason", reason: " ", want: "requires non-empty --reason"},
		{name: "not attached", unattached: true, want: "not attached"},
		{name: "failed attempt", status: runattempt.StatusFailed, want: "requires terminal succeeded"},
		{name: "abandoned attempt", status: runattempt.StatusAbandoned, want: "requires terminal succeeded"},
		{name: "nonterminal attempt", status: runattempt.StatusRunning, want: "requires terminal succeeded"},
		{name: "completed outcome", outcome: "completed", want: "requires outcome failed"},
		{name: "cancelled outcome", outcome: "cancelled", want: "requires outcome failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t)
			id, _ := closeTerminalizeWork(t, s, "verification-refusal", fixedNow)
			ra := attachTerminalCloseHistoryAttempt(t, s, id, "", runattempt.StatusSucceeded, true, fixedNow.Add(4*time.Minute))
			if tt.status != "" {
				ra.Status = tt.status
				if !runattempt.IsTerminal(ra.Status) {
					ra.CompletedAt = time.Time{}
				}
				writeRunAttemptForReconcileTest(t, s, ra)
			}
			if tt.state != "" {
				if _, err := s.Advance(id, AdvanceInput{To: tt.state, Actor: "codex"}, fixedNow.Add(6*time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			if tt.unattached {
				otherID, _ := closeTerminalizeWork(t, s, "verification-other-work", fixedNow)
				ra = attachTerminalCloseHistoryAttempt(t, s, otherID, "", runattempt.StatusSucceeded, true, fixedNow.Add(4*time.Minute))
			}
			in := verificationRejectedCloseInput(ra.ID, "verifier rejected output")
			if tt.name == "empty reason" {
				in.RunAttemptTerminalizations[0].Reason = tt.reason
			}
			if tt.outcome != "" {
				in.Outcome = tt.outcome
			}
			before := runAttemptBytesForCloseTest(t, s.home, ra.ID)
			_, err := s.Close(id, in, fixedNow.Add(10*time.Minute))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if !reflect.DeepEqual(before, runAttemptBytesForCloseTest(t, s.home, ra.ID)) {
				t.Fatal("refusal rewrote attempt")
			}
		})
	}
}

func TestCloseVerificationRejectedOnlyNamedAttempts(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	id, _ := closeTerminalizeWork(t, s, "verification-named-only", fixedNow)
	first := attachTerminalCloseHistoryAttempt(t, s, id, "", runattempt.StatusSucceeded, true, fixedNow.Add(4*time.Minute))
	second := attachTerminalCloseHistoryAttempt(t, s, id, "", runattempt.StatusSucceeded, true, fixedNow.Add(5*time.Minute))
	in := verificationRejectedCloseInput(first.ID, "first result rejected")
	_, err := s.Close(id, in, fixedNow.Add(10*time.Minute))
	want := "terminal runattempt " + second.ID + " status succeeded contradicts close outcome failed (want failed)"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	in.RunAttemptTerminalizations = append(in.RunAttemptTerminalizations, verificationRejectedCloseInput(second.ID, "second result rejected").RunAttemptTerminalizations[0])
	if _, err := s.Close(id, in, fixedNow.Add(11*time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func TestRunAttemptHasExecutedRequiresObservedBoundary(t *testing.T) {
	t.Parallel()
	heartbeat := fixedNow.Add(time.Minute)
	completed := fixedNow.Add(2 * time.Minute)
	prepared := fixedNow.Add(30 * time.Second)
	committed := fixedNow.Add(time.Minute)
	tests := []struct {
		name string
		ra   runattempt.RunAttempt
		want bool
	}{
		{
			name: "origin only",
			ra:   runattempt.RunAttempt{Origin: runattempt.OriginHarnessExecution, CreatedAt: fixedNow},
		},
		{
			name: "prepared launch ignores heartbeat",
			ra: runattempt.RunAttempt{
				Origin:          runattempt.OriginHarnessExecution,
				CreatedAt:       fixedNow,
				LastHeartbeatAt: &heartbeat,
				Launch:          &runattempt.LaunchLifecycle{PreparedAt: &prepared},
			},
		},
		{
			name: "committed launch",
			ra: runattempt.RunAttempt{
				Origin:    runattempt.OriginHarnessExecution,
				CreatedAt: fixedNow,
				Launch:    &runattempt.LaunchLifecycle{PreparedAt: &prepared, CommittedAt: &committed},
			},
			want: true,
		},
		{
			name: "post-mint heartbeat without launch lifecycle",
			ra: runattempt.RunAttempt{
				Origin:          runattempt.OriginHarnessExecution,
				CreatedAt:       fixedNow,
				CompletedAt:     completed,
				LastHeartbeatAt: &heartbeat,
			},
			want: true,
		},
		{
			name: "heartbeat after completion",
			ra: runattempt.RunAttempt{
				Origin:          runattempt.OriginHarnessExecution,
				CreatedAt:       fixedNow,
				CompletedAt:     heartbeat,
				LastHeartbeatAt: &completed,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runAttemptHasExecuted(tt.ra); got != tt.want {
				t.Fatalf("runAttemptHasExecuted() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRunAttemptOwnsHarnessOutcomeRequiresExecutedOrExactStartError(t *testing.T) {
	t.Parallel()
	prepared := fixedNow.Add(time.Minute)
	other := prepared.Add(time.Second)
	committed := prepared
	tests := []struct {
		name string
		ra   runattempt.RunAttempt
		want bool
	}{
		{
			name: "origin only",
			ra:   runattempt.RunAttempt{Origin: runattempt.OriginHarnessExecution, CreatedAt: fixedNow},
		},
		{
			name: "prepared only",
			ra: runattempt.RunAttempt{
				Origin: runattempt.OriginHarnessExecution,
				Launch: &runattempt.LaunchLifecycle{PreparedAt: &prepared},
			},
		},
		{
			name: "mismatched start error",
			ra: runattempt.RunAttempt{
				Origin: runattempt.OriginHarnessExecution,
				Launch: &runattempt.LaunchLifecycle{PreparedAt: &prepared, StartErrorAt: &other},
			},
		},
		{
			name: "conflicting committed and start error",
			ra: runattempt.RunAttempt{
				Origin: runattempt.OriginHarnessExecution,
				Launch: &runattempt.LaunchLifecycle{PreparedAt: &prepared, CommittedAt: &committed, StartErrorAt: &prepared},
			},
		},
		{
			name: "exact start error",
			ra: runattempt.RunAttempt{
				Origin: runattempt.OriginHarnessExecution,
				Launch: &runattempt.LaunchLifecycle{PreparedAt: &prepared, StartErrorAt: &prepared},
			},
			want: true,
		},
		{
			name: "committed execution",
			ra: runattempt.RunAttempt{
				Origin: runattempt.OriginHarnessExecution,
				Launch: &runattempt.LaunchLifecycle{PreparedAt: &prepared, CommittedAt: &committed},
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runAttemptOwnsHarnessOutcome(tt.ra); got != tt.want {
				t.Fatalf("runAttemptOwnsHarnessOutcome() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Falsifier for work_114bc68834fb: `work close` terminalizes the work's bound
// non-terminal RunAttempts by outcome. Mapping (settled design):
//
//	completed  -> succeeded
//	failed     -> failed
//	cancelled  -> abandoned
//
// Already-terminal attempts (succeeded/failed/abandoned/superseded) MUST be
// skipped — a re-close carrying the same outcome returns the existing terminal
// proof pair (idempotency, see TestCloseIsIdempotentOnSameOutcome) and must
// NOT rewrite the attempt status.

func closeTerminalizeWork(t *testing.T, s *Store, owner string, at time.Time) (string, []string) {
	t.Helper()
	w, err := s.Create(CreateInput{Owner: owner, SuccessPredicate: "close terminalizes run attempts"}, at)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(w.ID, "codex", at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "codex"}, at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "verifying", VerificationID: "ver_" + w.ID}, at.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	rattIDs := append([]string{}, claimed.RunAttemptIDs...)
	return w.ID, rattIDs
}

// closeTerminalizeWorkClaimed keeps the work at "claimed" so the cancel path
// (verifying does NOT allow a legal transition to cancelled — see work.transitions).
func closeTerminalizeWorkClaimed(t *testing.T, s *Store, owner string, at time.Time) (string, []string) {
	t.Helper()
	w, err := s.Create(CreateInput{Owner: owner, SuccessPredicate: "close terminalizes run attempts (claimed)"}, at)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(w.ID, "codex", at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	rattIDs := append([]string{}, claimed.RunAttemptIDs...)
	return w.ID, rattIDs
}

// TestCloseCompletedTerminalizesRunAttemptSucceeded: closing with outcome=completed
// must terminalize every bound non-terminal RunAttempt as "succeeded".
func TestCloseCompletedTerminalizesRunAttemptSucceeded(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	id, rattIDs := closeTerminalizeWork(t, s, "close-completed-terminalizes", fixedNow)
	if len(rattIDs) == 0 {
		t.Fatal("setup: claim must mint a RunAttempt")
	}
	if _, err := s.Close(id, CloseInput{
		Outcome:   "completed",
		MoveType:  "outward_delta",
		Summary:   "completed close",
		Changed:   true,
		ProofRefs: []string{"check:close-completed"},
	}, fixedNow.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, rid := range rattIDs {
		ra, err := runattempt.Open(s.home).Get(rid)
		if err != nil {
			t.Fatalf("RunAttempt %s lookup after close: %v", rid, err)
		}
		if ra.Status != runattempt.StatusSucceeded {
			t.Fatalf("RunAttempt %s status = %q, want %q (completed close must terminalize as succeeded)", rid, ra.Status, runattempt.StatusSucceeded)
		}
		if ra.CompletedAt.IsZero() {
			t.Fatalf("RunAttempt %s has zero CompletedAt after terminalization", rid)
		}
	}
}

// TestCloseFailedTerminalizesRunAttemptFailed: closing with outcome=failed must
// terminalize every bound non-terminal RunAttempt as "failed".
func TestCloseFailedTerminalizesRunAttemptFailed(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	id, rattIDs := closeTerminalizeWork(t, s, "close-failed-terminalizes", fixedNow)
	if len(rattIDs) == 0 {
		t.Fatal("setup: claim must mint a RunAttempt")
	}
	if _, err := s.Close(id, CloseInput{
		Outcome:  "failed",
		MoveType: "outward_delta",
		Summary:  "failed close",
	}, fixedNow.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, rid := range rattIDs {
		ra, err := runattempt.Open(s.home).Get(rid)
		if err != nil {
			t.Fatalf("RunAttempt %s lookup after close: %v", rid, err)
		}
		if ra.Status != runattempt.StatusFailed {
			t.Fatalf("RunAttempt %s status = %q, want %q (failed close must terminalize as failed)", rid, ra.Status, runattempt.StatusFailed)
		}
		if ra.CompletedAt.IsZero() {
			t.Fatalf("RunAttempt %s has zero CompletedAt after terminalization", rid)
		}
	}
}

// TestCloseCancelledTerminalizesRunAttemptAbandoned: closing with outcome=cancelled
// must terminalize every bound non-terminal RunAttempt as "abandoned".
func TestCloseCancelledTerminalizesRunAttemptAbandoned(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	id, rattIDs := closeTerminalizeWorkClaimed(t, s, "close-cancelled-terminalizes", fixedNow)
	if len(rattIDs) == 0 {
		t.Fatal("setup: claim must mint a RunAttempt")
	}
	if _, err := s.Close(id, CloseInput{
		Outcome:  "cancelled",
		MoveType: "audit_only",
		Summary:  "cancelled close",
	}, fixedNow.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, rid := range rattIDs {
		ra, err := runattempt.Open(s.home).Get(rid)
		if err != nil {
			t.Fatalf("RunAttempt %s lookup after close: %v", rid, err)
		}
		if ra.Status != runattempt.StatusAbandoned {
			t.Fatalf("RunAttempt %s status = %q, want %q (cancelled close must terminalize as abandoned)", rid, ra.Status, runattempt.StatusAbandoned)
		}
		if ra.CompletedAt.IsZero() {
			t.Fatalf("RunAttempt %s has zero CompletedAt after terminalization", rid)
		}
	}
}

// TestCloseSkipsAlreadyTerminalRunAttempts: a RunAttempt that is ALREADY terminal
// (succeeded/failed/abandoned/superseded) when the close runs must NOT be
// rewritten. Close must skip it.
func TestCloseSkipsAlreadyTerminalRunAttempts(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	id, rattIDs := closeTerminalizeWork(t, s, "close-skips-terminal", fixedNow)
	if len(rattIDs) == 0 {
		t.Fatal("setup: claim must mint a RunAttempt")
	}
	// Terminalize the claim-minted attempt, which already has exact claim
	// action/policy authority. A free-floating body is not valid close proof.
	preTerminalID := rattIDs[0]
	preTerminal, err := runattempt.Open(s.home).Complete(preTerminalID, runattempt.StatusSucceeded, fixedNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("plant pre-terminal RunAttempt terminal status: %v", err)
	}

	if _, err := s.Close(id, CloseInput{
		Outcome:   "completed",
		MoveType:  "outward_delta",
		Summary:   "close skips pre-terminal",
		Changed:   true,
		ProofRefs: []string{"check:close-skip"},
	}, fixedNow.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}

	// The pre-terminal attempt must still be succeeded and CompletedAt must be unchanged.
	after, err := runattempt.Open(s.home).Get(preTerminalID)
	if err != nil {
		t.Fatalf("pre-terminal RunAttempt lookup after close: %v", err)
	}
	if after.Status != runattempt.StatusSucceeded {
		t.Fatalf("pre-terminal RunAttempt status = %q, want %q (close must skip already-terminal)", after.Status, runattempt.StatusSucceeded)
	}
	if !after.CompletedAt.Equal(preTerminal.CompletedAt) {
		t.Fatalf("pre-terminal RunAttempt CompletedAt moved: was %v, now %v", preTerminal.CompletedAt, after.CompletedAt)
	}
}

func TestCloseRejectsTerminalWorkCloseAttemptWithStaleProofPair(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	workID, runAttemptIDs := closeTerminalizeWork(t, s, "close-rejects-stale-work-close-proof", fixedNow)
	if len(runAttemptIDs) != 1 {
		t.Fatalf("claim attempts = %v, want one reservation", runAttemptIDs)
	}
	if _, err := runattempt.Open(s.home).CompleteForWorkClose(
		runAttemptIDs[0],
		runattempt.StatusSucceeded,
		"rec_stale",
		"tr_stale",
		fixedNow.Add(4*time.Minute),
	); err != nil {
		t.Fatal(err)
	}

	_, err := s.Close(workID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "stale work-close ownership must not certify a new close",
		Changed:   true,
		ProofRefs: []string{"check:stale-work-close-proof"},
	}, fixedNow.Add(5*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "work.close proof pair") {
		t.Fatalf("Close error = %v, want stale work.close proof pair refusal", err)
	}
	work, getErr := s.Get(workID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if work.State != "verifying" {
		t.Fatalf("stale work.close proof refusal changed work state = %q, want verifying", work.State)
	}
}

func TestCloseCompletedAcceptsImmutableTerminalRetryHistory(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	workID, claimAttemptIDs := closeTerminalizeWork(t, s, "close-retry-history", fixedNow)
	if len(claimAttemptIDs) != 1 {
		t.Fatalf("claim attempts = %v, want one reservation", claimAttemptIDs)
	}
	attempts := runattempt.Open(s.home)
	claimAttempt, err := attempts.CompleteWithReason(
		claimAttemptIDs[0],
		runattempt.StatusAbandoned,
		"historical claim lane ended before retry execution",
		fixedNow.Add(4*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	failed := attachTerminalCloseHistoryAttempt(
		t, s, workID, "", runattempt.StatusFailed, true, fixedNow.Add(5*time.Minute),
	)
	succeeded := attachTerminalCloseHistoryAttempt(
		t, s, workID, failed.ID, runattempt.StatusSucceeded, true, fixedNow.Add(6*time.Minute),
	)

	before := map[string][]byte{
		claimAttempt.ID: runAttemptBytesForCloseTest(t, s.home, claimAttempt.ID),
		failed.ID:       runAttemptBytesForCloseTest(t, s.home, failed.ID),
		succeeded.ID:    runAttemptBytesForCloseTest(t, s.home, succeeded.ID),
	}
	closed, err := s.Close(workID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "a successful retry completed the work",
		Changed:   true,
		ProofRefs: []string{"check:terminal-retry-history"},
	}, fixedNow.Add(7*time.Minute))
	if err != nil {
		t.Fatalf("close rejected valid terminal retry history: %v", err)
	}
	if closed.Work.State != "completed" {
		t.Fatalf("closed work state = %q, want completed", closed.Work.State)
	}
	for id, want := range before {
		if got := runAttemptBytesForCloseTest(t, s.home, id); string(got) != string(want) {
			t.Fatalf("close rewrote immutable terminal retry history %s", id)
		}
	}
	report, err := s.ProofChainDoctor(fixedNow.Add(8*time.Minute), ProofChainDoctorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.RunAttemptOutcomeMismatch != 0 || proofChainFindingContains(report, "run_attempt_outcome_mismatch") {
		t.Fatalf("doctor rejected valid terminal retry history: counts=%+v findings=%+v", report.Counts, report.Findings)
	}
}

func TestCloseCompletedRejectsUnretriedTerminalFailure(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	workID, _ := closeTerminalizeWork(t, s, "close-unretried-failure", fixedNow)
	failed := attachTerminalCloseHistoryAttempt(
		t, s, workID, "", runattempt.StatusFailed, true, fixedNow.Add(5*time.Minute),
	)
	before := runAttemptBytesForCloseTest(t, s.home, failed.ID)

	if _, err := s.Close(workID, CloseInput{
		Outcome:  "completed",
		MoveType: "control_plane_repair",
		Summary:  "an unrelated failed execution cannot prove completed work",
	}, fixedNow.Add(6*time.Minute)); err == nil {
		t.Fatal("close accepted an unretried terminal failure")
	}
	work, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if work.State != "verifying" {
		t.Fatalf("refused close changed work state = %q, want verifying", work.State)
	}
	if got := runAttemptBytesForCloseTest(t, s.home, failed.ID); string(got) != string(before) {
		t.Fatal("refused close rewrote the unrelated terminal failure")
	}
}

func TestCloseRejectsCyclicRetryLineage(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	workID, _ := closeTerminalizeWork(t, s, "close-cyclic-retry-lineage", fixedNow)
	failed := attachTerminalCloseHistoryAttempt(
		t, s, workID, "", runattempt.StatusFailed, true, fixedNow.Add(5*time.Minute),
	)
	failed.PredecessorID = failed.ID
	writeRunAttemptForReconcileTest(t, s, failed)
	before := runAttemptBytesForCloseTest(t, s.home, failed.ID)

	_, err := s.Close(workID, CloseInput{
		Outcome:  "completed",
		MoveType: "control_plane_repair",
		Summary:  "cyclic retry lineage cannot excuse a contradictory terminal attempt",
	}, fixedNow.Add(6*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "retry lineage contains a cycle") {
		t.Fatalf("cyclic retry lineage error = %v, want cycle refusal", err)
	}
	work, getErr := s.Get(workID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if work.State != "verifying" {
		t.Fatalf("cycle refusal changed work state = %q, want verifying", work.State)
	}
	if got := runAttemptBytesForCloseTest(t, s.home, failed.ID); string(got) != string(before) {
		t.Fatal("cycle refusal rewrote the terminal attempt")
	}
}

func TestCloseCompletedRejectsOriginOnlyHarnessAsClaimReplacement(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	workID, claimAttemptIDs := closeTerminalizeWork(t, s, "close-origin-only-claim-replacement", fixedNow)
	attempts := runattempt.Open(s.home)
	if _, err := attempts.CompleteWithReason(
		claimAttemptIDs[0],
		runattempt.StatusAbandoned,
		"claim ended before any proved harness launch",
		fixedNow.Add(4*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	attachTerminalCloseHistoryAttempt(
		t, s, workID, "", runattempt.StatusSucceeded, false, fixedNow.Add(5*time.Minute),
	)

	if _, err := s.Close(workID, CloseInput{
		Outcome:  "completed",
		MoveType: "control_plane_repair",
		Summary:  "origin stamping alone is not execution evidence",
	}, fixedNow.Add(6*time.Minute)); err == nil {
		t.Fatal("close accepted an origin-only harness as a real claim replacement")
	}
}

func TestCloseCompletedRejectsNoncausalRetryLineage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(runattempt.RunAttempt, runattempt.RunAttempt) runattempt.RunAttempt
	}{
		{
			name: "padded predecessor",
			mutate: func(_ runattempt.RunAttempt, child runattempt.RunAttempt) runattempt.RunAttempt {
				child.PredecessorID = " " + child.PredecessorID + " "
				return child
			},
		},
		{
			name: "backdated child",
			mutate: func(parent, child runattempt.RunAttempt) runattempt.RunAttempt {
				child.CreatedAt = parent.CreatedAt.Add(-time.Second)
				return child
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t)
			workID, _ := closeTerminalizeWork(t, s, "close-noncausal-retry-"+tt.name, fixedNow)
			failed := attachTerminalCloseHistoryAttempt(
				t, s, workID, "", runattempt.StatusFailed, true, fixedNow.Add(5*time.Minute),
			)
			succeeded := attachTerminalCloseHistoryAttempt(
				t, s, workID, failed.ID, runattempt.StatusSucceeded, true, fixedNow.Add(6*time.Minute),
			)
			writeRunAttemptForReconcileTest(t, s, tt.mutate(failed, succeeded))

			if _, err := s.Close(workID, CloseInput{
				Outcome:  "completed",
				MoveType: "control_plane_repair",
				Summary:  "only canonical forward retry lineage is admissible",
			}, fixedNow.Add(7*time.Minute)); err == nil {
				t.Fatal("close accepted noncausal retry lineage")
			}
		})
	}
}

func TestCloseRetryHistoryExceptionIsCompletedOnly(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	workID, _ := closeTerminalizeWork(t, s, "close-retry-exception-completed-only", fixedNow)
	succeeded := attachTerminalCloseHistoryAttempt(
		t, s, workID, "", runattempt.StatusSucceeded, true, fixedNow.Add(5*time.Minute),
	)
	attachTerminalCloseHistoryAttempt(
		t, s, workID, succeeded.ID, runattempt.StatusFailed, true, fixedNow.Add(6*time.Minute),
	)

	if _, err := s.Close(workID, CloseInput{
		Outcome:  "failed",
		MoveType: "audit_only",
		Summary:  "retry exception does not relax failed work outcome checks",
	}, fixedNow.Add(7*time.Minute)); err == nil {
		t.Fatal("failed close accepted a contradictory succeeded predecessor")
	}
}

func TestCloseCompletedRejectsFailedExecutionFollowedOnlyByClaimReservation(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "close-claim-only-retry", SuccessPredicate: "claim reservation is not a successful retry"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	attempts := runattempt.Open(s.home)
	failed, err := attempts.Mint(w.ID, "codex", "claude", fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if failed, err = attempts.SetOrigin(failed.ID, runattempt.OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	if failed, err = attempts.CommitLaunch(failed.ID, fixedNow.Add(2*time.Minute+500*time.Millisecond), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{
		To:            "claimed",
		Actor:         "codex",
		RunAttemptIDs: []string{failed.ID},
	}, fixedNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.Complete(failed.ID, runattempt.StatusFailed, fixedNow.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{
		To:          "ready",
		Actor:       "codex",
		Steal:       true,
		StealReason: "retry after failed harness",
	}, fixedNow.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "codex"}, fixedNow.Add(7*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "codex"}, fixedNow.Add(8*time.Minute)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "a later claim reservation is not a successful harness retry",
		ProofRefs: []string{"repair:retry-claim-only"},
	}, fixedNow.Add(9*time.Minute)); err == nil {
		t.Fatal("close accepted a failed execution followed only by a claim reservation")
	}
}

// TestCloseSkipRunAttemptTerminalizationLeavesAttemptMutable: a caller that owns
// its own RunAttempt lifecycle (binds close-produced proof refs to the attempt
// and completes it with its honest status AFTER the close, e.g. internal/panel
// apply's fail-soft flow) must be able to opt out. With
// SkipRunAttemptTerminalization=true the bound non-terminal attempt stays
// non-terminal after the close, so SetProofRefs + Complete still land. This is
// the falsifier for the 2026-08-07 regression where close pre-terminalized the
// attempt (completed→succeeded) and panel's post-close Complete refused.
func TestCloseSkipRunAttemptTerminalizationLeavesAttemptMutable(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	id, rattIDs := closeTerminalizeWork(t, s, "close-skip-runattempt-termination", fixedNow)
	if len(rattIDs) == 0 {
		t.Fatal("setup: claim must mint a RunAttempt")
	}
	requests := make([]RunAttemptTerminalizationRequest, 0, len(rattIDs))
	for _, rid := range rattIDs {
		requests = append(requests, RunAttemptTerminalizationRequest{
			RunAttemptID: rid,
			Owner:        runattempt.TerminalizationOwnerPanelApply,
			TargetStatus: runattempt.StatusFailed,
		})
	}
	if _, err := s.Close(id, CloseInput{
		Outcome:                       "completed",
		MoveType:                      "outward_delta",
		Summary:                       "close with caller-owned RunAttempt",
		Changed:                       true,
		ProofRefs:                     []string{"check:close-skip-runattempt-termination"},
		SkipRunAttemptTerminalization: true,
		RunAttemptTerminalizations:    requests,
	}, fixedNow.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, rid := range rattIDs {
		ra, err := runattempt.Open(s.home).Get(rid)
		if err != nil {
			t.Fatalf("RunAttempt %s lookup after close: %v", rid, err)
		}
		if runattempt.IsTerminal(ra.Status) {
			t.Fatalf("RunAttempt %s status = %q, want non-terminal (opt-out must leave the attempt mutable for the caller's own completion)", rid, ra.Status)
		}
		if ra.TerminalizationIntent == nil {
			t.Fatalf("RunAttempt %s has no durable caller-owned terminalization intent", rid)
		}
		// The caller's post-close proof-ref binding + completion must still land
		// (this is what internal/panel apply does after its fail-soft close).
		if _, err := runattempt.Open(s.home).SetProofRefs(rid, []string{"proof:post-close"}); err != nil {
			t.Fatalf("post-close SetProofRefs on opted-out RunAttempt %s: %v", rid, err)
		}
		if _, err := runattempt.Open(s.home).CompletePreparedTerminalization(rid, *ra.TerminalizationIntent, fixedNow.Add(6*time.Minute)); err != nil {
			t.Fatalf("post-close exact prepared completion on opted-out RunAttempt %s: %v", rid, err)
		}
	}
}

// TestCloseAcknowledgeAbandonedAllowsClose: closes a work whose recorded run
// attempts include a real succeeded attempt AND a pre-attempt abandoned
// attempt. The close would otherwise fail with "terminal runattempt <id>
// status abandoned contradicts close outcome completed". Caller-owned
// acknowledgement of the abandoned attempt (target_status=abandoned,
// non-empty reason) is the wedge-fix door: the close lands completed, the
// real attempt stays succeeded, the abandoned attempt keeps its status, and
// the receipt carries an `abandon_ack:<id>:<reason>` proof ref so the audit
// reads the operator's named acknowledgement.
//
// This is the lived 2026-08-23 wedge for work_2742cffbeb5f: a mis-dispatch
// was killed before any work; the real attempt did the work but had no
// PredecessorID link to the abandoned one.
func TestCloseAcknowledgeAbandonedAllowsClose(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	id, _ := closeTerminalizeWork(t, s, "close-acknowledge-abandoned", fixedNow)

	// Plant a pre-attempt that is already abandoned, attached to the same
	// work via Advance. It carries the claim-reservation authority, so the
	// close-time terminal-attempt branch hits it.
	abandoned := attachTerminalCloseHistoryAttempt(t, s, id, "", runattempt.StatusAbandoned, false, fixedNow.Add(4*time.Minute))

	// Plant the real attempt that did the work — committed launch,
	// succeeded completion, same authority bound via SetPredecessor so the
	// retry-predecessor exception does not fire.
	real := attachTerminalCloseHistoryAttempt(t, s, id, abandoned.ID, runattempt.StatusSucceeded, true, fixedNow.Add(5*time.Minute))

	res, err := s.Close(id, CloseInput{
		Outcome:   "completed",
		MoveType:  "outward_delta",
		Summary:   "acknowledge-abandoned door unblocks lived wedge",
		Changed:   true,
		ProofRefs: []string{"check:ack-abandoned"},
		RunAttemptTerminalizations: []RunAttemptTerminalizationRequest{{
			RunAttemptID: abandoned.ID,
			Owner:        runattempt.TerminalizationOwnerWorkAcknowledgeAbandoned,
			TargetStatus: runattempt.StatusAbandoned,
			Reason:       "mis-dispatch killed before any side effect; real attempt did the work",
		}},
	}, fixedNow.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("acknowledge-abandoned close: %v", err)
	}
	if res.Work.State != "completed" {
		t.Fatalf("acknowledge-abandoned close state = %q, want completed", res.Work.State)
	}

	// Real attempt keeps succeeded; abandoned attempt keeps abandoned.
	attempts := runattempt.Open(s.home)
	afterReal, err := attempts.Get(real.ID)
	if err != nil {
		t.Fatalf("real attempt lookup: %v", err)
	}
	if afterReal.Status != runattempt.StatusSucceeded {
		t.Fatalf("real attempt status = %q, want succeeded (acknowledge-abandoned must not rewrite non-target attempts)", afterReal.Status)
	}
	afterAbandoned, err := attempts.Get(abandoned.ID)
	if err != nil {
		t.Fatalf("abandoned attempt lookup: %v", err)
	}
	if afterAbandoned.Status != runattempt.StatusAbandoned {
		t.Fatalf("abandoned attempt status = %q, want abandoned (acknowledge is not a rewrite)", afterAbandoned.Status)
	}

	// Receipt carries the structured acknowledgement.
	found := false
	wantPrefix := "abandon_ack:" + abandoned.ID + ":"
	for _, ref := range res.Receipt.ProofRefs {
		if strings.HasPrefix(ref, wantPrefix) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("receipt proof_refs = %v, want one starting with %q", res.Receipt.ProofRefs, wantPrefix)
	}
}

// TestCloseAcknowledgeAbandonedRejectsEmptyReason: the door refuses when
// the caller omits a reason — naming the abandoned attempt is the whole
// point of the acknowledgement, and a silent skip would be the original
// wedge recreated.
func TestCloseAcknowledgeAbandonedRejectsEmptyReason(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	id, _ := closeTerminalizeWork(t, s, "close-ack-rejects-empty", fixedNow)
	abandoned := attachTerminalCloseHistoryAttempt(t, s, id, "", runattempt.StatusAbandoned, false, fixedNow.Add(4*time.Minute))
	// Plant a real succeeded attempt so the close can otherwise succeed.
	attachTerminalCloseHistoryAttempt(t, s, id, abandoned.ID, runattempt.StatusSucceeded, true, fixedNow.Add(5*time.Minute))

	_, err := s.Close(id, CloseInput{
		Outcome:   "completed",
		MoveType:  "outward_delta",
		Summary:   "acknowledge-abandoned must require reason",
		Changed:   true,
		ProofRefs: []string{"check:ack-abandoned-no-reason"},
		RunAttemptTerminalizations: []RunAttemptTerminalizationRequest{{
			RunAttemptID: abandoned.ID,
			Owner:        runattempt.TerminalizationOwnerWorkAcknowledgeAbandoned,
			TargetStatus: runattempt.StatusAbandoned,
			Reason:       "",
		}},
	}, fixedNow.Add(10*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "requires a non-empty reason") {
		t.Fatalf("Close error = %v, want requires-non-empty-reason refusal", err)
	}
}

// --- Lived close wedge (work_7a5836372f5e): pre-execution policy refusal authority ---

// writeDenyingPolicyDecisionJSON is writePolicyDecisionJSON
// (runattempt_authority_wedge_test.go) with Outcome "deny": a decision that
// validates yet policy.Allowed rejects.
func writeDenyingPolicyDecisionJSON(t *testing.T, s *Store, id, workID, actionID string, at time.Time) {
	t.Helper()
	pd := envelope.PolicyDecision{
		Header: envelope.Header{
			SchemaVersion: envelope.SVPolicyDecision,
			ID:            id,
			CreatedAt:     at.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "test"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		ActionIntentID: actionID,
		Outcome:        "deny",
		Reasons:        []string{"synthetic pre-execution refusal"},
	}
	if err := pd.Validate(); err != nil {
		t.Fatalf("fixture policy_decision %s invalid: %v", id, err)
	}
	if _, err := s.events.PutJSON(id, &pd, "  "); err != nil {
		t.Fatalf("put policy decision %s: %v", id, err)
	}
}

// mintPreExecutionRefusalAttempt binds a harness_execution attempt to a
// denying (act_, pol_) pair and persists that pair onto the work envelope the
// way persistProofReservation does. Returned pre-terminalization so each test
// stamps its own disposition: the exact refusal shape, or a near-miss.
func mintPreExecutionRefusalAttempt(t *testing.T, s *Store, workID string, at time.Time) (*runattempt.Store, runattempt.RunAttempt) {
	t.Helper()
	const (
		actRefusal = "act_close_wedge_refused_before_exec"
		polRefusal = "pol_close_wedge_refused_before_exec"
	)
	writeActionIntentJSON(t, s, actRefusal, workID, at)
	writeDenyingPolicyDecisionJSON(t, s, polRefusal, workID, actRefusal, at)
	attempts := runattempt.Open(s.home)
	ra, err := attempts.MintWithActionIntent(workID, "codex", runattempt.HarnessPlaceholder, actRefusal, at)
	if err != nil {
		t.Fatalf("mint refusal RunAttempt: %v", err)
	}
	if ra, err = attempts.SetOrigin(ra.ID, runattempt.OriginHarnessExecution); err != nil {
		t.Fatalf("SetOrigin: %v", err)
	}
	if ra, err = attempts.SetExecutionPosture(ra.ID, runattempt.ExecutionPosture{
		PermissionMode:   "plan",
		PolicyDecisionID: polRefusal,
	}); err != nil {
		t.Fatalf("SetExecutionPosture: %v", err)
	}
	w, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	w.ActionIntentIDs = append(w.ActionIntentIDs, actRefusal)
	w.PolicyDecisionID = polRefusal
	if err := s.writeJSON(s.workPath(workID), &w); err != nil {
		t.Fatalf("persist refusal pair onto work envelope: %v", err)
	}
	return attempts, ra
}

// TestRunAttemptPolicyRefusedBeforeExecutionExactShape covers the exact
// refusal shape plus one near-miss per dimension: every field is a condition.
func TestRunAttemptPolicyRefusedBeforeExecutionExactShape(t *testing.T) {
	t.Parallel()
	heartbeat := fixedNow.Add(time.Minute)
	prepared := fixedNow.Add(30 * time.Second)
	refusal := func(mutate func(*runattempt.RunAttempt)) runattempt.RunAttempt {
		ra := runattempt.RunAttempt{
			ID:          "ratt_refusal_shape",
			Status:      runattempt.StatusAbandoned,
			Origin:      runattempt.OriginHarnessExecution,
			SweepReason: proofResourcePolicyRefusedBeforeExecution,
			CreatedAt:   fixedNow,
			CompletedAt: fixedNow,
		}
		if mutate != nil {
			mutate(&ra)
		}
		return ra
	}
	tests := []struct {
		name string
		ra   runattempt.RunAttempt
		want bool
	}{
		{name: "exact refusal shape", ra: refusal(nil), want: true},
		{name: "status failed with exact reason", ra: refusal(func(ra *runattempt.RunAttempt) { ra.Status = runattempt.StatusFailed })},
		{name: "status superseded with exact reason", ra: refusal(func(ra *runattempt.RunAttempt) { ra.Status = runattempt.StatusSuperseded })},
		{name: "claim reservation origin", ra: refusal(func(ra *runattempt.RunAttempt) { ra.Origin = runattempt.OriginClaimReservation })},
		{name: "lease variant reason", ra: refusal(func(ra *runattempt.RunAttempt) { ra.SweepReason = "proof resource refused before execution" })},
		{name: "stale sweep reason", ra: refusal(func(ra *runattempt.RunAttempt) { ra.SweepReason = "stale: no heartbeat for 30m" })},
		{name: "empty reason", ra: refusal(func(ra *runattempt.RunAttempt) { ra.SweepReason = "" })},
		{name: "prepared launch", ra: refusal(func(ra *runattempt.RunAttempt) { ra.Launch = &runattempt.LaunchLifecycle{PreparedAt: &prepared} })},
		{name: "committed launch", ra: refusal(func(ra *runattempt.RunAttempt) {
			committed := prepared.Add(time.Second)
			ra.Launch = &runattempt.LaunchLifecycle{PreparedAt: &prepared, CommittedAt: &committed}
		})},
		{name: "heartbeat observed", ra: refusal(func(ra *runattempt.RunAttempt) { ra.LastHeartbeatAt = &heartbeat })},
		{name: "completed at zero", ra: refusal(func(ra *runattempt.RunAttempt) { ra.CompletedAt = time.Time{} })},
		{name: "completed after mint", ra: refusal(func(ra *runattempt.RunAttempt) { ra.CompletedAt = fixedNow.Add(time.Second) })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runAttemptPolicyRefusedBeforeExecution(tt.ra); got != tt.want {
				t.Fatalf("runAttemptPolicyRefusedBeforeExecution() = %v, want %v", got, tt.want)
			}
		})
	}
}

func sweptClaimReservationForClose(t *testing.T, s *Store, at time.Time) (string, runattempt.RunAttempt) {
	t.Helper()
	workID, rattIDs := closeTerminalizeWork(t, s, "close-swept-reservation", at)
	if len(rattIDs) != 1 {
		t.Fatalf("claim RunAttempt count = %d, want 1", len(rattIDs))
	}
	attempts := runattempt.Open(s.home)
	report, err := attempts.SweepStale(30*time.Minute, at.Add(12*time.Hour))
	if err != nil {
		t.Fatalf("sweep claim reservation: %v", err)
	}
	if report.AutoClosed != 1 || len(report.AutoClosedIDs) != 1 || report.AutoClosedIDs[0] != rattIDs[0] {
		t.Fatalf("sweep report = %+v, want one auto-closed claim reservation", report)
	}
	ra, err := attempts.Get(rattIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	return workID, ra
}

func TestRunAttemptSweptSideEffectFreeReservationExactShape(t *testing.T) {
	t.Parallel()
	prepared := fixedNow.Add(time.Minute)
	exact := func(mutate func(*runattempt.RunAttempt)) runattempt.RunAttempt {
		ra := runattempt.RunAttempt{
			ID:                  "ratt_swept_reservation",
			Status:              runattempt.StatusAbandoned,
			Origin:              runattempt.OriginClaimReservation,
			CreatedAt:           fixedNow,
			CompletedAt:         fixedNow.Add(12 * time.Hour),
			SweepReason:         "stale: side_effect_free — no activity for 12h0m0s (threshold 30m0s)",
			SweepClassification: runattempt.SweepClassificationSideEffectFree,
		}
		if mutate != nil {
			mutate(&ra)
		}
		return ra
	}
	tests := []struct {
		name string
		ra   runattempt.RunAttempt
		want bool
	}{
		{name: "exact swept reservation", ra: exact(nil), want: true},
		{name: "ambiguous classification", ra: exact(func(ra *runattempt.RunAttempt) { ra.SweepClassification = runattempt.SweepClassificationAmbiguous })},
		{name: "forged reason", ra: exact(func(ra *runattempt.RunAttempt) { ra.SweepReason = "manual side_effect_free" })},
		{name: "harness origin", ra: exact(func(ra *runattempt.RunAttempt) { ra.Origin = runattempt.OriginHarnessExecution })},
		{name: "nonterminal", ra: exact(func(ra *runattempt.RunAttempt) { ra.Status = runattempt.StatusRunning })},
		{name: "zero completion", ra: exact(func(ra *runattempt.RunAttempt) { ra.CompletedAt = time.Time{} })},
		{name: "completion at mint", ra: exact(func(ra *runattempt.RunAttempt) { ra.CompletedAt = ra.CreatedAt })},
		{name: "launch evidence", ra: exact(func(ra *runattempt.RunAttempt) { ra.Launch = &runattempt.LaunchLifecycle{PreparedAt: &prepared} })},
		{name: "artifact evidence", ra: exact(func(ra *runattempt.RunAttempt) { ra.TranscriptRef = "artifact:transcript" })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runAttemptSweptSideEffectFreeReservation(tt.ra); got != tt.want {
				t.Fatalf("runAttemptSweptSideEffectFreeReservation() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCloseCompletedAcceptsSweptSideEffectFreeReservation(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	workID, swept := sweptClaimReservationForClose(t, s, fixedNow)

	res, err := s.Close(workID, CloseInput{
		Outcome:   "completed",
		MoveType:  "outward_delta",
		Summary:   "completed close accepts a sweep-proven non-executed reservation",
		Changed:   true,
		ProofRefs: []string{"check:completed-accepts-swept-reservation"},
	}, fixedNow.Add(13*time.Hour))
	if err != nil {
		t.Fatalf("completed close refused swept side-effect-free reservation: %v", err)
	}
	if res.Work.State != "completed" {
		t.Fatalf("close state = %q, want completed", res.Work.State)
	}
	after, err := runattempt.Open(s.home).Get(swept.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != runattempt.StatusAbandoned ||
		after.SweepClassification != runattempt.SweepClassificationSideEffectFree ||
		!after.CompletedAt.Equal(swept.CompletedAt) {
		t.Fatalf("close rewrote swept reservation: before=%+v after=%+v", swept, after)
	}
}

func TestCloseRejectsSweptReservationNearMisses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		reason         string
		classification string
	}{
		{
			name:           "ambiguous sweep",
			reason:         "stale: ambiguous — no activity for 12h0m0s (threshold 30m0s)",
			classification: runattempt.SweepClassificationAmbiguous,
		},
		{
			name:           "forged side-effect-free reason",
			reason:         "manual side_effect_free",
			classification: runattempt.SweepClassificationSideEffectFree,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t)
			workID, rattIDs := closeTerminalizeWork(t, s, "close-swept-near-miss", fixedNow)
			attempts := runattempt.Open(s.home)
			if _, err := attempts.CompleteWithReasonAndClassification(
				rattIDs[0],
				runattempt.StatusAbandoned,
				tt.reason,
				tt.classification,
				fixedNow.Add(12*time.Hour),
			); err != nil {
				t.Fatalf("stamp near-miss sweep: %v", err)
			}
			before := snapshotCloseAuthorityFiles(t, s.home)
			_, err := s.Close(workID, CloseInput{
				Outcome:   "completed",
				MoveType:  "outward_delta",
				Summary:   "near-miss sweep must not bypass outcome consistency",
				Changed:   true,
				ProofRefs: []string{"check:reject-swept-reservation-near-miss"},
			}, fixedNow.Add(13*time.Hour))
			if err == nil || !strings.Contains(err.Error(), "status abandoned contradicts close outcome completed") {
				t.Fatalf("Close error = %v, want abandoned/completed outcome contradiction", err)
			}
			if after := snapshotCloseAuthorityFiles(t, s.home); !reflect.DeepEqual(after, before) {
				t.Fatal("near-miss sweep rejection mutated durable authority")
			}
		})
	}
}

func TestCloseFailedRejectsSweptSideEffectFreeReservation(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	workID, _ := sweptClaimReservationForClose(t, s, fixedNow)
	before := snapshotCloseAuthorityFiles(t, s.home)

	_, err := s.Close(workID, CloseInput{
		Outcome:  "failed",
		MoveType: "audit_only",
		Summary:  "failed close must not inherit the completed-close exception",
	}, fixedNow.Add(13*time.Hour))
	if err == nil || !strings.Contains(err.Error(), "status abandoned contradicts close outcome failed") {
		t.Fatalf("Close error = %v, want abandoned/failed outcome contradiction", err)
	}
	if after := snapshotCloseAuthorityFiles(t, s.home); !reflect.DeepEqual(after, before) {
		t.Fatal("failed-outcome rejection mutated durable authority")
	}
}

// TestCloseCompletedAcceptsPolicyRefusedBeforeExecution exercises both the
// authority and outcome-consistency gates for the lived close wedge.
func TestCloseCompletedAcceptsPolicyRefusedBeforeExecution(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	workID, _ := closeTerminalizeWork(t, s, "close-completed-accepts-pre-exec-refusal", fixedNow)
	attempts, ra := mintPreExecutionRefusalAttempt(t, s, workID, fixedNow.Add(2*time.Minute))
	refused, err := attempts.CompleteWithReason(ra.ID, runattempt.StatusAbandoned, proofResourcePolicyRefusedBeforeExecution, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("abandon refusal RunAttempt: %v", err)
	}

	res, err := s.Close(workID, CloseInput{
		Outcome:       "completed",
		MoveType:      "outward_delta",
		Summary:       "completed close accepts the exact pre-execution refusal shape",
		Changed:       true,
		ProofRefs:     []string{"check:completed-accepts-pre-exec-refusal"},
		RunAttemptIDs: []string{refused.ID},
	}, fixedNow.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("completed close refused the exact pre-execution refusal shape: %v", err)
	}
	if res.Work.State != "completed" {
		t.Fatalf("close state = %q, want completed", res.Work.State)
	}
	after, err := runattempt.Open(s.home).Get(refused.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != runattempt.StatusAbandoned || after.SweepReason != proofResourcePolicyRefusedBeforeExecution {
		t.Fatalf("refused attempt rewritten by completed close: status=%q sweep_reason=%q", after.Status, after.SweepReason)
	}
	if after.Launch != nil || after.LastHeartbeatAt != nil || !after.CompletedAt.Equal(refused.CompletedAt) {
		t.Fatalf("refused attempt acquired execution evidence during completed close: launch=%v heartbeat=%v completed_at=%v", after.Launch, after.LastHeartbeatAt, after.CompletedAt)
	}
}

func TestCloseFailedRejectsPolicyRefusedBeforeExecutionOutcome(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	workID, _ := closeTerminalizeWork(t, s, "close-failed-rejects-pre-exec-refusal", fixedNow)
	attempts, ra := mintPreExecutionRefusalAttempt(t, s, workID, fixedNow.Add(2*time.Minute))
	refused, err := attempts.CompleteWithReason(ra.ID, runattempt.StatusAbandoned, proofResourcePolicyRefusedBeforeExecution, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("abandon refusal RunAttempt: %v", err)
	}
	before := snapshotCloseAuthorityFiles(t, s.home)

	_, err = s.Close(workID, CloseInput{
		Outcome:       "failed",
		MoveType:      "audit_only",
		Summary:       "failed close must not inherit the completed-close exception",
		RunAttemptIDs: []string{refused.ID},
	}, fixedNow.Add(5*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "status abandoned contradicts close outcome failed") {
		t.Fatalf("Close error = %v, want abandoned/failed outcome contradiction", err)
	}
	if after := snapshotCloseAuthorityFiles(t, s.home); !reflect.DeepEqual(after, before) {
		t.Fatal("failed-outcome rejection mutated durable authority")
	}
}

// TestCloseRejectsNearMissPreExecutionRefusalEvidence: one dimension off the
// exact shape and the exception must not fire — the does-not-allow rejection
// returns and durable authority stays untouched.
func TestCloseRejectsNearMissPreExecutionRefusalEvidence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// terminalize stamps the exact refusal shape minus one dimension.
		terminalize func(t *testing.T, attempts *runattempt.Store, ra runattempt.RunAttempt, at time.Time) runattempt.RunAttempt
	}{
		{
			name: "lease variant reason",
			terminalize: func(t *testing.T, attempts *runattempt.Store, ra runattempt.RunAttempt, at time.Time) runattempt.RunAttempt {
				out, err := attempts.CompleteWithReason(ra.ID, runattempt.StatusAbandoned, "proof resource refused before execution", at)
				if err != nil {
					t.Fatalf("abandon: %v", err)
				}
				return out
			},
		},
		{
			name: "heartbeat observed",
			terminalize: func(t *testing.T, attempts *runattempt.Store, ra runattempt.RunAttempt, at time.Time) runattempt.RunAttempt {
				if _, err := attempts.Heartbeat(ra.ID, at.Add(time.Second)); err != nil {
					t.Fatalf("heartbeat: %v", err)
				}
				// CompletedAt stays on the mint instant: only the heartbeat differs.
				out, err := attempts.CompleteWithReason(ra.ID, runattempt.StatusAbandoned, proofResourcePolicyRefusedBeforeExecution, at)
				if err != nil {
					t.Fatalf("abandon: %v", err)
				}
				return out
			},
		},
		{
			name: "prepared launch",
			terminalize: func(t *testing.T, attempts *runattempt.Store, ra runattempt.RunAttempt, at time.Time) runattempt.RunAttempt {
				if _, err := attempts.CommitLaunch(ra.ID, at.Add(500*time.Millisecond), func() error { return nil }); err != nil {
					t.Fatalf("commit launch: %v", err)
				}
				out, err := attempts.CompleteWithReason(ra.ID, runattempt.StatusAbandoned, proofResourcePolicyRefusedBeforeExecution, at)
				if err != nil {
					t.Fatalf("abandon: %v", err)
				}
				return out
			},
		},
		{
			name: "completed after mint",
			terminalize: func(t *testing.T, attempts *runattempt.Store, ra runattempt.RunAttempt, at time.Time) runattempt.RunAttempt {
				out, err := attempts.CompleteWithReason(ra.ID, runattempt.StatusAbandoned, proofResourcePolicyRefusedBeforeExecution, at.Add(time.Second))
				if err != nil {
					t.Fatalf("abandon: %v", err)
				}
				return out
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			workID, _ := closeTerminalizeWorkClaimed(t, s, "close-refusal-near-miss", fixedNow)
			attempts, ra := mintPreExecutionRefusalAttempt(t, s, workID, fixedNow.Add(2*time.Minute))
			stamped := tc.terminalize(t, attempts, ra, fixedNow.Add(2*time.Minute))
			before := snapshotCloseAuthorityFiles(t, s.home)
			_, err := s.Close(workID, CloseInput{
				Outcome:       "cancelled",
				MoveType:      "audit_only",
				Summary:       "near-miss refusal evidence must stay a rejection",
				RunAttemptIDs: []string{stamped.ID},
			}, fixedNow.Add(5*time.Minute))
			if err == nil || !strings.Contains(err.Error(), "does not allow action") {
				t.Fatalf("Close error = %v, want the does-not-allow rejection", err)
			}
			if after := snapshotCloseAuthorityFiles(t, s.home); !reflect.DeepEqual(after, before) {
				t.Fatal("near-miss refusal rejection mutated durable authority")
			}
		})
	}
}
