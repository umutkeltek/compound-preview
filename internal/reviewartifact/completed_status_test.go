package reviewartifact

import (
	"path/filepath"
	"testing"
	"time"

	"compound/internal/home"
)

var completedStatusTestNow = time.Date(2026, 8, 18, 2, 0, 0, 0, time.UTC)

// TestQueueCountsCompletedArtifactAsOK is the red-first falsifier for a
// terminal status that the queue's OK set forgot.
//
// The OK set (queue.go) already admits every other word this system uses for
// "finished": ok, applied, resolved, accepted, recorded, candidate_recorded.
// It omits "completed" — the most canonical terminal word in the codebase (it
// is a work outcome, a receipt outcome, and a retirement result phase). Every
// artifact carrying it therefore falls to the default branch and is counted as
// ReviewRequired, i.e. as an open decision the operator still owes.
//
// This is not hypothetical. On the live home 107 of the 191 review_required
// rows are `status: completed` worktree-retirement RESULTS — governed work
// that already ran to completion, 106 of them with worktree_removed: true.
// The operator's review backlog reads 56% larger than it is, and the phantom
// rows all land in one area, which makes that area look like the system's
// largest undrainable queue.
//
// A miscount is not a cosmetic defect here: the review queue feeds
// attention.Build, which feeds the operator-notice channel. An inflated
// backlog spends the operator's scarcest resource — decisions — on rows that
// have nothing left to decide.
func TestQueueCountsCompletedArtifactAsOK(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "test", completedStatusTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(homePath, "source-control-cleanup", "wtr_completed_fixture", map[string]any{
		"id":               "wtr_completed_fixture",
		"status":           "completed",
		"phase":            "verified",
		"worktree_removed": true,
	}, "  ", "# Worktree retirement result\n"); err != nil {
		t.Fatal(err)
	}

	report, err := Queue(homePath, QueueOptions{Area: "source-control-cleanup"}, completedStatusTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Items) != 1 {
		t.Fatalf("queue items = %d, want 1", len(report.Items))
	}
	if got := report.Items[0].Status; got != "completed" {
		t.Fatalf("item status = %q, want completed (fixture must survive the read)", got)
	}
	if report.Counts.OK != 1 {
		t.Fatalf("Counts.OK = %d, want 1: a completed artifact is finished work, not an open decision (counts=%+v)", report.Counts.OK, report.Counts)
	}
	if report.Counts.ReviewRequired != 0 {
		t.Fatalf("Counts.ReviewRequired = %d, want 0: completed work must not be billed to the operator as a pending review (counts=%+v)", report.Counts.ReviewRequired, report.Counts)
	}
}

// TestQueueOKStatusSetCoversEveryTerminalWord pins the whole OK set, so a
// future status word cannot be added to the writers while the queue silently
// keeps billing it to the operator. Each entry here is a status some producer
// in this repo actually writes to mean "finished".
func TestQueueOKStatusSetCoversEveryTerminalWord(t *testing.T) {
	terminal := []string{"ok", "applied", "resolved", "accepted", "recorded", "candidate_recorded", "completed"}
	for _, status := range terminal {
		t.Run(status, func(t *testing.T) {
			homePath := filepath.Join(t.TempDir(), ".compound")
			if err := home.Init(homePath, "test", completedStatusTestNow); err != nil {
				t.Fatal(err)
			}
			if _, err := Write(homePath, "parity", "parity_"+status, map[string]any{
				"id":     "parity_" + status,
				"status": status,
			}, "  ", "# Parity review\n"); err != nil {
				t.Fatal(err)
			}
			report, err := Queue(homePath, QueueOptions{Area: "parity"}, completedStatusTestNow)
			if err != nil {
				t.Fatal(err)
			}
			if report.Counts.OK != 1 || report.Counts.ReviewRequired != 0 {
				t.Fatalf("status %q counted as review-required: counts=%+v", status, report.Counts)
			}
		})
	}
}
