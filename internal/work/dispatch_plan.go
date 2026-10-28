package work

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/laneselect"
)

// PreparationLaneSelector is the injected adapter that ranks preparation-class lanes from the
// Compound model-router registry + route-evidence throughput. cmd/compound wires it to
// routeevidence.PlanPreparationLane. It breaks the work<->routeevidence import cycle: work
// depends only on the pure laneselect types and calls the adapter through this seam. A nil
// selector (pure work-package tests that do not inject one) means preparation routing is
// unavailable and dispatch falls back to the codex default.
var PreparationLaneSelector func(homePath, class string, now time.Time) (*laneselect.PreparationLaneRanking, error)

// LaneCapacityGuard is the injected route-evidence adapter that evaluates the lane selected by
// dispatch. Keeping the seam in work avoids a work -> routeevidence import cycle; the
// routeevidence package installs the production adapter and tests may inject fixture paths.
// A nil guard preserves pure work-package behavior for harnesses that do not import routing.
var LaneCapacityGuard LaneCapacityGuardFunc

const DispatchPlanSchemaVersion = "compound.work_dispatch_plan.v0.1"
const DispatchClaimSchemaVersion = "compound.work_dispatch_claim.v0.1"
const DispatchAdvanceSchemaVersion = "compound.work_dispatch_advance.v0.1"
const DispatchClosePlanSchemaVersion = "compound.work_dispatch_close_plan.v0.1"
const DispatchCloseSchemaVersion = "compound.work_dispatch_close.v0.1"
const DispatchStepSchemaVersion = "compound.work_dispatch_step.v0.1"
const DispatchStepRunSchemaVersion = "compound.work_dispatch_step_run.v0.1"

const defaultDispatchStepRunMaxSteps = 10

type DispatchPlanOptions struct {
	ExpectedWorkState        string
	ExpectedPolicyDecisionID string
	ExpectedActionIntentID   string
	Actor                    string
	Record                   bool
	RecentLimit              int
	WorkID                   string
	SourcePacketPath         string
	SourcePacketState        string
	SourcePacketOwner        string
	SourcePacketRisk         string
	SourcePacketPrivacy      string
	SourcePacketContextID    string
	// CapabilityClass, when set to a preparation class (see routeevidence.IsPreparationClass)
	// and no explicit Actor is given, routes the dispatch to a lane chosen by the
	// preparation-lane selection instead of the "codex" default. Empty or non-preparation
	// classes keep today's behavior byte-identical.
	CapabilityClass string
	// ExpectedMinutes is the work window checked against a lane capacity reset. Zero uses 60.
	ExpectedMinutes int
	// Confirm explicitly admits an unknown or expiring lane. It never overrides a wall/death.
	Confirm bool
	// ReadOnly suppresses evidence writes by injected guards. Selection and all
	// warnings remain visible, but callers can promise a byte-identical preview.
	ReadOnly bool
}

type DispatchPlan struct {
	SchemaVersion string              `json:"schema_version"`
	ID            string              `json:"id"`
	GeneratedAt   string              `json:"generated_at"`
	Status        string              `json:"status"`
	Mode          string              `json:"mode"`
	Actor         string              `json:"actor"`
	PlanPath      string              `json:"plan_path,omitempty"`
	Source        *DispatchPlanSource `json:"source,omitempty"`
	Counts        WorkbenchCounts     `json:"counts"`
	Selected      *DispatchPlanItem   `json:"selected,omitempty"`
	Candidates    []DispatchPlanItem  `json:"candidates"`
	NoAction      string              `json:"no_action,omitempty"`
	Warnings      []string            `json:"warnings,omitempty"`
	Integrity     []string            `json:"integrity,omitempty"`
	// LaneSelection is the preparation-lane routing receipt (Law 9): the ranked candidates,
	// their fit/speed/cost inputs, and the winning lane whose id became the dispatch Actor.
	// Present only when a preparation-class dispatch ran the selection; omitted (and behavior
	// byte-identical) for every other dispatch.
	LaneSelection *laneselect.PreparationLaneRanking `json:"lane_selection,omitempty"`
	// LaneCapacity is the capacity annotation for the selected actor. It remains visible when the
	// actor is refused or held for confirmation; absence means no capacity adapter was installed.
	LaneCapacity *DispatchLaneCapacity `json:"lane_capacity,omitempty"`
}

// DispatchLaneCapacity is the work-package projection of routeevidence.LaneCapacity. Times are
// already normalized RFC3339 strings so this leaf seam stays independent of routeevidence types.
type DispatchLaneCapacity struct {
	Remaining  float64 `json:"remaining"`
	ResetsAt   string  `json:"resets_at,omitempty"`
	LastProven string  `json:"last_proven,omitempty"`
	State      string  `json:"state"`
	DataRef    string  `json:"data_ref,omitempty"`
}

type LaneCapacityGuardRequest struct {
	HomePath        string
	Lane            string
	CapabilityClass string
	WorkID          string
	ExpectedMinutes int
	Confirm         bool
	ReadOnly        bool
	Alternatives    []string
	Now             time.Time
	// TupleBacked distinguishes a resolved Compound LaneTuple from legacy
	// actor strings. Declared sources are read exactly; an absent declaration
	// leaves runtime quota enforcement with the execution gateway.
	TupleBacked    bool
	CapacitySource *LaneCapacitySource
}

type LaneCapacitySource struct {
	Kind             string
	FreshnessMinutes int
	Ref              string
}

type LaneCapacityGuardResult struct {
	Capacity             DispatchLaneCapacity
	Refused              bool
	ConfirmationRequired bool
	Message              string
}

type LaneCapacityGuardFunc func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error)

type DispatchPlanSource struct {
	Kind         string `json:"kind"`
	Path         string `json:"path"`
	Created      bool   `json:"created"`
	WorkID       string `json:"work_id"`
	State        string `json:"state"`
	PacketTitle  string `json:"packet_title,omitempty"`
	EvidenceRef  string `json:"evidence_ref,omitempty"`
	SourceDigest string `json:"source_digest,omitempty"`
}

type DispatchPlanItem struct {
	sourcePolicyDecisionID string
	sourceActionIntentID   string
	WorkID                 string `json:"work_id"`
	State                  string `json:"state"`
	Owner                  string `json:"owner,omitempty"`
	GitDeliveryBound       bool   `json:"git_delivery_bound,omitempty"`
	ExpectedBaseSHA        string `json:"expected_base_sha,omitempty"`
	RepositoryFingerprint  string `json:"repository_fingerprint,omitempty"`
	LandingStatus          string `json:"landing_status,omitempty"`
	SuccessPredicate       string `json:"success_predicate,omitempty"`
	// PredicateCommand is the success predicate parsed as a single runnable command
	// (CommandShapedPredicate, standing_proof.go) — the deterministic qualification signal
	// for an external re-execute-and-verify driver. Derived at plan render time, never
	// stored; absent for prose/multi-line predicates. Distinct from Command, which is the
	// next lifecycle action, and from the continue/dispatch-step lifecycle loops.
	PredicateCommand string                `json:"predicate_command,omitempty"`
	DesiredOutcome   string                `json:"desired_outcome,omitempty"`
	Action           string                `json:"action"`
	Command          []string              `json:"command,omitempty"`
	Reason           string                `json:"reason"`
	NextAction       string                `json:"next_action,omitempty"`
	EvidenceRefs     []string              `json:"evidence_refs,omitempty"`
	ActionContext    *WorkActionContext    `json:"action_context,omitempty"`
	ClaimPolicy      *ClaimPolicySummary   `json:"claim_policy,omitempty"`
	AdvancePolicy    *AdvancePolicySummary `json:"advance_policy,omitempty"`
	ClosePolicy      *ClosePolicySummary   `json:"close_policy,omitempty"`
	ClosePlanReady   bool                  `json:"close_plan_ready,omitempty"`
	// ClaimedBy is the live lease holder (WorkbenchItem.ClaimedBy, workbench.go:678-681),
	// carried through so a dispatch surface can skip an item held by a different actor
	// instead of silently offering it. Additive; empty when unclaimed.
	ClaimedBy string `json:"claimed_by,omitempty"`
}

type DispatchCapabilityOffer struct {
	Ref   string  `json:"ref"`
	Kind  string  `json:"kind"`
	Name  string  `json:"name"`
	Score float64 `json:"score"`
	Why   string  `json:"why"`
}

