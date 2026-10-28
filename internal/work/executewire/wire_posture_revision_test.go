// work_r6m_seam_posture_20260911 — posture evidence must carry a revision
// before it is stamped onto a Claude-family RunAttempt. Today the readiness
// guard in cmd/compound adds an `executable` evidence item WITHOUT a
// Revision (the field is `omitempty`), the launch wire copies every item
// into the ExecutionPosture verbatim, and runattempt.SetExecutionPosture
// refuses any item lacking kind/ref/revision. The seam dies at the posture
// stamp on every tuple-backed Claude launch and the live run minting the
// envelope never reaches adapter.Run.
//
// RED shapes (this file pins both before any code change):
//   - TestUnattendedLaunchReachesTheAdapterForATupleBackedLane: readiness
//     returns an `executable` evidence item WITHOUT a Revision (exactly the
//     production shape). The launch wire MUST fill in a real revision (from
//     the lane tuple's profile revision / model-capabilities snapshot) and
//     the launch must reach a stub adapter's Run. RED today because the
//     wire copies the item verbatim and runattempt refuses.
//   - TestUnattendedLaunchRefusesPostureEvidenceWithoutRevision: an
//     evidence kind that the wire CANNOT give a revision to MUST surface
//     as a recognized `lane_unready:` prefix, no attempt left `running`.
//     The wire's contract: never drop evidence silently, never stamp junk.
//     RED today because the wire passes evidence verbatim with no revision
//     check.
package executewire

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/harness"
	"compound/internal/runattempt"
	"compound/internal/work"
)

// postureStubAdapter is the stub Claude-family adapter the posture tests
// use. It records the RunInput it received and mints a RunAttempt whose
// status is Succeeded so the wire's canonical finalize can advance the
// envelope. The adapter also implements GovernedPermissionAdapter so the
// wire's posture-building branch executes end-to-end. Today this branch
// fails at runattempt.SetExecutionPosture because the production readiness
// evidence has at least one item (the `executable` kind) without a
// Revision; once the wire fills the Revision from the tuple resolver, the
// stamp succeeds and the launch reaches adapter.Run.
type postureStubAdapter struct {
	name     string
	home     string
	lastIn   harness.RunInput
	calls    int
	mintedID string
}

func (a *postureStubAdapter) Name() string { return a.name }

func (a *postureStubAdapter) GovernedPermissionPosture() {}

func (a *postureStubAdapter) RevalidateGovernedRun(ctx context.Context, in harness.RunInput) (harness.RunInput, error) {
	return in, nil
}

func (a *postureStubAdapter) Run(ctx context.Context, in harness.RunInput) (harness.RunOutput, error) {
	a.calls++
	a.lastIn = in
	now := time.Now().UTC()
	raStore := runattempt.Open(a.home)
	ra, err := raStore.MintWithActionIntent(in.WorkID, "compound", a.name, in.ActionIntentID, now)
	if err != nil {
		return harness.RunOutput{}, err
	}
	if _, err := raStore.SetOrigin(ra.ID, runattempt.OriginHarnessExecution); err != nil {
		return harness.RunOutput{}, err
	}
	if _, err := raStore.SetWorkspace(ra.ID, in.Workspace); err != nil {
		return harness.RunOutput{}, err
	}
	if _, err := raStore.SetCommand(ra.ID, in.Command); err != nil {
		return harness.RunOutput{}, err
	}
	// Pin the wire's ExecutionPosture onto the attempt so the runattempt
	// posture validator (the gate the live run dies at) actually fires.
	// The whole point of these tests is to exercise SetExecutionPosture —
	// without it the wire's posture-building branch is a no-op.
	if in.ExecutionPosture != nil {
		posture := *in.ExecutionPosture
		stamped, err := raStore.SetExecutionPosture(ra.ID, posture)
		if err != nil {
			// SetExecutionPosture refused: surface the validator's
			// error verbatim so the wire can name the bad evidence
			// item (production failure shape).
			return harness.RunOutput{RunAttempt: ra}, err
		}
		ra = stamped
	}
	terminal, err := raStore.Complete(ra.ID, runattempt.StatusSucceeded, now)
	if err != nil {
		return harness.RunOutput{}, err
	}
	a.mintedID = terminal.ID
	return harness.RunOutput{RunAttempt: terminal}, nil
}

