package product

import (
	"testing"
	"time"

	"compound/internal/reviewartifact"
)

// TestAnnotateProductActionReviewItemFillsFallback covers gap #1's
// product-action-cards area: annotateProductActionReviewItem tracked
// readiness/actions/decision-card but never computed the operator card, so a
// legacy recorded plan (written before card injection existed) kept
// rendering the generic fallback sentence.
func TestAnnotateProductActionReviewItemFillsFallback(t *testing.T) {
	homePath := t.TempDir()
	plan := ActionPlan{
		SchemaVersion: ActionPlanSchemaVersion,
		ID:            "pacp_test",
		CreatedAt:     "2026-07-01T00:00:00Z",
		Status:        "recorded",
		ProductID:     "demo-product",
		ProductName:   "product:demo-product",
		CardID:        "card_test",
		CardKind:      "review_signals",
		CardTitle:     "Review pending signals",
		ActionSummary: "Review 2 product signals without a matching hypothesis. Hold ASC fetch or spend.",
		EvidenceRefs:  []string{"compound://product/demo-product"},
	}
	if _, err := reviewartifact.WriteJSON(homePath, ProductActionReviewArea, plan.ID+".json", plan, "  "); err != nil {
		t.Fatal(err)
	}
	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: ProductActionReviewArea}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Items[0].SummarySource != reviewartifact.SummarySourceFallback {
		t.Fatalf("precondition: legacy plan should start as fallback, got %+v", report.Items[0])
	}
	AnnotateReviewQueue(&report)
	reviewartifact.Recount(&report)

	item := report.Items[0]
	want := "demo-product has one bounded move ready: Review 2 product signals without a matching hypothesis. Start it?"
	if item.OperatorSummary != want {
		t.Fatalf("OperatorSummary = %q, want %q", item.OperatorSummary, want)
	}
	if item.Band != reviewartifact.BandFiled {
		t.Fatalf("Band = %q, want filed (status already recorded)", item.Band)
	}
}
