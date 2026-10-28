// DEFECT 3 (AUT-P0-003) — the chain that ties a running subprocess to the
// decision that authorized it MUST be end-to-end auditable:
//
//	frozen WorkEnvelope generation
//	-> persisted unattended ActionIntent
//	-> persisted PolicyDecision
//	-> those exact ids on the running reservation
//	-> RunInput.ActionIntentID
//	-> RunAttempt
//	-> verifying transition comparing the SAME ids
//
// The pre-fix wire built harness.RunInput with no ActionIntentID, so
// runWithSubprocess called MintWithActionIntent(..., "", ...) and the
// RunAttempt carried an empty authority link — nothing on disk tied the
// running process back to the policy decision that permitted it. These
// tests pin every link in the chain and the close-time refusal of a chain
// that drifted between launch and finalization.

package executewire

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"compound/internal/actuator"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/harness"
	"compound/internal/runattempt"
	"compound/internal/work"
)

var _ = error(nil) // keep errors import alive across test rewrites

// authorityBindingAdapter is a stub harness.Adapter that records the
// RunInput.its received and stamps a controllable terminal RunAttempt on
// the way back. The terminal RunAttempt carries the SAME ActionIntentID
// the adapter saw on RunInput, so a downstream check can prove the
// attempt is bound to the wire's persisted authority rather than to an
// empty string (DEFECT 3's red state). It also persists the attempt via
// runattempt.Open so the on-disk shape matches what a real governed
// subprocess would produce.
type authorityBindingAdapter struct {
	name    string
	home    string
	lastIn  harness.RunInput
	calls   int
	station string // expected ActionIntentID; mismatch fails the test
}

func (a *authorityBindingAdapter) Name() string { return a.name }

func (a *authorityBindingAdapter) Run(_ context.Context, in harness.RunInput) (harness.RunOutput, error) {
	a.calls++
	a.lastIn = in
	if a.station != "" && in.ActionIntentID != a.station {
		return harness.RunOutput{}, fmt.Errorf("authority-binding stub: RunInput.ActionIntentID = %q, want %q", in.ActionIntentID, a.station)
	}
	now := time.Now().UTC()
	raStore := runattempt.Open(a.home)
	ra, mintErr := raStore.MintWithActionIntent(in.WorkID, "compound", a.name, in.ActionIntentID, now)
	if mintErr != nil {
		return harness.RunOutput{}, fmt.Errorf("authority-binding stub: mint run attempt: %w", mintErr)
	}
	if _, err := raStore.SetOrigin(ra.ID, runattempt.OriginHarnessExecution); err != nil {
		return harness.RunOutput{}, fmt.Errorf("authority-binding stub: set origin: %w", err)
	}
	if _, err := raStore.SetWorkspace(ra.ID, in.Workspace); err != nil {
		return harness.RunOutput{}, fmt.Errorf("authority-binding stub: set workspace: %w", err)
	}
	if _, err := raStore.SetCommand(ra.ID, in.Command); err != nil {
		return harness.RunOutput{}, fmt.Errorf("authority-binding stub: set command: %w", err)
	}
	terminal, err := raStore.Complete(ra.ID, runattempt.StatusSucceeded, now)
	if err != nil {
		return harness.RunOutput{}, fmt.Errorf("authority-binding stub: complete: %w", err)
	}
	return harness.RunOutput{RunAttempt: terminal}, nil
}

// installAuthorityBindingLaunchAdapter swaps AdapterFor to return an
// authorityBindingAdapter. Unlike argvRecordingAdapter, this stub makes the
// minted RunAttempt carry the SAME ActionIntentID RunInput carried, which is
// exactly the invariant the production wire must produce.
func installAuthorityBindingLaunchAdapter(t *testing.T, homePath, harnessName string) *authorityBindingAdapter {
	t.Helper()
	rec := &authorityBindingAdapter{name: harnessName, home: homePath, station: ""}
	prevAdapter := AdapterFor
	AdapterFor = func(home, name string) (harness.Adapter, bool) {
		if name == harnessName {
			return rec, true
		}
		return nil, false
	}
	t.Cleanup(func() { AdapterFor = prevAdapter })
	prevReadiness := LaunchReadiness
	LaunchReadiness = func(req work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
		return work.DelegateHarnessReadinessResult{Ready: true}, nil
	}
	t.Cleanup(func() { LaunchReadiness = prevReadiness })
	return rec
}

