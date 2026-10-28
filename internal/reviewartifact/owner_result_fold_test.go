package reviewartifact

import (
	"strings"
	"testing"
	"time"

	"compound/internal/home"
)

var ownerResultFoldTestNow = time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)

// TestQueueFoldsOwnerTerminalResultIntoPlan is the red-first falsifier for a
// plan artifact that outlives its own completion.
//
// A worktree-retirement plan is written once, with `status: review_required`
// frozen into the file. The owner then reviews it (`<stem>.<review>.reviewed.json`)
// and runs it (`<stem>.result.json`, `status: completed`, `worktree_removed:
// true`). The result is a separate core artifact and is counted OK; the plan
// keeps its birth status forever, because nothing folds the owner's later
// result back onto it. The generic resolution sidecar cannot help: it expects
// `<stem>.reviewed.json` with a `rres_` schema, and the owner writes neither.
//
// On the live home this is 651 completed retirements (plus 37
// source-control-cleanup plans of the same shape) surfacing as
// `needs_you` in one "a new record needs your read" group with no action —
// 85% of everything the operator was told they owed a decision on, all of it
// already decided and already done.
//
// The rule this test pins: an owner-written terminal result on the same stem
// terminalizes the plan. Only a plan with no result stays open.
func TestQueueFoldsOwnerTerminalResultIntoPlan(t *testing.T) {
	homePath := t.TempDir() + "/.compound"
	if err := home.Init(homePath, "test", ownerResultFoldTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteJSON(homePath, "worktree-retirement", "wtr_fixture.retirement.json", map[string]any{
		"schema_version": "compound.worktree_retirement_plan.v0.1",
		"id":             "wtr_fixture",
		"status":         "review_required",
		"created_at":     "2026-08-26T19:41:00Z",
	}, "  "); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteJSON(homePath, "worktree-retirement", "wtr_fixture.retirement.result.json", map[string]any{
		"schema_version":   "compound.worktree_retirement_result.v0.1",
		"id":               "wtrres_fixture",
		"plan_id":          "wtr_fixture",
		"status":           "completed",
		"phase":            "verified",
		"worktree_removed": true,
		"created_at":       "2026-08-26T19:41:18Z",
	}, "  "); err != nil {
		t.Fatal(err)
	}

	report, err := Queue(homePath, QueueOptions{Area: "worktree-retirement"}, ownerResultFoldTestNow)
	if err != nil {
		t.Fatal(err)
	}
	plan := findQueueItemByPathSuffix(t, report, "wtr_fixture.retirement.json")
	if plan.Status != "completed" {
		t.Fatalf("plan status = %q, want completed: the owner's terminal result on the same stem must fold onto the plan", plan.Status)
	}
	if plan.Band != BandFiled {
		t.Fatalf("plan band = %q, want %q", plan.Band, BandFiled)
	}
	if len(plan.Actions) != 0 {
		t.Fatalf("plan actions = %d, want 0: nothing is left to decide", len(plan.Actions))
	}
	if !strings.Contains(plan.NextAction, "wtrres_fixture") {
		t.Fatalf("plan next action = %q, want it to name the owner result that finished it", plan.NextAction)
	}
	if report.Counts.ReviewRequired != 0 {
		t.Fatalf("Counts.ReviewRequired = %d, want 0 (counts=%+v)", report.Counts.ReviewRequired, report.Counts)
	}
	for _, group := range report.Groups {
		if group.Area == "worktree-retirement" && group.Count > 0 {
			t.Fatalf("a completed retirement still forms a review group: %+v", group)
		}
	}
}

// TestQueueKeepsPlanOpenWithoutTerminalOwnerResult pins the other edge: a
// result that is not terminal (still running) and a plan with no result at all
// both stay open. The fold must never hide a retirement that has not happened.
func TestQueueKeepsPlanOpenWithoutTerminalOwnerResult(t *testing.T) {
	homePath := t.TempDir() + "/.compound"
	if err := home.Init(homePath, "test", ownerResultFoldTestNow); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"wtr_running", "wtr_unrun"} {
		if _, err := WriteJSON(homePath, "worktree-retirement", id+".retirement.json", map[string]any{
			"schema_version": "compound.worktree_retirement_plan.v0.1",
			"id":             id,
			"status":         "review_required",
			"created_at":     "2026-08-26T19:41:00Z",
		}, "  "); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := WriteJSON(homePath, "worktree-retirement", "wtr_running.retirement.result.json", map[string]any{
		"schema_version": "compound.worktree_retirement_result.v0.1",
		"id":             "wtrres_running",
		"plan_id":        "wtr_running",
		"status":         "running",
		"created_at":     "2026-08-26T19:41:18Z",
	}, "  "); err != nil {
		t.Fatal(err)
	}

	report, err := Queue(homePath, QueueOptions{Area: "worktree-retirement"}, ownerResultFoldTestNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"wtr_running.retirement.json", "wtr_unrun.retirement.json"} {
		plan := findQueueItemByPathSuffix(t, report, suffix)
		if plan.Status != "review_required" {
			t.Fatalf("%s status = %q, want review_required: no terminal owner result exists", suffix, plan.Status)
		}
	}
}

func findQueueItemByPathSuffix(t *testing.T, report QueueReport, suffix string) QueueItem {
	t.Helper()
	for _, item := range report.Items {
		if strings.HasSuffix(item.JSONPath, "/"+suffix) {
			return item
		}
	}
	t.Fatalf("no queue item with path suffix %q among %d items", suffix, len(report.Items))
	return QueueItem{}
}
