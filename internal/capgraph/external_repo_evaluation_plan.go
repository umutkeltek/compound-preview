package capgraph

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/reviewartifact"
)

const ExternalRepoCapabilityEvaluationPlanSchemaVersion = "compound.capability.external_repo_evaluation_plan.v0.1"

type ExternalRepoCapabilityEvaluationPlanOptions struct {
	SourceListPath string
	RepoQuery      string
	Capability     string
	TargetPath     string
}

type ExternalRepoCapabilityEvaluationPlanReport struct {
	SchemaVersion         string   `json:"schema_version"`
	Status                string   `json:"status"`
	CreatedAt             string   `json:"created_at"`
	AuthorityRef          string   `json:"authority_ref"`
	SourceListPath        string   `json:"source_list_path"`
	SourceListQuery       string   `json:"source_list_query"`
	Repo                  string   `json:"repo"`
	Owner                 string   `json:"owner,omitempty"`
	OwnerReviewStatus     string   `json:"owner_review_status,omitempty"`
	OwnerReviewPath       string   `json:"owner_review_path,omitempty"`
	OwnerReviewDecision   string   `json:"owner_review_decision,omitempty"`
	OwnerReviewNextAction string   `json:"owner_review_next_action,omitempty"`
	Capability            string   `json:"capability"`
	Verdict               string   `json:"verdict"`
	EvaluationID          string   `json:"evaluation_id"`
	EvaluationPath        string   `json:"evaluation_path"`
	EvaluationSet         string   `json:"evaluation_set"`
	PlanID                string   `json:"plan_id"`
	PlanPath              string   `json:"plan_path"`
	ReviewPath            string   `json:"review_path"`
	ReviewYAMLPath        string   `json:"review_yaml_path"`
	ManifestDraftPath     string   `json:"manifest_draft_path"`
	Boundaries            []string `json:"boundaries,omitempty"`
	Warnings              []string `json:"warnings,omitempty"`
}

func RecordExternalRepoCapabilityEvaluationPlan(homePath string, opts ExternalRepoCapabilityEvaluationPlanOptions, now time.Time) (ExternalRepoCapabilityEvaluationPlanReport, error) {
	createdAt := reviewartifact.Timestamp(now)
	report := ExternalRepoCapabilityEvaluationPlanReport{
		SchemaVersion:   ExternalRepoCapabilityEvaluationPlanSchemaVersion,
		Status:          "blocked",
		CreatedAt:       createdAt,
		AuthorityRef:    ExternalRepoIntakeAuthorityRef,
		SourceListPath:  strings.TrimSpace(opts.SourceListPath),
		SourceListQuery: strings.TrimSpace(opts.RepoQuery),
		Boundaries: []string{
			"no clone/install/serve/fetch/execute external repos",
			"no hook, MCP, plugin, or project registry activation",
			"no CapabilityGraph mutation; output is capability-evaluation review artifact only",
			"no memory promotion",
		},
		Warnings: []string{
			"generated evaluation remains local evidence until reviewed",
			"activation requires capability evaluation-review plus separate manifest import",
		},
	}
	if strings.TrimSpace(homePath) == "" {
		return report, fmt.Errorf("COMPOUND_HOME required")
	}
	if report.SourceListPath == "" {
		return report, fmt.Errorf("--source-list required")
	}
	if report.SourceListQuery == "" {
		return report, fmt.Errorf("--repo required")
	}

	intake, err := ExternalRepoIntakeReportForHomeWithSourceList(homePath, report.SourceListPath)
	if err != nil {
		return report, err
	}
	sourceItem, match, err := selectReviewedExternalRepoEvaluationItem(intake, report.SourceListQuery)
	if err != nil {
		return report, err
	}

	report.SourceListQuery = sourceItem.Query
	report.Repo = reviewartifact.FirstNonEmpty(match.Repo, sourceItem.Normalized)
	report.Owner = match.Owner
	report.OwnerReviewStatus = match.OwnerReviewStatus
	report.OwnerReviewPath = match.OwnerReviewPath
	report.OwnerReviewDecision = match.OwnerReviewDecision
	report.OwnerReviewNextAction = reviewartifact.FirstNonEmpty(match.OwnerReviewNextAction, match.NextReviewStep)
	report.Capability = externalRepoCapabilityName(opts.Capability, match)
	report.Verdict = "evaluate_further"
	report.Boundaries = reviewartifact.Unique(append(report.Boundaries, match.NotToDo))

	eval := externalRepoCapabilityEvaluation(report, match, now)
	evalPath, err := writeExternalRepoCapabilityEvaluation(homePath, eval)
	if err != nil {
		return report, err
	}
	report.EvaluationID = eval.ID
	report.EvaluationPath = evalPath

	set, err := LoadCapabilityEvaluations(evalPath)
	if err != nil {
		return report, err
	}
	plan, err := BuildCapabilityEvaluationPlan(homePath, set, eval.Capability, opts.TargetPath, now)
	if err != nil {
		return report, err
	}
	paths, err := RecordCapabilityEvaluationPlan(homePath, plan)
	if err != nil {
		return report, err
	}
	report.Status = plan.Status
	report.EvaluationSet = set.ID
	report.PlanID = plan.ID
	report.PlanPath = paths.PlanPath
	report.ReviewPath = paths.ReviewPath
	report.ReviewYAMLPath = paths.ReviewYAMLPath
	report.ManifestDraftPath = paths.ManifestDraftPath
	return report, nil
}

