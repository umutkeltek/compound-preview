package work

import (
	"fmt"
	"strings"
	"time"
)

const ReviewCardWorkPlanReportSchemaVersion = "compound.work_review_card_plan_report.v0.1"

type ReviewCardWorkPlanOptions struct {
	Actor string
}

type ReviewCardWorkDraft struct {
	SchemaVersion         string            `json:"schema_version"`
	ID                    string            `json:"id"`
	Status                string            `json:"status"`
	CardID                string            `json:"card_id"`
	CardKind              string            `json:"card_kind"`
	CardTitle             string            `json:"card_title"`
	NextAction            string            `json:"next_action,omitempty"`
	ConditionStatus       string            `json:"condition_status,omitempty"`
	TriggeringEvidenceRef string            `json:"triggering_evidence_ref,omitempty"`
	MatchedConditionRefs  []string          `json:"matched_condition_refs,omitempty"`
	SourceWorkID          string            `json:"source_work_id"`
	GateID                string            `json:"gate_id,omitempty"`
	SourceKind            string            `json:"source_kind,omitempty"`
	CandidateID           string            `json:"candidate_id,omitempty"`
	PanelRunID            string            `json:"panel_run_id,omitempty"`
	PromotionSummaryRef   string            `json:"promotion_summary_ref,omitempty"`
	ActionCommand         []string          `json:"action_command,omitempty"`
	RequiredInputs        []string          `json:"required_inputs,omitempty"`
	ExpectedValidation    []string          `json:"expected_validation,omitempty"`
	StopConditions        []string          `json:"stop_conditions,omitempty"`
	Boundary              string            `json:"boundary,omitempty"`
	Proof                 string            `json:"proof,omitempty"`
	WorkID                string            `json:"work_id"`
	WorkOwner             string            `json:"work_owner"`
	SuccessPredicate      string            `json:"success_predicate"`
	DesiredOutcome        string            `json:"desired_outcome"`
	NonGoals              []string          `json:"non_goals,omitempty"`
	EvidenceRefs          []string          `json:"evidence_refs,omitempty"`
	ActionContext         WorkActionContext `json:"action_context"`
}

type ReviewCardWorkPlanReport struct {
	SchemaVersion string              `json:"schema_version"`
	Status        string              `json:"status"`
	ReviewCard    ReviewCardWorkDraft `json:"review_card"`
	WorkPlan      CreatePlan          `json:"work_plan"`
}

func (s *Store) BuildReviewCardWorkPlan(cardID string, now time.Time, opts ReviewCardWorkPlanOptions) (ReviewCardWorkPlanReport, error) {
	if s == nil {
		return ReviewCardWorkPlanReport{}, fmt.Errorf("work store required")
	}
	draft, err := s.BuildReviewCardWorkDraft(cardID, now)
	if err != nil {
		return ReviewCardWorkPlanReport{}, err
	}
	actor := strings.TrimSpace(opts.Actor)
	if actor == "" {
		actor = "codex"
	}
	plan, err := s.CreatePlan(CreateInput{
		WorkID:           draft.WorkID,
		Owner:            draft.WorkOwner,
		Actor:            actor,
		State:            "proposed",
		SuccessPredicate: draft.SuccessPredicate,
		DesiredOutcome:   draft.DesiredOutcome,
		NonGoals:         draft.NonGoals,
		EvidenceRefs:     draft.EvidenceRefs,
		Risk:             "tier2",
		Privacy:          "private",
	}, now, CreatePlanOptions{
		Actor:         actor,
		Capability:    "work.review_card",
		Action:        "plan_work_from_workbench_review_card",
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		TargetKind:    "workbench_review_card",
		TargetID:      draft.CardID,
		TargetPath:    draft.ActionContext.SourceRef,
		ActionContext: &draft.ActionContext,
		Record:        true,
	})
	if err != nil {
		return ReviewCardWorkPlanReport{}, err
	}
	return ReviewCardWorkPlanReport{
		SchemaVersion: ReviewCardWorkPlanReportSchemaVersion,
		Status:        "recorded",
		ReviewCard:    draft,
		WorkPlan:      plan,
	}, nil
}

