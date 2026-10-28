package product

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/reviewartifact"
)

const SourceIssuePlanSchemaVersion = "compound.product_source_issue_plan.v0.1"

// SourceIssuePlan turns an open product source issue into a reviewed closure
// draft. It does not close the issue, log a durable product change, or mutate a
// product surface.
type SourceIssuePlan struct {
	SchemaVersion string               `json:"schema_version"`
	ID            string               `json:"id"`
	CreatedAt     string               `json:"created_at"`
	Status        string               `json:"status"`
	ProductID     string               `json:"product_id"`
	IssueID       string               `json:"issue_id"`
	SourceRef     string               `json:"source_ref"`
	Basis         string               `json:"basis"`
	EvidenceRefs  []string             `json:"evidence_refs"`
	TargetPath    string               `json:"target_path,omitempty"`
	TargetExists  bool                 `json:"target_exists"`
	TargetHash    string               `json:"target_hash,omitempty"`
	ReviewDraft   SourceIssuePlanDraft `json:"review_draft"`
	Warnings      []string             `json:"warnings,omitempty"`
}

type SourceIssuePlanDraft struct {
	ReviewID         string   `json:"review_id"`
	Title            string   `json:"title"`
	ReviewQuestion   string   `json:"review_question"`
	AllowedResults   []string `json:"allowed_results"`
	RequiredEvidence []string `json:"required_evidence"`
	ClosureCommand   string   `json:"closure_command"`
	ReviewNote       string   `json:"review_note"`
}

type SourceIssuePlanPaths struct {
	PlanPath       string
	ReviewPath     string
	ReviewYAMLPath string
}

