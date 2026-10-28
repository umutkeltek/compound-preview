package authority

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
	JobHealthReviewSchemaVersion       = "compound.job_health_review.v0.1"
	JobHealthReviewReportSchemaVersion = "compound.job_health_review_report.v0.1"
)

type JobHealthReview struct {
	SchemaVersion string   `json:"schema_version" yaml:"schema_version"`
	ID            string   `json:"id" yaml:"id"`
	PlanID        string   `json:"plan_id" yaml:"plan_id"`
	JobID         string   `json:"job_id,omitempty" yaml:"job_id,omitempty"`
	JobName       string   `json:"job_name,omitempty" yaml:"job_name,omitempty"`
	Decision      string   `json:"decision" yaml:"decision"`
	Rationale     string   `json:"rationale" yaml:"rationale"`
	Confidence    float64  `json:"confidence" yaml:"confidence"`
	NextAction    string   `json:"next_action,omitempty" yaml:"next_action,omitempty"`
	EvidenceRefs  []string `json:"evidence_refs" yaml:"evidence_refs"`
	ReviewedAt    string   `json:"reviewed_at,omitempty" yaml:"reviewed_at,omitempty"`
	SourceRef     string   `json:"source_ref,omitempty" yaml:"source_ref,omitempty"`
}

type JobHealthReviewReport struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	CreatedAt     string   `json:"created_at"`
	Status        string   `json:"status"`
	PlanID        string   `json:"plan_id"`
	ReviewID      string   `json:"review_id"`
	Decision      string   `json:"decision"`
	JobID         string   `json:"job_id,omitempty"`
	JobName       string   `json:"job_name,omitempty"`
	NextAction    string   `json:"next_action,omitempty"`
	SourcePath    string   `json:"source_path,omitempty"`
	ReviewPath    string   `json:"review_path,omitempty"`
	EvidenceRefs  []string `json:"evidence_refs,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

func RenderJobHealthReviewYAMLDraft(plan JobHealthPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version: %s\n", JobHealthReviewSchemaVersion)
	fmt.Fprintf(&b, "id: %s\n", plan.ReviewDraft.ReviewID)
	fmt.Fprintf(&b, "plan_id: %s\n", plan.ID)
	if plan.JobID != "" {
		fmt.Fprintf(&b, "job_id: %s\n", plan.JobID)
	}
	if plan.JobName != "" {
		fmt.Fprintf(&b, "job_name: %q\n", plan.JobName)
	}
	fmt.Fprintln(&b, "decision: TODO_decision_repair_required_parked_disabled_no_action_owner_action_done")
	fmt.Fprintln(&b, "rationale: \"\"")
	fmt.Fprintln(&b, "confidence: -1")
	fmt.Fprintln(&b, "next_action: \"TODO: reviewed job-health next action\"")
	fmt.Fprintln(&b, "evidence_refs:")
	for _, ref := range plan.EvidenceRefs {
		fmt.Fprintf(&b, "  - %q\n", ref)
	}
	return b.String()
}

// jobHealthReviewPlaceholderDecision is the literal TODO sentinel
// RenderJobHealthReviewYAMLDraft writes for an unfilled review.
// FillJobHealthReviewDraft treats an empty decision the same way
// (belt-and-suspenders against a hand-edited draft).
const jobHealthReviewPlaceholderDecision = "TODO_decision_repair_required_parked_disabled_no_action_owner_action_done"

// jobHealthReviewNextActionDefaults mirrors the packet's settled per-decision
// next-action copy (2026-07-11): the plain default an operator gets when they
// pick a decision but do not type their own next action.
var jobHealthReviewNextActionDefaults = map[string]string{
	"repair_required":   "file repair work for this job",
	"parked":            "revisit when the job matters again",
	"disabled":          "confirm the job is intentionally off",
	"no_action":         "none",
	"owner_action_done": "verify the owner's fix on next run",
}

// JobHealthReviewFillInput carries the CLI's optional `--decision` /
// `--rationale` / `--confidence` / `--next-action` flags into
// FillJobHealthReviewDraft. Confidence is a pointer so "flag absent" (default
// 1 — an operator record is a full-confidence decision) is distinguishable
// from an explicit 0.
type JobHealthReviewFillInput struct {
	Decision   string
	Rationale  string
	Confidence *float64
	NextAction string
}

// FillJobHealthReviewDraft turns an unfilled job-health-review draft into a
// filled one IN PLACE on disk: the YAML file remains the durable record
// (fill semantics, settled design #2). It only fills a draft whose on-disk
// decision is still the TODO placeholder (or empty) — a draft that already
// carries a valid, DIFFERENT decision is refused rather than silently
// overwritten. The caller still runs the existing Load+Validate+Record path
// on the (possibly just-filled) file afterward; this function does not
// validate or record on its own.
func FillJobHealthReviewDraft(path string, input JobHealthReviewFillInput, now time.Time) error {
	review, err := LoadJobHealthReview(path)
	if err != nil {
		return err
	}
	existing := strings.TrimSpace(review.Decision)
	if existing != "" && existing != jobHealthReviewPlaceholderDecision {
		if existing != input.Decision {
			return fmt.Errorf("job-health review already recorded as %s; edit the YAML explicitly to change a recorded decision", existing)
		}
		return nil // already filled with this same decision; leave the record untouched
	}
	review.Decision = input.Decision
	review.Rationale = input.Rationale
	if input.Confidence != nil {
		review.Confidence = *input.Confidence
	} else {
		review.Confidence = 1
	}
	if strings.TrimSpace(input.NextAction) != "" {
		review.NextAction = input.NextAction
	} else {
		review.NextAction = jobHealthReviewNextActionDefaults[input.Decision]
	}
	review.ReviewedAt = reviewartifact.Timestamp(now)
	out, err := yaml.Marshal(review)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

func LoadJobHealthReview(path string) (JobHealthReview, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return JobHealthReview{}, err
	}
	var review JobHealthReview
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&review); err != nil {
		return JobHealthReview{}, err
	}
	return review, nil
}

func ValidateJobHealthReview(review JobHealthReview) error {
	if review.SchemaVersion != JobHealthReviewSchemaVersion {
		return fmt.Errorf("schema_version must be %s, got %q", JobHealthReviewSchemaVersion, review.SchemaVersion)
	}
	if strings.TrimSpace(review.ID) == "" || strings.TrimSpace(review.PlanID) == "" {
		return fmt.Errorf("job-health review id and plan_id required")
	}
	if review.PlanID != filepath.Base(review.PlanID) {
		return fmt.Errorf("job-health review plan_id %q must be bare id", review.PlanID)
	}
	if strings.TrimSpace(review.JobID) == "" && strings.TrimSpace(review.JobName) == "" {
		return fmt.Errorf("job-health review job_id or job_name required")
	}
	if !oneOf(review.Decision, []string{"repair_required", "parked", "disabled", "no_action", "owner_action_done"}) {
		return fmt.Errorf("job-health review decision must be repair_required|parked|disabled|no_action|owner_action_done, got %q", review.Decision)
	}
	if strings.TrimSpace(review.Rationale) == "" {
		return fmt.Errorf("job-health review rationale required")
	}
	if review.Confidence < 0 || review.Confidence > 1 {
		return fmt.Errorf("job-health review confidence must be between 0 and 1")
	}
	if len(reviewartifact.Unique(review.EvidenceRefs)) == 0 {
		return fmt.Errorf("job-health review evidence_refs required")
	}
	return nil
}

func RecordJobHealthReview(homePath string, review JobHealthReview, sourcePath string, now time.Time) (JobHealthReviewReport, error) {
	createdAt := reviewartifact.Timestamp(now)
	report := JobHealthReviewReport{
		SchemaVersion: JobHealthReviewReportSchemaVersion,
		ID:            reviewartifact.ID("jhr_", review.PlanID, review.ID, review.Decision, createdAt),
		CreatedAt:     createdAt,
		Status:        "recorded",
		PlanID:        review.PlanID,
		ReviewID:      review.ID,
		Decision:      review.Decision,
		JobID:         review.JobID,
		JobName:       review.JobName,
		NextAction:    strings.TrimSpace(review.NextAction),
		SourcePath:    strings.TrimSpace(sourcePath),
		EvidenceRefs:  reviewartifact.Unique(review.EvidenceRefs),
		Warnings: []string{
			"does not execute job",
			"does not mutate scheduler files",
			"does not change job enabled state",
			"does not promote memory claims",
		},
	}
	if err := ValidateJobHealthReview(review); err != nil {
		report.Status = "blocked"
		return report, err
	}
	plan, err := loadJobHealthPlanForReview(sourcePath, review.PlanID)
	if err != nil {
		report.Status = "blocked"
		return report, err
	}
	if plan.ID != review.PlanID {
		report.Status = "blocked"
		return report, fmt.Errorf("job-health review plan_id %q does not match plan %q", review.PlanID, plan.ID)
	}
	if strings.TrimSpace(review.JobID) != "" && strings.TrimSpace(plan.JobID) != "" && review.JobID != plan.JobID {
		report.Status = "blocked"
		return report, fmt.Errorf("job-health review job_id %q does not match plan job_id %q", review.JobID, plan.JobID)
	}
	if strings.TrimSpace(review.JobName) != "" && strings.TrimSpace(plan.JobName) != "" && review.JobName != plan.JobName {
		report.Status = "blocked"
		return report, fmt.Errorf("job-health review job_name %q does not match plan job_name %q", review.JobName, plan.JobName)
	}
	if review.ReviewedAt == "" {
		review.ReviewedAt = createdAt
	}
	review.SourceRef = "job-health-review:" + filepath.Base(sourcePath)
	report.JobID = reviewartifact.FirstNonEmpty(report.JobID, plan.JobID)
	report.JobName = reviewartifact.FirstNonEmpty(report.JobName, plan.JobName)
	report.ReviewPath = filepath.Join(filepath.Dir(sourcePath), review.PlanID+"."+review.ID+".reviewed.json")
	if _, err := reviewartifact.WriteJSON(homePath, jobHealthReviewArea, filepath.Base(report.ReviewPath), report, " "); err != nil {
		return JobHealthReviewReport{}, err
	}
	return report, nil
}

func LoadJobHealthReviewReport(path string) (JobHealthReviewReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return JobHealthReviewReport{}, err
	}
	var report JobHealthReviewReport
	if err := json.Unmarshal(data, &report); err != nil {
		return JobHealthReviewReport{}, err
	}
	return report, nil
}

func loadJobHealthPlanForReview(reviewPath, planID string) (JobHealthPlan, error) {
	if strings.TrimSpace(reviewPath) == "" {
		return JobHealthPlan{}, fmt.Errorf("job-health review source path required")
	}
	if strings.TrimSpace(planID) == "" || planID != filepath.Base(planID) {
		return JobHealthPlan{}, fmt.Errorf("job-health review plan_id %q must be bare id", planID)
	}
	planPath := filepath.Join(filepath.Dir(reviewPath), planID+".json")
	data, err := os.ReadFile(planPath)
	if err != nil {
		return JobHealthPlan{}, err
	}
	var plan JobHealthPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return JobHealthPlan{}, err
	}
	return plan, nil
}

func AnnotateJobHealthQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	for i := range report.Items {
		item := &report.Items[i]
		if item.Area != jobHealthReviewArea {
			continue
		}
		annotateJobHealthItem(item)
	}
}

func annotateJobHealthItem(item *reviewartifact.QueueItem) {
	if item.JSONPath == "" {
		item.Readiness = "blocked"
		item.NextAction = "job-health plan path missing"
		item.Band = reviewartifact.BandNeedsYou
		return
	}
	plan, planErr := loadJobHealthPlanFile(item.JSONPath)
	if reviewedPath := jobHealthReviewedPath(*item); reviewedPath != "" {
		item.Status = "recorded"
		item.Readiness = "review_recorded"
		item.NextAction = "job-health review recorded; re-import job roster to prove closure or continuing debt"
		if planErr == nil {
			annotateJobHealthOperatorCard(item, plan, reviewartifact.BandFiled, "The scheduled job "+reviewartifact.ShortLabel(plan.JobName, 40)+" review is already recorded.")
		}
		if report, err := LoadJobHealthReviewReport(reviewedPath); err == nil {
			item.NextAction = reviewartifact.FirstNonEmpty(report.NextAction, "job-health review recorded: "+report.Decision)
		}
		return
	}
	if jobHealthPlanSupersededByCurrentRoster(*item) {
		item.Status = "resolved"
		item.Readiness = "no_action"
		item.BlockedReason = ""
		item.NextAction = "job-health artifact superseded by current compound://registries/job-roster; rerun job-health-plan only if the failure returns"
		item.Actions = nil
		if planErr == nil {
			annotateJobHealthOperatorCard(item, plan, reviewartifact.BandFiled, "The scheduled job "+reviewartifact.ShortLabel(plan.JobName, 40)+" failure is already resolved.")
		}
		return
	}
	if planErr == nil {
		annotateJobHealthOperatorCard(item, plan, reviewartifact.BandNeedsYou, "")
	}
	reviewYAML := jobHealthReviewYAMLPath(*item)
	if reviewYAML == "" {
		item.Readiness = "blocked"
		item.BlockedReason = "job-health review YAML draft missing"
		item.NextAction = "regenerate job-health plan to create review YAML draft"
		item.Band = reviewartifact.BandNeedsYou
		return
	}
	argv := []string{"compound", "authority", "job-health-review", reviewYAML}
	item.Actions = []reviewartifact.Action{{
		ID:             "record_job_health_review",
		Label:          "Record job-health review",
		Command:        jobHealthShellJoin(argv),
		Argv:           argv,
		RequiresReview: true,
	}}
	review, err := LoadJobHealthReview(reviewYAML)
	if err != nil {
		item.Readiness = "blocked"
		item.BlockedReason = "job-health review YAML unreadable: " + jobHealthQueueError(err)
		item.NextAction = "fix job-health review YAML before recording"
		return
	}
	if err := ValidateJobHealthReview(review); err != nil {
		item.Readiness = "needs_review"
		item.BlockedReason = jobHealthQueueError(err)
		item.NextAction = "fill valid job-health review YAML before recording"
		return
	}
	item.Readiness = "ready_to_record"
	item.NextAction = "record reviewed job-health disposition"
}

func annotateJobHealthOperatorCard(item *reviewartifact.QueueItem, plan JobHealthPlan, band, summaryOverride string) {
	card, err := OperatorCardForJobHealthPlan(plan)
	if err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "job-health operator card invalid: "+err.Error())
		return
	}
	if strings.TrimSpace(summaryOverride) != "" {
		card.Summary = reviewartifact.ShortLabel(summaryOverride, 159)
		if !strings.HasSuffix(card.Summary, ".") {
			card.Summary += "."
		}
		card.Unlock = ""
	}
	if err := reviewartifact.SetOperatorCard(item, card, reviewartifact.SummarySourceAnnotator, band); err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "job-health operator card invalid: "+err.Error())
	}
}

func jobHealthPlanSupersededByCurrentRoster(item reviewartifact.QueueItem) bool {
	if item.JSONPath == "" || item.SourceKind != "job_roster_error" {
		return false
	}
	plan, err := loadJobHealthPlanFile(item.JSONPath)
	if err != nil {
		return false
	}
	if plan.SourceKind != "job_roster_error" || (strings.TrimSpace(plan.JobID) == "" && strings.TrimSpace(plan.JobName) == "") {
		return false
	}
	homePath := filepath.Clean(filepath.Join(filepath.Dir(item.JSONPath), "..", ".."))
	roster, err := LoadJobRosterAuthority(homePath)
	if err != nil {
		return false
	}
	return !jobHealthRosterHasCurrentError(roster, plan)
}

func loadJobHealthPlanFile(path string) (JobHealthPlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return JobHealthPlan{}, err
	}
	var plan JobHealthPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return JobHealthPlan{}, err
	}
	return plan, nil
}

func jobHealthRosterHasCurrentError(roster JobRosterAuthority, plan JobHealthPlan) bool {
	for _, job := range jobHealthErrorJobs(roster) {
		if strings.TrimSpace(plan.JobID) != "" && plan.JobID == job.JobID {
			return true
		}
		if strings.TrimSpace(plan.JobName) != "" && plan.JobName == job.Name {
			return true
		}
	}
	return false
}

func jobHealthReviewYAMLPath(item reviewartifact.QueueItem) string {
	for _, path := range item.ExtraPaths {
		if strings.HasSuffix(path, ".review.yaml") {
			return path
		}
	}
	return ""
}

func jobHealthReviewedPath(item reviewartifact.QueueItem) string {
	for _, path := range item.ExtraPaths {
		if strings.HasSuffix(path, ".reviewed.json") {
			return path
		}
	}
	return ""
}

func jobHealthQueueError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if len(msg) > 240 {
		msg = msg[:237] + "..."
	}
	return msg
}

func jobHealthShellJoin(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, arg := range argv {
		parts = append(parts, jobHealthShellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func jobHealthShellQuote(arg string) string {
	if arg == "" {
		return "''"
	}
	if strings.IndexFunc(arg, func(r rune) bool {
		return !(r == '-' || r == '_' || r == '/' || r == '.' || r == ':' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'))
	}) == -1 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
}
