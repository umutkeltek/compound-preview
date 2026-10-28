package observation

import (
	"fmt"

	"compound/internal/reviewartifact"
)

func (p ObservationReviewPlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForReviewPlan(p)
}

func OperatorCardForReviewPlan(p ObservationReviewPlan) (reviewartifact.OperatorCard, error) {
	kind, err := reviewartifact.RequireText("observation_kind", p.ObservationKind, p.CandidateKind)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	source, err := reviewartifact.RequireText("source", p.Source.Ref, p.Source.Kind, p.SourceKind)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("A new %s observation arrived from %s. Review it?", reviewartifact.PlainLabel(kind), reviewartifact.ShortLabel(source, 44)),
		reviewartifact.FirstNonEmpty(p.Subject.ID, source),
		"observation-candidates|"+kind,
		"observation evidence gets routed through lenses",
	)
}