type DispatchClaimResult struct {
	SchemaVersion       string                    `json:"schema_version"`
	ID                  string                    `json:"id"`
	GeneratedAt         string                    `json:"generated_at"`
	Status              string                    `json:"status"`
	Actor               string                    `json:"actor"`
	ResultPath          string                    `json:"result_path,omitempty"`
	DispatchPlanID      string                    `json:"dispatch_plan_id"`
	DispatchPlanPath    string                    `json:"dispatch_plan_path,omitempty"`
	Selected            *DispatchPlanItem         `json:"selected,omitempty"`
	ClaimedWorkID       string                    `json:"claimed_work_id,omitempty"`
	ClaimedState        string                    `json:"claimed_state,omitempty"`
	ClaimedBy           string                    `json:"claimed_by,omitempty"`
	ActionIntentID      string                    `json:"action_intent_id,omitempty"`
	PolicyDecisionID    string                    `json:"policy_decision_id,omitempty"`
	ContextPackID       string                    `json:"context_pack_id,omitempty"`
	CapabilitiesOffered []DispatchCapabilityOffer `json:"capabilities_offered,omitempty"`
	WorkerPromptMD      string                    `json:"worker_prompt_md,omitempty"`
	NoAction            string                    `json:"no_action,omitempty"`
	Warnings            []string                  `json:"warnings,omitempty"`
	Plan                DispatchPlan              `json:"dispatch_plan"`
}

type DispatchAdvanceResult struct {
	SchemaVersion    string             `json:"schema_version"`
	ID               string             `json:"id"`
	GeneratedAt      string             `json:"generated_at"`
	Status           string             `json:"status"`
	Actor            string             `json:"actor"`
	ResultPath       string             `json:"result_path,omitempty"`
	Counts           WorkbenchCounts    `json:"counts"`
	Selected         *DispatchPlanItem  `json:"selected,omitempty"`
	Candidates       []DispatchPlanItem `json:"candidates"`
	AdvancedWorkID   string             `json:"advanced_work_id,omitempty"`
	FromState        string             `json:"from_state,omitempty"`
	AdvancedState    string             `json:"advanced_state,omitempty"`
	ActionIntentID   string             `json:"action_intent_id,omitempty"`
	PolicyDecisionID string             `json:"policy_decision_id,omitempty"`
	AdvancePlan      *AdvancePlan       `json:"advance_plan,omitempty"`
	NoAction         string             `json:"no_action,omitempty"`
	Warnings         []string           `json:"warnings,omitempty"`
	Integrity        []string           `json:"integrity,omitempty"`
}

type DispatchClosePlanOptions struct {
	Actor         string
	RecentLimit   int
	WorkID        string
	Outcome       string
	MoveType      string
	Capability    string
	Action        string
	MutationClass string
	Reversibility string
	BlastRadius   string
	TargetKind    string
	TargetID      string
	TargetPath    string
}

type DispatchClosePlanResult struct {
	SchemaVersion string             `json:"schema_version"`
	ID            string             `json:"id"`
	GeneratedAt   string             `json:"generated_at"`
	Status        string             `json:"status"`
	Mode          string             `json:"mode"`
	Actor         string             `json:"actor"`
	Outcome       string             `json:"outcome"`
	MoveType      string             `json:"move_type"`
	ResultPath    string             `json:"result_path,omitempty"`
	Counts        WorkbenchCounts    `json:"counts"`
	Selected      *DispatchPlanItem  `json:"selected,omitempty"`
	Candidates    []DispatchPlanItem `json:"candidates"`
	ClosePlanID   string             `json:"close_plan_id,omitempty"`
	ClosePlanPath string             `json:"close_plan_path,omitempty"`
	ClosePlan     *ClosePlan         `json:"close_plan,omitempty"`
	NoAction      string             `json:"no_action,omitempty"`
	Warnings      []string           `json:"warnings,omitempty"`
	Integrity     []string           `json:"integrity,omitempty"`
}

type DispatchCloseOptions struct {
	// Optional exact source reservation, checked by Close under its admission lock.
	ExpectedWorkState        string
	ExpectedPolicyDecisionID string
	ExpectedActionIntentID   string
	Actor                    string
	RecentLimit              int
	WorkID                   string
	Outcome                  string
	MoveType                 string
	Summary                  string
}

type DispatchCloseResult struct {
	SchemaVersion         string                   `json:"schema_version"`
	ID                    string                   `json:"id"`
	GeneratedAt           string                   `json:"generated_at"`
	Status                string                   `json:"status"`
	Actor                 string                   `json:"actor"`
	Outcome               string                   `json:"outcome"`
	MoveType              string                   `json:"move_type"`
	ResultPath            string                   `json:"result_path,omitempty"`
	DispatchClosePlanID   string                   `json:"dispatch_close_plan_id,omitempty"`
	DispatchClosePlanPath string                   `json:"dispatch_close_plan_path,omitempty"`
	DispatchClosePlan     *DispatchClosePlanResult `json:"dispatch_close_plan,omitempty"`
	Selected              *DispatchPlanItem        `json:"selected,omitempty"`
	ClosePlanID           string                   `json:"close_plan_id,omitempty"`
	ClosePlanPath         string                   `json:"close_plan_path,omitempty"`
	ClosedWorkID          string                   `json:"closed_work_id,omitempty"`
	FinalState            string                   `json:"final_state,omitempty"`
	ReceiptID             string                   `json:"receipt_id,omitempty"`
	TransitionID          string                   `json:"transition_id,omitempty"`
	CloseResult           *CloseResult             `json:"close_result,omitempty"`
	NoAction              string                   `json:"no_action,omitempty"`
	Warnings              []string                 `json:"warnings,omitempty"`
	Integrity             []string                 `json:"integrity,omitempty"`
}

type DispatchStepOptions struct {
	ExpectWorkID             string
	ExpectedWorkState        string
	ExpectedPolicyDecisionID string
	ExpectedActionIntentID   string
	Actor                    string
	RecentLimit              int
	Outcome                  string
	MoveType                 string
	Summary                  string
	SourcePacketPath         string
	SourcePacketState        string
	SourcePacketOwner        string
	SourcePacketRisk         string
	SourcePacketPrivacy      string
	SourcePacketContextID    string
}

type DispatchStepResult struct {
	SchemaVersion       string                    `json:"schema_version"`
	ID                  string                    `json:"id"`
	GeneratedAt         string                    `json:"generated_at"`
	Status              string                    `json:"status"`
	Actor               string                    `json:"actor"`
	Action              string                    `json:"action"`
	WorkID              string                    `json:"work_id,omitempty"`
	ResultPath          string                    `json:"result_path,omitempty"`
	Source              *DispatchPlanSource       `json:"source,omitempty"`
	Counts              WorkbenchCounts           `json:"counts"`
	PostCounts          *WorkbenchCounts          `json:"post_counts,omitempty"`
	DispatchClose       *DispatchCloseResult      `json:"dispatch_close,omitempty"`
	DispatchAdvance     *DispatchAdvanceResult    `json:"dispatch_advance,omitempty"`
	DispatchClaim       *DispatchClaimResult      `json:"dispatch_claim,omitempty"`
	ContextPackID       string                    `json:"context_pack_id,omitempty"`
	CapabilitiesOffered []DispatchCapabilityOffer `json:"capabilities_offered,omitempty"`
	WorkerPromptMD      string                    `json:"worker_prompt_md,omitempty"`
	NextAction          string                    `json:"next_action,omitempty"`
	CanContinue         bool                      `json:"can_continue"`
	ContinueCommand     []string                  `json:"continue_command,omitempty"`
	WaitReason          string                    `json:"wait_reason,omitempty"`
	BlockedOn           []string                  `json:"blocked_on,omitempty"`
	NoAction            string                    `json:"no_action,omitempty"`
	Warnings            []string                  `json:"warnings,omitempty"`
	Integrity           []string                  `json:"integrity,omitempty"`
}

type DispatchStepRunOptions struct {
	DispatchStepOptions
	MaxSteps int
}

type DispatchStepRunResult struct {
	SchemaVersion   string               `json:"schema_version"`
	ID              string               `json:"id"`
	GeneratedAt     string               `json:"generated_at"`
	Status          string               `json:"status"`
	Actor           string               `json:"actor"`
	ResultPath      string               `json:"result_path,omitempty"`
	MaxSteps        int                  `json:"max_steps"`
	StepCount       int                  `json:"step_count"`
	Steps           []DispatchStepResult `json:"steps"`
	CanContinue     bool                 `json:"can_continue"`
	ContinueCommand []string             `json:"continue_command,omitempty"`
	WaitReason      string               `json:"wait_reason,omitempty"`
	BlockedOn       []string             `json:"blocked_on,omitempty"`
	NextAction      string               `json:"next_action,omitempty"`
	Warnings        []string             `json:"warnings,omitempty"`
	Integrity       []string             `json:"integrity,omitempty"`
}

