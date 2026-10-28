package main

import (
	"fmt"
	"strings"
	"time"

	"compound/internal/actuator"
	"compound/internal/capgraph"
	"compound/internal/routeevidence"
	"compound/internal/work"
	"compound/internal/work/executewire"
)

type workDispatchExecuteOptions struct {
	Workspace        string
	PrivacySensitive bool
	PermissionMode   string
}

type workDispatchExecutionPlan struct {
	SchemaVersion string                            `json:"schema_version"`
	Status        string                            `json:"status"`
	WorkID        string                            `json:"work_id"`
	Task          string                            `json:"task"`
	Workspace     string                            `json:"workspace"`
	Adapter       string                            `json:"adapter"`
	Selected      routeevidence.CapabilityRouteLane `json:"selected"`
	Resolution    capabilityResolutionReport        `json:"resolution"`
	Warnings      []string                          `json:"warnings,omitempty"`
}

type workDispatchExecutionResult = actuator.SelectedHarnessExecutionResult

func buildWorkDispatchExecutionPlan(homePath string, store *work.Store, workID string, opts workDispatchExecuteOptions, now time.Time) (workDispatchExecutionPlan, error) {
	wenv, err := store.Get(strings.TrimSpace(workID))
	if err != nil {
		return workDispatchExecutionPlan{}, fmt.Errorf("load target work: %w", err)
	}
	if wenv.State != "ready" {
		return workDispatchExecutionPlan{}, fmt.Errorf("work %s is %s; dispatch-execute requires ready work", wenv.ID, wenv.State)
	}
	task := strings.TrimSpace(strings.Join([]string{wenv.DesiredOutcome, wenv.SuccessPredicate}, "\n"))
	if task == "" {
		return workDispatchExecutionPlan{}, fmt.Errorf("work %s has no desired outcome or success predicate to route", wenv.ID)
	}

	graph, err := capgraph.OpenReadOnly(homePath)
	if err != nil {
		return workDispatchExecutionPlan{}, fmt.Errorf("open capability graph authority: %w", err)
	}
	resolution, err := buildCapabilityResolutionReport(graph, homePath, task, "", opts.PrivacySensitive, true, now)
	if err != nil {
		return workDispatchExecutionPlan{}, fmt.Errorf("resolve capability route: %w", err)
	}

	selected, adapterName, warnings, err := actuator.SelectAdapterBackedRoute(homePath, resolution.selectionChain)
	if err != nil {
		return workDispatchExecutionPlan{}, err
	}

	workspaceArgs := []string(nil)
	if strings.TrimSpace(opts.Workspace) != "" {
		workspaceArgs = []string{"--workspace", strings.TrimSpace(opts.Workspace)}
	}
	workspace, tried := resolveExecuteWorkspace(workspaceArgs, wenv.GitDelivery)
	if workspace == "" {
		return workDispatchExecutionPlan{}, fmt.Errorf("resolve execution workspace for %s: %s", wenv.ID, strings.Join(tried, "; "))
	}
	if observed, ok := checkExecuteWorkspaceExists(workspace); !ok {
		return workDispatchExecutionPlan{}, fmt.Errorf("execution workspace unavailable: %s", observed)
	}

	return workDispatchExecutionPlan{
		SchemaVersion: actuator.SelectedHarnessExecutionSchemaVersion,
		Status:        "preview",
		WorkID:        wenv.ID,
		Task:          task,
		Workspace:     workspace,
		Adapter:       adapterName,
		Selected:      selected,
		Resolution:    resolution,
		Warnings:      warnings,
	}, nil
}

func executeWorkDispatchExecution(homePath string, store *work.Store, plan workDispatchExecutionPlan, opts workDispatchExecuteOptions, now time.Time) (workDispatchExecutionResult, error) {
	// The signal-derived launch context: cancellation reaches the governed
	// execution owner, not just the CLI process.
	return actuator.ExecuteSelectedHarness(executewire.LaunchContext(), homePath, store, actuator.SelectedHarnessExecutionPlan{
		WorkID:              plan.WorkID,
		Task:                plan.Task,
		Workspace:           plan.Workspace,
		Adapter:             plan.Adapter,
		Selected:            plan.Selected,
		CapabilityClass:     plan.Resolution.Classification.CapabilityClass,
		RoutePlanID:         plan.Resolution.RoutePlan.ID,
		RouteSourceRevision: plan.Resolution.RoutePlan.SourceRevision,
		ProofRefs:           workDispatchRouteProofRefs(plan),
		Warnings:            plan.Warnings,
	}, actuator.SelectedHarnessExecutionOptions{
		PermissionMode: opts.PermissionMode,
	}, now)
}

func workDispatchRouteProofRefs(plan workDispatchExecutionPlan) []string {
	return []string{
		"capability-resolution:" + plan.Resolution.SchemaVersion,
		"capability-route-plan:" + plan.Resolution.RoutePlan.ID,
		"model-router-authority:" + plan.Resolution.RoutePlan.AuthorityRef + "@" + plan.Resolution.RoutePlan.SourceRevision,
		"capability-graph-authority:" + plan.Resolution.CapabilitySurface.AuthorityRef + "@" + plan.Resolution.CapabilitySurface.AuthorityRevision,
		"task-classifier-authority:" + plan.Resolution.Classification.AuthorityRef + "@" + plan.Resolution.Classification.AuthorityRevision,
	}
}
