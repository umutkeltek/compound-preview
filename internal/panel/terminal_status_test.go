package panel

import (
	"os"
	"path/filepath"
	"testing"

	"compound/internal/reviewartifact"
)

// Red-first falsifier for the panel verdict annotator's hand-copied
// terminal-status set — the next of the copies cited as follow-up debt in
// capgraph's terminal_status_test.go. Its five-word switch ("ok", "applied",
// "resolved", "accepted", "recorded") omits BOTH "candidate_recorded" and
// "completed", so a finished verdict artifact falls through to the
// needs_review branch and is billed to the operator with live decision
// actions and a member-clean action attached.
//
// Red output at this commit:
//
//	TestVerdictQueueBillsEveryTerminalWord/completed: Readiness = "needs_review", want "no_action"
//	TestVerdictQueueBillsEveryTerminalWord/candidate_recorded: Readiness = "needs_review", want "no_action"
func TestVerdictQueueBillsEveryTerminalWord(t *testing.T) {
	verdictPath := filepath.Join(t.TempDir(), "verdict.json")
	if err := os.WriteFile(verdictPath, []byte(`{"id":"verdict_terminal_fixture","synthesis":{"decision":"accept"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"ok", "applied", "resolved", "accepted", "recorded", "candidate_recorded", "completed"} {
		t.Run(status, func(t *testing.T) {
			item := &reviewartifact.QueueItem{
				Area:     verdictReviewArea,
				ID:       "verdict_" + status,
				Status:   status,
				JSONPath: verdictPath,
			}
			annotateVerdictQueueItem(item, map[string]verdictApplication{}, t.TempDir(), map[string]memoryCandidatePanelTarget{})
			if item.Readiness != "no_action" {
				t.Fatalf("verdict annotator: status %q Readiness = %q, want no_action: a terminal verdict artifact is finished work, not a pending operator decision (next_action=%q)", status, item.Readiness, item.NextAction)
			}
		})
	}
}