// resolveDispatchActor decides which lane (actor) a dispatch runs as. An explicit Actor is
// always honored. When no Actor is given and the work is a preparation class, the
// preparation-lane selection picks the lane by capability-class fit, token-generation speed
// above the floor, then cost — replacing the historical hardcoded "codex" default with a
// choice made from Compound-owned evidence (model-router registry + route-evidence
// throughput). The selection ranking is returned so it lands in the DispatchPlan receipt.
// Any other case (empty class, non-preparation class, no eligible lane, missing registry)
// falls back to "codex" byte-identically, surfacing a warning when a selection was attempted.
func (s *Store) resolveDispatchActor(opts DispatchPlanOptions, now time.Time) (actor string, selection *laneselect.PreparationLaneRanking, warnings []string) {
	actor = strings.TrimSpace(opts.Actor)
	if actor != "" {
		return actor, nil, nil
	}
	class := strings.TrimSpace(opts.CapabilityClass)
	if !laneselect.IsPreparationClass(class) {
		return "codex", nil, nil
	}
	if PreparationLaneSelector == nil {
		return "codex", nil, []string{"preparation lane selection unavailable: no selector wired"}
	}
	ranking, err := PreparationLaneSelector(s.home, class, now)
	if err != nil {
		return "codex", nil, []string{"preparation lane selection unavailable: " + err.Error()}
	}
	if ranking == nil || ranking.Winner == nil {
		status := "no ranking"
		if ranking != nil {
			status = ranking.Status
		}
		return "codex", ranking, []string{fmt.Sprintf("preparation lane selection found no eligible lane (%s); using default actor", status)}
	}
	return ranking.Winner.Lane, ranking, nil
}

func (s *Store) DispatchPlan(now time.Time, opts DispatchPlanOptions) (DispatchPlan, error) {
	actor, laneSelection, laneWarnings := s.resolveDispatchActor(opts, now)
	var source *DispatchPlanSource
	if strings.TrimSpace(opts.SourcePacketPath) != "" {
		sourcePreview, err := dispatchPlanSourcePreview(opts.SourcePacketPath, dispatchSourceState(opts))
		if err != nil {
			return DispatchPlan{}, err
		}
		source = sourcePreview
	}
	report, err := s.Workbench(now, WorkbenchOptions{RecentLimit: opts.RecentLimit})
	if err != nil {
		return DispatchPlan{}, err
	}
	targetWorkID := strings.TrimSpace(opts.WorkID)
	plan := DispatchPlan{
		SchemaVersion: DispatchPlanSchemaVersion,
		ID:            idFor("wdp_", actor, ts(now)),
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Status:        "no_open_work",
		Mode:          "proposal_only",
		Actor:         actor,
		Source:        source,
		Counts:        report.Counts,
		Candidates:    []DispatchPlanItem{},
		Warnings: []string{
			"does not claim work",
			"does not execute work",
			"does not close work",
			"does not import source packets",
		},
		Integrity: append([]string{}, report.Integrity...),
	}
	plan.LaneSelection = laneSelection
	if len(laneWarnings) > 0 {
		plan.Warnings = append(plan.Warnings, laneWarnings...)
	}
	if source != nil {
		plan.Warnings = append(plan.Warnings, "source packet is preview-only; use work import-packet or mutating dispatch command to import")
	}
	policyBlocked := false
	leaseSkipped := 0
	for _, item := range report.Open {
		if targetWorkID != "" && item.WorkID != targetWorkID {
			continue
		}
		candidate := dispatchCandidate(item, actor)
		if candidate.Action == "held_by_other" {
			leaseSkipped++
		}
		if candidate.Action == "claim" {
			claimPlan, err := s.ClaimPlan(item.WorkID, now, ClaimPlanOptions{Actor: actor})
			if err != nil {
				candidate.Reason = strings.TrimSpace(candidate.Reason + "; claim policy plan failed: " + err.Error())
				policyBlocked = true
			} else {
				policySummary := claimPlan.Policy
				candidate.ClaimPolicy = &policySummary
				if !policySummary.Allowed {
					candidate.Reason = strings.TrimSpace(candidate.Reason + "; claim policy " + policySummary.Outcome)
					policyBlocked = true
				}
			}
		}
		plan.Candidates = append(plan.Candidates, candidate)
		if plan.Selected == nil && candidate.Action == "claim" && candidate.ClaimPolicy != nil && candidate.ClaimPolicy.Allowed {
			selected := candidate
			plan.Selected = &selected
			plan.Status = "ready_to_claim"
		}
	}
	// Skip, never silently offer: items held by a different actor stay visible in
	// Candidates (marked held_by_other, no silent shrinkage) but are excluded from claim
	// eligibility above; the skip count is always reported, never dropped quietly.
	if leaseSkipped > 0 {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("skipped %d held by other actors", leaseSkipped))
	}
	if plan.Selected == nil {
		switch {
		case targetWorkID != "" && policyBlocked:
			plan.Status = "claim_policy_blocked"
			plan.NoAction = "target work exists but claim policy does not allow dispatch"
		case targetWorkID != "" && leaseSkipped > 0:
			plan.Status = "held_by_other_actor"
			plan.NoAction = fmt.Sprintf("target work %s is held by another actor", targetWorkID)
		case targetWorkID != "":
			plan.Status = "target_not_claimable"
			plan.NoAction = fmt.Sprintf("target work %s is not ready to claim", targetWorkID)
		case len(plan.Candidates) == 0:
			plan.NoAction = "workbench has no open work"
		case policyBlocked:
			plan.Status = "claim_policy_blocked"
			plan.NoAction = "ready work exists but claim policy does not allow dispatch"
		case report.Counts.NeedsReview > 0:
			plan.Status = "needs_review"
			plan.NoAction = "open work exists but requires review before dispatch"
		case report.Counts.InProgress > 0:
			plan.Status = "in_progress_only"
			plan.NoAction = "open work is already claimed or running"
		case leaseSkipped > 0:
			plan.Status = "held_by_other_actor"
			plan.NoAction = fmt.Sprintf("%d ready item(s) held by other actors; nothing else claimable", leaseSkipped)
		default:
			plan.Status = "no_claimable_work"
			plan.NoAction = "open work is not ready to claim"
		}
	}
	if plan.Selected != nil && LaneCapacityGuard != nil {
		guarded, err := LaneCapacityGuard(LaneCapacityGuardRequest{
			HomePath:        s.home,
			Lane:            actor,
			CapabilityClass: strings.TrimSpace(opts.CapabilityClass),
			WorkID:          plan.Selected.WorkID,
			ExpectedMinutes: dispatchExpectedMinutes(opts.ExpectedMinutes),
			Confirm:         opts.Confirm,
			ReadOnly:        opts.ReadOnly,
			Alternatives:    dispatchAlternativeLadder(laneSelection),
			Now:             now,
		})
		if err != nil {
			return DispatchPlan{}, fmt.Errorf("guard lane capacity: %w", err)
		}
		capacity := guarded.Capacity
		plan.LaneCapacity = &capacity
		switch {
		case guarded.Refused:
			plan.Selected = nil
			plan.Status = "capacity_refused"
			plan.NoAction = guarded.Message
		case guarded.ConfirmationRequired:
			plan.Selected = nil
			plan.Status = "capacity_confirmation_required"
			plan.NoAction = guarded.Message
		case strings.TrimSpace(guarded.Message) != "":
			plan.Warnings = append(plan.Warnings, guarded.Message)
		}
	}
	if opts.Record {
		path, err := s.RecordDispatchPlan(plan)
		if err != nil {
			return DispatchPlan{}, err
		}
		plan.PlanPath = path
	}
	return plan, nil
}

func dispatchExpectedMinutes(value int) int {
	if value <= 0 {
		return 60
	}
	return value
}

func dispatchAlternativeLadder(selection *laneselect.PreparationLaneRanking) []string {
	if selection == nil || len(selection.Candidates) == 0 {
		return nil
	}
	candidates := append([]laneselect.PreparationLaneScore(nil), selection.Candidates...)
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].RoutingPosition < candidates[j].RoutingPosition
	})
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		lane := strings.TrimSpace(candidate.Lane)
		if lane != "" {
			out = append(out, lane)
		}
	}
	return out
}

func (s *Store) RecordDispatchPlan(plan DispatchPlan) (string, error) {
	if strings.TrimSpace(plan.ID) == "" {
		return "", os.ErrInvalid
	}
	dir := filepath.Join(s.home, "dispatch-plans")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, plan.ID+".json")
	if err := s.writeJSON(path, &plan); err != nil {
		return "", err
	}
	return path, nil
}

func dispatchPlanSourcePreview(packetPath, state string) (*DispatchPlanSource, error) {
	proof, err := PacketSourceProofForPath(packetPath)
	if err != nil {
		return nil, err
	}
	return &DispatchPlanSource{
		Kind:         "packet",
		Path:         packetPath,
		Created:      false,
		State:        state,
		PacketTitle:  proof.PacketTitle,
		EvidenceRef:  proof.EvidenceRef,
		SourceDigest: proof.SourceDigest,
	}, nil
}

