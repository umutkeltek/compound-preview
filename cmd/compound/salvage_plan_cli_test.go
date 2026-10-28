package main

import (
	"encoding/json"
	"strings"
	"testing"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/work"
)

// F6 at the CLI door: the teeth re-proof obligation and the sanctioned
// successor sequence must be present in BOTH the --json output and the
// default human-readable output, not just in the internal Go struct.
func TestSalvagePlanCLIReportsTeethAndSuccessorSequenceInJSONAndHuman(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	repo, base, tip := initLandingCLIRepo(t)

	createOut := captureStdout(t, func() {
		cmdWork([]string{
			"create", "--owner", "codex", "--predicate", "salvage-plan CLI reports the true delta",
			"--target-worktree-root", repo, "--target-local-ref", "refs/heads/main",
			"--expected-base-sha", base, "--local-only", "--by", "codex",
		})
	})
	// `create` prints a one-line summary ("created work_x (state)") before
	// the JSON envelope; only the JSON portion unmarshals.
	_, createJSON, found := strings.Cut(createOut, "\n")
	if !found {
		t.Fatalf("create output missing summary/JSON split:\n%s", createOut)
	}
	var w envelope.WorkEnvelope
	if err := json.Unmarshal([]byte(createJSON), &w); err != nil {
		t.Fatalf("parse created WorkEnvelope: %v\n%s", err, createOut)
	}

	jsonOut := captureStdout(t, func() {
		cmdWork([]string{"salvage-plan", w.ID, "--branch", "refs/heads/candidate", "--json"})
	})
	var plan work.SalvagePlan
	if err := json.Unmarshal([]byte(jsonOut), &plan); err != nil {
		t.Fatalf("parse salvage-plan JSON: %v\n%s", err, jsonOut)
	}
	// initLandingCLIRepo never advances main past base, and the candidate
	// branch forked exactly at base: main tip is base, candidate tip is tip.
	if plan.TargetSHA != base {
		t.Fatalf("plan.target_sha = %s, want main tip (base) %s", plan.TargetSHA, base)
	}
	if plan.BranchSHA != tip {
		t.Fatalf("plan.branch_sha = %s, want candidate tip %s", plan.BranchSHA, tip)
	}
	if !strings.Contains(jsonOut, `"teeth_reproof_note"`) {
		t.Fatalf("--json output missing teeth_reproof_note field:\n%s", jsonOut)
	}
	if !strings.Contains(jsonOut, `"successor_commands"`) {
		t.Fatalf("--json output missing successor_commands field:\n%s", jsonOut)
	}
	// Checked on the DECODED struct, not the raw JSON text: encoding/json
	// HTML-escapes < and > in string values by default (printJSON uses
	// json.MarshalIndent), so the placeholder's literal angle brackets do
	// not survive raw-text search even though the decoded value is exact.
	wantWorktreeAdd := "git worktree add -b land/" + w.ID + " <fresh-worktree-path> " + base
	if len(plan.SuccessorCommands) == 0 || plan.SuccessorCommands[0] != wantWorktreeAdd {
		t.Fatalf("--json output missing the sanctioned worktree-add step: %+v", plan.SuccessorCommands)
	}

	humanOut := captureStdout(t, func() {
		cmdWork([]string{"salvage-plan", w.ID, "--branch", "refs/heads/candidate"})
	})
	if !strings.Contains(humanOut, "teeth_reproof:") {
		t.Fatalf("human output missing teeth_reproof line:\n%s", humanOut)
	}
	if !strings.Contains(humanOut, "successor_command: "+wantWorktreeAdd) {
		t.Fatalf("human output missing successor_command worktree-add line:\n%s", humanOut)
	}
	if !strings.Contains(humanOut, "successor_command: git cherry-pick") {
		t.Fatalf("human output missing successor_command cherry-pick line:\n%s", humanOut)
	}
	if strings.Contains(humanOut, `"schema_version"`) {
		t.Fatalf("default (non --json) output must be human-readable, not raw JSON:\n%s", humanOut)
	}
}
