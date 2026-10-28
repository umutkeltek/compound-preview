package executewire

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/routeevidence"
	"compound/internal/work"
)

// resolveLaneLaunchTuple returns the {command, modelID, providerRoute,
// profileRevision, tupleBacked} the lane's adapter would resolve at launch.
// The unattended execute seam calls this from both the preflight and launch
// hooks so the request handed to the readiness guard carries the resolved
// tuple — the guard's resolved-vs-expected comparison in
// internal/harness/registry.go:ReadOnlyLaunchReadiness then succeeds for a
// tuple-backed lane.
//
// profileRevision is the Compound model-capabilities snapshot revision the
// resolver just consulted (sha256 of the manifest bytes). The launch wire
// uses it as the fallback Revision for posture evidence items the readiness
// guard produced WITHOUT a Revision — see work_r6m_seam_posture_20260911.
// Today the production `executable` evidence item in
// cmd/compound/work_delegate_readiness.go has Ref set and Revision empty;
// the wire fills the Revision from profileRevision before stamping the
// posture, so runattempt.SetExecutionPosture accepts the item. Items the
// wire CANNOT give a revision to (no resolver, no manifest) surface as
// `lane_unready:posture evidence <kind> has no revision` — the recognized
// refusal prefix the execute stage treats as a skip reason.
//
// For lanes the resolver treats as not tuple-backed (raw claude, codex), or
// when the resolver is not installed / returns an error, the helper returns
// ("", "", "", "", false): the seam falls through to passing the lane id,
// exactly as before. That matches the interactive delegate path's behavior
// when DelegateHarnessResolver is absent or rejects the request, and the
// readiness guard then surfaces the resolver-specific refusal with full
// context if the lane really is unready. The profileRevision field is left
// empty in that case — the wire's posture-evidence fallback degrades to
// refusing with `lane_unready:` rather than stamping a synthetic revision.
//
// The helper is read-only: it loads the Compound model-router authority but
// never mutates it, never records evidence, and never starts a subprocess.
//
// work_r6j_readiness_tuple_20260911 — the seam's two readiness call sites
// (PreflightViaActuator and LaunchViaActuator) used to fill
// {RequestedModelID, RequestedProviderRoute, Command} with the lane id and
// the empty string, so a tuple-backed Claude lane always failed the guard's
// "resolved tuple matches the request" comparison. The interactive delegate
// path already calls work.DelegateHarnessResolver; this helper is the same
// resolver reused read-only from the seam, with a fall-through for the
// non-tuple lanes the comparison does not gate.
//
// work_r6m_seam_posture_20260911 — added the profileRevision return value
// so the launch wire can give a real revision to posture evidence items
// that the readiness guard produced without one.
func resolveLaneLaunchTuple(homePath, lane string, now time.Time) (command, modelID, providerRoute, profileRevision string, tupleBacked bool) {
	if strings.TrimSpace(lane) == "" || homePath == "" {
		return "", "", "", "", false
	}
	resolver := work.DelegateHarnessResolver
	if resolver == nil {
		return "", "", "", "", false
	}
	resolution, err := resolver(work.DelegateHarnessResolveRequest{
		HomePath: homePath,
		Harness:  lane,
		Now:      now,
	})
	if err != nil {
		// Fall through. The downstream readiness guard revalidates the same
		// tuple via the adapter's RevalidateGovernedRun, which surfaces the
		// resolver's error verbatim.
		return "", "", "", "", false
	}
	if !resolution.TupleBacked {
		return "", "", "", "", false
	}
	// Load the manifest snapshot revision directly: routeevidence is the
	// Compound authority owner and its LoadModelCapabilitiesSnapshot returns
	// the sha256 of the manifest bytes. That revision IS the lane tuple's
	// profile revision the wire stamps onto posture evidence items the
	// readiness guard produced without one (work_r6m_seam_posture_20260911).
	// An unreadable manifest here means we cannot derive a fallback revision
	// at all — the wire's posture-evidence pass surfaces `lane_unready:`
	// rather than stamping junk.
	profileRevision = ""
	if _, _, rev, snapshotErr := routeevidence.LoadModelCapabilitiesSnapshot(homePath); snapshotErr == nil {
		profileRevision = rev
	}
	return strings.TrimSpace(resolution.Command),
		strings.TrimSpace(resolution.RequestedModelID),
		strings.TrimSpace(resolution.RequestedProviderRoute),
		profileRevision,
		true
}

