package work

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/policy"
)

const ClosePlanSchemaVersion = "compound.work_close_plan.v0.1"

type ClosePlanOptions struct {
	Actor          string
	Outcome        string
	MoveType       string
	ProofRefs      []string
	ActionPlanPath string
	Capability     string
	Action         string
	MutationClass  string
	Reversibility  string
	BlastRadius    string
	TargetKind     string
	TargetID       string
	TargetPath     string
	Record         bool
	// Steal and StealReason preview the close-gate lease escape hatch (see CloseInput in
	// work.go): when set with a non-empty reason, the plan reports the close as steal-ready
	// (and its CloseCommand carries --steal --reason) instead of held-by-other. ClosePlan
	// never mutates or records anything for this preview — only Close (work.go) does.
	Steal       bool
	StealReason string
	// FrozenActionIntentID is internal recovery input. An exact prepared
	// RunAttempt may carry the legacy delimiter-joined close identity minted
	// before length framing shipped; ordinary callers leave this empty.
	FrozenActionIntentID string
}

type ClosePolicySummary struct {
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

// ClosePrecondition is one named close-gate requirement reported on a ClosePlan: Met is
// the verdict for THIS request and Detail states what the gate requires (which work
// states may close to the requested outcome, which move types are accepted, who holds
// the lease), so an unmet check teaches the remedy instead of hiding behind the
// not_closeable verdict — every unmet precondition is readable in one plan.
type ClosePrecondition struct {
	Name   string `json:"name"`
	Met    bool   `json:"met"`
	Detail string `json:"detail,omitempty"`
}

type ClosePlan struct {
	SchemaVersion           string                  `json:"schema_version"`
	ID                      string                  `json:"id"`
	GeneratedAt             string                  `json:"generated_at"`
	Status                  string                  `json:"status"`
	Mode                    string                  `json:"mode"`
	Actor                   string                  `json:"actor"`
	WorkID                  string                  `json:"work_id"`
	WorkState               string                  `json:"work_state"`
	Outcome                 string                  `json:"outcome"`
	MoveType                string                  `json:"move_type"`
	WorkOwner               string                  `json:"work_owner,omitempty"`
	PlanPath                string                  `json:"plan_path,omitempty"`
	CloseCommand            []string                `json:"close_command,omitempty"`
	ProofRefs               []string                `json:"proof_refs,omitempty"`
	ArtifactIDs             []string                `json:"artifact_ids,omitempty"`
	VerificationIDs         []string                `json:"verification_ids,omitempty"`
	RunObservations         []RunObservationSummary `json:"run_observations,omitempty"`
	ActionExecutionPlanPath string                  `json:"action_execution_plan_path,omitempty"`
	ActionExecutionPlanID   string                  `json:"action_execution_plan_id,omitempty"`
	ExpectedValidation      []string                `json:"expected_validation,omitempty"`
	ReceiptDraft            envelope.Receipt        `json:"receipt_draft,omitempty"`
	Policy                  ClosePolicySummary      `json:"policy"`
	ActionIntent            envelope.ActionIntent   `json:"action_intent"`
	Decision                envelope.PolicyDecision `json:"policy_decision"`
	NoAction                string                  `json:"no_action,omitempty"`
	Warnings                []string                `json:"warnings,omitempty"`
	Checks                  []ClosePrecondition     `json:"checks,omitempty"`
}

func (s *Store) ClosePlan(workID string, now time.Time, opts ClosePlanOptions) (ClosePlan, error) {
	workID = strings.TrimSpace(workID)
	if workID == "" {
		return ClosePlan{}, fmt.Errorf("close-plan requires <work_id>")
	}

	actionPlanPath := strings.TrimSpace(opts.ActionPlanPath)
	var actionPlan *ActionExecutionPlan
	if actionPlanPath != "" {
		loaded, err := LoadActionExecutionPlan(actionPlanPath)
		if err != nil {
			return ClosePlan{}, err
		}
		if loaded.WorkID != workID {
			return ClosePlan{}, fmt.Errorf("action execution plan belongs to work %s, not %s", loaded.WorkID, workID)
		}
		if err := loaded.ReceiptDraft.Validate(); err != nil {
			return ClosePlan{}, fmt.Errorf("action execution plan receipt draft invalid: %w", err)
		}
		actionPlan = &loaded
	}

	outcome := strings.TrimSpace(opts.Outcome)
	if outcome == "" && actionPlan != nil {
		outcome = strings.TrimSpace(actionPlan.ReceiptDraft.Outcome)
	}
	if outcome == "" {
		return ClosePlan{}, fmt.Errorf("close-plan requires --outcome <state> or --action-plan with receipt_draft.outcome")
	}
	moveType := strings.TrimSpace(opts.MoveType)
	if moveType == "" && actionPlan != nil {
		moveType = strings.TrimSpace(actionPlan.ReceiptDraft.MoveType)
	}
	if moveType == "" {
		return ClosePlan{}, fmt.Errorf("close-plan requires --move-type <type> or --action-plan with receipt_draft.move_type")
	}
	if actionPlan != nil {
		if strings.TrimSpace(opts.Outcome) != "" && strings.TrimSpace(actionPlan.ReceiptDraft.Outcome) != "" && outcome != actionPlan.ReceiptDraft.Outcome {
			return ClosePlan{}, fmt.Errorf("close-plan outcome %q does not match action execution receipt draft outcome %q", outcome, actionPlan.ReceiptDraft.Outcome)
		}
		if strings.TrimSpace(opts.MoveType) != "" && strings.TrimSpace(actionPlan.ReceiptDraft.MoveType) != "" && moveType != actionPlan.ReceiptDraft.MoveType {
			return ClosePlan{}, fmt.Errorf("close-plan move_type %q does not match action execution receipt draft move_type %q", moveType, actionPlan.ReceiptDraft.MoveType)
		}
	}

	w, err := s.Get(workID)
	if err != nil {
		return ClosePlan{}, err
	}
	actor := strings.TrimSpace(opts.Actor)
	if actor == "" {
		actor = "codex"
	}
	leaseStatus := s.leaseHolderStatusFor(w.ID, actor, opts.Steal, opts.StealReason, "close")
	ai := s.closeActionIntent(w, actor, outcome, moveType, now, opts, leaseStatus)
	if err := ai.Validate(); err != nil {
		return ClosePlan{}, err
	}
	if frozen := strings.TrimSpace(opts.FrozenActionIntentID); frozen != "" && ai.ID != frozen {
		return ClosePlan{}, fmt.Errorf("prepared close action identity %s is incompatible with current close input", frozen)
	}
	cfg, source, err := policy.LoadConfig(s.home)
	if err != nil {
		return ClosePlan{}, err
	}
	pd := policy.Evaluate(ai, cfg, now)
	// Close recovery seals every authority object and the lifecycle event to one
	// exact instant. Policy's generic header is second-resolution, so finalize
	// this close-specific decision from the already validated ActionIntent clock
	// before its final validation and persistence.
	pd.CreatedAt = ai.CreatedAt
	pd.IDs.ContextPackID = ai.IDs.ContextPackID
	if err := pd.Validate(); err != nil {
		return ClosePlan{}, err
	}
	observations, observationWarnings := s.workbenchRunObservations(w.ID)
	proofRefs := mergeUnique(w.ArtifactIDs, w.VerificationIDs, landCheckEvidenceRefs(w.EvidenceRefs), opts.ProofRefs)
	var expectedValidation []string
	var receiptDraft envelope.Receipt
	if actionPlan != nil {
		proofRefs = mergeUnique(proofRefs, actionPlan.ProofRefs, actionPlan.ReceiptDraft.ProofRefs)
		expectedValidation = append([]string{}, actionPlan.ReceiptDraft.StateDelta.Verification...)
		receiptDraft = actionPlan.ReceiptDraft
	}
	deliveryNoAction := ""
	if _, err := s.gitDeliveryProofForClose(w, outcome, moveType, proofRefs, now); err != nil {
		deliveryNoAction = err.Error()
	}
	// leaseHolderStatusFor is read-only, so computing it before the plan literal lets every
	// returned plan (including the action-plan early return below) carry the full
	// precondition enumeration, not just the first refusal NoAction names.
	plan := ClosePlan{
		SchemaVersion:      ClosePlanSchemaVersion,
		ID:                 idFor("wclp_", workID, actor, outcome, moveType, ai.ID, ts(now)),
		GeneratedAt:        now.UTC().Format(time.RFC3339),
		Status:             "not_closeable",
		Mode:               "proposal_only",
		Actor:              actor,
		WorkID:             w.ID,
		WorkState:          w.State,
		Outcome:            outcome,
		MoveType:           moveType,
		WorkOwner:          w.Owner,
		ProofRefs:          proofRefs,
		ArtifactIDs:        append([]string{}, w.ArtifactIDs...),
		VerificationIDs:    append([]string{}, w.VerificationIDs...),
		RunObservations:    observations,
		ExpectedValidation: expectedValidation,
		ReceiptDraft:       receiptDraft,
		Policy:             summarizeClosePolicy(pd, source),
		ActionIntent:       ai,
		Decision:           pd,
		Warnings: []string{
			"does not close work",
			"does not emit receipt or transition",
			"does not enforce policy; it reports close gate decision",
		},
		Checks: s.closePreconditionChecks(w, outcome, moveType, actor, leaseStatus),
	}
	if actionPlan != nil {
		plan.ActionExecutionPlanPath = actionPlanPath
		plan.ActionExecutionPlanID = actionPlan.ID
		plan.Warnings = append(plan.Warnings, "action execution receipt draft consumed; close only after expected validation is satisfied")
	}
	for _, warning := range observationWarnings {
		plan.Warnings = append(plan.Warnings, "run observation: "+warning)
	}
	if (outcome == "completed" || outcome == "failed") && len(proofRefs) == 0 && len(observations) == 0 {
		plan.Warnings = append(plan.Warnings, "no artifact, verification, or observed-run evidence attached")
	}
	if actionPlan != nil {
		switch {
		case strings.TrimSpace(actionPlan.ActionIntent.ID) == "":
			plan.NoAction = "action execution plan missing action_intent.id"
		case !stringIn(w.ActionIntentIDs, actionPlan.ActionIntent.ID):
			plan.NoAction = fmt.Sprintf("action execution plan %s not recorded on work; run: compound work act %s --plan %s", actionPlan.ID, w.ID, actionPlanPath)
		case actionPlan.Status != "action_execution_policy_allowed":
			plan.NoAction = fmt.Sprintf("action execution plan status %q is not closeable", actionPlan.Status)
		}
		if plan.NoAction != "" {
			if opts.Record {
				path, err := s.RecordClosePlan(plan)
				if err != nil {
					return ClosePlan{}, err
				}
				plan.PlanPath = path
			}
			return plan, nil
		}
	}
	heldReleaseErr := heldReleaseOperatorError(w.State, outcome, actor)
	switch {
	case !terminalStates[outcome]:
		plan.NoAction = fmt.Sprintf("invalid close outcome %q", outcome)
	case !oneOf(moveType, envelope.MoveTypes):
		plan.NoAction = fmt.Sprintf("invalid move_type %q", moveType)
	case deliveryNoAction != "":
		plan.NoAction = deliveryNoAction
	case heldReleaseErr != nil:
		plan.NoAction = heldReleaseErr.Error()
	case leaseStatus.Blocked:
		plan.NoAction = leaseStatus.Message
	case w.State == outcome || canTransition(w.State, outcome):
		plan.CloseCommand = []string{"compound", "work", "close", w.ID, "--outcome", outcome, "--move-type", moveType, "--by", actor}
		if leaseStatus.Stolen {
			plan.CloseCommand = append(plan.CloseCommand, "--steal", "--reason", opts.StealReason)
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("lease held by %q; close command carries --steal --reason", leaseStatus.Holder))
		}
		if len(proofRefs) > 0 {
			plan.CloseCommand = append(plan.CloseCommand, "--proof", strings.Join(proofRefs, ","))
		}
		if policy.Allowed(pd) {
			plan.Status = "close_policy_allowed"
		} else {
			plan.Status = "close_policy_blocked"
			plan.NoAction = "close lifecycle-ready but policy does not allow it"
		}
	default:
		plan.NoAction = fmt.Sprintf("cannot close from state %q to %q", w.State, outcome)
	}
	if opts.Record {
		path, err := s.RecordClosePlan(plan)
		if err != nil {
			return ClosePlan{}, err
		}
		plan.PlanPath = path
	}
	return plan, nil
}

