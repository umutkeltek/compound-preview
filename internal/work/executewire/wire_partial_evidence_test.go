package executewire

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"compound/internal/harness"
	"compound/internal/runattempt"
	"compound/internal/work"
)

// Partial-evidence preservation on a FAILED adapter run.
//
// A harness adapter that crashes mid-run still mints a RunAttempt, records a
// workspace and a command, and can leave a transcript behind. It then returns
// BOTH that output and an error. The launch wire discarded the output and
// returned the bare error, so the minted RunAttempt — the only durable record
// that the launch happened at all — was never bound to the WorkEnvelope. The
// envelope was closed "failed" by the seam with a synthetic proof ref, so the
// system reported a failure whose evidence it had thrown away.

// partialFailureAdapter mints a real RunAttempt, attaches a transcript, marks
// it failed, and returns the output ALONGSIDE an error — the exact shape a
// crashed subprocess produces.
type partialFailureAdapter struct {
	name       string
	home       string
	mintedID   string
	transcript string
	calls      int
}

func (a *partialFailureAdapter) Name() string { return a.name }

func (a *partialFailureAdapter) Run(ctx context.Context, in harness.RunInput) (harness.RunOutput, error) {
	a.calls++
	now := time.Now().UTC()
	raStore := runattempt.Open(a.home)
	ra, err := raStore.MintWithActionIntent(in.WorkID, "compound", a.name, in.ActionIntentID, now)
	if err != nil {
		return harness.RunOutput{}, fmt.Errorf("partial-failure adapter mint: %w", err)
	}
	if _, err := raStore.SetOrigin(ra.ID, runattempt.OriginHarnessExecution); err != nil {
		return harness.RunOutput{}, fmt.Errorf("partial-failure adapter SetOrigin: %w", err)
	}
	if _, err := raStore.SetWorkspace(ra.ID, in.Workspace); err != nil {
		return harness.RunOutput{}, fmt.Errorf("partial-failure adapter SetWorkspace: %w", err)
	}
	if _, err := raStore.SetCommand(ra.ID, in.Command); err != nil {
		return harness.RunOutput{}, fmt.Errorf("partial-failure adapter SetCommand: %w", err)
	}
	if _, err := raStore.SetTranscript(ra.ID, a.transcript); err != nil {
		return harness.RunOutput{}, fmt.Errorf("partial-failure adapter SetTranscript: %w", err)
	}
	terminal, err := raStore.Complete(ra.ID, runattempt.StatusFailed, now)
	if err != nil {
		return harness.RunOutput{}, fmt.Errorf("partial-failure adapter Complete: %w", err)
	}
	a.mintedID = terminal.ID
	// The crash shape: real partial output AND an error.
	return harness.RunOutput{RunAttempt: terminal, TranscriptPath: a.transcript},
		errors.New("harness subprocess exited 137 (killed)")
}

func installPartialFailureAdapter(t *testing.T, homePath, harnessName string) *partialFailureAdapter {
	t.Helper()
	adapter := &partialFailureAdapter{name: harnessName, home: homePath, transcript: "transcript://partial-evidence"}
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
	return adapter
}

