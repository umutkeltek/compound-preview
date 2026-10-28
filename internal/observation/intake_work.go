package observation

import (
	"fmt"
	"strings"
	"time"

	"compound/internal/work"
)

const SourceIntakeWorkPlanReportSchemaVersion = "compound.source_intake_work_plan_report.v0.1"

type SourceIntakeWorkPlanOptions struct {
	Actor string
}

type SourceIntakeWorkDraft struct {
	SchemaVersion      string                 `json:"schema_version"`
	ID                 string                 `json:"id"`
	Status             string                 `json:"status"`
	CardID             string                 `json:"card_id"`
	CardKind           string                 `json:"card_kind"`
	CardTitle          string                 `json:"card_title"`
	RowID              string                 `json:"row_id"`
	RecordID           string                 `json:"record_id"`
	ObservationID      string                 `json:"observation_id"`
	Route              string                 `json:"route"`
	Owner              string                 `json:"owner,omitempty"`
	ActionCommand      []string               `json:"action_command,omitempty"`
	RequiredInputs     []string               `json:"required_inputs,omitempty"`
	ExpectedValidation []string               `json:"expected_validation,omitempty"`
	StopConditions     []string               `json:"stop_conditions,omitempty"`
	Boundary           string                 `json:"boundary,omitempty"`
	Proof              string                 `json:"proof,omitempty"`
	WorkID             string                 `json:"work_id"`
	WorkOwner          string                 `json:"work_owner"`
	SuccessPredicate   string                 `json:"success_predicate"`
	DesiredOutcome     string                 `json:"desired_outcome"`
	NonGoals           []string               `json:"non_goals,omitempty"`
	EvidenceRefs       []string               `json:"evidence_refs,omitempty"`
	ActionContext      work.WorkActionContext `json:"action_context"`
}

type SourceIntakeWorkPlanReport struct {
	SchemaVersion string                `json:"schema_version"`
	Status        string                `json:"status"`
	SourceIntake  SourceIntakeWorkDraft `json:"source_intake"`
	WorkPlan      work.CreatePlan       `json:"work_plan"`
}

func BuildSourceIntakeWorkPlan(homePath string, store *work.Store, cardID string, now time.Time, opts SourceIntakeWorkPlanOptions) (SourceIntakeWorkPlanReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return SourceIntakeWorkPlanReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if store == nil {
		return SourceIntakeWorkPlanReport{}, fmt.Errorf("work store required")
	}
	draft, err := BuildSourceIntakeWorkDraft(homePath, cardID, now)
	if err != nil {
		return SourceIntakeWorkPlanReport{}, err
	}
	actor := strings.TrimSpace(opts.Actor)
	if actor == "" {
		actor = "codex"
	}
	plan, err := store.CreatePlan(work.CreateInput{
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
	}, now, work.CreatePlanOptions{
		Actor:         actor,
		Capability:    "source.intake",
		Action:        "plan_work_from_source_intake_action_card",
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		TargetKind:    "source_intake_action_card",
		TargetID:      draft.CardID,
		TargetPath:    draft.ActionContext.SourceRef,
		ActionContext: &draft.ActionContext,
		Record:        true,
	})
	if err != nil {
		return SourceIntakeWorkPlanReport{}, err
	}
	return SourceIntakeWorkPlanReport{
		SchemaVersion: SourceIntakeWorkPlanReportSchemaVersion,
		Status:        "recorded",
		SourceIntake:  draft,
		WorkPlan:      plan,
	}, nil
}

func BuildSourceIntakeWorkDraft(homePath, cardID string, now time.Time) (SourceIntakeWorkDraft, error) {
	cardID = strings.TrimSpace(cardID)
	if cardID == "" {
		return SourceIntakeWorkDraft{}, fmt.Errorf("source-intake action card id required")
	}
	projection, err := LoadSourceIntakeProjection(homePath, now, 0)
	if err != nil {
		return SourceIntakeWorkDraft{}, err
	}
	row, card, ok := findSourceIntakeCard(projection.Rows, cardID)
	if !ok {
		return SourceIntakeWorkDraft{}, fmt.Errorf("source-intake action card %q not found", cardID)
	}
	sourceRef := "compound://sources/intake/action-cards/" + card.ID
	evidenceRefs := compact(append(append([]string{
		"source_intake_projection:" + SourceIntakeAuthorityRef,
		"source_intake_record:" + row.RecordID,
		"source_intake_row:" + row.ID,
		"source_intake_card:" + card.ID,
	}, row.EvidenceRefs...), row.ReportPath))
	successPredicate := fmt.Sprintf("Resolve source-intake action card %s (%s) by running `%s`; proof: %s", card.ID, card.Kind, strings.Join(card.Command, " "), card.Proof)
	desiredOutcome := fmt.Sprintf("Source-intake row %s no longer needs this action, or remains explicitly visible with reason and evidence", row.ID)
	nonGoals := []string{
		"do not execute the source-intake action command while creating or admitting this work-plan",
		"do not call external accounts, fetch private collectors, post, spend, or mutate provider configuration",
		"do not clear held operator valves or promote memory from this admission preview",
		"do not create a second source-intake ledger or provider-specific connector authority",
	}
	ctx := work.WorkActionContext{
		Kind:               "source_intake_action_card",
		SourceRef:          sourceRef,
		SourceID:           card.ID,
		Title:              card.Title,
		Command:            append([]string{}, card.Command...),
		RequiredInputs:     append([]string{}, card.RequiredInputs...),
		ExpectedValidation: append([]string{}, card.ExpectedValidation...),
		StopConditions:     append([]string{}, card.StopConditions...),
		Boundary:           card.Boundary,
		Proof:              card.Proof,
		EvidenceRefs:       evidenceRefs,
	}
	return SourceIntakeWorkDraft{
		SchemaVersion:      SourceIntakeWorkPlanReportSchemaVersion,
		ID:                 stableID("siwork_", card.ID, row.ID),
		Status:             "ready_for_work_plan",
		CardID:             card.ID,
		CardKind:           card.Kind,
		CardTitle:          card.Title,
		RowID:              row.ID,
		RecordID:           row.RecordID,
		ObservationID:      row.ObservationID,
		Route:              row.Route,
		Owner:              row.Owner,
		ActionCommand:      append([]string{}, card.Command...),
		RequiredInputs:     append([]string{}, card.RequiredInputs...),
		ExpectedValidation: append([]string{}, card.ExpectedValidation...),
		StopConditions:     append([]string{}, card.StopConditions...),
		Boundary:           card.Boundary,
		Proof:              card.Proof,
		WorkID:             stableID("work_", "source_intake_action_card", card.ID),
		WorkOwner:          "internal/observation",
		SuccessPredicate:   successPredicate,
		DesiredOutcome:     desiredOutcome,
		NonGoals:           nonGoals,
		EvidenceRefs:       evidenceRefs,
		ActionContext:      ctx,
	}, nil
}

func findSourceIntakeCard(rows []SourceIntakeRow, cardID string) (SourceIntakeRow, SourceIntakeActionCard, bool) {
	for _, row := range rows {
		for _, card := range row.ActionCards {
			if card.ID == cardID {
				return row, card, true
			}
		}
	}
	return SourceIntakeRow{}, SourceIntakeActionCard{}, false
}