func stringIn(set []string, v string) bool {
	for _, item := range set {
		if item == v {
			return true
		}
	}
	return false
}

func (s *Store) RecordClosePlan(plan ClosePlan) (string, error) {
	if strings.TrimSpace(plan.ID) == "" {
		return "", os.ErrInvalid
	}
	dir, err := home.HabitatPath(s.home, home.HabitatClosePlans)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, plan.ID+".json")
	if err := s.writeJSON(path, &plan); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) closeActionIntent(w envelope.WorkEnvelope, actor, outcome, moveType string, now time.Time, opts ClosePlanOptions, leaseStatus leaseHolderStatus) envelope.ActionIntent {
	targetID := strings.TrimSpace(opts.TargetID)
	if targetID == "" {
		targetID = w.ID
	}
	targetPath := strings.TrimSpace(opts.TargetPath)
	if targetPath == "" {
		targetPath = s.workPath(w.ID)
	}
	capability := orDefault(opts.Capability, "work.close")
	action := orDefault(opts.Action, "close_work_as_"+outcome)
	targetKind := orDefault(opts.TargetKind, "work")
	mutationClass := orDefault(opts.MutationClass, "local_reversible")
	reversibility := orDefault(opts.Reversibility, "manual")
	blastRadius := orDefault(opts.BlastRadius, "local")
	stealReason := strings.TrimSpace(opts.StealReason)
	preview := fmt.Sprintf("close work %s from %s to %s move_type %s actor %s", w.ID, w.State, outcome, moveType, actor)
	if opts.Steal {
		preview += fmt.Sprintf(" lease_override_from %q reason %q", leaseStatus.Holder, stealReason)
	}
	identityParts := []string{
		capability,
		action,
		targetKind,
		targetID,
		targetPath,
		mutationClass,
		reversibility,
		blastRadius,
		actor,
		outcome,
		moveType,
		fmt.Sprintf("%t", opts.Steal),
		leaseStatus.Holder,
		stealReason,
		ts(now),
	}
	actionID := closeActionIntentIDFor(identityParts...)
	if frozen := strings.TrimSpace(opts.FrozenActionIntentID); frozen != "" {
		legacyID := idFor("act_", identityParts...)
		if frozen == actionID || frozen == legacyID {
			actionID = frozen
		}
	}
	return envelope.ActionIntent{
		Header: newCloseHeader(
			envelope.SVActionIntent,
			actionID,
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
		Preview:       preview,
		RollbackPlan:  fmt.Sprintf("reopen work from %s if receipt/transition proof is invalid", outcome),
	}
}

// closeActionIntentIDFor frames every field before using the package's stable
// digest helper. Close authority contains operator-provided holder and reason
// strings, so a bare "|" join can map two different authority tuples to the
// same ActionIntent identity.
func closeActionIntentIDFor(parts ...string) string {
	framed := make([]string, len(parts))
	for i, part := range parts {
		framed[i] = strconv.Itoa(len(part)) + ":" + part
	}
	return idFor("act_", framed...)
}

func summarizeClosePolicy(pd envelope.PolicyDecision, source string) ClosePolicySummary {
	if strings.TrimSpace(source) == "" {
		source = "builtin:default"
	}
	return ClosePolicySummary{
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

// closeSourceStates returns the work states a close to outcome may legally start from —
// the states holding a live state-machine edge to it (for "completed": running or
// verifying). Sorted so the requirement reads deterministically.
func closeSourceStates(outcome string) []string {
	var states []string
	for from := range transitions {
		if canTransition(from, outcome) {
			states = append(states, from)
		}
	}
	sort.Strings(states)
	return states
}

// closePreconditionChecks enumerates the close gate's named preconditions for one request,
// each with its met/unmet verdict and a Detail stating what the gate requires, so a
// not_closeable plan lists EVERY unmet precondition in one read instead of surfacing one
// refusal per trial-and-error re-run. NoAction keeps reporting the first refusal; Checks
// is the full enumeration. The verdicts mirror the ClosePlan switch: work_state needs a
// live edge (or an already-terminal match) to the requested outcome, move_type must be
// one of envelope.MoveTypes, and a lease held by another actor requires --by <holder> or
// --steal --reason.
func (s *Store) closePreconditionChecks(w envelope.WorkEnvelope, outcome, moveType, actor string, leaseStatus leaseHolderStatus) []ClosePrecondition {
	stateMet := w.State == outcome || canTransition(w.State, outcome)
	stateDetail := fmt.Sprintf("work state %q can close to %q", w.State, outcome)
	switch {
	case !terminalStates[outcome]:
		stateMet = false
		stateDetail = fmt.Sprintf("outcome %q is not a terminal close outcome; close outcomes are completed, failed, cancelled", outcome)
	case !stateMet:
		stateDetail = fmt.Sprintf("work state %q cannot close to %q; closing to %q requires work state %s", w.State, outcome, outcome, strings.Join(closeSourceStates(outcome), " or "))
	}

	moveMet := oneOf(moveType, envelope.MoveTypes)
	moveDetail := fmt.Sprintf("move_type %q is an accepted move type", moveType)
	if !moveMet {
		moveDetail = fmt.Sprintf("move_type %q is not an accepted move type; accepted move types: %s", moveType, strings.Join(envelope.MoveTypes, ", "))
	}

	var leaseDetail string
	switch {
	case leaseStatus.Blocked:
		leaseDetail = leaseStatus.Message
	case leaseStatus.Stolen:
		leaseDetail = fmt.Sprintf("--steal --reason overrides lease holder %q", leaseStatus.Holder)
	default:
		if lease, err := s.readLease(w.ID); err == nil && strings.TrimSpace(lease.By) != "" {
			leaseDetail = fmt.Sprintf("--by %q matches lease holder %q", actor, strings.TrimSpace(lease.By))
		} else {
			leaseDetail = "no active lease; any --by actor may close"
		}
	}

	return []ClosePrecondition{
		{Name: "work_state", Met: stateMet, Detail: stateDetail},
		{Name: "move_type", Met: moveMet, Detail: moveDetail},
		{Name: "lease_holder", Met: !leaseStatus.Blocked, Detail: leaseDetail},
	}
}
