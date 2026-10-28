package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
	"compound/internal/work/executewire"
)

// runExecuteCapturingExit runs one in-process `work advance --execute` and
// returns its stdout plus the exit code the governed door requested (0 when
// it returned normally).
func runExecuteCapturingExit(t *testing.T, args []string) (string, int) {
	t.Helper()
	type exitSignal struct{ code int }
	originalExit := workAdvanceExecuteExit
	workAdvanceExecuteExit = func(code int) { panic(exitSignal{code: code}) }
	t.Cleanup(func() { workAdvanceExecuteExit = originalExit })
	exitCode := 0
	out := captureStdout(t, func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				signal, ok := recovered.(exitSignal)
				if !ok {
					panic(recovered)
				}
				exitCode = signal.code
			}
		}()
		cmdWork(args)
	})
	return out, exitCode
}

// A governed run that outlives the unrequested default budget must succeed
// when the caller requested a longer budget, and the same run without the
// request must still die at the default. Before --timeout existed every
// governed lane was killed at exactly the 5-minute default (median real lane
// ~577s), so a product lane could never finish its note or outcome.json.
func TestWorkAdvanceExecuteHonoursRequestedTimeout(t *testing.T) {
	previousDefault := executewire.DefaultLaunchTimeout
	executewire.DefaultLaunchTimeout = 300 * time.Millisecond
	t.Cleanup(func() { executewire.DefaultLaunchTimeout = previousDefault })

	requested := newClaimedExecutionFixture(t, "codex", "requested budget outlives the default")
	out, code := runExecuteCapturingExit(t, []string{
		"advance", requested.workID, "--to", "running", "--execute",
		"--harness", "os-harness-entrypoints", "--timeout", "20s",
		"--", "sleep", "1",
	})
	if code != 0 || !strings.Contains(out, "status: succeeded") || strings.Contains(out, "timed_out: true") {
		t.Fatalf("requested --timeout 20s run: exit=%d, want succeeded without timeout:\n%s", code, out)
	}
	if final, err := requested.store.Get(requested.workID); err != nil || final.State != "verifying" {
		t.Fatalf("requested-budget work state = %+v err=%v, want verifying", final.State, err)
	}
	if attempt := requested.executionAttempt(t); attempt.Status != runattempt.StatusSucceeded {
		t.Fatalf("requested-budget attempt status = %q, want succeeded", attempt.Status)
	}

	unrequested := newClaimedExecutionFixture(t, "codex", "unrequested budget stays the default")
	out, code = runExecuteCapturingExit(t, []string{
		"advance", unrequested.workID, "--to", "running", "--execute",
		"--harness", "os-harness-entrypoints",
		"--", "sleep", "1",
	})
	if code == 0 || !strings.Contains(out, "timed_out: true") {
		t.Fatalf("run without --timeout must still time out at the default budget: exit=%d\n%s", code, out)
	}
	attempt := unrequested.executionAttempt(t)
	if attempt.Status != runattempt.StatusFailed || !strings.Contains(strings.Join(attempt.ProofRefs, " "), "timeout:exceeded:300ms") {
		t.Fatalf("unrequested attempt = status %q proof %v, want failed with timeout:exceeded:300ms", attempt.Status, attempt.ProofRefs)
	}
}

// A malformed or negative --timeout is a usage refusal before any state
// change: the envelope stays claimed and no execution RunAttempt is minted.
func TestWorkAdvanceExecuteRejectsMalformedTimeoutBeforeStateChange(t *testing.T) {
	for _, raw := range []string{"forty", "-5m"} {
		fixture := newClaimedExecutionFixture(t, "codex", "malformed budget never launches")
		before, err := fixture.store.Get(fixture.workID)
		if err != nil {
			t.Fatal(err)
		}
		_, code := runExecuteCapturingExit(t, []string{
			"advance", fixture.workID, "--to", "running", "--execute",
			"--harness", "os-harness-entrypoints", "--timeout", raw,
			"--", "true",
		})
		if code != 2 {
			t.Fatalf("--timeout %q exit = %d, want usage exit 2", raw, code)
		}
		after, err := fixture.store.Get(fixture.workID)
		if err != nil {
			t.Fatal(err)
		}
		if after.State != "claimed" || len(after.RunAttemptIDs) != len(before.RunAttemptIDs) {
			t.Fatalf("--timeout %q moved the envelope: state=%s attempts=%v (before %v)", raw, after.State, after.RunAttemptIDs, before.RunAttemptIDs)
		}
	}
}

// --timeout only means something to the governed execute door; a plain
// lifecycle advance must refuse it instead of silently ignoring a budget.
func TestWorkAdvanceTimeoutRequiresExecute(t *testing.T) {
	fixture := newClaimedExecutionFixture(t, "codex", "timeout without execute is a usage error")
	cmd := exec.Command(compoundTestBinary(t), "work", "advance", fixture.workID, "--to", "running", "--by", "codex", "--timeout", "40m")
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+fixture.homePath)
	out, err := cmd.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 2 || !strings.Contains(string(out), "--timeout requires --execute") {
		t.Fatalf("work advance --timeout without --execute: err=%v\n%s", err, out)
	}
	if final, err := fixture.store.Get(fixture.workID); err != nil || final.State != "claimed" {
		t.Fatalf("refused advance moved the envelope: state=%v err=%v", final.State, err)
	}
}
