package executewire

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/harness"
	"compound/internal/runattempt"
	"compound/internal/work"
)

// snapshotStub is a minimal harness.Adapter that records the RunAttempt
// snapshot the wire hands to it and reports it. The stub NEVER starts a
// subprocess — it mints + completes a RunAttempt and returns so the test
// is fast and deterministic.
//
// work_r7a_execution_selection_20260911.
type snapshotStub struct {
	name          string
	observedSnap  atomic.Pointer[runattempt.ExecutionSelectionSnapshot]
	observedInput atomic.Pointer[harness.RunInput]
}

func (s *snapshotStub) Name() string { return s.name }
func (s *snapshotStub) Run(_ context.Context, in harness.RunInput) (harness.RunOutput, error) {
	copyIn := in
	s.observedInput.Store(&copyIn)
	if in.ExecutionSelectionSnapshot != nil {
		snap := *in.ExecutionSelectionSnapshot
		s.observedSnap.Store(&snap)
	}
	raStore := runattempt.Open(stubHomePath)
	ra, err := raStore.MintWithActionIntent(in.WorkID, "wire-test", in.Harness, in.ActionIntentID, time.Now().UTC())
	if err != nil {
		return harness.RunOutput{}, err
	}
	ra, err = raStore.Complete(ra.ID, runattempt.StatusSucceeded, time.Now().UTC().Add(time.Second))
	if err != nil {
		return harness.RunOutput{}, err
	}
	return harness.RunOutput{RunAttempt: ra, ExitCode: 0, Duration: time.Second}, nil
}

// stubHomePath is set by each test before invoking LaunchViaActuator so
// the stub mints its RunAttempt into the same home the wire uses.
var stubHomePath string

// registerStubAdapter overrides the production adapter registry with the
// stub. The override is per-test (new registry per call) and the wire
// picks the registry via the AdapterFor hook, which test setup patches.
func registerStubAdapter(t *testing.T, home string, name string, stub *snapshotStub) {
	t.Helper()
	prev := AdapterFor
	AdapterFor = func(homePath, harnessName string) (harness.Adapter, bool) {
		if homePath != home || harnessName != name {
			return prev(homePath, harnessName)
		}
		return stub, true
	}
	t.Cleanup(func() { AdapterFor = prev })
}

