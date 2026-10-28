package runtimebinding

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
	RepairReviewSchemaVersion       = "compound.runtime_repair_review.v0.1"
	RepairReviewReportSchemaVersion = "compound.runtime_repair_review_report.v0.1"
)

type RepairReview struct {
	SchemaVersion string   `json:"schema_version" yaml:"schema_version"`
	ID            string   `json:"id" yaml:"id"`
	PlanID        string   `json:"plan_id" yaml:"plan_id"`
	ServiceID     string   `json:"service_id" yaml:"service_id"`
	Owner         string   `json:"owner,omitempty" yaml:"owner,omitempty"`
	Decision      string   `json:"decision" yaml:"decision"`
	Rationale     string   `json:"rationale" yaml:"rationale"`
	Confidence    float64  `json:"confidence" yaml:"confidence"`
	NextAction    string   `json:"next_action,omitempty" yaml:"next_action,omitempty"`
	EvidenceRefs  []string `json:"evidence_refs" yaml:"evidence_refs"`
	ReviewedAt    string   `json:"reviewed_at,omitempty" yaml:"reviewed_at,omitempty"`
	SourceRef     string   `json:"source_ref,omitempty" yaml:"source_ref,omitempty"`
}

type RepairReviewReport struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	CreatedAt     string   `json:"created_at"`
	Status        string   `json:"status"`
	PlanID        string   `json:"plan_id"`
	ReviewID      string   `json:"review_id"`
	ServiceID     string   `json:"service_id"`
	Owner         string   `json:"owner,omitempty"`
	Decision      string   `json:"decision"`
	NextAction    string   `json:"next_action,omitempty"`
	SourcePath    string   `json:"source_path,omitempty"`
	ReviewPath    string   `json:"review_path,omitempty"`
	EvidenceRefs  []string `json:"evidence_refs,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

func RenderRepairReviewYAMLDraft(plan RepairPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version: %s\n", RepairReviewSchemaVersion)
	fmt.Fprintf(&b, "id: %s\n", reviewartifact.ID("rrv_", plan.ID, plan.ServiceID, plan.Owner))
	fmt.Fprintf(&b, "plan_id: %s\n", plan.ID)
	fmt.Fprintf(&b, "service_id: %q\n", plan.ServiceID)
	if strings.TrimSpace(plan.Owner) != "" {
		fmt.Fprintf(&b, "owner: %q\n", plan.Owner)
	}
	fmt.Fprintln(&b, "decision: TODO_decision_owner_action_required_owner_action_done_defer_no_action")
	fmt.Fprintln(&b, "rationale: \"\"")
	fmt.Fprintln(&b, "confidence: -1")
	fmt.Fprintln(&b, "next_action: \"TODO: reviewed runtime repair next action\"")
	fmt.Fprintln(&b, "evidence_refs:")
	for _, ref := range reviewartifact.Unique(plan.EvidenceRefs) {
		fmt.Fprintf(&b, "  - %q\n", ref)
	}
	fmt.Fprintln(&b)
	return b.String()
}

func LoadRepairReview(path string) (RepairReview, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return RepairReview{}, err
	}
	var review RepairReview
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&review); err != nil {
		return RepairReview{}, err
	}
	return review, nil
}

func ValidateRepairReview(review RepairReview) error {
	if review.SchemaVersion != RepairReviewSchemaVersion {
		return fmt.Errorf("runtime repair review schema_version must be %s, got %q", RepairReviewSchemaVersion, review.SchemaVersion)
	}
	if strings.TrimSpace(review.ID) == "" || strings.TrimSpace(review.PlanID) == "" {
		return fmt.Errorf("runtime repair review id and plan_id required")
	}
	if review.PlanID != filepath.Base(review.PlanID) {
		return fmt.Errorf("runtime repair review plan_id %q must be bare id", review.PlanID)
	}
	if strings.TrimSpace(review.ServiceID) == "" {
		return fmt.Errorf("runtime repair review service_id required")
	}
	if !repairReviewOneOf(review.Decision, []string{"owner_action_required", "owner_action_done", "defer", "no_action"}) {
		return fmt.Errorf("runtime repair review decision must be owner_action_required|owner_action_done|defer|no_action, got %q", review.Decision)
	}
	if strings.TrimSpace(review.Rationale) == "" {
		return fmt.Errorf("runtime repair review rationale required")
	}
	if review.Confidence < 0 || review.Confidence > 1 {
		return fmt.Errorf("runtime repair review confidence must be between 0 and 1")
	}
	if len(reviewartifact.Unique(review.EvidenceRefs)) == 0 {
		return fmt.Errorf("runtime repair review evidence_refs required")
	}
	return nil
}

func RecordRepairReview(homePath string, review RepairReview, sourcePath string, now time.Time) (RepairReviewReport, error) {
	createdAt := reviewartifact.Timestamp(now)
	report := RepairReviewReport{
		SchemaVersion: RepairReviewReportSchemaVersion,
		ID:            reviewartifact.ID("rrr_", review.PlanID, review.ID, review.Decision, createdAt),
		CreatedAt:     createdAt,
		Status:        "recorded",
		PlanID:        review.PlanID,
		ReviewID:      review.ID,
		ServiceID:     review.ServiceID,
		Owner:         review.Owner,
		Decision:      review.Decision,
		NextAction:    strings.TrimSpace(review.NextAction),
		SourcePath:    strings.TrimSpace(sourcePath),
		EvidenceRefs:  reviewartifact.Unique(review.EvidenceRefs),
		Warnings: []string{
			"does not mutate runtime owner files",
			"does not regenerate runtime contracts",
			"does not execute runtime probes",
			"does not promote memory claims",
		},
	}
	if err := ValidateRepairReview(review); err != nil {
		report.Status = "blocked"
		return report, err
	}
	plan, err := loadRepairPlanForReview(sourcePath, review.PlanID)
	if err != nil {
		report.Status = "blocked"
		return report, err
	}
	if plan.ID != review.PlanID {
		report.Status = "blocked"
		return report, fmt.Errorf("runtime repair review plan_id %q does not match plan %q", review.PlanID, plan.ID)
	}
	if review.ServiceID != plan.ServiceID {
		report.Status = "blocked"
		return report, fmt.Errorf("runtime repair review service_id %q does not match plan service_id %q", review.ServiceID, plan.ServiceID)
	}
	if strings.TrimSpace(review.Owner) != "" && strings.TrimSpace(plan.Owner) != "" && review.Owner != plan.Owner {
		report.Status = "blocked"
		return report, fmt.Errorf("runtime repair review owner %q does not match plan owner %q", review.Owner, plan.Owner)
	}
	if review.ReviewedAt == "" {
		review.ReviewedAt = createdAt
	}
	review.SourceRef = "runtime-repair-review:" + filepath.Base(sourcePath)
	report.ReviewPath = filepath.Join(filepath.Dir(sourcePath), repairReviewReportName(review.PlanID, review.ID))
	if _, err := reviewartifact.WriteJSON(homePath, RuntimeRepairReviewArea, filepath.Base(report.ReviewPath), report, "  "); err != nil {
		return RepairReviewReport{}, err
	}
	return report, nil
}

// repairReviewReportName is the single owner of the recorded-disposition file
// name. Nothing pins a review id apart from its plan id, so a review YAML that
// reuses the plan id joined the two into a doubled `<id>.<id>.reviewed.json`
// stem that no sidecar reader stats: the core item would keep its non-terminal
// producer status forever and its review area could never drain. De-duping puts
// the disposition on the sidecar stem its own core item owns, while the joined
// form survives whenever the two ids genuinely differ so distinct dispositions
// of one plan never collide.
//
// Recorded dispositions are immutable evidence, so no reader changes with this:
// the queue's reviewed-path probe already matches any `<id>.*.reviewed.json`,
// and the archive sweep already selects the doubled stem as terminal.
func repairReviewReportName(planID, reviewID string) string {
	if planID == reviewID {
		return planID + ".reviewed.json"
	}
	return planID + "." + reviewID + ".reviewed.json"
}

func LoadRepairReviewReport(path string) (RepairReviewReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return RepairReviewReport{}, err
	}
	var report RepairReviewReport
	if err := json.Unmarshal(data, &report); err != nil {
		return RepairReviewReport{}, err
	}
	return report, nil
}

func AnnotateRepairQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	for i := range report.Items {
		if report.Items[i].Area != RuntimeRepairReviewArea {
			continue
		}
		annotateRepairItem(&report.Items[i])
	}
}

func annotateRepairItem(item *reviewartifact.QueueItem) {
	if item == nil {
		return
	}
	if item.JSONPath == "" {
		item.Readiness = "needs_review"
		item.BlockedReason = "runtime repair plan path missing"
		return
	}
	if reviewedPath := runtimeRepairReviewedPath(*item); reviewedPath != "" {
		item.Status = "recorded"
		item.Readiness = "review_recorded"
		item.NextAction = "runtime repair review recorded; rerun runtime doctor after owner repair or deferment"
		if report, err := LoadRepairReviewReport(reviewedPath); err == nil {
			item.NextAction = reviewartifact.FirstNonEmpty(report.NextAction, item.NextAction)
		}
		return
	}
	reviewPath := runtimeRepairReviewYAMLPath(*item)
	if reviewPath == "" {
		item.Readiness = "needs_review"
		item.BlockedReason = "runtime repair review YAML draft missing"
		item.NextAction = "regenerate runtime repair-plan to create review YAML draft"
		return
	}
	review, err := LoadRepairReview(reviewPath)
	if err != nil {
		item.Readiness = "needs_review"
		item.BlockedReason = "runtime repair review YAML unreadable: " + runtimeRepairQueueError(err)
		item.NextAction = "fix runtime repair review YAML before recording"
		return
	}
	if err := ValidateRepairReview(review); err != nil {
		item.Readiness = "needs_review"
		item.BlockedReason = runtimeRepairQueueError(err)
		item.NextAction = "fill valid runtime repair review YAML before recording"
		return
	}
	item.Readiness = "ready_to_record"
	item.NextAction = "record reviewed runtime repair disposition"
	item.Actions = append(item.Actions, reviewartifact.Action{
		ID:             "runtime-repair-review-record",
		Label:          "Record runtime repair review",
		Command:        "compound runtime repair-review " + reviewPath,
		Argv:           []string{"compound", "runtime", "repair-review", reviewPath},
		RequiresReview: true,
	})
}

func loadRepairPlanForReview(reviewPath, planID string) (RepairPlan, error) {
	if strings.TrimSpace(reviewPath) == "" {
		return RepairPlan{}, fmt.Errorf("runtime repair review source path required")
	}
	if strings.TrimSpace(planID) == "" || planID != filepath.Base(planID) {
		return RepairPlan{}, fmt.Errorf("runtime repair review plan_id %q must be bare id", planID)
	}
	planPath := filepath.Join(filepath.Dir(reviewPath), planID+".json")
	data, err := os.ReadFile(planPath)
	if err != nil {
		return RepairPlan{}, err
	}
	var plan RepairPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return RepairPlan{}, err
	}
	return plan, nil
}

func runtimeRepairReviewedPath(item reviewartifact.QueueItem) string {
	for _, path := range item.ExtraPaths {
		name := filepath.Base(path)
		if strings.HasPrefix(name, item.ID+".") && strings.HasSuffix(name, ".reviewed.json") {
			return path
		}
	}
	return ""
}

func runtimeRepairReviewYAMLPath(item reviewartifact.QueueItem) string {
	want := item.ID + ".review.yaml"
	for _, path := range item.ExtraPaths {
		if filepath.Base(path) == want {
			return path
		}
	}
	return ""
}

func runtimeRepairQueueError(err error) string {
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return "unknown runtime repair review error"
	}
	return msg
}

func repairReviewOneOf(value string, allowed []string) bool {
	for _, v := range allowed {
		if value == v {
			return true
		}
	}
	return false
}