// readActionIntentEnvelope reads a persisted act_* envelope off the home's
// event store. The test uses this to prove the launch produced a
// persisted unattended ActionIntent (DEFECT 3's chain element #1).
func readActionIntentEnvelope(t *testing.T, homePath, id string) envelope.ActionIntent {
	t.Helper()
	es, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatalf("open event store: %v", err)
	}
	rec, err := es.Read(id)
	if err != nil {
		t.Fatalf("read ActionIntent %s: %v", id, err)
	}
	var ai envelope.ActionIntent
	if err := json.Unmarshal(rec.Bytes, &ai); err != nil {
		t.Fatalf("unmarshal ActionIntent %s: %v", id, err)
	}
	if ai.ID != id {
		t.Fatalf("ActionIntent id = %q, want %q", ai.ID, id)
	}
	return ai
}

// readPolicyDecisionEnvelope reads a persisted pol_* envelope off the home's
// event store. The test uses this to prove the launch produced a
// persisted unattended PolicyDecision (DEFECT 3's chain element #2).
func readPolicyDecisionEnvelope(t *testing.T, homePath, id string) envelope.PolicyDecision {
	t.Helper()
	es, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatalf("open event store: %v", err)
	}
	rec, err := es.Read(id)
	if err != nil {
		t.Fatalf("read PolicyDecision %s: %v", id, err)
	}
	var pd envelope.PolicyDecision
	if err := json.Unmarshal(rec.Bytes, &pd); err != nil {
		t.Fatalf("unmarshal PolicyDecision %s: %v", id, err)
	}
	if pd.ID != id {
		t.Fatalf("PolicyDecision id = %q, want %q", pd.ID, id)
	}
	return pd
}

// defect3LaunchSeed brings a work envelope from ready through claim +
// advance(running), the way the execute seam does before calling the
// launch wire. Returns the running envelope so callers can compare the
// running-reservation IDs against the wire's authority.
func defect3LaunchSeed(t *testing.T, store *work.Store, workID, actor string, now time.Time) envelope.WorkEnvelope {
	t.Helper()
	if _, err := store.Claim(workID, actor, now); err != nil {
		t.Fatalf("claim: %v", err)
	}
	advanced, err := store.Advance(workID, work.AdvanceInput{To: "running", Actor: actor, Note: "defect3 wire setup"}, now)
	if err != nil {
		t.Fatalf("advance to running: %v", err)
	}
	return advanced
}

