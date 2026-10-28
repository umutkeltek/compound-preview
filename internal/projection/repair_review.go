package projection

import (
	"bytes"
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
	ProjectionRegenerationReviewArea      = "projection-regeneration"
	RegenerationReviewSchemaVersion       = "compound.projection_regeneration_review.v0.1"
	RegenerationReviewReportSchemaVersion = "compound.projection_regeneration_review_report.v0.1"
)

type RegenerationReview struct {
	SchemaVersion string   `json:"schema_version" yaml:"schema_version"`
	ID            string   `json:"id" yaml:"id"`
	PlanID        string   `json:"plan_id" yaml:"plan_id"`
	ProjectionID  string   `json:"projection_id" yaml:"projection_id"`
	ViewRef       string   `json:"view_ref" yaml:"view_ref"`
	Decision      string   `json:"decision" yaml:"decision"`
	Rationale     string   `json:"rationale" yaml:"rationale"`
	Confidence    float64  `json:"confidence" yaml:"confidence"`
	NextAction    string   `json:"next_action,omitempty" yaml:"next_action,omitempty"`
	EvidenceRefs  []string `json:"evidence_refs" yaml:"evidence_refs"`
	ReviewedAt    string   `json:"reviewed_at,omitempty" yaml:"reviewed_at,omitempty"`
	SourceRef     string   `json:"source_ref,omitempty" yaml:"source_ref,omitempty"`
}

