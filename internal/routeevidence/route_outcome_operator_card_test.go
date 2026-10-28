package routeevidence

import (
	"testing"
	"time"

	"compound/internal/reviewartifact"
)

// TestAnnotateRouteOutcomeQueueFillsFallback covers gap #1's route-remorse
// area: there was no annotator at all, so the single live artifact rendered
// the generic fallback sentence.
func TestAnnotateRouteOutcomeQueueFillsFallback(t *testing.T) {
	homePath := t.TempDir()
	review := RouteOutcomeReview{
		SchemaVersion:   RouteOutcomeReviewSchemaVersion,
		ID:              "ror_test",
		CreatedAt:       "2026-07-03T07:39:30Z",
		Status:          "recorded",
		RouteEvidenceID: "route_trial_test",
		ReceiptID:       "rec_test",
		ReceiptMoveType: "outward_delta",
		Assessment: RouteOutcomeAssessment{
			Class:              "route_supported",
			ShouldReviewPolicy: false,
			CostClass:          "tokens_without_cost",
		},
	}
	if _, err := reviewartifact.WriteJSON(homePath, routeOutcomeReviewArea, review.ID+".json", review, "  "); err != nil {
		t.Fatal(err)
	}
	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: routeOutcomeReviewArea}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Items[0].SummarySource != reviewartifact.SummarySourceFallback {
		t.Fatalf("precondition: legacy review should start as fallback, got %+v", report.Items[0])
	}
	AnnotateRouteOutcomeQueue(&report)
	reviewartifact.Recount(&report)

	item := report.Items[0]
	want := "The route watcher recorded outward delta route evidence. Read the outcome."
	if item.OperatorSummary != want {
		t.Fatalf("OperatorSummary = %q, want %q", item.OperatorSummary, want)
	}
	if item.Band != reviewartifact.BandFiled {
		t.Fatalf("Band = %q, want filed", item.Band)
	}
	if item.ContextLabel == "rec_test" {
		t.Fatalf("context_label must not be the raw receipt id: %q", item.ContextLabel)
	}
}

// TestPromotionReviewedHoldInsufficientStaysWatching covers gap #4: a
// recorded hold_insufficient_evidence review is still an informational
// monitoring state, not an applied decision — it must stay in watching, not
// disappear into filed once a hold-review sidecar exists.
func TestPromotionReviewedHoldInsufficientStaysWatching(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 7, 20, 31, 25, 0, time.UTC)
	review := PromotionReview{
		SchemaVersion:   PromotionReviewSchemaVersion,
		ID:              "route_promotion_watch_test",
		CreatedAt:       now.Format(time.RFC3339),
		Status:          "review_required",
		Recommendation:  promotionRecommendationHoldInsufficient,
		Reason:          "samples 0 below min_samples 10",
		LaneID:          "auto/large-context",
		CapabilityClass: "classify-extract",
		Criteria:        PromotionCriterion{MinSamples: 10},
		EvidenceRefs:    []string{"route_a"},
	}
	paths, err := reviewartifact.Write(homePath, routePromotionReviewArea, review.ID, review, " ", "# Route promotion\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordPromotionHoldReview(homePath, paths.PlanPath, "hold until more evidence arrives", now.Add(time.Hour)); err != nil {
		t.Fatalf("record hold review: %v", err)
	}

	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: routePromotionReviewArea}, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	AnnotatePromotionReviewQueue(&report)
	reviewartifact.Recount(&report)

	item := report.Items[0]
	if item.Status != "recorded" {
		t.Fatalf("Status = %q, want recorded", item.Status)
	}
	if item.Band != reviewartifact.BandWatching {
		t.Fatalf("Band = %q, want watching (recorded holds must not hide in filed)", item.Band)
	}
	want := "Not enough evidence yet to judge auto/large-context classify extract — 0/10 runs. Still watching."
	if item.OperatorSummary != want {
		t.Fatalf("OperatorSummary = %q, want %q", item.OperatorSummary, want)
	}
}
