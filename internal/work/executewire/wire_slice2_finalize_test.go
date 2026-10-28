package executewire

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"compound/internal/harness"
	"compound/internal/runattempt"
	"compound/internal/work"
)

// Slice 2 (aut-launch-canonical 2026-09-07): the error-path falsifiers the
// audit named — caller cancellation, process failure, and late rotation
// caught by the reservation StartGuard — plus the posture contract for
// Claude-family adapters. Each drives LaunchViaActuator end to end through
// an adapter that behaves like the shared subprocess runner (mints and
// terminalizes a real RunAttempt, honours in.StartGuard around the start,
// persists the execution posture, fails closed when a stamp is refused)
// and asserts the envelope's terminal state and authority chain on disk.
// Before slice 2 the wire returned the raw adapter error without
// finalizing, leaving the envelope `running` with an unrecorded attempt,
// built RunInput without a StartGuard so a rotated reservation still
// launched, and minted launch authority that no close could resolve.

// slice2RunnerAdapter models the shared runner's contract.
type slice2RunnerAdapter struct {
	name        string
	home        string
	processErr  error
	beforeGuard func()
	calls       int
	sawGuard    bool
	started     bool
	attemptID   string
}

func (a *slice2RunnerAdapter) Name() string { return a.name }

func (a *slice2RunnerAdapter) Run(ctx context.Context, in harness.RunInput) (harness.RunOutput, error) {
	a.calls++
	now := time.Now().UTC()
	raStore := runattempt.Open(a.home)
	ra, err := raStore.MintWithActionIntent(in.WorkID, "compound", a.name, in.ActionIntentID, now)
	if err != nil {
		return harness.RunOutput{}, fmt.Errorf("slice2 runner: mint: %w", err)
	}
	a.attemptID = ra.ID
	// failTerm mirrors the runner: a stamp failure after the mint is an
	// infrastructure failure that terminalizes the attempt and returns it
	// with the error — never a pending attempt.
	finish := func(status string, runErr error) (harness.RunOutput, error) {
		terminal, err := raStore.Complete(ra.ID, status, time.Now().UTC())
		if err != nil {
			return harness.RunOutput{}, fmt.Errorf("slice2 runner: complete: %w", err)
		}
		return harness.RunOutput{RunAttempt: terminal}, runErr
	}
	if _, err := raStore.SetOrigin(ra.ID, runattempt.OriginHarnessExecution); err != nil {
		return finish(runattempt.StatusFailed, fmt.Errorf("slice2 runner: set origin: %w", err))
	}
	if _, err := raStore.SetWorkspace(ra.ID, in.Workspace); err != nil {
		return finish(runattempt.StatusFailed, fmt.Errorf("slice2 runner: set workspace: %w", err))
	}
	if _, err := raStore.SetCommand(ra.ID, in.Command); err != nil {
		return finish(runattempt.StatusFailed, fmt.Errorf("slice2 runner: set command: %w", err))
	}
	if in.ExecutionPosture != nil {
		if _, err := raStore.SetExecutionPosture(ra.ID, *in.ExecutionPosture); err != nil {
			return finish(runattempt.StatusFailed, fmt.Errorf("slice2 runner: set execution posture: %w", err))
		}
	}
	if a.beforeGuard != nil {
		a.beforeGuard()
	}
	if in.StartGuard != nil {
		a.sawGuard = true
		if err := in.StartGuard(ctx, func() error { a.started = true; return nil }); err != nil {
			return finish(runattempt.StatusFailed, err)
		}
	} else {
		a.started = true
	}
	if err := ctx.Err(); err != nil {
		return finish(runattempt.StatusFailed, err)
	}
	if a.processErr != nil {
		return finish(runattempt.StatusFailed, a.processErr)
	}
	return finish(runattempt.StatusSucceeded, nil)
}

// slice2GovernedRunnerAdapter is the Claude-family shape: it implements
// harness.GovernedPermissionAdapter, so the wire stamps an execution
// posture onto its RunInput exactly as PrepareGovernedHarnessLaunch does.
type slice2GovernedRunnerAdapter struct {
	*slice2RunnerAdapter
}

func (a *slice2GovernedRunnerAdapter) GovernedPermissionPosture() {}

func (a *slice2GovernedRunnerAdapter) RevalidateGovernedRun(_ context.Context, in harness.RunInput) (harness.RunInput, error) {
	return in, nil
}

