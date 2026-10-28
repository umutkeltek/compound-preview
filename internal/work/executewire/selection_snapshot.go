package executewire

import (
	"fmt"
	"strings"

	"compound/internal/envelope"
	"compound/internal/harness"
	"compound/internal/runattempt"
)

// buildSelectionSnapshotFromSaved resolves the launch wire's
// ExecutionSelectionSnapshot from the work envelope's saved selection,
// any explicit one-run override, the resolver-observed profile revision,
// and the harness-level inputs. source is "saved" unless overrideLaunchProfile
// is non-empty (which switches the snapshot to source: override and discards
// the saved revision).
//
// The snapshot reflects what the wire ASKED the adapter to run, plus the
// honest evidence about what the adapter COULD apply (effort control,
// session pinning). The attempt's observed identity (real model id from
// the provider receipt) stays on RunAttempt.ModelID / ProviderRoute and is
// attested only by route evidence -- the snapshot never claims observed
// identity, only what was asked.
//
// work_r7a_execution_selection_20260911.
func buildSelectionSnapshotFromSaved(
	wenv envelope.WorkEnvelope,
	overrideHarness string,
	overrideLaunchProfile string,
	resolvedProviderRoute string,
	resolvedModelID string,
	observedProfileRevision string,
	tupleProfileRevision string,
) (runattempt.ExecutionSelectionSnapshot, error) {
	if overrideLaunchProfile != "" {
		// Explicit one-run override. The wire treats the override as
		// authoritative for THIS run; the saved selection is untouched.
		// observedProfileRevision may be empty (a one-run override
		// can name a profile the resolver cannot resolve) -- the
		// validator accepts an empty revision, and the reader sees
		// source: override so it knows why.
		harnessName := strings.TrimSpace(overrideHarness)
		if harnessName == "" {
			harnessName = firstNonEmpty(envelopePtr(wenv.ExecutionSelection).Harness, "os-harness-entrypoints")
		}
		snap := runattempt.ExecutionSelectionSnapshot{
			Source:                "override",
			LaunchProfileID:       overrideLaunchProfile,
			LaunchProfileRevision: observedProfileRevision,
			Harness:               harnessName,
			ProviderRoute:         resolvedProviderRoute,
			RequestedModelID:      resolvedModelID,
			RequestedEffort:       envelopePtr(wenv.ExecutionSelection).RequestedEffort,
		}
		applyEffortEvidence(&snap)
		applyNativeSessionEvidence(&snap)
		if err := snap.Validate(); err != nil {
			return runattempt.ExecutionSelectionSnapshot{}, fmt.Errorf("build override snapshot: %w", err)
		}
		return snap, nil
	}
	sel := wenv.ExecutionSelection
	if sel == nil {
		// No saved selection: the wire falls through to the project
		// default. The snapshot records source: project_default so a
		// reader sees why no saved selection drove the launch.
		snap := runattempt.ExecutionSelectionSnapshot{
			Source:           "project_default",
			Harness:          firstNonEmpty(strings.TrimSpace(overrideHarness), "os-harness-entrypoints"),
			ProviderRoute:    resolvedProviderRoute,
			RequestedModelID: resolvedModelID,
		}
		applyEffortEvidence(&snap)
		applyNativeSessionEvidence(&snap)
		if err := snap.Validate(); err != nil {
			return runattempt.ExecutionSelectionSnapshot{}, fmt.Errorf("build project_default snapshot: %w", err)
		}
		return snap, nil
	}
	// Saved selection. Stamp the consumed revision; record drift when
	// the observed profile revision differs from the saved one.
	snap := runattempt.ExecutionSelectionSnapshot{
		Source:                "saved",
		SelectionRevision:     sel.Revision,
		LaunchProfileID:       sel.LaunchProfileID,
		LaunchProfileRevision: observedProfileRevision,
		Harness:               sel.Harness,
		ProviderRoute:         sel.ProviderRoute,
		RequestedModelID:      sel.RequestedModelID,
		RequestedEffort:       sel.RequestedEffort,
	}
	if overrideHarness != "" && overrideHarness != sel.Harness {
		// One-run harness override on a saved selection: the snapshot
		// records source: saved (the selection is the plan), and the
		// override is reflected by switching the harness on the snapshot
		// to the override's family. The envelope's saved selection is
		// untouched.
		snap.Harness = overrideHarness
	}
	if strings.TrimSpace(snap.LaunchProfileRevision) == "" {
		snap.LaunchProfileRevision = sel.LaunchProfileRevision
	}
	if tupleProfileRevision != "" && sel.LaunchProfileRevision != "" && tupleProfileRevision != sel.LaunchProfileRevision {
		snap.SelectionDrift = fmt.Sprintf("launch_profile_revision saved=%s launched=%s", sel.LaunchProfileRevision, tupleProfileRevision)
	}
	applyEffortEvidence(&snap)
	applyNativeSessionEvidence(&snap)
	if err := snap.Validate(); err != nil {
		return runattempt.ExecutionSelectionSnapshot{}, fmt.Errorf("build saved snapshot: %w", err)
	}
	return snap, nil
}

