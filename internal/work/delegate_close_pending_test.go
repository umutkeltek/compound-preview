package work

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
)

type delegateClosePendingSnapshot struct {
	authority map[string][]byte
	attempts  map[string]runattempt.RunAttempt
	creates   []fakeCreateCall
	kills     []string
	live      map[string]bool
	panes     map[string]SessionStatus
}

func snapshotDelegateClosePending(t *testing.T, s *Store, runner *fakeSessionRunner) delegateClosePendingSnapshot {
	t.Helper()
	runner.mu.Lock()
	defer runner.mu.Unlock()

	creates := make([]fakeCreateCall, len(runner.creates))
	for i, call := range runner.creates {
		creates[i] = call
		creates[i].Args = append([]string(nil), call.Args...)
		if call.Environment != nil {
			creates[i].Environment = make(map[string]string, len(call.Environment))
			for key, value := range call.Environment {
				creates[i].Environment[key] = value
			}
		}
	}
	var live map[string]bool
	if runner.live != nil {
		live = make(map[string]bool, len(runner.live))
		for session, value := range runner.live {
			live[session] = value
		}
	}
	var panes map[string]SessionStatus
	if runner.panes != nil {
		panes = make(map[string]SessionStatus, len(runner.panes))
		for session, value := range runner.panes {
			panes[session] = value
		}
	}
	return delegateClosePendingSnapshot{
		authority: snapshotLifecycleAuthority(t, s),
		attempts:  runAttemptsByID(t, s),
		creates:   creates,
		kills:     append([]string(nil), runner.kills...),
		live:      live,
		panes:     panes,
	}
}

func assertDelegateClosePendingUnchanged(t *testing.T, before, after delegateClosePendingSnapshot) {
	t.Helper()
	assertLifecycleAuthorityUnchanged(t, before.authority, after.authority)
	if !reflect.DeepEqual(before.attempts, after.attempts) {
		t.Fatalf("frozen delegate changed RunAttempts\nbefore=%+v\nafter=%+v", before.attempts, after.attempts)
	}
	if !reflect.DeepEqual(before.creates, after.creates) ||
		!reflect.DeepEqual(before.kills, after.kills) ||
		!reflect.DeepEqual(before.live, after.live) ||
		!reflect.DeepEqual(before.panes, after.panes) {
		t.Fatalf(
			"frozen delegate changed runner state\ncreates: %+v -> %+v\nkills: %v -> %v\nlive: %v -> %v\npanes: %+v -> %+v",
			before.creates,
			after.creates,
			before.kills,
			after.kills,
			before.live,
			after.live,
			before.panes,
			after.panes,
		)
	}
}

func installFrozenCloseDelegateDefaults(t *testing.T) {
	t.Helper()
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
}

func requireFrozenCloseAuthorityChanged(t *testing.T, result DelegateResult, err error, frozen closePending) {
	t.Helper()
	if err != nil {
		t.Fatalf("delegate over frozen close error = %v, want structured authority_changed refusal", err)
	}
	if result.Status != "refused" || result.Refusal == nil || result.Refusal.Reason != "authority_changed" {
		t.Fatalf("delegate over frozen close result = %+v, want refused authority_changed", result)
	}
	if !strings.Contains(result.Refusal.Message, "close recovery_required") ||
		!strings.Contains(result.Refusal.Message, frozen.Action.ID) {
		t.Fatalf("delegate refusal %q does not bind frozen close action %s", result.Refusal.Message, frozen.Action.ID)
	}
}

func freezeDelegateClosePendingAfterPrepare(t *testing.T, s *Store, workID string, in CloseInput, at time.Time) closePending {
	t.Helper()
	token := &struct{}{}
	previous := closeAfterTerminalizationPrepare
	closeAfterTerminalizationPrepare = func() { panic(token) }
	t.Cleanup(func() { closeAfterTerminalizationPrepare = previous })
	crashed := false
	var closeErr error
	func() {
		defer func() {
			switch recovered := recover(); recovered {
			case token:
				crashed = true
			case nil:
			default:
				panic(recovered)
			}
		}()
		_, closeErr = s.Close(workID, in, at)
	}()
	closeAfterTerminalizationPrepare = previous
	if !crashed {
		t.Fatalf("close did not reach the post-prepare crash seam: %v", closeErr)
	}
	pending, present, err := s.loadClosePending(workID)
	if err != nil || !present {
		t.Fatalf("load frozen close frame: present=%v err=%v", present, err)
	}
	return pending
}

