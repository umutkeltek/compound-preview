package work

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
)

const ActionExecutionPlanSchemaVersion = "compound.work_action_execution_plan.v0.1"

type ActionExecutionPlanOptions struct {
	Actor          string
	LaneID         string
	Capability     string
	Action         string
	MutationClass  string
	Reversibility  string
	BlastRadius    string
	TargetKind     string
	TargetID       string
	TargetPath     string
	MoveType       string
	ReceiptOutcome string
	Record         bool
}

type ActionExecutionPolicySummary struct {
	ActionIntentID   string   `json:"action_intent_id"`
	PolicyDecisionID string   `json:"policy_decision_id"`
	PolicySource     string   `json:"policy_source,omitempty"`
	Outcome          string   `json:"outcome"`
	Allowed          bool     `json:"allowed"`
	StandingGrantID  string   `json:"standing_grant_id,omitempty"`
	ExpiresAt        string   `json:"expires_at,omitempty"`
	Reasons          []string `json:"reasons,omitempty"`
	Obligations      []string `json:"obligations,omitempty"`
	EvidenceRefs     []string `json:"evidence_refs,omitempty"`
}

type ActionExecutionPlan struct {
	SchemaVersion      string                       `json:"schema_version"`
	ID                 string                       `json:"id"`
	GeneratedAt        string                       `json:"generated_at"`
	Status             string                       `json:"status"`
	Mode               string                       `json:"mode"`
	Actor              string                       `json:"actor"`
	WorkID             string                       `json:"work_id"`
	WorkState          string                       `json:"work_state"`
	WorkOwner          string                       `json:"work_owner,omitempty"`
	PlanPath           string                       `json:"plan_path,omitempty"`
	ActionSummary      string                       `json:"action_summary,omitempty"`
	ActionCommand      []string                     `json:"action_command,omitempty"`
	RecordCommand      []string                     `json:"record_command,omitempty"`
	CloseCommand       []string                     `json:"close_command,omitempty"`
	RequiredInputs     []string                     `json:"required_inputs,omitempty"`
	ExpectedValidation []string                     `json:"expected_validation,omitempty"`
	StopConditions     []string                     `json:"stop_conditions,omitempty"`
	Boundary           string                       `json:"boundary,omitempty"`
	Proof              string                       `json:"proof,omitempty"`
	RollbackPlan       string                       `json:"rollback_plan,omitempty"`
	EscalationNotes    []string                     `json:"escalation_notes,omitempty"`
	ProofRefs          []string                     `json:"proof_refs,omitempty"`
	Policy             ActionExecutionPolicySummary `json:"policy"`
	ActionIntent       envelope.ActionIntent        `json:"action_intent"`
	Decision           envelope.PolicyDecision      `json:"policy_decision"`
	ReceiptDraft       envelope.Receipt             `json:"receipt_draft"`
	ActionContext      *WorkActionContext           `json:"action_context,omitempty"`
	NoAction           string                       `json:"no_action,omitempty"`
	Warnings           []string                     `json:"warnings,omitempty"`
}