// seedReadyWorkWithSelection creates a ready envelope that carries the
// given saved selection (nil is allowed). The returned home is the
// work store's home.
func seedReadyWorkWithSelection(t *testing.T, store *work.Store, id string, sel *envelope.ExecutionSelection) envelope.WorkEnvelope {
	t.Helper()
	now := time.Now().UTC()
	in := work.CreateInput{
		WorkID: id, Owner: "orchestrator:claude_minimax", Actor: "claude_code:orchestrator",
		State: "ready", Risk: "tier1", Privacy: "private",
		SuccessPredicate:   "selection snapshot stamped before adapter.Run",
		ExecutionSelection: sel,
	}
	w, err := store.Create(in, now)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// driveToRunning claims the envelope and advances it to "running" so
// the wire's preconditions pass. The git_delivery is already bound by
// attachGitDelivery, so the seam only needs Claim + advance to running.
func driveToRunning(t *testing.T, store *work.Store, workID, executorActor string, now time.Time) {
	t.Helper()
	if _, err := store.Claim(workID, executorActor, now); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := store.Advance(workID, work.AdvanceInput{To: "running", Actor: executorActor, Note: "sel-snapshot wire setup running"}, now); err != nil {
		t.Fatalf("advance to running: %v", err)
	}
}

// attachGitDelivery wires a workspace onto the envelope so the wire's
// workspace resolution succeeds. Goes through the production admission
// path (which runs the same-state git identity migration) so the
// envelope's GitDelivery carries a real fingerprint.
func attachGitDelivery(t *testing.T, store *work.Store, w envelope.WorkEnvelope) envelope.WorkEnvelope {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	sha := initGitRepoOrSkip(t, repo)
	if sha == "" {
		t.Fatal("could not init git repo for workspace fixture")
	}
	// Re-admit through store.Create so the migration stamps the
	// fingerprint. We delete the old envelope first.
	if err := os.Remove(filepath.Join(store.HomePath(), "work", "work", w.ID+".json")); err != nil {
		t.Fatal(err)
	}
	in := work.CreateInput{
		WorkID:             w.ID,
		Owner:              w.Owner,
		Actor:              "claude_code:orchestrator",
		State:              "ready",
		Risk:               w.Risk,
		Privacy:            w.Privacy,
		SuccessPredicate:   w.SuccessPredicate,
		ExecutionSelection: w.ExecutionSelection,
		GitDelivery: &envelope.GitDeliveryContract{
			TargetWorktreeRoot: repo,
			TargetLocalRef:     "refs/heads/main",
			ExpectedBaseSHA:    sha,
			PublicationMode:    "local_only",
		},
	}
	if _, err := store.Create(in, time.Now().UTC()); err != nil {
		t.Fatalf("re-create with git_delivery: %v", err)
	}
	got, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// installReadinessStub swaps LaunchReadiness so the production
// permission/hook check does not refuse; returns a restore func.
func installReadinessStub(t *testing.T) func() {
	t.Helper()
	orig := LaunchReadiness
	LaunchReadiness = func(work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
		return work.DelegateHarnessReadinessResult{Ready: true, Details: []string{"stubbed"}}, nil
	}
	return func() { LaunchReadiness = orig }
}

// installDelegateResolver wires a TupleBacked resolver into the wire
// so the snapshot builder reads a real observed profile revision. Each
// test pins a fixture tuple the helper can quote; the manifest is
// seeded by writeDriftedProfileManifest before the resolver installs.
func installDelegateResolver(t *testing.T, harnessName, command, modelID, route string) func() {
	t.Helper()
	prev := work.DelegateHarnessResolver
	work.DelegateHarnessResolver = func(work.DelegateHarnessResolveRequest) (work.DelegateHarnessResolution, error) {
		return work.DelegateHarnessResolution{
			Harness:                harnessName,
			LaneID:                 harnessName,
			Command:                command,
			RequestedModelID:       modelID,
			RequestedProviderRoute: route,
			TupleBacked:            true,
		}, nil
	}
	return func() { work.DelegateHarnessResolver = prev }
}

// A launch stamps the snapshot BEFORE adapter.Run sees the RunAttempt.
// The stub adapter records the snapshot at Run time and the test
// asserts it is set with the expected source/revision content.
//
// work_r7a_execution_selection_20260911.
func TestLaunchStampsSelectionSnapshotBeforeAdapterRun(t *testing.T) {
	home := initTestHome(t)
	if err := writeDriftedProfileManifest(t, home); err != nil {
		t.Fatal(err)
	}
	stubHomePath = home
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	sel := &envelope.ExecutionSelection{
		LaunchProfileID:       "claude-minimax",
		LaunchProfileRevision: "sha256:eeb7c22957a53d0876ede9bf0a28515c27665f5f2e329e83d93768d715400f4a",
		Harness:               "claude_minimax",
		ProviderRoute:         "minimax/MiniMax-M3",
		RequestedModelID:      "MiniMax-M3",
		Source:                "explicit",
		Revision:              2,
		SelectedBy:            "claude_code:orchestrator",
		SelectedAt:            now.Format(time.RFC3339),
	}
	w := seedReadyWorkWithSelection(t, store, "work_sel_snap", sel)
	w = attachGitDelivery(t, store, w)
	driveToRunning(t, store, w.ID, "test-actor", now)

	restoreResolver := installDelegateResolver(t, "claude_minimax", "claude", "MiniMax-M3", "minimax/MiniMax-M3")
	defer restoreResolver()
	stub := &snapshotStub{name: "claude_minimax"}
	registerStubAdapter(t, home, "claude_minimax", stub)
	restoreReadiness := installReadinessStub(t)
	defer restoreReadiness()

	if _, err := LaunchViaActuator(store, w.ID, "claude_minimax", "claude_minimax", "stub-prompt", "test-actor", now); err != nil {
		t.Fatalf("LaunchViaActuator: %v", err)
	}

	snap := stub.observedSnap.Load()
	if snap == nil {
		t.Fatalf("stub never observed a snapshot (wire did not pass RunInput.ExecutionSelectionSnapshot)")
	}
	if snap.Source != "saved" {
		t.Errorf("snapshot source = %q, want \"saved\"", snap.Source)
	}
	gotIn := stub.observedInput.Load()
	if gotIn == nil || gotIn.LaunchProfileID != snap.LaunchProfileID || snap.LaunchProfileID != sel.LaunchProfileID {
		t.Fatalf("saved launch profile did not drive adapter input: input=%+v snapshot=%+v", gotIn, snap)
	}
	if gotIn.RequestedModelID != sel.RequestedModelID || gotIn.RequestedProviderRoute != sel.ProviderRoute {
		t.Fatalf("adapter input substituted the saved tuple: model=%q route=%q, want model=%q route=%q", gotIn.RequestedModelID, gotIn.RequestedProviderRoute, sel.RequestedModelID, sel.ProviderRoute)
	}
	if snap.SelectionRevision != 2 {
		t.Errorf("snapshot selection_rev = %d, want 2", snap.SelectionRevision)
	}
	if snap.Harness != "claude_minimax" {
		t.Errorf("snapshot harness = %q, want claude_minimax", snap.Harness)
	}
	if snap.RequestedModelID != "MiniMax-M3" {
		t.Errorf("snapshot model = %q, want MiniMax-M3", snap.RequestedModelID)
	}
	if snap.EffortEvidence == "" {
		t.Errorf("snapshot effort_evidence is empty")
	}
}

// When the observed profile revision differs from the saved one, the
// snapshot records selection_drift so a reader sees why the run
// differed from the plan.
//
// work_r7a_execution_selection_20260911.
func TestLaunchRecordsSelectionDriftWhenProfileRevisionMoved(t *testing.T) {
	home := initTestHome(t)
	if err := writeDriftedProfileManifest(t, home); err != nil {
		t.Fatal(err)
	}
	stubHomePath = home
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	savedRev := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sel := &envelope.ExecutionSelection{
		LaunchProfileID:       "claude-minimax",
		LaunchProfileRevision: savedRev,
		Harness:               "claude_minimax",
		ProviderRoute:         "minimax/MiniMax-M3",
		RequestedModelID:      "MiniMax-M3",
		Source:                "explicit",
		Revision:              1,
		SelectedBy:            "claude_code:orchestrator",
		SelectedAt:            now.Format(time.RFC3339),
	}
	w := seedReadyWorkWithSelection(t, store, "work_sel_drift", sel)
	w = attachGitDelivery(t, store, w)
	driveToRunning(t, store, w.ID, "actor", now)

	restoreResolver := installDelegateResolver(t, "claude_minimax", "claude", "MiniMax-M3", "minimax/MiniMax-M3")
	defer restoreResolver()
	stub := &snapshotStub{name: "claude_minimax"}
	registerStubAdapter(t, home, "claude_minimax", stub)
	restoreReadiness := installReadinessStub(t)
	defer restoreReadiness()

	if _, err := LaunchViaActuator(store, w.ID, "claude_minimax", "claude_minimax", "stub", "actor", now); err != nil {
		t.Fatalf("launch: %v", err)
	}
	snap := stub.observedSnap.Load()
	if snap == nil {
		t.Fatalf("stub observed no snapshot")
	}
	if !strings.Contains(snap.SelectionDrift, "saved="+savedRev) {
		t.Errorf("selection_drift does not name the saved revision: %q", snap.SelectionDrift)
	}
	if !strings.Contains(snap.SelectionDrift, "launched=") {
		t.Errorf("selection_drift does not name the launched revision: %q", snap.SelectionDrift)
	}
}

// A lane with no effort control records the snapshot honestly:
// applied_effort stays empty, evidence says unsupported, native session
// evidence says unsupported, and the validator accepts the record.
//
// work_r7a_execution_selection_20260911.
func TestLaunchRecordsUnsupportedEffortHonestly(t *testing.T) {
	home := initTestHome(t)
	if err := writeDriftedProfileManifest(t, home); err != nil {
		t.Fatal(err)
	}
	stubHomePath = home
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	w := seedReadyWorkWithSelection(t, store, "work_sel_unsupported", nil)
	w = attachGitDelivery(t, store, w)
	driveToRunning(t, store, w.ID, "actor", now)

	// claude lanes bind the routed model but have no effort control; with no
	// effort requested the launch proceeds and records that honestly. (The
	// old fixture used os-harness-entrypoints, which binds no model: the
	// routed model now refuses there, see TestLaunchRefusesClaudeLaneEffortBeforeAdapterRun.)
	restoreResolver := installDelegateResolver(t, "claude_minimax", "claude", "MiniMax-M3", "minimax/MiniMax-M3")
	defer restoreResolver()
	stub := &snapshotStub{name: "claude_minimax"}
	registerStubAdapter(t, home, "claude_minimax", stub)
	restoreReadiness := installReadinessStub(t)
	defer restoreReadiness()

	if _, err := LaunchViaActuator(store, w.ID, "claude_minimax", "claude_minimax", "stub", "actor", now); err != nil {
		t.Fatalf("launch: %v", err)
	}
	snap := stub.observedSnap.Load()
	if snap == nil {
		t.Fatalf("stub observed no snapshot")
	}
	if snap.Source != "project_default" {
		t.Errorf("source = %q, want project_default", snap.Source)
	}
	if !strings.HasPrefix(snap.EffortEvidence, "unsupported:") {
		t.Errorf("expected unsupported effort evidence, got %q", snap.EffortEvidence)
	}
	if snap.AppliedEffort != "" {
		t.Errorf("applied_effort must be empty under unsupported, got %q", snap.AppliedEffort)
	}
	if !strings.HasPrefix(snap.NativeSessionEvidence, "unsupported:") {
		t.Errorf("expected unsupported session evidence, got %q", snap.NativeSessionEvidence)
	}
	if snap.NativeSessionID != "" {
		t.Errorf("native_session_id must be empty under unsupported, got %q", snap.NativeSessionID)
	}
}

// writeDriftedProfileManifest seeds a model-capabilities manifest so the
// tuple resolver reads a sha different from the saved profile revision.
// The drift the wire records is the diff between this manifest's sha and
// the saved envelope.revision.
func writeDriftedProfileManifest(t *testing.T, home string) error {
	t.Helper()
	body := "version: 1\n" +
		"purpose: drifted-profile fixture\n" +
		"capability_classes:\n" +
		"  - work.continue.unattended\n" +
		"models:\n" +
		"  claude-minimax:\n" +
		"    lane: claude-minimax\n" +
		"    harness: claude_code\n" +
		"    tier: cheap\n" +
		"    packet_ceiling: multi-file\n" +
		"    capabilities:\n" +
		"      work.continue.unattended:\n" +
		"        level: capable\n" +
		"routing_defaults:\n" +
		"  work.continue.unattended:\n" +
		"    - claude-minimax\n"
	dir := filepath.Join(home, "registries")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "model-capabilities.yaml"), []byte(body), 0o644)
}

