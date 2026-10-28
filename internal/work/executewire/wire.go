// Package executewire is the composition root for the R3A unattended execute
// stage. internal/work owns the seam and its fail-closed defaults; this
// package binds the package-level hooks to the real resolvers at startup, the
// same pattern internal/work's delegate.go uses for the readiness guard.
// Nesting under internal/work avoids an internal/work -> internal/routeevidence
// import cycle (routeevidence already imports work).
//
// cmd/compound calls Install() from a startup init(); the production wiring
// uses the package-level vars below so tests can swap any single resolver
// without rebuilding the registry.
package executewire

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"compound/internal/actuator"
	"compound/internal/envelope"
	"compound/internal/harness"
	"compound/internal/hookplan"
	"compound/internal/routeevidence"
	autonomyproof "compound/internal/routeevidence/autonomy"
	"compound/internal/runattempt"
	"compound/internal/work"
)

// Install binds the production route / capacity / preflight / launch hooks
// on the internal/work seam. Call exactly once from cmd/compound's startup
// init(). DEFECT 1 fix (AUT-P0-001 2026-09-04): the preflight hook MUST be
// installed before any production caller invokes the seam so a recognized
// refusal (adapter_missing / workspace_unresolvable / lane_unready) leaves
// the envelope byte-identical to before, instead of stranding it in
// `running` with a lease.
func Install() {
	work.SelectedContinueLauncher = LaunchSelectedViaDoor
	work.SwapExecuteStageRoutePlan(RouteFromManifest)
	work.SwapExecuteStageRouteCandidates(RouteCandidatesFromManifest)
	work.SwapExecuteStageCapacity(CapacityFromSnapshot)
	work.SwapExecuteStagePreflight(PreflightViaActuator)
	work.SwapExecuteStageLaunch(LaunchViaActuator)
	work.SwapExecuteStageCandidatePreflight(PreflightCandidateViaActuator)
	work.SwapExecuteStageCandidateLaunch(LaunchCandidateViaActuator)
	// R7C (2026-09-12): bind the execution-selection resolver. R7A shipped the
	// seam with a nil default, so the installed CLI refused every
	// `--launch-profile` while its tests passed against a stub.
	work.DefaultExecutionSelectionResolver = ResolveExecutionSelectionViaOwner
	// The launch stamp reads each harness's live hook posture with the reader
	// `compound doctor` uses (hookplan), so the attempt and doctor agree.
	harness.SwapHookPostureReader(liveHookPosture)
	// Slice 2 (aut-launch-canonical 2026-09-07): mark the seam installed so
	// the launch wire's precondition passes. A process that never ran
	// Install() sees every launch refused with "LaunchContext not
	// installed" before adapter.Run mints a RunAttempt. This does not wire
	// a cancellable context — see the LaunchContext doc for the bound that
	// applies in production and the open follow-up.
	launchContextInstalled.Store(true)
}

// launchSelectionSnapshot is the one snapshot construction preflight and
// launch share: a candidate with no saved selection is a one-run override.
func launchSelectionSnapshot(wenv envelope.WorkEnvelope, candidate work.ExecutionRouteCandidate, route, model, revision string) (runattempt.ExecutionSelectionSnapshot, error) {
	overrideHarness, overrideProfile := "", ""
	if wenv.ExecutionSelection == nil && candidate.SourceRevision != "" {
		overrideHarness, overrideProfile = candidate.Harness, candidate.LaunchProfileID
	}
	return buildSelectionSnapshotFromSaved(wenv, overrideHarness, overrideProfile, route, model, revision, revision)
}

func liveHookPosture(harnessName string) (harness.HookPostureReading, bool) {
	base, err := os.UserHomeDir()
	if err != nil {
		return harness.HookPostureReading{}, false
	}
	live, ok := hookplan.ReadLiveHookPosture(base, harnessName)
	return harness.HookPostureReading{Posture: live.Posture, ConfigPath: live.ConfigPath, ConfigDigest: live.ConfigDigest}, ok
}

func RouteCandidatesFromManifest(homePath, capability string) []work.ExecutionRouteCandidate {
	manifest, _, revision, err := routeevidence.LoadModelCapabilitiesSnapshot(homePath)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []work.ExecutionRouteCandidate
	for _, modelID := range manifest.RoutingDefaults[capability] {
		entry, ok := manifest.Models[modelID]
		if !ok {
			continue
		}
		claim := entry.Capabilities[capability]
		if autonomyproof.GateRole(capability, entry.Tier, strings.TrimSpace(claim.Level)) != "eligible" {
			continue
		}
		h := strings.TrimSpace(entry.Harness)
		profile := strings.TrimSpace(modelID)
		if h == "" || !AdapterKnown(homePath, h) {
			continue
		}
		key := strings.ToLower(strings.ReplaceAll(profile, "-", "_"))
		if seen[key] {
			continue
		}
		seen[key] = true
		lane := strings.TrimSpace(entry.ProviderRoute)
		if lane == "" {
			lane = h
		}
		out = append(out, work.ExecutionRouteCandidate{LaunchProfileID: profile, Harness: h, RequestedModelID: profile, ProviderRoute: strings.TrimSpace(entry.ProviderRoute), SourceRevision: revision, CapacityLane: lane})
	}
	return out
}