func resolveExecutionRouteCandidate(homePath string, candidate work.ExecutionRouteCandidate, now time.Time) (string, error) {
	if strings.TrimSpace(candidate.LaunchProfileID) == "" || strings.TrimSpace(candidate.Harness) == "" {
		return "", fmt.Errorf("route candidate is missing profile or harness")
	}
	if work.DelegateHarnessResolver == nil {
		return "", fmt.Errorf("owner tuple resolver is not installed")
	}
	resolution, err := work.DelegateHarnessResolver(work.DelegateHarnessResolveRequest{HomePath: homePath, Harness: candidate.LaunchProfileID, Now: now})
	if err != nil {
		return "", err
	}
	if !resolution.TupleBacked {
		return "", fmt.Errorf("route candidate %q is not tuple-backed", candidate.LaunchProfileID)
	}
	if strings.TrimSpace(resolution.Harness) != strings.TrimSpace(candidate.Harness) {
		return "", fmt.Errorf("route candidate %q harness drift: manifest=%q resolved=%q", candidate.LaunchProfileID, candidate.Harness, resolution.Harness)
	}
	if resolution.RequestedModelID != candidate.RequestedModelID {
		return "", fmt.Errorf("route candidate %q model drift: manifest=%q resolved=%q", candidate.LaunchProfileID, candidate.RequestedModelID, resolution.RequestedModelID)
	}
	if resolution.RequestedProviderRoute != candidate.ProviderRoute {
		return "", fmt.Errorf("route candidate %q provider route drift: manifest=%q resolved=%q", candidate.LaunchProfileID, candidate.ProviderRoute, resolution.RequestedProviderRoute)
	}
	_, _, revision, err := routeevidence.LoadModelCapabilitiesSnapshot(homePath)
	if err != nil || revision != candidate.SourceRevision {
		return "", fmt.Errorf("route candidate %q authority revision changed", candidate.LaunchProfileID)
	}
	if resolution.CapacityLane != candidate.CapacityLane {
		return "", fmt.Errorf("route candidate %q capacity lane changed", candidate.LaunchProfileID)
	}
	return strings.TrimSpace(resolution.Command), nil
}

// validateSavedSelectionTuple refuses before readiness or minting when an
// explicit work choice cannot be carried by the selected route unchanged.
// Hyphen/underscore are lane spelling aliases only; model and provider route
// remain exact, case-sensitive identifiers.
func validateSavedSelectionTuple(wenv envelope.WorkEnvelope, candidate work.ExecutionRouteCandidate, now time.Time) error {
	sel := wenv.ExecutionSelection
	if sel == nil {
		return nil
	}
	normalizeLane := func(value string) string {
		return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), "-", "_")
	}
	wantLane := normalizeLane(sel.LaunchProfileID)
	gotLane := normalizeLane(candidate.LaunchProfileID)
	if wantLane == "" || wantLane != gotLane {
		return fmt.Errorf("saved execution_selection launch profile %q does not match selected lane %q", sel.LaunchProfileID, candidate.LaunchProfileID)
	}
	wantHarness := normalizeLane(sel.Harness)
	gotHarness := normalizeLane(candidate.Harness)
	if wantHarness != gotHarness {
		return fmt.Errorf("saved execution_selection harness %q does not match selected harness %q", sel.Harness, candidate.Harness)
	}
	if savedModel := strings.TrimSpace(sel.RequestedModelID); savedModel != "" && savedModel != candidate.RequestedModelID {
		return fmt.Errorf("saved execution_selection model %q does not match selected model %q", savedModel, candidate.RequestedModelID)
	}
	if savedRoute := strings.TrimSpace(sel.ProviderRoute); savedRoute != "" && savedRoute != candidate.ProviderRoute {
		return fmt.Errorf("saved execution_selection provider route %q does not match selected provider route %q", savedRoute, candidate.ProviderRoute)
	}
	_ = now
	return nil
}

// ExecutionTupleDigest seals route identity separately from the envelope's
// integer execution-selection CAS revision. It grants no launch authority.
func ExecutionTupleDigest(candidate work.ExecutionRouteCandidate) string {
	candidate.SelectionRevision = 0
	body, _ := json.Marshal(candidate)
	return fmt.Sprintf("sha256:%x", sha256.Sum256(body))
}

// ValidateExpectedExecutionTuple is the detached child's read-only admission
// check. The fresh owner tuple must match the parent before adapter preflight
// or any work mutation. Existing launch guards still revalidate at start.
func ValidateExpectedExecutionTuple(homePath, expected, harnessName, profileID string, wenv envelope.WorkEnvelope, now time.Time) error {
	if expected == "" {
		return nil
	}
	if profileID == "" {
		return fmt.Errorf("expected execution tuple requires a launch profile")
	}
	resolved, err := ResolveExecutionSelectionViaOwner(work.ExecutionSelectionResolveRequest{HomePath: homePath, LaunchProfileID: profileID, Now: now})
	if err != nil {
		return err
	}
	candidate := work.ExecutionRouteCandidate{LaunchProfileID: resolved.LaunchProfileID, Harness: resolved.Harness, RequestedModelID: resolved.RequestedModelID, ProviderRoute: resolved.ProviderRoute, SourceRevision: resolved.LaunchProfileRevision, CapacityLane: resolved.ProviderRoute}
	if candidate.Harness != harnessName || ExecutionTupleDigest(candidate) != expected {
		return fmt.Errorf("expected execution tuple changed for profile %q", profileID)
	}
	return validateSavedSelectionTuple(wenv, candidate, now)
}

// Legacy direct callers can omit a route snapshot. Resolve their tuple without
// inventing model/provider identity; production candidate dispatch is sealed.
func legacyRouteCandidate(homePath, lane, harnessName string, now time.Time) work.ExecutionRouteCandidate {
	candidate := work.ExecutionRouteCandidate{LaunchProfileID: lane, Harness: harnessName}
	_, candidate.RequestedModelID, candidate.ProviderRoute, _, _ = resolveLaneLaunchTuple(homePath, lane, now)
	if candidate.RequestedModelID == "" {
		candidate.RequestedModelID = lane
	}
	if candidate.ProviderRoute == "" {
		candidate.ProviderRoute = lane
	}
	return candidate
}
