package operatornotice

// Red-first falsifiers for how the operator channel is SERVED.
//
// With no cap, one flush over a real frontier emits a notification per
// needs-you group — 18 groups on the live home as of 2026-08-18. Eighteen
// simultaneous asks is not eighteen decisions; it is one ignored batch. The
// scarce resource is decisions, not inbox space, so the queue is served one at
// a time and the ask that gets the channel must be one the operator can act on.
//
// NOT covered here: closure (how many asks were ANSWERED, and how long they
// waited). That needs to observe holds which have LEFT the frontier, and the
// event store is immutable — a record cannot be restamped resolved, and
// enumerating departed holds needs either a Scan on the unmapped "opnotice_"
// prefix (which disables routing and parses every segment) or a declared
// projection path. Both are separate design calls. Occupancy below is derived
// from the current frontier instead, which needs no stored state.
//
// Both tests are RED on the current implementation.

import (
	"testing"

	"compound/internal/attention"
)

// needsYouGroup builds one BandNeedsYou group directly, so these tests control
// the frontier exactly rather than inferring it from a seeded workbench.
func needsYouGroup(area, itemID string, decidable bool) attention.Group {
	g := attention.Group{
		Key:             "test|" + area,
		Area:            area,
		Band:            attention.BandNeedsYou,
		Source:          attention.SourceReviews,
		Kind:            "adjudicate",
		Count:           1,
		OperatorSummary: area + " needs an operator decision.",
		ItemIDs:         []string{itemID},
		OldestCreatedAt: "2026-08-01T00:00:00Z",
	}
	if decidable {
		g.Action = &attention.Action{
			ID:   "review-resolve-" + area,
			Argv: []string{"compound", "reviews", "resolve", "--area", area},
		}
	}
	return g
}

func reportWith(groups ...attention.Group) attention.Report {
	return attention.Report{Groups: groups}
}

// TestNotifyEmitsAtMostMaxOpen pins the serialization promise: an unanswered
// hold occupies the channel until it is answered. Three simultaneous holds
// produce one ask and two queued, not three asks.
//
// RED today: the loop emits one notification per needs-you group with no cap.
func TestNotifyEmitsAtMostMaxOpen(t *testing.T) {
	homePath := seedHeldHome(t, "work_q1", "work_q2", "work_q3")
	withFakeExecute(t, "rec_q", "tr_q")

	in := reportWith(
		needsYouGroup("area-one", "work_q1", true),
		needsYouGroup("area-two", "work_q2", true),
		needsYouGroup("area-three", "work_q3", true),
	)

	got, err := NotifyFromInputs(homePath, "buzz", in, fixedNow)
	if err != nil {
		t.Fatalf("notify: %v", err)
	}
	if got.Emitted != 1 {
		t.Errorf("emitted %d holds at once; the operator funnel is one decision at a time: %+v", got.Emitted, got)
	}
	if got.Queued != 2 {
		t.Errorf("queued = %d, want 2 (the holds that did not get the channel): %+v", got.Queued, got)
	}

	// A queued hold is NOT notified, so it must remain eligible: nothing was
	// persisted for it and the next flush can still reach it.
	if _, rec, _ := findPriorNotification(homePath, HoldIDFor(in.Groups[2])); rec != nil {
		t.Errorf("a queued hold wrote a record, so it can never be notified later: %+v", rec)
	}
}

// TestNotifyPrefersDecidableHolds guards the interaction between the cap and
// decidability: when only one ask fits, it must be one the operator can act
// on. An undecidable hold winning the channel starves every decidable one.
//
// RED today: order is frontier order, and decidability is not consulted.
func TestNotifyPrefersDecidableHolds(t *testing.T) {
	homePath := seedHeldHome(t, "work_undecidable", "work_decidable")
	withFakeExecute(t, "rec_pref", "tr_pref")

	undecidable := needsYouGroup("workbench-holds", "work_undecidable", false)
	decidable := needsYouGroup("route-promotions", "work_decidable", true)

	// Undecidable first in frontier order — the cap must still pick the other.
	got, err := NotifyFromInputs(homePath, "buzz", reportWith(undecidable, decidable), fixedNow)
	if err != nil {
		t.Fatalf("notify: %v", err)
	}
	if got.Emitted != 1 {
		t.Fatalf("emitted = %d, want 1: %+v", got.Emitted, got)
	}

	_, rec, err := findPriorNotification(homePath, HoldIDFor(decidable))
	if err != nil {
		t.Fatalf("read notice record: %v", err)
	}
	if rec == nil || rec.Outcome != "emitted" {
		t.Errorf("the decidable hold did not get the channel; an undecidable ask consumed it: %+v", rec)
	}
}