func dispatchSourceState(opts DispatchPlanOptions) string {
	state := strings.TrimSpace(opts.SourcePacketState)
	if state == "" {
		return "ready"
	}
	return state
}

func (s *Store) importDispatchSourcePacket(now time.Time, actor string, opts DispatchPlanOptions) (*DispatchPlanSource, error) {
	imported, err := s.ImportPacket(opts.SourcePacketPath, PacketImportOptions{
		Owner:         opts.SourcePacketOwner,
		Actor:         actor,
		State:         dispatchSourceState(opts),
		Risk:          opts.SourcePacketRisk,
		Privacy:       opts.SourcePacketPrivacy,
		ContextPackID: opts.SourcePacketContextID,
	}, now)
	if err != nil {
		return nil, err
	}
	return &DispatchPlanSource{
		Kind:         "packet",
		Path:         opts.SourcePacketPath,
		Created:      imported.Created,
		WorkID:       imported.Work.ID,
		State:        imported.Work.State,
		PacketTitle:  imported.PacketTitle,
		EvidenceRef:  imported.EvidenceRef,
		SourceDigest: imported.SourceDigest,
	}, nil
}

func (s *Store) DispatchClaim(now time.Time, opts DispatchPlanOptions) (DispatchClaimResult, error) {
	actor := strings.TrimSpace(opts.Actor)
	if actor == "" {
		actor = "codex"
		opts.Actor = actor
	}
	var source *DispatchPlanSource
	if strings.TrimSpace(opts.SourcePacketPath) != "" {
		imported, err := s.importDispatchSourcePacket(now, actor, opts)
		if err != nil {
			return DispatchClaimResult{}, err
		}
		source = imported
		opts.SourcePacketPath = ""
	}
	opts.Record = false
	plan, err := s.DispatchPlan(now, opts)
	if err != nil {
		return DispatchClaimResult{}, err
	}
	if source != nil {
		plan.Source = source
	}
	path, err := s.RecordDispatchPlan(plan)
	if err != nil {
		return DispatchClaimResult{}, err
	}
	plan.PlanPath = path
	result := DispatchClaimResult{
		SchemaVersion:    DispatchClaimSchemaVersion,
		ID:               idFor("wdc_", plan.ID, ts(now)),
		GeneratedAt:      now.UTC().Format(time.RFC3339),
		Status:           plan.Status,
		Actor:            plan.Actor,
		DispatchPlanID:   plan.ID,
		DispatchPlanPath: plan.PlanPath,
		Selected:         plan.Selected,
		NoAction:         plan.NoAction,
		Warnings: []string{
			"claims at most one selected ready work item",
			"does not execute work",
			"does not close work",
		},
		Plan: plan,
	}
	if plan.Selected == nil {
		path, err := s.RecordDispatchClaim(result)
		if err != nil {
			return DispatchClaimResult{}, err
		}
		result.ResultPath = path
		return result, nil
	}
	if plan.Selected.Action != "claim" {
		result.NoAction = "selected work is not claimable"
		path, err := s.RecordDispatchClaim(result)
		if err != nil {
			return DispatchClaimResult{}, err
		}
		result.ResultPath = path
		return result, nil
	}
	if plan.Selected.ClaimPolicy == nil || !plan.Selected.ClaimPolicy.Allowed {
		result.Status = "claim_policy_blocked"
		result.NoAction = "selected work claim policy does not allow dispatch"
		path, err := s.RecordDispatchClaim(result)
		if err != nil {
			return DispatchClaimResult{}, err
		}
		result.ResultPath = path
		return result, nil
	}
	opts.ExpectedWorkState, opts.ExpectedPolicyDecisionID, opts.ExpectedActionIntentID = plan.Selected.expectedReservation(
		opts.ExpectedWorkState, opts.ExpectedPolicyDecisionID, opts.ExpectedActionIntentID)
	claimed, err := s.claimExpected(plan.Selected.WorkID, plan.Actor,
		opts.ExpectedWorkState, opts.ExpectedPolicyDecisionID, opts.ExpectedActionIntentID, now)
	if err != nil {
		return DispatchClaimResult{}, err
	}
	result.Status = "claimed"
	result.NoAction = ""
	result.ClaimedWorkID = claimed.ID
	result.ClaimedState = claimed.State
	result.ClaimedBy = claimed.Authority.Owner
	result.PolicyDecisionID = claimed.PolicyDecisionID
	if len(claimed.ActionIntentIDs) > 0 {
		result.ActionIntentID = claimed.ActionIntentIDs[len(claimed.ActionIntentIDs)-1]
	}
	prompt, err := s.attachDispatchPrompt(claimed.ID, plan.Actor, now)
	if err != nil {
		return DispatchClaimResult{}, err
	}
	result.ContextPackID = prompt.ContextPackID
	result.CapabilitiesOffered = prompt.CapabilitiesOffered
	result.WorkerPromptMD = prompt.Markdown
	path, err = s.RecordDispatchClaim(result)
	if err != nil {
		return DispatchClaimResult{}, err
	}
	result.ResultPath = path
	return result, nil
}

func (s *Store) RecordDispatchClaim(result DispatchClaimResult) (string, error) {
	if strings.TrimSpace(result.ID) == "" {
		return "", os.ErrInvalid
	}
	dir := filepath.Join(s.home, "dispatch-claims")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, result.ID+".json")
	if err := s.writeJSON(path, &result); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) DispatchAdvance(now time.Time, opts DispatchPlanOptions) (DispatchAdvanceResult, error) {
	actor := strings.TrimSpace(opts.Actor)
	if actor == "" {
		actor = "codex"
	}
	report, err := s.Workbench(now, WorkbenchOptions{RecentLimit: opts.RecentLimit})
	if err != nil {
		return DispatchAdvanceResult{}, err
	}
	targetWorkID := strings.TrimSpace(opts.WorkID)
	result := DispatchAdvanceResult{
		SchemaVersion: DispatchAdvanceSchemaVersion,
		ID:            idFor("wda_", actor, ts(now)),
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Status:        "no_open_work",
		Actor:         actor,
		Counts:        report.Counts,
		Candidates:    []DispatchPlanItem{},
		Warnings: []string{
			"advances at most one actor-claimed work item to running",
			"does not execute task commands",
			"does not close work",
		},
		Integrity: append([]string{}, report.Integrity...),
	}
	policyBlocked := false
	claimedByOther := false
	for _, item := range report.Open {
		if targetWorkID != "" && item.WorkID != targetWorkID {
			continue
		}
		candidate := dispatchAdvanceCandidate(item, actor)
		if candidate.Action == "advance_running" {
			advancePlan, err := s.AdvancePlan(item.WorkID, now, AdvancePlanOptions{Actor: actor, To: "running"})
			if err != nil {
				candidate.Reason = strings.TrimSpace(candidate.Reason + "; advance policy plan failed: " + err.Error())
				policyBlocked = true
			} else {
				policySummary := advancePlan.Policy
				candidate.AdvancePolicy = &policySummary
				if !policySummary.Allowed {
					candidate.Reason = strings.TrimSpace(candidate.Reason + "; advance policy " + policySummary.Outcome)
					policyBlocked = true
				}
			}
		}
		if item.State == "claimed" && item.ClaimedBy != "" && item.ClaimedBy != actor {
			claimedByOther = true
		}
		result.Candidates = append(result.Candidates, candidate)
		if result.Selected == nil && candidate.Action == "advance_running" && candidate.AdvancePolicy != nil && candidate.AdvancePolicy.Allowed {
			selected := candidate
			result.Selected = &selected
			result.Status = "ready_to_advance"
		}
	}
	if result.Selected == nil {
		switch {
		case targetWorkID != "" && policyBlocked:
			result.Status = "advance_policy_blocked"
			result.NoAction = "target work exists but advance policy does not allow dispatch"
		case targetWorkID != "" && claimedByOther:
			result.Status = "claimed_by_other"
			result.NoAction = fmt.Sprintf("target work %s is claimed by another actor", targetWorkID)
		case targetWorkID != "":
			result.Status = "target_not_advanceable"
			result.NoAction = fmt.Sprintf("target work %s is not claimed by actor", targetWorkID)
		case len(result.Candidates) == 0:
			result.NoAction = "workbench has no open work"
		case policyBlocked:
			result.Status = "advance_policy_blocked"
			result.NoAction = "claimed work exists but advance policy does not allow dispatch"
		case claimedByOther:
			result.Status = "claimed_by_other"
			result.NoAction = "claimed work is owned by another actor"
		case report.Counts.InProgress > 0:
			result.Status = "in_progress_only"
			result.NoAction = "open work is running or verifying, not claimed-ready"
		default:
			result.Status = "no_advanceable_work"
			result.NoAction = "open work is not claimed by actor"
		}
		path, err := s.RecordDispatchAdvance(result)
		if err != nil {
			return DispatchAdvanceResult{}, err
		}
		result.ResultPath = path
		return result, nil
	}
	advancePlan, err := s.AdvancePlan(result.Selected.WorkID, now, AdvancePlanOptions{Actor: actor, To: "running"})
	if err != nil {
		return DispatchAdvanceResult{}, err
	}
	result.AdvancePlan = &advancePlan
	if !advancePlan.Policy.Allowed {
		result.Status = "advance_policy_blocked"
		result.NoAction = "selected work advance policy does not allow dispatch"
		path, err := s.RecordDispatchAdvance(result)
		if err != nil {
			return DispatchAdvanceResult{}, err
		}
		result.ResultPath = path
		return result, nil
	}
	opts.ExpectedWorkState, opts.ExpectedPolicyDecisionID, opts.ExpectedActionIntentID = result.Selected.expectedReservation(
		opts.ExpectedWorkState, opts.ExpectedPolicyDecisionID, opts.ExpectedActionIntentID)
	advanced, err := s.Advance(result.Selected.WorkID, AdvanceInput{
		To: "running", Actor: actor, Note: "dispatch advance",
		ExpectedWorkState: opts.ExpectedWorkState, ExpectedPolicyDecisionID: opts.ExpectedPolicyDecisionID,
		ExpectedActionIntentID: opts.ExpectedActionIntentID,
	}, now)
	if err != nil {
		return DispatchAdvanceResult{}, err
	}
	result.Status = "running"
	result.NoAction = ""
	result.AdvancedWorkID = advanced.ID
	result.FromState = "claimed"
	result.AdvancedState = advanced.State
	result.PolicyDecisionID = advanced.PolicyDecisionID
	if len(advanced.ActionIntentIDs) > 0 {
		result.ActionIntentID = advanced.ActionIntentIDs[len(advanced.ActionIntentIDs)-1]
	}
	path, err := s.RecordDispatchAdvance(result)
	if err != nil {
		return DispatchAdvanceResult{}, err
	}
	result.ResultPath = path
	return result, nil
}

