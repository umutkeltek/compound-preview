package observation

import (
	"testing"
	"time"

	"compound/internal/reviewartifact"
)

// TestAnnotateReviewQueueFillsFallbackForObservationCandidates covers gap
// #1's observation-candidates area: annotateObservationReviewQueueItem set
// readiness/actions/decision-card but never computed the operator card.
func TestAnnotateReviewQueueFillsFallbackForObservationCandidates(t *testing.T) {
	homePath := t.TempDir()
	plan := ObservationReviewPlan{
		SchemaVersion:   ObservationReviewPlanSchemaVersion,
		ID:              "obsrp_test",
		CreatedAt:       "2026-07-01T00:00:00Z",
		Status:          "review_required",
		ObservationID:   "obs_test",
		ObservationKind: "capability_observation",
		Source:          Source{Kind: "conversation", Ref: "session:test"},
		Subject:         Subject{Kind: "capability", ID: "cap_test"},
	}
	if _, err := reviewartifact.WriteJSON(homePath, ObservationReviewArea, plan.ID+".json", plan, "  "); err != nil {
		t.Fatal(err)
	}
	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: ObservationReviewArea}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Items[0].SummarySource != reviewartifact.SummarySourceFallback {
		t.Fatalf("precondition: legacy plan should start as fallback, got %+v", report.Items[0])
	}
	AnnotateReviewQueue(&report)
	reviewartifact.Recount(&report)

	item := report.Items[0]
	want := "A new capability observation observation arrived from session:test. Review it?"
	if item.OperatorSummary != want {
		t.Fatalf("OperatorSummary = %q, want %q", item.OperatorSummary, want)
	}
	if item.Band != reviewartifact.BandNeedsYou {
		t.Fatalf("Band = %q, want needs_you", item.Band)
	}
}