// RouteFromManifest resolves the first eligible lane for the capability from
// the home's model-capabilities authority (the route plan the recon named:
// routeevidence.LoadModelCapabilitiesSnapshot). Fail-closed on every input: a
// missing or invalid authority, no routing_defaults entry for the capability,
// a claim whose computed gate role is not "eligible", an entry with no
// harness, or a harness the adapter registry has no executor for all return
// no route.
func RouteFromManifest(homePath, capability string) (string, bool) {
	manifest, _, _, err := routeevidence.LoadModelCapabilitiesSnapshot(homePath)
	if err != nil {
		return "", false
	}
	for _, modelID := range manifest.RoutingDefaults[capability] {
		entry, ok := manifest.Models[modelID]
		if !ok {
			continue
		}
		claim := entry.Capabilities[capability]
		if autonomyproof.GateRole(capability, entry.Tier, strings.TrimSpace(claim.Level)) != "eligible" {
			continue
		}
		harnessName := strings.TrimSpace(entry.Harness)
		if harnessName == "" || !AdapterKnown(homePath, harnessName) {
			continue
		}
		return harnessName, true
	}
	return "", false
}

// AdapterKnown reports whether the harness adapter registry has an executor
// for harnessName. A var so tests can pin the registry lookup without
// building a real one.
var AdapterKnown = func(homePath, harnessName string) bool {
	registry := harness.NewAdapterRegistry(runattempt.Open(homePath))
	_, ok := registry.AdapterByName(harnessName)
	return ok
}

// CapacityConfig builds the reader config for the capacity hook. Production
// uses the operator-owned defaults (CodexBar's usage feed plus the home's own
// projections); tests override the var to point the same reader at fixture
// paths so the operator's real feed can never leak into a test.
var CapacityConfig = routeevidence.DefaultLaneCapacityConfig

// CapacityFromSnapshot reads the lane's capacity through the real routeevidence
// reader (the codexbar-fed snapshot, merged with Compound's own supplement
// feed) and translates it into the seam's (usedPercent, fresh) contract.
// Fail-closed: a read error propagates, and an unknown state or a missing/zero
// observation reports fresh=false — the seam refuses on that signal
// regardless of the percentage, never skipping the check.
func CapacityFromSnapshot(homePath, lane string, now time.Time) (float64, bool, error) {
	capacity, err := routeevidence.ReadLaneCapacityWithConfig(lane, now, CapacityConfig(homePath))
	if err != nil {
		return 0, false, err
	}
	used := 100 - capacity.Remaining
	if used < 0 {
		used = 0
	}
	fresh := capacity.State != routeevidence.CapacityUnknown &&
		work.ExecuteStageCapacityFreshness(capacity.LastProven, now)
	return used, fresh, nil
}

// AdapterFor returns the registered adapter for harnessName, same registry
// `work_advance_execute.go` uses; tests pin via this var.
var AdapterFor = func(homePath, harnessName string) (harness.Adapter, bool) {
	registry := harness.NewAdapterRegistry(runattempt.Open(homePath))
	adapter, ok := registry.AdapterByName(harnessName)
	return adapter, ok
}

// LaunchReadiness is the readiness guard the launch wire calls before
// handing the envelope to the adapter. Tests override the var to inject a
// controlled verdict. Packet deliverable 4: the launch MUST refuse (not
// silently plan) when the lane's readiness is unmet; an unconfigured guard
// is the most unmet a guard can be.
var LaunchReadiness = func(req work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
	if work.DelegateHarnessReadiness == nil {
		return work.DelegateHarnessReadinessResult{Ready: false, Details: []string{"delegate readiness guard not installed"}}, nil
	}
	return work.DelegateHarnessReadiness(req)
}

// LaunchContext is the context the launch wire forwards to adapter.Run.
// Production lazily derives cancellation from SIGINT/SIGTERM; the runner
// terminates its subprocess group and records cancellation before the work
// owner finalizes. RunInput.Timeout supplies the separate execution deadline.
// The Install flag proves only that the production seam was bound, not that
// a caller supplied a per-tick context. Tests replacing LaunchContext must
// also set that flag (or call Install) and restore their context afterward.
var LaunchContext = signalLaunchContext

// InstallLaunchContext binds ctx as the context every subsequent launch
// forwards to adapter.Run, and returns a restore func.
//
// This is the TEST seam, not the production wiring — production gets a real
// cancellable context from the default (signalLaunchContext) with no caller
// cooperation, precisely because the previous design required cooperation
// and never got it. Tests use this to drive a context they can cancel and
// observe the runner persisting "cancel:context_cancelled" evidence.
func InstallLaunchContext(ctx context.Context) func() {
	prev := LaunchContext
	LaunchContext = func() context.Context { return ctx }
	return func() { LaunchContext = prev }
}

// launchContextInstalled is flipped to true by Install when the
// production LaunchContext wiring is in place. The launch wire
// enforces it as a fail-closed precondition (see LaunchContext for what
// it does and does not prove). Tests can pin it via
// SetLaunchContextInstalled to exercise the wire without re-running
// Install.
var launchContextInstalled atomic.Bool

