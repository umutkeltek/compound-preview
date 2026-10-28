package routeevidence

import (
	"fmt"
	"strings"

	"compound/internal/work"
)

func resolveDelegateHarness(req work.DelegateHarnessResolveRequest) (work.DelegateHarnessResolution, error) {
	manifest, _, err := LoadModelCapabilitiesFromHome(req.HomePath)
	if err != nil {
		return work.DelegateHarnessResolution{}, fmt.Errorf("load Compound model-router authority: %w", err)
	}
	modelID, tuple, err := ResolveModelCapabilityTuple(manifest, req.Harness)
	if err != nil {
		return work.DelegateHarnessResolution{}, err
	}
	if strings.TrimSpace(tuple.Harness) == "" {
		return work.DelegateHarnessResolution{}, fmt.Errorf("Compound lane tuple %q has no harness", modelID)
	}
	if strings.TrimSpace(tuple.ProviderRoute) == "" {
		return work.DelegateHarnessResolution{}, fmt.Errorf("Compound lane tuple %q has no provider_route", modelID)
	}
	providerRouteKind := strings.TrimSpace(tuple.ProviderRouteKind)
	if providerRouteKind != "" && !validProviderRouteKind(providerRouteKind) {
		return work.DelegateHarnessResolution{}, fmt.Errorf(
			"Compound lane tuple %q has unknown provider_route_kind %q",
			modelID,
			providerRouteKind,
		)
	}
	command := strings.TrimSpace(req.CommandOverride)
	if command == "" {
		command, err = ResolveModelCapabilityExecutable(tuple.Aliases)
		if err != nil {
			return work.DelegateHarnessResolution{}, fmt.Errorf("Compound lane tuple %q: %w", modelID, err)
		}
	}
	resolution := work.DelegateHarnessResolution{
		Harness:                    strings.TrimSpace(tuple.Harness),
		Command:                    command,
		LaneID:                     strings.TrimSpace(tuple.Harness),
		RequestedModelID:           modelID,
		RequestedProviderRoute:     strings.TrimSpace(tuple.ProviderRoute),
		RequestedProviderRouteKind: providerRouteKind,
		// Transport is executor-owned in the lane projection. Keep it unknown
		// until the selected executor returns an owner-attested profile plan.
		SelectedTransportID: "",
		CapacityLane:        strings.TrimSpace(tuple.ProviderRoute),
		TupleBacked:         true,
	}
	if tuple.CapacitySource != nil {
		resolution.CapacitySource = &work.LaneCapacitySource{
			Kind:             tuple.CapacitySource.Kind,
			FreshnessMinutes: tuple.CapacitySource.FreshnessMinutes,
			Ref:              tuple.CapacitySource.Ref,
		}
	}
	return resolution, nil
}

func init() {
	work.DelegateHarnessResolver = resolveDelegateHarness
}
