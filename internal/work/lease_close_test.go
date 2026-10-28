package work

// Close-gate lease enforcement (2026-07-09 packet). Tonight's twin-collisions (see
// docs_private/plan/packets/2026-07-09-close-gate-lease.md) showed leases exist but nothing
// enforces them at close: a non-holder actor could close a work another actor's executor
// was still mid-build on. These tests pin the falsifier list from that packet.
//
// 2026-07-31 incident (work_close_steal_lease_corruption_20260731): a close --steal that
// PASSED the lease gate transferred the lease BEFORE a downstream validation refused
// the closure, leaving the work bricked — lease named actor-b, but the work was still in
// "running" state under actor-a's original build. Refusal must be atomic across every
// lifecycle surface, including the lease itself and any audit records the gate would
// otherwise append eagerly. See TestCloseStealRefusalLeavesLifecycleByteIdentical.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/runattempt"
)

// Regression for work_close_steal_lease_corruption_20260731. A close --steal may pass the
// lease gate and re-point the lease BEFORE a later close validation refuses the closure
// (e.g. control_plane_repair outcome with no proof refs). The lease and every audit
// record the gate eagerly appended must be rolled back so the work is not bricked.
//
// Today's bug: leaseHolderCloseGate runs at the top of closeLocked (before ClosePlan
// and policy evaluation), so a valid --steal --reason transfers the lease to actor-b,
// then ClosePlan/policy refuses the close. The work is left in its pre-close state
// (e.g. "running") with a lease naming actor-b — exactly the wave10 brick pattern from
// 2026-07-31. After the fix, the lease must be byte-identical to before the attempt and
// no audit/event/ActionIntent may have been appended.
func TestCloseStealRefusalLeavesLifecycleByteIdentical(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "lane-w", SuccessPredicate: "refused steal is atomic"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "actor-a", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: "actor-a"}, fixedNow); err != nil {
		t.Fatalf("advance: %v", err)
	}
	before := snapshotLifecycleAuthority(t, s)

	_, err = s.Close(w.ID, CloseInput{
		Outcome:     "completed",
		MoveType:    "control_plane_repair",
		Changed:     true,
		Actor:       "actor-b",
		Steal:       true,
		StealReason: "original executor is unavailable",
	}, fixedNow)
	if err == nil {
		t.Fatal("expected close to refuse a changed control-plane repair without proof")
	}
	if !strings.Contains(err.Error(), "proof") {
		t.Fatalf("expected proof refusal, got %v", err)
	}

	after := snapshotLifecycleAuthority(t, s)
	assertLifecycleAuthorityUnchanged(t, before, after)
}

