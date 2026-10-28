package distiller

import (
	"testing"
	"time"

	"compound/internal/reviewartifact"
)

// TestAnnotateImprovementQueueFillsFallback covers gap #1's
// distiller-improvements area: annotateImprovementQueueItem tracked
// readiness/actions but never computed the operator card.
func TestAnnotateImprovementQueueFillsFallback(t *testing.T) {
	homePath := t.TempDir()
	plan := ImprovementPlan{
		SchemaVersion: ImprovementPlanSchemaVersion,
		ID:            "evp_test",
		CreatedAt:     "2026-07-01T00:00:00Z",
		Status:        "review_required",
		CandidateID:   "cand_test",
		Subject:       "clusters extraction candidates before ranking them",
		Statement:     "clusters extraction candidates before ranking them",
		EvidenceRefs:  []string{"compound://distiller/improvement"},
	}
	if _, err := reviewartifact.WriteJSON(homePath, ImprovementReviewArea, plan.ID+".json", plan, "  "); err != nil {
		t.Fatal(err)
	}
	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: ImprovementReviewArea}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Items[0].SummarySource != reviewartifact.SummarySourceFallback {
		t.Fatalf("precondition: legacy plan should start as fallback, got %+v", report.Items[0])
	}
	AnnotateImprovementQueue(&report)
	reviewartifact.Recount(&report)

	item := report.Items[0]
	want := "The distiller proposes a change to how it clusters extraction candidates before ranking them. Review it?"
	if item.OperatorSummary != want {
		t.Fatalf("OperatorSummary = %q, want %q", item.OperatorSummary, want)
	}
	if item.Band != reviewartifact.BandNeedsYou {
		t.Fatalf("Band = %q, want needs_you", item.Band)
	}
}

// TestAnnotateRequestQueueFillsFallback covers gap #1's distiller-requests
// area: a legacy replay packet with no operator_summary on disk must still
// get the §4 sentence at read time.
func TestAnnotateRequestQueueFillsFallback(t *testing.T) {
	homePath := t.TempDir()
	req := LLMExtractionRequest{
		SchemaVersion: LLMExtractionSchemaVersion,
		TraceRef:      "trace_test_01",
		Instructions:  []string{"extract meaning, not shape"},
	}
	if _, err := reviewartifact.WriteJSON(homePath, LLMRequestRecordArea, "llmreq_test.json", req, " "); err != nil {
		t.Fatal(err)
	}
	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: LLMRequestRecordArea}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Items[0].SummarySource != reviewartifact.SummarySourceFallback {
		t.Fatalf("precondition: legacy request should start as fallback, got %+v", report.Items[0])
	}
	AnnotateRequestQueue(&report)
	reviewartifact.Recount(&report)

	item := report.Items[0]
	want := "The distiller recorded an extraction replay request. Read the packet."
	if item.OperatorSummary != want {
		t.Fatalf("OperatorSummary = %q, want %q", item.OperatorSummary, want)
	}
	if item.Band != reviewartifact.BandFiled {
		t.Fatalf("Band = %q, want filed (recorded replay packet)", item.Band)
	}
}
