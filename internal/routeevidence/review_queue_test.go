package routeevidence

import (
	"bytes"
	"os"
	"reflect"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/reviewartifact"
)

func TestPromotionReviewQueueUsesArchivedMatchingHoldSidecar(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 2, 16, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatalf("init home: %v", err)
	}
	review := PromotionReview{
		SchemaVersion:   PromotionReviewSchemaVersion,
		ID:              "route_promotion_archived_hold",
		CreatedAt:       now.Format(time.RFC3339),
		Status:          "review_required",
		Recommendation:  promotionRecommendationHoldInsufficient,
		Reason:          "samples 2 below min_samples 10",
		LaneID:          "auto/judge",
		CapabilityClass: "classify-extract",
		EvidenceRefs:    []string{"route_a"},
	}
	paths, err := reviewartifact.Write(homePath, routePromotionReviewArea, review.ID, review, " ", "# Route promotion\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordPromotionHoldReview(homePath, paths.PlanPath, "hold until the promotion criteria window is satisfied", now); err != nil {
		t.Fatalf("record hold review: %v", err)
	}
	if _, err := home.ArchiveOperationalHistory(homePath, home.OperationalArchiveOptions{Kinds: []string{"review"}}, now.Add(time.Minute)); err != nil {
		t.Fatalf("archive review: %v", err)
	}
	review.CreatedAt = now.Add(2 * time.Minute).Format(time.RFC3339)
	if _, err := reviewartifact.Write(homePath, routePromotionReviewArea, review.ID, review, " ", "# Route promotion\n"); err != nil {
		t.Fatal(err)
	}

	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: routePromotionReviewArea}, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	AnnotatePromotionReviewQueue(&report)
	reviewartifact.Recount(&report)
	if report.Counts.Total != 1 || report.Counts.ReviewRequired != 0 || report.Counts.OK != 1 {
		t.Fatalf("matching archived hold sidecar should clear review debt: %+v items=%+v", report.Counts, report.Items)
	}
	if report.Items[0].Status != "recorded" || report.Items[0].Readiness != "review_recorded" {
		t.Fatalf("matching archived hold sidecar should mark recorded: %+v", report.Items[0])
	}
}

func TestPromotionReviewQueueRejectsArchivedMismatchedHoldSidecar(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 2, 16, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatalf("init home: %v", err)
	}
	review := PromotionReview{
		SchemaVersion:   PromotionReviewSchemaVersion,
		ID:              "route_promotion_archived_mismatch",
		CreatedAt:       now.Format(time.RFC3339),
		Status:          "review_required",
		Recommendation:  promotionRecommendationHoldInsufficient,
		Reason:          "samples 2 below min_samples 10",
		LaneID:          "auto/judge",
		CapabilityClass: "classify-extract",
	}
	paths, err := reviewartifact.Write(homePath, routePromotionReviewArea, review.ID, review, " ", "# Route promotion\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordPromotionHoldReview(homePath, paths.PlanPath, "hold until the promotion criteria window is satisfied", now); err != nil {
		t.Fatalf("record hold review: %v", err)
	}
	if _, err := home.ArchiveOperationalHistory(homePath, home.OperationalArchiveOptions{Kinds: []string{"review"}}, now.Add(time.Minute)); err != nil {
		t.Fatalf("archive review: %v", err)
	}
	review.Reason = "evidence window 3 days satisfies the old hold reason no longer applies"
	if _, err := reviewartifact.Write(homePath, routePromotionReviewArea, review.ID, review, " ", "# Route promotion\n"); err != nil {
		t.Fatal(err)
	}

	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: routePromotionReviewArea}, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	AnnotatePromotionReviewQueue(&report)
	reviewartifact.Recount(&report)
	if report.Counts.Total != 1 || report.Counts.ReviewRequired != 1 || report.Counts.OK != 0 {
		t.Fatalf("mismatched archived hold sidecar should not clear review debt: %+v items=%+v", report.Counts, report.Items)
	}
}

