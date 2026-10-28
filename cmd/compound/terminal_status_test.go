package main

import (
	"testing"

	"compound/internal/reviewartifact"
)

// Red-first falsifiers for the last two hand-copied terminal-status sets
// outside the owner — cited as follow-up debt in capgraph's
// terminal_status_test.go. sourceControlReviewItemRecorded and
// adaptationReviewItemRecorded each carry a FOUR-word status switch
// ("recorded", "applied", "resolved", "accepted") that omits "ok",
// "candidate_recorded", and "completed". An orient projection that asks
// "is this review item recorded?" gets false for finished work — most
// visibly the completed worktree-retirement results in source-control
// cleanup, which then read as open orient debt.
//
// Red output at this commit (both functions, three words):
//
//	TestReviewItemRecordedAdmitsEveryTerminalWord/ok: sourceControlReviewItemRecorded = false, want true
//	TestReviewItemRecordedAdmitsEveryTerminalWord/candidate_recorded: ... = false, want true
//	TestReviewItemRecordedAdmitsEveryTerminalWord/completed: ... = false, want true
func TestReviewItemRecordedAdmitsEveryTerminalWord(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"ok", "applied", "resolved", "accepted", "recorded", "candidate_recorded", "completed"} {
		t.Run(status, func(t *testing.T) {
			item := reviewartifact.QueueItem{Status: status}
			if !sourceControlReviewItemRecorded(item) {
				t.Fatalf("sourceControlReviewItemRecorded: status %q = false, want true: a terminal status means the review is recorded, not open orient debt", status)
			}
			if !adaptationReviewItemRecorded(item) {
				t.Fatalf("adaptationReviewItemRecorded: status %q = false, want true: a terminal status means the review is recorded, not open orient debt", status)
			}
		})
	}
}
