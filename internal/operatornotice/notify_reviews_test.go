package operatornotice

// Red-first falsifier for the frontier the notifier can actually see.
//
// attention.Build walks review groups only when Inputs.Reviews is non-nil, and
// Notify built its inputs from Workbench + Executors alone. Every review
// artifact — 449 of them on the live home as of 2026-08-18, 323 in
// source-control-cleanup — was therefore structurally unreachable by the one
// component whose job is telling the operator a hold exists.
//
// That is not a theoretical gap. The single flush this system has ever run
// (2026-08-06) emitted exactly one card, reading "33 open work items are
// blocked on operator review" — the workbench group, because it was the only
// group the notifier could see.

import (
	"testing"

	"compound/internal/reviewartifact"
)

// reviewQueueWithNeedsYou builds a minimal annotated review queue carrying one
// needs-you group, the shape reviewqueue.Build produces for a real review area.
func reviewQueueWithNeedsYou(area, itemID string) *reviewartifact.QueueReport {
	return &reviewartifact.QueueReport{
		SchemaVersion: reviewartifact.QueueSchemaVersion,
		GeneratedAt:   fixedNow.Format("2006-01-02T15:04:05Z"),
		Groups: []reviewartifact.QueueGroup{{
			Key:             "reviews|" + area,
			Area:            area,
			Band:            reviewartifact.BandNeedsYou,
			Count:           1,
			OperatorSummary: area + " has an item past its defer trigger. Review it?",
			ItemIDs:         []string{itemID},
		}},
	}
}

// TestNotifyWithReviewsReachesReviewFrontier proves the review frontier is
// reachable at all: a needs-you review group must be scanned and asked about.
//
// RED before the wiring: Notify passed no Reviews, attention.Build skipped
// review groups entirely, and Scanned came back 0 no matter how many review
// artifacts existed.
func TestNotifyWithReviewsReachesReviewFrontier(t *testing.T) {
	homePath := seedHeldHome(t, "work_review_frontier")
	withFakeExecute(t, "rec_reviews", "tr_reviews")

	queue := reviewQueueWithNeedsYou("capability-lifecycle-proposals", "work_review_frontier")

	got, err := NotifyWithReviews(homePath, "buzz", queue, fixedNow)
	if err != nil {
		t.Fatalf("notify with reviews: %v", err)
	}
	if got.Scanned == 0 {
		t.Fatalf("the review frontier was not scanned at all: %+v", got)
	}
	if got.Emitted != 1 {
		t.Fatalf("emitted = %d, want 1 ask from the review frontier: %+v", got.Emitted, got)
	}
}

// TestNotifyWithoutReviewsMissesReviewFrontier pins the contract from the other
// side, and is the regression guard that keeps the wiring honest: a nil review
// queue degrades to workbench-only rather than failing, so the fix must be the
// caller passing the queue, never this package quietly inventing one.
func TestNotifyWithoutReviewsMissesReviewFrontier(t *testing.T) {
	homePath := seedHeldHome(t, "work_no_reviews")
	withFakeExecute(t, "rec_none", "tr_none")

	// The same review area exists conceptually, but is not supplied.
	unsupplied := reviewQueueWithNeedsYou("capability-lifecycle-proposals", "work_no_reviews")
	holdID := HoldIDFor(needsYouGroup("capability-lifecycle-proposals", "work_no_reviews", false))
	_ = unsupplied

	if _, err := NotifyWithReviews(homePath, "buzz", nil, fixedNow); err != nil {
		t.Fatalf("notify without reviews: %v", err)
	}

	// Nothing was asked about the review area, because nothing supplied it.
	// This is the honest degradation: the package never fabricates a frontier
	// it was not given, so the only fix is the caller passing the queue.
	found, rec, err := findPriorNotification(homePath, holdID)
	if err != nil {
		t.Fatalf("read notice record: %v", err)
	}
	if found {
		t.Errorf("a review hold was notified without any review queue being supplied: %+v", rec)
	}
}
