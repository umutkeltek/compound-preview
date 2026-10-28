package product

import (
	"testing"

	"compound/internal/reviewartifact"
)

// Red-first falsifiers for the product package's two hand-copied
// terminal-status sets — the next of the copies cited as follow-up debt in
// capgraph's terminal_status_test.go. Both five-word switches ("ok",
// "applied", "resolved", "accepted", "recorded") omit "candidate_recorded"
// and "completed", so a finished product review falls to the default branch,
// is marked needs_review, and — with no review YAML draft on the item — is
// then marked blocked: finished work billed to the operator as a missing
// draft.
//
// Red output at this commit:
//
//	TestProductQueueBillsEveryTerminalWord/review_completed: Readiness = "blocked", want "no_action"
//	TestProductQueueBillsEveryTerminalWord/action_completed: Readiness = "blocked", want "no_action"
//	(same for candidate_recorded)
func TestProductQueueBillsEveryTerminalWord(t *testing.T) {
	for _, status := range []string{"ok", "applied", "resolved", "accepted", "recorded", "candidate_recorded", "completed"} {
		t.Run("review_"+status, func(t *testing.T) {
			item := &reviewartifact.QueueItem{Area: productReviewArea, ID: "review_" + status, Status: status}
			annotateProductReviewItem(item)
			if item.Readiness != "no_action" {
				t.Fatalf("product review annotator: status %q Readiness = %q, want no_action: a terminal review is finished work, not a missing draft (next_action=%q)", status, item.Readiness, item.NextAction)
			}
		})
		t.Run("action_"+status, func(t *testing.T) {
			item := &reviewartifact.QueueItem{Area: ProductActionReviewArea, ID: "action_" + status, Status: status}
			annotateProductActionReviewItem(item)
			if item.Readiness != "no_action" {
				t.Fatalf("product action review annotator: status %q Readiness = %q, want no_action: a terminal review is finished work, not a missing draft (next_action=%q)", status, item.Readiness, item.NextAction)
			}
		})
	}
}
