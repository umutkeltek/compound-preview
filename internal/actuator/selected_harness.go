package actuator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"compound/internal/harness"
	"compound/internal/hookplan"
	"compound/internal/routeevidence"
	"compound/internal/runattempt"
	"compound/internal/work"
)

const SelectedHarnessExecutionSchemaVersion = "compound.work_dispatch_execution.v0.1"

// SelectedHarnessExecutionPlan is the post-routing execution contract. The
// caller owns capability projection and workspace resolution; the actuator
// owns every stateful step from adapter preflight through finalization.
type SelectedHarnessExecutionPlan struct {
	WorkID              string
	Task                string
	Workspace           string
	Adapter             string
	Selected            routeevidence.CapabilityRouteLane
	CapabilityClass     string
	RoutePlanID         string
	RouteSourceRevision string
	ProofRefs           []string
	Warnings            []string
}

type SelectedHarnessExecutionOptions struct {
	PermissionMode string
}

type SelectedHarnessExecutionResult struct {
	SchemaVersion       string                            `json:"schema_version"`
	Status              string                            `json:"status"`
	WorkID              string                            `json:"work_id"`
	FinalState          string                            `json:"final_state"`
	Adapter             string                            `json:"adapter"`
	Selected            routeevidence.CapabilityRouteLane `json:"selected"`
	RoutePlanID         string                            `json:"route_plan_id"`
	RouteSourceRevision string                            `json:"route_source_revision"`
	DispatchClaimID     string                            `json:"dispatch_claim_id"`
	RunAttemptID        string                            `json:"run_attempt_id"`
	ExitCode            int                               `json:"exit_code"`
	TranscriptPath      string                            `json:"transcript_path,omitempty"`
	Warnings            []string                          `json:"warnings,omitempty"`
}

type preparedSelectedHarnessExecution struct {
	adapter  harness.Adapter
	runInput harness.RunInput
}

// SelectAdapterBackedRoute searches the full eligible route, not a bounded
// presentation projection, and returns the first lane Compound can execute.
func SelectAdapterBackedRoute(homePath string, chain []routeevidence.CapabilityRouteLane) (routeevidence.CapabilityRouteLane, string, []string, error) {
	registry := harness.NewAdapterRegistry(runattempt.Open(homePath))
	var warnings []string
	for _, lane := range chain {
		adapter, ok := registry.AdapterByName(lane.Harness)
		if !ok {
			warnings = append(warnings, fmt.Sprintf("route lane %s skipped: no registered adapter for harness %q", lane.ModelID, lane.Harness))
			continue
		}
		return lane, adapter.Name(), warnings, nil
	}
	return routeevidence.CapabilityRouteLane{}, "", warnings, fmt.Errorf("no registered adapter for any eligible route lane")
}

