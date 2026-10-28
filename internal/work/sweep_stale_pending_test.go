package work

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

func TestSweepStalePersistenceFaultsRecoverExactly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		stage            string
		pending          bool
		recoveryRequired bool
		state            string
		eventDelta       int
		leasePresent     bool
	}{
		{name: "before pending persist", stage: sweepStaleFaultBeforePendingPersist, state: "claimed", leasePresent: true},
		{name: "after pending persist", stage: sweepStaleFaultAfterPendingPersist, pending: true, recoveryRequired: true, state: "claimed", leasePresent: true},
		{name: "before event publish", stage: sweepStaleFaultBeforeEventPublish, pending: true, recoveryRequired: true, state: "claimed", leasePresent: true},
		{name: "before work publish", stage: sweepStaleFaultBeforeWorkPublish, pending: true, recoveryRequired: true, state: "claimed", eventDelta: 1, leasePresent: true},
		{name: "after publication", stage: sweepStaleFaultAfterPublication, pending: true, recoveryRequired: true, state: "cancelled", eventDelta: 1, leasePresent: true},
		{name: "before lease release", stage: sweepStaleFaultBeforeLeaseRelease, pending: true, recoveryRequired: true, state: "cancelled", eventDelta: 1, leasePresent: true},
		{name: "before pending clear", stage: sweepStaleFaultBeforePendingClear, pending: true, recoveryRequired: true, state: "cancelled", eventDelta: 1},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			w := staleEnvelope(t, s, "claimed", 30*24*time.Hour, fixedNow)
			beforeEvents, err := s.eventsFor(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			fired := false
			injected := errors.New("injected " + test.stage)
			s.sweepStaleFault = func(stage string) error {
				if stage == test.stage && !fired {
					fired = true
					return injected
				}
				return nil
			}

			failed, err := s.SweepStaleApply(72*time.Hour, newFakeProbe(), fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			s.sweepStaleFault = nil
			if !fired {
				t.Fatalf("fault stage %q was not reached", test.stage)
			}
			if failed.Reclaimed != 0 || failed.Skipped != 1 || len(failed.Candidates) != 1 {
				t.Fatalf("failed report = %+v, want one skipped candidate", failed)
			}
			gotRecoveryRequired := strings.Contains(failed.Candidates[0].ReclaimError, "sweep-stale recovery_required")
			if gotRecoveryRequired != test.recoveryRequired {
				t.Fatalf("recovery_required = %v, want %v: %s", gotRecoveryRequired, test.recoveryRequired, failed.Candidates[0].ReclaimError)
			}
			pendingPath := s.sweepStalePendingPath(w.ID)
			_, statErr := os.Stat(pendingPath)
			if (statErr == nil) != test.pending {
				t.Fatalf("pending present = %v, want %v: %v", statErr == nil, test.pending, statErr)
			}
			current, err := s.Get(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.State != test.state {
				t.Fatalf("state after %s = %q, want %q", test.stage, current.State, test.state)
			}
			afterEvents, err := s.eventsFor(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(afterEvents) != len(beforeEvents)+test.eventDelta {
				t.Fatalf("event count after %s = %d, want %d", test.stage, len(afterEvents), len(beforeEvents)+test.eventDelta)
			}
			if !reflect.DeepEqual(afterEvents[:len(beforeEvents)], beforeEvents) {
				t.Fatalf("event prefix changed after %s", test.stage)
			}
			_, leaseErr := s.readLease(w.ID)
			if (leaseErr == nil) != test.leasePresent {
				t.Fatalf("lease present after %s = %v, want %v: %v", test.stage, leaseErr == nil, test.leasePresent, leaseErr)
			}

			recovered, err := s.SweepStaleApply(72*time.Hour, newFakeProbe(), fixedNow.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if recovered.Reclaimed != 1 || len(recovered.Candidates) != 1 || !recovered.Candidates[0].Reclaimed {
				t.Fatalf("recovery report = %+v, want one exact completed reclaim", recovered)
			}
			current, err = s.Get(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.State != "cancelled" {
				t.Fatalf("recovered state = %q, want cancelled", current.State)
			}
			finalEvents, err := s.eventsFor(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(finalEvents) != len(beforeEvents)+1 || !reflect.DeepEqual(finalEvents[:len(beforeEvents)], beforeEvents) {
				t.Fatalf("recovered event history = %+v, want unchanged prefix plus one event", finalEvents)
			}
			if _, err := s.readLease(w.ID); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("lease after recovery: %v", err)
			}
			if _, err := os.Stat(pendingPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("pending frame after recovery: %v", err)
			}
		})
	}
}

func TestSweepStalePendingFencesLifecycleMutation(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := staleEnvelope(t, s, "claimed", 30*24*time.Hour, fixedNow)
	fired := false
	s.sweepStaleFault = func(stage string) error {
		if stage == sweepStaleFaultAfterPendingPersist && !fired {
			fired = true
			return errors.New("freeze before publication")
		}
		return nil
	}
	report, err := s.SweepStaleApply(72*time.Hour, newFakeProbe(), fixedNow)
	s.sweepStaleFault = nil
	if err != nil {
		t.Fatal(err)
	}
	if !fired || len(report.Candidates) != 1 || !strings.Contains(report.Candidates[0].ReclaimError, "recovery_required") {
		t.Fatalf("setup report = %+v, want frozen recovery frame", report)
	}

	if _, err := s.Claim(w.ID, "codex", fixedNow.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "recovery_required") {
		t.Fatalf("Claim error = %v, want sweep recovery fence", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "claimed", Actor: "codex"}, fixedNow.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "recovery_required") {
		t.Fatalf("Advance error = %v, want sweep recovery fence", err)
	}
	if _, err := s.Close(w.ID, CloseInput{}, fixedNow.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "recovery_required") {
		t.Fatalf("Close error = %v, want sweep recovery fence", err)
	}
	current, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != "claimed" {
		t.Fatalf("fenced mutations changed state to %q", current.State)
	}
	if _, err := s.readLease(w.ID); err != nil {
		t.Fatalf("fenced mutations changed lease: %v", err)
	}

	recovered, err := s.SweepStaleApply(72*time.Hour, newFakeProbe(), fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Reclaimed != 1 {
		t.Fatalf("recovery report = %+v, want one completed reclaim", recovered)
	}
}

// TestSweepStalePendingRejectsChecksumValidSemanticCorruption keeps the frame
// checksum honest while changing fields whose meaning the checksum alone cannot
// protect. A pending frame is a recovery authority, so every one of these
// mutations must be rejected before replay or fencing can trust it.
func TestSweepStalePendingRejectsChecksumValidSemanticCorruption(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*sweepStalePending) error
	}{
		{
			name: "invalid source WorkEnvelope",
			mutate: func(p *sweepStalePending) error {
				p.SourceWork.Owner = ""
				return resealSweepStaleSemanticMutation(p)
			},
		},
		{
			name: "invalid final WorkEnvelope",
			mutate: func(p *sweepStalePending) error {
				p.FinalWork.Owner = ""
				return resealSweepStaleSemanticMutation(p)
			},
		},
		{
			name: "source state is reclaimable",
			mutate: func(p *sweepStalePending) error {
				p.SourceWork.State = "ready"
				p.FinalWork = p.SourceWork
				p.FinalWork.State = sweepStaleTargetState(p.SourceWork.State)
				return resealSweepStaleSemanticMutation(p)
			},
		},
		{
			name: "held source state is not reclaimable",
			mutate: func(p *sweepStalePending) error {
				p.SourceWork.State = "held"
				p.FinalWork = p.SourceWork
				p.FinalWork.State = sweepStaleTargetState(p.SourceWork.State)
				return resealSweepStaleSemanticMutation(p)
			},
		},
		{
			name: "threshold is positive",
			mutate: func(p *sweepStalePending) error {
				p.ThresholdNanos = 0
				return nil
			},
		},
		{
			name: "negative threshold is rejected",
			mutate: func(p *sweepStalePending) error {
				p.ThresholdNanos = -1
				return nil
			},
		},
		{
			name: "frozen timestamp is parseable and nonzero",
			mutate: func(p *sweepStalePending) error {
				p.FrozenAt = time.Time{}.Format(time.RFC3339Nano)
				p.Event.TS = p.FrozenAt
				return nil
			},
		},
		{
			name: "malformed frozen timestamp is rejected",
			mutate: func(p *sweepStalePending) error {
				p.FrozenAt = "not-a-timestamp"
				p.Event.TS = p.FrozenAt
				return nil
			},
		},
		{
			name: "frozen timestamp has no padding",
			mutate: func(p *sweepStalePending) error {
				p.FrozenAt = " " + p.FrozenAt + " "
				p.Event.TS = p.FrozenAt
				return nil
			},
		},
		{
			name: "event timestamp equals frozen timestamp",
			mutate: func(p *sweepStalePending) error {
				p.Event.TS = ts(fixedNow.Add(time.Second))
				return nil
			},
		},
		{
			name: "sweep reason is canonical and nonblank",
			mutate: func(p *sweepStalePending) error {
				p.Candidate.SweepReason = "manual reclaim"
				p.Event.Note = p.Candidate.SweepReason
				return nil
			},
		},
		{
			name: "candidate last event timestamp is parseable",
			mutate: func(p *sweepStalePending) error {
				p.Candidate.LastEventTS = "not-a-timestamp"
				return nil
			},
		},
		{
			name: "candidate last event age is exact",
			mutate: func(p *sweepStalePending) error {
				p.Candidate.LastEventAge = "1s"
				return nil
			},
		},
		{
			name: "source event prefix is nonempty",
			mutate: func(p *sweepStalePending) error {
				p.SourceEventCount = 0
				return nil
			},
		},
		{
			name: "candidate heartbeat is blank",
			mutate: func(p *sweepStalePending) error {
				p.Candidate.LastHeartbeatAt = "heartbeat-forgery"
				return nil
			},
		},
		{
			name: "candidate live attempts is zero",
			mutate: func(p *sweepStalePending) error {
				p.Candidate.LiveAttempts = 1
				return nil
			},
		},
		{
			name: "candidate delegate is not live",
			mutate: func(p *sweepStalePending) error {
				p.Candidate.DelegateLive = true
				return nil
			},
		},
		{
			name: "candidate land lease is not held",
			mutate: func(p *sweepStalePending) error {
				p.Candidate.LeaseHeld = true
				return nil
			},
		},
		{
			name: "canonical prefix reason still matches frozen facts",
			mutate: func(p *sweepStalePending) error {
				p.Candidate.SweepReason = "sweep-stale: fabricated"
				p.Event.Note = p.Candidate.SweepReason
				return nil
			},
		},
		{
			name: "sweep reason is not blank",
			mutate: func(p *sweepStalePending) error {
				p.Candidate.SweepReason = ""
				p.Event.Note = p.Candidate.SweepReason
				return nil
			},
		},
		{
			name: "sweep reason has no padding",
			mutate: func(p *sweepStalePending) error {
				p.Candidate.SweepReason = "  " + p.Candidate.SweepReason + "  "
				p.Event.Note = p.Candidate.SweepReason
				return nil
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			w := staleEnvelope(t, s, "claimed", 30*24*time.Hour, fixedNow)
			fired := false
			s.sweepStaleFault = func(stage string) error {
				if stage == sweepStaleFaultAfterPendingPersist && !fired {
					fired = true
					return errors.New("freeze for semantic corruption test")
				}
				return nil
			}
			report, err := s.SweepStaleApply(72*time.Hour, newFakeProbe(), fixedNow)
			s.sweepStaleFault = nil
			if err != nil {
				t.Fatal(err)
			}
			if !fired || report.Reclaimed != 0 || report.Skipped != 1 {
				t.Fatalf("setup report = %+v, want one frozen pending frame", report)
			}
			pending, present, err := s.loadSweepStalePending(w.ID)
			if err != nil || !present {
				t.Fatalf("load frozen pending: present=%v err=%v", present, err)
			}
			if err := test.mutate(&pending); err != nil {
				t.Fatalf("mutate pending: %v", err)
			}
			pending.FrameSHA256, err = sweepStalePendingDigest(pending)
			if err != nil {
				t.Fatalf("reseal frame: %v", err)
			}
			if err := s.writeJSON(s.sweepStalePendingPath(w.ID), &pending); err != nil {
				t.Fatalf("persist checksum-valid corruption: %v", err)
			}
			if _, _, err := s.loadSweepStalePending(w.ID); err == nil {
				t.Fatal("checksum-valid semantic corruption was accepted")
			}
		})
	}
}

func TestSweepStalePendingCorruptionPreservesRecoveryClassification(t *testing.T) {
	s := newStore(t)
	w, pending := freezeSweepStalePendingForTest(t, s, "test-fixture")
	pending.Candidate.WorkID = "work_forged"
	pending.Candidate.SweepReason = "manual reclaim"
	pending.Event.Note = pending.Candidate.SweepReason
	var err error
	pending.FrameSHA256, err = sweepStalePendingDigest(pending)
	if err != nil {
		t.Fatalf("reseal corrupt pending: %v", err)
	}
	if err := s.writeJSON(s.sweepStalePendingPath(w.ID), &pending); err != nil {
		t.Fatalf("persist corrupt pending: %v", err)
	}

	before := snapshotLifecycleAuthority(t, s)
	for _, apply := range []bool{false, true} {
		var report SweepStaleReport
		if apply {
			report, err = s.SweepStaleApply(72*time.Hour, newFakeProbe(), fixedNow)
		} else {
			report, err = s.SweepStalePlan(72*time.Hour, newFakeProbe(), fixedNow)
		}
		if err != nil {
			t.Fatalf("sweep apply=%v over corrupt pending: %v", apply, err)
		}
		if report.Unreadable != 0 || report.Skipped != 1 || report.Reclaimed != 0 || len(report.Candidates) != 1 {
			t.Fatalf("sweep apply=%v report = %+v, want one recovery-required candidate", apply, report)
		}
		candidate := report.Candidates[0]
		if candidate.WorkID != w.ID {
			t.Fatalf("sweep apply=%v candidate work_id = %q, want live work %q", apply, candidate.WorkID, w.ID)
		}
		if !strings.Contains(candidate.ReclaimError, "sweep-stale recovery_required") {
			t.Fatalf("sweep apply=%v candidate error = %q, want recovery_required", apply, candidate.ReclaimError)
		}
	}
	if _, err := s.Claim(w.ID, "other-actor", fixedNow.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "sweep-stale recovery_required") {
		t.Fatalf("Claim over corrupt pending error = %v, want sweep-stale recovery_required", err)
	}
	if _, err := s.Close(w.ID, CloseInput{}, fixedNow.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "sweep-stale recovery_required") {
		t.Fatalf("Close over corrupt pending error = %v, want sweep-stale recovery_required", err)
	}
	assertLifecycleAuthorityUnchanged(t, before, snapshotLifecycleAuthority(t, s))
}

func TestSweepStalePendingRejectsForgedLastEventCandidateWithoutMutation(t *testing.T) {
	s := newStore(t)
	w, pending := freezeSweepStalePendingForTest(t, s, "test-fixture")
	frozenAt, err := parseLifecycleTS(pending.FrozenAt)
	if err != nil {
		t.Fatalf("parse frozen timestamp: %v", err)
	}
	forgedAt := frozenAt.Add(-31 * 24 * time.Hour)
	pending.Candidate.LastEventTS = ts(forgedAt)
	pending.Candidate.LastEventAge = frozenAt.Sub(forgedAt).Round(time.Second).String()
	pending.Candidate.SweepReason = sweepStaleReason(pending.Candidate, time.Duration(pending.ThresholdNanos))
	pending.Event.Note = pending.Candidate.SweepReason
	pending.FrameSHA256, err = sweepStalePendingDigest(pending)
	if err != nil {
		t.Fatalf("reseal forged candidate: %v", err)
	}
	if err := s.writeJSON(s.sweepStalePendingPath(w.ID), &pending); err != nil {
		t.Fatalf("persist forged candidate: %v", err)
	}

	before := snapshotLifecycleAuthority(t, s)
	for _, apply := range []bool{false, true} {
		var report SweepStaleReport
		if apply {
			report, err = s.SweepStaleApply(72*time.Hour, newFakeProbe(), fixedNow)
		} else {
			report, err = s.SweepStalePlan(72*time.Hour, newFakeProbe(), fixedNow)
		}
		if err != nil {
			t.Fatalf("sweep apply=%v over forged candidate: %v", apply, err)
		}
		if report.Reclaimed != 0 || report.Skipped != 1 || report.Unreadable != 0 || len(report.Candidates) != 1 {
			t.Fatalf("sweep apply=%v report = %+v, want one recovery-required candidate", apply, report)
		}
		candidate := report.Candidates[0]
		if candidate.WorkID != w.ID || candidate.LastEventTS != "" || candidate.LastEventAge != "" {
			t.Fatalf("sweep apply=%v surfaced forged candidate facts: %+v", apply, candidate)
		}
		if !strings.Contains(candidate.ReclaimError, "sweep-stale recovery_required") {
			t.Fatalf("sweep apply=%v candidate error = %q, want recovery_required", apply, candidate.ReclaimError)
		}
	}
	assertLifecycleAuthorityUnchanged(t, before, snapshotLifecycleAuthority(t, s))
}

func resealSweepStaleSemanticMutation(p *sweepStalePending) error {
	var err error
	p.SourceWorkSHA256, err = workEnvelopeSHA256(p.SourceWork)
	if err != nil {
		return err
	}
	p.FinalWorkSHA256, err = workEnvelopeSHA256(p.FinalWork)
	if err != nil {
		return err
	}
	p.Candidate.SourceWorkSHA256 = p.SourceWorkSHA256
	p.Candidate.State = p.SourceWork.State
	p.Candidate.TargetState = p.FinalWork.State
	p.Event.From = p.SourceWork.State
	p.Event.To = p.FinalWork.State
	p.Event.PreviousWorkSHA256 = p.SourceWorkSHA256
	p.Event.WorkSHA256 = p.FinalWorkSHA256
	return nil
}

func freezeSweepStalePendingForTest(t *testing.T, s *Store, actor string) (envelope.WorkEnvelope, sweepStalePending) {
	t.Helper()
	w := staleEnvelope(t, s, "claimed", 30*24*time.Hour, fixedNow)
	if actor != "test-fixture" {
		lease, err := s.readLease(w.ID)
		if err != nil {
			t.Fatalf("read fixture lease: %v", err)
		}
		lease.By = actor
		if err := s.writeJSON(s.leasePath(w.ID), &lease); err != nil {
			t.Fatalf("rewrite fixture lease: %v", err)
		}
	}
	fired := false
	s.sweepStaleFault = func(stage string) error {
		if stage == sweepStaleFaultAfterPendingPersist && !fired {
			fired = true
			return errors.New("freeze before semantic authority mutation")
		}
		return nil
	}
	report, err := s.SweepStaleApply(72*time.Hour, newFakeProbe(), fixedNow)
	s.sweepStaleFault = nil
	if err != nil {
		t.Fatalf("freeze sweep-stale pending: %v", err)
	}
	if !fired || report.Reclaimed != 0 || report.Skipped != 1 {
		t.Fatalf("freeze report = %+v, want one frozen pending frame", report)
	}
	pending, present, err := s.loadSweepStalePending(w.ID)
	if err != nil || !present {
		t.Fatalf("load frozen pending: present=%v err=%v", present, err)
	}
	return w, pending
}

func TestDelegateRefusesFrozenSweepStaleBeforeMint(t *testing.T) {
	originalReadiness := DelegateHarnessReadiness
	originalGuard := LaneCapacityGuard
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	t.Cleanup(func() {
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalGuard
	})

	s := newExperimentalDelegateStore(t)
	const holder = "test-fixture"
	w, frozen := freezeSweepStalePendingForTest(t, s, holder)
	beforeAuthority := snapshotLifecycleAuthority(t, s)
	beforeAttempts := runAttemptsByID(t, s)
	runner := newFakeRunner()
	result, err := s.Delegate(DelegateOptions{
		WorkID:    w.ID,
		Harness:   "claude-zai",
		Actor:     holder,
		Command:   "claude-zai",
		Workspace: t.TempDir(),
		Now:       fixedNow.Add(2 * time.Minute),
	}, runner)
	if err != nil {
		t.Fatalf("delegate over frozen sweep-stale error = %v, want refused authority_changed before mint", err)
	}
	if result.Status != "refused" || result.Refusal == nil || result.Refusal.Reason != "authority_changed" {
		t.Fatalf("delegate over frozen sweep-stale result = %+v, want refused authority_changed", result)
	}
	if !strings.Contains(result.Refusal.Message, "sweep-stale recovery_required") ||
		!strings.Contains(result.Refusal.Message, frozen.FrameSHA256) {
		t.Fatalf("delegate refusal %q does not bind frozen sweep-stale frame %s", result.Refusal.Message, frozen.FrameSHA256)
	}
	if len(runner.creates) != 0 {
		t.Fatalf("delegate over frozen sweep-stale created a session: %+v", runner.creates)
	}
	assertLifecycleAuthorityUnchanged(t, beforeAuthority, snapshotLifecycleAuthority(t, s))
	if afterAttempts := runAttemptsByID(t, s); !reflect.DeepEqual(beforeAttempts, afterAttempts) {
		t.Fatalf("delegate over frozen sweep-stale changed RunAttempts\nbefore=%+v\nafter=%+v", beforeAttempts, afterAttempts)
	}
}

func TestLeaseRepairRefusesFrozenSweepStaleAuthorityWithoutMutation(t *testing.T) {
	s := newStore(t)
	const holder = "test-fixture"
	w, frozen := freezeSweepStalePendingForTest(t, s, holder)
	beforeAuthority := snapshotLifecycleAuthority(t, s)
	beforeAttempts := runAttemptsByID(t, s)

	_, err := s.RepairLease(w.ID, LeaseRepairInput{
		Actor:                 "repair-operator",
		ExpectedHolder:        "actor-b",
		ExpectedCurrentHolder: holder,
		Reason:                "must not overtake the frozen sweep-stale generation",
	}, fixedNow.Add(2*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "sweep-stale recovery_required") {
		t.Fatalf("repair over frozen sweep-stale %s error = %v, want sweep-stale recovery_required", frozen.FrameSHA256, err)
	}
	if !strings.Contains(err.Error(), frozen.FrameSHA256) {
		t.Fatalf("repair fence error %q does not bind frozen sweep-stale frame %s", err, frozen.FrameSHA256)
	}
	assertLifecycleAuthorityUnchanged(t, beforeAuthority, snapshotLifecycleAuthority(t, s))
	if afterAttempts := runAttemptsByID(t, s); !reflect.DeepEqual(beforeAttempts, afterAttempts) {
		t.Fatalf("repair over frozen sweep-stale changed RunAttempts\nbefore=%+v\nafter=%+v", beforeAttempts, afterAttempts)
	}
}