func TestDelegateMutationPackageGateDefaultsOff(t *testing.T) {
	installFrozenCloseDelegateDefaults(t)
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const holder = "orchestrator:test"
	workID := prepareClaimedWork(t, s, holder, "", "delegated mutation is unsupported by default")
	runner := newFakeRunner()
	before := snapshotDelegateClosePending(t, s, runner)

	delegated, err := s.Delegate(DelegateOptions{
		WorkID: workID, Harness: "claude-zai", Actor: holder,
		Command: "claude-zai", Workspace: t.TempDir(), Now: fixedNow.Add(time.Minute),
	}, runner)
	if err != nil || delegated.Refusal == nil || delegated.Refusal.Reason != "experimental_disabled" {
		t.Fatalf("default delegate gate: result=%+v err=%v", delegated, err)
	}
	if _, err := s.Steer(SteerOptions{WorkID: workID, Text: "do not send", Now: fixedNow.Add(time.Minute)}, runner); !errors.Is(err, ErrExperimentalDelegateMutationsDisabled) {
		t.Fatalf("default steer gate error = %v", err)
	}
	if _, err := s.DelegateSweep(&DelegateBinding{WorkID: workID, Session: "unsupported"}, SessionStatus{Exists: true, PaneDead: true}, runner, holder, fixedNow.Add(time.Minute)); !errors.Is(err, ErrExperimentalDelegateMutationsDisabled) {
		t.Fatalf("default sweep gate error = %v", err)
	}
	if _, err := s.DelegateProgress(workID, runattempt.RunProgress{}, fixedNow.Add(time.Minute)); !errors.Is(err, ErrExperimentalDelegateMutationsDisabled) {
		t.Fatalf("default progress gate error = %v", err)
	}

	dryRun, err := s.Delegate(DelegateOptions{
		WorkID: workID, Harness: "claude-zai", Actor: holder,
		Command: "claude-zai", Workspace: t.TempDir(), DryRun: true, Now: fixedNow.Add(time.Minute),
	}, runner)
	if err != nil || (dryRun.Refusal != nil && dryRun.Refusal.Reason == "experimental_disabled") {
		t.Fatalf("default-off dry-run must remain available: result=%+v err=%v", dryRun, err)
	}
	assertDelegateClosePendingUnchanged(t, before, snapshotDelegateClosePending(t, s, runner))
}

func TestDelegateFrozenCloseFencesPreMintRefusalPublications(t *testing.T) {
	for _, name := range []string{
		"lease_held",
		"gate_absent",
		"capacity_refused",
		"confirmation_required",
		"session_live",
		"tmux_absent",
	} {
		t.Run(name, func(t *testing.T) {
			installFrozenCloseDelegateDefaults(t)
			s := newExperimentalDelegateStore(t)
			const holder = "orchestrator:test"
			workID := prepareClaimedWork(t, s, holder, "", "frozen close fences delegate refusal publication")
			runner := newFakeRunner()
			workspace := t.TempDir()
			actor := holder
			switch name {
			case "lease_held":
				actor = "orchestrator:competitor"
			case "gate_absent":
				LaneCapacityGuard = nil
			case "capacity_refused":
				LaneCapacityGuard = fakeGuard(true, false, "capacity refused", "walled")
			case "confirmation_required":
				LaneCapacityGuard = fakeGuard(false, true, "confirmation required", "expiring")
			case "session_live":
				spawned, err := s.Delegate(DelegateOptions{
					WorkID: workID, Harness: "claude-zai", Actor: holder,
					Command: "claude-zai", Workspace: workspace, Now: fixedNow.Add(30 * time.Second),
				}, runner)
				if err != nil || spawned.Status != "spawned" || spawned.Binding == nil {
					t.Fatalf("seed live delegate: result=%+v err=%v", spawned, err)
				}
				// No process runs in this fake session. Keep its live session
				// entry to exercise the refusal fence, but finish the launch
				// record before freezing terminal authority.
				if _, err := s.runAttempts().Complete(spawned.Binding.RunAttemptID, runattempt.StatusAbandoned, fixedNow.Add(45*time.Second)); err != nil {
					t.Fatal(err)
				}
			case "tmux_absent":
				runner.availErr = errors.New("tmux unavailable")
			default:
				t.Fatalf("unknown case %q", name)
			}
			frozen := freezeClosePendingAfterPrepare(t, s, workID, CloseInput{
				Outcome: "cancelled", MoveType: "audit_only", Actor: holder,
				Summary: "freeze close before delegate refusal",
			}, fixedNow.Add(time.Minute))
			before := snapshotDelegateClosePending(t, s, runner)
			result, err := s.Delegate(DelegateOptions{
				WorkID: workID, Harness: "claude-zai", Actor: actor,
				Command: "claude-zai", Workspace: workspace, Now: fixedNow.Add(2 * time.Minute),
			}, runner)
			requireFrozenCloseAuthorityChanged(t, result, err, frozen)
			assertDelegateClosePendingUnchanged(t, before, snapshotDelegateClosePending(t, s, runner))
		})
	}
}

