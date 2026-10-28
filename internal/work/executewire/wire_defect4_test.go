// DEFECT 4 (AUT-P0-004) — the unattended launch MUST enforce a finite
// execution budget, propagate the caller's context (instead of
// context.Background), terminate the whole process group rather than
// just the direct child, and persist timeout/cancellation evidence on
// the RunAttempt.
//
// The pre-fix wire built harness.RunInput with no Timeout and called
// adapter.Run(context.Background(), runInput); the shared subprocess
// runner only arms its kill timer when `RunInput.Timeout > 0`, so an
// unattended launch could block forever while the heartbeat's outer
// `timeout 600` killed the Compound parent with the subprocess process
// group still alive.
//
// These tests pin the wire-side invariant: a finite default timeout,
// and the live context forwarded end-to-end. The subprocess runner's
// own ctx-honor goroutine + evidence persistence is pinned by sibling
// tests in internal/harness/subprocess_runner_test.go
// (TestRunnerCtxCancelKillsProcessGroup, TestRunnerTimeoutPersistsEvidence).

package executewire

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"compound/internal/harness"
	"compound/internal/runattempt"
	"compound/internal/work"
)

// timeoutCtxRecorder is a context.Context wrapper that exposes a
// Done() channel a test can close so the wire-side invariant "the
// LaunchContext-derived ctx reaches adapter.Run" is observable from
// the adapter's recorded `lastCtx`.
type timeoutCtxRecorder struct {
	base    context.Context
	doneCh  chan struct{}
	doneHit atomic.Bool
}

func newTimeoutCtxRecorder(base context.Context) *timeoutCtxRecorder {
	return &timeoutCtxRecorder{base: base, doneCh: make(chan struct{})}
}

func (r *timeoutCtxRecorder) Done() <-chan struct{} {
	return r.doneCh
}

func (r *timeoutCtxRecorder) Err() error {
	select {
	case <-r.doneCh:
		return errors.New("recorder cancelled")
	default:
		return nil
	}
}

func (r *timeoutCtxRecorder) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

func (r *timeoutCtxRecorder) Value(key any) any { return r.base.Value(key) }

// timeoutRecordingAdapter is a stub harness.Adapter that records the
// RunInput and the context it was called with so the wire-side
// invariants (finite Timeout set; context came from LaunchContext, not
// context.Background) are observable in tests.
type timeoutRecordingAdapter struct {
	name         string
	home         string
	lastIn       harness.RunInput
	lastCtx      context.Context
	ctxWasRecCtx bool
	calls        int
}

func (a *timeoutRecordingAdapter) Name() string { return a.name }

func (a *timeoutRecordingAdapter) Run(ctx context.Context, in harness.RunInput) (harness.RunOutput, error) {
	a.calls++
	a.lastIn = in
	a.lastCtx = ctx
	if _, ok := ctx.(*timeoutCtxRecorder); ok {
		a.ctxWasRecCtx = true
	}
	now := time.Now().UTC()
	raStore := runattempt.Open(a.home)
	ra, err := raStore.MintWithActionIntent(in.WorkID, "compound", a.name, in.ActionIntentID, now)
	if err != nil {
		return harness.RunOutput{}, fmt.Errorf("timeout-recording adapter mint: %w", err)
	}
	if _, err := raStore.SetOrigin(ra.ID, runattempt.OriginHarnessExecution); err != nil {
		return harness.RunOutput{}, fmt.Errorf("timeout-recording adapter SetOrigin: %w", err)
	}
	if _, err := raStore.SetWorkspace(ra.ID, in.Workspace); err != nil {
		return harness.RunOutput{}, fmt.Errorf("timeout-recording adapter SetWorkspace: %w", err)
	}
	if _, err := raStore.SetCommand(ra.ID, in.Command); err != nil {
		return harness.RunOutput{}, fmt.Errorf("timeout-recording adapter SetCommand: %w", err)
	}
	terminal, err := raStore.Complete(ra.ID, runattempt.StatusSucceeded, now)
	if err != nil {
		return harness.RunOutput{}, fmt.Errorf("timeout-recording adapter Complete: %w", err)
	}
	return harness.RunOutput{RunAttempt: terminal}, nil
}

// installTimeoutRecordingAdapter swaps AdapterFor + LaunchContext so
// the wire's propagation contract can be observed end-to-end.
func installTimeoutRecordingAdapter(t *testing.T, homePath, harnessName string, rec *timeoutCtxRecorder) *timeoutRecordingAdapter {
	t.Helper()
	adapter := &timeoutRecordingAdapter{name: harnessName, home: homePath}
	prevAdapter := AdapterFor
	AdapterFor = func(home, name string) (harness.Adapter, bool) {
		if name == harnessName {
			return adapter, true
		}
		return nil, false
	}
	t.Cleanup(func() { AdapterFor = prevAdapter })
	prevReadiness := LaunchReadiness
	LaunchReadiness = func(req work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
		return work.DelegateHarnessReadinessResult{Ready: true}, nil
	}
	t.Cleanup(func() { LaunchReadiness = prevReadiness })
	prevCtx := LaunchContext
	LaunchContext = func() context.Context { return rec }
	t.Cleanup(func() { LaunchContext = prevCtx })
	// Slice 2 (aut-launch-canonical 2026-09-07): the LaunchContext
	// precondition gate requires the install flag too. The test owns
	// the wire it installs.
	prevInstalled := launchContextInstalled.Load()
	SetLaunchContextInstalled(true)
	t.Cleanup(func() { SetLaunchContextInstalled(prevInstalled) })
	return adapter
}