func (s *Store) RecordDispatchAdvance(result DispatchAdvanceResult) (string, error) {
	if strings.TrimSpace(result.ID) == "" {
		return "", os.ErrInvalid
	}
	dir := filepath.Join(s.home, "dispatch-advances")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, result.ID+".json")
	if err := s.writeJSON(path, &result); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) DispatchClosePlan(now time.Time, opts DispatchClosePlanOptions) (DispatchClosePlanResult, error) {
	actor := strings.TrimSpace(opts.Actor)
	if actor == "" {
		actor = "codex"
	}
	outcome := strings.TrimSpace(opts.Outcome)
	if outcome == "" {
		outcome = "completed"
	}
	moveType := strings.TrimSpace(opts.MoveType)
	if moveType == "" {
		moveType = "control_plane_repair"
	}
	report, err := s.Workbench(now, WorkbenchOptions{RecentLimit: opts.RecentLimit})
	if err != nil {
		return DispatchClosePlanResult{}, err
	}
	targetWorkID := strings.TrimSpace(opts.WorkID)
	result := DispatchClosePlanResult{
		SchemaVersion: DispatchClosePlanSchemaVersion,
		ID:            idFor("wdcp_", actor, outcome, moveType, ts(now)),
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Status:        "no_open_work",
		Mode:          "proposal_only",
		Actor:         actor,
		Outcome:       outcome,
		MoveType:      moveType,
		Counts:        report.Counts,
		Candidates:    []DispatchPlanItem{},
		Warnings: []string{
			"records close-plan readiness only",
			"does not close work",
			"does not emit receipt or transition",
			"does not promote memory",
		},
		Integrity: append([]string{}, report.Integrity...),
	}
	policyBlocked := false
	awaitingProof := false
	leaseSkipped := 0
	var blocked *DispatchPlanItem
	for _, item := range report.Open {
		if targetWorkID != "" && item.WorkID != targetWorkID {
			continue
		}
		candidate := dispatchClosePlanCandidate(item, actor, outcome, moveType)
		if candidate.Action == "held_by_other" {
			leaseSkipped++
		}
		if candidate.Action == "plan_close" {
			closePlan, err := s.dispatchClosePlan(item.WorkID, now, opts, actor, outcome, moveType, false)
			if err != nil {
				candidate.Reason = strings.TrimSpace(candidate.Reason + "; close policy plan failed: " + err.Error())
				policyBlocked = true
			} else {
				policySummary := closePlan.Policy
				candidate.ClosePolicy = &policySummary
				candidate.Command = append([]string{}, closePlan.CloseCommand...)
				candidate.EvidenceRefs = mergeUnique(candidate.EvidenceRefs, closePlan.ProofRefs)
				if !policySummary.Allowed || closePlan.NoAction != "" {
					policyBlocked = true
					candidate.Reason = strings.TrimSpace(candidate.Reason + "; close policy " + policySummary.Outcome)
					if blocked == nil {
						copyCandidate := candidate
						blocked = &copyCandidate
					}
				} else {
					candidate.ClosePlanReady = true
				}
			}
		}
		if candidate.Action == "await_proof" {
			awaitingProof = true
		}
		result.Candidates = append(result.Candidates, candidate)
		if result.Selected == nil && candidate.Action == "plan_close" && candidate.ClosePlanReady {
			selected := candidate
			result.Selected = &selected
			result.Status = "close_policy_allowed"
		}
	}
	// Skip, never silently offer: a verifying item held by a different actor stays visible
	// in Candidates (marked held_by_other) but is excluded from close-plan selection above,
	// and dispatch-close (which selects through this same plan) can never auto-close it.
	if leaseSkipped > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf("skipped %d held by other actors", leaseSkipped))
	}
	if result.Selected == nil && blocked != nil {
		result.Selected = blocked
	}
	if result.Selected != nil {
		closePlan, err := s.dispatchClosePlan(result.Selected.WorkID, now, opts, actor, outcome, moveType, true)
		if err != nil {
			return DispatchClosePlanResult{}, err
		}
		result.ClosePlan = &closePlan
		result.ClosePlanID = closePlan.ID
		result.ClosePlanPath = closePlan.PlanPath
		result.Status = closePlan.Status
		if !closePlan.Policy.Allowed || closePlan.NoAction != "" {
			result.NoAction = "selected work close policy does not allow dispatch"
		}
		path, err := s.RecordDispatchClosePlan(result)
		if err != nil {
			return DispatchClosePlanResult{}, err
		}
		result.ResultPath = path
		return result, nil
	}
	switch {
	case targetWorkID != "" && policyBlocked:
		result.Status = "close_policy_blocked"
		result.NoAction = "target work exists but close policy does not allow dispatch"
	case targetWorkID != "" && awaitingProof:
		result.Status = "awaiting_proof"
		result.NoAction = fmt.Sprintf("target work %s is verifying without proof evidence", targetWorkID)
	case targetWorkID != "" && leaseSkipped > 0:
		result.Status = "held_by_other_actor"
		result.NoAction = fmt.Sprintf("target work %s is held by another actor", targetWorkID)
	case targetWorkID != "":
		result.Status = "target_not_closeable"
		result.NoAction = fmt.Sprintf("target work %s is not ready for close planning", targetWorkID)
	case len(result.Candidates) == 0:
		result.NoAction = "workbench has no open work"
	case policyBlocked:
		result.Status = "close_policy_blocked"
		result.NoAction = "verifying work exists but close policy does not allow dispatch"
	case awaitingProof:
		result.Status = "awaiting_proof"
		result.NoAction = "verifying work exists without artifact, verification, or observed-run evidence"
	case report.Counts.InProgress > 0:
		result.Status = "in_progress_only"
		result.NoAction = "open work is not verifying with proof evidence"
	case leaseSkipped > 0:
		result.Status = "held_by_other_actor"
		result.NoAction = fmt.Sprintf("%d verifying item(s) held by other actors; nothing else close-planable", leaseSkipped)
	default:
		result.Status = "no_closeable_work"
		result.NoAction = "open work is not ready for close planning"
	}
	path, err := s.RecordDispatchClosePlan(result)
	if err != nil {
		return DispatchClosePlanResult{}, err
	}
	result.ResultPath = path
	return result, nil
}

