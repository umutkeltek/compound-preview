package sourceobs

import (
	"testing"
	"time"

	"compound/internal/reviewartifact"
)

// TestAnnotateObservationVerdictQueueFillsFallback covers gap #1's
// source-observation-verdicts area: annotateSourceObservationVerdictItem
// never loaded the plan in its "already reviewed" branch and never computed
// the operator card in either branch.
func TestAnnotateObservationVerdictQueueFillsFallback(t *testing.T) {
	homePath := t.TempDir()
	plan := SourceObservationVerdictPlan{
		SchemaVersion: SourceObservationVerdictPlanSchemaVersion,
		ID:            "sovp_test",
		CreatedAt:     "2026-07-01T00:00:00Z",
		Status:        "review_required",
		OwnerKind:     "product",
		OwnerRef:      "demo-product",
		VerdictID:     "panel_verdict_test",
		PanelDecision: "reject",
		Statement:     "the observed metric does not support the hypothesis",
	}
	if _, err := reviewartifact.WriteJSON(homePath, SourceObservationVerdictReviewArea, plan.ID+".json", plan, "  "); err != nil {
		t.Fatal(err)
	}
	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: SourceObservationVerdictReviewArea}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Items[0].SummarySource != reviewartifact.SummarySourceFallback {
		t.Fatalf("precondition: legacy plan should start as fallback, got %+v", report.Items[0])
	}
	AnnotateObservationVerdictQueue(&report)
	reviewartifact.Recount(&report)

	item := report.Items[0]
	want := "Observation intake has source evidence for the observed metric does not support the hypothesis. Record no-promote evidence?"
	if item.OperatorSummary != want {
		t.Fatalf("OperatorSummary = %q, want %q", item.OperatorSummary, want)
	}
	if item.Band != reviewartifact.BandNeedsYou {
		t.Fatalf("Band = %q, want needs_you", item.Band)
	}
}
