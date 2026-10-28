package authority

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/reviewartifact"
)

const (
	JobHealthPlanSchemaVersion       = "compound.job_health_plan.v0.1"
	JobHealthPlanReportSchemaVersion = "compound.job_health_plan_report.v0.1"
	jobHealthReviewArea              = "job-health"
)

type JobHealthPlan struct {
	SchemaVersion  string               `json:"schema_version"`
	ID             string               `json:"id"`
	CreatedAt      string               `json:"created_at"`
	Status         string               `json:"status"`
	RepairCount    int                  `json:"repair_count"`
	AuthorityRef   string               `json:"authority_ref"`
	AuthorityPath  string               `json:"authority_path,omitempty"`
	SourceKind     string               `json:"source_kind"`
	SourcePath     string               `json:"source_path,omitempty"`
	TargetPath     string               `json:"target_path,omitempty"`
	SourceRevision string               `json:"source_revision,omitempty"`
	GeneratedAt    string               `json:"generated_at,omitempty"`
	JobID          string               `json:"job_id,omitempty"`
	JobName        string               `json:"job_name"`
	JobStatus      string               `json:"job_status,omitempty"`
	LastError      string               `json:"last_error,omitempty"`
	LastRunAt      string               `json:"last_run_at,omitempty"`
	NextAction     string               `json:"next_action"`
	EvidenceRefs   []string             `json:"evidence_refs"`
	ReviewDraft    JobHealthReviewDraft `json:"review_draft"`
	Warnings       []string             `json:"warnings,omitempty"`
}

type JobHealthReviewDraft struct {
	ReviewID          string   `json:"review_id"`
	Title             string   `json:"title"`
	Situation         string   `json:"situation"`
	ProposedNextStep  string   `json:"proposed_next_step"`
	MutationGuardrail string   `json:"mutation_guardrail"`
	Verification      string   `json:"verification"`
	RequiredEvidence  []string `json:"required_evidence"`
	RetirementTrigger string   `json:"retirement_trigger"`
	ReviewNote        string   `json:"review_note"`
}

type JobHealthPlanReport struct {
	SchemaVersion       string                 `json:"schema_version"`
	CreatedAt           string                 `json:"created_at"`
	Status              string                 `json:"status"`
	AuthorityRef        string                 `json:"authority_ref"`
	AuthorityPath       string                 `json:"authority_path"`
	PlanCount           int                    `json:"plan_count"`
	ReviewRequiredCount int                    `json:"review_required_count"`
	RecordedCount       int                    `json:"recorded_count"`
	ResolvedCount       int                    `json:"resolved_count"`
	Plans               []JobHealthPlan        `json:"plans,omitempty"`
	Paths               []reviewartifact.Paths `json:"paths,omitempty"`
	ReviewYAMLs         []string               `json:"review_yamls,omitempty"`
}