// TestLaunchViaActuatorBindsAuthorityChain (DEFECT 3 RED -> GREEN):
//
// The production launch must (a) persist exactly one new ActionIntent and
// exactly one new PolicyDecision for THIS launch, (b) carry those exact
// ids on the running reservation, (c) pass ActionIntentID on
// harness.RunInput, and (d) mint the RunAttempt with that ActionIntentID.
//
// RED before fix: RunInput.ActionIntentID is "" (the wire never sets it),
// runWithSubprocess mints the RunAttempt with an empty ActionIntentID,
// and no act_/pol_ pair was created specifically for the launch. The
// GREEN fix is for the launch wire to mint a launch-specific act_/pol_
// pair, persist them, append the act_ to the wenv, propagate the same id
// on RunInput, and let the verifying transition compare the same ids.
func TestLaunchViaActuatorBindsAuthorityChain(t *testing.T) {
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
	seedExecuteStageReadyWorkWithDelivery(t, store, "work_defect3_chain", homePath)

	// Install the authority-binding adapter so the test fails loudly if
	// RunInput.ActionIntentID doesn't arrive — the stub's station field is
	// set once we know the expected id (the persisted launch act_ id).
	rec := installAuthorityBindingLaunchAdapter(t, homePath, "claude_minimax")
	prevReadiness := LaunchReadiness
	LaunchReadiness = func(req work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
		return work.DelegateHarnessReadinessResult{Ready: true}, nil
	}
	t.Cleanup(func() { LaunchReadiness = prevReadiness })

	const actor = "actor:defect3"
	const lane = "claude_minimax"
	running := defect3LaunchSeed(t, store, "work_defect3_chain", actor, now)

	// Capture the running-reservation's authority so we can prove the
	// launch mints a NEW pair rather than reusing it.
	runningAction := ""
	if n := len(running.ActionIntentIDs); n > 0 {
		runningAction = running.ActionIntentIDs[n-1]
	}
	runningPolicy := strings.TrimSpace(running.PolicyDecisionID)
	if runningAction == "" || runningPolicy == "" {
		t.Fatalf("running reservation missing authority: action=%q policy=%q", runningAction, runningPolicy)
	}

	rattID, err := LaunchViaActuator(store, "work_defect3_chain", lane, "claude_minimax", "defect3 prompt", actor, now)
	if err != nil {
		t.Fatalf("LaunchViaActuator: %v", err)
	}
	if rattID == "" {
		t.Fatal("expected non-empty RunAttempt id")
	}

	// (1) The adapter saw a non-empty ActionIntentID — this is the RED
	// assertion: before the fix RunInput.ActionIntentID is "".
	if rec.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", rec.calls)
	}
	if rec.lastIn.ActionIntentID == "" {
		t.Fatal("RunInput.ActionIntentID is empty (DEFECT 3 RED state); the RunAttempt minted with an empty authority link")
	}
	if !strings.HasPrefix(rec.lastIn.ActionIntentID, "act_") {
		t.Fatalf("RunInput.ActionIntentID = %q, want an act_ id", rec.lastIn.ActionIntentID)
	}

	// (2) The persisted RunAttempt carries the same ActionIntentID. The
	// adapter stub uses the same id it received, so this proves the
	// attempt minted via MintWithActionIntent at
	// internal/harness/subprocess_runner.go:98 was bound to the
	// wire's intent rather than to "".
	att, err := runattempt.Open(homePath).Get(rattID)
	if err != nil {
		t.Fatalf("get RunAttempt %s: %v", rattID, err)
	}
	if att.ActionIntentID != rec.lastIn.ActionIntentID {
		t.Fatalf("RunAttempt.ActionIntentID = %q, want %q", att.ActionIntentID, rec.lastIn.ActionIntentID)
	}

	// (3) The same act_ envelope was PERSISTED to the event store
	// (DEFECT 3 chain element: persisted unattended ActionIntent).
	ai := readActionIntentEnvelope(t, homePath, rec.lastIn.ActionIntentID)
	if ai.ID != rec.lastIn.ActionIntentID {
		t.Fatalf("persisted act_ id = %q, want %q", ai.ID, rec.lastIn.ActionIntentID)
	}

	// (4) A pol_ envelope bound to that act_ was PERSISTED on the same
	// home (DEFECT 3 chain element: persisted PolicyDecision). The wire's
	// finalization writes this PD; its ActionIntentID must match.
	// Read the envelope's current running action/policy and require a
	// matching pol_ envelope exists; the test directly inspects the
	// envelope's PolicyDecisionID is on disk in the event store.
	wenv, err := store.Get("work_defect3_chain")
	if err != nil {
		t.Fatal(err)
	}
	postPolicy := strings.TrimSpace(wenv.PolicyDecisionID)
	if postPolicy == "" {
		t.Fatal("post-launch envelope has empty PolicyDecisionID — finalization advance erased the running reservation")
	}
	pd := readPolicyDecisionEnvelope(t, homePath, postPolicy)
	if pd.ActionIntentID != wenv.ActionIntentIDs[len(wenv.ActionIntentIDs)-1] {
		t.Fatalf("PolicyDecision %s bound to %q, want %q",
			postPolicy, pd.ActionIntentID,
			wenv.ActionIntentIDs[len(wenv.ActionIntentIDs)-1])
	}

	// (5) The launching act_ is NEW, distinct from the running-reservation
	// act_, and the final verifying transition still succeeds against
	// the launching ids (the post-launch envelope's action_intent_ids
	// does not contain BOTH the running pair and the launch pair — the
	// wire's launch authority is the LAST one now, since the launch
	// appends it and the finalization advance must mint a fresh verify
	// pair on top).
	if postAction := wenv.ActionIntentIDs[len(wenv.ActionIntentIDs)-1]; postAction == runningAction {
		t.Fatalf("post-launch envelope tail action_intent = %q (the running-reservation id); expected a freshly minted launch authority",
			postAction)
	}
	if postPolicy == runningPolicy {
		t.Fatalf("post-launch envelope PolicyDecisionID = %q (the running-reservation policy); expected a freshly minted launch authority",
			postPolicy)
	}

	// (6) The sealed envelope state is "verifying" — the finalization
	// advance succeeded with the same ids (DEFECT 3 chain element:
	// verifying transition comparing the SAME ids).
	if wenv.State != "verifying" {
		t.Fatalf("envelope state = %q, want verifying (finalization refused under the chain ids)", wenv.State)
	}
}