// Regression for REL-P0-001. A valid steal must not transfer the lease or
// publish a successful steal trail until every RunAttempt terminalization
// intent is durably prepared. A preparation failure is still before the
// recoverable close boundary, so work, lease, and lifecycle authority must
// remain byte-identical and the exact close must remain retryable.
func TestCloseStealPrepareFailureLeavesLeaseAndWorkUnchangedThenRetries(t *testing.T) {
	s := newStore(t)
	workID, rattIDs := closeTerminalizeWork(t, s, "close-steal-prepare-failure", fixedNow)
	before := snapshotLifecycleAuthority(t, s)
	beforeAttempts := map[string][]byte{}
	for _, id := range rattIDs {
		body, err := os.ReadFile(filepath.Join(s.home, "run_attempts", id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		beforeAttempts[id] = body
	}

	previousPrepare := prepareRunAttemptTerminalization
	t.Cleanup(func() { prepareRunAttemptTerminalization = previousPrepare })
	prepareRunAttemptTerminalization = func(_ *runattempt.Store, _ string, _ runattempt.TerminalizationIntent) (runattempt.RunAttempt, error) {
		return runattempt.RunAttempt{}, errors.New("injected runattempt prepare terminalization failure")
	}

	closeInput := CloseInput{
		Outcome:     "completed",
		MoveType:    "audit_only",
		Actor:       "actor-b",
		Steal:       true,
		StealReason: "the original executor is unavailable",
	}
	if _, err := s.Close(workID, closeInput, fixedNow); err == nil {
		t.Fatal("expected RunAttempt terminalization preparation to fail")
	} else if !strings.Contains(strings.ToLower(err.Error()), "injected runattempt prepare") {
		t.Fatalf("expected RunAttempt preparation error, got %v", err)
	}
	prepareRunAttemptTerminalization = previousPrepare

	gotWork, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if gotWork.State != "verifying" {
		t.Errorf("failed close changed work state to %q, want verifying", gotWork.State)
	}
	gotLease, err := s.readLease(workID)
	if err != nil {
		t.Fatal(err)
	}
	if gotLease.By != "codex" {
		t.Errorf("failed close transferred lease to %q, want original holder codex", gotLease.By)
	}
	_, events, err := s.Show(workID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.Contains(event.Note, "lease stolen") {
			t.Errorf("failed close published a successful steal trail: %q", event.Note)
		}
	}
	assertLifecycleAuthorityUnchanged(t, before, snapshotLifecycleAuthority(t, s))
	for id, beforeBody := range beforeAttempts {
		afterBody, err := os.ReadFile(filepath.Join(s.home, "run_attempts", id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if string(afterBody) != string(beforeBody) {
			t.Errorf("failed preparation changed RunAttempt %s", id)
		}
	}

	result, err := s.Close(workID, closeInput, fixedNow)
	if err != nil {
		t.Fatalf("retry exact close: %v", err)
	}
	if result.Work.State != "completed" {
		t.Fatalf("retry state = %q, want completed", result.Work.State)
	}
	_, events, err = s.Show(workID)
	if err != nil {
		t.Fatal(err)
	}
	stealTrails := 0
	for _, event := range events {
		if strings.Contains(event.Note, "lease stolen") {
			stealTrails++
		}
	}
	if stealTrails != 1 {
		t.Fatalf("successful retry steal trails = %d, want exactly 1", stealTrails)
	}
	if _, err := s.readLease(workID); err == nil {
		t.Fatal("terminal retry left a lease behind")
	}
	if _, err := os.Stat(s.leasePath(workID)); !os.IsNotExist(err) {
		t.Fatalf("terminal retry lease residue: %v", err)
	}
}

// The final steal decision is authorization for a terminal close, not a
// transient lease hand-off. A crash immediately after that decision or after
// the first close envelope must leave the live work and lease byte-identical,
// publish no successful steal trail, and keep the exact generation retryable.
func TestCloseStealPostGateFailureLeavesLeaseAndWorkUnchangedThenRetries(t *testing.T) {
	for _, tt := range []struct {
		name               string
		afterFirstEnvelope bool
	}{
		{name: "after_steal_decision"},
		{name: "after_first_envelope", afterFirstEnvelope: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t)
			workID, _ := closeTerminalizeWork(t, s, "close-steal-post-gate-"+tt.name, fixedNow)
			workPath := filepath.Join(s.home, "work", "work", workID+".json")
			beforeWork, err := os.ReadFile(workPath)
			if err != nil {
				t.Fatal(err)
			}
			beforeLease, err := os.ReadFile(s.leasePath(workID))
			if err != nil {
				t.Fatal(err)
			}

			closeInput := CloseInput{
				Outcome:     "completed",
				MoveType:    "audit_only",
				Actor:       "actor-b",
				Steal:       true,
				StealReason: "the original executor is unavailable",
			}
			previousDecisionSeam := closeAfterLeaseStealDecision
			previousFirstEnvelopeSeam := closeAfterFirstEnvelopeWrite
			t.Cleanup(func() {
				closeAfterLeaseStealDecision = previousDecisionSeam
				closeAfterFirstEnvelopeWrite = previousFirstEnvelopeSeam
			})
			if tt.afterFirstEnvelope {
				closeAfterFirstEnvelopeWrite = func() { panic("injected crash after first close envelope") }
			} else {
				closeAfterLeaseStealDecision = func() { panic("injected crash after close steal decision") }
			}

			crashed := false
			var closeErr error
			func() {
				defer func() {
					if recover() != nil {
						crashed = true
					}
				}()
				_, closeErr = s.Close(workID, closeInput, fixedNow)
			}()
			closeAfterLeaseStealDecision = previousDecisionSeam
			closeAfterFirstEnvelopeWrite = previousFirstEnvelopeSeam
			if !crashed {
				t.Fatalf("expected injected post-gate crash, close error=%v", closeErr)
			}

			afterWork, err := os.ReadFile(workPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(afterWork) != string(beforeWork) {
				t.Errorf("post-gate failure changed the WorkEnvelope")
			}
			afterLease, err := os.ReadFile(s.leasePath(workID))
			if err != nil {
				t.Fatal(err)
			}
			if string(afterLease) != string(beforeLease) {
				t.Errorf("post-gate failure transferred the lease")
			}
			gotWork, err := s.Get(workID)
			if err != nil {
				t.Fatal(err)
			}
			if gotWork.State != "verifying" {
				t.Errorf("post-gate failure work state = %q, want verifying", gotWork.State)
			}
			_, events, err := s.Show(workID)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if strings.Contains(event.Note, "lease stolen") {
					t.Errorf("post-gate failure published a successful steal trail: %q", event.Note)
				}
			}

			result, err := s.Close(workID, closeInput, fixedNow)
			if err != nil {
				t.Fatalf("retry exact close: %v", err)
			}
			if result.Work.State != "completed" {
				t.Fatalf("retry state = %q, want completed", result.Work.State)
			}
			_, events, err = s.Show(workID)
			if err != nil {
				t.Fatal(err)
			}
			stealTrails := 0
			for _, event := range events {
				if strings.Contains(event.Note, "lease stolen") {
					stealTrails++
				}
			}
			if stealTrails != 1 {
				t.Fatalf("successful retry steal trails = %d, want exactly 1", stealTrails)
			}
		})
	}
}

func TestCloseFailsClosedOnUnreadableLeaseAuthority(t *testing.T) {
	s := newStore(t)
	workID, _ := closeTerminalizeWork(t, s, "close-unreadable-lease", fixedNow)
	workPath := filepath.Join(s.home, "work", "work", workID+".json")
	beforeWork, err := os.ReadFile(workPath)
	if err != nil {
		t.Fatal(err)
	}
	corruptLease := []byte("{not-json")
	if err := os.WriteFile(s.leasePath(workID), corruptLease, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = s.Close(workID, CloseInput{
		Outcome:  "completed",
		MoveType: "audit_only",
		Actor:    "codex",
	}, fixedNow)
	if err == nil {
		t.Fatal("expected unreadable lease authority to fail closed")
	}
	if !strings.Contains(err.Error(), "lease authority") {
		t.Fatalf("error does not name lease authority: %v", err)
	}
	afterWork, err := os.ReadFile(workPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterWork) != string(beforeWork) {
		t.Fatal("unreadable lease authority changed the WorkEnvelope")
	}
	afterLease, err := os.ReadFile(s.leasePath(workID))
	if err != nil {
		t.Fatal(err)
	}
	if string(afterLease) != string(corruptLease) {
		t.Fatal("unreadable lease authority was rewritten")
	}
}

func TestCloseFailsClosedOnParseableLeaseAuthorityCorruption(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "blank authority", body: "{}"},
		{name: "mismatched work id", body: `{"work_id":"work_other","by":"codex","claimed_at":"2026-08-29T00:00:00Z"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t)
			workID, rattIDs := closeTerminalizeWork(t, s, "close-parseable-lease-"+tt.name, fixedNow)
			workPath := filepath.Join(s.home, "work", "work", workID+".json")
			beforeWork, err := os.ReadFile(workPath)
			if err != nil {
				t.Fatal(err)
			}
			corruptLease := []byte(tt.body)
			if err := os.WriteFile(s.leasePath(workID), corruptLease, 0o644); err != nil {
				t.Fatal(err)
			}
			before := snapshotLifecycleAuthority(t, s)
			beforeAttempts := map[string][]byte{}
			for _, id := range rattIDs {
				body, err := os.ReadFile(filepath.Join(s.home, "run_attempts", id+".json"))
				if err != nil {
					t.Fatal(err)
				}
				beforeAttempts[id] = body
			}

			_, err = s.Close(workID, CloseInput{
				Outcome:  "completed",
				MoveType: "audit_only",
				Actor:    "codex",
			}, fixedNow)
			if err == nil {
				t.Fatal("expected parseable corrupt lease authority to fail closed")
			}
			if !strings.Contains(err.Error(), "lease authority") {
				t.Fatalf("error does not name lease authority: %v", err)
			}
			afterWork, readErr := os.ReadFile(workPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(afterWork) != string(beforeWork) {
				t.Fatal("parseable corrupt lease authority changed the WorkEnvelope")
			}
			afterLease, readErr := os.ReadFile(s.leasePath(workID))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(afterLease) != string(corruptLease) {
				t.Fatal("parseable corrupt lease authority was rewritten")
			}
			assertLifecycleAuthorityUnchanged(t, before, snapshotLifecycleAuthority(t, s))
			for id, beforeBody := range beforeAttempts {
				afterBody, err := os.ReadFile(filepath.Join(s.home, "run_attempts", id+".json"))
				if err != nil {
					t.Fatal(err)
				}
				if string(afterBody) != string(beforeBody) {
					t.Fatalf("parseable corrupt lease authority changed RunAttempt %s", id)
				}
			}
		})
	}
}

// Falsifier 1 (RED-first): a work claimed by one actor is closeable by a different actor
// with no steal on today's code. This is the exact regression of tonight's canon_class
// close. On the gated code this must refuse, naming the holder and the remedy paths.
func TestCloseRefusesNonLeaseHolder(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "lane-w", SuccessPredicate: "lease enforced"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "actor-a", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	reachedPrepare := false
	previousPrepareSeam := closeAfterTerminalizationPrepare
	t.Cleanup(func() { closeAfterTerminalizationPrepare = previousPrepareSeam })
	closeAfterTerminalizationPrepare = func() { reachedPrepare = true }
	_, err = s.Close(w.ID, CloseInput{Outcome: "cancelled", MoveType: "audit_only", Actor: "actor-b"}, fixedNow)
	closeAfterTerminalizationPrepare = previousPrepareSeam
	if err == nil {
		t.Fatal("expected close by non-holder actor-b to be refused (work held by actor-a)")
	}
	if reachedPrepare {
		t.Fatal("denied close reached the RunAttempt terminalization preparation boundary")
	}
	if !strings.Contains(err.Error(), "actor-a") {
		t.Fatalf("refusal error must name the holder actor-a: %v", err)
	}
	if !strings.Contains(err.Error(), "--steal") {
		t.Fatalf("refusal error must name the --steal remedy: %v", err)
	}
	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "claimed" {
		t.Fatalf("refused close must not mutate work state: got %q", after.State)
	}
}

// Falsifier 2: --steal with a non-empty reason succeeds, and the trail (lifecycle events)
// carries the prior holder, the reason string, and the stealing actor. --steal with an
// empty reason refuses exactly like a plain non-holder close.
func TestCloseStealOverridesLeaseHolder(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "lane-w", SuccessPredicate: "steal path"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "actor-a", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Empty reason refuses even with --steal set.
	if _, err := s.Close(w.ID, CloseInput{Outcome: "cancelled", MoveType: "audit_only", Actor: "actor-b", Steal: true}, fixedNow); err == nil {
		t.Fatal("expected --steal with empty reason to be refused")
	}
	if got, err := s.Get(w.ID); err != nil || got.State != "claimed" {
		t.Fatalf("empty-reason steal attempt must not mutate work: state=%q err=%v", got.State, err)
	}

	stealReason := "actor-a's executor died mid-build"
	plan, err := s.ClosePlan(w.ID, fixedNow, ClosePlanOptions{
		Actor:       "actor-b",
		Outcome:     "cancelled",
		MoveType:    "audit_only",
		Steal:       true,
		StealReason: stealReason,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.ActionIntent.Preview, "lease_override_from \"actor-a\"") ||
		!strings.Contains(plan.ActionIntent.Preview, stealReason) {
		t.Fatalf("close ActionIntent does not bind exact lease override: %q", plan.ActionIntent.Preview)
	}

	// A terminal close does not transiently hand the lease to actor-b. The exact
	// close ActionIntent binds holder and reason, the terminal event publishes the
	// successful override, and release deletes the original lease last.
	var holderAtRelease string
	var holderAtReleaseErr error
	previousSeam := closeBeforeLeaseRelease
	t.Cleanup(func() { closeBeforeLeaseRelease = previousSeam })
	closeBeforeLeaseRelease = func() {
		lease, err := s.readLease(w.ID)
		holderAtRelease, holderAtReleaseErr = lease.By, err
	}

	res, err := s.Close(w.ID, CloseInput{
		Outcome:     "cancelled",
		MoveType:    "audit_only",
		Actor:       "actor-b",
		Steal:       true,
		StealReason: stealReason,
	}, fixedNow)
	closeBeforeLeaseRelease = previousSeam
	if err != nil {
		t.Fatalf("expected steal with non-empty reason to succeed: %v", err)
	}
	if holderAtReleaseErr != nil {
		t.Fatalf("read lease at the release seam: %v", holderAtReleaseErr)
	}
	if holderAtRelease != "actor-a" {
		t.Fatalf("terminal close must retain the original holder until release: got %q, want %q", holderAtRelease, "actor-a")
	}
	if res.Work.State != "cancelled" {
		t.Fatalf("stolen close should land the work terminal, got %q", res.Work.State)
	}

	_, evs, err := s.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stealNote string
	closeActionAttached := false
	for _, e := range evs {
		if strings.Contains(e.Note, "stolen") {
			stealNote = e.Note
			if e.By != "actor-b" {
				t.Fatalf("steal event must be attributed to the stealing actor actor-b, got %q", e.By)
			}
			for _, attached := range e.Attached {
				if attached == plan.ActionIntent.ID {
					closeActionAttached = true
				}
			}
		}
	}
	if stealNote == "" {
		t.Fatal("trail missing a steal event")
	}
	if !strings.Contains(stealNote, "actor-a") {
		t.Fatalf("steal event must name the prior holder actor-a: %q", stealNote)
	}
	if !strings.Contains(stealNote, "actor-a's executor died mid-build") {
		t.Fatalf("steal event must carry the reason string: %q", stealNote)
	}
	if !closeActionAttached {
		t.Fatalf("steal event does not attach exact close ActionIntent %s", plan.ActionIntent.ID)
	}
	// The terminal close releases the original lease only after work and the
	// combined close/steal event are durable, so no live lease survives.
	if _, err := s.readLease(w.ID); err == nil {
		t.Fatal("a terminal close must release the lease it stole, leaving no residue behind")
	}
	if _, err := os.Stat(s.leasePath(w.ID)); !os.IsNotExist(err) {
		t.Fatalf("stolen lease file must be gone after the terminal close: stat err = %v", err)
	}
}

// P0 falsifier, close-path mirror (RED FIRST): actor-a claims; actor-b steals via Advance
// (a different gated verb, proven above to succeed and to transfer the lease); THEN actor-a
// attempts Close WITHOUT --steal. Both gates read the same work/leases/<id>.json through the
// shared leaseHolderStatusFor, so a steal recorded on one verb must be honored by the other —
// otherwise the original holder could route around a steal simply by calling the other
// mutation verb. On the unfixed code this test fails: actor-a's close sails through because
// the lease file was never rewritten by actor-b's advance-steal.
func TestCloseBlockedAfterAdvanceStealTransfersLease(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "lane-w", SuccessPredicate: "cross-verb steal transfer"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "actor-a", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{
		To:          "running",
		Actor:       "actor-b",
		Steal:       true,
		StealReason: "actor-a's executor died mid-build",
	}, fixedNow); err != nil {
		t.Fatalf("expected actor-b's advance-steal to succeed: %v", err)
	}

	_, err = s.Close(w.ID, CloseInput{Outcome: "completed", MoveType: "audit_only", Actor: "actor-a"}, fixedNow)
	if err == nil {
		t.Fatal("expected actor-a's post-steal close (no --steal) to be BLOCKED: the lease is now actor-b's")
	}
	if !strings.Contains(err.Error(), "actor-b") {
		t.Fatalf("refusal must name the new holder actor-b: %v", err)
	}
	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "running" {
		t.Fatalf("actor-a's blocked close must not mutate work state: got %q, want %q", after.State, "running")
	}
}

// Falsifier 3: claimed by A, closed by A — byte-identical behavior to pre-change. No
// refusal/steal events land on the trail beyond the normal claim+close events.
func TestCloseSameActorPassesUnchanged(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "lane-w", SuccessPredicate: "same actor"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "actor-a", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Close(w.ID, CloseInput{Outcome: "cancelled", MoveType: "audit_only", Actor: "actor-a"}, fixedNow); err != nil {
		t.Fatalf("same-actor close should succeed: %v", err)
	}
	_, evs, err := s.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantStates := []string{"ready", "claimed", "cancelled"}
	if len(evs) != len(wantStates) {
		t.Fatalf("same-actor close must add no gate events beyond the normal chain: got %d events %+v, want %d", len(evs), evs, len(wantStates))
	}
	for i, want := range wantStates {
		if evs[i].To != want {
			t.Errorf("event[%d].To = %q, want %q", i, evs[i].To, want)
		}
		if strings.Contains(evs[i].Note, "refused") || strings.Contains(evs[i].Note, "stolen") {
			t.Errorf("same-actor close must not add a lease-gate event: %+v", evs[i])
		}
	}
}

// Falsifier 4: an unclaimed (never-claimed) envelope closes exactly as before — fixture
// parity, no lease means no gate interference at all.
func TestCloseUnclaimedWorkUnaffected(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "lane-w", SuccessPredicate: "never claimed"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Close by an arbitrary actor that never claimed anything — no lease exists at all.
	res, err := s.Close(w.ID, CloseInput{Outcome: "cancelled", MoveType: "audit_only", Actor: "whoever"}, fixedNow)
	if err != nil {
		t.Fatalf("unclaimed close should succeed unconditionally: %v", err)
	}
	if res.Work.State != "cancelled" {
		t.Fatalf("unclaimed close should land the work terminal, got %q", res.Work.State)
	}
	_, evs, err := s.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].To != "ready" || evs[1].To != "cancelled" {
		t.Fatalf("unclaimed close must be the plain create+close chain: %+v", evs)
	}
}

// Falsifier 7: a refused close appends a refusal record/event; a second identical refusal
// does not corrupt state — the trail just grows, and Get() stays coherent.
func TestCloseRefusalAppendsIdempotentTrail(t *testing.T) {
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "lane-w", SuccessPredicate: "idempotent refusal"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "actor-a", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.Close(w.ID, CloseInput{Outcome: "cancelled", MoveType: "audit_only", Actor: "actor-b"}, fixedNow); err == nil {
			t.Fatalf("refusal %d: expected non-holder close to be refused", i)
		}
	}
	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatalf("get after two refusals: %v", err)
	}
	if after.State != "claimed" {
		t.Fatalf("two refusals must leave work state untouched: got %q", after.State)
	}
	_, evs, err := s.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	refusals := 0
	for _, e := range evs {
		if strings.Contains(e.Note, "refused") {
			refusals++
		}
	}
	if refusals != 2 {
		t.Fatalf("expected 2 refusal events on the trail (idempotent-safe, not deduped-away or corrupted), got %d: %+v", refusals, evs)
	}
}

// ClosePlan (the non-mutating preview) must report the same lease-holder denial the real
// close would hit, without recording or mutating anything — the plan surface reports the
// denial too, per the packet's insertion point at close_plan.go.
func TestClosePlanReportsLeaseHolderDenialWithoutMutating(t *testing.T) {
	s := newStore(t)
	workID := readyRunningWork(t, s) // claimed + advanced to running by "codex"
	before, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.ClosePlan(workID, fixedNow, ClosePlanOptions{Actor: "actor-b", Outcome: "completed", MoveType: "control_plane_repair"})
	if err != nil {
		t.Fatalf("close-plan: %v", err)
	}
	if plan.NoAction == "" || !strings.Contains(plan.NoAction, "codex") {
		t.Fatalf("close-plan must report the lease holder denial, got NoAction=%q", plan.NoAction)
	}
	if len(plan.CloseCommand) != 0 {
		t.Fatalf("close-plan must not offer a close command when lease-blocked: %v", plan.CloseCommand)
	}
	after, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != before.State || len(after.ActionIntentIDs) != len(before.ActionIntentIDs) {
		t.Fatalf("close-plan must not mutate work: before=%+v after=%+v", before, after)
	}

	// With a valid steal, the plan reports steal-readiness and the printed close command
	// carries --steal --reason.
	stealPlan, err := s.ClosePlan(workID, fixedNow, ClosePlanOptions{
		Actor: "actor-b", Outcome: "completed", MoveType: "control_plane_repair",
		Steal: true, StealReason: "codex executor is dead",
	})
	if err != nil {
		t.Fatalf("close-plan with steal: %v", err)
	}
	if len(stealPlan.CloseCommand) == 0 {
		t.Fatalf("steal-ready close-plan must offer a close command: %+v", stealPlan)
	}
	if !commandHasFlagValue(stealPlan.CloseCommand, "--reason", "codex executor is dead") {
		t.Fatalf("steal-ready close command must carry --reason: %v", stealPlan.CloseCommand)
	}
	found := false
	for _, part := range stealPlan.CloseCommand {
		if part == "--steal" {
			found = true
		}
	}
	if !found {
		t.Fatalf("steal-ready close command must carry --steal: %v", stealPlan.CloseCommand)
	}
}