func TestPromotionReviewQueueRoutesAuthorityRecommendationsToBriefPlan(t *testing.T) {
	tests := []struct {
		name           string
		reviewID       string
		recommendation string
	}{
		{name: "promote", reviewID: "route_promotion_promote", recommendation: "promote"},
		{name: "demote", reviewID: "route_promotion_demote", recommendation: "demote"},
		{name: "hold failing", reviewID: "route_promotion_hold_failing", recommendation: promotionRecommendationHoldFailing},
		{name: "unknown", reviewID: "route_promotion_unknown", recommendation: "future_review_kind"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := t.TempDir()
			now := time.Date(2026, 7, 9, 16, 0, 0, 0, time.UTC)
			if err := home.Init(homePath, "test", now); err != nil {
				t.Fatalf("init home: %v", err)
			}
			review := PromotionReview{
				SchemaVersion:   PromotionReviewSchemaVersion,
				ID:              tt.reviewID,
				CreatedAt:       now.Format(time.RFC3339),
				Status:          "review_required",
				Recommendation:  tt.recommendation,
				Reason:          "authority recommendation needs a decision brief",
				LaneID:          "lane/extract",
				CapabilityClass: "classify-extract",
				EvidenceRefs:    []string{"route_b"},
			}
			if _, err := reviewartifact.Write(homePath, routePromotionReviewArea, review.ID, review, " ", "# Route promotion\n"); err != nil {
				t.Fatal(err)
			}

			report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: routePromotionReviewArea}, now.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Items) != 1 {
				t.Fatalf("items = %d, want 1", len(report.Items))
			}
			before, err := os.ReadFile(report.Items[0].JSONPath)
			if err != nil {
				t.Fatal(err)
			}
			AnnotatePromotionReviewQueue(&report)
			reviewartifact.Recount(&report)
			after, err := os.ReadFile(report.Items[0].JSONPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatal("queue annotation mutated the review artifact")
			}
			item := report.Items[0]
			if item.Readiness != "needs_review" {
				t.Fatalf("readiness = %q, want needs_review", item.Readiness)
			}
			if len(item.Actions) != 1 {
				t.Fatalf("review_required promotion review should carry exactly one clean action: %+v", item.Actions)
			}
			action := item.Actions[0]
			if action.ID != "route_promotion_authority_review" || !action.RequiresReview {
				t.Fatalf("action = %+v, want id=route_promotion_authority_review requires_review=true", action)
			}
			wantArgv := []string{"compound", "reviews", "brief-plan", "--item", tt.reviewID, "--json"}
			if !reflect.DeepEqual(action.Argv, wantArgv) {
				t.Fatalf("action argv = %v, want clean proposal-only argv %v", action.Argv, wantArgv)
			}
		})
	}
}

func TestPromotionReviewQueueKeepsRecordActionForInsufficientEvidence(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 9, 16, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatalf("init home: %v", err)
	}
	review := PromotionReview{
		SchemaVersion:   PromotionReviewSchemaVersion,
		ID:              "route_promotion_insufficient_evidence",
		CreatedAt:       now.Format(time.RFC3339),
		Status:          "review_required",
		Recommendation:  promotionRecommendationHoldInsufficient,
		Reason:          "samples below promotion criteria",
		LaneID:          "lane/extract",
		CapabilityClass: "classify-extract",
		EvidenceRefs:    []string{"route_c"},
	}
	if _, err := reviewartifact.Write(homePath, routePromotionReviewArea, review.ID, review, " ", "# Route promotion\n"); err != nil {
		t.Fatal(err)
	}

	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: routePromotionReviewArea}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(report.Items))
	}
	before, err := os.ReadFile(report.Items[0].JSONPath)
	if err != nil {
		t.Fatal(err)
	}
	AnnotatePromotionReviewQueue(&report)
	reviewartifact.Recount(&report)
	after, err := os.ReadFile(report.Items[0].JSONPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("queue annotation mutated the review artifact")
	}
	item := report.Items[0]
	if len(item.Actions) != 1 {
		t.Fatalf("hold-insufficient review should carry exactly one clean action: %+v", item.Actions)
	}
	action := item.Actions[0]
	if action.ID != "route_promotion_record" || !action.RequiresReview {
		t.Fatalf("action = %+v, want id=route_promotion_record requires_review=true", action)
	}
	wantArgv := []string{"compound", "route", "promotion-record", "--review", item.JSONPath}
	if !reflect.DeepEqual(action.Argv, wantArgv) {
		t.Fatalf("action argv = %v, want clean hold-record argv %v", action.Argv, wantArgv)
	}
}
