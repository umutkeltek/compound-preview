package reviewartifact

import (
	"testing"
	"time"
)

// systemicReviewItem is a minimal needs-review item in its own group. Each test
// controls the group's action so the group's console-blocked reason is exactly
// what the systemic-blocker aggregation must see.
func systemicReviewItem(area, groupKey, id string, actions ...Action) QueueItem {
	return QueueItem{
		Area:      area,
		ID:        id,
		Status:    "review_required",
		Readiness: "needs_review",
		GroupKey:  groupKey,
		Actions:   actions,
	}
}

// (a) A group whose sole member carries a complete, current, allowed owner
// authority bundle exposes a group action and never counts as systemically
// blocked.
func TestSystemicBlockerAbsentWhenGroupHasValidBundle(t *testing.T) {
	signed := validReviewAuthorityAction(t, time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC), "compound://policy/effective")
	report := QueueReport{Items: []QueueItem{
		systemicReviewItem("owner-alpha", "owner-alpha|a", "item_a", signed),
	}}
	Recount(&report)

	if len(report.Groups) != 1 || report.Groups[0].SafeBatchAction == nil || report.Groups[0].TerminalReason != "" {
		t.Fatalf("valid bundle should expose a group action: %+v", report.Groups)
	}
	if got := groupConsoleBlockedReason(report.Groups[0]); got != "" {
		t.Fatalf("signed, current, allowed group must not be blocked: %q", got)
	}
	if report.SystemicBlocker != nil {
		t.Fatalf("one actionable group must not surface a systemic blocker: %+v", report.SystemicBlocker)
	}
}

// (b) A group whose member carries a partial (invalid-identity) bundle exposes
// no group action; queueGroupAction preserves the exact fail-closed reason on
// the group, and the console gate reads the partial bundle as authority-
// unavailable.
func TestSystemicBlockerPreservesInvalidAuthorityReason(t *testing.T) {
	signed := validReviewAuthorityAction(t, time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC), "compound://policy/effective")
	partial := cloneReviewAuthorityAction(signed)
	partial.PolicyDecision = nil // intent + face present, decision missing => partial bundle

	report := QueueReport{Items: []QueueItem{
		systemicReviewItem("owner-beta", "owner-beta|b", "item_b", partial),
	}}
	Recount(&report)

	group := report.Groups[0]
	if group.SafeBatchAction != nil {
		t.Fatalf("partial authority bundle must not expose a batch action: %+v", group.SafeBatchAction)
	}
	const wantTerminal = "owner authority bundle is incomplete or invalid; review item details before acting"
	if group.TerminalReason != wantTerminal {
		t.Fatalf("queueGroupAction must preserve the exact blocked reason: %q", group.TerminalReason)
	}
	if got := groupConsoleBlockedReason(group); got != systemicNoAuthorityReason {
		t.Fatalf("a partial bundle parses to nothing on the console: %q", got)
	}
}

// (c) When every group in the queue is blocked for one identical reason (here:
// no producer seeded an owner authority bundle), the projection surfaces one
// systemic blocker with the affected group count, while each group keeps its
// legacy action for non-authority consumers.
func TestSystemicBlockerPopulatedWhenAllGroupsShareOneReason(t *testing.T) {
	legacy := Action{ID: "owner_action", Label: "Apply", Command: "compound owner apply", Argv: []string{"compound", "owner", "apply"}}
	report := QueueReport{Items: []QueueItem{
		systemicReviewItem("owner-a", "owner-a|1", "item_1", legacy),
		systemicReviewItem("owner-b", "owner-b|2", "item_2", legacy),
		systemicReviewItem("owner-c", "owner-c|3", "item_3", legacy),
	}}
	Recount(&report)

	if len(report.Groups) != 3 {
		t.Fatalf("want 3 groups: %+v", report.Groups)
	}
	if report.SystemicBlocker == nil {
		t.Fatal("all groups blocked for one reason must surface a systemic blocker")
	}
	if report.SystemicBlocker.Reason != systemicNoAuthorityReason || report.SystemicBlocker.AffectedGroupCount != 3 {
		t.Fatalf("bad systemic blocker: %+v", report.SystemicBlocker)
	}
	for _, group := range report.Groups {
		if group.SafeBatchAction == nil {
			t.Fatalf("legacy action must remain exposed for non-authority consumers: %+v", group)
		}
	}
}

// (d) When the groups do not all share one reason -- here one group is
// actionable (a signed, current, allowed bundle) while another is authority-
// blocked -- no single banner can stand in for them, so the systemic blocker
// stays absent.
func TestSystemicBlockerAbsentWhenReasonsDiffer(t *testing.T) {
	signed := validReviewAuthorityAction(t, time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC), "compound://policy/effective")
	legacy := Action{ID: "owner_action", Label: "Apply", Command: "compound owner apply", Argv: []string{"compound", "owner", "apply"}}
	report := QueueReport{Items: []QueueItem{
		systemicReviewItem("owner-a", "owner-a|1", "item_1", signed), // actionable => ""
		systemicReviewItem("owner-b", "owner-b|2", "item_2", legacy), // unsigned => no-authority reason
	}}
	Recount(&report)

	if len(report.Groups) != 2 {
		t.Fatalf("want 2 groups: %+v", report.Groups)
	}
	reasons := map[string]bool{
		groupConsoleBlockedReason(report.Groups[0]): true,
		groupConsoleBlockedReason(report.Groups[1]): true,
	}
	if !reasons[""] || !reasons[systemicNoAuthorityReason] {
		t.Fatalf("fixture must mix an actionable group with a blocked one: %v", reasons)
	}
	if report.SystemicBlocker != nil {
		t.Fatalf("a mix of actionable and blocked groups must not collapse to one banner: %+v", report.SystemicBlocker)
	}
}