// SetLaunchContextInstalled lets tests install (or uninstall) the
// LaunchContext gate without re-running Install(). Production code
// MUST NOT call this.
func SetLaunchContextInstalled(installed bool) {
	launchContextInstalled.Store(installed)
}

// AssertLaunchContextInstalled is the precondition check LaunchViaActuator
// calls before any adapter.Run. It returns nil exactly when the
// LaunchContext wiring is live (Install was called, or a test pinned
// the var AND set the flag). A production caller that forgets Install
// sees every launch refused with an explicit error before adapter.Run
// mints a RunAttempt, instead of silently running with
// context.Background().
func AssertLaunchContextInstalled() error {
	if !launchContextInstalled.Load() {
		return errors.New("execute stage launch: LaunchContext not installed (run executewire.Install before launch)")
	}
	return nil
}

// DefaultLaunchTimeout is the finite execution budget LaunchViaActuator
// applies to every unattended harness launch when the caller did not pin
// RunInput.Timeout. DEFECT 4 fix (AUT-P0-004 2026-09-04).
//
// Justification: the heartbeat running the unattended launch has an outer
// `timeout 600` (10 minutes); the internal deadline MUST be safely below
// that with margin to persist timeout evidence (a terminal
// terminalize-Complete cycle, a SetProofRefs call, and the
// SetTranscript write all need to land before the heartbeat SIGKILLs the
// Compound parent). 5 minutes (300s) is comfortably under 600s and above
// the heaviest observed legacy session, so a productive unattended
// launch completes inside the budget and a runaway launch is bounded.
// It is also what `work advance --execute` uses when --timeout is absent
// or zero; see LaunchTimeoutFor.
var DefaultLaunchTimeout = 5 * time.Minute

// MaxLaunchTimeout caps an explicitly requested budget
// (`work advance --execute --timeout`). It equals the detached door's
// wrapper budget: no governed run may be asked to outlive the outermost
// bound Compound itself puts around a door.
var MaxLaunchTimeout = DefaultDetachedLaunchBudget

// LaunchTimeoutFor resolves one governed run's execution budget: a
// non-positive request keeps DefaultLaunchTimeout, a request above
// MaxLaunchTimeout is capped to it, and anything else is honoured.
func LaunchTimeoutFor(requested time.Duration) time.Duration {
	if requested <= 0 {
		return DefaultLaunchTimeout
	}
	if requested > MaxLaunchTimeout {
		return MaxLaunchTimeout
	}
	return requested
}

// ParseLaunchTimeoutFlag resolves the `work advance --execute --timeout`
// value through LaunchTimeoutFor. Parsing lives here, not in cmd/compound,
// so the CLI keeps one call. A malformed or negative value is a usage error
// the caller reports before any state change.
func ParseLaunchTimeoutFlag(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return LaunchTimeoutFor(0), nil
	}
	requested, err := time.ParseDuration(raw)
	if err != nil || requested < 0 {
		return 0, fmt.Errorf("--timeout %q must be a non-negative duration such as 40m (absent or 0 = %s, capped at %s)", raw, DefaultLaunchTimeout, MaxLaunchTimeout)
	}
	return LaunchTimeoutFor(requested), nil
}

// PreflightViaActuator is the production preflight hook the seam calls
// BEFORE Claim/Advance (DEFECT 1 fix, AUT-P0-001 2026-09-04). It resolves
// every prerequisite that can fail before governed work enters running:
// adapter enrollment, workspace resolvability, and lane readiness. The
// returned error MUST carry a recognized skip prefix (adapter_missing:,
// workspace_unresolvable:, lane_unready:) so the seam can surface the
// refusal as a preflight refusal without any state mutation. The launch
// wire (LaunchViaActuator) re-resolves the same values when it runs; the
// preflight is read-only and runs against the envelope's pre-claim state.
//
// Wire contract: this function never mints a RunAttempt, never mutates the
// envelope, never grants a lease, and never advances any state. A bare
// error (infra failure inside the preflight itself) is also reported
// without mutation — the seam fails closed.
func PreflightViaActuator(store *work.Store, workID, lane string, now time.Time) error {
	if store == nil {
		return errors.New("execute stage preflight: nil work store")
	}
	return PreflightCandidateViaActuator(store, workID, legacyRouteCandidate(store.HomePath(), lane, lane, now), now)
}