// TestLaunchViaActuatorBindsNewAuthorityNotOlderTail (DEFECT 3 RED -> GREEN):
//
// If the envelope already carries older action-intent ids (from prior
// lifecycle events: claim, advance-to-running, prior retry...), the
// launch MUST bind a freshly minted act_/pol_, not the tail of the
// already-existing list. The pre-fix wire never minted a launch
// authority (RunInput.ActionIntentID was empty) — so the RunAttempt
// was minted with an empty authority link. The test asserts the post-
// fix behavior: a NEW act_/pol_ pair exists for THIS launch and the
// RunAttempt carries the new id.
//
// RED output before fix: RunInput.ActionIntentID == "" — the adapter
// call lands with an empty action_intent_id, the assertion
// `if !strings.HasPrefix(rec.lastIn.ActionIntentID, "act_")` fires.
func TestLaunchViaActuatorBindsNewAuthorityNotOlderTail(t *testing.T) {
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
	seedExecuteStageReadyWorkWithDelivery(t, store, "work_defect3_older_tail", homePath)

	rec := installAuthorityBindingLaunchAdapter(t, homePath, "claude_minimax")
	prevReadiness := LaunchReadiness
	LaunchReadiness = func(req work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
		return work.DelegateHarnessReadinessResult{Ready: true}, nil
	}
	t.Cleanup(func() { LaunchReadiness = prevReadiness })

	const actor = "actor:defect3-retried"
	const lane = "claude_minimax"

	// First live cycle: claim + advance to running mints one (ai, pol)
	// pair already. The launch under test must NOT bind that one.
	running := defect3LaunchSeed(t, store, "work_defect3_older_tail", actor, now)
	olderAction := running.ActionIntentIDs[len(running.ActionIntentIDs)-1]
	olderPolicy := strings.TrimSpace(running.PolicyDecisionID)

	rattID, err := LaunchViaActuator(store, "work_defect3_older_tail", lane, "claude_minimax", "defect3 prompt", actor, now)
	if err != nil {
		t.Fatalf("LaunchViaActuator: %v", err)
	}
	if rec.lastIn.ActionIntentID == "" {
		t.Fatal("RunInput.ActionIntentID is empty (DEFECT 3 RED state)")
	}
	if !strings.HasPrefix(rec.lastIn.ActionIntentID, "act_") {
		t.Fatalf("RunInput.ActionIntentID = %q does not start with act_", rec.lastIn.ActionIntentID)
	}
	if rec.lastIn.ActionIntentID == olderAction {
		t.Fatalf("RunInput.ActionIntentID = %q is the older running-reservation id; the launch must bind a fresh act_, not the older tail",
			rec.lastIn.ActionIntentID)
	}

	// The RunAttempt is on the NEW act_ id, not the older one.
	att, err := runattempt.Open(homePath).Get(rattID)
	if err != nil {
		t.Fatalf("get RunAttempt: %v", err)
	}
	if att.ActionIntentID != rec.lastIn.ActionIntentID {
		t.Fatalf("RunAttempt.ActionIntentID = %q, want %q (the launch authority)",
			att.ActionIntentID, rec.lastIn.ActionIntentID)
	}
	if att.ActionIntentID == olderAction {
		t.Fatal("RunAttempt bound to the older running-reservation tail act_")
	}

	// The new act_ and the envelope-bound pol_ are PERSISTED. The pol_
	// the finalization advance wrote is NOT the older running one.
	wenv, err := store.Get("work_defect3_older_tail")
	if err != nil {
		t.Fatal(err)
	}
	if wenv.PolicyDecisionID == olderPolicy {
		t.Fatalf("post-launch envelope PolicyDecisionID = %q (the older running-reservation pol_); the launch must bind a fresh pol_",
			wenv.PolicyDecisionID)
	}
	if wenv.ActionIntentIDs[len(wenv.ActionIntentIDs)-1] == olderAction {
		t.Fatal("post-launch envelope's tail action_intent is the older running-reservation act_; a fresh launch authority should be appended")
	}
}