// BuildSelectionSnapshot resolves the per-run ExecutionSelectionSnapshot for
// a governed launch OUTSIDE the launch wire — today the public
// `work advance --execute` CLI path (and any future adapter-driven caller
// that has no lane tuple). Resolution precedence is the contract's:
//
//  1. resolvedOverride (an explicit one-run --launch-profile/--effort
//     resolution) — source=override; the envelope's saved selection is
//     not touched;
//  2. the envelope's saved selection — source=saved, stamped with its
//     consumed revision;
//  3. otherwise the project default — source=project_default.
//
// The snapshot records only what this launch was ASKED to run plus the
// honest effort/session evidence; observed provider identity stays on the
// RunAttempt's own fields. The route/model asked for come from the
// resolution that produced the choice: the override's resolution when the
// override drives, the saved selection's own values when it drives, and
// nothing when the project default stands in (this caller has no lane
// tuple to resolve).
//
// work_r7a_execution_selection_20260911; public-CLI contract 2026-09-15.
func BuildSelectionSnapshot(
	wenv envelope.WorkEnvelope,
	overrideHarness string,
	resolvedOverride *envelope.ExecutionSelection,
) (runattempt.ExecutionSelectionSnapshot, error) {
	if resolvedOverride != nil && strings.TrimSpace(resolvedOverride.LaunchProfileID) != "" {
		snap := runattempt.ExecutionSelectionSnapshot{
			Source:                "override",
			LaunchProfileID:       strings.TrimSpace(resolvedOverride.LaunchProfileID),
			LaunchProfileRevision: strings.TrimSpace(resolvedOverride.LaunchProfileRevision),
			Harness:               firstNonEmpty(strings.TrimSpace(overrideHarness), strings.TrimSpace(resolvedOverride.Harness)),
			ProviderRoute:         strings.TrimSpace(resolvedOverride.ProviderRoute),
			RequestedModelID:      strings.TrimSpace(resolvedOverride.RequestedModelID),
			RequestedEffort:       strings.TrimSpace(resolvedOverride.RequestedEffort),
		}
		if snap.Harness == "" {
			snap.Harness = firstNonEmpty(envelopePtr(wenv.ExecutionSelection).Harness, "os-harness-entrypoints")
		}
		applyEffortEvidence(&snap)
		applyNativeSessionEvidence(&snap)
		if err := snap.Validate(); err != nil {
			return runattempt.ExecutionSelectionSnapshot{}, fmt.Errorf("build override snapshot: %w", err)
		}
		return snap, nil
	}
	sel := wenv.ExecutionSelection
	resolvedRoute, resolvedModel := "", ""
	if sel != nil {
		// The saved selection's own route/model ARE what the work asks for;
		// this caller has no lane tuple to resolve them from again.
		resolvedRoute, resolvedModel = sel.ProviderRoute, sel.RequestedModelID
	}
	return buildSelectionSnapshotFromSaved(
		wenv,
		overrideHarness,
		"",
		resolvedRoute,
		resolvedModel,
		"", // no observed profile revision on this path; the saved one stamps through
		"",
	)
}

