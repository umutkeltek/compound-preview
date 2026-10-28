package work

import (
	"fmt"
	"strings"
	"time"
)

// ContinuePreview is an observation, never authorization for a later mutation.
// Deferred checks stay explicit even when every current readiness check passes.
type ContinuePreview struct {
	Status          string                   `json:"status"`
	WorkID          string                   `json:"work_id,omitempty"`
	Action          string                   `json:"action"`
	ExecutionChoice *ExecutionRouteCandidate `json:"execution_choice,omitempty"`
	ChecksPerformed []string                 `json:"checks_performed"`
	ChecksDeferred  []string                 `json:"checks_deferred"`
	ProposedEffects []string                 `json:"proposed_effects"`
}

func (s *Store) previewContinue(now time.Time, opts ContinueOptions, report WorkbenchReport, result ContinueResult) (ContinueResult, error) {
	p := &ContinuePreview{
		Status: "waiting", Action: "none",
		ChecksPerformed: []string{"workbench_readiness"},
		ChecksDeferred:  []string{"locked_lifecycle_admission", "process_start_revalidation", "execution", "outcome_verification"},
		ProposedEffects: []string{},
	}
	result.Preview = p
	result.Warnings[0] = "read-only preview; no lifecycle or detached-recovery mutation"
	result.PostCounts = &report.Counts
	result.WaitReason = "preview"
	if opts.MaxExecuteSteps > 0 {
		launches, err := ListDetachedLaunches(s.home)
		if err != nil {
			return result, err
		}
		p.ChecksPerformed = append(p.ChecksPerformed, "detached_records_read")
		for _, launch := range launches {
			if launch.Outcome == DetachedOutcomeReserved || launch.Outcome == DetachedOutcomeRunning {
				p.WorkID, p.Action = launch.WorkID, "reconcile_detached"
				p.ProposedEffects = []string{"reconcile_detached_before_selecting_lifecycle_work"}
				result.NoAction = "detached reconciliation is pending; preview does not perform recovery"
				return result, nil
			}
		}
	}
	if continueExecuteStageThreshold(report) {
		stage, err := s.executeStage(now, opts, result.Actor)
		if err != nil {
			return result, err
		}
		result.Skipped, result.Lane, result.Human = stage.Skipped, stage.Lane, stage.Human
		result.WorkID, result.PolicyDecisionID, result.Refused = stage.WorkID, stage.PolicyDecisionID, stage.Refused
		p.ChecksPerformed = append(p.ChecksPerformed, "execution_gates_to_reported_verdict")
		if stage.Skipped == "would_execute" {
			p.Status, p.Action, p.WorkID = "eligible", "execute", stage.WorkID
			p.ExecutionChoice = stage.executionChoice
			p.ChecksPerformed = append(p.ChecksPerformed, "adapter_workspace_launch_readiness")
			p.ProposedEffects = []string{"claim", "governed_execution"}
			result.Execute = buildDryRunVerdict(stage)
			return result, nil
		}
		result.Execute = buildExecuteVerdict(stage, "refused")
		if stage.Skipped == "capacity_unknown" || isPreflightRefusalReason(stage.Skipped) || executeStageRefusedHeadUntouchable(stage) {
			p.Status, p.WorkID = "refused", stage.WorkID
			result.Status = "refused"
			return result, nil
		}
	}
	if report.DispatchNext.CanRun && continueMatchesExpected(result.ExpectWorkID, report.DispatchNext.WorkID) {
		p.WorkID, p.Action = report.DispatchNext.WorkID, report.DispatchNext.Action
		workID, err := s.previewDispatchTarget(now, opts, report, result.Actor)
		if err != nil {
			return result, err
		}
		p.ChecksPerformed = append(p.ChecksPerformed, "lifecycle_policy_plan")
		p.Status = "refused"
		if workID != "" {
			p.Status, p.WorkID = "eligible", workID
			p.ProposedEffects = []string{p.Action}
		} else {
			result.Status = "refused"
			result.NoAction = "no selected lifecycle target passed its policy plan"
		}
	} else if item, _, blocked := s.continuePrepareCandidate(now, report, result.Actor, result.ExpectWorkID); item != nil {
		p.Status, p.WorkID, p.Action = "eligible", item.WorkID, "prepare"
		p.ProposedEffects = []string{"advance_to_ready"}
		result.BlockedOn = blocked
	} else if admission := continueAdmissionPreview(report.AdmissionPreviews, result.ExpectWorkID); admission != nil {
		p.Status, p.WorkID, p.Action = "eligible", admission.WorkID, "admit"
		p.ProposedEffects = []string{"admit_create_plan"}
	}
	result.WorkID = p.WorkID
	if p.Status == "eligible" {
		result.Human = fmt.Sprintf("would %s %s; fresh policy and generation checks required at mutation", p.Action, p.WorkID)
	} else if result.NoAction == "" {
		result.NoAction = continueNoAction(report, result.ExpectWorkID)
	}
	return result, nil
}

// Read the same policy plans as dispatch, without recording diagnostic plans or
// mutating lifecycle state. Unpinned selection may skip a blocked candidate.
func (s *Store) previewDispatchTarget(now time.Time, opts ContinueOptions, report WorkbenchReport, actor string) (string, error) {
	if report.DispatchNext.Action == "claim" {
		plan, err := s.DispatchPlan(now, DispatchPlanOptions{Actor: actor, WorkID: opts.ExpectWorkID, RecentLimit: opts.RecentLimit, ReadOnly: true})
		if err != nil {
			return "", err
		}
		if plan.Selected != nil {
			return plan.Selected.WorkID, nil
		}
		return "", nil
	}
	for _, item := range report.Open {
		if !continueMatchesExpected(opts.ExpectWorkID, item.WorkID) {
			continue
		}
		switch report.DispatchNext.Action {
		case "advance":
			if dispatchAdvanceCandidate(item, actor).Action != "advance_running" {
				continue
			}
			plan, err := s.AdvancePlan(item.WorkID, now, AdvancePlanOptions{Actor: actor, To: "running"})
			if err == nil && plan.Policy.Allowed && plan.NoAction == "" {
				return item.WorkID, nil
			}
		case "close":
			outcome, moveType := strings.TrimSpace(opts.Outcome), strings.TrimSpace(opts.MoveType)
			if outcome == "" {
				outcome = "completed"
			}
			if moveType == "" {
				moveType = "control_plane_repair"
			}
			if dispatchClosePlanCandidate(item, actor, outcome, moveType).Action != "plan_close" {
				continue
			}
			plan, err := s.dispatchClosePlan(item.WorkID, now, DispatchClosePlanOptions{}, actor, outcome, moveType, false)
			if err == nil && plan.Policy.Allowed && plan.NoAction == "" {
				return item.WorkID, nil
			}
		}
	}
	return "", nil
}

func dispatchSnapshot(report WorkbenchReport, workID string) WorkbenchItem {
	for _, item := range report.Open {
		if item.WorkID == workID {
			return item
		}
	}
	return WorkbenchItem{WorkID: workID}
}
