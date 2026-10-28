package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
	"compound/internal/work/executewire"
)

func TestWorkAdvanceExecuteTimeoutArgumentOwnership(t *testing.T) {
	previousDefault := executewire.DefaultLaunchTimeout
	executewire.DefaultLaunchTimeout = 300 * time.Millisecond
	t.Cleanup(func() { executewire.DefaultLaunchTimeout = previousDefault })

	for _, tc := range []struct {
		name          string
		parentTimeout string
		childTimeout  string
		wantExit      int
		wantTimeout   string
	}{
		{"child numeric", "", "30", 1, "300ms"},
		{"child duration", "", "30s", 1, "300ms"},
		{"child invalid duration", "", "not-a-duration", 1, "300ms"},
		{"parent only", "20s", "", 0, ""},
		{"both numeric child", "20s", "30", 0, ""},
		{"both duration child", "400ms", "30s", 1, "400ms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newClaimedExecutionFixture(t, "codex", "child options retain their owner")
			args := []string{"advance", fixture.workID, "--to", "running", "--execute", "--harness", "os-harness-entrypoints"}
			if tc.parentTimeout != "" {
				args = append(args, "--timeout", tc.parentTimeout)
			}
			// Print each argument before waiting past the shortened default.
			// Empty strings, spaces and another -- must survive the CLI boundary.
			args = append(args, "--", "sh", "-c", "printf '<%s>\\n' \"$@\"; sleep 1", "synthetic-tool")
			childArgs := []string{"", "two words", "--", "--by", "child-actor", "--run-attempt", "child-attempt", "--launch-profile", "child-profile", "--prompt", "child prompt"}
			if tc.childTimeout != "" {
				childArgs = append(childArgs, "--timeout", tc.childTimeout)
			}
			args = append(args, childArgs...)
			out, code := runExecuteCapturingExit(t, args)
			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d; output: %s", code, tc.wantExit, out)
			}
			attempt := fixture.executionAttempt(t)
			transcript, err := os.ReadFile(attempt.TranscriptRef)
			if err != nil {
				t.Fatal(err)
			}
			wantArgv := "stdout: <" + strings.Join(childArgs, ">\nstdout: <") + ">\n"
			if !strings.Contains(string(transcript), wantArgv) {
				t.Fatalf("child arguments changed: want %q in %q", wantArgv, transcript)
			}
			if tc.wantTimeout != "" {
				if attempt.Status != runattempt.StatusFailed || !strings.Contains(strings.Join(attempt.ProofRefs, " "), "timeout:exceeded:"+tc.wantTimeout) {
					t.Fatalf("child timeout affected parent: status=%s proofs=%v; want %s", attempt.Status, attempt.ProofRefs, tc.wantTimeout)
				}
			} else if attempt.Status != runattempt.StatusSucceeded {
				t.Fatalf("parent timeout was not honored: status=%s proofs=%v", attempt.Status, attempt.ProofRefs)
			}
		})
	}
}

func TestWorkAdvanceExecuteInvalidParentTimeoutWithChildTimeout(t *testing.T) {
	fixture := newClaimedExecutionFixture(t, "codex", "invalid parent timeout never launches")
	before, err := fixture.store.Get(fixture.workID)
	if err != nil {
		t.Fatal(err)
	}
	_, code := runExecuteCapturingExit(t, []string{
		"advance", fixture.workID, "--to", "running", "--execute", "--harness", "os-harness-entrypoints",
		"--timeout", "invalid", "--", "true", "--timeout", "30s",
	})
	if code != 2 {
		t.Fatalf("exit = %d, want usage exit 2", code)
	}
	after, err := fixture.store.Get(fixture.workID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != before.State || after.PolicyDecisionID != before.PolicyDecisionID || len(after.RunAttemptIDs) != len(before.RunAttemptIDs) || len(after.ActionIntentIDs) != len(before.ActionIntentIDs) {
		t.Fatalf("invalid parent timeout changed work: before=%+v after=%+v", before, after)
	}
}