func selectReviewedExternalRepoEvaluationItem(report ExternalRepoIntakeReport, query string) (ExternalRepoSourceListItem, ExternalRepoIntakeDecision, error) {
	needle := normalizeExternalRepoQuery(query)
	if needle == "" {
		return ExternalRepoSourceListItem{}, ExternalRepoIntakeDecision{}, fmt.Errorf("repo query %q did not normalize to owner/repo", query)
	}
	var needsReview []string
	for _, item := range report.SourceListItems {
		if !externalRepoSourceItemMatches(item, needle) {
			continue
		}
		if len(item.Matches) == 0 {
			return ExternalRepoSourceListItem{}, ExternalRepoIntakeDecision{}, fmt.Errorf("external repo %q is unknown in source list", query)
		}
		match := item.Matches[0]
		if match.Decision != "keep_needs_review" {
			return ExternalRepoSourceListItem{}, ExternalRepoIntakeDecision{}, fmt.Errorf("external repo %q decision is %q; capability evaluation bridge only consumes keep_needs_review rows", match.Repo, match.Decision)
		}
		if match.OwnerReviewStatus == "recorded" {
			return item, match, nil
		}
		needsReview = append(needsReview, reviewartifact.FirstNonEmpty(match.Repo, item.Normalized))
	}
	if len(needsReview) > 0 {
		return ExternalRepoSourceListItem{}, ExternalRepoIntakeDecision{}, fmt.Errorf("external repo %q needs recorded owner review before capability evaluation", query)
	}
	return ExternalRepoSourceListItem{}, ExternalRepoIntakeDecision{}, fmt.Errorf("external repo %q not found in source list evaluation candidates", query)
}

func externalRepoSourceItemMatches(item ExternalRepoSourceListItem, needle string) bool {
	if externalRepoQueryMatches(needle, item.Query) || externalRepoQueryMatches(needle, item.Normalized) {
		return true
	}
	for _, match := range item.Matches {
		if externalRepoQueryMatches(needle, match.Repo) {
			return true
		}
	}
	return false
}

func externalRepoCapabilityName(value string, match ExternalRepoIntakeDecision) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	repo := reviewartifact.Slugify(match.Repo, 48)
	if repo == "" {
		repo = reviewartifact.Digest12(match.ReviewID + match.OwnerReviewPath)
	}
	return "external-repo-" + repo + "-workflow"
}

