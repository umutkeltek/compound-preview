package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func newResumeFixture(t *testing.T, predicate string) claimedExecutionFixture {
	t.Helper()
	homePath := initializedExecutionHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	w, err := store.Create(work.CreateInput{Owner: "codex", SuccessPredicate: predicate}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "codex", now); err != nil {
		t.Fatal(err)
	}
	return claimedExecutionFixture{homePath: homePath, store: store, workID: w.ID}
}

func resumeCommand(t *testing.T, f claimedExecutionFixture, flags ...string) (string, error) {
	t.Helper()
	args := append([]string{"work", "advance", f.workID}, flags...)
	cmd := exec.Command(compoundTestBinary(t), args...)
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+f.homePath)
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func resumeEnvelopeBytes(t *testing.T, f claimedExecutionFixture) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.homePath, "work", "work", f.workID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func resumeRunFirst(t *testing.T, f claimedExecutionFixture) envelope.WorkEnvelope {
	t.Helper()
	out, err := resumeCommand(t, f, "--to", "running", "--execute", "--by", "codex",
		"--harness", "os-harness-entrypoints", "--", "/usr/bin/true")
	if err != nil {
		t.Fatalf("first governed run: %v\n%s", err, out)
	}
	w, err := f.store.Get(f.workID)
	if err != nil {
		t.Fatal(err)
	}
	if w.State != "verifying" {
		t.Fatalf("first run state = %s", w.State)
	}
	return w
}