type RegenerationReviewReport struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	CreatedAt     string   `json:"created_at"`
	Status        string   `json:"status"`
	PlanID        string   `json:"plan_id"`
	ReviewID      string   `json:"review_id"`
	ProjectionID  string   `json:"projection_id"`
	ViewRef       string   `json:"view_ref"`
	Decision      string   `json:"decision"`
	NextAction    string   `json:"next_action,omitempty"`
	SourcePath    string   `json:"source_path,omitempty"`
	ReviewPath    string   `json:"review_path,omitempty"`
	EvidenceRefs  []string `json:"evidence_refs,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

func RenderRegenerationReviewYAMLDraft(plan RegenerationPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version: %s\n", RegenerationReviewSchemaVersion)
	fmt.Fprintf(&b, "id: %s\n", reviewartifact.ID("prrv_", plan.ID, plan.ProjectionID, plan.ViewRef))
	fmt.Fprintf(&b, "plan_id: %s\n", plan.ID)
	fmt.Fprintf(&b, "projection_id: %s\n", plan.ProjectionID)
	fmt.Fprintf(&b, "view_ref: %q\n", plan.ViewRef)
	fmt.Fprintln(&b, "decision: TODO_decision_owner_action_required_owner_action_done_defer_no_action")
	fmt.Fprintln(&b, "rationale: \"\"")
	fmt.Fprintln(&b, "confidence: -1")
	fmt.Fprintln(&b, "next_action: \"TODO: reviewed projection regeneration next action\"")
	fmt.Fprintln(&b, "evidence_refs:")
	for _, ref := range reviewartifact.Unique(plan.EvidenceRefs) {
		fmt.Fprintf(&b, "  - %q\n", ref)
	}
	fmt.Fprintln(&b)
	return b.String()
}

func LoadRegenerationReview(path string) (RegenerationReview, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return RegenerationReview{}, err
	}
	var review RegenerationReview
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&review); err != nil {
		return RegenerationReview{}, err
	}
	return review, nil
}

func ValidateRegenerationReview(review RegenerationReview) error {
	if review.SchemaVersion != RegenerationReviewSchemaVersion {
		return fmt.Errorf("projection regeneration review schema_version must be %s, got %q", RegenerationReviewSchemaVersion, review.SchemaVersion)
	}
	if strings.TrimSpace(review.ID) == "" || strings.TrimSpace(review.PlanID) == "" {
		return fmt.Errorf("projection regeneration review id and plan_id required")
	}
	if review.PlanID != filepath.Base(review.PlanID) {
		return fmt.Errorf("projection regeneration review plan_id %q must be bare id", review.PlanID)
	}
	if strings.TrimSpace(review.ProjectionID) == "" || strings.TrimSpace(review.ViewRef) == "" {
		return fmt.Errorf("projection regeneration review projection_id and view_ref required")
	}
	if !reviewOneOf(review.Decision, []string{"owner_action_required", "owner_action_done", "defer", "no_action"}) {
		return fmt.Errorf("projection regeneration review decision must owner_action_required|owner_action_done|defer|no_action, got %q", review.Decision)
	}
	if strings.TrimSpace(review.Rationale) == "" {
		return fmt.Errorf("projection regeneration review rationale required")
	}
	if review.Confidence < 0 || review.Confidence > 1 {
		return fmt.Errorf("projection regeneration review confidence must be between 0 and 1")
	}
	if len(reviewartifact.Unique(review.EvidenceRefs)) == 0 {
		return fmt.Errorf("projection regeneration review evidence_refs required")
	}
	return nil
}

func RecordRegenerationReview(homePath string, review RegenerationReview, sourcePath string, now time.Time) (RegenerationReviewReport, error) {
	createdAt := reviewartifact.Timestamp(now)
	report := RegenerationReviewReport{
		SchemaVersion: RegenerationReviewReportSchemaVersion,
		ID:            reviewartifact.ID("prrr_", review.PlanID, review.ID, review.Decision, createdAt),
		CreatedAt:     createdAt,
		Status:        "recorded",
		PlanID:        review.PlanID,
		ReviewID:      review.ID,
		ProjectionID:  review.ProjectionID,
		ViewRef:       review.ViewRef,
		Decision:      review.Decision,
		NextAction:    strings.TrimSpace(review.NextAction),
		SourcePath:    strings.TrimSpace(sourcePath),
		EvidenceRefs:  reviewartifact.Unique(review.EvidenceRefs),
		Warnings: []string{
			"does not regenerate generated view",
			"does not mutate owner files",
			"does not execute generator or tools",
			"does not promote memory claims",
		},
	}
	if err := ValidateRegenerationReview(review); err != nil {
		report.Status = "blocked"
		return report, err
	}
	plan, err := loadRegenerationPlanForReview(sourcePath, review.PlanID)
	if err != nil {
		report.Status = "blocked"
		return report, err
	}
	if plan.ID != review.PlanID {
		report.Status = "blocked"
		return report, fmt.Errorf("projection regeneration review plan_id %q does not match plan %q", review.PlanID, plan.ID)
	}
	if review.ProjectionID != plan.ProjectionID {
		report.Status = "blocked"
		return report, fmt.Errorf("projection regeneration review projection_id %q does not match plan projection_id %q", review.ProjectionID, plan.ProjectionID)
	}
	if review.ViewRef != plan.ViewRef {
		report.Status = "blocked"
		return report, fmt.Errorf("projection regeneration review view_ref %q does not match plan view_ref %q", review.ViewRef, plan.ViewRef)
	}
	if review.ReviewedAt == "" {
		review.ReviewedAt = createdAt
	}
	review.SourceRef = "projection-regeneration-review:" + filepath.Base(sourcePath)
	report.ReviewPath = filepath.Join(filepath.Dir(sourcePath), review.PlanID+"."+review.ID+".reviewed.json")
	if _, err := reviewartifact.WriteJSON(homePath, ProjectionRegenerationReviewArea, filepath.Base(report.ReviewPath), report, " "); err != nil {
		return RegenerationReviewReport{}, err
	}
	return report, nil
}

func LoadRegenerationReviewReport(path string) (RegenerationReviewReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return RegenerationReviewReport{}, err
	}
	var report RegenerationReviewReport
	if err := json.Unmarshal(data, &report); err != nil {
		return RegenerationReviewReport{}, err
	}
	return report, nil
}

func AnnotateProjectionRegenerationQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	for i := range report.Items {
		if report.Items[i].Area != ProjectionRegenerationReviewArea {
			continue
		}
		annotateProjectionRegenerationItem(&report.Items[i])
	}
}

func annotateProjectionRegenerationItem(item *reviewartifact.QueueItem) {
	if item == nil {
		return
	}
	if item.JSONPath == "" {
		item.Readiness = "needs_review"
		item.BlockedReason = "projection regeneration plan path missing"
		return
	}
	if reviewedPath := projectionRegenerationReviewedPath(*item); reviewedPath != "" {
		item.Status = "recorded"
		item.Readiness = "review_recorded"
		item.NextAction = "projection regeneration review recorded; rerun projection doctor after owner regeneration or deferment"
		if report, err := LoadRegenerationReviewReport(reviewedPath); err == nil {
			item.NextAction = reviewartifact.FirstNonEmpty(report.NextAction, item.NextAction)
		}
		return
	}
	reviewPath := projectionRegenerationReviewYAMLPath(*item)
	if reviewPath == "" {
		item.Readiness = "needs_review"
		item.NextAction = "review projection regeneration plan"
		return
	}
	review, err := LoadRegenerationReview(reviewPath)
	if err != nil {
		item.Readiness = "needs_review"
		item.BlockedReason = "projection regeneration review YAML unreadable: " + projectionRegenerationQueueError(err)
		item.NextAction = "fix projection regeneration review YAML before recording"
		return
	}
	if err := ValidateRegenerationReview(review); err != nil {
		item.Readiness = "needs_review"
		item.BlockedReason = projectionRegenerationQueueError(err)
		item.NextAction = "fill valid projection regeneration review YAML before recording"
		return
	}
	item.Readiness = "ready_to_record"
	item.NextAction = "record reviewed projection regeneration disposition"
	item.Actions = append(item.Actions, reviewartifact.Action{
		ID:             "projection-regeneration-review-record",
		Label:          "Record projection regeneration review",
		Command:        "compound projection repair-review " + reviewPath,
		Argv:           []string{"compound", "projection", "repair-review", reviewPath},
		RequiresReview: true,
	})
}

func loadRegenerationPlanForReview(reviewPath, planID string) (RegenerationPlan, error) {
	if strings.TrimSpace(reviewPath) == "" {
		return RegenerationPlan{}, fmt.Errorf("projection regeneration review source path required")
	}
	if strings.TrimSpace(planID) == "" || planID != filepath.Base(planID) {
		return RegenerationPlan{}, fmt.Errorf("projection regeneration review plan_id %q must be bare id", planID)
	}
	planPath := filepath.Join(filepath.Dir(reviewPath), planID+".json")
	data, err := os.ReadFile(planPath)
	if err != nil {
		return RegenerationPlan{}, err
	}
	var plan RegenerationPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return RegenerationPlan{}, err
	}
	return plan, nil
}

func projectionRegenerationReviewYAMLPath(item reviewartifact.QueueItem) string {
	for _, path := range item.ExtraPaths {
		if strings.HasSuffix(path, ".review.yaml") {
			return path
		}
	}
	return ""
}

func projectionRegenerationReviewedPath(item reviewartifact.QueueItem) string {
	for _, path := range item.ExtraPaths {
		if strings.HasSuffix(path, ".reviewed.json") {
			return path
		}
	}
	return ""
}

func projectionRegenerationQueueError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ReplaceAll(err.Error(), "\n", " ")
	return strings.Join(strings.Fields(msg), " ")
}

func reviewOneOf(value string, allowed []string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
