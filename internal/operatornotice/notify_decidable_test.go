package operatornotice

// Red-first falsifiers for the decidability defect.
//
// Measured 2026-08-18: `notice flush` has run exactly once in the life of this
// home (2026-08-06, opnotice_2ae5f8c4d7dd884903c1f365). It delivered, and it
// decided nothing — 33 distinct holds collapsed into one card whose only
// "approve" line re-posts the work to the same transport that carried it.
//
// Two independent causes, one falsifier each:
//
//   - cardBody discards Group.Action (the policy-stamped argv that review
//     groups already carry) and fabricates an `act post` line instead. The
//     operator receives a card with no decision on it.
//   - HoldIDFor hashes the full ItemIDs set and OldestCreatedAt, so the id
//     changes whenever any item joins or leaves the group. Idempotency is
//     therefore only stable for a frozen frontier — and a real frontier is
//     never frozen. This is why the edge cannot be put on a schedule as-is:
//     every flush would re-post the same holds under fresh ids.
//
// Both tests are RED on the current implementation.

import (
	"strings"
	"testing"

	"compound/internal/attention"
)

// TestCardBodyCarriesDecidableAction pins the predicate "a notice the operator
// can act on". When the group carries a policy-stamped action, the card must
// hand the operator THAT command. The fabricated `act post --from-work` line
// is not a decision — it re-posts the notification — so it must be gone.
//
// RED today: cardBody ignores g.Action entirely (notify.go:348-359) and always
// emits the act post line (notify.go:357).
func TestCardBodyCarriesDecidableAction(t *testing.T) {
	argv := []string{"compound", "reviews", "resolve", "--item", "clp_e95dd4e88699", "--area", "capability-lifecycle-proposals"}
	g := attention.Group{
		Area:            "capability-lifecycle-proposals",
		Band:            attention.BandNeedsYou,
		Source:          attention.SourceReviews,
		Kind:            "adjudicate",
		OperatorSummary: "capability lifecycle proposal is past its defer trigger. Review it?",
		ItemIDs:         []string{"clp_e95dd4e88699"},
		Action: &attention.Action{
			ID:    "review-resolve-clp_e95dd4e88699",
			Label: "Resolve this review",
			Argv:  argv,
			Kind:  "review.resolve",
		},
	}

	body := cardBody(g, "opnotice_test", "work_test", "buzz")

	want := strings.Join(argv, " ")
	if !strings.Contains(body, want) {
		t.Errorf("card body does not carry the group's decidable command.\nwant substring: %s\ngot body:\n%s", want, body)
	}
	if strings.Contains(body, "act post") {
		t.Errorf("card body still carries the fabricated act post line, which decides nothing.\ngot body:\n%s", body)
	}
}

// TestCardBodyFallsBackWhenNoAction keeps the change honest: a group with no
// action must NOT silently gain a fake one. It states plainly that there is no
// command, so the decidability gate can count it as unnotifiable rather than
// shipping a card the operator cannot act on.
func TestCardBodyFallsBackWhenNoAction(t *testing.T) {
	g := attention.Group{
		Area:            "work",
		Band:            attention.BandNeedsYou,
		Source:          "workbench",
		Kind:            "review",
		OperatorSummary: "3 open work items are blocked on operator review.",
		ItemIDs:         []string{"work_a", "work_b", "work_c"},
	}

	body := cardBody(g, "opnotice_test", "work_a", "buzz")

	if strings.Contains(body, "act post") {
		t.Errorf("actionless card must not fabricate an approve command.\ngot body:\n%s", body)
	}
	if !strings.Contains(body, "ask=") {
		t.Errorf("actionless card must still carry the operator ask.\ngot body:\n%s", body)
	}
}

// TestHoldIDStableAcrossItemSetChange is the falsifier that gates scheduling.
//
// A hold is the same hold when it is the same area/kind/source/action, even as
// items join and leave it. Today the id is content-addressed over the full
// ItemIDs set plus OldestCreatedAt, so a group that gains one item resolves to
// a NEW id, findPriorNotification misses, and the operator is notified again
// about a hold they have already seen.
//
// On a manual verb that is a nuisance. On a schedule it is a notification loop,
// which is exactly why this must land before `notice flush` is scheduled.
//
// RED today: notify.go:289-295 hashes ids and OldestCreatedAt.
func TestHoldIDStableAcrossItemSetChange(t *testing.T) {
	action := &attention.Action{
		ID:   "review-resolve-source-control-cleanup",
		Argv: []string{"compound", "reviews", "resolve", "--area", "source-control-cleanup"},
	}
	before := attention.Group{
		Area:            "source-control-cleanup",
		Band:            attention.BandNeedsYou,
		Source:          attention.SourceReviews,
		Kind:            "adjudicate",
		ItemIDs:         []string{"scc_001", "scc_002"},
		OldestCreatedAt: "2026-08-01T00:00:00Z",
		Action:          action,
	}
	// Same hold, one more item, and the oldest item is now older. Nothing
	// about WHAT the operator must decide has changed.
	after := attention.Group{
		Area:            "source-control-cleanup",
		Band:            attention.BandNeedsYou,
		Source:          attention.SourceReviews,
		Kind:            "adjudicate",
		ItemIDs:         []string{"scc_001", "scc_002", "scc_003"},
		OldestCreatedAt: "2026-07-28T00:00:00Z",
		Action:          action,
	}

	if got, want := HoldIDFor(after), HoldIDFor(before); got != want {
		t.Errorf("hold id changed when the item set changed, so a scheduled flush would re-notify a hold the operator already saw.\nbefore: %s\nafter:  %s", want, got)
	}
}

// TestHoldIDDistinguishesDifferentHolds is the guard against over-correcting:
// collapsing the id must not merge two genuinely different decisions into one.
func TestHoldIDDistinguishesDifferentHolds(t *testing.T) {
	a := attention.Group{
		Area:   "source-control-cleanup",
		Source: attention.SourceReviews,
		Kind:   "adjudicate",
		Action: &attention.Action{ID: "review-resolve-source-control-cleanup"},
	}
	b := attention.Group{
		Area:   "route-promotions",
		Source: attention.SourceReviews,
		Kind:   "adjudicate",
		Action: &attention.Action{ID: "review-resolve-route-promotions"},
	}
	if HoldIDFor(a) == HoldIDFor(b) {
		t.Errorf("two different holds collapsed to one id: %s", HoldIDFor(a))
	}
}