func PreflightCandidateViaActuator(store *work.Store, workID string, candidate work.ExecutionRouteCandidate, now time.Time) error {
	if store == nil {
		return errors.New("execute stage preflight: nil work store")
	}
	workID = strings.TrimSpace(workID)
	candidate.LaunchProfileID = strings.TrimSpace(candidate.LaunchProfileID)
	candidate.Harness = strings.TrimSpace(candidate.Harness)
	if workID == "" || candidate.LaunchProfileID == "" || candidate.Harness == "" {
		return fmt.Errorf("execute stage preflight: workID=%q candidate=%+v; required", workID, candidate)
	}
	homePath := store.HomePath()

	// 1. Resolve adapter enrollment. A missing adapter means the route
	// plan names a lane the harness registry cannot drive — refuse with
	// adapter_missing:<lane>.
	_, ok := AdapterFor(homePath, candidate.Harness)
	if !ok {
		return errors.New("adapter_missing:" + candidate.Harness)
	}

	// 2. Resolve workspace from the envelope's git_delivery. The
	// production launch wire reads the same field; doing the check here
	// lets a missing-git_delivery envelope surface as a refusal BEFORE
	// the seam advances the envelope to running.
	wenv, err := store.Get(workID)
	if err != nil {
		return fmt.Errorf("workspace_unresolvable:load envelope %s: %w", workID, err)
	}
	workspace := ""
	if wenv.GitDelivery != nil {
		workspace = strings.TrimSpace(wenv.GitDelivery.TargetWorktreeRoot)
	}
	if workspace == "" {
		return errors.New("workspace_unresolvable:envelope " + workID + " has no git_delivery.target_worktree_root")
	}

	// 3. Run the lane readiness check. The launch wire runs the same
	// guard, but doing it here lets the seam refuse without ever
	// claiming the envelope. For tuple-backed Claude lanes the guard's
	// resolved-vs-expected comparison (internal/harness/registry.go
	// ReadOnlyLaunchReadiness) requires RequestedModelID /
	// RequestedProviderRoute / Command to match the values the adapter's
	// RevalidateGovernedRun will resolve; resolveLaneLaunchTuple reads
	// the same Compound model-router authority the adapter reads, so
	// passing its output here makes the comparison hold. Non-tuple
	// lanes fall through to the lane id, exactly as before.
	tupleCommand, tupleModelID, tupleProviderRoute, tupleProfileRevision, _ := resolveLaneLaunchTuple(homePath, candidate.LaunchProfileID, now)
	if candidate.SourceRevision != "" {
		resolved, resolveErr := resolveExecutionRouteCandidate(homePath, candidate, now)
		if resolveErr != nil {
			return fmt.Errorf("lane_unready:%v", resolveErr)
		}
		tupleCommand, tupleModelID, tupleProviderRoute = resolved, candidate.RequestedModelID, candidate.ProviderRoute
	}
	if tupleModelID == "" {
		tupleModelID = candidate.RequestedModelID
	}
	if tupleProviderRoute == "" {
		tupleProviderRoute = candidate.ProviderRoute
	}
	readinessReq := work.DelegateHarnessReadinessRequest{
		HomePath: homePath, HarnessHome: "", ContextPackID: wenv.IDs.ContextPackID, LaunchProfileID: candidate.LaunchProfileID,
		WorkID: workID, Workspace: workspace, Harness: candidate.Harness, LaneID: candidate.LaunchProfileID,
		Command: tupleCommand, Args: []string{},
		RequestedModelID: tupleModelID, RequestedProviderRoute: tupleProviderRoute, Now: now,
	}
	if err := validateSavedSelectionTuple(wenv, candidate, now); err != nil {
		return fmt.Errorf("lane_unready:%v", err)
	}
	// A selection the harness cannot apply refuses here, before the seam claims.
	if snap, err := launchSelectionSnapshot(wenv, candidate, tupleProviderRoute, tupleModelID, tupleProfileRevision); err == nil {
		if err := harness.CheckSelectionApplicable(candidate.Harness, &snap); err != nil {
			return err
		}
	}
	readiness, readinessErr := LaunchReadiness(readinessReq)
	if readinessErr != nil {
		return fmt.Errorf("execute stage preflight: readiness check errored: %w", readinessErr)
	}
	if !readiness.Ready {
		detail := strings.Join(readiness.Details, "; ")
		if detail == "" {
			detail = "delegate readiness reported not-ready without details"
		}
		return errors.New("lane_unready:" + detail)
	}
	return nil
}

// postureEvidenceRevisionFallback is the revision the launch wire stamps
// onto posture evidence items the readiness guard produced WITHOUT a
// Revision. Today the only kind that lacks a Revision in production is the
// `executable` item cmd/compound/work_delegate_readiness.go builds — its
// Ref is the lane tuple URL but the Revision field is left empty because
// the readiness evaluator has no canonical sha256 to point at. The wire
// fills the gap with the lane tuple's profile revision (the model-
// capabilities manifest snapshot sha256 the tuple resolver just consulted),
// so runattempt.SetExecutionPosture accepts the item and the launch
// reaches adapter.Run.
//
// work_r6m_seam_posture_20260911.
type postureEvidenceRevisionFallback string

