package executewire

import (
	"errors"
	"fmt"
	"strings"

	"compound/internal/routeevidence"
	"compound/internal/work"
)

// ResolveExecutionSelectionViaOwner is the production binding for
// work.DefaultExecutionSelectionResolver: it turns a launch profile id into
// the harness / route / model identity through the SAME owner consultation
// the launch itself uses (work.DelegateHarnessResolver over the Compound
// model-router authority), so a saved selection and the launch that later
// consumes it can never disagree about what the profile means.
//
// The R7A slice shipped the seam with a nil default, and the installed CLI
// refused every `--launch-profile` with "no execution selection resolver
// installed" while its tests passed against a stub. This file is the wire
// that makes the landed behavior live; executewire.Install binds it.
//
// What is and is not claimed here:
//   - Harness is the adapter name the owner resolution returns (the value
//     `work advance --execute --harness` takes), never a display alias.
//   - RequestedModelID and ProviderRoute are exactly what the launch will
//     request; observed identity stays on route evidence.
//   - LaunchProfileRevision is the model-router authority snapshot revision
//     the resolution was made against (sha256 of the manifest bytes).
//   - RequestedEffort is left empty: the model-router tuple carries no
//     effort axis today, and an invented value would be recorded as if the
//     owner had asked for it. An explicit --effort override still lands on
//     the selection through work.ResolveSelection; the launch snapshot then
//     reports how (or whether) the adapter could apply it.
func ResolveExecutionSelectionViaOwner(req work.ExecutionSelectionResolveRequest) (work.ExecutionSelectionResolution, error) {
	var zero work.ExecutionSelectionResolution
	id := strings.TrimSpace(req.LaunchProfileID)
	if id == "" {
		return zero, errors.New("launch_profile_id is required")
	}
	if strings.TrimSpace(req.HomePath) == "" {
		return zero, errors.New("home path is required to consult the model-router authority")
	}
	resolver := work.DelegateHarnessResolver
	if resolver == nil {
		return zero, errors.New("owner resolver (work.DelegateHarnessResolver) is not installed")
	}
	resolution, err := resolver(work.DelegateHarnessResolveRequest{
		HomePath:        req.HomePath,
		Harness:         id,
		CommandOverride: strings.TrimSpace(req.CommandOverride),
		Now:             req.Now,
	})
	if err != nil {
		// The owner's refusal is the contract: surface it verbatim.
		return zero, err
	}
	if !resolution.TupleBacked {
		return zero, fmt.Errorf("launch profile %q is not a tuple-backed lane in the model-router authority; only owner-published lanes can be saved as an execution selection", id)
	}
	harness := strings.TrimSpace(resolution.Harness)
	if harness == "" {
		harness = strings.TrimSpace(resolution.LaneID)
	}
	revision := ""
	if _, _, rev, snapshotErr := routeevidence.LoadModelCapabilitiesSnapshot(req.HomePath); snapshotErr == nil {
		revision = rev
	}
	return work.ExecutionSelectionResolution{
		LaunchProfileID:       id,
		LaunchProfileRevision: revision,
		Harness:               harness,
		ProviderRoute:         strings.TrimSpace(resolution.RequestedProviderRoute),
		RequestedModelID:      strings.TrimSpace(resolution.RequestedModelID),
	}, nil
}
