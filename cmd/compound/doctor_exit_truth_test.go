// Falsifier tests for the doctor exit-code contract (packet work_3da5d3c76707).
//
// These tests were RED on the unmodified codebase (measured against
// main 61442538; the four FAIL lines are in the R2B report) and GREEN
// after the smallest fix lands. They prove the three operators
// (compound doctor, compound memory doctor, compound work
// proof-chain-doctor) honor the shared contract from
// compound/internal/doctor/exitcode: a doctor that finds a real problem
// exits non-zero, prints a final plain-language count line, and exposes
// its warnings in JSON.
//
// All three drive the CLI through compoundTestBinary(t) so the verb
// bodies' os.Exit calls are observable — the in-process runDoctor path
// returns an int the caller can choose to honor, and a test that ran
// `if code := runDoctor(...); code != 0 { t.Fatal }` would never see the
// real operator-facing exit code. The packet specifically opens with the
// observed gap: doctor printed 22 problem lines and exited 0.
package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	candpkg "compound/internal/candidates"
	homepkg "compound/internal/home"
	"compound/internal/mem"
	"compound/internal/runattempt"
	"compound/internal/work"
)

// compoundDoctorCLISubprocess runs `compound doctor` (or any sub-verb) in
// a child process so the verb's os.Exit code is observable. Pattern
// matches runCompoundSplit in work_proof_chain_doctor_test.go: stdout is
// the operator-facing text or JSON, stderr is the diagnostic stream.
func compoundDoctorCLISubprocess(t *testing.T, homePath string, verb []string, args ...string) (stdout, stderr []byte, exitCode int) {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmdArgs := append([]string{}, verb...)
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.Command(compoundTestBinary(t), cmdArgs...)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
	stdout, runErr := cmd.Output()
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		stderr = exitErr.Stderr
		exitCode = exitErr.ExitCode()
		return stdout, stderr, exitCode
	}
	if runErr != nil {
		t.Fatalf("compound %s subprocess errored before exit: %v\nstderr:\n%s", strings.Join(verb, " "), runErr, stderr)
	}
	return stdout, stderr, 0
}

