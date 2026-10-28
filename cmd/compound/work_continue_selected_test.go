package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/routeevidence"
	"compound/internal/runattempt"
	"compound/internal/work"
)

// The public Continue command must reach the real governed door, not a swapped
// package-private launcher. Only the enrolled executor is synthetic (no model).
func TestSelectedContinuePublicDoorPreservesGoalSelectionAndIsolation(t *testing.T) {
	runSelectedContinueJourney(t, false)
}

func TestSelectedContinueInitialInterruptionIsRecoverable(t *testing.T) {
	runSelectedContinueJourney(t, true)
}

func runSelectedContinueJourney(t *testing.T, interruptFirst bool) {
	t.Helper()
	homePath := initializedExecutionHome(t)
	defer logSelectedContinueFailure(t, homePath)
	writeDispatchExecuteModelRouterFixtureForHarnessAlias(t, homePath, "codex", "fixture-codex-wrapper")
	_, _, revision, err := routeevidence.LoadModelCapabilitiesSnapshot(homePath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	workspace, base, _ := initLandingCLIRepo(t)
	now := time.Now().UTC()
	w, err := store.Create(work.CreateInput{
		Owner: "codex", SuccessPredicate: "maintenance contract remains satisfied", DesiredOutcome: "maintain the saved product goal",
		GitDelivery: &envelope.GitDeliveryContract{TargetWorktreeRoot: workspace, TargetLocalRef: "refs/heads/main", ExpectedBaseSHA: base, PublicationMode: "local_only"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "codex", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveExecutionSelection(w.ID, 0, envelope.ExecutionSelection{
		LaunchProfileID: "fixture-codex", LaunchProfileRevision: revision, Harness: "codex", ProviderRoute: "fixture-provider", RequestedModelID: "fixture-codex",
		Source: "explicit", Revision: 1, SelectedBy: "codex", SelectedAt: now.Format(time.RFC3339),
	}, "codex", now); err != nil {
		t.Fatal(err)
	}
	before, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Create(work.CreateInput{Owner: "codex", SuccessPredicate: "unrelated work must remain untouched"}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	otherPath := filepath.Join(homePath, "work", "work", other.ID+".json")
	otherBefore, err := os.ReadFile(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	launchLog := filepath.Join(t.TempDir(), "launch.txt")
	blockPath := filepath.Join(t.TempDir(), "interrupt")
	readyPath := filepath.Join(t.TempDir(), "running")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$COMPOUND_TEST_LAUNCH_LOG\"\nif test -f \"$COMPOUND_TEST_BLOCK\"; then printf 'running' > \"$COMPOUND_TEST_READY\"; exec sleep 30; fi\n"
	if err := os.WriteFile(filepath.Join(binDir, "fixture-codex-wrapper"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	invoke := func(extra ...string) work.ContinueResult {
		t.Helper()
		// No work ID, harness, model, prompt or lifecycle command is copied
		// into the next session. The actor's sole held work is unambiguous.
		args := append([]string{"work", "continue", "--actor", "codex", "--selected", "--json"}, extra...)
		cmd := exec.Command(compoundTestBinary(t), args...)
		cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"), "COMPOUND_TEST_LAUNCH_LOG="+launchLog, "COMPOUND_TEST_BLOCK="+blockPath, "COMPOUND_TEST_READY="+readyPath)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("selected Continue: %v\n%s", err, out)
		}
		var result work.ContinueResult
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatalf("decode: %v\n%s", err, out)
		}
		return result
	}
	if interruptFirst {
		if err := os.WriteFile(blockPath, []byte("interrupt first execution"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result := invoke()
	if result.Action != "execute" || result.Detached == nil || result.WorkID != w.ID {
		t.Fatalf("not a selected launch: %+v", result)
	}
	if interruptFirst {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(readyPath); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if _, err := os.Stat(readyPath); err != nil {
			t.Fatal("initial executor did not start")
		}
		if err := syscall.Kill(-result.Detached.PGID, syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		deadline = time.Now().Add(10 * time.Second)
		var interrupted envelope.WorkEnvelope
		for time.Now().Before(deadline) {
			interrupted, err = store.Get(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			if interrupted.State == "failed" || (interrupted.State == "verifying" && len(interrupted.RunAttemptIDs) == len(before.RunAttemptIDs)+1) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if interrupted.State != "verifying" {
			t.Fatalf("initial interruption destroyed resumable work: state=%s", interrupted.State)
		}
		if err := os.Remove(blockPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(readyPath); err != nil {
			t.Fatal(err)
		}
		deadline = time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, owns := work.DetachedDoorOwns(homePath, w.ID); !owns {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		before = interrupted
		result = invoke("--resume")
		if result.Action != "execute" {
			t.Fatalf("initial interruption could not resume: %+v", result)
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	var final envelope.WorkEnvelope
	for time.Now().Before(deadline) {
		final, err = store.Get(w.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.State == "verifying" && len(final.RunAttemptIDs) == len(before.RunAttemptIDs)+1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if final.State != "verifying" || len(final.RunAttemptIDs) != len(before.RunAttemptIDs)+1 {
		log, _ := os.ReadFile(result.Detached.LogPath)
		t.Fatalf("door did not finalize to verification: %+v\ndoor log:\n%s", final, log)
	}
	attempt, err := runattempt.Open(homePath).Get(final.RunAttemptIDs[len(final.RunAttemptIDs)-1])
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Status != runattempt.StatusSucceeded || attempt.RequestedModelID != "fixture-codex" || attempt.RequestedProviderRoute != "fixture-provider" || attempt.ExecutionSelection == nil {
		t.Fatalf("selected attempt lost identity: %+v", attempt)
	}
	logged, err := os.ReadFile(launchLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{w.ID, "maintain the saved product goal", "maintenance contract remains satisfied"} {
		if !strings.Contains(string(logged), want) {
			t.Fatalf("saved intent %q absent from actual executor input: %s", want, logged)
		}
	}
	otherAfter, err := os.ReadFile(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(otherBefore, otherAfter) {
		t.Fatal("selected Continue mutated unrelated work")
	}
	// A fresh Continue session cannot turn subprocess success into acceptance,
	// nor spend another attempt while proof is awaiting review.
	wait := invoke()
	if wait.Action != "none" || (wait.WaitReason != "verification_required" && wait.WaitReason != "detached_door_running") {
		t.Fatalf("success became acceptance/relaunch: %+v", wait)
	}
	firstCount := len(final.RunAttemptIDs)
	if err := os.WriteFile(blockPath, []byte("interrupt this synthetic executor"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Wait for the original door to exit; an overlapping click only observes.
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, owns := work.DetachedDoorOwns(homePath, w.ID); !owns {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	invoke("--verification", "check:synthetic-first-attempt-only")
	resumed := invoke("--resume")
	if resumed.Action != "execute" || resumed.Detached == nil {
		t.Fatalf("resume not launched: %+v", resumed)
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(readyPath); err != nil {
		t.Fatal("resumed executor did not start")
	}
	duplicate := invoke("--resume")
	if duplicate.Action != "none" || duplicate.Detached == nil || duplicate.Detached.LaunchID != resumed.Detached.LaunchID {
		t.Fatalf("duplicate click launched again: %+v", duplicate)
	}
	if err := syscall.Kill(-resumed.Detached.PGID, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		final, err = store.Get(w.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.State == "verifying" && len(final.RunAttemptIDs) == firstCount+1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if final.State != "verifying" || len(final.RunAttemptIDs) != firstCount+1 {
		t.Fatalf("interruption lost recoverable work: %+v", final)
	}
	interrupted, err := runattempt.Open(homePath).Get(final.RunAttemptIDs[len(final.RunAttemptIDs)-1])
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.Status != runattempt.StatusFailed || interrupted.PredecessorID != attempt.ID {
		t.Fatalf("interruption lost lineage: %+v", interrupted)
	}
	if err := os.Remove(blockPath); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, owns := work.DetachedDoorOwns(homePath, w.ID); !owns {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	recovered := invoke("--resume")
	if recovered.Action != "execute" {
		t.Fatalf("recovery refused: %+v", recovered)
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		final, err = store.Get(w.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.State == "verifying" && len(final.RunAttemptIDs) == firstCount+2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if final.State != "verifying" || len(final.RunAttemptIDs) != firstCount+2 {
		t.Fatalf("recovery did not finish: %+v", final)
	}
	last, err := runattempt.Open(homePath).Get(final.RunAttemptIDs[len(final.RunAttemptIDs)-1])
	if err != nil {
		t.Fatal(err)
	}
	if last.Status != runattempt.StatusSucceeded || last.PredecessorID != interrupted.ID || last.ExecutionSelection == nil {
		t.Fatalf("recovery lost identity or lineage: %+v", last)
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, owns := work.DetachedDoorOwns(homePath, w.ID); !owns {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Execution alone must not satisfy the same final proof gate. The verifier
	// records the inspected transcript using the public lifecycle owner only
	// after the assertions above established this synthetic task's contract.
	noProof := invoke("--outcome", "completed", "--move-type", "audit_only", "--summary", "synthetic journey verification")
	if noProof.Action == "close" {
		t.Fatal("subprocess success was silently substituted for verification proof")
	}
	replayed := invoke("--verification", "check:synthetic-first-attempt-only", "--outcome", "completed", "--move-type", "audit_only", "--summary", "replayed old check must not accept newer execution")
	if replayed.Action == "close" {
		t.Fatal("reattaching an old check was substituted for fresh verification")
	}
	accepted := invoke("--verification", "check:synthetic-goal-selection-isolation-and-recovery", "--outcome", "completed", "--move-type", "audit_only", "--summary", "synthetic journey contract verified; real product benefit not measured")
	if accepted.Action != "close" || accepted.Status != "completed" || accepted.SelectedClose == nil || accepted.SelectedClose.ReceiptID == "" || accepted.SelectedClose.TransitionID == "" {
		t.Fatalf("verified result did not close truthfully: %+v", accepted)
	}
	report, err := store.ProofChainDoctor(time.Now().UTC(), work.ProofChainDoctorOptions{})
	if err != nil || report.Status != "ok" {
		t.Fatalf("recovery broke proof chain: %v %+v", err, report)
	}
	archived, err := os.ReadDir(filepath.Join(work.DetachedLaunchDir(homePath), "history"))
	wantArchived := 2
	if interruptFirst {
		wantArchived++
	}
	if err != nil || len(archived) != wantArchived {
		t.Fatalf("prior launch evidence was overwritten: %v %+v", err, archived)
	}
}

func TestSelectedDoorRejectsStaleSourceBeforeAnyExecution(t *testing.T) {
	f := newResumeFixture(t, "stale selected source cannot execute")
	before := resumeEnvelopeBytes(t, f)
	out, err := resumeCommand(t, f, "--to", "running", "--execute", "--by", "codex", "--harness", "os-harness-entrypoints", "--expected-work-revision", strings.Repeat("0", 64), "--", "/usr/bin/true")
	if err == nil || !strings.Contains(out, "selected work changed") {
		t.Fatalf("stale selected source accepted: %v\n%s", err, out)
	}
	if !bytes.Equal(before, resumeEnvelopeBytes(t, f)) {
		t.Fatal("stale selected door changed work")
	}
}