// fillPostureEvidenceRevisions filters readiness evidence to the kinds
// runattempt.SetExecutionPosture accepts for a mutation-capable posture
// (the four required kinds below), stamps a Revision onto every item
// that lacks one (using profileRevision as the canonical source), and
// refuses with a recognized `lane_unready:` prefix when a required kind
// is missing or when no profile revision is available to fill one. The
// remaining evidence kinds (executable, activation_argv, instruction_pack,
// context_pack, effective_contract, lane_capacity, …) stay in the
// readiness snapshot for the DelegateBinding view — they are NOT in the
// runattempt posture, but they ARE in the readiness record the operator
// reads.
//
// Why filter rather than stamp: runattempt.SetExecutionPosture's validator
// refuses mutation-capable postures that carry evidence kinds outside
// the four required kinds. The production readiness guard produces the
// four required kinds AND several supporting kinds (the `executable` item
// is the live-run example: Ref set, Revision empty). Stamping every item
// verbatim would push the validator's failure mode from
// "evidence requires kind, ref, revision" (live-run shape) to
// "unexpected authority evidence kind <kind>" (the second validator check).
// Filtering to the four required kinds BEFORE the validator runs is the
// only way to satisfy the validator without weakening it (the fence
// forbids touching internal/runattempt/**).
//
// work_r6m_seam_posture_20260911.
func fillPostureEvidenceRevisions(evidence []work.DelegateHarnessReadinessEvidence, profileRevision string) ([]work.DelegateHarnessReadinessEvidence, error) {
	if len(evidence) == 0 {
		// A mutation-capable posture requires ALL four kinds. Empty
		// readiness evidence means none of them are present — refuse
		// with the recognized lane_unready prefix so the execute stage
		// treats this as a skip reason, not a hard launch failure.
		return nil, errors.New("lane_unready:posture evidence is empty (no readiness evidence was attached)")
	}
	out := make([]work.DelegateHarnessReadinessEvidence, 0, len(postureEvidenceAcceptedKinds))
	seen := make(map[string]bool, len(postureEvidenceAcceptedKinds))
	for _, item := range evidence {
		if !postureEvidenceAcceptedKinds[item.Kind] {
			// Supporting-kind evidence (executable, activation_argv,
			// instruction_pack, context_pack, effective_contract,
			// lane_capacity, …) stays in the readiness snapshot for
			// the DelegateBinding view. It is NOT stamped onto the
			// runattempt posture because the validator rejects
			// mutation-capable postures carrying it. NOT a silent
			// drop — the readiness record carries it.
			continue
		}
		copy := item
		if strings.TrimSpace(copy.Revision) == "" {
			// No revision: try to fill from the lane tuple's profile
			// revision. An empty profileRevision means the resolver
			// AND the manifest are both unavailable, so the wire
			// cannot give this item a revision at all. Surface the
			// refusal rather than stamp a synthetic value — the
			// runattempt posture validator already proved it refuses
			// any item lacking kind/ref/revision (live-run evidence:
			// 2026-09-11T03:41:44Z).
			if strings.TrimSpace(profileRevision) == "" {
				return nil, fmt.Errorf("lane_unready:posture evidence %q has no revision and no lane tuple profile revision is available to fill it", copy.Kind)
			}
			copy.Revision = profileRevision
		}
		out = append(out, copy)
		seen[copy.Kind] = true
	}
	// Mutation-capable postures require ALL four kinds. Missing any one
	// means the readiness guard did not attest the lane — refuse with
	// the recognized prefix so the execute stage treats this as a skip.
	for _, required := range []string{"permission_authority", "permission_posture", "blocking_hook_plan", "blocking_hook_installation"} {
		if !seen[required] {
			return nil, fmt.Errorf("lane_unready:posture evidence %q is required (the readiness guard did not attest %s for this lane)", required, required)
		}
	}
	return out, nil
}

// postureEvidenceAcceptedKinds is the set of evidence kinds the runattempt
// posture validator accepts for a mutation-capable posture. The four
// required kinds in internal/runattempt/runattempt.go:SetExecutionPosture
// are the authoritative allow-list; this constant mirrors that allow-list
// so the wire filters supporting kinds (executable, activation_argv, …)
// before the validator runs. Mirrored rather than imported because the
// runattempt package is internal and exposes no canonical name; a drift
// here surfaces as the validator refusing a kind this list accepted.
//
// work_r6m_seam_posture_20260911.
var postureEvidenceAcceptedKinds = map[string]bool{
	"permission_authority":       true,
	"permission_posture":         true,
	"blocking_hook_plan":         true,
	"blocking_hook_installation": true,
}

// LaunchViaActuator is the production launch hook. It runs the SAME machinery
// `compound work advance --execute` uses (harness.Run via the registered
// adapter, then FinalizeGovernedHarnessRun to advance the envelope from
// running to verifying), passing --permission-mode bypass so the unattended
// envelope drives code-editing lanes rather than the safe plan default. A
// lane whose readiness guard reports not-ready is refused with
// skipped=lane_unready:<detail> rather than silently planned. The seam has
// already moved the envelope from ready → claimed → running; this function
// takes the running envelope, runs the adapter, and returns the RunAttempt
// id the adapter minted.
//
// DEFECT 2 fix (AUT-P0-002 2026-09-04): the actor parameter is the claimant
// and authority holder for this launch. FinalizeGovernedHarnessRun MUST use
// the actor (NOT the lane) as the Finalization Actor — leases compare exact
// strings, and the heartbeat's --actor (e.g. "claude-minimax" with a hyphen)
// is distinct from the canonical lane (e.g. "claude_minimax" with an
// underscore). Lane and harness are execution attribution only.
func LaunchViaActuator(store *work.Store, workID, lane, harnessName, prompt, actor string, now time.Time) (string, error) {
	if store == nil {
		return "", errors.New("execute stage launch: nil work store")
	}
	return LaunchCandidateViaActuator(store, workID, legacyRouteCandidate(store.HomePath(), lane, harnessName, now), prompt, actor, now)
}

