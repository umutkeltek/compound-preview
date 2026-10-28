package distiller

import (
	"fmt"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/routeevidence"
)

const ExecutionRouteEvidenceSchemaVersion = routeevidence.SchemaVersion

type RouteTarget = routeevidence.Target
type RejectedRouteTarget = routeevidence.RejectedTarget
type ExecutionRouteEvidence = routeevidence.Evidence

func BuildExecutionRouteEvidence(req LLMExtractionRequest, report LiveExtractionReport, now time.Time) (ExecutionRouteEvidence, error) {
	if strings.TrimSpace(req.TraceRef) == "" {
		return ExecutionRouteEvidence{}, fmt.Errorf("trace_ref required execution route evidence")
	}
	if strings.TrimSpace(report.Provider) == "" {
		return ExecutionRouteEvidence{}, fmt.Errorf("provider required execution route evidence")
	}
	if strings.TrimSpace(report.Model) == "" {
		return ExecutionRouteEvidence{}, fmt.Errorf("model required execution route evidence")
	}
	status := "completed"
	if !report.Called {
		status = "not_called"
	}
	workID := ""
	if req.Work != nil {
		workID = req.Work.ID
	}
	return routeevidence.Build(routeevidence.BuildInput{
		CreatedAt:     now,
		RequestID:     "llm_extraction:" + strings.TrimSpace(req.TraceRef),
		WorkID:        workID,
		ClientProfile: "compound.distiller.extract",
		Protocol:      "openai-chat-completions",
		Provider:      report.Provider,
		Model:         report.Model,
		Usage:         routeevidence.Usage(report.Usage),
		LatencyMS:     report.LatencyMS,
		Status:        status,
		EvidenceRefs:  routeEvidenceRefs(req, report),
	})
}

func BuildExecutionRouteTransition(ev ExecutionRouteEvidence) (envelope.Transition, error) {
	return routeevidence.BuildTransition(ev)
}

func RecordExecutionRouteEvidence(homePath string, ev ExecutionRouteEvidence) error {
	return routeevidence.RecordEvidence(homePath, ev)
}

func RecordExecutionRouteTransition(homePath string, tr envelope.Transition) error {
	return routeevidence.RecordTransition(homePath, tr)
}

func JoinExecutionRouteTransition(ev ExecutionRouteEvidence) (ExecutionRouteEvidence, envelope.Transition, error) {
	return routeevidence.JoinTransition(ev)
}

func routeEvidenceRefs(req LLMExtractionRequest, report LiveExtractionReport) []string {
	seen := map[string]bool{}
	var refs []string
	add := func(ref string) {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			return
		}
		seen[ref] = true
		refs = append(refs, ref)
	}
	for _, ref := range req.SourceRefs {
		add(ref)
	}
	if report.Eval.Dir != "" {
		add("eval_corpus:" + report.Eval.Dir)
	}
	if len(refs) == 0 {
		add("trace_ref:" + req.TraceRef)
	}
	return refs
}
