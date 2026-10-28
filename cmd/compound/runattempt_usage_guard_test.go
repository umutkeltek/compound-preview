package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
)

const runAttemptUsageChildArgsEnv = "COMPOUND_TEST_RUN_ATTEMPT_USAGE_ARGS"
const runAttemptUnreadableSweepChildEnv = "COMPOUND_TEST_RUN_ATTEMPT_UNREADABLE_SWEEP"
const runAttemptUnreadableListChildEnv = "COMPOUND_TEST_RUN_ATTEMPT_UNREADABLE_LIST"

func TestRunAttemptListReportsReadablePartialBeforeNonzeroExit(t *testing.T) {
	t.Parallel()
	if os.Getenv(runAttemptUnreadableListChildEnv) != "" {
		cmdRunAttempt([]string{"list", "--non-terminal", "--json"})
		return
	}

	homePath := initializedTestHome(t)
	store := runattempt.Open(homePath)
	unreadable, err := store.Mint("work_unreadable_list_cli", "actor:test", "claude-code", cliTestNow.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitLaunch(unreadable.ID, cliTestNow.Add(-time.Hour), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	preparedPath := filepath.Join(homePath, "run_attempts", "launch", unreadable.ID+".prepared.json")
	if err := os.Chmod(preparedPath, 0o644); err != nil {
		t.Fatal(err)
	}
	readable, err := store.Mint("work_readable_list_cli", "actor:test", "claude-code", cliTestNow.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunAttemptListReportsReadablePartialBeforeNonzeroExit$")
	cmd.Env = append(os.Environ(),
		"COMPOUND_HOME="+homePath,
		runAttemptUnreadableListChildEnv+"=1",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	exitErr, ok := runErr.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("list exit error = %v, stdout=%q stderr=%q; want exit 1", runErr, stdout.String(), stderr.String())
	}
	var attempts []runattempt.RunAttempt
	if err := json.Unmarshal(stdout.Bytes(), &attempts); err != nil {
		t.Fatalf("decode partial list: %v\nstdout=%q\nstderr=%q", err, stdout.String(), stderr.String())
	}
	if len(attempts) != 1 || attempts[0].ID != readable.ID {
		t.Fatalf("partial list = %+v, want readable attempt %s", attempts, readable.ID)
	}
	if !strings.Contains(stderr.String(), unreadable.ID) {
		t.Fatalf("list stderr omitted unreadable attempt id: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), homePath) {
		t.Fatalf("list stderr leaked private home path: %q", stderr.String())
	}
}

func TestRunAttemptSweepReportsUnreadableAttemptsBeforeNonzeroExit(t *testing.T) {
	t.Parallel()
	if os.Getenv(runAttemptUnreadableSweepChildEnv) != "" {
		cmdRunAttempt([]string{"sweep", "--json", "--threshold", "1m"})
		return
	}

	homePath := initializedTestHome(t)
	store := runattempt.Open(homePath)
	attempt, err := store.Mint("work_unreadable_cli", "actor:test", "claude-code", cliTestNow.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitLaunch(attempt.ID, cliTestNow.Add(-time.Hour), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	preparedPath := filepath.Join(homePath, "run_attempts", "launch", attempt.ID+".prepared.json")
	if err := os.Chmod(preparedPath, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunAttemptSweepReportsUnreadableAttemptsBeforeNonzeroExit$")
	cmd.Env = append(os.Environ(),
		"COMPOUND_HOME="+homePath,
		runAttemptUnreadableSweepChildEnv+"=1",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	exitErr, ok := runErr.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("sweep exit error = %v, stdout=%q stderr=%q; want exit 1", runErr, stdout.String(), stderr.String())
	}
	var report runattempt.SweepReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode partial sweep report: %v\nstdout=%q\nstderr=%q", err, stdout.String(), stderr.String())
	}
	if report.Scanned != 1 || report.Skipped != 1 || report.Unreadable != 1 || len(report.UnreadableIDs) != 1 || report.UnreadableIDs[0] != attempt.ID {
		t.Fatalf("partial sweep report = %+v, want unreadable attempt %s", report, attempt.ID)
	}
	if !strings.Contains(stderr.String(), attempt.ID) {
		t.Fatalf("sweep stderr omitted unreadable attempt id: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), homePath) {
		t.Fatalf("sweep stderr leaked private home path: %q", stderr.String())
	}
}

// A mutating sweep must treat help as read-only and malformed arguments as a
// usage error. Every case runs against a real stale attempt: if validation
// happens after the sweep, the stored status exposes the mutation even when the
// command printed a plausible banner or error.
func TestRunAttemptSweepValidatesArgumentsBeforeMutation(t *testing.T) {
	t.Parallel()
	if encoded := os.Getenv(runAttemptUsageChildArgsEnv); encoded != "" {
		raw, err := base64.RawStdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		var args []string
		if err := json.Unmarshal(raw, &args); err != nil {
			t.Fatal(err)
		}
		cmdRunAttempt(args)
		return
	}

	tests := []struct {
		name     string
		args     []string
		wantExit int
	}{
		{name: "long help", args: []string{"sweep", "--help"}, wantExit: 0},
		{name: "short help", args: []string{"sweep", "-h"}, wantExit: 0},
		{name: "unknown flag", args: []string{"sweep", "--bogus"}, wantExit: 2},
		{name: "threshold missing", args: []string{"sweep", "--threshold"}, wantExit: 2},
		{name: "threshold consumes flag", args: []string{"sweep", "--threshold", "--json"}, wantExit: 2},
		{name: "home missing", args: []string{"sweep", "--home"}, wantExit: 2},
		{name: "home consumes flag", args: []string{"sweep", "--home", "--json"}, wantExit: 2},
		{name: "negative threshold", args: []string{"sweep", "--threshold", "-5m"}, wantExit: 2},
		{name: "zero threshold", args: []string{"sweep", "--threshold", "0"}, wantExit: 2},
		{name: "unexpected positional", args: []string{"sweep", "surprise"}, wantExit: 2},
		{name: "list unknown flag", args: []string{"list", "--bogus"}, wantExit: 2},
		{name: "list home missing", args: []string{"list", "--home"}, wantExit: 2},
		{name: "list home consumes flag", args: []string{"list", "--home", "--json"}, wantExit: 2},
		{name: "list unexpected positional", args: []string{"list", "surprise"}, wantExit: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := initializedTestHome(t)
			store := runattempt.Open(homePath)
			attempt, err := store.Mint("work_guard", "actor:test", "claude-code", cliTestNow.Add(-24*time.Hour))
			if err != nil {
				t.Fatalf("mint attempt: %v", err)
			}

			raw, err := json.Marshal(tt.args)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestRunAttemptSweepValidatesArgumentsBeforeMutation$")
			cmd.Env = append(os.Environ(),
				"COMPOUND_HOME="+homePath,
				runAttemptUsageChildArgsEnv+"="+base64.RawStdEncoding.EncodeToString(raw),
			)
			output, runErr := cmd.CombinedOutput()
			gotExit := 0
			if runErr != nil {
				exitErr, ok := runErr.(*exec.ExitError)
				if !ok {
					t.Fatalf("run child: %v\n%s", runErr, output)
				}
				gotExit = exitErr.ExitCode()
			}
			if gotExit != tt.wantExit {
				t.Fatalf("exit=%d, want %d; output:\n%s", gotExit, tt.wantExit, output)
			}
			stored, err := store.Get(attempt.ID)
			if err != nil {
				t.Fatalf("read attempt: %v", err)
			}
			if stored.Status != runattempt.StatusPending {
				t.Fatalf("%v mutated stale attempt to %q; output:\n%s", tt.args, stored.Status, output)
			}
			if tt.wantExit == 2 && !strings.Contains(string(output), "usage: compound run-attempt") {
				t.Fatalf("usage error omitted command help:\n%s", output)
			}
		})
	}
}
