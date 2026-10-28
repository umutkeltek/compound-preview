package product

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/reviewartifact"
)

const ReviewPlanSchemaVersion = "compound.product_review_plan.v0.1"

// ReviewPlan turns a pending product hypothesis plus explicit ProductSignal
// evidence into review artifacts. It does not resolve the hypothesis, infer
// causality, create claims, or mutate a product surface.
type ReviewPlan struct {
	SchemaVersion      string          `json:"schema_version"`
	ID                 string          `json:"id"`
	CreatedAt          string          `json:"created_at"`
	Status             string          `json:"status"`
	ProductID          string          `json:"product_id"`
	HypothesisID       string          `json:"hypothesis_id"`
	SignalID           string          `json:"signal_id"`
	ChangeEvent        string          `json:"change_event"`
	ExpectedEffect     string          `json:"expected_effect"`
	MetricName         string          `json:"metric_name"`
	Before             float64         `json:"before"`
	After              float64         `json:"after"`
	Window             string          `json:"window"`
	SignalConfidence   float64         `json:"signal_confidence"`
	Confounders        []string        `json:"confounders,omitempty"`
	EvidenceRefs       []string        `json:"evidence_refs"`
	TargetPath         string          `json:"target_path,omitempty"`
	TargetExists       bool            `json:"target_exists"`
	TargetHash         string          `json:"target_hash,omitempty"`
	ReviewDraft        ReviewPlanDraft `json:"review_draft"`
	ReviewedAt         string          `json:"reviewed_at,omitempty"`
	ReviewID           string          `json:"review_id,omitempty"`
	ReviewResult       string          `json:"review_result,omitempty"`
	ReviewEvidenceID   string          `json:"review_evidence_id,omitempty"`
	ReviewTransitionID string          `json:"review_transition_id,omitempty"`
	Warnings           []string        `json:"warnings,omitempty"`
}

type ReviewPlanDraft struct {
	ReviewID         string   `json:"review_id"`
	ReviewQuestion   string   `json:"review_question"`
	AllowedResults   []string `json:"allowed_results"`
	EvaluationMethod string   `json:"evaluation_method"`
	RequiredEvidence []string `json:"required_evidence"`
	ReviewCommand    string   `json:"review_command"`
	ReviewNote       string   `json:"review_note"`
}

type ReviewPlanPaths struct {
	PlanPath       string
	ReviewPath     string
	ReviewYAMLPath string
}

