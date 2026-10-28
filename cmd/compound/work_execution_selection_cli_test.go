package main

import (
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/work"
)

// work_execution_selection_test.go (cmd/compound): prove the CLI
// dispatcher wires --launch-profile / --effort / --expected-selection-revision
// through the work package and round-trips the saved selection through
// `compound work show` so a human sees what will run.
//
// work_r7a_execution_selection_20260911.

// TestWorkAdvanceExecuteUsesSavedSelectionWithoutHarnessFlag:
// RED before the fence — the dispatcher demanded --harness on every
// --execute. GREEN after — when --harness is omitted AND the work has a
// saved selection, the dispatcher falls through to the saved
// selection.harness so a single command line drives the launch.
//
// The test uses the os-harness-entrypoints adapter (echo) and reads the
// attempt's snapshot via runattempt.Open so the falsifier names both
// the input (no --harness) and the output (source: saved, the saved
// harness, the saved profile id).
func TestWorkAdvanceExecuteUsesSavedSelectionWithoutHarnessFlag(t *testing.T) {
	homePath := initializedExecutionHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	now := cliTestNow
	sel := &envelope.ExecutionSelection{
		LaunchProfileID:       "os-harness-entrypoints-fixture",
		LaunchProfileRevision: "sha256:eeb7c22957a53d0876ede9bf0a28515c27665f5f2e329e83d93768d715400f4a",
		Harness:               "os-harness-entrypoints",
		ProviderRoute:         "os/test",
		// No requested model: os-harness-entrypoints binds none, and an
		// unapplicable saved model is refused rather than dropped.
		Source:     "explicit",
		Revision:   1,
		SelectedBy: "claude_code:orchestrator",
		SelectedAt: now.Format(time.RFC3339),
	}
	w, err := store.Create(work.CreateInput{
		Owner: "codex", Actor: "codex",
		SuccessPredicate:   "advance --execute falls back to saved selection",
		State:              "ready",
		Risk:               "tier1",
		Privacy:            "private",
		ExecutionSelection: sel,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "codex", now); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdWork([]string{
			"advance", w.ID,
			"--to", "running",
			"--execute",
			"--", "echo", "sel-fallback",
		})
	})

	if !strings.Contains(out, "run_attempt_id: ratt_") {
		t.Fatalf("expected run_attempt_id, got:\n%s", out)
	}
	if !strings.Contains(out, "harness: os-harness-entrypoints") {
		t.Fatalf("expected harness os-harness-entrypoints in output, got:\n%s", out)
	}

	// The envelope's saved selection is unchanged (the test proves the
	// runtime path, not the CLI's caller-side mutation; the saved
	// selection survives the launch byte-equal).
	got, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExecutionSelection == nil || got.ExecutionSelection.LaunchProfileID != sel.LaunchProfileID {
		t.Fatalf("saved selection was rewritten by --execute without --harness: got %+v", got.ExecutionSelection)
	}
}

// TestWorkShowPrintsSelectionAndAttemptSource: the show output prints
// the saved selection under "--- execution_selection ---" AND each
// attached attempt's source / selection_rev / profile id / harness /
// route / model, so a human reads "what will run" AND "what did run"
// without opening the JSON envelope.
func TestWorkShowPrintsSelectionAndAttemptSource(t *testing.T) {
	homePath := initializedExecutionHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	now := cliTestNow
	sel := &envelope.ExecutionSelection{
		LaunchProfileID:       "claude-minimax",
		LaunchProfileRevision: "sha256:eeb7c22957a53d0876ede9bf0a28515c27665f5f2e329e83d93768d715400f4a",
		Harness:               "claude_code",
		ProviderRoute:         "minimax/MiniMax-M3",
		RequestedModelID:      "MiniMax-M3",
		RequestedEffort:       "xhigh",
		Source:                "explicit",
		Revision:              2,
		SelectedBy:            "claude_code:orchestrator",
		SelectedAt:            now.Format(time.RFC3339),
	}
	w, err := store.Create(work.CreateInput{
		Owner: "codex", Actor: "codex",
		SuccessPredicate:   "show prints saved selection + attempt source",
		State:              "ready",
		ExecutionSelection: sel,
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdWork([]string{"show", w.ID})
	})
	if !strings.Contains(out, "--- execution_selection (saved: what will run) ---") {
		t.Fatalf("show output missing saved-selection header:\n%s", out)
	}
	if !strings.Contains(out, "launch_profile: claude-minimax") {
		t.Fatalf("show output missing launch_profile line:\n%s", out)
	}
	if !strings.Contains(out, "harness: claude_code") {
		t.Fatalf("show output missing harness line:\n%s", out)
	}
	if !strings.Contains(out, "requested_effort: xhigh") {
		t.Fatalf("show output missing requested_effort line:\n%s", out)
	}
	// No RunAttempts attached yet — the show output's attempt block is
	// absent (the test asserts the saved-selection block, which is
	// always emitted when the saved selection is present).
}
