package projection

import (
	"fmt"

	"compound/internal/reviewartifact"
)

func (p RegenerationPlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForRegenerationPlan(p)
}

func OperatorCardForRegenerationPlan(p RegenerationPlan) (reviewartifact.OperatorCard, error) {
	view, err := reviewartifact.RequireText("view_ref", p.ViewRef, p.ProjectionID)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	context := projectionOperatorContext(view)
	count := len(p.FailedChecks)
	if count == 0 {
		count = 1
	}
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("Projection repair found %d failed checks for %s. Regenerate after review?", count, context),
		context,
		"projection-regeneration|"+reviewartifact.FirstNonEmpty(p.ProjectionID, "projection"),
		"generated views refresh only through owner generators",
	)
}

func projectionOperatorContext(value string) string {
	label := reviewartifact.PlainLabel(reviewartifact.HumanizeEntity(value))
	if label == "" {
		label = "projection"
	}
	return reviewartifact.ShortLabel(label, 42)
}
