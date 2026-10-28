package product

import (
	"fmt"

	"compound/internal/reviewartifact"
)

func (p ActionPlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForActionPlan(p)
}

func OperatorCardForActionPlan(p ActionPlan) (reviewartifact.OperatorCard, error) {
	product, err := reviewartifact.RequireText("product", p.ProductName, p.ProductID)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	// ProductName commonly carries a "product:" machine prefix (e.g.
	// "product:demo-app"). HumanizeEntity strips it so the sentence and
	// context_label read as a plain product name, not a machine ref.
	product = reviewartifact.HumanizeEntity(product)
	move, err := reviewartifact.RequireText("action summary", p.ActionSummary, p.CardTitle)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	move = reviewartifact.ShortLabel(reviewartifact.FirstClause(move), 58)
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("%s has one bounded move ready: %s. Start it?", reviewartifact.ShortLabel(product, 36), move),
		product,
		"product-action-cards|"+p.ProductID,
		"the product loop gets one reviewed next move",
	)
}

func (p ReviewPlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForReviewPlan(p)
}

func OperatorCardForReviewPlan(p ReviewPlan) (reviewartifact.OperatorCard, error) {
	product, err := reviewartifact.RequireText("product_id", p.ProductID)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	metric, err := reviewartifact.RequireText("metric_name", p.MetricName)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("%s has %s evidence for a hypothesis. Review the result?", reviewartifact.ShortLabel(product, 34), reviewartifact.ShortLabel(metric, 34)),
		product,
		"product-reviews|"+p.ProductID,
		"the hypothesis gets a reviewed product result",
	)
}

func (p ObservationVerdictPlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForObservationVerdictPlan(p)
}

func OperatorCardForObservationVerdictPlan(p ObservationVerdictPlan) (reviewartifact.OperatorCard, error) {
	product, err := reviewartifact.RequireText("product_id", p.ProductID)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	event := reviewartifact.ShortLabel(reviewartifact.FirstClause(reviewartifact.FirstNonEmpty(p.ChangeEvent, p.Statement)), 50)
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("%s has reviewed observation evidence: %s. Create a hypothesis?", reviewartifact.ShortLabel(product, 34), event),
		product,
		"product-observation-verdicts|"+p.ProductID,
		"product feedback becomes a reviewed hypothesis candidate",
	)
}

func (p OutboundDryRunEvidencePlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForOutboundDryRunEvidencePlan(p)
}

func OperatorCardForOutboundDryRunEvidencePlan(p OutboundDryRunEvidencePlan) (reviewartifact.OperatorCard, error) {
	product, err := reviewartifact.RequireText("product_id", p.ProductID)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	move := reviewartifact.ShortLabel(reviewartifact.FirstClause(p.NextBoundedMove), 48)
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("%s has outbound dry-run evidence for %s. Create a hypothesis?", reviewartifact.ShortLabel(product, 34), move),
		product,
		"product-outbound-dry-runs|"+p.ProductID,
		"dry-run evidence enters the product loop only after review",
	)
}

func (p HypothesisPlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForHypothesisPlan(p)
}

func OperatorCardForHypothesisPlan(p HypothesisPlan) (reviewartifact.OperatorCard, error) {
	product, err := reviewartifact.RequireText("product_id", p.ProductID)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	effect, err := reviewartifact.RequireText("expected effect", p.ExpectedEffect, p.ChangeEvent)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("%s has a product hypothesis: %s. Review it?", reviewartifact.ShortLabel(product, 34), reviewartifact.ShortLabel(reviewartifact.FirstClause(effect), 52)),
		product,
		"product-hypotheses|"+p.ProductID,
		"the product loop gets an evidence-shaped hypothesis",
	)
}

func (p SourceIssuePlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForSourceIssuePlan(p)
}

func OperatorCardForSourceIssuePlan(p SourceIssuePlan) (reviewartifact.OperatorCard, error) {
	product, err := reviewartifact.RequireText("product_id", p.ProductID)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	basis, err := reviewartifact.RequireText("basis", p.Basis, p.SourceRef, p.IssueID)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("%s has a source issue: %s. Close or keep it?", reviewartifact.ShortLabel(product, 34), reviewartifact.ShortLabel(reviewartifact.FirstClause(basis), 52)),
		product,
		"product-source-issues|"+p.ProductID,
		"source issue debt gets an explicit product decision",
	)
}
