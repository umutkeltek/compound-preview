package executewire

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/harness"
	"compound/internal/work"
)

// work_r6j_readiness_tuple_20260911 — the seam's two readiness call sites
// (PreflightViaActuator and LaunchViaActuator) used to fill
// RequestedModelID/RequestedProviderRoute/Command with the lane id and the
// empty string, so a tuple-backed Claude lane always failed the readiness
// guard's resolved-vs-expected comparison (adapter_preflight: lane tuple
// changed during readiness evaluation). The interactive delegate path
// already calls work.DelegateHarnessResolver; this test package reuses it
// read-only from the seam via resolveLaneLaunchTuple so the request the
// guard sees carries the resolved tuple, not the lane id.

// seedExecuteStageManifestWithTuple is the manifest shape a tuple-backed
// Claude lane needs: provider_route + provider_route_kind + aliases, plus
// the capabilities and routing_defaults the seam's route gate (and the
// model-capabilities validator) require. Existing executewire tests use the
// bare seedExecuteStageManifest; the new readiness-tuple tests need the
// tuple fields so resolveLaneLaunchTuple returns a non-trivial resolution.
func seedExecuteStageManifestWithTuple(t *testing.T, homePath, modelID, harnessName, providerRoute string, aliases []string) {
	t.Helper()
	aliasYAML := ""
	for _, alias := range aliases {
		aliasYAML += "      - " + alias + "\n"
	}
	body := "version: 1\n" +
		"purpose: tuple-backed readiness fixture\n" +
		"capability_classes:\n" +
		"  - work.continue.unattended\n" +
		"models:\n" +
		"  " + modelID + ":\n" +
		"    lane: " + modelID + "\n" +
		"    harness: " + harnessName + "\n" +
		"    provider_route: " + providerRoute + "\n" +
		"    provider_route_kind: provider_class\n" +
		"    tier: cheap\n" +
		"    packet_ceiling: multi-file\n" +
		"    aliases:\n" +
		aliasYAML +
		"    capabilities:\n" +
		"      work.continue.unattended:\n" +
		"        level: capable\n" +
		"        evidence: tuple-backed readiness fixture\n" +
		"routing_defaults:\n" +
		"  work.continue.unattended:\n" +
		"    - " + modelID + "\n"
	dir := filepath.Join(homePath, "registries")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "model-capabilities.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedTupleReadyWork seeds a ready envelope with git_delivery rooted at
// targetRoot. The launch wire's workspace resolution reads
// git_delivery.target_worktree_root, so a preflight test must seed it.
func seedTupleReadyWork(t *testing.T, store *work.Store, id, targetRoot string) {
	t.Helper()
	sha := initGitRepoOrSkip(t, targetRoot)
	if _, err := store.Create(work.CreateInput{
		WorkID:           id,
		Owner:            "orchestrator:claude_minimax",
		Actor:            "claude_code:orchestrator",
		State:            "ready",
		Risk:             "tier1",
		Privacy:          "private",
		SuccessPredicate: "tuple-backed readiness fixture predicate",
		GitDelivery: &envelope.GitDeliveryContract{
			TargetWorktreeRoot: targetRoot,
			TargetLocalRef:     "refs/heads/main",
			ExpectedBaseSHA:    sha,
			PublicationMode:    "local_only",
		},
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

// stubLaunchReadinessViaReadOnlyLaunchReadiness pins the seam's
// LaunchReadiness to call the real ReadOnlyLaunchReadiness directly so the
// new tests exercise the resolved-vs-expected comparison in isolation. A
// production readiness guard checks permission, hooks, and instruction
// packs as well — those are unrelated to the tuple fix and would otherwise
// force every readiness-tuple test to seed all of those authorities too.
func stubLaunchReadinessViaReadOnlyLaunchReadiness(t *testing.T, homePath string, captured *atomic.Pointer[work.DelegateHarnessReadinessRequest]) {
	t.Helper()
	prev := LaunchReadiness
	LaunchReadiness = func(req work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
		if captured != nil {
			captured.Store(&req)
		}
		details := harness.ReadOnlyLaunchReadiness(homePath, harness.RunInput{
			WorkID: req.WorkID, Harness: req.Harness, Workspace: req.Workspace,
			Command: req.Command, Args: append([]string(nil), req.Args...), PermissionMode: "plan",
			LaneID: req.LaneID, RequestedModelID: req.RequestedModelID,
			RequestedProviderRoute: req.RequestedProviderRoute, Now: req.Now,
		})
		var readyDetails []string
		if len(details) > 0 {
			readyDetails = details
		}
		return work.DelegateHarnessReadinessResult{Ready: len(details) == 0, Details: readyDetails}, nil
	}
	t.Cleanup(func() { LaunchReadiness = prev })
}

// TestPreflightPassesReadinessForATupleBackedLane (RED on main today:
// `lane_unready:adapter_preflight: lane tuple changed during readiness
// evaluation`. GREEN after fix: PreflightViaActuator returns nil for a
// fixture tuple-backed Claude lane whose manifest carries
// provider_route/aliases, because resolveLaneLaunchTuple reads the same
// Compound model-router authority the adapter reads and the request the
// guard sees now matches the adapter's resolved tuple.)
func TestPreflightPassesReadinessForATupleBackedLane(t *testing.T) {
	homePath := initTestHome(t)
	now := time.Now().UTC()
	seedExecuteStageManifestWithTuple(t, homePath, "claude-minimax (token plan)", "claude_minimax", "minimax", []string{"/bin/echo"})
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	seedTupleReadyWork(t, store, "work_tuple_preflight_ok", homePath)
	stubLaunchReadinessViaReadOnlyLaunchReadiness(t, homePath, nil)

	if err := PreflightViaActuator(store, "work_tuple_preflight_ok", "claude_minimax", now); err != nil {
		t.Fatalf("PreflightViaActuator returned %v; want nil for a tuple-backed lane whose manifest carries provider_route+aliases (the seam must reuse resolveLaneLaunchTuple so the readiness guard's resolved-vs-expected comparison holds)", err)
	}
}

// TestLaunchReadinessRequestCarriesTheResolvedTuple: the seam's
// DelegateHarnessReadinessRequest now carries the resolver's
// {Command, RequestedModelID, RequestedProviderRoute} for a tuple-backed
// lane — NOT the lane id (the original RED shape). The test pins the
// request shape so a future regression to lane-id fields is caught before
// it ever reaches the readiness guard.
func TestLaunchReadinessRequestCarriesTheResolvedTuple(t *testing.T) {
	homePath := initTestHome(t)
	now := time.Now().UTC()
	seedExecuteStageManifestWithTuple(t, homePath, "claude-minimax (token plan)", "claude_minimax", "minimax", []string{"/bin/echo"})
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	seedTupleReadyWork(t, store, "work_tuple_req_carries", homePath)
	var captured atomic.Pointer[work.DelegateHarnessReadinessRequest]
	stubLaunchReadinessViaReadOnlyLaunchReadiness(t, homePath, &captured)

	// Drive PreflightViaActuator through the same path the seam drives
	// during work.Continue; the readiness guard is what the fix targets,
	// and PreflightViaActuator is the most direct way to invoke it
	// without claiming / advancing the envelope.
	if err := PreflightViaActuator(store, "work_tuple_req_carries", "claude_minimax", now); err != nil {
		t.Fatalf("PreflightViaActuator: %v", err)
	}
	got := captured.Load()
	if got == nil {
		t.Fatal("LaunchReadiness was never invoked; the seam did not build a readiness request")
	}
	if got.Harness != "claude_minimax" {
		t.Errorf("readiness harness = %q, want claude_minimax", got.Harness)
	}
	if got.LaneID != "claude_minimax" {
		t.Errorf("readiness lane_id = %q, want claude_minimax", got.LaneID)
	}
	if got.Command != "/bin/echo" {
		t.Errorf("readiness command = %q, want /bin/echo (the tuple-resolved executable); the RED state had Command=\"\"", got.Command)
	}
	if got.RequestedModelID != "claude-minimax (token plan)" {
		t.Errorf("readiness requested_model_id = %q, want \"claude-minimax (token plan)\" (the tuple-resolved model id); the RED state had RequestedModelID=lane (\"claude_minimax\")", got.RequestedModelID)
	}
	if got.RequestedProviderRoute != "minimax" {
		t.Errorf("readiness requested_provider_route = %q, want minimax (the tuple-resolved provider route); the RED state had RequestedProviderRoute=lane (\"claude_minimax\")", got.RequestedProviderRoute)
	}
}

// TestPreflightFallsThroughToLaneForNonTupleBackedLane preserves the
// pre-fix behavior for lanes the resolver treats as not tuple-backed: the
// request the guard sees still carries the lane id as model id and provider
// route, so a raw-claude or codex envelope keeps working unchanged. The
// readiness guard's tuple comparison only runs for tuple-backed
// *ClaudeAdapter instances, so passing the lane id is correct for the
// non-tuple lane family.
//
// Stub DelegateHarnessResolver to a TupleBacked=false resolver so the
// helper exercises the fall-through branch directly — no need to fabricate
// a manifest that would fool the real resolver. The LaunchReadiness stub
// captures the request and returns Ready=true, so the adapter's
// RevalidateGovernedRun never runs.
func TestPreflightFallsThroughToLaneForNonTupleBackedLane(t *testing.T) {
	homePath := initTestHome(t)
	now := time.Now().UTC()
	seedExecuteStageManifest(t, homePath, "capable")
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	seedTupleReadyWork(t, store, "work_tuple_fallthrough", homePath)

	prevResolver := work.DelegateHarnessResolver
	work.DelegateHarnessResolver = func(req work.DelegateHarnessResolveRequest) (work.DelegateHarnessResolution, error) {
		return work.DelegateHarnessResolution{
			Harness:     req.Harness,
			LaneID:      req.Harness,
			TupleBacked: false,
		}, nil
	}
	t.Cleanup(func() { work.DelegateHarnessResolver = prevResolver })

	var capturedLaunch atomic.Pointer[work.DelegateHarnessReadinessRequest]
	prevLaunch := LaunchReadiness
	LaunchReadiness = func(req work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
		capturedLaunch.Store(&req)
		return work.DelegateHarnessReadinessResult{Ready: true}, nil
	}
	t.Cleanup(func() { LaunchReadiness = prevLaunch })

	if err := PreflightViaActuator(store, "work_tuple_fallthrough", "claude_minimax", now); err != nil {
		t.Fatalf("PreflightViaActuator: %v", err)
	}
	got := capturedLaunch.Load()
	if got == nil {
		t.Fatal("LaunchReadiness was never invoked")
	}
	if got.RequestedModelID != "claude_minimax" {
		t.Errorf("readiness requested_model_id = %q, want claude_minimax (lane id pass-through for non-tuple lanes)", got.RequestedModelID)
	}
	if got.RequestedProviderRoute != "claude_minimax" {
		t.Errorf("readiness requested_provider_route = %q, want claude_minimax (lane id pass-through for non-tuple lanes)", got.RequestedProviderRoute)
	}
	if got.Command != "" {
		t.Errorf("readiness command = %q, want empty (the seam does not synthesize a command when the resolver does not)", got.Command)
	}
}

// TestPreflightRefusesLaneUnreadyWhenTupleResolutionMissing: the seam
// must still refuse a lane whose tuple resolution errors out (e.g. the
// manifest lacks provider_route / aliases). The error surfaces through the
// downstream readiness guard, not as a synthesized lane-id request — the
// guard is the authority on tuple viability, and resolveLaneLaunchTuple's
// fall-through is only for the lane-id pass-through, never a silent
// synthetic verdict. This preserves TestContinueLaunchHookRefusesLaneUnready's
// inverse shape: an unready lane MUST refuse.
func TestPreflightRefusesLaneUnreadyWhenTupleResolutionMissing(t *testing.T) {
	homePath := initTestHome(t)
	now := time.Now().UTC()
	// A manifest with no model for `claude_minimax` — the resolver will
	// return "no Compound lane tuple for %q" and the seam will fall through
	// to lane id. The readiness guard then refuses because the resolver
	// surfaced the same error inside ReadOnlyLaunchReadiness.
	manifestBody := "version: 1\n" +
		"purpose: missing-tuple fixture\n" +
		"capability_classes:\n" +
		"  - work.continue.unattended\n" +
		"models: {}\n" +
		"routing_defaults:\n" +
		"  work.continue.unattended: []\n"
	dir := filepath.Join(homePath, "registries")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "model-capabilities.yaml"), []byte(manifestBody), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	seedTupleReadyWork(t, store, "work_tuple_missing", homePath)
	stubLaunchReadinessViaReadOnlyLaunchReadiness(t, homePath, nil)

	preflightErr := PreflightViaActuator(store, "work_tuple_missing", "claude_minimax", now)
	if preflightErr == nil {
		t.Fatalf("PreflightViaActuator returned nil; a lane whose tuple resolution is missing must refuse (got the fix's downstream guard refusal, not a silent pass)")
	}
	if !strings.HasPrefix(preflightErr.Error(), "lane_unready:") {
		t.Fatalf("preflight error = %v, want lane_unready:<detail>", preflightErr)
	}
}
