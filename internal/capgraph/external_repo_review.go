package capgraph

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
	ExternalRepoIntakeReviewSchemaVersion       = "compound.capability.external_repo_intake_review.v0.1"
	ExternalRepoIntakeReviewReportSchemaVersion = "compound.capability.external_repo_intake_review_report.v0.1"
)

type ExternalRepoIntakeReview struct {
	SchemaVersion string   `json:"schema_version" yaml:"schema_version"`
	ID            string   `json:"id" yaml:"id"`
	PlanID        string   `json:"plan_id" yaml:"plan_id"`
	Repo          string   `json:"repo" yaml:"repo"`
	Decision      string   `json:"decision" yaml:"decision"`
	Owner         string   `json:"owner,omitempty" yaml:"owner,omitempty"`
	Rationale     string   `json:"rationale" yaml:"rationale"`
	Confidence    float64  `json:"confidence" yaml:"confidence"`
	NextAction    string   `json:"next_action,omitempty" yaml:"next_action,omitempty"`
	EvidenceRefs  []string `json:"evidence_refs" yaml:"evidence_refs"`
	Boundaries    []string `json:"boundaries,omitempty" yaml:"boundaries,omitempty"`
	ReviewedAt    string   `json:"reviewed_at,omitempty" yaml:"reviewed_at,omitempty"`
	ReviewedBy    string   `json:"reviewed_by,omitempty" yaml:"reviewed_by,omitempty"`
	SourceRef     string   `json:"source_ref,omitempty" yaml:"source_ref,omitempty"`
}

