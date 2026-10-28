package work

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
)

const AdvancePlanSchemaVersion = "compound.work_advance_plan.v0.1"

type AdvancePlanOptions struct {
	proofAttachmentRef string
	NonSourceDelivery  *ActionDeliveryRequest
	Actor              string
	To                 string
	GitDelivery        *envelope.GitDeliveryContract
	Capability         string
	Action             string
	MutationClass      string
	Reversibility      string
	BlastRadius        string
	TargetKind         string
	TargetID           string
	TargetPath         string
	Record             bool
	// Steal and StealReason preview the advance-gate lease escape hatch (see AdvanceInput in
	// work.go): when set with a non-empty reason, the plan reports the advance as
	// steal-ready (and its AdvanceCommand carries --steal --reason) instead of
	// held-by-other. AdvancePlan never mutates or records anything for this preview — only
	// Advance (work.go) does.
	Steal       bool
	StealReason string
}

type AdvancePolicySummary struct {
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

type AdvancePlan struct {
	NonSourceDelivery *envelope.ActionDeliveryBinding `json:"non_source_delivery,omitempty"`
	SchemaVersion     string                          `json:"schema_version"`
	ID                string                          `json:"id"`
	GeneratedAt       string                          `json:"generated_at"`
	Status            string                          `json:"status"`
	Mode              string                          `json:"mode"`
	Actor             string                          `json:"actor"`
	WorkID            string                          `json:"work_id"`
	WorkState         string                          `json:"work_state"`
	ToState           string                          `json:"to_state"`
	WorkOwner         string                          `json:"work_owner,omitempty"`
	GitDelivery       *envelope.GitDeliveryContract   `json:"git_delivery,omitempty"`
	PlanPath          string                          `json:"plan_path,omitempty"`
	AdvanceCommand    []string                        `json:"advance_command,omitempty"`
	Policy            AdvancePolicySummary            `json:"policy"`
	ActionIntent      envelope.ActionIntent           `json:"action_intent"`
	Decision          envelope.PolicyDecision         `json:"policy_decision"`
	ActionContext     *WorkActionContext              `json:"action_context,omitempty"`
	NoAction          string                          `json:"no_action,omitempty"`
	Warnings          []string                        `json:"warnings,omitempty"`
}

func (s *Store) AdvancePlan(workID string, now time.Time, opts AdvancePlanOptions) (AdvancePlan, error) {
	workID = strings.TrimSpace(workID)
	if workID == "" {
		return AdvancePlan{}, fmt.Errorf("advance-plan requires <work_id>")
	}
	to := strings.TrimSpace(opts.To)
	if to == "" {
		return AdvancePlan{}, fmt.Errorf("advance-plan requires --to <state>")
	}
	w, err := s.Get(workID)
	if err != nil {
		return AdvancePlan{}, err
	}
	if err := crossStateClaimedAdvanceError(w.State, to); err != nil {
		return AdvancePlan{}, err
	}
	actor := strings.TrimSpace(opts.Actor)
	if actor == "" {
		actor = "codex"
	}
	gitDelivery, err := s.preflightGitDeliveryBinding(w, actor, to, opts.GitDelivery)
	if err != nil {
		return AdvancePlan{}, err
	}
	if gitDelivery != nil {
		opts.GitDelivery = gitDelivery
		if opts.NonSourceDelivery != nil && opts.GitDelivery != nil {
			return AdvancePlan{}, fmt.Errorf("cannot combine Git and non-source authority binding")
		}
	}
	if opts.NonSourceDelivery != nil && (opts.Capability != "" || opts.Action != "" || opts.MutationClass != "" || opts.Reversibility != "" || opts.BlastRadius != "" || opts.TargetKind != "" || opts.TargetID != "" || opts.TargetPath != "") {
		return AdvancePlan{}, fmt.Errorf("non-source delivery binding does not accept action overrides")
	}
	deliveryBinding, err := s.preflightActionDeliveryBinding(w, actor, to, opts.NonSourceDelivery)
	if err != nil {
		return AdvancePlan{}, err
	}
	if terminalStates[to] {
		return AdvancePlan{}, terminalStateRequiresClose("advance-plan", to)
	}
	ai := s.advanceActionIntent(w, actor, to, now, opts)
	if deliveryBinding != nil {
		deliveryBinding.BindingActionIntentID = ai.ID
	}
	if opts.NonSourceDelivery != nil {
		ai.EvidenceRefs = mergeUnique(ai.EvidenceRefs, []string{opts.NonSourceDelivery.ActionIntentID, opts.NonSourceDelivery.EvidenceRef})
	}
	if err := ai.Validate(); err != nil {
		return AdvancePlan{}, err
	}
	candidate := w
	if gitDelivery != nil {
		candidate.GitDelivery = gitDelivery
		candidate.ExpectedBaseSHA = ""
	}
	if err := s.validateSourceActionAdmission(candidate, ai); err != nil {
		return AdvancePlan{}, err
	}
	cfg, source, err := policy.LoadConfig(s.home)
	if err != nil {
		return AdvancePlan{}, err
	}
	pd := policy.Evaluate(ai, cfg, now)
	if err := pd.Validate(); err != nil {
		return AdvancePlan{}, err
	}
	plan := AdvancePlan{
		NonSourceDelivery: deliveryBinding,
		SchemaVersion:     AdvancePlanSchemaVersion,
		ID:                idFor("wap_", workID, actor, to, ts(now)),
		GeneratedAt:       now.UTC().Format(time.RFC3339),
		Status:            "not_advanceable",
		Mode:              "proposal_only",
		Actor:             actor,
		WorkID:            w.ID,
		WorkState:         w.State,
		ToState:           to,
		WorkOwner:         w.Owner,
		GitDelivery:       gitDelivery,
		Policy:            summarizeAdvancePolicy(pd, source),
		ActionIntent:      ai,
		Decision:          pd,
		Warnings: []string{
			"does not advance work",
			"does not attach run, artifact, or verification ids",
			"does not enforce policy; it reports advance gate decision",
		},
	}
	heldReleaseErr := heldReleaseOperatorError(w.State, to, actor)
	directorReleaseErr := directorProposedReleaseOperatorError(w, to, actor)
	leaseStatus := s.leaseHolderStatusFor(w.ID, actor, opts.Steal, opts.StealReason, "advance")
	switch {
	case heldReleaseErr != nil:
		plan.NoAction = heldReleaseErr.Error()
	case directorReleaseErr != nil:
		plan.NoAction = directorReleaseErr.Error()
	case leaseStatus.Blocked:
		plan.NoAction = leaseStatus.Message
	case terminalStates[w.State]:
		plan.NoAction = fmt.Sprintf("work is terminal in state %q", w.State)
	case to == w.State || canTransition(w.State, to):
		plan.AdvanceCommand = []string{"compound", "work", "advance", w.ID, "--to", to, "--by", actor}
		if opts.NonSourceDelivery != nil {
			if len(w.ActionIntentIDs) == 0 || w.PolicyDecisionID == "" {
				return AdvancePlan{}, fmt.Errorf("non-source binding requires an existing action and policy generation")
			}
			plan.AdvanceCommand = append(plan.AdvanceCommand,
				"--delivery-action", opts.NonSourceDelivery.ActionIntentID,
				"--delivery-kind", opts.NonSourceDelivery.Kind,
				"--delivery-evidence", opts.NonSourceDelivery.EvidenceRef,
				"--expected-work-state", w.State,
				"--expected-policy-decision", w.PolicyDecisionID,
				"--expected-action-intent", w.ActionIntentIDs[len(w.ActionIntentIDs)-1],
			)
		}
		if gitDelivery != nil {
			plan.AdvanceCommand = append(plan.AdvanceCommand,
				"--target-worktree-root", gitDelivery.TargetWorktreeRoot,
				"--target-local-ref", gitDelivery.TargetLocalRef,
				"--expected-base-sha", gitDelivery.ExpectedBaseSHA,
			)
			if gitDelivery.PublicationMode == "local_only" {
				plan.AdvanceCommand = append(plan.AdvanceCommand, "--local-only")
			} else {
				plan.AdvanceCommand = append(plan.AdvanceCommand, "--publication-ref", gitDelivery.PublicationRef)
			}
		}
		if leaseStatus.Stolen {
			plan.AdvanceCommand = append(plan.AdvanceCommand, "--steal", "--reason", opts.StealReason)
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("lease held by %q; advance command carries --steal --reason", leaseStatus.Holder))
		}
		if policy.Allowed(pd) {
			plan.Status = "advance_policy_allowed"
		} else {
			plan.Status = "advance_policy_blocked"
			plan.NoAction = "advance lifecycle-ready but policy does not allow it"
		}
	default:
		plan.NoAction = fmt.Sprintf("illegal transition %s -> %s", w.State, to)
	}
	if actionContext, warnings := s.actionContextForWork(w); actionContext != nil || len(warnings) > 0 {
		plan.ActionContext = actionContext
		plan.Warnings = append(plan.Warnings, warnings...)
	}
	if opts.Record {
		path, err := s.RecordAdvancePlan(plan)
		if err != nil {
			return AdvancePlan{}, err
		}
		plan.PlanPath = path
	}
	return plan, nil
}