func BuildJobHealthPlans(homePath string, roster JobRosterAuthority, now time.Time) ([]JobHealthPlan, error) {
	if strings.TrimSpace(roster.AuthorityRef) == "" {
		return nil, fmt.Errorf("job roster authority required")
	}
	jobs := jobHealthErrorJobs(roster)
	plans := make([]JobHealthPlan, 0, len(jobs))
	for _, job := range jobs {
		jobName := reviewartifact.FirstNonEmpty(job.Name, job.JobID)
		if jobName == "" {
			continue
		}
		jobKey := reviewartifact.FirstNonEmpty(job.JobID, job.Name)
		evidence := reviewartifact.Unique([]string{
			roster.AuthorityRef,
			roster.SourceRevision,
			job.LastRunAt,
			job.LastStatus,
			job.LastError,
		})
		id := reviewartifact.ID("jhp_", roster.SourceRevision, jobKey, job.LastRunAt, job.LastStatus, job.LastError)
		reviewID := "review_" + reviewartifact.Digest12(id)
		nextAction := "review failing scheduled job dependency and decide owner-approved repair or parking"
		plan := JobHealthPlan{
			SchemaVersion:  JobHealthPlanSchemaVersion,
			ID:             id,
			CreatedAt:      reviewartifact.Timestamp(now),
			Status:         "review_required",
			RepairCount:    1,
			AuthorityRef:   roster.AuthorityRef,
			AuthorityPath:  JobRosterAuthorityPath(homePath),
			SourceKind:     "job_roster_error",
			SourcePath:     roster.SourcePath,
			TargetPath:     roster.SourcePath,
			SourceRevision: roster.SourceRevision,
			GeneratedAt:    roster.GeneratedAt,
			JobID:          job.JobID,
			JobName:        jobName,
			JobStatus:      job.LastStatus,
			LastError:      job.LastError,
			LastRunAt:      job.LastRunAt,
			NextAction:     nextAction,
			EvidenceRefs:   evidence,
			ReviewDraft: JobHealthReviewDraft{
				ReviewID:          reviewID,
				Title:             "Review scheduled job failure for " + jobName,
				Situation:         jobHealthSituation(job),
				ProposedNextStep:  nextAction,
				MutationGuardrail: "Do not run, pause, resume, edit, restart, reschedule, or delete the job from this plan. Any scheduler mutation needs a separate reviewed owner action.",
				Verification:      "A follow-up Compound job roster import shows the job healthy, deliberately parked, disabled by owner decision, or still failing with a new review note.",
				RequiredEvidence: []string{
					"Compound job roster authority report containing the failing job",
					"source scheduler or runtime evidence for the failure cause",
					"owner-approved repair, parking, disablement, or no-action decision",
					"post-action job roster authority report or explicit owner note",
				},
				RetirementTrigger: "Close this review when the failure no longer needs operator action or the job is deliberately parked with evidence.",
				ReviewNote:        "This artifact turns job health into a governed review item. It is not a scheduler action.",
			},
			Warnings: []string{
				"does not execute the job",
				"does not mutate scheduler files",
				"does not change job enabled state",
				"does not promote memory claims",
			},
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

func RecordJobHealthPlans(homePath string, roster JobRosterAuthority, now time.Time) (JobHealthPlanReport, error) {
	plans, err := BuildJobHealthPlans(homePath, roster, now)
	if err != nil {
		return JobHealthPlanReport{}, err
	}
	report := JobHealthPlanReport{
		SchemaVersion: JobHealthPlanReportSchemaVersion,
		CreatedAt:     reviewartifact.Timestamp(now),
		Status:        "ok",
		AuthorityRef:  roster.AuthorityRef,
		AuthorityPath: JobRosterAuthorityPath(homePath),
		PlanCount:     len(plans),
	}
	for _, plan := range plans {
		plan, reviewedPath, err := annotateJobHealthPlanWithReview(homePath, plan)
		if err != nil {
			return JobHealthPlanReport{}, err
		}
		report.Plans = append(report.Plans, plan)
		switch plan.Status {
		case "recorded":
			report.RecordedCount++
		case "resolved":
			report.ResolvedCount++
		default:
			report.ReviewRequiredCount++
		}
		paths, err := reviewartifact.Write(homePath, jobHealthReviewArea, plan.ID, plan, "  ", RenderJobHealthPlanMarkdown(plan))
		if err != nil {
			return JobHealthPlanReport{}, err
		}
		reviewYAMLPath := jobHealthReviewYAMLFilePath(homePath, plan.ID)
		if reviewedPath == "" {
			reviewYAMLPath, err = reviewartifact.WriteRaw(homePath, jobHealthReviewArea, plan.ID+".review.yaml", []byte(RenderJobHealthReviewYAMLDraft(plan)))
			if err != nil {
				return JobHealthPlanReport{}, err
			}
		}
		report.Paths = append(report.Paths, paths)
		report.ReviewYAMLs = append(report.ReviewYAMLs, reviewYAMLPath)
	}
	return report, nil
}

func annotateJobHealthPlanWithReview(homePath string, plan JobHealthPlan) (JobHealthPlan, string, error) {
	reviewedPath, err := jobHealthReviewedFileForPlan(homePath, plan.ID)
	if err != nil {
		return JobHealthPlan{}, "", err
	}
	if reviewedPath == "" {
		return plan, "", nil
	}
	report, err := LoadJobHealthReviewReport(reviewedPath)
	if err != nil {
		return JobHealthPlan{}, "", err
	}
	plan.Status = "recorded"
	plan.NextAction = reviewartifact.FirstNonEmpty(report.NextAction, "job-health review recorded: "+report.Decision)
	plan.EvidenceRefs = reviewartifact.Unique(append(plan.EvidenceRefs, report.EvidenceRefs...))
	return plan, reviewedPath, nil
}

func jobHealthReviewedFileForPlan(homePath, planID string) (string, error) {
	if strings.TrimSpace(planID) == "" {
		return "", nil
	}
	matches, err := filepath.Glob(filepath.Join(homePath, "reviews", jobHealthReviewArea, planID+".*.reviewed.json"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", nil
	}
	return matches[0], nil
}

func jobHealthReviewYAMLFilePath(homePath, planID string) string {
	return filepath.Join(homePath, "reviews", jobHealthReviewArea, planID+".review.yaml")
}

func RenderJobHealthPlanMarkdown(plan JobHealthPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Job Health Review: %s\n\n", plan.JobName)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- authority_ref: %s\n", plan.AuthorityRef)
	if plan.SourcePath != "" {
		fmt.Fprintf(&b, "- source_path: %s\n", plan.SourcePath)
	}
	if plan.JobID != "" {
		fmt.Fprintf(&b, "- job_id: %s\n", plan.JobID)
	}
	if plan.JobStatus != "" {
		fmt.Fprintf(&b, "- job_status: %s\n", plan.JobStatus)
	}
	if plan.LastRunAt != "" {
		fmt.Fprintf(&b, "- last_run_at: %s\n", plan.LastRunAt)
	}
	if plan.LastError != "" {
		fmt.Fprintf(&b, "- last_error: %s\n", strings.ReplaceAll(plan.LastError, "\n", "\\n"))
	}
	fmt.Fprintf(&b, "- evidence_refs: %s\n\n", strings.Join(plan.EvidenceRefs, ", "))
	fmt.Fprintln(&b, "This owner-review artifact does not run, pause, resume, edit, restart, reschedule, or delete scheduler jobs.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Draft Review")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- situation: %s\n", plan.ReviewDraft.Situation)
	fmt.Fprintf(&b, "- proposed_next_step: %s\n", plan.ReviewDraft.ProposedNextStep)
	fmt.Fprintf(&b, "- mutation_guardrail: %s\n", plan.ReviewDraft.MutationGuardrail)
	fmt.Fprintf(&b, "- verification: %s\n", plan.ReviewDraft.Verification)
	fmt.Fprintf(&b, "- retirement_trigger: %s\n", plan.ReviewDraft.RetirementTrigger)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Required Evidence")
	fmt.Fprintln(&b)
	for _, evidence := range plan.ReviewDraft.RequiredEvidence {
		fmt.Fprintf(&b, "- %s\n", evidence)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Review YAML Template")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "```yaml")
	fmt.Fprint(&b, RenderJobHealthReviewYAMLDraft(plan))
	fmt.Fprintln(&b, "```")
	if len(plan.Warnings) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Guardrails")
		fmt.Fprintln(&b)
		for _, warning := range plan.Warnings {
			fmt.Fprintf(&b, "- %s\n", warning)
		}
	}
	return b.String()
}

func jobHealthErrorJobs(roster JobRosterAuthority) []JobSummary {
	seen := map[string]bool{}
	var out []JobSummary
	add := func(job JobSummary) {
		key := reviewartifact.FirstNonEmpty(job.JobID, job.Name)
		if key == "" || seen[key] || job.LastStatus != "error" || !actionableJobError(job) {
			return
		}
		seen[key] = true
		out = append(out, job)
	}
	if len(roster.ActiveErrors) > 0 {
		for _, job := range roster.ActiveErrors {
			add(job)
		}
		return out
	}
	for _, job := range roster.RecentErrors {
		add(job)
	}
	for _, job := range roster.Jobs {
		add(job)
	}
	return out
}

func jobHealthSituation(job JobSummary) string {
	name := reviewartifact.FirstNonEmpty(job.Name, job.JobID, "scheduled job")
	status := reviewartifact.FirstNonEmpty(job.LastStatus, "unknown")
	if strings.TrimSpace(job.LastError) != "" {
		return fmt.Sprintf("%s last reported status %s with error: %s", name, status, strings.ReplaceAll(job.LastError, "\n", "\\n"))
	}
	return fmt.Sprintf("%s last reported status %s", name, status)
}
