package reviewartifact

import "testing"

func TestAuthorizationRecordedIsTerminalAcrossQueueClassifiers(t *testing.T) {
	const status = "authorization_recorded"
	if !terminalResolutionStatus(status) {
		t.Fatalf("%q must reject generic resolution as an owner-terminal status", status)
	}
	item := QueueItem{Status: status, Readiness: "no_action"}
	if itemNeedsReviewGroup(item) {
		t.Fatalf("%q must not reopen a review group", status)
	}
	if got := statusPriority(status); got != 3 {
		t.Fatalf("status priority = %d, want terminal priority 3", got)
	}
	report := QueueReport{Items: []QueueItem{item}}
	Recount(&report)
	if report.Counts.OK != 1 || report.Counts.ReviewRequired != 0 {
		t.Fatalf("terminal counts = %+v", report.Counts)
	}
}
