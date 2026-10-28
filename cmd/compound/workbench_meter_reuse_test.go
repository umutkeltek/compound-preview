package main

import (
	"testing"
	"time"

	"compound/internal/work"
)

// TestApplyAttentionMeterReuseMatchesFreshExecutors proves the slice-5 Workbench
// reuse in the workbench meter is output-preserving: the needs_you /
// blocked_on_you counts are identical whether the executor projection reuses the
// displayed non-archived board or builds its own. attention derives those counts
// from executor lease-hygiene and Open-derived burden, never the Recent-derived
// LastSuccess/LastFailure, so reusing a RecentLimit<8 board must not move them.
// If reuse ever fed the meter a different executor set, these counts diverge.
func TestApplyAttentionMeterReuseMatchesFreshExecutors(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.Create(work.CreateInput{WorkID: "work_held_meter", Owner: "compound-core", State: "held", SuccessPredicate: "held work needs an operator"}, cliTestNow); err != nil {
		t.Fatalf("create held: %v", err)
	}
	if _, err := store.Create(work.CreateInput{WorkID: "work_ready_meter", Owner: "compound-core", State: "ready", SuccessPredicate: "ready work can move"}, cliTestNow); err != nil {
		t.Fatalf("create ready: %v", err)
	}

	reviews, err := buildReviewQueueReport(homePath, "", false)
	if err != nil {
		t.Fatalf("review report: %v", err)
	}
	// RecentLimit 5 (< the projection's binding 8) is the important case: it is
	// what `compound workbench --json` builds by default and would be a contract
	// violation for a consumer that read LastSuccess/LastFailure; the meter does not.
	bench, err := store.Workbench(cliTestNow, work.WorkbenchOptions{RecentLimit: 5})
	if err != nil {
		t.Fatalf("workbench: %v", err)
	}

	meter := func(reuse bool) workbenchReviewQueue {
		q := buildWorkbenchReviewQueue(reviews)
		applyAttentionMeter(&q, store, reviews, bench, reuse, time.Now())
		return q
	}
	reused := meter(true)
	fresh := meter(false)
	if reused.AttentionUnavailable || fresh.AttentionUnavailable {
		t.Fatalf("attention unavailable: reused=%v fresh=%v", reused.AttentionUnavailable, fresh.AttentionUnavailable)
	}
	if reused.Counts.ReviewRequired != fresh.Counts.ReviewRequired || reused.BlockedOnYou != fresh.BlockedOnYou {
		t.Fatalf("reuse changed the meter: reused(review_required=%d blocked=%d) fresh(review_required=%d blocked=%d)",
			reused.Counts.ReviewRequired, reused.BlockedOnYou, fresh.Counts.ReviewRequired, fresh.BlockedOnYou)
	}
}