func (s *Store) dispatchClosePlan(workID string, now time.Time, opts DispatchClosePlanOptions, actor, outcome, moveType string, record bool) (ClosePlan, error) {
	return s.ClosePlan(workID, now, ClosePlanOptions{
		Actor:         actor,
		Outcome:       outcome,
		MoveType:      moveType,
		Capability:    opts.Capability,
		Action:        opts.Action,
		MutationClass: opts.MutationClass,
		Reversibility: opts.Reversibility,
		BlastRadius:   opts.BlastRadius,
		TargetKind:    opts.TargetKind,
		TargetID:      opts.TargetID,
		TargetPath:    opts.TargetPath,
		Record:        record,
	})
}

func (s *Store) RecordDispatchClosePlan(result DispatchClosePlanResult) (string, error) {
	if strings.TrimSpace(result.ID) == "" {
		return "", os.ErrInvalid
	}
	dir := filepath.Join(s.home, "dispatch-close-plans")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, result.ID+".json")
	if err := s.writeJSON(path, &result); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) DispatchClose(now time.Time, opts DispatchCloseOptions) (DispatchCloseResult, error) {
	actor := strings.TrimSpace(opts.Actor)
	if actor == "" {
		actor = "codex"
	}
	outcome := strings.TrimSpace(opts.Outcome)
	if outcome == "" {
		outcome = "completed"
	}
	moveType := strings.TrimSpace(opts.MoveType)
	if moveType == "" {
		moveType = "control_plane_repair"
	}
	plan, err := s.DispatchClosePlan(now, DispatchClosePlanOptions{
		Actor:       actor,
		RecentLimit: opts.RecentLimit,
		WorkID:      opts.WorkID,
		Outcome:     outcome,
		MoveType:    moveType,
	})
	if err != nil {
		return DispatchCloseResult{}, err
	}
	result := DispatchCloseResult{
		SchemaVersion:         DispatchCloseSchemaVersion,
		ID:                    idFor("wdcl_", actor, outcome, moveType, ts(now)),
		GeneratedAt:           now.UTC().Format(time.RFC3339),
		Status:                plan.Status,
		Actor:                 actor,
		Outcome:               outcome,
		MoveType:              moveType,
		DispatchClosePlanID:   plan.ID,
		DispatchClosePlanPath: plan.ResultPath,
		DispatchClosePlan:     &plan,
		Selected:              plan.Selected,
		ClosePlanID:           plan.ClosePlanID,
		ClosePlanPath:         plan.ClosePlanPath,
		NoAction:              plan.NoAction,
		Warnings: []string{
			"closes at most one proof-ready verifying work item",
			"does not execute task commands",
			"does not promote memory",
		},
		Integrity: append([]string{}, plan.Integrity...),
	}
	if plan.Selected == nil || plan.ClosePlan == nil || plan.Status != "close_policy_allowed" || plan.ClosePlan.NoAction != "" || !plan.ClosePlan.Policy.Allowed {
		if result.NoAction == "" {
			result.NoAction = "no proof-ready close plan selected"
		}
		path, err := s.RecordDispatchClose(result)
		if err != nil {
			return DispatchCloseResult{}, err
		}
		result.ResultPath = path
		return result, nil
	}
	summary := strings.TrimSpace(opts.Summary)
	if summary == "" {
		summary = "dispatch close"
	}
	opts.ExpectedWorkState, opts.ExpectedPolicyDecisionID, opts.ExpectedActionIntentID = plan.Selected.expectedReservation(
		opts.ExpectedWorkState, opts.ExpectedPolicyDecisionID, opts.ExpectedActionIntentID)
	closed, err := s.Close(plan.Selected.WorkID, CloseInput{
		ExpectedWorkState:        opts.ExpectedWorkState,
		ExpectedPolicyDecisionID: opts.ExpectedPolicyDecisionID,
		ExpectedActionIntentID:   opts.ExpectedActionIntentID,
		Outcome:                  outcome,
		MoveType:                 moveType,
		Actor:                    actor,
		Summary:                  summary,
		Changed:                  true,
		ProofRefs:                append([]string{}, plan.ClosePlan.ProofRefs...),
	}, now)
	if err != nil {
		return DispatchCloseResult{}, err
	}
	result.Status = "closed"
	result.NoAction = ""
	result.ClosedWorkID = closed.Work.ID
	result.FinalState = closed.Work.State
	result.ReceiptID = closed.Receipt.ID
	result.TransitionID = closed.Transition.ID
	result.CloseResult = &closed
	path, err := s.RecordDispatchClose(result)
	if err != nil {
		return DispatchCloseResult{}, err
	}
	result.ResultPath = path
	return result, nil
}