func BuildSourceIssuePlan(homePath, productID, issueID, targetPath string, now time.Time) (SourceIssuePlan, error) {
	homePath = strings.TrimSpace(homePath)
	productID = strings.TrimSpace(productID)
	issueID = strings.TrimSpace(issueID)
	if homePath == "" {
		return SourceIssuePlan{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if productID == "" || issueID == "" {
		return SourceIssuePlan{}, fmt.Errorf("product_id and issue_id required")
	}
	issues, err := loadProductSourceIssues(homePath, productID)
	if err != nil {
		return SourceIssuePlan{}, err
	}
	var issue ProductSourceIssue
	for _, candidate := range issues {
		if candidate.ID == issueID {
			issue = candidate
			break
		}
	}
	if issue.ID == "" {
		return SourceIssuePlan{}, fmt.Errorf("product source issue %q not found for %s", issueID, productID)
	}
	if issue.Status == "closed" {
		return SourceIssuePlan{}, fmt.Errorf("product source issue %q already closed by review %q", issue.ID, issue.ReviewID)
	}
	targetExists, targetHash, err := hashHypothesisPlanTarget(targetPath)
	if err != nil {
		return SourceIssuePlan{}, err
	}
	evidenceRefs := compact(issue.EvidenceRefs)
	key := normalizeIDPart(productID + " " + issue.SourceRef + " " + issue.ID)
	if key == "" {
		key = normalizeIDPart(issue.ID)
	}
	warnings := []string{
		"does not close the source issue",
		"does not log a durable product change",
		"does not create ProductSignal or ProductHypothesis records",
		"does not mutate product strategy or store assets",
	}
	if len(evidenceRefs) == 0 {
		warnings = append(warnings, "issue has no evidence refs; reviewer must source evidence before closure")
	}
	return SourceIssuePlan{
		SchemaVersion: SourceIssuePlanSchemaVersion,
		ID:            stableID("psip_", productID, issue.ID, issue.SourceRef),
		CreatedAt:     now.UTC().Format(time.RFC3339),
		Status:        "review_required",
		ProductID:     productID,
		IssueID:       issue.ID,
		SourceRef:     issue.SourceRef,
		Basis:         issue.Basis,
		EvidenceRefs:  evidenceRefs,
		TargetPath:    strings.TrimSpace(targetPath),
		TargetExists:  targetExists,
		TargetHash:    targetHash,
		ReviewDraft: SourceIssuePlanDraft{
			ReviewID:       "source_issue_review_" + key + "_" + stableID("", issue.ID),
			Title:          "Review product source issue " + issue.ID,
			ReviewQuestion: "Should this source issue be resolved, waived, or marked false_positive?",
			AllowedResults: []string{"resolved", "waived", "false_positive"},
			RequiredEvidence: []string{
				"current owner/source state for " + issue.SourceRef,
				"decision whether the candidate represents a real durable product change",
				"if real, separate evidence for ProductSignal/ProductHypothesis or durable change logging",
				"rationale with evidence refs for closing or waiving this issue",
			},
			ClosureCommand: "compound product source-issue-review <review.yaml>",
			ReviewNote:     "Fill a source-issue-review YAML only after checking the source owner. Closing this issue must not imply the product change is durable or causal.",
		},
		Warnings: warnings,
	}, nil
}

func RecordSourceIssuePlan(homePath string, plan SourceIssuePlan) (SourceIssuePlanPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return SourceIssuePlanPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if plan.ID == "" {
		return SourceIssuePlanPaths{}, fmt.Errorf("product source issue plan id required")
	}
	dir, err := reviewartifact.Dir(homePath, "product-source-issues")
	if err != nil {
		return SourceIssuePlanPaths{}, err
	}
	paths := SourceIssuePlanPaths{
		PlanPath:       filepath.Join(dir, plan.ID+".json"),
		ReviewPath:     filepath.Join(dir, plan.ID+".md"),
		ReviewYAMLPath: filepath.Join(dir, plan.ID+".review.yaml"),
	}
	if _, err := reviewartifact.Write(homePath, "product-source-issues", plan.ID, plan, " ", RenderSourceIssuePlanReview(plan)); err != nil {
		return paths, err
	}
	if _, err := reviewartifact.WriteRaw(homePath, "product-source-issues", plan.ID+".review.yaml", []byte(RenderSourceIssueReviewYAML(plan))); err != nil {
		return paths, err
	}
	return paths, nil
}

func RenderSourceIssuePlanReview(plan SourceIssuePlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Product Source Issue Review Plan: %s\n\n", plan.IssueID)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- product: %s\n", plan.ProductID)
	fmt.Fprintf(&b, "- issue: %s\n", plan.IssueID)
	fmt.Fprintf(&b, "- source_ref: %s\n", plan.SourceRef)
	if plan.TargetPath != "" {
		fmt.Fprintf(&b, "- target_path: %s\n", plan.TargetPath)
		fmt.Fprintf(&b, "- target_exists: %t\n", plan.TargetExists)
	}
	if plan.TargetHash != "" {
		fmt.Fprintf(&b, "- target_hash: %s\n", plan.TargetHash)
	}
	fmt.Fprintf(&b, "- evidence_refs: %s\n\n", strings.Join(plan.EvidenceRefs, ", "))
	fmt.Fprintln(&b, "This owner-review artifact does not close the issue, log a durable change, create product signals, or mutate product surfaces.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Basis")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "%s\n\n", plan.Basis)
	fmt.Fprintln(&b, "## Review Draft")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- review_id: %s\n", plan.ReviewDraft.ReviewID)
	fmt.Fprintf(&b, "- question: %s\n", plan.ReviewDraft.ReviewQuestion)
	fmt.Fprintf(&b, "- allowed_results: %s\n", strings.Join(plan.ReviewDraft.AllowedResults, ", "))
	fmt.Fprintf(&b, "- closure_command: `%s`\n", plan.ReviewDraft.ClosureCommand)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Required Evidence Before Closure")
	fmt.Fprintln(&b)
	for _, evidence := range plan.ReviewDraft.RequiredEvidence {
		fmt.Fprintf(&b, "- %s\n", evidence)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Review YAML Template")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "```yaml")
	fmt.Fprintf(&b, "schema_version: %s\n", SourceIssueReviewSchemaVersion)
	fmt.Fprintf(&b, "id: %s\n", plan.ReviewDraft.ReviewID)
	fmt.Fprintf(&b, "product_id: %s\n", plan.ProductID)
	fmt.Fprintf(&b, "issue_id: %s\n", plan.IssueID)
	fmt.Fprintln(&b, "result: resolved|waived|false_positive")
	fmt.Fprintln(&b, "rationale: \"TODO: reviewed source owner and evidence\"")
	fmt.Fprintln(&b, "evidence_refs:")
	for _, ref := range plan.EvidenceRefs {
		fmt.Fprintf(&b, "  - %q\n", ref)
	}
	fmt.Fprintln(&b, "```")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "%s\n", plan.ReviewDraft.ReviewNote)
	return b.String()
}

func RenderSourceIssueReviewYAML(plan SourceIssuePlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version: %s\n", SourceIssueReviewSchemaVersion)
	fmt.Fprintf(&b, "id: %s\n", plan.ReviewDraft.ReviewID)
	fmt.Fprintf(&b, "product_id: %s\n", plan.ProductID)
	fmt.Fprintf(&b, "issue_id: %s\n", plan.IssueID)
	fmt.Fprintln(&b, "result: TODO_resolved_waived_or_false_positive")
	fmt.Fprintln(&b, "rationale: \"TODO: reviewed source owner evidence\"")
	fmt.Fprintln(&b, "evidence_refs:")
	for _, ref := range plan.EvidenceRefs {
		fmt.Fprintf(&b, "  - %q\n", ref)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "# Edit result to one of: resolved, waived, false_positive.")
	fmt.Fprintln(&b, "# Optional: when result is resolved and the source issue represents a real product change to measure,")
	fmt.Fprintln(&b, "# add follow_up to create a parked ProductHypothesis candidate, not a ProductSignal or claim.")
	fmt.Fprintln(&b, "# follow_up:")
	fmt.Fprintln(&b, "#   change_event: \"TODO: reviewed product change\"")
	fmt.Fprintln(&b, "#   expected_effect: \"TODO: expected measurable effect\"")
	fmt.Fprintln(&b, "#   metrics:")
	fmt.Fprintln(&b, "#     - app_store_product_page_conversion")
	fmt.Fprintln(&b, "#   observation_window: \"14d after reviewed change\"")
	fmt.Fprintln(&b, "#   next_action: \"TODO: import ProductSignal after the evidence window\"")
	fmt.Fprintln(&b, "# Closing this issue must not imply a durable product change; log ProductSignal/ProductHypothesis separately when evidence supports it.")
	return b.String()
}
