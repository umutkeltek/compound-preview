package product

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/reviewartifact"
	"gopkg.in/yaml.v3"
)

const (
	ActionPlanSchemaVersion         = "compound.product_action_plan.v0.1"
	ActionReviewSchemaVersion       = "compound.product_action_review.v0.1"
	ActionReviewReportSchemaVersion = "compound.product_action_review_report.v0.1"
	ProductActionReviewArea         = "product-action-cards"
)

// ActionPlan turns a projected product loop action card into a review artifact.
// It admits no work and mutates no product loop ledgers.
type ActionPlan struct {
	SchemaVersion      string          `json:"schema_version"`
	ID                 string          `json:"id"`
	CreatedAt          string          `json:"created_at"`
	Status             string          `json:"status"`
	ProductID          string          `json:"product_id"`
	ProductName        string          `json:"product_name,omitempty"`
	CardID             string          `json:"card_id"`
	CardKind           string          `json:"card_kind"`
	CardTitle          string          `json:"card_title"`
	Command            []string        `json:"command,omitempty"`
	ActionSummary      string          `json:"action_summary,omitempty"`
	RequiredInputs     []string        `json:"required_inputs,omitempty"`
	ExpectedValidation []string        `json:"expected_validation,omitempty"`
	StopConditions     []string        `json:"stop_conditions,omitempty"`
	Boundary           string          `json:"boundary,omitempty"`
	Proof              string          `json:"proof,omitempty"`
	EvidenceRefs       []string        `json:"evidence_refs"`
	TargetPath         string          `json:"target_path,omitempty"`
	TargetExists       bool            `json:"target_exists"`
	TargetHash         string          `json:"target_hash,omitempty"`
	ReviewDraft        ActionPlanDraft `json:"review_draft"`
	ReviewedAt         string          `json:"reviewed_at,omitempty"`
	ReviewID           string          `json:"review_id,omitempty"`
	ReviewDecision     string          `json:"review_decision,omitempty"`
	ReviewedPath       string          `json:"reviewed_path,omitempty"`
	Warnings           []string        `json:"warnings,omitempty"`
}

type ActionPlanDraft struct {
	ReviewID         string   `json:"review_id"`
	Title            string   `json:"title"`
	AllowedDecisions []string `json:"allowed_decisions"`
	RequiredEvidence []string `json:"required_evidence"`
	ReviewCommand    string   `json:"review_command"`
	ReviewNote       string   `json:"review_note"`
}

type ActionPlanPaths struct {
	PlanPath       string
	ReviewPath     string
	ReviewYAMLPath string
}

type ActionReview struct {
	SchemaVersion string   `json:"schema_version" yaml:"schema_version"`
	ID            string   `json:"id" yaml:"id"`
	PlanID        string   `json:"plan_id" yaml:"plan_id"`
	ProductID     string   `json:"product_id" yaml:"product_id"`
	CardID        string   `json:"card_id" yaml:"card_id"`
	Decision      string   `json:"decision" yaml:"decision"`
	Rationale     string   `json:"rationale" yaml:"rationale"`
	Confidence    float64  `json:"confidence" yaml:"confidence"`
	EvidenceRefs  []string `json:"evidence_refs" yaml:"evidence_refs"`
	ReviewedBy    string   `json:"reviewed_by,omitempty" yaml:"reviewed_by,omitempty"`
	ReviewedAt    string   `json:"reviewed_at,omitempty" yaml:"reviewed_at,omitempty"`
	SourceRef     string   `json:"source_ref,omitempty" yaml:"source_ref,omitempty"`
}