// TestDoctorExitsNonZeroOnStaleWorktreeLock (FALSIFIER for compound doctor):
//
// Pre-fix behavior: `compound doctor` printed 22 problem lines on the
// installed binary and exited 0 — the user observed the doctor saying
// green for 28 hours while every check it should speak for was red.
// This test pins the smallest viable fixture (one stale worktree lock)
// and asserts the doctor exits 1 AND prints the canonical count line.
//
// RED on main: exit code 0, no count line. GREEN once runDoctor learns
// to sum Problems and exit ProblemFound from the shared contract.
func TestDoctorExitsNonZeroOnStaleWorktreeLock(t *testing.T) {
	t.Parallel()
	repo, _, _ := initLandingCLIRepo(t)
	runLandingCLIGit(t, repo, "switch", "main")

	stale := filepath.Join(t.TempDir(), "stale-lane")
	runLandingCLIGit(t, repo, "worktree", "add", "-b", "stale-lane", stale, "main")
	runLandingCLIGit(t, repo, "worktree", "lock", "--reason", "active governed delegate", stale)

	homeDir := t.TempDir()
	raStore := runattempt.Open(homeDir)
	seedAttempt(t, raStore, "work_stale000000", stale, true)
	stdout, stderr, exitCode := compoundDoctorCLISubprocess(t, homeDir, []string{"doctor"})
	if exitCode == 0 {
		t.Fatalf("compound doctor exited 0 with a stale worktree lock present; this is the failure mode the packet opens with.\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if exitCode != 1 {
		t.Fatalf("compound doctor exited %d, want 1 (the shared contract's ProblemFound)", exitCode)
	}
	out := string(stdout)
	// Final-line contract from internal/doctor/exitcode:
	// "<verb>: N problem(s) found — <hint>". N is >= 1; the
	// installed-binary harness version drift on the developer's
	// machine contributes an extra one in some environments, so we
	// pin only the prefix and the existence of the stale-lock line.
	if !strings.Contains(out, "problem(s) found") {
		t.Fatalf("doctor text does not end with the canonical count line:\n%s", out)
	}
	if !strings.Contains(out, "stale worktree lock") {
		t.Fatalf("doctor text did not name the stale-lock problem:\n%s", out)
	}
	if !strings.Contains(out, "compound work delegate sweep") {
		t.Fatalf("doctor text did not name the remediation door for a stale lock:\n%s", out)
	}
}

// TestDoctorExitsZeroOnFreshHome (OK-path guard): a fresh home with no
// problems must exit 0 and stay quiet — but the harness-version-drift
// check is environment-dependent and would correctly exit 1 on machines whose
// installed binaries differ from the registry. The OK-path "stay quiet"
// contract is covered by the unit test in internal/doctor/exitcode
// (TestDoctorExitWith_OKStaysQuiet). Keeping this as a Skip stub so a
// future fix that finds a portable way to construct a fresh-home
// zero-drift fixture can re-enable it.
func TestDoctorExitsZeroOnFreshHome(t *testing.T) {
	t.Skip("OK-path contract is unit-tested at internal/doctor/exitcode; see TestDoctorExitWith_OKStaysQuiet")
}

// TestMemoryDoctorSurfacesWarningsNotSilentOk (FALSIFIER for compound
// memory doctor, warning half):
//
// Pre-fix behavior: a warning-only hop (a memory candidate sits in the
// inbox and the pending count was never reconciled — the capture hop
// warns, no hop fails) printed `"ok": true` with NO warnings field and
// exited 0. The packet names exactly this: "compound memory doctor
// buries warnings inside \"ok\": true". A warning must stay exit 0
// (warnings are info-only) but it must be VISIBLE: a non-empty
// "warnings" array in JSON.
//
// RED on main: Report has no warnings field at all. GREEN once
// mem.Report.Warnings rolls up warn hops and the JSON path prints it.
func TestMemoryDoctorSurfacesWarningsNotSilentOk(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := homepkg.Init(home, "test", cliTestNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	// One proposed memory candidate and no reconciled pending state: the
	// capture hop warns ("pending count not reconciled"), nothing fails.
	cand := mem.Candidate{ID: "cand_exittruth", Entity: "entity:exittruth", Predicate: "observed", Object: "warn hop", State: "proposed", Visibility: "candidate"}
	if err := candpkg.WriteCandidate(home, candpkg.Memory, cand.ID, cand); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	stdout, _, exitCode := compoundDoctorCLISubprocess(t, home, []string{"memory", "doctor"}, "--json")
	if exitCode != 0 {
		t.Fatalf("warning-only memory doctor exited %d, want 0 (warnings are info-only, never fatal)\nstdout:\n%s", exitCode, stdout)
	}
	var report struct {
		OK       bool     `json:"ok"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(stdout, &report); err != nil {
		t.Fatalf("parse memory doctor json: %v\nstdout:\n%s", err, stdout)
	}
	if !report.OK {
		t.Fatalf("warning-only fixture must keep ok=true (only fail hops flip it):\n%s", stdout)
	}
	if len(report.Warnings) == 0 {
		t.Fatalf("memory doctor buried a warning inside ok:true — warnings array is empty/absent:\n%s", stdout)
	}
}

// TestMemoryDoctorExitsNonZeroOnProblem (FALSIFIER for compound memory
// doctor, problem half): seeds a claim without provenance (the
// authority hop reports fail on any missing-derivation/missing-evidence
// claim). It asserts:
//
//   - exit code is 1
//   - `ok: false` is present (or a non-empty `warnings` array)
//
// RED on main for the warning half above; the problem half pins the
// exit-1 path so a future refactor cannot regress it quietly.
func TestMemoryDoctorExitsNonZeroOnProblem(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := homepkg.Init(home, "test", cliTestNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	// authority hop reports fail (see internal/mem/mem.go DoctorAt).
	if err := os.MkdirAll(filepath.Join(home, "ledger"), 0o755); err != nil {
		t.Fatalf("mkdir ledger: %v", err)
	}
	claimsPath := filepath.Join(home, "ledger", "claims.jsonl")
	if err := os.WriteFile(claimsPath, []byte(`{"id":"claim_broken","entity":"x","predicate":"y","confidence":0.5,"source":{"class":"memory/observation"},"created_at":"2026-09-04T00:00:00Z"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write claims: %v", err)
	}

	stdout, _, exitCode := compoundDoctorCLISubprocess(t, home, []string{"memory", "doctor"}, "--json")
	if exitCode == 0 {
		t.Fatalf("memory doctor exited 0 with a broken authority claim; the packet's primary memory gap.\nstdout:\n%s", stdout)
	}
	if exitCode != 1 {
		t.Fatalf("memory doctor exited %d, want 1", exitCode)
	}
	var report map[string]any
	if err := json.Unmarshal(stdout, &report); err != nil {
		t.Fatalf("parse memory doctor json: %v\nstdout:\n%s", err, stdout)
	}
	if ok, _ := report["ok"].(bool); ok {
		ws, _ := report["warnings"].([]any)
		if len(ws) == 0 {
			t.Fatalf("memory doctor reported ok=true with no warnings array despite a failing hop:\n%s", stdout)
		}
	}
}

// TestMemoryDoctorExitsZeroOnFreshHome (OK-PATH GUARD):
//
// The memory doctor on an empty fresh home must exit 0 — the contract
// is that warnings never cause a non-zero exit (warnings are info-only).
func TestMemoryDoctorExitsZeroOnFreshHome(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := homepkg.Init(home, "test", cliTestNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	stdout, _, exitCode := compoundDoctorCLISubprocess(t, home, []string{"memory", "doctor"}, "--json")
	if exitCode != 0 {
		t.Fatalf("fresh-home memory doctor exited %d, want 0\nstdout:\n%s", exitCode, stdout)
	}
}

// TestWorkProofChainDoctorPrintsCountLineOnCleanFixture (FALSIFIER (c)
// half 1: OK-path with mandatory count line):
//
// Pre-fix behavior: `compound work proof-chain-doctor` on a clean
// fixture printed a header and zero counts but NO verdict line — a
// caller scanning the bottom of the output could not tell "clean" from
// "never ran". The packet's response: even when clean, the doctor MUST
// print one canonical line so an operator can see "0 problems found" —
// silence is the gap.
//
// RED on main (measured): body ends at "- complete: 0", no count line.
func TestWorkProofChainDoctorPrintsCountLineOnCleanFixture(t *testing.T) {
	t.Parallel()
	home := initializedTestHome(t)
	stdout, _, exitCode := compoundDoctorCLISubprocess(t, home, []string{"work", "proof-chain-doctor"})
	if exitCode != 0 {
		t.Fatalf("clean proof-chain doctor exit = %d, want 0\nstdout:\n%s", exitCode, stdout)
	}
	if !strings.Contains(string(stdout), "proof-chain-doctor: 0 problem(s) found") {
		t.Fatalf("clean proof-chain doctor did not print the canonical count line:\n%s", stdout)
	}
}

// TestWorkProofChainDoctorExitsNonZeroOnBrokenChain (FALSIFIER (c)
// half 2: broken chain → exit 1 + named finding + count line):
//
// Pre-fix behavior: a broken chain WAS reported in the doctor body and
// the verb already exited 1 (that much main got right) — but with no
// final count line, so the output's last words were a raw finding, not
// the canonical "N problem(s) found — what to do first" a tired
// operator (or a grep in CI) can rely on. This pins all three: exit 1,
// the broken work id named, and the canonical count line.
//
// We construct the broken chain by closing a work item whose receipt
// file we then delete. The doctor's proofChainItem paths report
// receipt_unreadable findings and the contract routes that to exit 1.
//
// RED on main (measured): findings printed, exit 1, no count line.
func TestWorkProofChainDoctorExitsNonZeroOnBrokenChain(t *testing.T) {
	// Note: not parallel — uses t.Setenv below, which is incompatible
	// with t.Parallel under the testing package.
	home := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", home)
	store, err := work.Open(home)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	w, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "exit-truth broken-chain falsifier"}, cliTestNow)
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
		Outcome:  "completed",
		MoveType: "audit_only",
		Summary:  "exit-truth broken-chain",
	}, cliTestNow); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Break the chain by deleting all receipt segments. Receipts live
	// at $HOME/events/segments/receipt/<date>.jsonl (the event store's
	// framed-append-only layout, see internal/eventstore/segment.go).
	// Deleting them makes proofChainItem's readEventJSON fail and the
	// doctor emits receipt_unreadable findings (status=fail, exits 1
	// under the new contract).
	receiptSegDir := filepath.Join(home, "events", "segments", "receipt")
	entries, err := os.ReadDir(receiptSegDir)
	if err != nil {
		t.Fatalf("read receipt segment dir: %v", err)
	}
	deleted := 0
	for _, e := range entries {
		if err := os.Remove(filepath.Join(receiptSegDir, e.Name())); err != nil {
			t.Fatalf("remove receipt segment: %v", err)
		}
		deleted++
	}
	if deleted == 0 {
		t.Fatal("expected at least one receipt segment file; close did not emit a receipt")
	}
	stdout, stderr, exitCode := compoundDoctorCLISubprocess(t, home, []string{"work", "proof-chain-doctor"})
	if exitCode == 0 {
		t.Fatalf("proof-chain doctor exited 0 with a missing receipt; the contract demands exit 1.\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if exitCode != 1 {
		t.Fatalf("proof-chain doctor exited %d, want 1", exitCode)
	}
	out := string(stdout)
	if !strings.Contains(out, w.ID) {
		t.Fatalf("proof-chain doctor did not name the broken work id %s in its output:\n%s", w.ID, out)
	}
	if !strings.Contains(out, "proof-chain-doctor: ") || !strings.Contains(out, " problem(s) found") {
		t.Fatalf("proof-chain doctor did not end with the canonical count line:\n%s", out)
	}
}
