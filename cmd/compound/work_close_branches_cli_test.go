package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
	"compound/internal/work"
)

// Falsifier (CLI write door): `compound work close --opens-branch slug:owner:kind`
// parses the repeatable flag (owner may itself contain colons) and lands the
// branch on the emitted transition.
func TestWorkCloseOpensBranchFlagLandsOnTransition(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := store.Create(work.CreateInput{Owner: "branch-lifecycle", SuccessPredicate: "cli close declares a branch"}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(ready.ID, "codex", cliTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(ready.ID, work.AdvanceInput{To: "running", Actor: "codex"}, cliTestNow); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdWork([]string{"close", ready.ID, "--outcome", "completed", "--move-type", "rule_delta", "--summary", "branch via CLI", "--proof", "check:cli-branch-door", "--by", "codex", "--opens-branch", "branch-lifecycle:claude:orchestrator:opens"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("close output missing JSON:\n%s", out)
	}
	var closeResult work.CloseResult
	if err := json.Unmarshal([]byte(out[start:]), &closeResult); err != nil {
		t.Fatalf("parse close output: %v\n%s", err, out)
	}
	if len(closeResult.Transition.OpensBranches) != 1 {
		t.Fatalf("transition opens_branches = %+v, want exactly one entry", closeResult.Transition.OpensBranches)
	}
	got := closeResult.Transition.OpensBranches[0]
	if got.Branch != "branch-lifecycle" || got.Owner != "claude:orchestrator" || got.Kind != "opens" {
		t.Fatalf("opens_branch entry wrong (colon-bearing owner must survive): %+v", got)
	}
}

// Falsifier (fail-fast parse): malformed --opens-branch values are rejected by
// the parser before any close runs — wrong segment count, empty slug, empty
// owner, empty kind.
func TestParseOpensBranchFlagRejectsMalformed(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{
		"",
		"branch-lifecycle",                      // missing owner+kind
		"branch-lifecycle:claude",               // missing kind
		":claude:orchestrator:opens",            // empty slug
		"branch-lifecycle::opens",               // empty owner
		"branch-lifecycle:claude:orchestrator:", // empty kind
		"branch-lifecycle:claude:orchestrator:frobnicate", // bogus kind
	} {
		if _, err := parseOpensBranchFlag(bad); err == nil {
			t.Fatalf("parseOpensBranchFlag(%q) must fail", bad)
		}
	}
	ref, err := parseOpensBranchFlag("branch-lifecycle:claude:orchestrator:opens")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Branch != "branch-lifecycle" || ref.Owner != "claude:orchestrator" || ref.Kind != "opens" {
		t.Fatalf("colon-bearing owner must parse whole: %+v", ref)
	}
}

func TestWorkCloseVerificationRejectedFlagLandsOnReceipt(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{Owner: "verification-rejection", SuccessPredicate: "verification rejects successful execution"}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "codex", cliTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "codex"}, cliTestNow); err != nil {
		t.Fatal(err)
	}
	attempts := runattempt.Open(homePath)
	ra, err := attempts.Mint(w.ID, "codex", "codex", cliTestNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.SetOrigin(ra.ID, runattempt.OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.CommitLaunch(ra.ID, cliTestNow.Add(2*time.Minute), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "verifying", Actor: "codex", RunAttemptIDs: []string{ra.ID}}, cliTestNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.Complete(ra.ID, runattempt.StatusSucceeded, cliTestNow.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(homePath, "run_attempts", ra.ID+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reason := "independent verifier rejected unsupported claims"
	out := captureStdout(t, func() {
		cmdWork([]string{"close", w.ID, "--outcome", "failed", "--move-type", "audit_only", "--verification-rejected", ra.ID, "--reason", reason, "--proof", "check:cli-verification-rejected", "--by", "codex"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("close output missing JSON: %s", out)
	}
	var result work.CloseResult
	if err := json.Unmarshal([]byte(out[start:]), &result); err != nil {
		t.Fatal(err)
	}
	if result.Work.State != "failed" || !strings.Contains(strings.Join(result.Receipt.ProofRefs, "\n"), "verification_rejected:"+ra.ID+":"+reason) {
		t.Fatalf("close result = %+v", result)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatalf("acknowledgement rewrote attempt: %v", err)
	}
}

func TestParseVerificationRejectedCloseInput(t *testing.T) {
	t.Parallel()
	args := []string{"work_example", "--outcome", "failed", "--verification-rejected", "ratt_20261004T000000Z_example", "--reason", "reviewer rejected unsupported claims"}
	requests, err := work.ParseAcknowledgeAbandonedFlagPairs(args, "")
	if err != nil {
		t.Fatal(err)
	}
	in := work.CloseInput{Outcome: "failed", RunAttemptTerminalizations: requests}
	if len(in.RunAttemptTerminalizations) != 1 {
		t.Fatalf("close requests = %v, want one", in.RunAttemptTerminalizations)
	}
	got := in.RunAttemptTerminalizations[0]
	if got.RunAttemptID != args[4] || got.Owner != "work.close.verification_rejected" || got.TargetStatus != runattempt.StatusSucceeded || got.Reason != args[6] {
		t.Fatalf("close request = %+v", got)
	}
}

func TestParseVerificationRejectedReasons(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		args      []string
		wantError string
	}{
		{name: "repeatable", args: []string{"--verification-rejected", "ratt_first", "--reason", "first rejected", "--verification-rejected", "ratt_second", "--reason", "second rejected"}},
		{name: "mixed flags", args: []string{"--acknowledge-abandoned", "ratt_first", "--reason", "first abandoned", "--verification-rejected", "ratt_second", "--reason", "second rejected"}},
		{name: "reverse mixed flags", args: []string{"--verification-rejected", "ratt_first", "--reason", "first rejected", "--acknowledge-abandoned", "ratt_second", "--reason", "second abandoned"}},
		{name: "reason cannot cross next acknowledgement", args: []string{"--verification-rejected", "ratt_first", "--acknowledge-abandoned", "ratt_second", "--reason", "second abandoned"}, wantError: "non-empty --reason"},
		{name: "abandoned reason cannot cross rejection", args: []string{"--acknowledge-abandoned", "ratt_first", "--verification-rejected", "ratt_second", "--reason", "second rejected"}, wantError: "non-empty --reason"},
		{name: "empty reason", args: []string{"--verification-rejected", "ratt_first", "--reason", " "}, wantError: "non-empty --reason"},
		{name: "missing id", args: []string{"--verification-rejected"}, wantError: "requires a following ratt_"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := work.ParseAcknowledgeAbandonedFlagPairs(tt.args, "")
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("err = %v, want %q", err, tt.wantError)
				}
				return
			}
			if err != nil || len(got) != 2 || got[0].Reason != tt.args[3] || got[1].Reason != tt.args[7] {
				t.Fatalf("requests = %+v, err = %v", got, err)
			}
		})
	}
}

func TestParseAcknowledgeAbandonedHappy(t *testing.T) {
	t.Parallel()
	got, err := work.ParseAcknowledgeAbandonedFlagPairs(
		[]string{
			"work_abc123",
			"--acknowledge-abandoned", "ratt_20260823T120000Z_aaaaaaa",
			"--reason", "mis-dispatch killed; real attempt did the work",
			"--changed",
		},
		"",
	)
	if err != nil {
		t.Fatalf("parseAcknowledgeAbandoned: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("requests = %d, want 1", len(got))
	}
	if got[0].RunAttemptID != "ratt_20260823T120000Z_aaaaaaa" {
		t.Fatalf("RunAttemptID = %q", got[0].RunAttemptID)
	}
	if got[0].Owner != runattempt.TerminalizationOwnerWorkAcknowledgeAbandoned {
		t.Fatalf("Owner = %q, want %q", got[0].Owner, runattempt.TerminalizationOwnerWorkAcknowledgeAbandoned)
	}
	if got[0].TargetStatus != runattempt.StatusAbandoned {
		t.Fatalf("TargetStatus = %q, want %q", got[0].TargetStatus, runattempt.StatusAbandoned)
	}
	if got[0].Reason != "mis-dispatch killed; real attempt did the work" {
		t.Fatalf("Reason = %q", got[0].Reason)
	}
}

func TestParseAcknowledgeAbandonedRequiresReason(t *testing.T) {
	t.Parallel()
	_, err := work.ParseAcknowledgeAbandonedFlagPairs(
		[]string{"work_abc123", "--acknowledge-abandoned", "ratt_aaaa", "--reason", ""},
		"",
	)
	if err == nil || !strings.Contains(err.Error(), "non-empty --reason") {
		t.Fatalf("err = %v, want non-empty --reason refusal", err)
	}
}

func TestParseAcknowledgeAbandonedRequiresRattID(t *testing.T) {
	t.Parallel()
	_, err := work.ParseAcknowledgeAbandonedFlagPairs(
		[]string{"work_abc123", "--acknowledge-abandoned"},
		"",
	)
	if err == nil || !strings.Contains(err.Error(), "requires a following ratt_") {
		t.Fatalf("err = %v, want requires-ratt-id refusal", err)
	}
}