func externalRepoCapabilityEvaluation(report ExternalRepoCapabilityEvaluationPlanReport, match ExternalRepoIntakeDecision, now time.Time) CapabilityEvaluation {
	nextProof := reviewartifact.FirstNonEmpty(report.OwnerReviewNextAction, match.NextReviewStep)
	risks := reviewartifact.FirstNonEmpty(match.NotToDo, "do not adopt wholesale or create a parallel owner")
	contextPlan := externalRepoContextEvaluationPlan(report.Repo, match)
	signals := map[string]any{
		"kind":                      "external_repo_workflow",
		"cluster":                   reviewartifact.FirstNonEmpty(match.Lane, "external-repo-intake"),
		"source_family":             "external-repo-intake",
		"external_repo":             report.Repo,
		"source_list_query":         report.SourceListQuery,
		"existing_owner":            report.Owner,
		"owner_review_status":       report.OwnerReviewStatus,
		"owner_review_decision":     report.OwnerReviewDecision,
		"owner_review_path":         report.OwnerReviewPath,
		"required_next_proof":       nextProof,
		"main_risks":                risks,
		"graph_hint":                reviewartifact.FirstNonEmpty(match.GraphHint, match.Decision),
		"intake_authority":          ExternalRepoIntakeAuthorityRef,
		"evaluation_boundary":       "focused evaluation only; no external repo activation",
		"capability_evaluation_via": "external-intake-evaluation-plan",
	}
	for key, value := range externalRepoContextEvaluationSignals(contextPlan) {
		signals[key] = value
	}
	return CapabilityEvaluation{
		SchemaVersion:    1,
		ID:               reviewartifact.ID("capability-evaluation.external-repo.", report.Repo, report.Capability, report.OwnerReviewPath),
		CreatedAt:        reviewartifact.Timestamp(now),
		SourceReceipt:    reviewartifact.FirstNonEmpty(report.OwnerReviewPath, match.ReviewPath, ExternalRepoIntakeAuthorityRef+"/"+report.Repo),
		Capability:       report.Capability,
		Verdict:          report.Verdict,
		RubricNormalized: externalRepoEvaluationConfidence(match.EvidenceStrength),
		RubricScores: map[string]any{
			"owner_review_recorded":         1,
			"evidence_strength":             match.EvidenceStrength,
			"duplicate_risk":                match.DuplicateRisk,
			"context_evaluation_step_count": len(contextPlan),
		},
		Signals:        signals,
		LocalStateOnly: true,
	}
}

func externalRepoContextEvaluationSignals(steps []ExternalRepoContextEvaluationStep) map[string]any {
	out := map[string]any{}
	for _, step := range steps {
		addSignalString(out, "context_evaluation_lenses", step.Lens)
		addSignalString(out, "context_evaluation_questions", step.Question)
		addSignalString(out, "context_evaluation_triggers", step.Trigger)
		addSignalString(out, "context_evaluation_boundaries", step.Boundary)
		addSignalString(out, "context_evaluation_silence", step.Silence)
		addSignalString(out, "context_evaluation_proofs", step.Proof)
		addSignalString(out, "context_evaluation_retirement", step.Retirement)
	}
	return out
}

func addSignalString(signals map[string]any, key, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	current, _ := signals[key].([]string)
	for _, existing := range current {
		if existing == value {
			return
		}
	}
	signals[key] = append(current, value)
}

func externalRepoEvaluationConfidence(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return 0.5
	}
	if value > 1 {
		return 1
	}
	return value
}

func writeExternalRepoCapabilityEvaluation(homePath string, eval CapabilityEvaluation) (string, error) {
	if err := eval.Validate(); err != nil {
		return "", err
	}
	dir := filepath.Join(homePath, "inbox", "capability-evaluations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, eval.ID+".json")
	data, err := json.MarshalIndent(eval, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