func LaunchCandidateViaActuator(store *work.Store, workID string, candidate work.ExecutionRouteCandidate, prompt, actor string, now time.Time) (string, error) {
	if store == nil {
		return "", errors.New("execute stage launch: nil work store")
	}
	workID = strings.TrimSpace(workID)
	candidate.Harness = strings.TrimSpace(candidate.Harness)
	if workID == "" || candidate.Harness == "" {
		return "", fmt.Errorf("execute stage launch: workID=%q harness=%q; both required", workID, candidate.Harness)
	}
	homePath := store.HomePath()
	adapter, ok := AdapterFor(homePath, candidate.Harness)
	if !ok {
		return "", errors.New("adapter_missing:" + candidate.Harness)
	}
	wenv, err := store.Get(workID)
	if err != nil {
		return "", fmt.Errorf("execute stage launch: load envelope %s: %w", workID, err)
	}
	if wenv.State != "running" {
		return "", fmt.Errorf("execute stage launch: envelope %s is %s, expected running", workID, wenv.State)
	}
	workspace := ""
	if wenv.GitDelivery != nil {
		workspace = strings.TrimSpace(wenv.GitDelivery.TargetWorktreeRoot)
	}
	if workspace == "" {
		return "", fmt.Errorf("workspace_unresolvable:envelope %s has no git_delivery.target_worktree_root", workID)
	}
	// Tuple resolution mirrors the preflight hook's call: the readiness
	// guard's resolved-vs-expected comparison requires the request to
	// carry the adapter-resolved {Command, RequestedModelID,
	// RequestedProviderRoute}, not the lane id. resolveLaneLaunchTuple
	// reuses the same Compound model-router authority the adapter reads.
	// work_r6m_seam_posture_20260911: also capture the lane tuple's profile
	// revision so the wire can stamp a real Revision onto posture evidence
	// items the readiness guard produced WITHOUT one (the production
	// `executable` item — Ref set, Revision empty). The profile revision is
	// the model-capabilities manifest sha256 the resolver consulted.
	tupleCommand, tupleModelID, tupleProviderRoute, tupleProfileRevision, _ := resolveLaneLaunchTuple(homePath, candidate.LaunchProfileID, now)
	if candidate.SourceRevision != "" {
		resolved, resolveErr := resolveExecutionRouteCandidate(homePath, candidate, now)
		if resolveErr != nil {
			return "", fmt.Errorf("lane_unready:%v", resolveErr)
		}
		tupleCommand, tupleModelID, tupleProviderRoute = resolved, candidate.RequestedModelID, candidate.ProviderRoute
	}
	if err := validateSavedSelectionTuple(wenv, candidate, now); err != nil {
		return "", fmt.Errorf("lane_unready:%v", err)
	}
	if tupleModelID == "" {
		tupleModelID = candidate.LaunchProfileID
	}
	if tupleProviderRoute == "" {
		tupleProviderRoute = candidate.LaunchProfileID
	}
	readinessReq := work.DelegateHarnessReadinessRequest{
		HomePath: homePath, HarnessHome: "", ContextPackID: wenv.IDs.ContextPackID, LaunchProfileID: candidate.LaunchProfileID,
		WorkID: workID, Workspace: workspace, Harness: candidate.Harness, LaneID: candidate.LaunchProfileID,
		Command: tupleCommand, Args: []string{prompt},
		RequestedModelID: tupleModelID, RequestedProviderRoute: tupleProviderRoute, Now: now,
	}
	readiness, readinessErr := LaunchReadiness(readinessReq)
	if readinessErr != nil {
		return "", fmt.Errorf("execute stage launch: readiness check errored: %w", readinessErr)
	}
	if !readiness.Ready {
		detail := strings.Join(readiness.Details, "; ")
		if detail == "" {
			detail = "delegate readiness reported not-ready without details"
		}
		return "", fmt.Errorf("lane_unready:%s", detail)
	}
	// work_r7a_execution_selection_20260911: the per-run selection snapshot
	// (what this launch was asked to run, with honest applied-effort +
	// native-session evidence) is built here and stamped by the shared runner
	// BEFORE the subprocess starts. A request the harness cannot apply is
	// refused now, before any launch authority is minted (fail-closed 2026-10-06).
	snapshot, snapshotErr := launchSelectionSnapshot(wenv, candidate, tupleProviderRoute, tupleModelID, tupleProfileRevision)
	if snapshotErr != nil {
		return "", fmt.Errorf("execute stage launch: build selection snapshot: %w", snapshotErr)
	}
	if err := harness.CheckSelectionApplicable(candidate.Harness, &snapshot); err != nil {
		return "", err
	}
	// DEFECT 3 fix (AUT-P0-003 2026-09-04): mint a fresh
	// (ActionIntent, PolicyDecision) pair bound to THIS launch and stamp
	// it onto the running reservation BEFORE the adapter mints its
	// RunAttempt. The wire pass the new ActionIntentID on
	// RunInput.ActionIntentID so the subprocess runner's
	// MintWithActionIntent carries a non-empty authority link, and feeds
	// the same ids into FinalizeGovernedHarnessRun as the
	// ExpectedActionIntentID / ExpectedPolicyDecisionID the verifying
	// transition compares against.
	launchActionID, launchPolicyID, mintErr := store.MintLaunchAuthority(workID, actor, candidate.LaunchProfileID, prompt, now)
	if mintErr != nil {
		return "", fmt.Errorf("execute stage launch: mint launch authority: %w", mintErr)
	}
	if strings.TrimSpace(launchActionID) == "" || strings.TrimSpace(launchPolicyID) == "" {
		return "", fmt.Errorf("execute stage launch: MintLaunchAuthority returned empty ids (action=%q policy=%q)", launchActionID, launchPolicyID)
	}
	// Slice 2 (aut-launch-canonical 2026-09-07): precondition gate.
	// LaunchContext MUST be installed before adapter.Run is permitted to
	// mint a RunAttempt. Production calls Install() at startup; tests
	// pin the var directly and SetLaunchContextInstalled(true). A
	// production caller that forgets Install() now sees every launch
	// refuse with an explicit error before any subprocess is started.
	if err := AssertLaunchContextInstalled(); err != nil {
		return "", err
	}
	// M5 2026-09-03: --permission-mode bypass is the explicit operator
	// override the standing grant authorizes; the seam never synthesizes
	// bypass on its own.
	runInput := harness.RunInput{
		WorkID: workID, Harness: candidate.Harness, Workspace: workspace,
		RequestedModelID: tupleModelID, RequestedProviderRoute: tupleProviderRoute,
		Args: []string{prompt}, PermissionMode: "bypass",
		LaneID: candidate.LaunchProfileID, ActionIntentID: launchActionID,
		Timeout: LaunchTimeoutFor(0),
		Now:     now,
	}
	runInput.ExecutionSelectionSnapshot = &snapshot
	runInput.LaunchProfileID = snapshot.LaunchProfileID
	// Slice 2 (aut-launch-canonical 2026-09-07): bind the canonical
	// execution-reservation StartGuard so adapter.Run's subprocess
	// start is gated by the live running-reservation identity. A
	// caller that bypasses the guard (by mutating the wenv between
	// MintLaunchAuthority and FinalizeGovernedHarnessRun) surfaces
	// as a StartGuard refusal; the subprocess never starts. The
	// production wire has always had this on the cmd/compound seam
	// (PrepareGovernedHarnessLaunch sets it); the unattended execute
	// seam previously built RunInput without it.
	runInput.StartGuard = actuator.NewGovernedExecutionReservationStartGuard(
		homePath, workID, launchPolicyID, launchActionID,
	)
	if candidate.SourceRevision != "" {
		reservationGuard := runInput.StartGuard
		runInput.StartGuard = func(ctx context.Context, start func() error) error {
			return reservationGuard(ctx, func() error {
				command, err := resolveExecutionRouteCandidate(homePath, candidate, time.Now().UTC())
				if err != nil {
					return fmt.Errorf("execution tuple changed before launch: %w", err)
				}
				if command != tupleCommand {
					return errors.New("execution tuple executable changed before launch")
				}
				return start()
			})
		}
	}
	// Slice 2 (aut-launch-canonical 2026-09-07): mirror
	// PrepareGovernedHarnessLaunch. Claude-family adapters carry the launch
	// generation on the attempt itself: the shared runner persists
	// RunInput.ExecutionPosture onto the RunAttempt, and the close-time
	// authority gate reads the (ActionIntent, PolicyDecision) pair off the
	// attempt. The posture carries the readiness evidence the lane attested;
	// the runner refuses a mutation-capable posture that lacks the required
	// evidence kinds, so an unattested bypass fails closed at mint and the
	// envelope closes as failed instead of launching. Every other adapter
	// carries only the ActionIntent and binds to the generation through the
	// sealed same-state event MintLaunchAuthority records.
	if _, claudeFamily := adapter.(harness.GovernedPermissionAdapter); claudeFamily {
		// work_r6m_seam_posture_20260911: runattempt.SetExecutionPosture refuses
		// any evidence item lacking kind/ref/revision. The production
		// readiness guard produces at least one item (the `executable` kind
		// in cmd/compound/work_delegate_readiness.go) WITHOUT a Revision —
		// the evaluator has no canonical sha256 to point at. The wire fills
		// the gap with the lane tuple's profile revision (the model-
		// capabilities manifest sha256 resolveLaneLaunchTuple just consulted)
		// so the stamp succeeds and the launch reaches adapter.Run. Items
		// that still cannot be given a revision (no resolver, no manifest)
		// surface as `lane_unready:` — the recognized refusal prefix the
		// execute stage treats as a skip reason, not a hard launch failure.
		filledEvidence, fillErr := fillPostureEvidenceRevisions(readiness.Evidence, tupleProfileRevision)
		if fillErr != nil {
			// Refuse with the recognized `lane_unready:` prefix. The execute
			// stage's parseLaunchRefusal will surface this as a skip reason
			// rather than a Close (the pre-fix behavior tried to Close and
			// reported the refusal as the failure).
			return "", fillErr
		}
		posture := &runattempt.ExecutionPosture{
			PermissionMode:   runInput.PermissionMode,
			PolicyDecisionID: launchPolicyID,
		}
		for _, item := range filledEvidence {
			posture.AuthorityEvidence = append(posture.AuthorityEvidence, runattempt.ExecutionPostureEvidence{
				Kind: item.Kind, Ref: item.Ref, Revision: item.Revision,
			})
		}
		runInput.ExecutionPosture = posture
	}
	// DEFECT 4 fix (AUT-P0-004 2026-09-04): forward the live, cancellable
	// context so a stuck subprocess is killed at the deadline and a
	// cancelled heartbeat kills the tree. The shared runner honors
	// ctx.Done() via a process-group kill (killProcessTree), so an
	// orphaned grandchild holding the capture pipes open cannot survive.
	// Production installs a signal-derived cancellable ctx at the command
	// boundary (InstallLaunchContext); tests swap the var to prove the
	// cancellation path persists "cancel:context_cancelled" evidence.
	ctx := LaunchContext()
	out, runErr := adapter.Run(ctx, runInput)
	// Slice 2 (aut-launch-canonical 2026-09-07) layered onto the base
	// (R6A 2026-09-10): the canonical finalize path now runs UNCONDITIONALLY.
	// Out + runErr are both forwarded to FinalizeGovernedHarnessRun, which
	// decides whether to advance the envelope to verifying or close it as
	// failed based on the actual RunAttempt status. The base's wire returned
	// strings.TrimSpace(out.RunAttempt.ID) alongside runErr so callers
	// (notably execute_stage.launchStub) could record the partial-evidence
	// ratt id in result.RunAttemptIDs; the packet pins that contract, so on
	// a finalize-failed or runErr-nonzero path the wire still returns
	// (strings.TrimSpace(out.RunAttempt.ID), runErr) instead of ("", err).
	// A missing RunAttempt.ID (adapter short-circuited before minting) is
	// still an explicit refuse, since there is no id to name and no
	// canonical finalize to route through.

	if strings.TrimSpace(out.RunAttempt.ID) == "" {
		if runErr != nil { // a pre-mint refusal (e.g. selection_unsupported) keeps its reason
			return "", fmt.Errorf("execute stage launch: adapter returned empty RunAttempt.ID: %w", runErr)
		}
		return "", errors.New("execute stage launch: adapter returned empty RunAttempt.ID")
	}
	// R6A 2026-09-10: capture the minted ratt id once so every return path
	// below (finalize-refused, adapter-errored, success) names the same
	// record. Callers (notably execute_stage.launchStub) record this in
	// result.RunAttemptIDs even on failure so the partial-evidence id is
	// never anonymous.
	mintedRattID := strings.TrimSpace(out.RunAttempt.ID)
	// FinalizeGovernedHarnessRun advances running → verifying (or Close
	// failed on a non-success RunAttempt.Status). The seam's own
	// Advance(verifying) after launchStub returns is now a no-op on the
	// production path; tests that stub launchStub still move the
	// envelope the way the seam's pre-Advance(running) set up. The
	// RunningActionIntentID/RunningPolicyID the verifying transition
	// compares against are the freshly minted launch authority, not
	// whatever older ids happened to be on the envelope at load-time —
	// the spec's REQUIRED CHAIN closes here with an exact match.
	// ExistingRunAttemptIDs carries the prior reservation's RunAttempt
	// ids so the verifying transition sees the chain (the new RunAttempt
	// id joins that chain via FinalizeGovernedHarnessRun, not via a
	// separate write path).
	if _, finalizeErr := actuator.FinalizeGovernedHarnessRun(actuator.GovernedHarnessFinalizationInput{
		Store:                 store,
		WorkID:                workID,
		Actor:                 actor,
		HarnessName:           candidate.Harness,
		RunningPolicyID:       launchPolicyID,
		RunningActionIntentID: launchActionID,
		ExistingRunAttemptIDs: wenv.RunAttemptIDs,
		Output:                out,
		RunError:              runErr,
		Now:                   now,
	}); finalizeErr != nil {
		// R6A 2026-09-10: the packet pins that LaunchViaActuator must keep
		// returning the minted ratt id alongside the error so callers
		// (notably execute_stage.launchStub) can record the partial-evidence
		// id in result.RunAttemptIDs. The canonical finalize path refused
		// here so the envelope is still in its pre-finalize state; surface
		// the finalize error so the operator still sees what refused.
		return mintedRattID, fmt.Errorf("execute stage launch: finalize refused: %w", finalizeErr)
	}
	if runErr != nil {
		// R6A 2026-09-10: slice 2's canonical finalize persisted partial
		// output and closed the envelope as failed; surface the adapter
		// error verbatim AND return the minted ratt id so the seam can
		// record the partial-evidence id in result.RunAttemptIDs and a
		// future operator query can find the workspace/command/transcript
		// the crashed attempt left behind.
		return mintedRattID, runErr
	}
	return mintedRattID, nil
}

// InstallDetached (R6P 2026-09-11) makes the unattended seam hand launches to
// a detached governed door (LaunchDetachedViaDoor) instead of running the lane
// inside the tick. It is a SEPARATE composition step from Install so the
// package's own tests, which call Install to exercise the synchronous wire,
// never spawn real doors. cmd/compound calls both. The operator pins the
// synchronous seam with COMPOUND_UNATTENDED_SYNCHRONOUS=1 (the kill switch;
// that path keeps its 5-minute budget).
func InstallDetached() {
	if os.Getenv("COMPOUND_UNATTENDED_SYNCHRONOUS") == "1" {
		return
	}
	work.SwapExecuteStageDetachedLaunch(LaunchDetachedViaDoor)
	work.SwapExecuteStageDetachedCandidateLaunch(LaunchDetachedCandidateViaDoor)
}