func BuildReviewPlan(homePath, hypothesisID, signalID, targetPath string, now time.Time) (ReviewPlan, error) {
	if strings.TrimSpace(homePath) == "" {
		return ReviewPlan{}, fmt.Errorf("COMPOUND_HOME required")
	}
	hypothesisID = strings.TrimSpace(hypothesisID)
	signalID = strings.TrimSpace(signalID)
	if hypothesisID == "" || signalID == "" {
		return ReviewPlan{}, fmt.Errorf("hypothesis and signal required")
	}
	h, err := loadHypothesisCandidate(homePath, hypothesisID)
	if err != nil {
		return ReviewPlan{}, err
	}
	if h.Kind != "" && h.Kind != string(candidates.ProductHypothesis) {
		return ReviewPlan{}, fmt.Errorf("candidate %q is %q, not product_hypothesis", hypothesisID, h.Kind)
	}
	if strings.TrimSpace(h.ProductID) == "" {
		return ReviewPlan{}, fmt.Errorf("product hypothesis %q missing product_id", hypothesisID)
	}
	if h.Result != "" && h.Result != "unknown" {
		return ReviewPlan{}, fmt.Errorf("product hypothesis %q already has result %q", hypothesisID, h.Result)
	}
	reviews, err := loadReviews(homePath)
	if err != nil {
		return ReviewPlan{}, err
	}
	for _, r := range reviews {
		if r.ProductID == h.ProductID && r.HypothesisID == h.ID {
			return ReviewPlan{}, fmt.Errorf("product hypothesis %q already reviewed by %q", h.ID, r.ID)
		}
	}
	signal, err := loadSignalByID(homePath, signalID)
	if err != nil {
		return ReviewPlan{}, err
	}
	if signal.ProductID != h.ProductID {
		return ReviewPlan{}, fmt.Errorf("signal %q product_id %q does not match hypothesis product_id %q", signal.ID, signal.ProductID, h.ProductID)
	}
	targetExists, targetHash, err := hashHypothesisPlanTarget(targetPath)
	if err != nil {
		return ReviewPlan{}, err
	}
	key := normalizeIDPart(strings.Join([]string{h.ProductID, h.ID, signal.ID}, " "))
	if key == "" {
		key = normalizeIDPart(h.ID + " " + signal.ID)
	}
	evidenceRefs := compact(append(append([]string{}, h.EvidenceRefs...), signal.EvidenceRefs...))
	evidenceRefs = compact(append(evidenceRefs, "product_hypothesis:"+h.ID, "product_signal:"+signal.ID))
	warnings := []string{
		"does not resolve the hypothesis",
		"does not infer causality from metric movement",
		"does not mutate product strategy, analytics, or store assets",
		"does not promote memory claims",
	}
	if len(signal.Confounders) == 0 {
		warnings = append(warnings, "signal has no confounders; reviewer must check external causes")
	}
	return ReviewPlan{
		SchemaVersion:    ReviewPlanSchemaVersion,
		ID:               stableID("prp_", h.ProductID, h.ID, signal.ID),
		CreatedAt:        now.UTC().Format(time.RFC3339),
		Status:           "review_required",
		ProductID:        h.ProductID,
		HypothesisID:     h.ID,
		SignalID:         signal.ID,
		ChangeEvent:      firstNonEmpty(h.ChangeEvent, signal.ChangeEvent),
		ExpectedEffect:   h.ExpectedEffect,
		MetricName:       signal.MetricName,
		Before:           signal.Before,
		After:            signal.After,
		Window:           signal.Window,
		SignalConfidence: signal.Confidence,
		Confounders:      compact(signal.Confounders),
		EvidenceRefs:     evidenceRefs,
		TargetPath:       strings.TrimSpace(targetPath),
		TargetExists:     targetExists,
		TargetHash:       targetHash,
		ReviewDraft: ReviewPlanDraft{
			ReviewID:         "review_" + key + "_" + stableID("", h.ID, signal.ID),
			ReviewQuestion:   "Does this ProductSignal support, refute, or leave inconclusive the pending product hypothesis?",
			AllowedResults:   []string{"supported", "refuted", "inconclusive"},
			EvaluationMethod: "Compare before/after signal movement, hypothesis expected effect, evidence refs, confounders, and product context; do not infer causality without review.",
			RequiredEvidence: []string{
				"ProductSignal " + signal.ID,
				"pending hypothesis " + h.ID,
				"metric window " + signal.Window,
				"confounder check before result",
			},
			ReviewCommand: "compound product review <review.yaml>",
			ReviewNote:    "Fill the YAML only after checking the signal evidence and confounders. The review importer records the result; this plan does not.",
		},
		Warnings: warnings,
	}, nil
}

func RecordReviewPlan(homePath string, plan ReviewPlan) (ReviewPlanPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return ReviewPlanPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if plan.ID == "" {
		return ReviewPlanPaths{}, fmt.Errorf("product review plan id required")
	}
	dir, err := reviewartifact.Dir(homePath, "product-reviews")
	if err != nil {
		return ReviewPlanPaths{}, err
	}
	paths := ReviewPlanPaths{
		PlanPath:       filepath.Join(dir, plan.ID+".json"),
		ReviewPath:     filepath.Join(dir, plan.ID+".md"),
		ReviewYAMLPath: filepath.Join(dir, plan.ID+".review.yaml"),
	}
	if _, err := reviewartifact.Write(homePath, "product-reviews", plan.ID, plan, " ", RenderReviewPlan(plan)); err != nil {
		return paths, err
	}
	if _, err := reviewartifact.WriteRaw(homePath, "product-reviews", plan.ID+".review.yaml", []byte(RenderReviewYAMLDraft(plan))); err != nil {
		return paths, err
	}
	return paths, nil
}