func (s *Store) ActionExecutionPlan(workID string, now time.Time, opts ActionExecutionPlanOptions) (ActionExecutionPlan, error) {
	workID = strings.TrimSpace(workID)
	if workID == "" {
		return ActionExecutionPlan{}, fmt.Errorf("action-plan requires <work_id>")
	}
	w, err := s.Get(workID)
	if err != nil {
		return ActionExecutionPlan{}, err
	}
	producerLaneID := strings.TrimSpace(opts.LaneID)
	if producerLaneID == "" {
		producerLaneID = strings.TrimSpace(opts.Actor)
	}
	actor := strings.TrimSpace(opts.Actor)
	if actor == "" {
		actor = "codex"
	}
	actionContext, contextWarnings := s.actionContextForWork(w)
	plan := ActionExecutionPlan{
		SchemaVersion: ActionExecutionPlanSchemaVersion,
		ID:            idFor("waep_", workID, actor, ts(now)),
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Status:        "not_executable",
		Mode:          "proposal_only",
		Actor:         actor,
		WorkID:        w.ID,
		WorkState:     w.State,
		WorkOwner:     w.Owner,
		ActionContext: cloneWorkActionContext(actionContext),
		Warnings: []string{
			"does not execute action_context command",
			"does not record performed action",
			"does not close work or emit receipt/transition",
			"use record_command only after command execution and expected validation pass",
		},
	}
	for _, warning := range contextWarnings {
		plan.Warnings = append(plan.Warnings, "action context: "+warning)
	}
	if actionContext == nil {
		plan.NoAction = "work has no admitted action_context"
		return s.maybeRecordActionExecutionPlan(plan, opts.Record)
	}
	plan.ActionSummary = strings.TrimSpace(actionContext.ActionSummary)
	plan.ActionCommand = append([]string{}, actionContext.Command...)
	plan.RequiredInputs = append([]string{}, actionContext.RequiredInputs...)
	plan.ExpectedValidation = append([]string{}, actionContext.ExpectedValidation...)
	plan.StopConditions = append([]string{}, actionContext.StopConditions...)
	plan.Boundary = actionContext.Boundary
	plan.Proof = actionContext.Proof
	plan.ProofRefs = actionContextProofRefs(w.ID, actionContext)
	if len(plan.ActionCommand) == 0 {
		plan.NoAction = "action_context has no command"
		return s.maybeRecordActionExecutionPlan(plan, opts.Record)
	}
	if w.State != "claimed" && w.State != "running" {
		plan.NoAction = fmt.Sprintf("action execution requires claimed or running work, got %q", w.State)
		return s.maybeRecordActionExecutionPlan(plan, opts.Record)
	}
	ai := s.actionExecutionIntent(w, actionContext, actor, now, opts)
	if err := s.validateSourceActionAdmission(w, ai); err != nil {
		return ActionExecutionPlan{}, err
	}
	cfg, source, err := policy.LoadConfig(s.home)
	if err != nil {
		return ActionExecutionPlan{}, err
	}
	pd := policy.Evaluate(ai, cfg, now)
	if err := ai.Validate(); err != nil {
		return ActionExecutionPlan{}, err
	}
	if err := pd.Validate(); err != nil {
		return ActionExecutionPlan{}, err
	}
	moveType := orDefault(opts.MoveType, "outward_delta")
	outcome := orDefault(opts.ReceiptOutcome, "completed")
	receipt := s.actionExecutionReceiptDraft(w, actionContext, actor, producerLaneID, now, moveType, outcome, plan.ProofRefs)
	if err := receipt.Validate(); err != nil {
		return ActionExecutionPlan{}, fmt.Errorf("receipt draft invalid: %w", err)
	}
	plan.Policy = summarizeActionExecutionPolicy(pd, source)
	plan.ActionIntent = ai
	plan.Decision = pd
	plan.ReceiptDraft = receipt
	plan.RollbackPlan = ai.RollbackPlan
	plan.EscalationNotes = []string{
		"stop if any stop_condition is true",
		"stop if required inputs are missing",
		"stop if expected validation cannot be produced",
		"escalate before public, financial, destructive, security, or external-account side effects outside boundary",
	}
	if policy.Allowed(pd) {
		plan.Status = "action_execution_policy_allowed"
		plan.CloseCommand = []string{"compound", "work", "close", w.ID, "--outcome", outcome, "--move-type", moveType, "--by", actor}
		if len(plan.ProofRefs) > 0 {
			plan.CloseCommand = append(plan.CloseCommand, "--proof", strings.Join(plan.ProofRefs, ","))
		}
	} else {
		plan.Status = "action_execution_policy_blocked"
		plan.NoAction = "action execution lifecycle-ready but policy does not allow it"
	}
	return s.maybeRecordActionExecutionPlan(plan, opts.Record)
}

func (s *Store) maybeRecordActionExecutionPlan(plan ActionExecutionPlan, record bool) (ActionExecutionPlan, error) {
	if !record {
		return plan, nil
	}
	path, err := s.RecordActionExecutionPlan(plan)
	if err != nil {
		return ActionExecutionPlan{}, err
	}
	plan.PlanPath = path
	if plan.Status == "action_execution_policy_allowed" {
		plan.RecordCommand = []string{"compound", "work", "act", plan.WorkID, "--plan", path}
		plan.CloseCommand = []string{"compound", "work", "close-plan", plan.WorkID, "--action-plan", path, "--actor", plan.Actor}
		if err := s.writeJSON(path, &plan); err != nil {
			return ActionExecutionPlan{}, err
		}
	}
	return plan, nil
}

func (s *Store) RecordActionExecutionPlan(plan ActionExecutionPlan) (string, error) {
	if strings.TrimSpace(plan.ID) == "" {
		return "", os.ErrInvalid
	}
	dir := filepath.Join(s.home, "action-execution-plans")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, plan.ID+".json")
	if err := s.writeJSON(path, &plan); err != nil {
		return "", err
	}
	return path, nil
}

func LoadActionExecutionPlan(path string) (ActionExecutionPlan, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return ActionExecutionPlan{}, fmt.Errorf("action execution plan path required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ActionExecutionPlan{}, err
	}
	var plan ActionExecutionPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return plan, fmt.Errorf("parse action execution plan %s: %w", path, err)
	}
	if plan.SchemaVersion != ActionExecutionPlanSchemaVersion {
		return plan, fmt.Errorf("not an action execution plan: schema_version=%q", plan.SchemaVersion)
	}
	if strings.TrimSpace(plan.ID) == "" || strings.TrimSpace(plan.WorkID) == "" {
		return plan, fmt.Errorf("action execution plan missing id/work_id")
	}
	return plan, nil
}