// ExecuteSelectedHarness consumes a resolved capability route through the
// existing governed execution rail. Every fallible read-only admission check
// completes before DispatchClaim mutates the WorkEnvelope.
//
// ctx is the caller's launch context; it flows into the canonical
// ExecuteGovernedHarnessAdvance owner (production: signal-derived).
func ExecuteSelectedHarness(
	ctx context.Context,
	homePath string,
	store *work.Store,
	plan SelectedHarnessExecutionPlan,
	opts SelectedHarnessExecutionOptions,
	now time.Time,
) (SelectedHarnessExecutionResult, error) {
	prepared, err := prepareSelectedHarnessExecution(ctx, homePath, plan, opts, now)
	if err != nil {
		return SelectedHarnessExecutionResult{}, err
	}
	claim, err := store.DispatchClaim(now, work.DispatchPlanOptions{
		Actor:           plan.Adapter,
		WorkID:          plan.WorkID,
		CapabilityClass: plan.CapabilityClass,
		Confirm:         true,
	})
	if err != nil {
		return SelectedHarnessExecutionResult{}, fmt.Errorf("claim selected work: %w", err)
	}
	if claim.ClaimedWorkID != plan.WorkID {
		return SelectedHarnessExecutionResult{}, fmt.Errorf("selected work was not claimed: status=%s no_action=%s", claim.Status, claim.NoAction)
	}
	claimed, err := store.Get(plan.WorkID)
	if err != nil {
		return SelectedHarnessExecutionResult{}, fmt.Errorf("reload claimed work: %w", err)
	}
	prepared.runInput.Prompt = claim.WorkerPromptMD
	executed, err := ExecuteGovernedHarnessAdvance(
		ctx,
		homePath,
		store,
		claimed,
		prepared.adapter,
		prepared.runInput,
		plan.Adapter,
		"running",
		"Compound-selected capability route "+plan.RoutePlanID,
		claimed.RunAttemptIDs, false, now,
	)
	if err != nil {
		return SelectedHarnessExecutionResult{}, err
	}
	warnings := append([]string(nil), plan.Warnings...)
	for _, warning := range claim.Warnings {
		if warning != "does not execute work" {
			warnings = append(warnings, warning)
		}
	}
	warnings = append(warnings, executed.Finalized.Warnings...)
	if executed.RunError != nil {
		warnings = append(warnings, "harness run: "+executed.RunError.Error())
	}
	return SelectedHarnessExecutionResult{
		SchemaVersion:       SelectedHarnessExecutionSchemaVersion,
		Status:              "executed",
		WorkID:              plan.WorkID,
		FinalState:          executed.Finalized.FinalState,
		Adapter:             plan.Adapter,
		Selected:            plan.Selected,
		RoutePlanID:         plan.RoutePlanID,
		RouteSourceRevision: plan.RouteSourceRevision,
		DispatchClaimID:     claim.ID,
		RunAttemptID:        executed.Output.RunAttempt.ID,
		ExitCode:            executed.Output.ExitCode,
		TranscriptPath:      executed.Output.TranscriptPath,
		Warnings:            warnings,
	}, nil
}

func prepareSelectedHarnessExecution(
	ctx context.Context,
	homePath string,
	plan SelectedHarnessExecutionPlan,
	opts SelectedHarnessExecutionOptions,
	now time.Time,
) (preparedSelectedHarnessExecution, error) {
	registry := harness.NewAdapterRegistry(runattempt.Open(homePath))
	adapter, ok := registry.AdapterByName(plan.Adapter)
	if !ok {
		return preparedSelectedHarnessExecution{}, fmt.Errorf("selected adapter %q is no longer registered", plan.Adapter)
	}
	runInput := harness.RunInput{
		WorkID:                      plan.WorkID,
		Harness:                     adapter.Name(),
		Workspace:                   plan.Workspace,
		Prompt:                      plan.Task,
		PermissionMode:              strings.TrimSpace(opts.PermissionMode),
		LaneID:                      adapter.Name(),
		RequestedModelID:            plan.Selected.ModelID,
		RequestedProviderRoute:      plan.Selected.ProviderRoute,
		ExpectedModelRouterRevision: plan.RouteSourceRevision,
		ProofRefs:                   append([]string(nil), plan.ProofRefs...),
		Now:                         now,
	}
	if preflight, ok := adapter.(harness.PreflightAdapter); ok {
		resolved, err := preflight.Preflight(ctx, runInput)
		if err != nil {
			return preparedSelectedHarnessExecution{}, fmt.Errorf("harness %s preflight: %w", adapter.Name(), err)
		}
		runInput = resolved
	}
	if runInput.RequestedModelID == "" {
		runInput.RequestedModelID = plan.Selected.ModelID
	}
	if runInput.RequestedProviderRoute == "" {
		runInput.RequestedProviderRoute = plan.Selected.ProviderRoute
	}
	if runInput.RequestedModelID != plan.Selected.ModelID || runInput.RequestedProviderRoute != plan.Selected.ProviderRoute {
		return preparedSelectedHarnessExecution{}, fmt.Errorf(
			"selected route drifted during adapter preflight: planned model=%q route=%q resolved model=%q route=%q",
			plan.Selected.ModelID, plan.Selected.ProviderRoute, runInput.RequestedModelID, runInput.RequestedProviderRoute,
		)
	}
	if gate := hookplan.GovernedFullRosterGate(adapter.Name()); gate != nil {
		return preparedSelectedHarnessExecution{}, fmt.Errorf("%s: %s", gate.Gate, gate.Refused)
	}
	return preparedSelectedHarnessExecution{adapter: adapter, runInput: runInput}, nil
}