func installPostureStubAdapter(t *testing.T, homePath, harnessName string) *postureStubAdapter {
	t.Helper()
	adapter := &postureStubAdapter{name: harnessName, home: homePath}
	prevAdapter := AdapterFor
	AdapterFor = func(home, name string) (harness.Adapter, bool) {
		if name == harnessName {
			return adapter, true
		}
		return nil, false
	}
	t.Cleanup(func() { AdapterFor = prevAdapter })
	prevCtx := LaunchContext
	LaunchContext = func() context.Context { return context.Background() }
	t.Cleanup(func() { LaunchContext = prevCtx })
	prevInstalled := launchContextInstalled.Load()
	SetLaunchContextInstalled(true)
	t.Cleanup(func() { SetLaunchContextInstalled(prevInstalled) })
	return adapter
}

// stubPostureReadiness installs LaunchReadiness to return the given
// evidence (no Details, Ready=true). The `executable` evidence item is the
// one cmd/compound's readiness evaluator produces today — Ref set, no
// Revision — and it is the production shape that breaks the wire.
func stubPostureReadiness(t *testing.T, evidence []work.DelegateHarnessReadinessEvidence) {
	t.Helper()
	prev := LaunchReadiness
	LaunchReadiness = func(req work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
		return work.DelegateHarnessReadinessResult{Ready: true, Evidence: evidence}, nil
	}
	t.Cleanup(func() { LaunchReadiness = prev })
}

// productionReadinessEvidence returns the readiness evidence shape the
// production cmd/compound/work_delegate_readiness.go produces for a
// ready tuple-backed Claude lane: the four required kinds (each with a
// Revision) AND the `executable` supporting kind WITHOUT a Revision
// (the live-run shape that broke the wire). Tests that exercise the
// green posture path use this so the wire is forced to filter the
// supporting kinds AND keep the required kinds exactly as production
// needs.
func productionReadinessEvidence() []work.DelegateHarnessReadinessEvidence {
	return []work.DelegateHarnessReadinessEvidence{
		{Kind: "executable", Ref: "lane-tuple://claude-minimax (token plan)"},
		{Kind: "permission_authority", Ref: "harness://claude_minimax/permissions", Revision: "sha256:perm00000000000000000000000000000000000000000000000000000000"},
		{Kind: "permission_posture", Ref: "harness-posture://claude_minimax", Revision: "sha256:post00000000000000000000000000000000000000000000000000000000"},
		{Kind: "blocking_hook_plan", Ref: "binding://hook-plan-claude_minimax", Revision: "sha256:plan00000000000000000000000000000000000000000000000000000000"},
		{Kind: "blocking_hook_installation", Ref: "observation://hook-install-claude_minimax", Revision: "sha256:inst00000000000000000000000000000000000000000000000000000000"},
	}
}

