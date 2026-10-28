package authority

import (
	"fmt"
	"time"

	"compound/internal/reviewartifact"
)

func (p JobHealthPlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForJobHealthPlan(p)
}

func OperatorCardForJobHealthPlan(p JobHealthPlan) (reviewartifact.OperatorCard, error) {
	name, err := reviewartifact.RequireText("job_name", p.JobName, p.JobID)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	ago := "recently"
	if t, parseErr := time.Parse(time.RFC3339, p.LastRunAt); parseErr == nil && !t.IsZero() {
		ago = t.UTC().Format("2006-01-02")
	}
	summary := fmt.Sprintf("The scheduled job %q is failing — last ran %s. Repair it or park it?", reviewartifact.ShortLabel(name, 36), ago)
	if p.Status == "recorded" || p.Status == "resolved" {
		summary = fmt.Sprintf("The scheduled job %q review is already recorded.", reviewartifact.ShortLabel(name, 36))
	}
	return reviewartifact.NewOperatorCard(
		summary,
		name,
		"job-health|"+reviewartifact.FirstNonEmpty(p.JobID, p.JobName),
		"the schedule stops silently skipping "+reviewartifact.ShortLabel(name, 40),
	)
}

func (p SourceControlReviewPlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForSourceControlReviewPlan(p)
}

func OperatorCardForSourceControlReviewPlan(p SourceControlReviewPlan) (reviewartifact.OperatorCard, error) {
	repo, err := reviewartifact.RequireText("repo_id", p.RepoID)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	// RepoID commonly carries a "repo:" machine prefix (e.g. "repo:compound").
	repo = reviewartifact.HumanizeEntity(repo)
	upstream := reviewartifact.FirstNonEmpty(p.Upstream, "upstream")
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("Repo watch sees %s has %d uncommitted files and %d commits ahead of %s. Tidy it?", reviewartifact.ShortLabel(repo, 34), p.ChangedPaths, p.Ahead, reviewartifact.ShortLabel(upstream, 24)),
		repo,
		"source-control-cleanup|"+p.RepoID,
		"the repo stops blocking follow-up work",
	)
}