// TestFinalizeRefusesWhenAuthorityDrifts (DEFECT 3 GREEN-side evidence):
//
// FinalizeGovernedHarnessRun MUST refuse when the (RunningPolicyID,
// RunningActionIntentID) it was given does not match the live envelope.
// The launch wire, post-fix, stamps a freshly minted launch authority
// onto the wenv and tells FinalizeGovernedHarnessRun exactly those
// ids. If anything mutates the wenv between launch and finalize,
// FinalizeGovernedHarnessRun's reservation guard (via
// validateExpectedExecutionReservation) compares the id it was given
// against the wenv NOW and refuses (the advanced Succeeded=false path
// surfaces this through result.Warnings; the close-time gate refuses
// the terminal transition entirely).
func TestFinalizeRefusesWhenAuthorityDrifts(t *testing.T) {
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
	seedExecuteStageReadyWorkWithDelivery(t, store, "work_defect3_finalize_drift", homePath)

	rec := installAuthorityBindingLaunchAdapter(t, homePath, "claude_minimax")
	prevReadiness := LaunchReadiness
	LaunchReadiness = func(req work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
		return work.DelegateHarnessReadinessResult{Ready: true}, nil
	}
	t.Cleanup(func() { LaunchReadiness = prevReadiness })

	const actor = "actor:defect3-finalize-drift"
	const lane = "claude_minimax"
	defect3LaunchSeed(t, store, "work_defect3_finalize_drift", actor, now)

	// Mint a deterministic, well-formed ratt_ id for the forged output
	// (runattempt.Open refuses ill-formed ids with no fallback).
	forgedRatt := runattempt.MintDeterministic("forged-finalize-drift", now)
	forgedOutput := harness.RunOutput{
		RunAttempt: runattempt.RunAttempt{
			ID:             forgedRatt,
			Harness:        "claude_minimax",
			WorkID:         "work_defect3_finalize_drift",
			Status:         runattempt.StatusSucceeded,
			ActionIntentID: rec.lastIn.ActionIntentID,
			CreatedAt:      now,
		},
	}
	wrong := actuator.GovernedHarnessFinalizationInput{
		Store:                 store,
		WorkID:                "work_defect3_finalize_drift",
		Actor:                 actor,
		HarnessName:           "claude_minimax",
		RunningPolicyID:       "pol_fabricated_mismatch",
		RunningActionIntentID: "act_fabricated_mismatch",
		Output:                forgedOutput,
		Now:                   now,
	}
	res, _ := actuator.FinalizeGovernedHarnessRun(wrong)
	// The reservation guard must surface as a Warning naming the
	// reservation mismatch (path: Advance -> ErrExecutionReservationChanged
	// -> reservationErr -> result.Warnings) — a succeeded result would
	// mean the drift slipped past the chain comparison.
	hit := false
	for _, w := range res.Warnings {
		if strings.Contains(strings.ToLower(w), "reservation") &&
			strings.Contains(strings.ToLower(w), "changed") {
			hit = true
			break
		}
	}
	if !hit {
		t.Fatalf("finalization result = %+v; want a Warning naming a reservation drift", res)
	}

	// Envelope stays in "running": the finalization refusal did not
	// mutate state.
	after, err := store.Get("work_defect3_finalize_drift")
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "running" {
		t.Fatalf("envelope state = %q after a finalization refusal, want running (chain refusal must leave state untouched)", after.State)
	}
}
