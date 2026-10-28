package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func TestPrintWorkCloseResultSurfacesUnterminalizedRunAttempts(t *testing.T) {
	result := work.CloseResult{UnterminalizedRunAttemptIDs: []string{
		"ratt_20260820T000000Z_aabbccdd",
		"ratt_20260820T000001Z_eeff0011",
	}}
	out := captureStdout(t, func() { printWorkCloseResult(result) })
	if !strings.Contains(out, "unterminalized_run_attempts=ratt_20260820T000000Z_aabbccdd,ratt_20260820T000001Z_eeff0011") ||
		!strings.Contains(out, "compound work reconcile-run-attempts") {
		t.Fatalf("close output hid the post-publication recovery obligation:\n%s", out)
	}
}

func seedPreparedRunAttemptReconcileCLIWork(t *testing.T, homePath string) (string, string) {
	t.Helper()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	w, err := store.Create(work.CreateInput{
		Owner:            "compound-panel",
		SuccessPredicate: "public reconciliation repairs an exact prepared RunAttempt",
	}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	claimed, err := store.Claim(w.ID, "compound-panel", cliTestNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("claim work: %v", err)
	}
	if len(claimed.RunAttemptIDs) == 0 {
		t.Fatal("claim did not mint a RunAttempt")
	}
	runAttemptID := claimed.RunAttemptIDs[0]
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "compound-panel"}, cliTestNow.Add(2*time.Minute)); err != nil {
		t.Fatalf("advance running: %v", err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "verifying", Actor: "compound-panel", VerificationID: "ver_" + w.ID}, cliTestNow.Add(3*time.Minute)); err != nil {
		t.Fatalf("advance verifying: %v", err)
	}
	if _, err := store.Close(w.ID, work.CloseInput{
		Outcome:                       "completed",
		MoveType:                      "control_plane_repair",
		Actor:                         "compound-panel",
		Summary:                       "leave exact caller-owned terminalization prepared for CLI recovery",
		ProofRefs:                     []string{"check:reconcile-run-attempts-cli"},
		SkipRunAttemptTerminalization: true,
		RunAttemptTerminalizations: []work.RunAttemptTerminalizationRequest{{
			RunAttemptID: runAttemptID,
			Owner:        runattempt.TerminalizationOwnerPanelApply,
			TargetStatus: runattempt.StatusFailed,
			Reason:       "panel target failed after work proof publication",
		}},
	}, cliTestNow.Add(4*time.Minute)); err != nil {
		t.Fatalf("close work with prepared terminalization: %v", err)
	}
	return w.ID, runAttemptID
}

func TestWorkReconcileRunAttemptsCLIJSONRepairsPreparedAttempt(t *testing.T) {
	homePath := t.TempDir()
	workID, runAttemptID := seedPreparedRunAttemptReconcileCLIWork(t, homePath)

	stdout, stderr, exitCode, exitErr := runCompoundSplit(t, homePath,
		"work", "reconcile-run-attempts", workID, "--json")
	if exitErr != nil || exitCode != 0 {
		t.Fatalf("reconcile-run-attempts exited nonzero: code=%d err=%v\nstdout:\n%s\nstderr:\n%s", exitCode, exitErr, stdout, stderr)
	}
	var report work.RunAttemptReconcileReport
	if err := json.Unmarshal(stdout, &report); err != nil {
		t.Fatalf("parse reconcile report: %v\n%s", err, stdout)
	}
	if report.Status != "ok" || report.WorkID != workID || len(report.ReconciledIDs) != 1 || report.ReconciledIDs[0] != runAttemptID {
		t.Fatalf("reconcile report = %+v, want exact repaired attempt %s", report, runAttemptID)
	}
	after, err := runattempt.Open(homePath).Get(runAttemptID)
	if err != nil {
		t.Fatalf("get reconciled RunAttempt: %v", err)
	}
	if after.Status != runattempt.StatusFailed || after.TerminalizationOwner != runattempt.TerminalizationOwnerPanelApply {
		t.Fatalf("reconciled RunAttempt = %+v, want caller-owned failed status", after)
	}
}

func TestWorkReconcileRunAttemptsCLIJSONHoldsTamperedIntentAndExitsOne(t *testing.T) {
	homePath := t.TempDir()
	workID, runAttemptID := seedPreparedRunAttemptReconcileCLIWork(t, homePath)
	raStore := runattempt.Open(homePath)
	before, err := raStore.Get(runAttemptID)
	if err != nil || before.TerminalizationIntent == nil {
		t.Fatalf("load prepared RunAttempt: attempt=%+v err=%v", before, err)
	}
	tampered := *before.TerminalizationIntent
	tampered.TerminalizationReason = "changed after close publication"
	before.TerminalizationIntent = &tampered
	body, err := json.MarshalIndent(before, "", "  ")
	if err != nil {
		t.Fatalf("marshal tampered RunAttempt: %v", err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(filepath.Join(homePath, "run_attempts", runAttemptID+".json"), body, 0o600); err != nil {
		t.Fatalf("persist tampered RunAttempt: %v", err)
	}

	stdout, stderr, exitCode, exitErr := runCompoundSplit(t, homePath,
		"work", "reconcile-run-attempts", workID, "--json")
	if exitErr == nil || exitCode != 1 {
		t.Fatalf("held reconcile exit = %d err=%v, want exactly 1\nstdout:\n%s\nstderr:\n%s", exitCode, exitErr, stdout, stderr)
	}
	var report work.RunAttemptReconcileReport
	if err := json.Unmarshal(stdout, &report); err != nil {
		t.Fatalf("parse held reconcile report: %v\n%s", err, stdout)
	}
	if report.Status != "held" || len(report.HeldIDs) != 1 || report.HeldIDs[0] != runAttemptID || len(report.Issues) == 0 {
		t.Fatalf("held reconcile report = %+v, want exact held attempt %s", report, runAttemptID)
	}
	after, err := raStore.Get(runAttemptID)
	if err != nil {
		t.Fatalf("reload held RunAttempt: %v", err)
	}
	if runattempt.IsTerminal(after.Status) {
		t.Fatalf("held reconciliation terminalized tampered attempt: %+v", after)
	}
}
