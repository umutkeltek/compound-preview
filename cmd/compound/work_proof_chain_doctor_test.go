package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"compound/internal/envelope"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func TestWorkProofChainDoctorCLIJSON(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	w, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "proof-chain CLI reports clean terminal work"}, cliTestNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Claim(w.ID, "codex", cliTestNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running", Note: "run"}, cliTestNow); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if _, err := store.Close(w.ID, work.CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "proof-chain CLI covered",
		ProofRefs: []string{"repair:proof-chain-cli"},
	}, cliTestNow); err != nil {
		t.Fatalf("close: %v", err)
	}

	out := captureStdout(t, func() {
		cmdWork([]string{"proof-chain-doctor", "--json"})
	})
	for _, field := range []string{`"run_attempt_outcome_mismatch": 0`, `"run_attempt_terminalization_mismatch": 0`} {
		if !strings.Contains(out, field) {
			t.Fatalf("v0.3 report omitted always-serialized field %s:\n%s", field, out)
		}
	}
	var report work.ProofChainDoctorReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse report: %v\n%s", err, out)
	}
	if report.Status != "ok" || report.Counts.TerminalChecked != 1 || report.Counts.Complete != 1 {
		t.Fatalf("report = %+v, want one clean terminal chain", report)
	}
	if len(report.ProofRefs) == 0 || !strings.Contains(strings.Join(report.ProofRefs, ","), "work-proof-chain-terminal-receipt") {
		t.Fatalf("proof refs missing terminal receipt guard: %+v", report.ProofRefs)
	}
}