func (s *Store) BuildReviewCardWorkDraft(cardID string, now time.Time) (ReviewCardWorkDraft, error) {
	cardID = strings.TrimSpace(cardID)
	if cardID == "" {
		return ReviewCardWorkDraft{}, fmt.Errorf("workbench review card id required")
	}
	report, err := s.Workbench(now, WorkbenchOptions{RecentLimit: 100})
	if err != nil {
		return ReviewCardWorkDraft{}, err
	}
	item, card, ok := findWorkbenchReviewCard(report.Open, cardID)
	if !ok {
		return ReviewCardWorkDraft{}, fmt.Errorf("workbench review card %q not found", cardID)
	}
	sourceRef := "compound://workbench/review-cards/" + card.ID
	evidenceRefs := compactStrings(
		"workbench_review_card:"+card.ID,
		"work:"+item.WorkID,
		"work_state:"+item.State,
		"workbench_projection:compound.workbench",
	)
	if card.CandidateID != "" {
		evidenceRefs = append(evidenceRefs, "memory_candidate:"+card.CandidateID)
	}
	if card.PanelRunID != "" {
		evidenceRefs = append(evidenceRefs, "panel_run:"+card.PanelRunID)
	}
	if card.PanelRunRef != "" {
		evidenceRefs = append(evidenceRefs, "artifact:"+card.PanelRunRef)
	}
	if card.PromotionSummaryRef != "" {
		evidenceRefs = append(evidenceRefs, "artifact:"+card.PromotionSummaryRef)
	}
	if card.GateID != "" {
		evidenceRefs = append(evidenceRefs, "operator_gate:"+card.GateID)
	}
	if card.SourceKind != "" {
		evidenceRefs = append(evidenceRefs, "source_kind:"+card.SourceKind)
	}
	if card.TriggeringEvidenceRef != "" {
		evidenceRefs = append(evidenceRefs, "triggering_evidence:"+card.TriggeringEvidenceRef, card.TriggeringEvidenceRef)
	}
	for _, ref := range card.MatchedConditionRefs {
		evidenceRefs = append(evidenceRefs, "condition_ref:"+ref)
	}
	workOwner := strings.TrimSpace(item.Owner)
	if workOwner == "" {
		workOwner = "compound-core"
	}
	commandText := strings.Join(card.Command, " ")
	if commandText == "" {
		commandText = "operator review"
	}
	successPredicate := fmt.Sprintf("Resolve Workbench review card %s (%s) with explicit operator action `%s`; proof: %s", card.ID, card.Kind, commandText, card.Proof)
	desiredOutcome := fmt.Sprintf("Workbench review card %s is handled through a WorkEnvelope proposal while source work %s remains held until an explicit operator decision", card.ID, item.WorkID)
	nonGoals := []string{
		"do not execute review-card command while creating or admitting work-plan",
		"do not clear the source held WorkEnvelope from this proposal",
		"do not fetch live ASC data, apply ProductSignal, load scheduler, promote memory, or run memory adjudication from this proposal",
		"do not create a second review queue or bypass Workbench review-card authority",
	}
	ctx := WorkActionContext{
		Kind:               "workbench_review_card",
		SourceRef:          sourceRef,
		SourceID:           card.ID,
		Title:              card.Title,
		ActionSummary:      card.Title,
		Command:            append([]string{}, card.Command...),
		RequiredInputs:     append([]string{}, card.RequiredInputs...),
		ExpectedValidation: append([]string{}, card.ExpectedValidation...),
		StopConditions:     append([]string{}, card.StopConditions...),
		Boundary:           card.Boundary,
		Proof:              card.Proof,
		RequiresOperator:   card.RequiresOperator,
		EvidenceRefs:       evidenceRefs,
	}
	actionSummary := firstNonEmpty(card.NextAction, card.Title)
	ctx.ActionSummary = actionSummary
	return ReviewCardWorkDraft{
		SchemaVersion:         ReviewCardWorkPlanReportSchemaVersion,
		ID:                    idFor("wbrc_", card.ID, item.WorkID),
		Status:                "ready_for_work_plan",
		CardID:                card.ID,
		CardKind:              card.Kind,
		CardTitle:             card.Title,
		NextAction:            card.NextAction,
		ConditionStatus:       card.ConditionStatus,
		TriggeringEvidenceRef: card.TriggeringEvidenceRef,
		MatchedConditionRefs:  append([]string{}, card.MatchedConditionRefs...),
		SourceWorkID:          item.WorkID,
		GateID:                card.GateID,
		SourceKind:            card.SourceKind,
		CandidateID:           card.CandidateID,
		PanelRunID:            card.PanelRunID,
		PromotionSummaryRef:   card.PromotionSummaryRef,
		ActionCommand:         append([]string{}, card.Command...),
		RequiredInputs:        append([]string{}, card.RequiredInputs...),
		ExpectedValidation:    append([]string{}, card.ExpectedValidation...),
		StopConditions:        append([]string{}, card.StopConditions...),
		Boundary:              card.Boundary,
		Proof:                 card.Proof,
		WorkID:                idFor("work_", "workbench_review_card", card.ID),
		WorkOwner:             workOwner,
		SuccessPredicate:      successPredicate,
		DesiredOutcome:        desiredOutcome,
		NonGoals:              nonGoals,
		EvidenceRefs:          evidenceRefs,
		ActionContext:         ctx,
	}, nil
}

func findWorkbenchReviewCard(items []WorkbenchItem, cardID string) (WorkbenchItem, WorkbenchReviewCard, bool) {
	for _, item := range items {
		for _, card := range item.ReviewCards {
			if card.ID == cardID {
				return item, card, true
			}
		}
	}
	return WorkbenchItem{}, WorkbenchReviewCard{}, false
}