func (s *Store) RecordDispatchClose(result DispatchCloseResult) (string, error) {
	if strings.TrimSpace(result.ID) == "" {
		return "", os.ErrInvalid
	}
	dir := filepath.Join(s.home, "dispatch-closes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, result.ID+".json")
	if err := s.writeJSON(path, &result); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) DispatchStep(now time.Time, opts DispatchStepOptions) (DispatchStepResult, error) {
	actor := strings.TrimSpace(opts.Actor)
	if actor == "" {
		actor = "codex"
	}
	report, err := s.Workbench(now, WorkbenchOptions{RecentLimit: opts.RecentLimit, Actor: actor})
	if err != nil {
		return DispatchStepResult{}, err
	}
	if opts.ExpectWorkID != "" {
		if !report.DispatchNext.CanRun || report.DispatchNext.WorkID != opts.ExpectWorkID {
			return DispatchStepResult{}, fmt.Errorf("%w: dispatch target changed from %s to %s", ErrExecutionReservationChanged, opts.ExpectWorkID, report.DispatchNext.WorkID)
		}
		w, err := s.Get(opts.ExpectWorkID)
		if err != nil {
			return DispatchStepResult{}, err
		}
		if err := validateExpectedExecutionReservation(w, opts.ExpectedWorkState, opts.ExpectedPolicyDecisionID, opts.ExpectedActionIntentID); err != nil {
			return DispatchStepResult{}, err
		}
	}
	result := DispatchStepResult{
		SchemaVersion: DispatchStepSchemaVersion,
		ID:            idFor("wds_", actor, ts(now)),
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Status:        "no_open_work",
		Actor:         actor,
		Action:        "none",
		Counts:        report.Counts,
		Warnings: []string{
			"dispatches at most one lifecycle step",
			"does not execute task commands",
			"does not observe or synthesize traces",
			"does not promote memory",
		},
		Integrity: append([]string{}, report.Integrity...),
	}
	for _, item := range report.Open {
		if item.State == "verifying" && hasCloseProofEvidence(item) && !dispatchLeaseHeldByOther(item, actor) && continueMatchesExpected(opts.ExpectWorkID, item.WorkID) {
			closed, err := s.DispatchClose(now, DispatchCloseOptions{
				WorkID:                   opts.ExpectWorkID,
				ExpectedWorkState:        opts.ExpectedWorkState,
				ExpectedPolicyDecisionID: opts.ExpectedPolicyDecisionID,
				ExpectedActionIntentID:   opts.ExpectedActionIntentID,
				Actor:                    actor,
				RecentLimit:              opts.RecentLimit,
				Outcome:                  opts.Outcome,
				MoveType:                 opts.MoveType,
				Summary:                  opts.Summary,
			})
			if err != nil {
				return DispatchStepResult{}, err
			}
			result.Action = "close"
			result.Status = closed.Status
			result.WorkID = closed.ClosedWorkID
			if result.WorkID == "" && closed.Selected != nil {
				result.WorkID = closed.Selected.WorkID
			}
			result.DispatchClose = &closed
			result.NoAction = closed.NoAction
			return s.recordDispatchStep(now, result)
		}
	}
	for _, item := range report.Open {
		if item.State == "claimed" && item.ClaimedBy == actor && continueMatchesExpected(opts.ExpectWorkID, item.WorkID) {
			advanced, err := s.DispatchAdvance(now, DispatchPlanOptions{
				Actor: actor, RecentLimit: opts.RecentLimit, WorkID: opts.ExpectWorkID,
				ExpectedWorkState:        opts.ExpectedWorkState,
				ExpectedPolicyDecisionID: opts.ExpectedPolicyDecisionID,
				ExpectedActionIntentID:   opts.ExpectedActionIntentID,
			})
			if err != nil {
				return DispatchStepResult{}, err
			}
			result.Action = "advance"
			result.Status = advanced.Status
			result.WorkID = advanced.AdvancedWorkID
			if result.WorkID == "" && advanced.Selected != nil {
				result.WorkID = advanced.Selected.WorkID
			}
			result.DispatchAdvance = &advanced
			result.NoAction = advanced.NoAction
			return s.recordDispatchStep(now, result)
		}
	}
	readyToClaim := false
	for _, item := range report.Open {
		if item.State == "ready" {
			readyToClaim = true
			break
		}
	}
	if readyToClaim || strings.TrimSpace(opts.SourcePacketPath) != "" {
		claimed, err := s.DispatchClaim(now, DispatchPlanOptions{
			WorkID:                   opts.ExpectWorkID,
			ExpectedWorkState:        opts.ExpectedWorkState,
			ExpectedPolicyDecisionID: opts.ExpectedPolicyDecisionID,
			ExpectedActionIntentID:   opts.ExpectedActionIntentID,
			Actor:                    actor,
			RecentLimit:              opts.RecentLimit,
			SourcePacketPath:         opts.SourcePacketPath,
			SourcePacketState:        opts.SourcePacketState,
			SourcePacketOwner:        opts.SourcePacketOwner,
			SourcePacketRisk:         opts.SourcePacketRisk,
			SourcePacketPrivacy:      opts.SourcePacketPrivacy,
			SourcePacketContextID:    opts.SourcePacketContextID,
		})
		if err != nil {
			return DispatchStepResult{}, err
		}
		result.Action = "claim"
		result.Status = claimed.Status
		result.Counts = claimed.Plan.Counts
		result.Integrity = append([]string{}, claimed.Plan.Integrity...)
		result.Source = claimed.Plan.Source
		result.WorkID = claimed.ClaimedWorkID
		result.ContextPackID = claimed.ContextPackID
		result.CapabilitiesOffered = claimed.CapabilitiesOffered
		result.WorkerPromptMD = claimed.WorkerPromptMD
		if result.WorkID == "" && claimed.Selected != nil {
			result.WorkID = claimed.Selected.WorkID
		}
		result.DispatchClaim = &claimed
		result.NoAction = claimed.NoAction
		if sourcePacketAlreadyTerminal(claimed) {
			result.Action = "none"
			result.Status = "source_packet_terminal"
			result.WorkID = claimed.Plan.Source.WorkID
			result.NoAction = "source packet already has terminal Compound work proof; no dispatch step needed"
		}
		return s.recordDispatchStep(now, result)
	}
	switch {
	case report.Counts.Open == 0:
		result.NoAction = "workbench has no open work"
	case report.Counts.NeedsReview > 0:
		result.Status = "needs_review"
		result.NoAction = "open work requires review before dispatch"
	case report.Counts.InProgress > 0:
		result.Status = "in_progress_only"
		result.NoAction = "open work is not ready for a safe dispatch step"
	default:
		result.Status = "no_dispatchable_work"
		result.NoAction = "open work has no dispatchable lifecycle step"
	}
	return s.recordDispatchStep(now, result)
}

func (s *Store) DispatchStepRun(now time.Time, opts DispatchStepRunOptions) (DispatchStepRunResult, error) {
	stepOpts := opts.DispatchStepOptions
	actor := strings.TrimSpace(stepOpts.Actor)
	if actor == "" {
		actor = "codex"
		stepOpts.Actor = actor
	}
	maxSteps := opts.MaxSteps
	if maxSteps <= 0 {
		maxSteps = defaultDispatchStepRunMaxSteps
	}
	result := DispatchStepRunResult{
		SchemaVersion: DispatchStepRunSchemaVersion,
		ID:            idFor("wdsr_", actor, ts(now)),
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Status:        "waiting",
		Actor:         actor,
		MaxSteps:      maxSteps,
		Warnings: []string{
			"dispatches repeated safe lifecycle steps only while dispatch-step can continue",
			"does not execute task commands",
			"does not observe or synthesize traces",
			"does not promote memory",
		},
	}
	for i := 0; i < maxSteps; i++ {
		step, err := s.DispatchStep(now.Add(time.Duration(i)*time.Nanosecond), stepOpts)
		if err != nil {
			return DispatchStepRunResult{}, err
		}
		result.Steps = append(result.Steps, step)
		result.StepCount = len(result.Steps)
		result.NextAction = step.NextAction
		result.WaitReason = step.WaitReason
		result.BlockedOn = append([]string{}, step.BlockedOn...)
		result.Integrity = mergeUnique(result.Integrity, step.Integrity)
		stepOpts.SourcePacketPath = ""
		stepOpts.SourcePacketState = ""
		stepOpts.SourcePacketOwner = ""
		stepOpts.SourcePacketRisk = ""
		stepOpts.SourcePacketPrivacy = ""
		stepOpts.SourcePacketContextID = ""
		if !step.CanContinue {
			result.Status = "waiting"
			result.CanContinue = false
			result.ContinueCommand = nil
			return s.recordDispatchStepRun(result)
		}
	}
	result.Status = "max_steps_reached"
	result.CanContinue = true
	result.WaitReason = ""
	result.BlockedOn = nil
	result.ContinueCommand = dispatchStepRunContinuation(actor, maxSteps)
	return s.recordDispatchStepRun(result)
}

func (s *Store) recordDispatchStep(now time.Time, result DispatchStepResult) (DispatchStepResult, error) {
	post, err := s.Workbench(now, WorkbenchOptions{RecentLimit: 0})
	if err != nil {
		return DispatchStepResult{}, err
	}
	result.PostCounts = &post.Counts
	result.NextAction = dispatchStepNextAction(result, post)
	if dispatchStepMadeProgress(result) {
		result.CanContinue, result.ContinueCommand = dispatchStepContinuation(result, post)
	}
	if !result.CanContinue {
		result.WaitReason, result.BlockedOn = dispatchStepWait(result, post)
	}
	result.Integrity = mergeUnique(result.Integrity, post.Integrity)
	path, err := s.RecordDispatchStep(result)
	if err != nil {
		return DispatchStepResult{}, err
	}
	result.ResultPath = path
	return result, nil
}

func (s *Store) recordDispatchStepRun(result DispatchStepRunResult) (DispatchStepRunResult, error) {
	path, err := s.RecordDispatchStepRun(result)
	if err != nil {
		return DispatchStepRunResult{}, err
	}
	result.ResultPath = path
	return result, nil
}

func dispatchStepMadeProgress(result DispatchStepResult) bool {
	switch result.Status {
	case "claimed", "running", "closed":
		return true
	default:
		return false
	}
}

func dispatchStepNextAction(result DispatchStepResult, post WorkbenchReport) string {
	if result.Status == "source_packet_terminal" && strings.TrimSpace(result.NoAction) != "" {
		return result.NoAction
	}
	if strings.TrimSpace(result.WorkID) != "" {
		for _, item := range post.Open {
			if item.WorkID == result.WorkID {
				return item.NextAction
			}
		}
		for _, item := range post.Recent {
			if item.WorkID == result.WorkID {
				return item.NextAction
			}
		}
	}
	if strings.TrimSpace(result.NoAction) != "" {
		return result.NoAction
	}
	if post.Counts.Open == 0 {
		return "workbench has no open work"
	}
	return "run dispatch-step again for next open work"
}

func dispatchStepContinuation(result DispatchStepResult, post WorkbenchReport) (bool, []string) {
	actor := strings.TrimSpace(result.Actor)
	if actor == "" {
		actor = "codex"
	}
	for _, item := range post.Open {
		switch {
		case item.State == "verifying" && hasCloseProofEvidence(item):
			return true, []string{"compound", "work", "dispatch-step", "--actor", actor}
		case item.State == "claimed" && item.ClaimedBy == actor:
			return true, []string{"compound", "work", "dispatch-step", "--actor", actor}
		case item.State == "ready":
			return true, []string{"compound", "work", "dispatch-step", "--actor", actor}
		}
	}
	return false, nil
}

func dispatchStepWait(result DispatchStepResult, post WorkbenchReport) (string, []string) {
	if result.Status == "source_packet_terminal" {
		blockedOn := []string{"work.terminal"}
		if strings.TrimSpace(result.WorkID) != "" {
			blockedOn = append(blockedOn, "work.terminal:"+result.WorkID)
		}
		return "source_packet_terminal", blockedOn
	}
	if post.Counts.Open == 0 {
		return "no_open_work", []string{"work.ready_or_source_packet"}
	}
	actor := strings.TrimSpace(result.Actor)
	if actor == "" {
		actor = "codex"
	}
	switch result.Status {
	case "capacity_confirmation_required":
		return "capacity_confirmation_required", []string{"lane.capacity_confirmation:" + actor}
	case "capacity_refused":
		return "capacity_refused", []string{"lane.capacity_refused:" + actor}
	}
	var review []string
	var runEvidence []string
	var closeProof []string
	var claimedOther []string
	var readyState []string
	var unknown []string
	for _, item := range post.Open {
		switch item.State {
		case "held", "blocked":
			review = append(review, "work.review:"+item.WorkID)
		case "running":
			runEvidence = append(runEvidence, "work.run_evidence:"+item.WorkID)
		case "verifying":
			if !hasCloseProofEvidence(item) {
				closeProof = append(closeProof, "work.close_proof:"+item.WorkID)
			}
		case "claimed":
			if item.ClaimedBy != actor {
				claimedOther = append(claimedOther, "work.claimed_by_other_actor:"+item.WorkID)
			}
		case "proposed", "admitted":
			readyState = append(readyState, "work.ready_state:"+item.WorkID)
		case "ready":
			// dispatchStepContinuation would have returned true for ready work.
		default:
			unknown = append(unknown, "work.state:"+item.WorkID)
		}
	}
	switch {
	case len(review) > 0 || result.Status == "needs_review":
		return "needs_review", mergeUnique(review, runEvidence, closeProof, claimedOther, readyState, unknown)
	case len(runEvidence) > 0:
		return "run_evidence", mergeUnique(runEvidence, closeProof, claimedOther, readyState, unknown)
	case len(closeProof) > 0:
		return "close_proof", mergeUnique(closeProof, claimedOther, readyState, unknown)
	case len(claimedOther) > 0:
		return "claimed_by_other_actor", mergeUnique(claimedOther, readyState, unknown)
	case len(readyState) > 0:
		return "not_ready", mergeUnique(readyState, unknown)
	default:
		return "no_dispatchable_work", mergeUnique(unknown)
	}
}

func sourcePacketAlreadyTerminal(result DispatchClaimResult) bool {
	source := result.Plan.Source
	if source == nil || source.Created || strings.TrimSpace(source.WorkID) == "" {
		return false
	}
	if strings.TrimSpace(result.ClaimedWorkID) != "" || result.Selected != nil {
		return false
	}
	return terminalStates[source.State]
}

func dispatchStepRunContinuation(actor string, maxSteps int) []string {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		actor = "codex"
	}
	if maxSteps <= 0 {
		maxSteps = defaultDispatchStepRunMaxSteps
	}
	return []string{"compound", "work", "dispatch-step", "--actor", actor, "--until-wait", "--max-steps", strconv.Itoa(maxSteps)}
}

