package capgraph

import (
	"fmt"
	"strings"

	"compound/internal/reviewartifact"
)

func (p CapabilityLifecycleProposal) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForLifecycleProposal(p)
}

func OperatorCardForLifecycleProposal(p CapabilityLifecycleProposal) (reviewartifact.OperatorCard, error) {
	subject, err := reviewartifact.RequireText("proposal subject", p.Subject, p.Statement, p.Draft.CapabilityRef)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	n := len(p.Draft.RequiredEvidence)
	if n == 0 {
		n = len(p.EvidenceRefs)
	}
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("Capability screening proposes a lifecycle change touching %d evidence hooks. Review the proposal?", n),
		subject,
		"capability-lifecycle-proposals|"+p.ID,
		"capability lifecycle changes only after review",
	)
}

func (p CapabilityEvaluationPlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForEvaluationPlan(p)
}

func OperatorCardForEvaluationPlan(p CapabilityEvaluationPlan) (reviewartifact.OperatorCard, error) {
	capability, err := reviewartifact.RequireText("capability", p.Capability, p.NodeDraft.Name)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	score := fmt.Sprintf("%.2f", p.RubricScore)
	verdict := reviewartifact.PlainLabel(reviewartifact.FirstNonEmpty(p.Verdict, "review"))
	summary := fmt.Sprintf("Capability screening scored %s %s — verdict: %s. Review it?", reviewartifact.ShortLabel(capability, 44), score, verdict)
	if p.Status == "recorded" || p.Status == "ok" {
		summary = fmt.Sprintf("Capability screening scored %s %s — verdict: %s.", reviewartifact.ShortLabel(capability, 44), score, verdict)
	}
	return reviewartifact.NewOperatorCard(
		summary,
		capability,
		"capability-evaluation-intake|"+reviewartifact.FirstNonEmpty(p.Verdict, "review"),
		"capability evidence becomes reviewable graph input",
	)
}

func (p ExternalRepoIntakeReviewPlanItem) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForExternalRepoIntake(p)
}

func OperatorCardForExternalRepoIntake(p ExternalRepoIntakeReviewPlanItem) (reviewartifact.OperatorCard, error) {
	repo, err := reviewartifact.RequireText("repo", p.Repo, p.NormalizedRepo)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	owner := reviewartifact.ShortLabel(reviewartifact.PlainLabel(reviewartifact.FirstNonEmpty(p.Owner, p.Lane, "Compound")), 42)
	date := p.CreatedAt
	if i := strings.Index(date, "T"); i > 0 {
		date = date[:i]
	}
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("Repo intake kept %s reference evidence for %s — nothing installed or run. File it?", reviewartifact.ShortLabel(repo, 42), owner),
		repo,
		"external-repo-intake|"+reviewartifact.FirstNonEmpty(p.Decision, "review")+"|"+reviewartifact.FirstNonEmpty(date, "date"),
		"they become citable evidence, or clear out honestly",
	)
}

func (r SkillCandidateReview) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForSkillCandidateReview(r)
}

func OperatorCardForSkillCandidateReview(r SkillCandidateReview) (reviewartifact.OperatorCard, error) {
	name, err := reviewartifact.RequireText("skill candidate", r.External.Name, r.External.Slug, r.CandidateID)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	decision := reviewartifact.PlainLabel(reviewartifact.FirstNonEmpty(r.RecommendedDisposition, r.GraphCoherence.DecisionHint, "review"))
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("Capability screening found external skill %s — recommended %s. Review it?", reviewartifact.ShortLabel(name, 44), decision),
		name,
		"skill-candidates|"+reviewartifact.FirstNonEmpty(r.RecommendedDisposition, "review"),
		"external skill evidence stays behind review gates",
	)
}

func (p SkillPlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForSkillPlan(p)
}

func OperatorCardForSkillPlan(p SkillPlan) (reviewartifact.OperatorCard, error) {
	name, err := reviewartifact.RequireText("skill name", p.NodeDraft.Name, p.Subject, p.SkillKey)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("Capability screening proposes skill node %s. Review it?", reviewartifact.ShortLabel(name, 52)),
		name,
		"capability-skills|"+reviewartifact.FirstNonEmpty(p.SkillKey, p.CandidateID),
		"the graph gains only reviewed skill offers",
	)
}

func (p OutboundDistributionVerdictPlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForOutboundDistributionVerdict(p)
}

func OperatorCardForOutboundDistributionVerdict(p OutboundDistributionVerdictPlan) (reviewartifact.OperatorCard, error) {
	product, err := reviewartifact.RequireText("product_id", p.ProductID)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	change := reviewartifact.ShortLabel(reviewartifact.PlainLabel(reviewartifact.FirstClause(p.ChangeEvent)), 46)
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("Capability screening has outbound evidence for %s. Adopt dry-run only?", change),
		product,
		"outbound-distribution-verdicts|"+p.ProductID,
		"distribution stays dry-run until policy approval",
	)
}

func (r OutboundDistributionDryRunReceiptReport) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForOutboundDistributionDryRun(r)
}

func OperatorCardForOutboundDistributionDryRun(r OutboundDistributionDryRunReceiptReport) (reviewartifact.OperatorCard, error) {
	product, err := reviewartifact.RequireText("product_id", r.ProductID)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	move := reviewartifact.ShortLabel(reviewartifact.PlainLabel(reviewartifact.FirstClause(r.ProductLoop.NextBoundedMove)), 46)
	count := len(r.Artifacts)
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("Capability screening reviewed %d outbound dry-run artifacts for %s. Accept evidence?", count, move),
		product,
		"outbound-distribution-dry-runs|"+r.ProductID,
		"product loop gains dry-run evidence without public side effects",
	)
}