// TestUnattendedLaunchReachesTheAdapterForATupleBackedLane — the live-run
// shape. Readiness returns the four required kinds (with Revisions) AND
// an `executable` supporting kind WITHOUT a Revision (the production
// code path in cmd/compound/work_delegate_readiness.go does exactly this
// for a tuple-backed Claude lane). The wire MUST filter the supporting
// kinds, keep the four required kinds with their Revisions intact, and
// reach adapter.Run.
//
// RED today: the wire copies every item verbatim (including the
// `executable` item with no Revision) into the ExecutionPosture, the
// runner's SetExecutionPosture refuses with "execution posture evidence
// requires kind, ref, revision", and the launch dies at the posture
// stamp — adapter.calls == 0.
func TestUnattendedLaunchReachesTheAdapterForATupleBackedLane(t *testing.T) {
	homePath := initTestHome(t)
	now := time.Now().UTC()
	seedExecuteStageManifestWithTuple(t, homePath, "claude-minimax (token plan)", "claude_minimax", "minimax", []string{"/bin/echo"})
	seedExecuteStageGrant(t, homePath)
	seedExecuteStageCapacityRow(t, homePath, 30, 2*time.Minute, now)
	capacityTestConfig(t, homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	const workID = "work_posture_revision_filled"
	seedExecuteStageReadyWorkWithDelivery(t, store, workID, homePath)
	adapter := installPostureStubAdapter(t, homePath, "claude_minimax")
	// Production-shape evidence: `executable` (no Revision, supporting
	// kind) plus the four required kinds (each with a Revision). The
	// wire must filter `executable` out of the posture (the runattempt
	// validator rejects mutation-capable postures carrying supporting
	// kinds) AND stamp the four required kinds onto the posture with
	// their Revisions intact.
	stubPostureReadiness(t, productionReadinessEvidence())
	defect4LaunchSeed(t, store, workID, "actor:posture-revision", now)

	rattID, launchErr := LaunchViaActuator(store, workID, "claude_minimax", "claude_minimax", "posture revision", "actor:posture-revision", now)
	if launchErr != nil {
		t.Fatalf("LaunchViaActuator returned error %v; the wire MUST filter supporting evidence kinds (executable, …) AND stamp the four required kinds so the launch reaches adapter.Run on the production evidence shape", launchErr)
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter.calls = %d, want 1; the launch wire died before reaching the adapter (the live run dies at the posture stamp with `execution posture evidence requires kind, ref, revision`)", adapter.calls)
	}
	if rattID != adapter.mintedID || rattID == "" {
		t.Fatalf("wire returned run attempt id %q, want the minted %q", rattID, adapter.mintedID)
	}
}

// TestUnattendedLaunchRefusesPostureEvidenceWithoutRevision — the wire's
// refusal path. When readiness evidence does NOT carry the four
// runattempt-accepted kinds (the readiness guard reported not-ready or
// the wire's resolver fell through), the wire MUST surface a recognized
// `lane_unready:` prefix and leave no attempt in `running`. The execute
// stage treats `lane_unready:` as a refusal prefix, not a hard launch
// failure — the second defect's `failed to record failure` noise goes
// away because the stage no longer reaches its Close branch.
//
// RED today: the wire copies evidence verbatim and the failure surfaces
// deep inside the runner as `runattempt: execution posture evidence
// requires kind, ref, revision`, which the stage then treats as a hard
// failure and tries to Close — exactly the second defect.
func TestUnattendedLaunchRefusesPostureEvidenceWithoutRevision(t *testing.T) {
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
	const workID = "work_posture_revision_refused"
	seedExecuteStageReadyWorkWithDelivery(t, store, workID, homePath)
	adapter := installPostureStubAdapter(t, homePath, "claude_minimax")
	// Stub DelegateHarnessResolver to a NON-tuple-backed resolver so
	// resolveLaneLaunchTuple returns ("", "", "", "", false) and the wire
	// has no profile revision to fall back on. Delete the
	// model-capabilities manifest so the wire cannot derive a revision
	// from the snapshot either.
	prevResolver := work.DelegateHarnessResolver
	work.DelegateHarnessResolver = func(req work.DelegateHarnessResolveRequest) (work.DelegateHarnessResolution, error) {
		return work.DelegateHarnessResolution{
			Harness:     req.Harness,
			LaneID:      req.Harness,
			TupleBacked: false,
		}, nil
	}
	t.Cleanup(func() { work.DelegateHarnessResolver = prevResolver })
	if err := removeManifest(t, homePath); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}
	// Evidence shape the wire MUST refuse on: ONLY a supporting kind
	// (`executable`) with no Revision. The wire filters supporting kinds
	// out of the posture, finds none of the four required kinds are
	// present, and refuses with `lane_unready:` naming the first missing
	// required kind — the recognized refusal prefix the execute stage
	// treats as a skip reason.
	stubPostureReadiness(t, []work.DelegateHarnessReadinessEvidence{
		{Kind: "executable", Ref: "lane-tuple://claude_minimax"},
	})
	defect4LaunchSeed(t, store, workID, "actor:posture-revision-refused", now)

	rattID, launchErr := LaunchViaActuator(store, workID, "claude_minimax", "claude_minimax", "posture revision refused", "actor:posture-revision-refused", now)
	if launchErr == nil {
		t.Fatalf("LaunchViaActuator returned nil error; the wire MUST refuse when the readiness evidence does not carry any of the four posture-accepted kinds")
	}
	if !strings.HasPrefix(launchErr.Error(), "lane_unready:") {
		t.Fatalf("launch error = %v, want lane_unready:<detail>; the wire's refusal must carry the recognized `lane_unready:` prefix the execute stage treats as a skip reason (not a hard launch failure)", launchErr)
	}
	if !strings.Contains(launchErr.Error(), "required") {
		t.Fatalf("launch error = %v, want it to name the offending evidence kind (a required posture kind is missing) so the operator can see what the readiness guard failed to attest", launchErr)
	}
	if adapter.calls != 0 {
		t.Fatalf("adapter.calls = %d, want 0; the wire must refuse BEFORE invoking the adapter so no RunAttempt is left in `running`", adapter.calls)
	}
	if rattID != "" {
		t.Fatalf("rattID = %q, want empty (no attempt was minted when the wire refused)", rattID)
	}
	after, err := store.Get(workID)
	if err != nil {
		t.Fatalf("get work: %v", err)
	}
	if after.State != "running" {
		t.Fatalf("envelope state = %q, want running (the wire's lane_unready: refusal must NOT close the envelope; the stage records the skip and the next Continue retries)", after.State)
	}
}

// removeManifest deletes the seeded model-capabilities manifest so the
// wire cannot derive a fallback revision from the manifest snapshot
// either. The helper is scoped to the fixture home so unrelated state is
// untouched.
func removeManifest(t *testing.T, homePath string) error {
	t.Helper()
	return os.Remove(filepath.Join(homePath, "registries", "model-capabilities.yaml"))
}