func installSlice2RunnerAdapter(t *testing.T, adapter harness.Adapter, base *slice2RunnerAdapter, evidence []work.DelegateHarnessReadinessEvidence) {
	t.Helper()
	prevAdapter := AdapterFor
	AdapterFor = func(home, name string) (harness.Adapter, bool) {
		if name == base.name {
			base.home = home
			return adapter, true
		}
		return nil, false
	}
	t.Cleanup(func() { AdapterFor = prevAdapter })
	prevReadiness := LaunchReadiness
	LaunchReadiness = func(_ work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
		return work.DelegateHarnessReadinessResult{Ready: true, Evidence: append([]work.DelegateHarnessReadinessEvidence(nil), evidence...)}, nil
	}
	t.Cleanup(func() { LaunchReadiness = prevReadiness })
	prevInstalled := launchContextInstalled.Load()
	SetLaunchContextInstalled(true)
	t.Cleanup(func() { SetLaunchContextInstalled(prevInstalled) })
}

// slice2AttestedEvidence is the full readiness evidence set a Claude lane
// attests; the runner refuses a mutation-capable posture without it.
func slice2AttestedEvidence() []work.DelegateHarnessReadinessEvidence {
	return []work.DelegateHarnessReadinessEvidence{
		{Kind: "permission_authority", Ref: "grant:slice2", Revision: "r1"},
		{Kind: "permission_posture", Ref: "posture:slice2", Revision: "r1"},
		{Kind: "blocking_hook_plan", Ref: "hookplan:slice2", Revision: "r1"},
		{Kind: "blocking_hook_installation", Ref: "hookinstall:slice2", Revision: "r1"},
	}
}

// slice2RunningEnvelope seeds a home with a capable lane, grant, capacity,
// and a claimed work envelope advanced to running — the state the
// unattended execute seam launches from.
func slice2RunningEnvelope(t *testing.T, workID, actor string) (string, *work.Store, time.Time) {
	t.Helper()
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
	seedExecuteStageReadyWorkWithDelivery(t, store, workID, homePath)
	if _, err := store.Claim(workID, actor, now); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := store.Advance(workID, work.AdvanceInput{To: "running", Actor: actor, Note: "slice2 fixture"}, now); err != nil {
		t.Fatalf("advance to running: %v", err)
	}
	return homePath, store, now
}