func RenderReviewPlan(plan ReviewPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Product Review Plan: %s\n\n", plan.HypothesisID)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- product: %s\n", plan.ProductID)
	fmt.Fprintf(&b, "- hypothesis: %s\n", plan.HypothesisID)
	fmt.Fprintf(&b, "- signal: %s\n", plan.SignalID)
	fmt.Fprintf(&b, "- metric: %s %.4g -> %.4g\n", plan.MetricName, plan.Before, plan.After)
	fmt.Fprintf(&b, "- window: %s\n", plan.Window)
	if plan.TargetPath != "" {
		fmt.Fprintf(&b, "- target: %s\n", plan.TargetPath)
		fmt.Fprintf(&b, "- target_exists: %t\n", plan.TargetExists)
		if plan.TargetHash != "" {
			fmt.Fprintf(&b, "- target_hash: %s\n", plan.TargetHash)
		}
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Guardrails")
	fmt.Fprintln(&b)
	for _, warning := range plan.Warnings {
		fmt.Fprintf(&b, "- %s\n", warning)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Review Draft")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- review_id: %s\n", plan.ReviewDraft.ReviewID)
	fmt.Fprintf(&b, "- question: %s\n", plan.ReviewDraft.ReviewQuestion)
	fmt.Fprintf(&b, "- allowed_results: %s\n", strings.Join(plan.ReviewDraft.AllowedResults, ", "))
	fmt.Fprintf(&b, "- review_command: `%s`\n", plan.ReviewDraft.ReviewCommand)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Required Evidence Before Review")
	fmt.Fprintln(&b)
	for _, evidence := range plan.ReviewDraft.RequiredEvidence {
		fmt.Fprintf(&b, "- %s\n", evidence)
	}
	if len(plan.Confounders) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Confounders")
		fmt.Fprintln(&b)
		for _, confounder := range plan.Confounders {
			fmt.Fprintf(&b, "- %s\n", confounder)
		}
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Review YAML Template")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "```yaml")
	fmt.Fprint(&b, RenderReviewYAMLDraft(plan))
	fmt.Fprintln(&b, "```")
	return b.String()
}

func RenderReviewYAMLDraft(plan ReviewPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version: %s\n", ReviewSchemaVersion)
	fmt.Fprintf(&b, "id: %s\n", plan.ReviewDraft.ReviewID)
	fmt.Fprintf(&b, "product_id: %s\n", plan.ProductID)
	fmt.Fprintf(&b, "hypothesis_id: %s\n", plan.HypothesisID)
	fmt.Fprintln(&b, "result: TODO_result_supported_refuted_or_inconclusive")
	fmt.Fprintln(&b, "rationale: \"\"")
	fmt.Fprintln(&b, "confidence: -1")
	fmt.Fprintln(&b, "next_action: \"TODO: next action after reviewed evidence\"")
	fmt.Fprintln(&b, "reviewed_by: TODO_reviewed_by_actor")
	fmt.Fprintln(&b, "evidence_refs:")
	for _, ref := range plan.EvidenceRefs {
		fmt.Fprintf(&b, " - %s\n", ref)
	}
	return b.String()
}

func loadSignalByID(homePath, signalID string) (Signal, error) {
	signals, err := loadSignals(homePath)
	if err != nil {
		return Signal{}, err
	}
	for _, signal := range signals {
		if signal.ID == signalID {
			return signal, nil
		}
	}
	return Signal{}, fmt.Errorf("product signal %q not found", signalID)
}
