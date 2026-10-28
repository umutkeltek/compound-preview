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

const ClaimPlanSchemaVersion = "compound.work_claim_plan.v0.1"

type ClaimPlanOptions struct {
	Actor         string
	Capability    string
	Action        string
	MutationClass string
	Reversibility string
	BlastRadius   string
	TargetKind    string
	TargetID      string
	TargetPath    string
	Record        bool
}

type ClaimPolicySummary struct {
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

type ClaimPlan struct {
	SchemaVersion string                  `json:"schema_version"`
	ID            string                  `json:"id"`
	GeneratedAt   string                  `json:"generated_at"`
	Status        string                  `json:"status"`
	Mode          string                  `json:"mode"`
	Actor         string                  `json:"actor"`
	WorkID        string                  `json:"work_id"`
	WorkState     string                  `json:"work_state"`
	WorkOwner     string                  `json:"work_owner,omitempty"`
	PlanPath      string                  `json:"plan_path,omitempty"`
	ClaimCommand  []string                `json:"claim_command,omitempty"`
	Policy        ClaimPolicySummary      `json:"policy"`
	ActionIntent  envelope.ActionIntent   `json:"action_intent"`
	Decision      envelope.PolicyDecision `json:"policy_decision"`
	ActionContext *WorkActionContext      `json:"action_context,omitempty"`
	NoAction      string                  `json:"no_action,omitempty"`
	Warnings      []string                `json:"warnings,omitempty"`
}

func (s *Store) ClaimPlan(workID string, now time.Time, opts ClaimPlanOptions) (ClaimPlan, error) {
	workID = strings.TrimSpace(workID)
	if workID == "" {
		return ClaimPlan{}, fmt.Errorf("claim-plan requires <work_id>")
	}
	w, err := s.Get(workID)
	if err != nil {
		return ClaimPlan{}, err
	}
	if gitDeliveryRequiresRepositoryIdentityMigration(w.GitDelivery) {
		return ClaimPlan{}, repositoryIdentityMigrationError(w)
	}
	actor := strings.TrimSpace(opts.Actor)
	if actor == "" {
		actor = "codex"
	}
	ai := s.claimActionIntent(w, actor, now, opts)
	if err := ai.Validate(); err != nil {
		return ClaimPlan{}, err
	}
	cfg, source, err := policy.LoadConfig(s.home)
	if err != nil {
		return ClaimPlan{}, err
	}
	pd := policy.Evaluate(ai, cfg, now)
	if err := pd.Validate(); err != nil {
		return ClaimPlan{}, err
	}
	plan := ClaimPlan{
		SchemaVersion: ClaimPlanSchemaVersion,
		ID:            idFor("wcp_", workID, actor, ts(now)),
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Status:        "not_claimable",
		Mode:          "proposal_only",
		Actor:         actor,
		WorkID:        w.ID,
		WorkState:     w.State,
		WorkOwner:     w.Owner,
		Policy:        summarizeClaimPolicy(pd, source),
		ActionIntent:  ai,
		Decision:      pd,
		Warnings: []string{
			"does not claim work",
			"does not write a lease",
			"does not record the action intent",
			"does not enforce policy; it reports the claim gate decision",
		},
	}
	if canTransition(w.State, "claimed") || w.State == "claimed" {
		plan.ClaimCommand = []string{"compound", "work", "claim", w.ID, "--by", actor}
		if policy.Allowed(pd) {
			plan.Status = "claim_policy_allowed"
		} else {
			plan.Status = "claim_policy_blocked"
			plan.NoAction = "claim is lifecycle-ready but policy does not allow it"
		}
	} else {
		plan.NoAction = fmt.Sprintf("cannot claim from state %q", w.State)
	}
	if actionContext, warnings := s.actionContextForWork(w); actionContext != nil || len(warnings) > 0 {
		plan.ActionContext = actionContext
		plan.Warnings = append(plan.Warnings, warnings...)
	}
	if opts.Record {
		path, err := s.RecordClaimPlan(plan)
		if err != nil {
			return ClaimPlan{}, err
		}
		plan.PlanPath = path
	}
	return plan, nil
}

func (s *Store) RecordClaimPlan(plan ClaimPlan) (string, error) {
	if strings.TrimSpace(plan.ID) == "" {
		return "", os.ErrInvalid
	}
	dir := filepath.Join(s.home, "claim-plans")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, plan.ID+".json")
	if err := s.writeJSON(path, &plan); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) claimActionIntent(w envelope.WorkEnvelope, actor string, now time.Time, opts ClaimPlanOptions) envelope.ActionIntent {
	targetID := strings.TrimSpace(opts.TargetID)
	if targetID == "" {
		targetID = w.ID
	}
	targetPath := strings.TrimSpace(opts.TargetPath)
	if targetPath == "" {
		targetPath = s.workPath(w.ID)
	}
	capability := orDefault(opts.Capability, "work.claim")
	action := orDefault(opts.Action, "claim_work")
	targetKind := orDefault(opts.TargetKind, "work")
	mutationClass := orDefault(opts.MutationClass, "local_reversible")
	reversibility := orDefault(opts.Reversibility, "easy")
	blastRadius := orDefault(opts.BlastRadius, "local")
	return envelope.ActionIntent{
		Header: newHeader(
			envelope.SVActionIntent,
			idFor("act_", capability, action, targetKind, targetID, targetPath, mutationClass, reversibility, blastRadius, actor, ts(now)),
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
		Preview:       fmt.Sprintf("claim work %s for actor %s", w.ID, actor),
		RollbackPlan:  "release work lease and move claimed work back to ready if no action has started",
	}
}

func summarizeClaimPolicy(pd envelope.PolicyDecision, source string) ClaimPolicySummary {
	if strings.TrimSpace(source) == "" {
		source = "builtin:default"
	}
	return ClaimPolicySummary{
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