// TestLaunchViaActuatorProcessFailureFinalizesFailed: a non-Claude adapter
// returns a FAILED RunAttempt plus a process error. The wire must still
// run the canonical finalize path — the envelope closes as failed with
// the new attempt on its chain and the freshly minted authority pair on
// disk — and surface the adapter error verbatim. The attempt carries only
// its ActionIntent, so the close-time gate can bind it only through the
// sealed event MintLaunchAuthority records.
func TestLaunchViaActuatorProcessFailureFinalizesFailed(t *testing.T) {
	const workID = "work_slice2_process_failure"
	const actor = "actor:slice2-process-failure"
	homePath, store, now := slice2RunningEnvelope(t, workID, actor)
	pre, err := store.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	crash := errors.New("subprocess crashed")
	rec := &slice2RunnerAdapter{name: "claude_minimax", processErr: crash}
	installSlice2RunnerAdapter(t, rec, rec, nil)

	_, err = LaunchViaActuator(store, workID, "claude_minimax", "claude_minimax", "slice2 process failure", actor, now)
	if !errors.Is(err, crash) {
		t.Fatalf("err = %v; want the adapter's process error surfaced verbatim", err)
	}
	if rec.calls != 1 || !rec.sawGuard || !rec.started {
		t.Fatalf("adapter calls=%d sawGuard=%v started=%v; want one guarded start", rec.calls, rec.sawGuard, rec.started)
	}
	after, err := store.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "failed" {
		t.Fatalf("envelope state = %q; a process failure must finalize the envelope as failed, not leave it running", after.State)
	}
	if rec.attemptID == "" || !slices.Contains(after.RunAttemptIDs, rec.attemptID) {
		t.Fatalf("RunAttemptIDs = %v; the failed attempt %q must join the chain through finalize", after.RunAttemptIDs, rec.attemptID)
	}
	if after.PolicyDecisionID == "" || after.PolicyDecisionID == pre.PolicyDecisionID {
		t.Fatalf("PolicyDecisionID pre=%q post=%q; the launch authority must be minted and persisted", pre.PolicyDecisionID, after.PolicyDecisionID)
	}
	// The launch appended its own ActionIntent (the failed close then mints
	// another pair, so the chain grows by two); the attempt must be bound to
	// the launch one, which did not exist before the launch.
	att, err := runattempt.Open(homePath).Get(rec.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if att.ActionIntentID == "" || slices.Contains(pre.ActionIntentIDs, att.ActionIntentID) || !slices.Contains(after.ActionIntentIDs, att.ActionIntentID) {
		t.Fatalf("attempt ActionIntentID = %q; want the ActionIntent the launch appended (pre=%v post=%v)", att.ActionIntentID, pre.ActionIntentIDs, after.ActionIntentIDs)
	}
	if att.ExecutionPosture != nil {
		t.Fatalf("non-Claude attempt carries a posture %+v; the wire must stamp posture only for GovernedPermissionAdapter lanes", att.ExecutionPosture)
	}
}

// TestLaunchViaActuatorCancelledContextFinalizesFailed: the installed
// LaunchContext is already cancelled. The guard refuses the start on
// ctx.Err, the adapter returns the cancellation, and the wire finalizes
// the envelope as failed with the attempt recorded — instead of
// returning the raw error and leaving a running reservation behind.
func TestLaunchViaActuatorCancelledContextFinalizesFailed(t *testing.T) {
	const workID = "work_slice2_cancelled"
	const actor = "actor:slice2-cancelled"
	_, store, now := slice2RunningEnvelope(t, workID, actor)
	rec := &slice2RunnerAdapter{name: "claude_minimax"}
	installSlice2RunnerAdapter(t, rec, rec, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	prevCtx := LaunchContext
	LaunchContext = func() context.Context { return ctx }
	t.Cleanup(func() { LaunchContext = prevCtx })

	_, err := LaunchViaActuator(store, workID, "claude_minimax", "claude_minimax", "slice2 cancelled", actor, now)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want context.Canceled from the installed LaunchContext", err)
	}
	if rec.calls != 1 || !rec.sawGuard || rec.started {
		t.Fatalf("adapter calls=%d sawGuard=%v started=%v; the guard must refuse the start on a cancelled context", rec.calls, rec.sawGuard, rec.started)
	}
	after, err := store.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "failed" {
		t.Fatalf("envelope state = %q; a cancelled launch must finalize as failed", after.State)
	}
	if rec.attemptID == "" || !slices.Contains(after.RunAttemptIDs, rec.attemptID) {
		t.Fatalf("RunAttemptIDs = %v; the cancelled attempt %q must be recorded through finalize", after.RunAttemptIDs, rec.attemptID)
	}
}

// TestLaunchViaActuatorLateRotationRefusedByStartGuard: between the wire's
// MintLaunchAuthority and the subprocess start, a concurrent actor
// re-mints the launch authority (rotating PolicyDecisionID on disk). The
// StartGuard the wire bound on RunInput revalidates the reservation under
// the admission lock and refuses the start; the process never starts, the
// adapter surfaces the refusal, finalize preserves the newer lifecycle
// state, and the envelope stays running under the rotated authority.
func TestLaunchViaActuatorLateRotationRefusedByStartGuard(t *testing.T) {
	const workID = "work_slice2_late_rotation"
	const actor = "actor:slice2-late-rotation"
	_, store, now := slice2RunningEnvelope(t, workID, actor)
	var rotatedPolicy string
	rec := &slice2RunnerAdapter{name: "claude_minimax"}
	rec.beforeGuard = func() {
		_, policyID, err := store.MintLaunchAuthority(workID, "actor:slice2-concurrent", "claude_minimax", "concurrent re-mint", now.Add(time.Second))
		if err != nil {
			t.Fatalf("concurrent re-mint: %v", err)
		}
		rotatedPolicy = policyID
	}
	installSlice2RunnerAdapter(t, rec, rec, nil)

	_, err := LaunchViaActuator(store, workID, "claude_minimax", "claude_minimax", "slice2 late rotation", actor, now)
	if err == nil {
		t.Fatal("launch after a concurrent authority rotation must refuse")
	}
	if !rec.sawGuard {
		t.Fatal("the wire did not bind a StartGuard on RunInput; a rotated reservation would have launched")
	}
	if rec.started {
		t.Fatal("the subprocess started despite the rotated reservation; the StartGuard must refuse before start")
	}
	after, err := store.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "running" {
		t.Fatalf("envelope state = %q; finalize must preserve the newer lifecycle state, not close under a stale reservation", after.State)
	}
	if rotatedPolicy == "" || after.PolicyDecisionID != rotatedPolicy {
		t.Fatalf("PolicyDecisionID = %q; want the concurrently rotated %q preserved on disk", after.PolicyDecisionID, rotatedPolicy)
	}
	if rec.attemptID != "" && slices.Contains(after.RunAttemptIDs, rec.attemptID) {
		t.Fatalf("RunAttemptIDs = %v; a refused start must not join the chain under a stale reservation", after.RunAttemptIDs)
	}
}

// TestLaunchViaActuatorClaudeFamilyPostureCarriesLaunchAuthority: a
// Claude-family adapter (GovernedPermissionAdapter) gets an execution
// posture on RunInput that names the launch PolicyDecision and carries
// the attested readiness evidence; the runner persists it on the attempt,
// so the attempt carries its generation directly, as the governed CLI
// launch path establishes.
func TestLaunchViaActuatorClaudeFamilyPostureCarriesLaunchAuthority(t *testing.T) {
	const workID = "work_slice2_claude_posture"
	const actor = "actor:slice2-claude-posture"
	homePath, store, now := slice2RunningEnvelope(t, workID, actor)
	pre, err := store.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	base := &slice2RunnerAdapter{name: "claude_minimax"}
	installSlice2RunnerAdapter(t, &slice2GovernedRunnerAdapter{slice2RunnerAdapter: base}, base, slice2AttestedEvidence())

	rattID, err := LaunchViaActuator(store, workID, "claude_minimax", "claude_minimax", "slice2 claude posture", actor, now)
	if err != nil {
		t.Fatalf("LaunchViaActuator: %v", err)
	}
	after, err := store.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "verifying" {
		t.Fatalf("envelope state = %q; want verifying after a succeeded launch", after.State)
	}
	att, err := runattempt.Open(homePath).Get(rattID)
	if err != nil {
		t.Fatal(err)
	}
	// The verifying transition mints its own pair, so the envelope's current
	// PolicyDecisionID is not the launch one; the posture must name the
	// launch decision (a fresh pol_ that did not exist before the launch).
	if att.ExecutionPosture == nil || att.ExecutionPosture.PermissionMode != "bypass" || !strings.HasPrefix(att.ExecutionPosture.PolicyDecisionID, "pol_") || att.ExecutionPosture.PolicyDecisionID == pre.PolicyDecisionID {
		t.Fatalf("attempt posture = %+v; want permission_mode bypass bound to a freshly minted launch PolicyDecision (pre=%q)", att.ExecutionPosture, pre.PolicyDecisionID)
	}
	if got := len(att.ExecutionPosture.AuthorityEvidence); got != 4 {
		t.Fatalf("attempt posture evidence = %d kinds; want the 4 attested readiness kinds", got)
	}
	// The pair the attempt carries must resolve at the close-time authority
	// gate: a governed close from verifying with this attempt on the chain
	// succeeds only when the attempt's (ActionIntent, PolicyDecision) is a
	// real generation of this envelope.
	if _, err := store.Close(workID, work.CloseInput{
		Outcome:       "completed",
		MoveType:      "audit_only",
		Actor:         actor,
		Summary:       "slice2 posture close proof",
		RunAttemptIDs: []string{rattID},
	}, now.Add(2*time.Second)); err != nil {
		t.Fatalf("close with the posture-bound attempt on the chain: %v", err)
	}
}

// TestLaunchViaActuatorClaudeFamilyUnattestedBypassFailsClosed: a
// Claude-family adapter whose lane readiness attested no evidence. The
// wire (work_r6m_seam_posture_20260911) refuses with a recognized
// `lane_unready:` prefix BEFORE invoking the adapter, so the runner's
// posture validator is never reached. An unattested bypass never
// launches — the refusal happens earlier than the pre-fix runner-level
// refusal, but the safety property holds: no subprocess starts, the
// recognized skip prefix lets the execute stage treat this as a skip
// reason (not a hard launch failure), and the envelope stays in
// `running` for the next Continue to retry.
func TestLaunchViaActuatorClaudeFamilyUnattestedBypassFailsClosed(t *testing.T) {
	const workID = "work_slice2_claude_unattested"
	const actor = "actor:slice2-claude-unattested"
	_, store, now := slice2RunningEnvelope(t, workID, actor)
	base := &slice2RunnerAdapter{name: "claude_minimax"}
	installSlice2RunnerAdapter(t, &slice2GovernedRunnerAdapter{slice2RunnerAdapter: base}, base, nil)

	rattID, err := LaunchViaActuator(store, workID, "claude_minimax", "claude_minimax", "slice2 claude unattested", actor, now)
	if err == nil || !strings.HasPrefix(err.Error(), "lane_unready:") {
		t.Fatalf("err = %v; want the wire's lane_unready: refusal (the wire now refuses BEFORE invoking the adapter, so the runner's posture validator is never reached)", err)
	}
	if base.started {
		t.Fatal("the subprocess started without attested authority evidence")
	}
	if rattID != "" {
		t.Fatalf("rattID = %q, want empty (no RunAttempt was minted when the wire refused before invoking the adapter)", rattID)
	}
	after, err := store.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "running" {
		t.Fatalf("envelope state = %q; the wire's lane_unready: refusal must NOT close the envelope — the execute stage records the skip and the next Continue retries", after.State)
	}
}