func (s *Store) actionExecutionIntent(w envelope.WorkEnvelope, ctx *WorkActionContext, actor string, now time.Time, opts ActionExecutionPlanOptions) envelope.ActionIntent {
	targetKind := strings.TrimSpace(opts.TargetKind)
	if targetKind == "" {
		targetKind = strings.TrimSpace(ctx.Kind)
	}
	if targetKind == "" {
		targetKind = "action_context"
	}
	targetID := strings.TrimSpace(opts.TargetID)
	if targetID == "" {
		targetID = strings.TrimSpace(ctx.SourceID)
	}
	if targetID == "" {
		targetID = w.ID
	}
	targetPath := strings.TrimSpace(opts.TargetPath)
	if targetPath == "" {
		targetPath = strings.TrimSpace(ctx.SourceRef)
	}
	capability := orDefault(opts.Capability, "work.action_context.execute")
	action := orDefault(opts.Action, "execute_action_context")
	mutationClass := orDefault(opts.MutationClass, "local_reversible")
	reversibility := orDefault(opts.Reversibility, "manual")
	blastRadius := orDefault(opts.BlastRadius, "local")
	command := strings.Join(ctx.Command, " ")
	rollback := "stop, preserve trace, and close failed if validation does not match action_context proof"
	if strings.TrimSpace(ctx.Boundary) != "" {
		rollback = "stay inside boundary: " + strings.TrimSpace(ctx.Boundary)
	}
	return envelope.ActionIntent{
		Header: newHeader(
			envelope.SVActionIntent,
			idFor("act_", capability, action, targetKind, targetID, targetPath, mutationClass, reversibility, blastRadius, actor, command, ts(now)),
			now,
			w.ID,
			w.IDs.ContextPackID,
			w.Privacy,
			w.Risk,
		),
		Actor:         actor,
		Capability:    capability,
		Action:        action,
		Target:        envelope.ActionTarget{Kind: targetKind, ID: targetID, Path: targetPath},
		MutationClass: mutationClass,
		Reversibility: reversibility,
		BlastRadius:   blastRadius,
		Preview:       "execute reviewed action_context command: " + command,
		RollbackPlan:  rollback,
	}
}

func (s *Store) actionExecutionReceiptDraft(w envelope.WorkEnvelope, ctx *WorkActionContext, actor, producerLaneID string, now time.Time, moveType, outcome string, proofRefs []string) envelope.Receipt {
	summary := "expected after executing action_context command"
	if strings.TrimSpace(ctx.Proof) != "" {
		summary += ": " + strings.TrimSpace(ctx.Proof)
	}
	receipt := envelope.Receipt{
		Header: newHeader(
			envelope.SVReceipt,
			idFor("rec_", w.ID, actor, strings.Join(ctx.Command, " "), moveType, outcome, ts(now)),
			now,
			w.ID,
			w.IDs.ContextPackID,
			w.Privacy,
			w.Risk,
		),
		Outcome:  outcome,
		MoveType: moveType,
		StateDelta: envelope.StateDelta{
			Changed:      false,
			Summary:      summary,
			Verification: append([]string{}, ctx.ExpectedValidation...),
		},
		Cost:      envelope.ReceiptCost{},
		Skills:    envelope.ReceiptSkills{},
		Memory:    envelope.ReceiptMemory{},
		Next:      envelope.ReceiptNext{ReviewTrigger: strings.Join(ctx.StopConditions, "; ")},
		ProofRefs: append([]string{}, proofRefs...),
	}
	if strings.TrimSpace(producerLaneID) != "" {
		receipt.Producer.LaneID = strings.TrimSpace(producerLaneID)
	}
	return receipt
}

func actionContextProofRefs(workID string, ctx *WorkActionContext) []string {
	var refs []string
	if strings.TrimSpace(ctx.SourceRef) != "" {
		refs = append(refs, strings.TrimSpace(ctx.SourceRef))
	}
	refs = append(refs, ctx.EvidenceRefs...)
	if strings.TrimSpace(ctx.SourceID) != "" {
		refs = append(refs, "action_context:"+strings.TrimSpace(ctx.SourceID))
	}
	if len(mergeUnique(refs)) == 0 {
		refs = append(refs, "work:"+workID+":action_context")
	}
	return mergeUnique(refs)
}

func summarizeActionExecutionPolicy(pd envelope.PolicyDecision, source string) ActionExecutionPolicySummary {
	if strings.TrimSpace(source) == "" {
		source = "builtin:default"
	}
	return ActionExecutionPolicySummary{
		ActionIntentID:   pd.ActionIntentID,
		PolicyDecisionID: pd.ID,
		PolicySource:     source,
		Outcome:          pd.Outcome,
		Allowed:          policy.Allowed(pd),
		StandingGrantID:  pd.StandingGrantID,
		ExpiresAt:        pd.ExpiresAt,
		Reasons:          append([]string{}, pd.Reasons...),
		Obligations:      append([]string{}, pd.Obligations...),
		EvidenceRefs:     append([]string{}, pd.EvidenceRefs...),
	}
}