// Fail-closed 2026-10-06: a saved claude-lane effort the wrapper cannot apply
// is refused by the wire with the typed reason BEFORE any launch authority or
// RunAttempt is minted; the adapter never runs. Preflight refuses the same way
// so the unattended seam treats it as a skip, not a failed launch.
func TestLaunchRefusesClaudeLaneEffortBeforeAdapterRun(t *testing.T) {
	home := initTestHome(t)
	if err := writeDriftedProfileManifest(t, home); err != nil {
		t.Fatal(err)
	}
	stubHomePath = home
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	sel := &envelope.ExecutionSelection{
		LaunchProfileID:       "claude-minimax",
		LaunchProfileRevision: "sha256:eeb7c22957a53d0876ede9bf0a28515c27665f5f2e329e83d93768d715400f4a",
		Harness:               "claude_minimax",
		ProviderRoute:         "minimax/MiniMax-M3",
		RequestedModelID:      "MiniMax-M3",
		RequestedEffort:       "xhigh",
		Source:                "explicit",
		Revision:              2,
		SelectedBy:            "claude_code:orchestrator",
		SelectedAt:            now.Format(time.RFC3339),
	}
	w := seedReadyWorkWithSelection(t, store, "work_sel_effort_refused", sel)
	w = attachGitDelivery(t, store, w)

	restoreResolver := installDelegateResolver(t, "claude_minimax", "claude", "MiniMax-M3", "minimax/MiniMax-M3")
	defer restoreResolver()
	stub := &snapshotStub{name: "claude_minimax"}
	registerStubAdapter(t, home, "claude_minimax", stub)
	restoreReadiness := installReadinessStub(t)
	defer restoreReadiness()

	const want = "selection_unsupported:claude_minimax:effort"
	if err := PreflightViaActuator(store, w.ID, "claude_minimax", now); err == nil || err.Error() != want {
		t.Fatalf("preflight err = %v, want %s", err, want)
	}
	driveToRunning(t, store, w.ID, "test-actor", now)
	before, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	rattID, err := LaunchViaActuator(store, w.ID, "claude_minimax", "claude_minimax", "stub-prompt", "test-actor", now)
	var refusal *harness.SelectionUnsupportedError
	if !errors.As(err, &refusal) || err.Error() != want || rattID != "" {
		t.Fatalf("launch = (%q, %v), want refusal %s", rattID, err, want)
	}
	if stub.observedInput.Load() != nil {
		t.Fatal("adapter ran despite the refusal")
	}
	attempts, err := runattempt.Open(home).List()
	if err != nil {
		t.Fatal(err)
	}
	for _, ra := range attempts {
		if ra.Origin == runattempt.OriginHarnessExecution {
			t.Fatalf("refusal minted a harness execution attempt: %+v", ra)
		}
	}
	after, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != before.State || len(after.RunAttemptIDs) != len(before.RunAttemptIDs) || after.PolicyDecisionID != before.PolicyDecisionID {
		t.Fatalf("refusal mutated the envelope: before=%s/%v after=%s/%v", before.State, before.RunAttemptIDs, after.State, after.RunAttemptIDs)
	}
}