// The first run and explicit resume both use the public CLI and real OS adapter.
// No Claude profile or historical permission grant authorizes the second run.
func TestWorkAdvanceResumeVerifyingRealSecondLaunch(t *testing.T) {
	f := newResumeFixture(t, "same-work continuation with fresh authority")
	before := resumeRunFirst(t, f)
	attemptStore := runattempt.Open(f.homePath)
	old := f.executionAttempt(t)
	oldBytes, err := os.ReadFile(filepath.Join(f.homePath, "run_attempts", old.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := resumeCommand(t, f, "--to", "running", "--execute", "--resume-verifying",
		"--by", "codex", "--harness", "os-harness-entrypoints", "--", "/bin/echo", "second governed launch")
	if err != nil {
		t.Fatalf("same-work resume failed: %v\n%s", err, out)
	}
	after, err := f.store.Get(f.workID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != before.ID || after.State != "verifying" ||
		len(after.RunAttemptIDs) != len(before.RunAttemptIDs)+1 ||
		!slices.Equal(after.RunAttemptIDs[:len(before.RunAttemptIDs)], before.RunAttemptIDs) {
		t.Fatalf("resume lost work/history: before=%+v after=%+v", before, after)
	}
	if len(after.ActionIntentIDs) != len(before.ActionIntentIDs)+2 {
		t.Fatalf("resume must mint running and verifying actions: before=%v after=%v", before.ActionIntentIDs, after.ActionIntentIDs)
	}
	attempt, err := attemptStore.Get(after.RunAttemptIDs[len(after.RunAttemptIDs)-1])
	if err != nil {
		t.Fatal(err)
	}
	if attempt.WorkID != before.ID || attempt.ID == old.ID ||
		attempt.Status != runattempt.StatusSucceeded || attempt.ActionIntentID == old.ActionIntentID ||
		attempt.ActionIntentID != after.ActionIntentIDs[len(before.ActionIntentIDs)] {
		t.Fatalf("resume attempt not fresh/linked: %+v", attempt)
	}
	fields := strings.Fields(out[strings.Index(out, "policy=")+len("policy="):])
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "pol_") {
		t.Fatalf("missing policy: %s", out)
	}
	var decision envelope.PolicyDecision
	readTestEventJSON(t, f.homePath, fields[0], &decision)
	if decision.ID == before.PolicyDecisionID || decision.ActionIntentID != attempt.ActionIntentID {
		t.Fatalf("new running decision not linked to attempt: %+v", decision)
	}
	transcriptPath := attempt.TranscriptRef
	if !filepath.IsAbs(transcriptPath) {
		transcriptPath = filepath.Join(f.homePath, transcriptPath)
	}
	transcript, err := os.ReadFile(transcriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(transcript), "second governed launch") {
		t.Fatalf("second child not consumed: %s", transcript)
	}
	unchanged, err := os.ReadFile(filepath.Join(f.homePath, "run_attempts", old.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(oldBytes, unchanged) {
		t.Fatal("resume rewrote prior attempt")
	}
}

func TestWorkAdvanceResumeVerifyingRefusalsPreserveEnvelope(t *testing.T) {
	for _, name := range []string{"no_flag", "claimed", "no_execute", "wrong_target", "steal",
		"wrong_actor", "unknown_adapter", "missing_workspace", "policy_hold", "autonomy_halt",
		"pending_attempt", "missing_attempt", "wrong_work_attempt", "malformed_attempt"} {
		t.Run(name, func(t *testing.T) {
			f := newResumeFixture(t, "resume refuses without side effects")
			if name != "claimed" {
				resumeRunFirst(t, f)
			}
			flags := []string{"--to", "running", "--execute", "--resume-verifying", "--by", "codex",
				"--harness", "os-harness-entrypoints"}
			switch name {
			case "no_flag":
				flags = slices.Delete(flags, 3, 4)
			case "no_execute":
				flags = slices.Delete(flags, 2, 3)
			case "wrong_target":
				flags[1] = "verifying"
			case "steal":
				flags = append(flags, "--steal", "--reason", "not a resume lease transfer")
			case "wrong_actor":
				flags[5] = "another-executor"
			case "unknown_adapter":
				flags[7] = "missing-fixture-adapter"
			case "missing_workspace":
				flags = append(flags, "--workspace", filepath.Join(t.TempDir(), "absent"))
			case "policy_hold":
				if err := os.WriteFile(filepath.Join(f.homePath, "config", "policy.yaml"),
					[]byte("class_outcome:\n  local_reversible:\n    outcome: hold\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "autonomy_halt":
				if err := policy.WriteAutonomyHalt(f.homePath, policy.AutonomyHalt{
					DisabledClasses: map[string]string{"build-implement": "fixture stop"},
				}); err != nil {
					t.Fatal(err)
				}
			case "pending_attempt", "missing_attempt", "wrong_work_attempt", "malformed_attempt":
				a := f.executionAttempt(t)
				path := filepath.Join(f.homePath, "run_attempts", a.ID+".json")
				switch name {
				case "missing_attempt":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				case "malformed_attempt":
					if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
						t.Fatal(err)
					}
				default:
					if name == "pending_attempt" {
						a.Status = runattempt.StatusRunning
					} else {
						a.WorkID = "work_other_fixture"
					}
					b, err := json.Marshal(a)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, b, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := resumeEnvelopeBytes(t, f)
			attemptsBefore, err := filepath.Glob(filepath.Join(f.homePath, "run_attempts", "ratt_*.json"))
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "must-not-launch")
			flags = append(flags, "--", "/usr/bin/touch", marker)
			out, err := resumeCommand(t, f, flags...)
			if err == nil {
				t.Fatalf("refusal returned success: %s", out)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("refused child launched: %v", err)
			}
			if !bytes.Equal(before, resumeEnvelopeBytes(t, f)) {
				t.Fatal("refusal changed WorkEnvelope bytes")
			}
			attemptsAfter, err := filepath.Glob(filepath.Join(f.homePath, "run_attempts", "ratt_*.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(attemptsBefore, attemptsAfter) {
				t.Fatalf("refusal minted execution attempt: %v -> %v", attemptsBefore, attemptsAfter)
			}
		})
	}
}

func TestWorkAdvanceResumeVerifyingFailureRecoveryClosesWithProof(t *testing.T) {
	f := newResumeFixture(t, "same work can test, fail, repair and close truthfully")
	first := resumeRunFirst(t, f)
	old := f.executionAttempt(t)
	oldBytes, err := os.ReadFile(filepath.Join(f.homePath, "run_attempts", old.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	flags := []string{"--to", "running", "--execute", "--resume-verifying", "--by", "codex", "--harness", "os-harness-entrypoints", "--"}
	out, err := resumeCommand(t, f, append(append([]string(nil), flags...), "/usr/bin/false")...)
	if err == nil {
		t.Fatalf("failed subprocess returned success: %s", out)
	}
	failedWork, err := f.store.Get(f.workID)
	if err != nil {
		t.Fatal(err)
	}
	if failedWork.State != "verifying" {
		t.Fatalf("failed resume must remain verifying, got %s: %s", failedWork.State, out)
	}
	if !strings.Contains(out, "status: failed") || !strings.Contains(out, "remains verifying") {
		t.Fatalf("attempt failure hidden: %s", out)
	}
	failed := f.executionAttempt(t)
	if failed.Status != runattempt.StatusFailed || failed.PredecessorID != old.ID {
		t.Fatalf("failure lost immutable lineage: %+v", failed)
	}
	if len(failedWork.RunAttemptIDs) != len(first.RunAttemptIDs)+1 {
		t.Fatal("failed attempt not forward-linked")
	}
	out, err = resumeCommand(t, f, append(append([]string(nil), flags...), "/usr/bin/true")...)
	if err != nil {
		t.Fatalf("repair resume: %v\n%s", err, out)
	}
	repaired := f.executionAttempt(t)
	if repaired.Status != runattempt.StatusSucceeded || repaired.PredecessorID != failed.ID {
		t.Fatalf("repair lost retry lineage: %+v", repaired)
	}
	unchanged, err := os.ReadFile(filepath.Join(f.homePath, "run_attempts", old.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(oldBytes, unchanged) {
		t.Fatal("retry chain rewrote original success")
	}
	after, err := f.store.Get(f.workID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.RunAttemptIDs) != len(first.RunAttemptIDs)+2 {
		t.Fatal("work lost attempt history")
	}
	if _, err := f.store.Close(f.workID, work.CloseInput{Outcome: "completed", MoveType: "audit_only", Actor: "codex",
		Summary: "real success-failure-success loop verified", ProofRefs: []string{repaired.TranscriptRef}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	report, err := f.store.ProofChainDoctor(time.Now().UTC(), work.ProofChainDoctorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || len(report.Findings) != 0 {
		t.Fatalf("unchanged proof-chain gate rejected real recovery: %+v", report)
	}
}

// A terminal failed attempt is provenance, not a veto or permission grant.
func TestWorkAdvanceResumeVerifyingAcceptsTerminalHistory(t *testing.T) {
	for _, status := range []string{runattempt.StatusSucceeded, runattempt.StatusFailed, runattempt.StatusAbandoned, runattempt.StatusSuperseded} {
		t.Run(status, func(t *testing.T) {
			f := newResumeFixture(t, "terminal history is evidence only")
			w, err := f.store.Advance(f.workID, work.AdvanceInput{To: "running", Actor: "codex"}, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			store := runattempt.Open(f.homePath)
			a, err := store.Get(w.RunAttemptIDs[0])
			if err != nil {
				t.Fatal(err)
			}
			// Simulate historical terminal evidence; the resume must not require success.
			a.Status = status
			a.CompletedAt = time.Now().UTC()
			b, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.homePath, "run_attempts", a.ID+".json"), b, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.Advance(f.workID, work.AdvanceInput{To: "verifying", Actor: "codex"}, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			out, err := resumeCommand(t, f, "--to", "running", "--execute", "--resume-verifying", "--by", "codex",
				"--harness", "os-harness-entrypoints", "--", "/usr/bin/true")
			if err != nil {
				t.Fatalf("terminal %s history refused: %v\n%s", status, err, out)
			}
		})
	}
}