func (s *Store) preflightGitDeliveryBinding(w envelope.WorkEnvelope, actor, to string, requested *envelope.GitDeliveryContract) (*envelope.GitDeliveryContract, error) {
	if requested == nil {
		if gitDeliveryRequiresRepositoryIdentityMigration(w.GitDelivery) {
			return nil, repositoryIdentityMigrationError(w)
		}
		return nil, nil
	}
	if !policy.HasOperatorProvenance(actor) {
		return nil, fmt.Errorf("git_delivery binding requires explicit operator provenance; actor %q is an executor", actor)
	}
	if terminalStates[w.State] {
		return nil, fmt.Errorf("work is terminal in state %q; cannot bind git_delivery", w.State)
	}
	if to != w.State {
		return nil, fmt.Errorf("git_delivery binding requires a same-state advance, got %s -> %s", w.State, to)
	}
	backfillRepositoryIdentity := gitDeliveryRequiresRepositoryIdentityMigration(w.GitDelivery)
	if w.GitDelivery != nil && !backfillRepositoryIdentity {
		return nil, fmt.Errorf("work %s already has git_delivery authority; rebinding is not allowed", w.ID)
	}
	candidate := w
	contract := *requested
	if legacyBase := strings.TrimSpace(candidate.ExpectedBaseSHA); legacyBase != "" && legacyBase != strings.TrimSpace(contract.ExpectedBaseSHA) {
		return nil, fmt.Errorf("legacy expected_base_sha %s conflicts with git_delivery.expected_base_sha %s", legacyBase, strings.TrimSpace(contract.ExpectedBaseSHA))
	}
	candidate.ExpectedBaseSHA = ""
	candidate.GitDelivery = &contract
	if err := s.normalizeGitDeliveryForPlan(&candidate); err != nil {
		return nil, err
	}
	normalized := *candidate.GitDelivery
	if backfillRepositoryIdentity && !sameGitDeliveryAuthorityExceptFingerprint(*w.GitDelivery, normalized) {
		return nil, fmt.Errorf("work %s repository identity backfill must exactly match the existing git_delivery fields; rebinding is not allowed", w.ID)
	}
	return &normalized, nil
}