// BindSelectionBeforePreflight makes the public launch consume the same choice
// its RunAttempt will report. Codex resolves the model-router tuple and owned
// executable in Preflight; a snapshot alone cannot select that executable.
func BindSelectionBeforePreflight(wenv envelope.WorkEnvelope, harnessName string, override *envelope.ExecutionSelection, input harness.RunInput) (harness.RunInput, error) {
	snapshot, err := BuildSelectionSnapshot(wenv, harnessName, override)
	if err != nil {
		return harness.RunInput{}, err
	}
	// Same refusal the shared runner applies, raised here so the public CLI
	// refuses before any envelope state change.
	if err := harness.CheckSelectionApplicable(harnessName, &snapshot); err != nil {
		return harness.RunInput{}, err
	}
	input.ExecutionSelectionSnapshot = &snapshot
	if harnessName != "codex" || snapshot.LaunchProfileID == "" {
		return input, nil
	}
	if snapshot.Harness != harnessName || snapshot.RequestedModelID == "" || snapshot.ProviderRoute == "" || snapshot.LaunchProfileRevision == "" {
		return harness.RunInput{}, fmt.Errorf("Codex selection %q lacks an exact harness, model, provider route, or owner revision", snapshot.LaunchProfileID)
	}
	input.LaunchProfileID = snapshot.LaunchProfileID
	input.RequestedModelID = snapshot.RequestedModelID
	input.RequestedProviderRoute = snapshot.ProviderRoute
	input.ExpectedModelRouterRevision = snapshot.LaunchProfileRevision
	return input, nil
}

// applyEffortEvidence stamps the honest evidence about whether the requested
// effort can be applied, read from the harness's registry selection row
// (harness.SelectionCapabilityFor) instead of a name list. The requested value
// is NEVER cleared: when the row says the harness cannot apply it, the
// evidence says so and the shared runner refuses the launch before mint
// (selection_unsupported:<harness>:effort). Recording a request and then
// running without it was the silent drop this replaces.
//
// work_r7a_execution_selection_20260911; fail-closed 2026-10-06.
func applyEffortEvidence(snap *runattempt.ExecutionSelectionSnapshot) {
	capability, _ := harness.SelectionCapabilityFor(snap.Harness)
	switch {
	case snap.RequestedEffort == "" && capability.EffortPassable:
		snap.EffortEvidence = "unsupported:no effort requested"
	case snap.RequestedEffort == "":
		snap.EffortEvidence = "unsupported:harness " + snap.Harness + " has no effort control"
	case capability.EffortPassable:
		// The adapter attests argv application at Run, or refuses the launch.
		snap.EffortEvidence = "unobservable:" + snap.Harness + " effort application awaits adapter argv attestation"
	default:
		snap.EffortEvidence = "unsupported:harness " + snap.Harness + " cannot apply a requested effort; launch refuses selection_unsupported"
	}
}

// applyNativeSessionEvidence stamps the honest native-session evidence from
// the same registry row. No harness pins a session from the wire today, so the
// row's SessionEvidence (unsupported/unobservable) is what a reader sees; the
// validator refuses a NativeSessionID paired with either.
//
// work_r7a_execution_selection_20260911.
func applyNativeSessionEvidence(snap *runattempt.ExecutionSelectionSnapshot) {
	if snap.NativeSessionEvidence != "" {
		return
	}
	capability, _ := harness.SelectionCapabilityFor(snap.Harness)
	snap.NativeSessionEvidence = firstNonEmpty(capability.SessionEvidence, "unsupported:harness "+snap.Harness+" has no native session concept")
}

// envelopePtr returns *envelope.ExecutionSelection, treating a nil as a
// pointer-to-zero. Used to defensively read selection fields when the
// caller has not pre-checked nil.
func envelopePtr(s *envelope.ExecutionSelection) envelope.ExecutionSelection {
	if s == nil {
		return envelope.ExecutionSelection{}
	}
	return *s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