// TestLaunchViaActuatorNamesPartialEvidenceOnAdapterError — a failed launch
// must not be anonymous. The wire returns the id of the RunAttempt the
// adapter minted, so the seam can name the record that holds the workspace,
// command and transcript of the attempt that crashed.
//
// KNOWN GAP, deliberately not asserted here: the attempt is named but NOT
// bound to the WorkEnvelope. Close resolves an attached attempt's authority
// from PRIOR events and only Advance attaches attempts, so an attempt minted
// during this launch has no attaching event and finalization refuses with
// "authority generation is missing (pairs=0)". Binding it requires letting a
// close derive authority from the attempt's own immutable bindings, which
// loosens a forged-authority gate — an operator decision, not a wire fix.
func TestLaunchViaActuatorNamesPartialEvidenceOnAdapterError(t *testing.T) {
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
	const workID = "work_partial_evidence"
	seedExecuteStageReadyWorkWithDelivery(t, store, workID, homePath)
	adapter := installPartialFailureAdapter(t, homePath, "claude_minimax")
	defect4LaunchSeed(t, store, workID, "actor:partial", now)

	rattID, launchErr := LaunchViaActuator(store, workID, "claude_minimax", "claude_minimax", "partial evidence", "actor:partial", now)
	if launchErr == nil {
		t.Fatalf("LaunchViaActuator returned nil error; the adapter failed and the caller must learn about it")
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
	if adapter.mintedID == "" {
		t.Fatalf("fixture did not mint a RunAttempt; the assertion below would be vacuous")
	}
	if rattID != adapter.mintedID {
		t.Fatalf("wire returned run attempt id %q, want the minted %q: dropping the adapter's output leaves the crash evidence anonymous",
			rattID, adapter.mintedID)
	}
}

// TestFinalizeBindsAnAttemptMintedDuringTheLaunch pins the closed-side
// outcome the slice-2 replay lands. The pre-replay wire only NAMED the
// minted ratt id (the naming workaround in execute_stage.go's failure
// close) because the close-time authority gate had no event to bind the
// attempt from. After the replay MintLaunchAuthority seals a same-state
// event attaching the (ActionIntent, PolicyDecision) pair, so the gate
// can resolve the attempt minted during the launch and FinalizeGovernedHarnessRun
// closes the envelope as failed with the attempt on the chain. Inverted
// from TestFinalizeRefusesToBindAnAttemptMintedDuringTheLaunch so a future
// regression that un-binds the partial evidence surfaces RED here.
func TestFinalizeBindsAnAttemptMintedDuringTheLaunch(t *testing.T) {
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
	const workID = "work_partial_evidence_bound"
	seedExecuteStageReadyWorkWithDelivery(t, store, workID, homePath)
	adapter := installPartialFailureAdapter(t, homePath, "claude_minimax")
	defect4LaunchSeed(t, store, workID, "actor:bound", now)

	if _, launchErr := LaunchViaActuator(store, workID, "claude_minimax", "claude_minimax", "bound", "actor:bound", now); launchErr == nil {
		t.Fatalf("expected the adapter failure to surface")
	}
	after, err := store.Get(workID)
	if err != nil {
		t.Fatalf("get work: %v", err)
	}
	if after.State != "failed" {
		t.Fatalf("envelope state = %q, want failed (the canonical finalize must terminalize on a non-success RunAttempt after the slice-2 fix)", after.State)
	}
	bound := false
	for _, id := range after.RunAttemptIDs {
		if id == adapter.mintedID {
			bound = true
			break
		}
	}
	if !bound {
		t.Fatalf("envelope RunAttemptIDs = %v does NOT contain the minted failed attempt %s; the slice-2 fix binds the partial-evidence id, not just names it", after.RunAttemptIDs, adapter.mintedID)
	}
}

// TestLaunchContextDefaultsToACancellableContext — the defect was that
// nothing in production ever assigned LaunchContext, so every unattended
// launch ran under context.Background() and could not be interrupted. The
// default must therefore be cancellable ON ITS OWN, with no caller
// cooperation, and InstallLaunchContext must still be able to swap it and
// put it back so one test's context cannot leak into the next.
func TestLaunchContextDefaultsToACancellableContext(t *testing.T) {
	if LaunchContext().Done() == nil {
		t.Fatalf("the DEFAULT launch context is uncancellable — production launches cannot be interrupted, and the heartbeat's SIGTERM will orphan the subprocess tree")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	restore := InstallLaunchContext(ctx)
	if LaunchContext().Done() == nil {
		t.Fatalf("InstallLaunchContext did not replace the launch context; launches stay uncancellable")
	}
	cancel()
	select {
	case <-LaunchContext().Done():
	default:
		t.Fatalf("installed context does not observe cancellation; the runner's process-group kill would never fire")
	}
	restore()
	if LaunchContext().Done() == nil {
		t.Fatalf("restore() did not put the signal-derived default back")
	}
	if LaunchContext() == ctx {
		t.Fatalf("restore() leaked the installed context into the process default")
	}
}

// TestInstalledLaunchContextReachesTheAdapter — end to end through the real
// wire: the context a command installs is the one adapter.Run receives.
func TestInstalledLaunchContextReachesTheAdapter(t *testing.T) {
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
	const workID = "work_installed_ctx"
	seedExecuteStageReadyWorkWithDelivery(t, store, workID, homePath)

	rec := newTimeoutCtxRecorder(context.Background())
	recording := installTimeoutRecordingAdapter(t, homePath, "claude_minimax", rec)
	// installTimeoutRecordingAdapter swaps LaunchContext directly; go through
	// the production door instead so this test exercises what the CLI calls.
	t.Cleanup(InstallLaunchContext(rec))
	defect4LaunchSeed(t, store, workID, "actor:installed-ctx", now)

	if _, err := LaunchViaActuator(store, workID, "claude_minimax", "claude_minimax", "installed ctx", "actor:installed-ctx", now); err != nil {
		t.Fatalf("LaunchViaActuator: %v", err)
	}
	if !recording.ctxWasRecCtx {
		t.Fatalf("adapter.Run did not receive the installed context; InstallLaunchContext is not the door the wire reads")
	}
}