func defect4LaunchSeed(t *testing.T, store *work.Store, workID, actor string, now time.Time) {
	t.Helper()
	if _, err := store.Claim(workID, actor, now); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := store.Advance(workID, work.AdvanceInput{To: "running", Actor: actor, Note: "defect4 wire setup"}, now); err != nil {
		t.Fatalf("advance to running: %v", err)
	}
}

// TestLaunchViaActuatorSetsFiniteTimeoutAndClampsOverride (DEFECT 4
// RED -> GREEN):
//
// The wire MUST set a finite RunInput.Timeout so the subprocess
// runner's `if in.Timeout > 0` kill-timer branch arms. With the
// DefaultLaunchTimeout (5 minutes) the wire populates Timeout > 0 and
// the synchronous seam never requests more, so the deadline stays under
// the heartbeat's outer `timeout 600` plus margin. (Explicit CLI requests
// resolve through LaunchTimeoutFor; see launch_timeout_test.go.)
//
// RED before fix: RunInput.Timeout == 0 (no default applied) — the
// runner's kill goroutine never starts, so an unattended launch can
// run forever until the heartbeat's outer `timeout 600` SIGKILLs the
// Compound parent with the subprocess still alive.
func TestLaunchViaActuatorSetsFiniteTimeoutAndClampsOverride(t *testing.T) {
	homePath := initTestHome(t)
	now := time.Now().UTC()
	seedExecuteStageManifest(t, homePath, "capable")
	seedExecuteStageGrant(t, homePath)
	seedExecuteStageCapacityRow(t, homePath, 30, 2*time.Minute, now)
	capacityTestConfig(t, homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	seedExecuteStageReadyWorkWithDelivery(t, store, "work_defect4_default_timeout", homePath)

	rec := newTimeoutCtxRecorder(context.Background())
	recording := installTimeoutRecordingAdapter(t, homePath, "claude_minimax", rec)
	defect4LaunchSeed(t, store, "work_defect4_default_timeout", "actor:defect4", now)

	if _, err := LaunchViaActuator(store, "work_defect4_default_timeout", "claude_minimax", "claude_minimax", "defect4 default", "actor:defect4", now); err != nil {
		t.Fatalf("LaunchViaActuator: %v", err)
	}
	if recording.lastIn.Timeout == 0 {
		t.Fatalf("RunInput.Timeout = 0 (DEFECT 4 RED state); the runner's kill timer is dormant")
	}
	if recording.lastIn.Timeout != DefaultLaunchTimeout {
		t.Fatalf("RunInput.Timeout = %v, want DefaultLaunchTimeout (%v)", recording.lastIn.Timeout, DefaultLaunchTimeout)
	}
	if recording.lastIn.Timeout >= 10*time.Minute {
		t.Fatalf("RunInput.Timeout = %v must stay safely below the heartbeat's outer `timeout 600` (10m)", recording.lastIn.Timeout)
	}
}

// TestLaunchViaActuatorPropagatesCallerContext (DEFECT 4 RED ->
// GREEN):
//
// The wire MUST forward the caller's context (LaunchContext) to
// adapter.Run, NOT context.Background(). The shared subprocess
// runner's ctx-cancel goroutine watches Done() and triggers a
// process-group kill, persisting `cancel:context_cancelled`
// evidence on the RunAttempt.
//
// RED before fix: adapter.Run was called with context.Background(),
// so the runner's ctx-cancel goroutine never observed a cancellation
// and the subprocess tree kept running.
func TestLaunchViaActuatorPropagatesCallerContext(t *testing.T) {
	homePath := initTestHome(t)
	now := time.Now().UTC()
	seedExecuteStageManifest(t, homePath, "capable")
	seedExecuteStageGrant(t, homePath)
	seedExecuteStageCapacityRow(t, homePath, 30, 2*time.Minute, now)
	capacityTestConfig(t, homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	seedExecuteStageReadyWorkWithDelivery(t, store, "work_defect4_ctx_propagated", homePath)

	rec := newTimeoutCtxRecorder(context.Background())
	recording := installTimeoutRecordingAdapter(t, homePath, "claude_minimax", rec)
	defect4LaunchSeed(t, store, "work_defect4_ctx_propagated", "actor:defect4-ctx", now)

	if _, err := LaunchViaActuator(store, "work_defect4_ctx_propagated", "claude_minimax", "claude_minimax", "defect4 ctx", "actor:defect4-ctx", now); err != nil {
		t.Fatalf("LaunchViaActuator: %v", err)
	}
	if recording.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", recording.calls)
	}
	if !recording.ctxWasRecCtx {
		t.Fatalf("adapter.Run was NOT called with the LaunchContext-derived context; the wire discarded caller cancellation (DEFECT 4 RED state)")
	}
}
