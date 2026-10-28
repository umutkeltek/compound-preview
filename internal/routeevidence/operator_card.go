package routeevidence

import (
	"fmt"
	"math"

	"compound/internal/reviewartifact"
)

func (r PromotionReview) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForPromotionReview(r)
}

func OperatorCardForPromotionReview(r PromotionReview) (reviewartifact.OperatorCard, error) {
	lane, err := reviewartifact.RequireText("lane_id", r.LaneID)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	class, err := reviewartifact.RequireText("capability_class", r.CapabilityClass)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	class = reviewartifact.PlainLabel(class)
	samples := r.Tallies.Samples
	failPct := int(math.Round(r.Tallies.FailedStatusRate * 100))
	group := fmt.Sprintf("route-promotions|%s|%s", reviewartifact.FirstNonEmpty(r.Recommendation, "review"), r.CapabilityClass)
	switch r.Recommendation {
	case promotionRecommendationPromote:
		level := reviewartifact.FirstNonEmpty(r.RecommendedClaimLevel, r.CurrentClaimLevel, "trusted")
		return reviewartifact.NewOperatorCard(
			fmt.Sprintf("The route watcher says %s earned %s %s — %d runs, %d%% failed. Promote it?", lane, level, class, samples, failPct),
			lane,
			group,
			fmt.Sprintf("the router starts trusting %s with %s work", lane, class),
		)
	case promotionRecommendationDemote:
		return reviewartifact.NewOperatorCard(
			fmt.Sprintf("The route watcher wants %s off %s — %d%% of %d runs failed. Demote it?", lane, class, failPct, samples),
			lane,
			group,
			"the router stops sending it solo work",
		)
	case promotionRecommendationHoldInsufficient:
		minSamples := r.Criteria.MinSamples
		return reviewartifact.NewOperatorCard(
			fmt.Sprintf("Not enough evidence yet to judge %s %s — %d/%d runs. Still watching.", lane, class, samples, minSamples),
			lane,
			group,
			"",
		)
	default:
		return reviewartifact.NewOperatorCard(
			fmt.Sprintf("The route watcher has %d runs for %s %s. Review the route evidence?", samples, lane, class),
			lane,
			group,
			"routing policy gets reviewed evidence for this lane",
		)
	}
}

func (r RouteOutcomeReview) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForRouteOutcomeReview(r)
}

func OperatorCardForRouteOutcomeReview(r RouteOutcomeReview) (reviewartifact.OperatorCard, error) {
	// The receipt/route-evidence id is required as an existence check, but
	// must not surface as the context_label — G3 bans raw ids in
	// operator-facing text; ids belong in the card's mono meta line, not
	// here. The capability class (or move type) is the human-legible "what
	// is this about" the context_label needs instead.
	if _, err := reviewartifact.RequireText("receipt_id", r.ReceiptID, r.RouteEvidenceID); err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	class := reviewartifact.PlainLabel(reviewartifact.FirstNonEmpty(firstCapabilityClass(r.CapabilityClasses), r.ReceiptMoveType, "route outcome"))
	summary := fmt.Sprintf("The route watcher found remorse evidence for %s. Review policy impact?", class)
	if !r.Assessment.ShouldReviewPolicy {
		summary = fmt.Sprintf("The route watcher recorded %s route evidence. Read the outcome.", class)
	}
	return reviewartifact.NewOperatorCard(summary, class, "route-remorse|"+reviewartifact.FirstNonEmpty(r.Assessment.CostClass, r.RouteStatus, "outcome"), "routing policy gets cleaner evidence")
}

func (p RoutePolicyPlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForRoutePolicyPlan(p)
}

func OperatorCardForRoutePolicyPlan(p RoutePolicyPlan) (reviewartifact.OperatorCard, error) {
	subject, err := reviewartifact.RequireText("route policy subject", p.Subject, p.PolicyDraft.Title, p.RouteKey)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	method := reviewartifact.ShortLabel(reviewartifact.PlainLabel(reviewartifact.FirstClause(reviewartifact.FirstNonEmpty(p.Statement, p.PolicyDraft.RoutePreference))), 48)
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("The route watcher proposes policy for %s. Review it?", method),
		subject,
		"route-policy|"+reviewartifact.FirstNonEmpty(p.RouteKey, p.CandidateID),
		"future routing uses reviewed cost and capability evidence",
	)
}

func firstCapabilityClass(values []string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
