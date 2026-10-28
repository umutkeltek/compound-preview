package sourceobs

import (
	"fmt"

	"compound/internal/reviewartifact"
)

func (p SourceObservationVerdictPlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForObservationVerdictPlan(p)
}

func OperatorCardForObservationVerdictPlan(p SourceObservationVerdictPlan) (reviewartifact.OperatorCard, error) {
	owner, err := reviewartifact.RequireText("owner", p.OwnerRef, p.OwnerKind, p.SourceRef)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	owner = reviewartifact.ShortLabel(reviewartifact.PlainLabel(owner), 60)
	statement := reviewartifact.ShortLabel(reviewartifact.PlainLabel(reviewartifact.FirstClause(reviewartifact.FirstNonEmpty(p.Statement, p.Subject, p.Detail, p.PanelDecision))), 54)
	if statement == "" {
		statement = "this observation"
	}
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("Observation intake has source evidence for %s. Record no-promote evidence?", statement),
		owner,
		"source-observation-verdicts|"+reviewartifact.FirstNonEmpty(p.OwnerKind, "source"),
		"source evidence is filed without becoming truth",
	)
}