func TestDelegateFrozenCloseFencesPreMintSessionCleanup(t *testing.T) {
	for _, name := range []string{"dead_binding_sweep", "recorded_exit_pane_reap"} {
		t.Run(name, func(t *testing.T) {
			installFrozenCloseDelegateDefaults(t)
			s := newExperimentalDelegateStore(t)
			const holder = "orchestrator:test"
			workID := prepareClaimedWork(t, s, holder, "", "frozen close fences delegate cleanup")
			runner := newFakeRunner()
			workspace := t.TempDir()
			spawned, err := s.Delegate(DelegateOptions{
				WorkID: workID, Harness: "claude-zai", Actor: holder,
				Command: "claude-zai", Workspace: workspace, Now: fixedNow.Add(10 * time.Second),
			}, runner)
			if err != nil || spawned.Status != "spawned" || spawned.Binding == nil {
				t.Fatalf("seed delegate: result=%+v err=%v", spawned, err)
			}
			dead := SessionStatus{Exists: true, PaneDead: true, ExitCode: 0}
			runner.panes = map[string]SessionStatus{spawned.Binding.Session: dead}
			closeOutcome := "completed"
			switch name {
			case "dead_binding_sweep":
				if _, err := runattempt.Open(s.home).Complete(
					spawned.Binding.RunAttemptID,
					runattempt.StatusSucceeded,
					fixedNow.Add(20*time.Second),
				); err != nil {
					t.Fatalf("terminalize seeded delegate as succeeded: %v", err)
				}
				if _, err := s.Advance(workID, AdvanceInput{
					To:    "running",
					Actor: holder,
				}, fixedNow.Add(25*time.Second)); err != nil {
					t.Fatalf("advance seeded delegate work to running: %v", err)
				}
				if _, err := s.Advance(workID, AdvanceInput{
					To:             "verifying",
					Actor:          holder,
					VerificationID: "check:seeded-delegate-succeeded",
				}, fixedNow.Add(30*time.Second)); err != nil {
					t.Fatalf("advance seeded delegate work to verifying: %v", err)
				}
			case "recorded_exit_pane_reap":
				dead.ExitCode = 1
				runner.panes[spawned.Binding.Session] = dead
				runner.killErr = errors.New("injected pane reap failure")
				seeded, err := s.DelegateSweep(
					spawned.Binding,
					dead,
					runner,
					holder,
					fixedNow.Add(20*time.Second),
				)
				if err == nil || !strings.Contains(err.Error(), "injected pane reap failure") {
					t.Fatalf("seed real recorded exit with retained pane: %v", err)
				}
				terminalized, err := runattempt.Open(s.home).Get(spawned.Binding.RunAttemptID)
				if err != nil {
					t.Fatalf("load seeded delegate attempt: %v", err)
				}
				if terminalized.Status != runattempt.StatusFailed {
					t.Fatalf("seeded attempt status = %q, want %q; outcome=%+v", terminalized.Status, runattempt.StatusFailed, seeded)
				}
				runner.killErr = nil
				closeOutcome = "failed"
				if _, err := s.Advance(workID, AdvanceInput{
					To:    "running",
					Actor: holder,
				}, fixedNow.Add(30*time.Second)); err != nil {
					t.Fatalf("advance failed delegate work to running: %v", err)
				}
			default:
				t.Fatalf("unknown case %q", name)
			}
			frozen := freezeDelegateClosePendingAfterPrepare(t, s, workID, CloseInput{
				Outcome: closeOutcome, MoveType: "audit_only", Actor: holder,
				Summary: "freeze close before delegate cleanup",
			}, fixedNow.Add(time.Minute))
			before := snapshotDelegateClosePending(t, s, runner)
			result, err := s.Delegate(DelegateOptions{
				WorkID: workID, Harness: "claude-zai", Actor: holder,
				Command: "claude-zai", Workspace: workspace, Now: fixedNow.Add(2 * time.Minute),
			}, runner)
			requireFrozenCloseAuthorityChanged(t, result, err, frozen)
			assertDelegateClosePendingUnchanged(t, before, snapshotDelegateClosePending(t, s, runner))
		})
	}
}

