package decisions

import (
	"fmt"

	"compound/internal/reviewartifact"
)

func (p LifecycleRepairPlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForLifecycleRepairPlan(p)
}

func OperatorCardForLifecycleRepairPlan(p LifecycleRepairPlan) (reviewartifact.OperatorCard, error) {
	context, err := reviewartifact.RequireText("repair context", firstRepairTitle(p), p.SourceKind, "decision lifecycle")
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	count := p.RepairCount
	if count == 0 {
		count = len(p.Repairs)
	}
	summary := fmt.Sprintf("Decision lifecycle found %d records missing required fields. Repair them?", count)
	if p.Status == "ok" || p.Status == "recorded" || p.Status == "resolved" {
		summary = fmt.Sprintf("Decision lifecycle repair found %d records already handled.", count)
	}
	return reviewartifact.NewOperatorCard(
		summary,
		context,
		"decision-lifecycle-repairs|"+reviewartifact.FirstNonEmpty(p.SourceKind, "source"),
		"decision records regain required lifecycle fields",
	)
}

func (p DecisionCandidatePlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForDecisionCandidatePlan(p)
}

func OperatorCardForDecisionCandidatePlan(p DecisionCandidatePlan) (reviewartifact.OperatorCard, error) {
	context, err := reviewartifact.RequireText("decision candidate", p.LifecycleDraft.Title, p.Subject, p.DecisionKey)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	choice := reviewartifact.ShortLabel(reviewartifact.PlainLabel(reviewartifact.FirstClause(reviewartifact.FirstNonEmpty(p.LifecycleDraft.Choice, p.Statement, p.Object))), 50)
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("Decision lifecycle has candidate %s. Review it?", choice),
		context,
		decisionCandidateReviewGroupKey(p),
		"decision source changes only after reviewed approval",
	)
}

func decisionCandidateReviewGroupKey(p DecisionCandidatePlan) string {
	identity := reviewartifact.FirstNonEmpty(p.ID, p.CandidateID, p.DecisionKey, "decision")
	key := reviewartifact.Slugify(identity, 72)
	if key == "" {
		key = "decision"
	}
	return DecisionCandidateReviewArea + "|" + key
}

func firstRepairTitle(p LifecycleRepairPlan) string {
	for _, repair := range p.Repairs {
		if repair.Title != "" {
			return repair.Title
		}
		if repair.DecisionID != "" {
			return repair.DecisionID
		}
	}
	return ""
}