type ExternalRepoIntakeReviewReport struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	CreatedAt     string   `json:"created_at"`
	Status        string   `json:"status"`
	PlanID        string   `json:"plan_id"`
	ReviewID      string   `json:"review_id"`
	Repo          string   `json:"repo"`
	Decision      string   `json:"decision"`
	Owner         string   `json:"owner,omitempty"`
	Rationale     string   `json:"rationale,omitempty"`
	Confidence    float64  `json:"confidence,omitempty"`
	NextAction    string   `json:"next_action,omitempty"`
	SourcePath    string   `json:"source_path"`
	SourceHash    string   `json:"source_hash,omitempty"`
	SourceRef     string   `json:"source_ref,omitempty"`
	ReviewPath    string   `json:"review_path,omitempty"`
	EvidenceRefs  []string `json:"evidence_refs,omitempty"`
	Boundaries    []string `json:"boundaries,omitempty"`
	ReviewedAt    string   `json:"reviewed_at,omitempty"`
	ReviewedBy    string   `json:"reviewed_by,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

func LoadExternalRepoIntakeReview(path string) (ExternalRepoIntakeReview, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ExternalRepoIntakeReview{}, fmt.Errorf("read external repo review %s: %w", path, err)
	}
	var review ExternalRepoIntakeReview
	if err := yaml.Unmarshal(data, &review); err != nil {
		return ExternalRepoIntakeReview{}, fmt.Errorf("parse external repo review %s: %w", path, err)
	}
	return review, nil
}

func ValidateExternalRepoIntakeReview(review ExternalRepoIntakeReview) error {
	if review.SchemaVersion != ExternalRepoIntakeReviewSchemaVersion {
		return fmt.Errorf("schema_version must be %s, got %q", ExternalRepoIntakeReviewSchemaVersion, review.SchemaVersion)
	}
	if strings.TrimSpace(review.ID) == "" || strings.TrimSpace(review.PlanID) == "" {
		return fmt.Errorf("external repo review id and plan_id required")
	}
	if review.ID != filepath.Base(review.ID) || review.PlanID != filepath.Base(review.PlanID) {
		return fmt.Errorf("external repo review id and plan_id must be bare ids")
	}
	if strings.TrimSpace(review.Repo) == "" {
		return fmt.Errorf("external repo review repo required")
	}
	if !externalRepoDecisionAllowed(review.Decision, []string{"keep_needs_review", "merge_existing", "reference_only", "reject"}) {
		return fmt.Errorf("external repo review decision must be keep_needs_review|merge_existing|reference_only|reject, got %q", review.Decision)
	}
	if strings.TrimSpace(review.Rationale) == "" {
		return fmt.Errorf("external repo review rationale required")
	}
	if review.Confidence < 0 || review.Confidence > 1 {
		return fmt.Errorf("external repo review confidence must be between 0 and 1")
	}
	if len(reviewartifact.Unique(review.EvidenceRefs)) == 0 {
		return fmt.Errorf("external repo review evidence_refs required")
	}
	return nil
}

func externalRepoDecisionAllowed(value string, allowed []string) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}

func RecordExternalRepoIntakeReview(homePath string, review ExternalRepoIntakeReview, sourcePath string, now time.Time) (ExternalRepoIntakeReviewReport, error) {
	createdAt := reviewartifact.Timestamp(now)
	report := ExternalRepoIntakeReviewReport{
		SchemaVersion: ExternalRepoIntakeReviewReportSchemaVersion,
		ID:            reviewartifact.ID("errr_", review.PlanID, review.ID, review.Decision, createdAt),
		CreatedAt:     createdAt,
		Status:        "recorded",
		PlanID:        review.PlanID,
		ReviewID:      review.ID,
		Repo:          review.Repo,
		Decision:      review.Decision,
		Owner:         strings.TrimSpace(review.Owner),
		NextAction:    strings.TrimSpace(review.NextAction),
		SourcePath:    strings.TrimSpace(sourcePath),
		EvidenceRefs:  reviewartifact.Unique(review.EvidenceRefs),
		Warnings: []string{
			"does not mutate CapabilityGraph",
			"does not install, clone, fetch, serve, or execute external repos",
			"does not activate hooks, MCPs, plugins, or project registry entries",
			"does not promote memory claims",
		},
	}
	if err := ValidateExternalRepoIntakeReview(review); err != nil {
		report.Status = "blocked"
		return report, err
	}
	plan, err := loadExternalRepoIntakeReviewPlanForReview(sourcePath, review.PlanID)
	if err != nil {
		report.Status = "blocked"
		return report, err
	}
	if review.Repo != plan.Repo {
		report.Status = "blocked"
		return report, fmt.Errorf("external repo review repo %q does not match plan repo %q", review.Repo, plan.Repo)
	}
	if review.ReviewedAt == "" {
		review.ReviewedAt = createdAt
	}
	review.SourceRef = "external-repo-intake-review:" + filepath.Base(sourcePath)
	sourceFound, sourceHash, err := reviewartifact.HashFileTarget(sourcePath)
	if err != nil {
		report.Status = "blocked"
		return report, err
	}
	if !sourceFound {
		report.Status = "blocked"
		return report, fmt.Errorf("external repo review source file %q required", sourcePath)
	}
	report.Rationale = strings.TrimSpace(review.Rationale)
	report.Confidence = review.Confidence
	report.SourceHash = sourceHash
	report.SourceRef = review.SourceRef
	report.Boundaries = reviewartifact.Unique(review.Boundaries)
	report.ReviewedAt = review.ReviewedAt
	report.ReviewedBy = strings.TrimSpace(review.ReviewedBy)
	report.ReviewPath = filepath.Join(filepath.Dir(sourcePath), review.PlanID+"."+review.ID+".reviewed.json")
	if _, err := reviewartifact.WriteJSON(homePath, ExternalRepoIntakeReviewArea, filepath.Base(report.ReviewPath), report, " "); err != nil {
		return ExternalRepoIntakeReviewReport{}, err
	}
	return report, nil
}

func LoadExternalRepoIntakeReviewReport(path string) (ExternalRepoIntakeReviewReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ExternalRepoIntakeReviewReport{}, err
	}
	var report ExternalRepoIntakeReviewReport
	if err := json.Unmarshal(data, &report); err != nil {
		return ExternalRepoIntakeReviewReport{}, err
	}
	return report, nil
}

func AnnotateExternalRepoIntakeQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	for i := range report.Items {
		item := &report.Items[i]
		if item.Area != ExternalRepoIntakeReviewArea {
			continue
		}
		annotateExternalRepoIntakeQueueItem(item)
	}
}

func annotateExternalRepoIntakeQueueItem(item *reviewartifact.QueueItem) {
	if item.JSONPath == "" {
		item.Readiness = "blocked"
		item.NextAction = "external repo review plan path missing"
		return
	}
	plan, planErr := loadExternalRepoIntakePlanFile(item.JSONPath)
	if reviewedPath := externalRepoIntakeReviewedPath(*item); reviewedPath != "" {
		item.Status = "recorded"
		item.Readiness = "review_recorded"
		item.Actions = nil
		item.BlockedReason = ""
		item.NextAction = "external repo review recorded; no graph mutation performed"
		if report, err := LoadExternalRepoIntakeReviewReport(reviewedPath); err == nil {
			item.NextAction = reviewartifact.FirstNonEmpty(report.NextAction, "external repo review recorded: "+report.Decision)
		}
		if planErr == nil {
			// A recorded hold/keep is a reviewed, filed disposition, not an
			// open decision. It must not keep rendering in needs_you.
			annotateExternalRepoIntakeOperatorCard(item, plan, reviewartifact.BandFiled)
		}
		return
	}
	if planErr == nil {
		annotateExternalRepoIntakeOperatorCard(item, plan, reviewartifact.BandNeedsYou)
	}
	reviewYAML := externalRepoIntakeReviewYAMLPath(*item)
	if reviewYAML == "" {
		item.Readiness = "blocked"
		item.BlockedReason = "external repo review YAML draft missing"
		item.NextAction = "regenerate external-intake-review-plan to create review YAML draft"
		return
	}
	argv := []string{"compound", "capability", "external-intake-review", reviewYAML}
	item.Actions = []reviewartifact.Action{{
		ID:             "record_external_repo_intake_review",
		Label:          "Record external repo review",
		Command:        externalRepoIntakeShellJoin(argv),
		Argv:           argv,
		RequiresReview: true,
	}}
	if plan, err := loadExternalRepoIntakeReviewPlanForReview(reviewYAML, item.ID); err == nil {
		item.DecisionCard = externalRepoIntakeDecisionCard(*item, plan, reviewYAML)
		item.EvidenceRefs = reviewartifact.UniqueSorted(append(item.EvidenceRefs, plan.EvidenceRefs...))
		item.Boundaries = reviewartifact.Unique(append(item.Boundaries, plan.Boundaries...))
	}
	review, err := LoadExternalRepoIntakeReview(reviewYAML)
	if err != nil {
		item.Readiness = "blocked"
		item.BlockedReason = "external repo review YAML unreadable: " + externalRepoIntakeQueueError(err)
		item.NextAction = "fix external repo review YAML before recording"
		return
	}
	if err := ValidateExternalRepoIntakeReview(review); err != nil {
		item.Readiness = "needs_review"
		item.BlockedReason = externalRepoIntakeQueueError(err)
		item.NextAction = "fill valid external repo review YAML before recording"
		return
	}
	item.Readiness = "ready_to_record"
	item.BlockedReason = ""
	item.NextAction = "record reviewed external repo disposition"
}

func externalRepoIntakeDecisionCard(item reviewartifact.QueueItem, plan ExternalRepoIntakeReviewPlanItem, reviewYAML string) *reviewartifact.DecisionCard {
	claim := fmt.Sprintf("Review external repo %s before any capability evaluation, graph import, install, hook, MCP, plugin, or memory promotion.", reviewartifact.FirstNonEmpty(plan.Repo, item.TargetPath))
	card := &reviewartifact.DecisionCard{
		Claim:               claim,
		PanelLean:           reviewartifact.FirstNonEmpty(plan.Decision, item.GraphDecisionHint, "keep_needs_review"),
		OptionalRationale:   false,
		CanonicalActionHint: externalRepoIntakeReviewCommand(reviewYAML),
		Boundary:            reviewartifact.Unique(append([]string{"read-only projection", "review YAML must be edited before command", "no clone/install/serve/auth/graph mutation/memory promotion"}, plan.Boundaries...)),
		Meta: map[string]string{
			"repo":                  reviewartifact.FirstNonEmpty(plan.Repo, item.TargetPath),
			"owner":                 plan.Owner,
			"review_yaml":           reviewYAML,
			"evaluation_step_count": fmt.Sprintf("%d", len(plan.EvaluationPlan)),
		},
	}
	card.Why = []reviewartifact.DecisionCardReason{
		{
			Label:    "owner",
			Question: "Who would consume this if useful?",
			Detail:   reviewartifact.FirstNonEmpty(plan.Owner, "operator must choose existing owner or reject/reference only"),
		},
		{
			Label:    "next proof",
			Question: "What evidence is required before adoption?",
			Detail:   reviewartifact.FirstNonEmpty(plan.NextAction, "record owner review, then run focused capability evaluation"),
		},
		{
			Label:    "boundary",
			Question: "What must not happen during intake?",
			Severity: "warning",
			Detail:   reviewartifact.FirstNonEmpty(plan.NotToDo, strings.Join(plan.Boundaries, "; "), "review-only; no execution or graph mutation"),
		},
	}
	for _, step := range plan.EvaluationPlan {
		detail := fmt.Sprintf("trigger=%s; evidence=%s; silence=%s; proof=%s; retirement=%s", step.Trigger, step.EvidenceNeeded, step.Silence, step.Proof, step.Retirement)
		card.Why = append(card.Why, reviewartifact.DecisionCardReason{
			Label:    "evaluation lens: " + step.Lens,
			Question: step.Question,
			Detail:   detail,
		})
	}
	card.Choices = externalRepoIntakeDecisionChoices(reviewYAML)
	return card
}

func externalRepoIntakeDecisionChoices(reviewYAML string) []reviewartifact.DecisionCardChoice {
	choices := []struct {
		id     string
		label  string
		effect string
	}{
		{"keep_needs_review", "Keep Review", "Record owner fit; repo may enter focused capability evaluation, still no graph import."},
		{"reference_only", "Reference Only", "Keep as source evidence; no capability evaluation or graph adoption."},
		{"merge_existing", "Merge Existing", "Fold useful deltas into an existing owner; no parallel capability."},
		{"reject", "Reject", "Drop as adoption candidate while preserving the review receipt."},
	}
	cmd := externalRepoIntakeReviewCommand(reviewYAML)
	out := make([]reviewartifact.DecisionCardChoice, 0, len(choices))
	for _, choice := range choices {
		out = append(out, reviewartifact.DecisionCardChoice{
			ID:             choice.id,
			Label:          choice.label,
			Effect:         choice.effect,
			Command:        cmd,
			Argv:           []string{"/bin/sh", "-lc", cmd},
			RequiresReview: true,
		})
	}
	return out
}

func externalRepoIntakeReviewCommand(reviewYAML string) string {
	if strings.TrimSpace(reviewYAML) == "" {
		return "compound capability external-intake-review <review.yaml>"
	}
	quoted := shellQuote(reviewYAML)
	return "$EDITOR " + quoted + " && compound capability external-intake-review " + quoted
}

func annotateExternalRepoIntakeOperatorCard(item *reviewartifact.QueueItem, plan ExternalRepoIntakeReviewPlanItem, band string) {
	card, err := OperatorCardForExternalRepoIntake(plan)
	if err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "external repo intake operator card invalid: "+err.Error())
		return
	}
	if err := reviewartifact.SetOperatorCard(item, card, reviewartifact.SummarySourceAnnotator, band); err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "external repo intake operator card invalid: "+err.Error())
	}
}

func loadExternalRepoIntakePlanFile(path string) (ExternalRepoIntakeReviewPlanItem, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ExternalRepoIntakeReviewPlanItem{}, err
	}
	var plan ExternalRepoIntakeReviewPlanItem
	if err := json.Unmarshal(data, &plan); err != nil {
		return ExternalRepoIntakeReviewPlanItem{}, err
	}
	return plan, nil
}

func init() {
	reviewartifact.RegisterGroupSummary(ExternalRepoIntakeReviewArea, func(group reviewartifact.QueueGroup) reviewartifact.GroupSummaryResult {
		return reviewartifact.GroupSummaryResult{
			Summary: fmt.Sprintf("Repo intake reviewed %d external repos; all kept as read-only evidence. File them?", group.Count),
			Unlock:  "they become citable evidence, or clear out honestly",
		}
	})
}

func loadExternalRepoIntakeReviewPlanForReview(reviewPath, planID string) (ExternalRepoIntakeReviewPlanItem, error) {
	if strings.TrimSpace(reviewPath) == "" {
		return ExternalRepoIntakeReviewPlanItem{}, fmt.Errorf("external repo review source path required")
	}
	if strings.TrimSpace(planID) == "" || planID != filepath.Base(planID) {
		return ExternalRepoIntakeReviewPlanItem{}, fmt.Errorf("external repo review plan_id %q must be bare id", planID)
	}
	planPath := filepath.Join(filepath.Dir(reviewPath), planID+".json")
	data, err := os.ReadFile(planPath)
	if err != nil {
		return ExternalRepoIntakeReviewPlanItem{}, err
	}
	var plan ExternalRepoIntakeReviewPlanItem
	if err := json.Unmarshal(data, &plan); err != nil {
		return ExternalRepoIntakeReviewPlanItem{}, err
	}
	return plan, nil
}

func externalRepoIntakeReviewYAMLPath(item reviewartifact.QueueItem) string {
	for _, path := range item.ExtraPaths {
		if strings.HasSuffix(path, ".review.yaml") {
			return path
		}
	}
	return ""
}

func externalRepoIntakeReviewedPath(item reviewartifact.QueueItem) string {
	for _, path := range item.ExtraPaths {
		if strings.HasSuffix(path, ".reviewed.json") {
			return path
		}
	}
	return ""
}

func externalRepoIntakeShellJoin(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, arg := range argv {
		parts = append(parts, externalRepoIntakeShellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func externalRepoIntakeShellQuote(arg string) string {
	if arg == "" {
		return "''"
	}
	if strings.IndexFunc(arg, func(r rune) bool {
		return !(r == '/' || r == '-' || r == '_' || r == '.' || r == ':' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	}) == -1 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
}

func externalRepoIntakeQueueError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if len(msg) > 240 {
		msg = msg[:237] + "..."
	}
	return msg
}
