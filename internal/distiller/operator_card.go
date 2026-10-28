package distiller

import (
	"fmt"

	"compound/internal/reviewartifact"
)

func (p ImprovementPlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForImprovementPlan(p)
}

func OperatorCardForImprovementPlan(p ImprovementPlan) (reviewartifact.OperatorCard, error) {
	method, err := reviewartifact.RequireText("improvement method", p.Draft.Title, p.Subject, p.Statement)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	method = reviewartifact.ShortLabel(reviewartifact.PlainLabel(reviewartifact.FirstClause(method)), 56)
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("The distiller proposes a change to how it %s. Review it?", method),
		method,
		"distiller-improvements|"+reviewartifact.FirstNonEmpty(p.Draft.Area, p.CandidateKind, "method"),
		"distiller changes only after reviewed proof",
	)
}

func (p EvalPlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForEvalPlan(p)
}

func OperatorCardForEvalPlan(p EvalPlan) (reviewartifact.OperatorCard, error) {
	subject, err := reviewartifact.RequireText("eval subject", p.Subject, p.Statement, p.FixtureDraft.ProposedCaseID)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	label := reviewartifact.ShortLabel(reviewartifact.PlainLabel(reviewartifact.FirstClause(subject)), 56)
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("The distiller proposes an eval case for %s. Review it?", label),
		label,
		"distiller-eval|"+reviewartifact.FirstNonEmpty(p.CandidateVerdict, "review"),
		"the eval corpus gains only reviewed regression cases",
	)
}

func (p FailureRulePlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForFailureRulePlan(p)
}

func OperatorCardForFailureRulePlan(p FailureRulePlan) (reviewartifact.OperatorCard, error) {
	subject, err := reviewartifact.RequireText("failure rule subject", p.RuleDraft.Title, p.Subject, p.Statement)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	label := reviewartifact.ShortLabel(reviewartifact.PlainLabel(reviewartifact.FirstClause(subject)), 56)
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("The distiller proposes a failure rule for %s. Review it?", label),
		label,
		"failure-rules|"+reviewartifact.FirstNonEmpty(p.FailureKey, p.CandidateID),
		"repeat failures get a reviewed prevention rule",
	)
}

func (p TastePlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForTastePlan(p)
}

func OperatorCardForTastePlan(p TastePlan) (reviewartifact.OperatorCard, error) {
	subject, err := reviewartifact.RequireText("taste standard subject", p.StandardDraft.Title, p.Subject, p.Statement)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	label := reviewartifact.ShortLabel(reviewartifact.PlainLabel(reviewartifact.FirstClause(subject)), 56)
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("The distiller proposes a taste standard for %s. Review it?", label),
		label,
		"taste-standards|"+reviewartifact.FirstNonEmpty(p.TasteKey, p.CandidateID),
		"future reviews get an evidence-backed taste standard",
	)
}

func OperatorCardForLLMRequest(req LLMExtractionRequest) (reviewartifact.OperatorCard, error) {
	trace, err := reviewartifact.RequireText("trace_ref", req.TraceRef)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	return reviewartifact.NewOperatorCard(
		"The distiller recorded an extraction replay request. Read the packet.",
		trace,
		"distiller-requests|"+trace,
		"the replay stays citable without calling a model",
	)
}