func TestDelegateRefusalPublicationRechecksConcurrentFrozenClose(t *testing.T) {
	installFrozenCloseDelegateDefaults(t)
	s := newExperimentalDelegateStore(t)
	const holder = "orchestrator:test"
	workID := prepareClaimedWork(t, s, holder, "", "delegate refusal rechecks concurrent frozen close")
	runner := newFakeRunner()
	var frozen closePending
	var before *delegateClosePendingSnapshot
	LaneCapacityGuard = func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		frozen = freezeClosePendingAfterPrepare(t, s, workID, CloseInput{
			Outcome: "cancelled", MoveType: "audit_only", Actor: holder,
			Summary: "freeze close inside delegate capacity guard",
		}, fixedNow.Add(time.Minute))
		snap := snapshotDelegateClosePending(t, s, runner)
		before = &snap
		return LaneCapacityGuardResult{
			Capacity: DispatchLaneCapacity{State: "walled"},
			Refused:  true, Message: "capacity refused after close froze",
		}, nil
	}
	result, err := s.Delegate(DelegateOptions{
		WorkID: workID, Harness: "claude-zai", Actor: holder,
		Command: "claude-zai", Workspace: t.TempDir(), Now: fixedNow.Add(2 * time.Minute),
	}, runner)
	if before == nil {
		t.Fatal("capacity guard did not freeze close")
	}
	requireFrozenCloseAuthorityChanged(t, result, err, frozen)
	assertDelegateClosePendingUnchanged(t, *before, snapshotDelegateClosePending(t, s, runner))
}

func TestDelegateFrozenCloseDryRunRemainsReadOnly(t *testing.T) {
	installFrozenCloseDelegateDefaults(t)
	s := newExperimentalDelegateStore(t)
	const holder = "orchestrator:test"
	workID := prepareClaimedWork(t, s, holder, "", "frozen close keeps delegate dry-run read-only")
	runner := newFakeRunner()
	_ = freezeClosePendingAfterPrepare(t, s, workID, CloseInput{
		Outcome: "cancelled", MoveType: "audit_only", Actor: holder,
		Summary: "freeze close before delegate dry-run",
	}, fixedNow.Add(time.Minute))
	before := snapshotDelegateClosePending(t, s, runner)
	LaneCapacityGuard = fakeGuard(true, false, "capacity refused", "walled")
	result, err := s.Delegate(DelegateOptions{
		WorkID: workID, Harness: "claude-zai", Actor: holder,
		Command: "claude-zai", Workspace: t.TempDir(), DryRun: true,
		Now: fixedNow.Add(2 * time.Minute),
	}, runner)
	if err != nil {
		t.Fatalf("dry-run over frozen close: %v", err)
	}
	if result.Status != "preview" {
		t.Fatalf("dry-run status = %q, want preview: %+v", result.Status, result)
	}
	assertDelegateClosePendingUnchanged(t, before, snapshotDelegateClosePending(t, s, runner))
}