func (s *Store) RecordDispatchStep(result DispatchStepResult) (string, error) {
	if strings.TrimSpace(result.ID) == "" {
		return "", os.ErrInvalid
	}
	dir := filepath.Join(s.home, "dispatch-steps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, result.ID+".json")
	if err := s.writeJSON(path, &result); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) RecordDispatchStepRun(result DispatchStepRunResult) (string, error) {
	if strings.TrimSpace(result.ID) == "" {
		return "", os.ErrInvalid
	}
	dir := filepath.Join(s.home, "dispatch-step-runs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, result.ID+".json")
	if err := s.writeJSON(path, &result); err != nil {
		return "", err
	}
	return path, nil
}

// dispatchLeaseHeldByOther reports whether item's live lease (ClaimedBy, populated for
// every state — see workbench.go:678-681, leases are not deleted on release) is held by
// an actor other than the dispatching actor. A "ready" item can carry lease residue if it
// cycled claimed -> ready without releasing the lease; a "verifying" item can carry it from
// its original claim. Empty ClaimedBy or ClaimedBy == actor means no conflict.
func dispatchLeaseHeldByOther(item WorkbenchItem, actor string) bool {
	return item.ClaimedBy != "" && item.ClaimedBy != actor
}

func dispatchCandidate(item WorkbenchItem, actor string) DispatchPlanItem {
	out := DispatchPlanItem{
		sourcePolicyDecisionID: item.PolicyDecisionID,
		sourceActionIntentID:   item.sourceActionIntentID,
		WorkID:                 item.WorkID,
		State:                  item.State,
		Owner:                  item.Owner,
		GitDeliveryBound:       item.GitDeliveryBound,
		ExpectedBaseSHA:        item.ExpectedBaseSHA,
		RepositoryFingerprint:  item.RepositoryFingerprint,
		LandingStatus:          item.LandingStatus,
		SuccessPredicate:       item.SuccessPredicate,
		DesiredOutcome:         item.DesiredOutcome,
		Reason:                 item.OpenReason,
		NextAction:             item.NextAction,
		EvidenceRefs:           append([]string{}, item.EvidenceRefs...),
		ActionContext:          cloneWorkActionContext(item.ActionContext),
		ClaimedBy:              item.ClaimedBy,
	}
	if cmd, ok := CommandShapedPredicate(out.SuccessPredicate); ok {
		out.PredicateCommand = cmd
	}
	if item.LandingStatus == "repository_identity_migration_required" {
		out.Action = "review"
		out.Command = nil
		out.Reason = "repository fingerprint migration requires exact operator review"
		return out
	}
	switch item.State {
	case "ready":
		if dispatchLeaseHeldByOther(item, actor) {
			out.Action = "held_by_other"
			out.Reason = "lease held by " + item.ClaimedBy
		} else {
			out.Action = "claim"
			out.Command = []string{"compound", "work", "claim", item.WorkID, "--by", actor}
		}
	case "proposed", "admitted":
		out.Action = "prepare"
		if item.ActionContext != nil {
			out.Command = []string{"compound", "work", "advance-plan", item.WorkID, "--to", "ready", "--actor", actor}
			out.Reason = "reviewed action context available; prepare ready transition without executing action"
		}
	case "held", "blocked":
		out.Action = "review"
	case "claimed", "running", "verifying":
		out.Action = "observe"
	default:
		out.Action = "inspect"
	}
	return out
}

func (item DispatchPlanItem) expectedReservation(state, policyID, actionID string) (string, string, string) {
	if state != "" || policyID != "" || actionID != "" {
		return state, policyID, actionID
	}
	return item.State, item.sourcePolicyDecisionID, item.sourceActionIntentID
}

func dispatchClosePlanCandidate(item WorkbenchItem, actor, outcome, moveType string) DispatchPlanItem {
	out := dispatchCandidate(item, actor)
	out.Command = nil
	switch item.State {
	case "verifying":
		if dispatchLeaseHeldByOther(item, actor) {
			out.Action = "held_by_other"
			out.Reason = "lease held by " + item.ClaimedBy
		} else if workbenchItemNeedsGitDeliveryProof(item, moveType) && item.LandingStatus != "ready_to_close" {
			out.Action = "await_proof"
			out.Reason = "source-controlled work is not ready to close: " + firstNonEmpty(item.LandingStatus, "missing Git delivery proof")
			out.NextAction = item.NextAction
		} else if hasCloseProofEvidence(item) {
			out.Action = "plan_close"
			out.Command = []string{"compound", "work", "close-plan", item.WorkID, "--outcome", outcome, "--move-type", moveType, "--actor", actor}
			out.Reason = "verifying with proof evidence"
			out.EvidenceRefs = mergeUnique(out.EvidenceRefs, item.ArtifactIDs, item.VerificationIDs)
		} else {
			out.Action = "await_proof"
			out.Reason = "verifying without artifact, verification, or observed-run evidence"
		}
	case "running":
		out.Action = "observe"
		out.Reason = "running work needs observed-run proof before close planning"
	case "claimed":
		out.Action = "advance_running"
	case "ready":
		out.Action = "claim_first"
	case "proposed", "admitted":
		out.Action = "prepare"
	case "held", "blocked":
		out.Action = "review"
	}
	return out
}

// workbenchItemNeedsGitDeliveryProof mirrors gitDeliveryProofForClose's
// decision for the read model, and must mirror its ORDER too: a recorded commit
// artifact demands proof first, and only then does an audit_only move waive the
// binding. Without moveType this predicate answered a different question than
// the gate it advertises, so a bound audit envelope was reported "await_proof"
// forever while the close itself would have succeeded -- the dispatch loop
// stalled on a verdict the enforcer did not share.
func workbenchItemNeedsGitDeliveryProof(item WorkbenchItem, moveType string) bool {
	if _, _, found, _ := latestCommitArtifact(item.ArtifactIDs); found {
		return true
	}
	if moveType == "audit_only" {
		return false
	}
	return item.GitDeliveryBound
}

func hasCloseProofEvidence(item WorkbenchItem) bool {
	return len(item.ArtifactIDs) > 0 || len(item.VerificationIDs) > 0 || len(item.RunObservations) > 0
}

func dispatchAdvanceCandidate(item WorkbenchItem, actor string) DispatchPlanItem {
	out := dispatchCandidate(item, actor)
	switch item.State {
	case "claimed":
		if item.ClaimedBy == actor {
			out.Action = "advance_running"
			out.Command = []string{"compound", "work", "advance", item.WorkID, "--to", "running", "--by", actor}
			out.Reason = "claimed by actor"
		} else if item.ClaimedBy != "" {
			out.Action = "observe"
			out.Reason = "claimed by " + item.ClaimedBy
		}
	case "ready":
		out.Action = "claim_first"
		out.Command = nil
	case "running", "verifying":
		out.Action = "observe"
	}
	return out
}
