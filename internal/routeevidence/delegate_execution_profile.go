package routeevidence

import (
	"encoding/json"
	"fmt"
	"strings"

	"compound/internal/work"
)

func resolveDelegateExecutionProfile(req work.DelegateExecutionProfileResolveRequest) (*work.DelegateExecutionProfileResolution, error) {
	plan, err := ResolveExecutionProfile(ExecutionProfileIntent{
		ProfileID:           req.Intent.ProfileID,
		Role:                req.Intent.Role,
		CapabilityClass:     req.Intent.CapabilityClass,
		ClientHarnessID:     req.Intent.ClientHarnessID,
		TransportID:         req.Intent.TransportID,
		RouteAlias:          req.Intent.RouteAlias,
		ModelConstraint:     req.Intent.ModelConstraint,
		SemanticEffort:      req.Intent.SemanticEffort,
		NativeEffort:        req.Intent.NativeEffort,
		OrchestrationPolicy: req.Intent.OrchestrationPolicy,
		PrivacyCeiling:      req.Intent.PrivacyCeiling,
	})
	if err != nil {
		return nil, err
	}
	snapshot, err := json.Marshal(plan)
	if err != nil {
		return nil, fmt.Errorf("marshal execution profile plan snapshot: %w", err)
	}
	disposition := work.DelegateExecutionProfileIncompatible
	transportCompatible := delegateExecutionProfileTransportCompatible(plan.TransportID, req.SelectedTransportID)
	if strings.TrimSpace(req.SelectedTransportID) == "" &&
		strings.TrimSpace(req.Intent.LaunchProfileID) != "" &&
		strings.TrimSpace(plan.TransportID) == "switchback" {
		transportCompatible = true
	}
	if DelegateClientHarnessesCompatible(req.SelectedHarness, req.SelectedLaneID, plan.ClientHarnessID) &&
		delegateExecutionProfileModelCompatible(plan.ModelConstraint, req.SelectedModelID) &&
		delegateExecutionProfileRouteCompatible(plan.RouteAlias, req.SelectedProviderRoute, req.SelectedModelID) &&
		transportCompatible {
		disposition = work.DelegateExecutionProfileCompatible
	}
	return &work.DelegateExecutionProfileResolution{
		Snapshot: snapshot,
		Compatibility: work.DelegateExecutionProfileCompatibility{
			Disposition:           disposition,
			SelectedHarness:       req.SelectedHarness,
			SelectedLaneID:        req.SelectedLaneID,
			SelectedModelID:       req.SelectedModelID,
			SelectedProviderRoute: req.SelectedProviderRoute,
			SelectedTransportID:   req.SelectedTransportID,
			PlanID:                plan.ID,
			ClientHarnessID:       plan.ClientHarnessID,
			TransportID:           plan.TransportID,
			RequestedNativeEffort: plan.NativeEffort.Requested,
		},
	}, nil
}

func delegateExecutionProfileModelCompatible(modelConstraint, selectedModelID string) bool {
	if strings.TrimSpace(modelConstraint) == "" {
		return true
	}
	return delegateExecutionProfileTupleIdentityEqual(modelConstraint, selectedModelID)
}

func delegateExecutionProfileRouteCompatible(routeAlias, selectedProviderRoute, selectedRouteIdentity string) bool {
	if strings.TrimSpace(routeAlias) == "" {
		return true
	}
	return delegateExecutionProfileTupleIdentityEqual(routeAlias, selectedProviderRoute) ||
		delegateExecutionProfileTupleIdentityEqual(routeAlias, selectedRouteIdentity)
}

func delegateExecutionProfileTransportCompatible(planTransportID, selectedTransportID string) bool {
	return strings.TrimSpace(selectedTransportID) != "" &&
		delegateExecutionProfileTupleIdentityEqual(planTransportID, selectedTransportID)
}

func delegateExecutionProfileTupleIdentityEqual(left, right string) bool {
	return strings.TrimSpace(left) == strings.TrimSpace(right)
}

// DelegateClientHarnessesCompatible keeps client-family ownership
// beside the routeevidence tuple resolver. The accepted delegate identities
// are the exact Claude/Codex harnesses currently enrolled by Compound's
// HarnessRegistry; normalization only makes their declared hyphen/underscore
// spellings equivalent. Unknown prefix lookalikes remain unregistered.
func DelegateClientHarnessesCompatible(selectedHarness, selectedLaneID, clientHarnessID string) bool {
	selectedFamily, selectedRegistered := registeredDelegateClientHarnessFamily(selectedHarness)
	laneFamily, laneRegistered := registeredDelegateClientHarnessFamily(selectedLaneID)
	return selectedRegistered &&
		laneRegistered &&
		normalizeModelCapabilitySelector(selectedHarness) == normalizeModelCapabilitySelector(selectedLaneID) &&
		selectedFamily == laneFamily &&
		selectedFamily == normalizeModelCapabilitySelector(clientHarnessID)
}

func registeredDelegateClientHarnessFamily(harness string) (string, bool) {
	switch normalizeModelCapabilitySelector(harness) {
	case "claude_code", "claude_zai", "claude_minimax", "claude_qwen", "claude_zai_full",
		"claude_kimi", "claude_deepseek_flash_nvidia":
		return "claude_code", true
	case "codex":
		return "codex", true
	default:
		return "", false
	}
}

func init() {
	work.DelegateExecutionProfileResolver = resolveDelegateExecutionProfile
}