type ActionReviewReport struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	CreatedAt     string   `json:"created_at"`
	Status        string   `json:"status"`
	ProductID     string   `json:"product_id"`
	CardID        string   `json:"card_id"`
	CardKind      string   `json:"card_kind"`
	PlanID        string   `json:"plan_id"`
	ReviewID      string   `json:"review_id"`
	Decision      string   `json:"decision"`
	ReviewedBy    string   `json:"reviewed_by,omitempty"`
	EvidenceRefs  []string `json:"evidence_refs"`
	ReviewPath    string   `json:"review_path,omitempty"`
	ReviewedPath  string   `json:"reviewed_path,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

type ActionWorkDraft struct {
	SchemaVersion      string   `json:"schema_version"`
	ID                 string   `json:"id"`
	Status             string   `json:"status"`
	ProductID          string   `json:"product_id"`
	PlanID             string   `json:"plan_id"`
	ReviewReportID     string   `json:"review_report_id"`
	ReviewID           string   `json:"review_id"`
	Decision           string   `json:"decision"`
	CardID             string   `json:"card_id"`
	CardKind           string   `json:"card_kind"`
	CardTitle          string   `json:"card_title"`
	ActionSummary      string   `json:"action_summary,omitempty"`
	ActionCommand      []string `json:"action_command,omitempty"`
	RequiredInputs     []string `json:"required_inputs,omitempty"`
	ExpectedValidation []string `json:"expected_validation,omitempty"`
	StopConditions     []string `json:"stop_conditions,omitempty"`
	Boundary           string   `json:"boundary,omitempty"`
	Proof              string   `json:"proof,omitempty"`
	WorkID             string   `json:"work_id"`
	WorkOwner          string   `json:"work_owner"`
	SuccessPredicate   string   `json:"success_predicate"`
	DesiredOutcome     string   `json:"desired_outcome"`
	NonGoals           []string `json:"non_goals"`
	EvidenceRefs       []string `json:"evidence_refs"`
}

func BuildActionPlan(homePath, productID, cardID, targetPath string, now time.Time) (ActionPlan, error) {
	homePath = strings.TrimSpace(homePath)
	productID = strings.TrimSpace(productID)
	cardID = strings.TrimSpace(cardID)
	if homePath == "" {
		return ActionPlan{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if productID == "" || cardID == "" {
		return ActionPlan{}, fmt.Errorf("product_id card_id required")
	}
	model, err := BuildLoopModel(homePath, LoopOptions{ProductID: productID, Now: now})
	if err != nil {
		return ActionPlan{}, err
	}
	if len(model.Products) == 0 {
		return ActionPlan{}, fmt.Errorf("product loop %q not found", productID)
	}
	loop := model.Products[0]
	card, ok := loopActionCardByID(loop.ActionCards, cardID)
	if !ok {
		return ActionPlan{}, fmt.Errorf("product action card %q not found for %s", cardID, productID)
	}
	targetExists, targetHash, err := reviewartifact.HashFileTarget(targetPath)
	if err != nil {
		return ActionPlan{}, err
	}
	evidenceRefs := actionCardEvidenceRefs(loop, card)
	reviewID := "review_" + normalizeIDPart(productID+" "+card.ID+" "+card.Kind)
	return ActionPlan{
		SchemaVersion:      ActionPlanSchemaVersion,
		ID:                 stableID("pacp_", productID, card.ID),
		CreatedAt:          now.UTC().Format(time.RFC3339),
		Status:             "review_required",
		ProductID:          productID,
		ProductName:        loop.Name,
		CardID:             card.ID,
		CardKind:           card.Kind,
		CardTitle:          card.Title,
		Command:            append([]string{}, card.Command...),
		ActionSummary:      strings.TrimSpace(card.ActionSummary),
		RequiredInputs:     append([]string{}, card.RequiredInputs...),
		ExpectedValidation: append([]string{}, card.ExpectedValidation...),
		StopConditions:     append([]string{}, card.StopConditions...),
		Boundary:           card.Boundary,
		Proof:              card.Proof,
		EvidenceRefs:       evidenceRefs,
		TargetPath:         strings.TrimSpace(targetPath),
		TargetExists:       targetExists,
		TargetHash:         targetHash,
		ReviewDraft: ActionPlanDraft{
			ReviewID:         reviewID,
			Title:            "Review product action card before admitting work",
			AllowedDecisions: []string{"propose_work", "park", "reject"},
			RequiredEvidence: []string{
				"action card still exists in current product-loop projection",
				"reviewer can name the intended state delta and proof command",
				"operator-only/public/financial/security-sensitive gates are not bypassed",
			},
			ReviewCommand: "compound product action-review <review.yaml>",
			ReviewNote:    "Records only the action-card review decision. It does not create WorkEnvelope, mutate product ledgers, publish, spend, or call external accounts.",
		},
		Warnings: []string{
			"does not admit action card into WorkEnvelope execution",
			"does not mutate ProductSignal, ProductHypothesis, Review, or Transition ledgers",
			"does not grant public, financial, credential, security, or destructive authority",
		},
	}, nil
}

func RecordActionPlan(homePath string, plan ActionPlan) (ActionPlanPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return ActionPlanPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(plan.ID) == "" {
		return ActionPlanPaths{}, fmt.Errorf("product action plan id required")
	}
	dir, err := reviewartifact.Dir(homePath, ProductActionReviewArea)
	if err != nil {
		return ActionPlanPaths{}, err
	}
	paths := ActionPlanPaths{
		PlanPath:       filepath.Join(dir, plan.ID+".json"),
		ReviewPath:     filepath.Join(dir, plan.ID+".md"),
		ReviewYAMLPath: filepath.Join(dir, plan.ID+".review.yaml"),
	}
	if _, err := reviewartifact.Write(homePath, ProductActionReviewArea, plan.ID, plan, " ", RenderActionPlan(plan)); err != nil {
		return paths, err
	}
	if _, err := reviewartifact.WriteRaw(homePath, ProductActionReviewArea, plan.ID+".review.yaml", []byte(RenderActionReviewYAML(plan))); err != nil {
		return paths, err
	}
	return paths, nil
}

func RenderActionPlan(plan ActionPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Product Action Plan: %s\n\n", plan.CardID)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- product: %s\n", plan.ProductID)
	if plan.ProductName != "" {
		fmt.Fprintf(&b, "- product_name: %s\n", plan.ProductName)
	}
	fmt.Fprintf(&b, "- card_kind: %s\n", plan.CardKind)
	fmt.Fprintf(&b, "- card_title: %s\n", plan.CardTitle)
	if len(plan.Command) > 0 {
		fmt.Fprintf(&b, "- command: `%s`\n", strings.Join(plan.Command, " "))
	}
	if strings.TrimSpace(plan.ActionSummary) != "" {
		fmt.Fprintf(&b, "- action_summary: %s\n", strings.TrimSpace(plan.ActionSummary))
	}
	if plan.TargetPath != "" {
		fmt.Fprintf(&b, "- target: %s\n", plan.TargetPath)
		fmt.Fprintf(&b, "- target_exists: %t\n", plan.TargetExists)
	}
	if plan.TargetHash != "" {
		fmt.Fprintf(&b, "- target_hash: %s\n", plan.TargetHash)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Boundary")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "%s\n\n", plan.Boundary)
	fmt.Fprintln(&b, "## Proof")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "%s\n\n", plan.Proof)
	fmt.Fprintln(&b, "## Required Inputs")
	fmt.Fprintln(&b)
	for _, input := range plan.RequiredInputs {
		fmt.Fprintf(&b, "- %s\n", input)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Expected Validation")
	fmt.Fprintln(&b)
	for _, validation := range plan.ExpectedValidation {
		fmt.Fprintf(&b, "- %s\n", validation)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Stop Conditions")
	fmt.Fprintln(&b)
	for _, stop := range plan.StopConditions {
		fmt.Fprintf(&b, "- %s\n", stop)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Evidence Refs")
	fmt.Fprintln(&b)
	for _, ref := range plan.EvidenceRefs {
		fmt.Fprintf(&b, "- %s\n", ref)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Review Draft")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- review_id: %s\n", plan.ReviewDraft.ReviewID)
	fmt.Fprintf(&b, "- allowed_decisions: %s\n", strings.Join(plan.ReviewDraft.AllowedDecisions, ", "))
	fmt.Fprintf(&b, "- review_command: `%s`\n", plan.ReviewDraft.ReviewCommand)
	return b.String()
}

func RenderActionReviewYAML(plan ActionPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version: %s\n", ActionReviewSchemaVersion)
	fmt.Fprintf(&b, "id: %s\n", plan.ReviewDraft.ReviewID)
	fmt.Fprintf(&b, "plan_id: %s\n", plan.ID)
	fmt.Fprintf(&b, "product_id: %s\n", plan.ProductID)
	fmt.Fprintf(&b, "card_id: %s\n", plan.CardID)
	fmt.Fprintln(&b, "decision: TODO_decision_propose_work_park_reject")
	fmt.Fprintln(&b, "rationale: TODO_rationale")
	fmt.Fprintln(&b, "confidence: TODO_confidence_0_to_1")
	fmt.Fprintln(&b, "reviewed_by: TODO_reviewed_by_actor")
	fmt.Fprintln(&b, "evidence_refs:")
	for _, ref := range plan.EvidenceRefs {
		fmt.Fprintf(&b, " - %q\n", ref)
	}
	return b.String()
}

func LoadActionReview(path string) (ActionReview, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ActionReview{}, err
	}
	var review ActionReview
	if err := yaml.Unmarshal(data, &review); err != nil {
		return ActionReview{}, err
	}
	return review, nil
}

func LoadActionReviewReport(path string) (ActionReviewReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ActionReviewReport{}, err
	}
	var report ActionReviewReport
	if err := json.Unmarshal(data, &report); err != nil {
		return ActionReviewReport{}, err
	}
	if report.SchemaVersion != ActionReviewReportSchemaVersion {
		return ActionReviewReport{}, fmt.Errorf("schema_version must be %s, got %q", ActionReviewReportSchemaVersion, report.SchemaVersion)
	}
	if strings.TrimSpace(report.ID) == "" || strings.TrimSpace(report.PlanID) == "" {
		return ActionReviewReport{}, fmt.Errorf("action review report id plan_id required")
	}
	return report, nil
}

func BuildActionWorkDraft(homePath, reviewedPath string) (ActionWorkDraft, error) {
	homePath = strings.TrimSpace(homePath)
	reviewedPath = strings.TrimSpace(reviewedPath)
	if homePath == "" {
		return ActionWorkDraft{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if reviewedPath == "" {
		return ActionWorkDraft{}, fmt.Errorf("reviewed action report path required")
	}
	report, err := LoadActionReviewReport(reviewedPath)
	if err != nil {
		return ActionWorkDraft{}, err
	}
	if report.Status != "recorded" {
		return ActionWorkDraft{}, fmt.Errorf("action review report %q status must be recorded, got %q", report.ID, report.Status)
	}
	if report.Decision != "propose_work" {
		return ActionWorkDraft{}, fmt.Errorf("action review report %q decision must be propose_work, got %q", report.ID, report.Decision)
	}
	plan, err := loadActionPlanArtifact(homePath, report.PlanID)
	if err != nil {
		return ActionWorkDraft{}, err
	}
	if plan.ProductID != report.ProductID || plan.CardID != report.CardID {
		return ActionWorkDraft{}, fmt.Errorf("action review report does not match action plan product/card")
	}
	commandText := strings.Join(plan.Command, " ")
	actionSummary := strings.TrimSpace(plan.ActionSummary)
	actionTarget := commandText
	if actionSummary != "" {
		actionTarget = actionSummary
	}
	successPredicate := fmt.Sprintf("Complete reviewed product action card %s: %s; proof: %s", plan.CardID, actionTarget, plan.Proof)
	nonGoals := []string{
		"do not run the action command while generating this work-plan preview",
		"do not mutate ProductSignal, ProductHypothesis, Review, or Transition ledgers during admission preview",
		"do not perform public, financial, credential, destructive, or security-sensitive action without explicit policy",
	}
	if actionSummary != "" && strings.Contains(commandText, "product action-plan") {
		nonGoals = append(nonGoals, "do not treat product action-plan review-gate command as completed product action")
	}
	if strings.TrimSpace(plan.Boundary) != "" {
		nonGoals = append(nonGoals, "do not exceed reviewed boundary: "+strings.TrimSpace(plan.Boundary))
	}
	for _, stop := range plan.StopConditions {
		stop = strings.TrimSpace(stop)
		if stop != "" {
			nonGoals = append(nonGoals, "stop if: "+stop)
		}
	}
	evidenceRefs := append([]string{}, report.EvidenceRefs...)
	evidenceRefs = append(evidenceRefs,
		"product_action_plan:"+plan.ID,
		"product_action_review:"+report.ID,
		"compound://reviews/"+ProductActionReviewArea+"/"+filepath.Base(reviewedPath),
	)
	return ActionWorkDraft{
		SchemaVersion:      "compound.product_action_work_draft.v0.1",
		ID:                 stableID("pawd_", plan.ID, report.ID),
		Status:             "work_plan_ready",
		ProductID:          plan.ProductID,
		PlanID:             plan.ID,
		ReviewReportID:     report.ID,
		ReviewID:           report.ReviewID,
		Decision:           report.Decision,
		CardID:             plan.CardID,
		CardKind:           plan.CardKind,
		CardTitle:          plan.CardTitle,
		ActionSummary:      actionSummary,
		ActionCommand:      append([]string{}, plan.Command...),
		RequiredInputs:     append([]string{}, plan.RequiredInputs...),
		ExpectedValidation: append([]string{}, plan.ExpectedValidation...),
		StopConditions:     append([]string{}, plan.StopConditions...),
		Boundary:           plan.Boundary,
		Proof:              plan.Proof,
		WorkID:             stableID("work_", "product_action_review", report.ID),
		WorkOwner:          plan.ProductID,
		SuccessPredicate:   successPredicate,
		DesiredOutcome:     fmt.Sprintf("Reviewed product action %s becomes explicit bounded work for %s without bypassing gates: %s", report.ReviewID, plan.ProductID, actionTarget),
		NonGoals:           reviewartifact.Unique(nonGoals),
		EvidenceRefs:       reviewartifact.UniqueSorted(evidenceRefs),
	}, nil
}

func ApplyActionReview(homePath string, review ActionReview, sourcePath string, now time.Time) (ActionReviewReport, error) {
	if err := ValidateActionReview(review); err != nil {
		return ActionReviewReport{Status: "fail", ProductID: review.ProductID, CardID: review.CardID, ReviewID: review.ID}, err
	}
	plan, err := loadActionPlanArtifact(homePath, review.PlanID)
	if err != nil {
		return ActionReviewReport{}, err
	}
	if plan.ProductID != review.ProductID || plan.CardID != review.CardID {
		return ActionReviewReport{}, fmt.Errorf("action review does not match plan product/card")
	}
	if review.ReviewedAt == "" {
		review.ReviewedAt = now.UTC().Format(time.RFC3339)
	}
	review.SourceRef = "product-action-review:" + filepath.Base(sourcePath)
	evidenceRefs := reviewartifact.UniqueSorted(append(append([]string{}, plan.EvidenceRefs...), review.EvidenceRefs...))
	report := ActionReviewReport{
		SchemaVersion: ActionReviewReportSchemaVersion,
		ID:            reviewartifact.ID("pacr_", plan.ID, review.ID, review.Decision),
		CreatedAt:     now.UTC().Format(time.RFC3339),
		Status:        "recorded",
		ProductID:     review.ProductID,
		CardID:        review.CardID,
		CardKind:      plan.CardKind,
		PlanID:        plan.ID,
		ReviewID:      review.ID,
		Decision:      review.Decision,
		ReviewedBy:    strings.TrimSpace(review.ReviewedBy),
		EvidenceRefs:  evidenceRefs,
		ReviewPath:    strings.TrimSpace(sourcePath),
		Warnings: []string{
			"does not admit action card into WorkEnvelope execution",
			"does not mutate product loop ledgers",
			"does not grant public, financial, credential, security, or destructive authority",
		},
	}
	dir, err := reviewartifact.Dir(homePath, ProductActionReviewArea)
	if err != nil {
		return ActionReviewReport{}, err
	}
	name := plan.ID + "." + report.ID + ".reviewed.json"
	report.ReviewedPath = filepath.Join(dir, name)
	if _, err := reviewartifact.WriteJSON(homePath, ProductActionReviewArea, name, report, " "); err != nil {
		return ActionReviewReport{}, err
	}
	plan.Status = "recorded"
	plan.ReviewedAt = report.CreatedAt
	plan.ReviewID = review.ID
	plan.ReviewDecision = review.Decision
	plan.ReviewedPath = report.ReviewedPath
	if _, err := reviewartifact.Write(homePath, ProductActionReviewArea, plan.ID, plan, " ", RenderActionPlan(plan)); err != nil {
		return ActionReviewReport{}, err
	}
	return report, nil
}

func ValidateActionReview(review ActionReview) error {
	if review.SchemaVersion != ActionReviewSchemaVersion {
		return fmt.Errorf("schema_version must be %s, got %q", ActionReviewSchemaVersion, review.SchemaVersion)
	}
	if strings.TrimSpace(review.ID) == "" || strings.TrimSpace(review.PlanID) == "" || strings.TrimSpace(review.ProductID) == "" || strings.TrimSpace(review.CardID) == "" {
		return fmt.Errorf("action review id plan_id product_id card_id required")
	}
	if strings.ContainsAny(review.PlanID, `/\`) || strings.ContainsAny(review.CardID, `/\`) {
		return fmt.Errorf("action review plan_id/card_id must not contain path separators")
	}
	if !oneOf(review.Decision, []string{"propose_work", "park", "reject"}) {
		return fmt.Errorf("action review decision must be propose_work|park|reject, got %q", review.Decision)
	}
	if strings.Contains(review.Decision, "TODO") || strings.TrimSpace(review.Rationale) == "" || strings.Contains(review.Rationale, "TODO") {
		return fmt.Errorf("action review rationale and decision must be completed")
	}
	if strings.TrimSpace(review.ReviewedBy) == "" || strings.Contains(review.ReviewedBy, "TODO") {
		return fmt.Errorf("action review reviewed_by required (name the judge; agent lanes annotate, operators adjudicate)")
	}
	if review.Confidence < 0 || review.Confidence > 1 {
		return fmt.Errorf("action review confidence must be between 0 and 1")
	}
	if len(compact(review.EvidenceRefs)) == 0 {
		return fmt.Errorf("action review evidence_refs required")
	}
	for _, ref := range review.EvidenceRefs {
		if strings.Contains(ref, "TODO") {
			return fmt.Errorf("action review evidence_refs must be completed")
		}
	}
	return nil
}

func loadActionPlanArtifact(homePath, planID string) (ActionPlan, error) {
	if strings.TrimSpace(planID) == "" || strings.ContainsAny(planID, `/\`) {
		return ActionPlan{}, fmt.Errorf("product action plan id invalid")
	}
	path := filepath.Join(homePath, "reviews", ProductActionReviewArea, planID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ActionPlan{}, err
	}
	var plan ActionPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return ActionPlan{}, err
	}
	if plan.ID == "" {
		return ActionPlan{}, fmt.Errorf("product action plan %s missing id", path)
	}
	return plan, nil
}

func loopActionCardByID(cards []LoopActionCard, id string) (LoopActionCard, bool) {
	for _, card := range cards {
		if card.ID == id {
			return card, true
		}
	}
	return LoopActionCard{}, false
}

func actionCardEvidenceRefs(loop ProductLoop, card LoopActionCard) []string {
	refs := []string{
		"product_loop:" + loop.ProductID,
		"product_action_card:" + card.ID,
	}
	for _, group := range loop.ClosedReviewsByNextAction {
		if !loopActionCardGroupHasID(group.ActionCards, card.ID) {
			continue
		}
		for _, id := range group.ReviewIDs {
			refs = append(refs, "product_review:"+id)
		}
		for _, id := range group.HypothesisIDs {
			refs = append(refs, "product_hypothesis:"+id)
		}
		for _, id := range group.SignalIDs {
			refs = append(refs, "product_signal:"+id)
		}
	}
	for _, group := range loop.OpenHypothesesByMissingSignalSource {
		if !loopActionCardGroupHasID(group.ActionCards, card.ID) {
			continue
		}
		for _, id := range group.HypothesisIDs {
			refs = append(refs, "product_hypothesis:"+id)
		}
		if group.Source != "" {
			refs = append(refs, "product_signal_source:"+group.Source)
		}
	}
	for _, group := range loop.SignalsWithoutHypothesis {
		if !loopActionCardGroupHasID(group.ActionCards, card.ID) {
			continue
		}
		for _, id := range group.SignalIDs {
			refs = append(refs, "product_signal:"+id)
		}
		if group.ChangeEvent != "" {
			refs = append(refs, "product_signal_change_event:"+normalizeIDPart(group.ChangeEvent))
		}
	}
	if card.Kind == "product_review_plan" {
		for _, input := range card.RequiredInputs {
			input = strings.TrimSpace(input)
			if id, ok := strings.CutPrefix(input, "hypothesis: "); ok {
				refs = append(refs, "product_hypothesis:"+strings.TrimSpace(id))
			}
			if id, ok := strings.CutPrefix(input, "product signal: "); ok {
				refs = append(refs, "product_signal:"+strings.TrimSpace(id))
			}
		}
	}
	return reviewartifact.UniqueSorted(refs)
}

func loopActionCardGroupHasID(cards []LoopActionCard, id string) bool {
	for _, card := range cards {
		if card.ID == id {
			return true
		}
	}
	return false
}