// TestWorkAdvanceExecuteThenClose: full public CLI E2E for the proof-chain
// RunAttempt dimension. Subprocesses only -- no in-process calls into the
// work store and no captureStdout/cmdWork so the doctor's os.Exit on a fail
// report cannot kill the test process.
//
// Falsifier (RED): pre-v0.2 the doctor never loaded RunAttempt evidence; the
// intact-chain assertions would still pass, but deleting the referenced
// run_attempts/<id>.json would leave the doctor reporting status:"ok"
// complete:1 even though the chain was provably broken.
func TestWorkAdvanceExecuteThenClose(t *testing.T) {
	homePath := initializedTestHome(t)

	// Seed only ready state through the public CLI.
	createOut, err := runCompound(t, homePath,
		"work", "create",
		"--owner", "codex",
		"--predicate", "advance --execute -> close -> proof-chain doctor reaches RunAttempt evidence",
		"--by", "codex",
		"--state", "ready",
	)
	if err != nil {
		t.Fatalf("work create: %v\n%s", err, createOut)
	}
	workID := parseWorkIDFromCreate(t, createOut)

	// Claim, then advance --execute through the os-harness-entrypoints adapter
	// so a real RunAttempt is minted and persisted on disk.
	if out, err := runCompound(t, homePath, "work", "claim", workID, "--by", "codex"); err != nil {
		t.Fatalf("work claim: %v\n%s", err, out)
	}
	workspace := t.TempDir()
	var advanceOut string
	if advanceOut, err = runCompound(t, homePath,
		"work", "advance", workID,
		"--to", "running",
		"--execute",
		"--harness", "os-harness-entrypoints",
		"--by", "codex",
		"--workspace", workspace,
		"--", "echo", "advance-execute-then-close",
	); err != nil {
		t.Fatalf("work advance --execute: %v\n%s", err, advanceOut)
	}
	if !strings.Contains(advanceOut, "status: succeeded") {
		t.Fatalf("work advance --execute did not surface a succeeded run; output:\n%s", advanceOut)
	}
	if out, err := runCompound(t, homePath,
		"work", "close", workID,
		"--outcome", "completed",
		"--move-type", "control_plane_repair",
		"--summary", "advance-execute-then-close covered",
		"--proof", "repair:proof-chain-runattempt",
		"--by", "codex",
	); err != nil {
		t.Fatalf("work close: %v\n%s", err, out)
	}

	// Show the work via typed envelope.WorkEnvelope so the assertions bind
	// to the public JSON contract, not tolerant map helpers.
	showOut, err := runCompound(t, homePath, "work", "show", workID, "--json")
	if err != nil {
		t.Fatalf("work show: %v\n%s", err, showOut)
	}
	var wEnv envelope.WorkEnvelope
	if err := json.Unmarshal([]byte(showOut), &wEnv); err != nil {
		t.Fatalf("parse show as typed envelope: %v\n%s", err, showOut)
	}
	rattIDs := wEnv.RunAttemptIDs
	actionIDs := wEnv.ActionIntentIDs
	if len(rattIDs) < 2 {
		t.Fatalf("expected claim-reservation + execution RunAttempts, got %d (%v)", len(rattIDs), rattIDs)
	}
	raStore := runattempt.Open(homePath)
	actionSet := map[string]bool{}
	for _, aid := range actionIDs {
		actionSet[aid] = true
	}
	var executionRatt string
	for _, rattID := range rattIDs {
		ra, err := raStore.Get(rattID)
		if err != nil {
			t.Fatalf("intact RunAttempt %s unreadable: %v", rattID, err)
		}
		if ra.WorkID != workID {
			t.Fatalf("RunAttempt %s work_id=%q, want %q", rattID, ra.WorkID, workID)
		}
		if !runattempt.IsTerminal(ra.Status) {
			t.Fatalf("RunAttempt %s status=%q, want terminal", rattID, ra.Status)
		}
		if ra.ActionIntentID != "" && !actionSet[ra.ActionIntentID] {
			t.Fatalf("RunAttempt %s action_intent_id=%q is absent from work actions %v", rattID, ra.ActionIntentID, actionIDs)
		}
		if ra.Origin == runattempt.OriginHarnessExecution {
			if executionRatt != "" {
				t.Fatalf("expected one harness execution RunAttempt, got at least %s and %s", executionRatt, rattID)
			}
			if ra.ActionIntentID == "" {
				t.Fatalf("execution RunAttempt %s has no ActionIntent", rattID)
			}
			executionRatt = rattID
		}
	}
	if executionRatt == "" {
		t.Fatalf("expected one harness execution RunAttempt; rattIDs=%v actions=%v", rattIDs, actionIDs)
	}

	// Intact chain: doctor must report status:"ok" complete:1, schema v0.3,
	// the new proof ref, and the work item RunAttemptIDs that include the
	// execution attempt (the public JSON contract, not storage-only).
	doctorOut, err := runCompound(t, homePath, "work", "proof-chain-doctor", "--json")
	if err != nil {
		t.Fatalf("intact proof-chain-doctor exited non-zero: %v\n%s", err, doctorOut)
	}
	var report work.ProofChainDoctorReport
	if err := json.Unmarshal([]byte(doctorOut), &report); err != nil {
		t.Fatalf("parse intact report: %v\n%s", err, doctorOut)
	}
	if report.SchemaVersion != "compound.work_proof_chain_doctor.v0.3" {
		t.Fatalf("schema_version = %q, want compound.work_proof_chain_doctor.v0.3", report.SchemaVersion)
	}
	if report.Status != "ok" || report.Counts.Complete != 1 || report.Counts.MissingRunAttempt != 0 {
		t.Fatalf("intact chain rejected: status=%q counts=%+v", report.Status, report.Counts)
	}
	if !slices.Contains(report.ProofRefs, "work-proof-chain-run-attempt") {
		t.Fatalf("proof refs missing work-proof-chain-run-attempt: %v", report.ProofRefs)
	}
	var item *work.ProofChainDoctorItem
	for i := range report.Items {
		if report.Items[i].WorkID == workID {
			item = &report.Items[i]
			break
		}
	}
	if item == nil {
		t.Fatalf("doctor items missing work %s: %+v", workID, report.Items)
	}
	if !slices.Contains(item.RunAttemptIDs, executionRatt) {
		t.Fatalf("doctor item RunAttemptIDs = %v, want to include execution attempt %s", item.RunAttemptIDs, executionRatt)
	}

	// Corruption assertion: deleting the executed RunAttempt file must turn
	// the doctor into a fail report carrying missing_run_attempt and exit
	// the subprocess with EXACT exit code 1. Capture stdout and stderr
	// separately so the JSON report on stdout is not conflated with the
	// doctor's trailing stderr messages.
	rattPath := filepath.Join(homePath, "run_attempts", executionRatt+".json")
	if err := os.Remove(rattPath); err != nil {
		t.Fatalf("remove ratt: %v", err)
	}
	reportBytes, stderr, exitCode, exitErr := runCompoundSplit(t, homePath, "work", "proof-chain-doctor", "--json")
	if exitErr == nil {
		t.Fatalf("corrupted proof-chain-doctor exited zero; stdout:\n%s\nstderr:\n%s", reportBytes, stderr)
	}
	if exitCode != 1 {
		t.Fatalf("corrupted proof-chain-doctor exit code = %d, want exactly 1; err=%v", exitCode, exitErr)
	}
	var reportBad work.ProofChainDoctorReport
	if err := json.Unmarshal(reportBytes, &reportBad); err != nil {
		t.Fatalf("parse corrupted report: %v\nstdout:\n%s\nstderr:\n%s", err, reportBytes, stderr)
	}
	if reportBad.Status != "fail" {
		t.Fatalf("corruption not detected: status=%q counts=%+v", reportBad.Status, reportBad.Counts)
	}
	if reportBad.Counts.MissingRunAttempt != 1 {
		t.Fatalf("MissingRunAttempt = %d, want exactly 1 after corruption", reportBad.Counts.MissingRunAttempt)
	}
	var foundMissingRunAttempt bool
	for _, finding := range reportBad.Findings {
		if finding.Code == "missing_run_attempt" {
			foundMissingRunAttempt = true
			break
		}
	}
	if !foundMissingRunAttempt {
		t.Fatalf("findings missing missing_run_attempt: %+v", reportBad.Findings)
	}
}

// parseWorkIDFromCreate extracts the work_ id from the public "created <id>"
// prefix printed by `compound work create`.
func parseWorkIDFromCreate(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "created ") {
			rest := strings.TrimPrefix(line, "created ")
			fields := strings.Fields(rest)
			if len(fields) > 0 && strings.HasPrefix(fields[0], "work_") {
				return fields[0]
			}
		}
	}
	t.Fatalf("could not find created work id in:\n%s", out)
	return ""
}

// runCompoundSplit runs the compound binary and returns stdout, stderr, the
// numeric exit code, and the original ExitError. Uses Output (not
// CombinedOutput) so the JSON report on stdout is not conflated with the
// doctor's stderr messages.
func runCompoundSplit(t *testing.T, homePath string, args ...string) (stdout, stderr []byte, exitCode int, exitErr error) {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(compoundTestBinary(t), args...)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
	stdout, err = cmd.Output()
	var exitErrTyped *exec.ExitError
	if errors.As(err, &exitErrTyped) {
		stderr = exitErrTyped.Stderr
		exitCode = exitErrTyped.ExitCode()
		exitErr = exitErrTyped
	} else if err != nil {
		stderr = []byte{}
		t.Fatalf("compound binary output error: %v\nstderr:\n%s", err, stderr)
	}
	return stdout, stderr, exitCode, exitErr
}
