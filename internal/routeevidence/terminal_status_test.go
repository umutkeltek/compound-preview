package routeevidence

import (
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/reviewartifact"
)

// Red-first falsifier for the route-promotion review annotator's hand-copied
// terminal-status set — the next of the copies cited as follow-up debt in
// capgraph's terminal_status_test.go. Its five-word switch omits BOTH
// "candidate_recorded" and "completed", so a finished promotion review falls
// to the default branch: needs_review, needs_you band, "owner review required
// for route promotion artifact" — an operator billed for a decision already
// made.
//
// Red output at this commit:
//
//	TestPromotionReviewQueueBillsEveryTerminalWord/completed: Readiness = "needs_review", want "no_action"
//	TestPromotionReviewQueueBillsEveryTerminalWord/candidate_recorded: Readiness = "needs_review", want "no_action"
func TestPromotionReviewQueueBillsEveryTerminalWord(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	for _, status := range []string{"ok", "applied", "resolved", "accepted", "recorded", "candidate_recorded", "completed"} {
		t.Run(status, func(t *testing.T) {
			homePath := t.TempDir()
			if err := home.Init(homePath, "test", now); err != nil {
				t.Fatal(err)
			}
			review := PromotionReview{
				SchemaVersion:   PromotionReviewSchemaVersion,
				ID:              "route_promotion_terminal_" + status,
				CreatedAt:       now.Format(time.RFC3339),
				Status:          status,
				Recommendation:  promotionRecommendationPromote,
				Reason:          "fixture reason",
				LaneID:          "auto/judge",
				CapabilityClass: "classify-extract",
			}
			if _, err := reviewartifact.Write(homePath, routePromotionReviewArea, review.ID, review, " ", "# Route promotion\n"); err != nil {
				t.Fatal(err)
			}
			report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: routePromotionReviewArea}, now.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			AnnotatePromotionReviewQueue(&report)
			if len(report.Items) != 1 {
				t.Fatalf("queue items = %d, want 1", len(report.Items))
			}
			if got := report.Items[0].Readiness; got != "no_action" {
				t.Fatalf("promotion review annotator: status %q Readiness = %q, want no_action: a terminal review is finished work, not a pending operator decision (next_action=%q)", status, got, report.Items[0].NextAction)
			}
		})
	}
}