func (s *Store) RecordAdvancePlan(plan AdvancePlan) (string, error) {
	if strings.TrimSpace(plan.ID) == "" {
		return "", os.ErrInvalid
	}
	dir := filepath.Join(s.home, "advance-plans")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, plan.ID+".json")
	if err := s.writeJSON(path, &plan); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) advanceActionIntent(w envelope.WorkEnvelope, actor, to string, now time.Time, opts AdvancePlanOptions) envelope.ActionIntent {
	targetID := strings.TrimSpace(opts.TargetID)
	if targetID == "" {
		targetID = w.ID
	}
	targetPath := strings.TrimSpace(opts.TargetPath)
	if targetPath == "" {
		targetPath = s.workPath(w.ID)
	}
	capability := orDefault(opts.Capability, "work.advance")
	action := orDefault(opts.Action, "advance_work_to_"+to)
	targetKind := orDefault(opts.TargetKind, "work")
	mutationClass := orDefault(opts.MutationClass, "local_reversible")
	reversibility := orDefault(opts.Reversibility, "easy")
	backfillRepositoryIdentity := opts.GitDelivery != nil && gitDeliveryRequiresRepositoryIdentityMigration(w.GitDelivery)
	if opts.GitDelivery != nil {
		reversibility = "manual"
	}
	blastRadius := orDefault(opts.BlastRadius, "local")
	idParts := []string{capability, action, targetKind, targetID, targetPath, mutationClass, reversibility, blastRadius, actor, to}
	if opts.proofAttachmentRef != "" {
		idParts = append(idParts, opts.proofAttachmentRef, w.PolicyDecisionID, latestActionIntentID(w))
	}
	preview := fmt.Sprintf("advance work %s from %s to %s actor %s", w.ID, w.State, to, actor)
	rollbackPlan := fmt.Sprintf("advance work back to %s if no downstream action started", w.State)
	if opts.NonSourceDelivery != nil {
		binding := opts.NonSourceDelivery
		idParts = append(idParts, binding.ActionIntentID, binding.Kind, binding.EvidenceRef)
		preview = fmt.Sprintf("bind exact %s delivery of action %s to evidence %s on work %s at state %s", binding.Kind, binding.ActionIntentID, binding.EvidenceRef, w.ID, w.State)
		rollbackPlan = "preserve immutable binding history; refuse invalid evidence rather than rewriting prior authority"
	}
	if opts.GitDelivery != nil {
		contract := opts.GitDelivery
		idParts = append(idParts,
			contract.TargetWorktreeRoot,
			contract.RepositoryFingerprint,
			contract.TargetLocalRef,
			contract.ExpectedBaseSHA,
			contract.PublicationMode,
			contract.PublicationRef,
		)
		if backfillRepositoryIdentity {
			preview = fmt.Sprintf("backfill Git repository identity on work %s at state %s for existing target %s repository fingerprint %s ref %s expected base %s publication mode %s publication ref %s actor %s",
				w.ID, w.State, contract.TargetWorktreeRoot, contract.RepositoryFingerprint, contract.TargetLocalRef, contract.ExpectedBaseSHA, contract.PublicationMode, contract.PublicationRef, actor)
			rollbackPlan = "repository identity remains unbound unless this exact governed advance commits"
		} else {
			preview = fmt.Sprintf("bind Git delivery on work %s at state %s to target %s repository fingerprint %s ref %s expected base %s publication mode %s publication ref %s actor %s",
				w.ID, w.State, contract.TargetWorktreeRoot, contract.RepositoryFingerprint, contract.TargetLocalRef, contract.ExpectedBaseSHA, contract.PublicationMode, contract.PublicationRef, actor)
			rollbackPlan = "git_delivery authority is immutable after binding; correct a rejected plan before advancing"
		}
	}
	return envelope.ActionIntent{
		ArgumentsHash: proofAdvanceArgumentsHash(opts.proofAttachmentRef),
		Header: newHeader(
			envelope.SVActionIntent,
			idFor("act_", append(idParts, ts(now))...),
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
		RollbackPlan:  rollbackPlan,
	}
}

func summarizeAdvancePolicy(pd envelope.PolicyDecision, source string) AdvancePolicySummary {
	if strings.TrimSpace(source) == "" {
		source = "builtin:default"
	}
	return AdvancePolicySummary{
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

func proofAdvanceArgumentsHash(ref string) string {
	if ref == "" {
		return ""
	}
	return proofAttachmentArgumentsHash(ref)
}
