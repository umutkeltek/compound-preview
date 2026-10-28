package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/runattempt"
	"compound/internal/work"
)

const delegateNoImplicitSweepChildEnv = "COMPOUND_TEST_DELEGATE_NO_IMPLICIT_SWEEP"
const delegateNoImplicitSweepModeEnv = "COMPOUND_TEST_DELEGATE_NO_IMPLICIT_SWEEP_MODE"
const delegateNoFootprintChildEnv = "COMPOUND_TEST_DELEGATE_NO_FOOTPRINT"

func TestWorkDelegateReadOnlyAndUnadmittedCommandsDoNotMaterializeHome(t *testing.T) {
	t.Parallel()
	if mode := os.Getenv(delegateNoFootprintChildEnv); mode != "" {
		const missingWorkID = "work_delegate_no_footprint"
		var args []string
		switch mode {
		case "status":
			args = []string{"delegate", "status", missingWorkID, "--json"}
		case "attach":
			args = []string{"attach", missingWorkID}
		case "delegate-dry-run":
			args = []string{"delegate", missingWorkID, "--harness", "claude-zai", "--dry-run", "--json"}
		case "delegate-default-off":
			args = []string{"delegate", missingWorkID, "--harness", "claude-zai", "--json"}
		case "sweep-dry-run":
			args = []string{"delegate", "sweep", missingWorkID, "--dry-run", "--json"}
		case "progress-default-off":
			args = []string{"delegate", "progress", missingWorkID, "--done", "0", "--empty", "0", "--failed", "0", "--remaining", "0", "--json"}
		case "steer-default-off":
			args = []string{"steer", missingWorkID, "--text", "do not send", "--json"}
		case "delegate-gate-is-harness-value":
			args = []string{"delegate", missingWorkID, "--harness", work.ExperimentalDelegateMutationsFlag}
		case "delegate-gate-is-raw-command":
			args = []string{"delegate", missingWorkID, "--harness", "claude-zai", "--", work.ExperimentalDelegateMutationsFlag}
		case "steer-gate-is-text-value":
			args = []string{"steer", missingWorkID, "--text", work.ExperimentalDelegateMutationsFlag}
		case "progress-gate-is-count-value":
			args = []string{"delegate", "progress", missingWorkID, "--done", work.ExperimentalDelegateMutationsFlag, "--empty", "0", "--failed", "0", "--remaining", "0"}
		case "sweep-gate-with-unknown-flag":
			args = []string{"delegate", "sweep", missingWorkID, work.ExperimentalDelegateMutationsFlag, "--unknown"}
		default:
			t.Fatalf("unknown delegate no-footprint child mode %q", mode)
		}
		cmdWork(args)
		return
	}

	for _, tc := range []struct {
		mode     string
		wantExit int
		wantText string
	}{
		{mode: "status", wantExit: 0, wantText: `"bound": false`},
		{mode: "attach", wantExit: 1, wantText: "error:"},
		{mode: "delegate-dry-run", wantExit: 1, wantText: `"reason": "harness_unknown"`},
		{mode: "delegate-default-off", wantExit: 2, wantText: "unsupported in v0.1"},
		{mode: "sweep-dry-run", wantExit: 0, wantText: `"bound": false`},
		{mode: "progress-default-off", wantExit: 2, wantText: "unsupported in v0.1"},
		{mode: "steer-default-off", wantExit: 2, wantText: "unsupported in v0.1"},
		{mode: "delegate-gate-is-harness-value", wantExit: 2, wantText: "unsupported in v0.1"},
		{mode: "delegate-gate-is-raw-command", wantExit: 2, wantText: "unsupported in v0.1"},
		{mode: "steer-gate-is-text-value", wantExit: 2, wantText: "unsupported in v0.1"},
		{mode: "progress-gate-is-count-value", wantExit: 2, wantText: "--done must be a non-negative integer"},
		{mode: "sweep-gate-with-unknown-flag", wantExit: 2, wantText: "unknown flag --unknown"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			homePath := t.TempDir()
			if err := home.Init(homePath, "test", time.Date(2026, 8, 30, 1, 0, 0, 0, time.UTC)); err != nil {
				t.Fatal(err)
			}
			for _, rel := range []string{"work", "events"} {
				if err := os.RemoveAll(filepath.Join(homePath, rel)); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotDelegateCommandTree(t, homePath)
			cmd := exec.Command(os.Args[0], "-test.run=^TestWorkDelegateReadOnlyAndUnadmittedCommandsDoNotMaterializeHome$")
			cmd.Env = append(os.Environ(),
				"COMPOUND_HOME="+homePath,
				delegateNoFootprintChildEnv+"="+tc.mode,
			)
			output, runErr := cmd.CombinedOutput()
			gotExit := 0
			if runErr != nil {
				var exitErr *exec.ExitError
				if !errors.As(runErr, &exitErr) {
					t.Fatalf("delegate %s subprocess: %v\n%s", tc.mode, runErr, output)
				}
				gotExit = exitErr.ExitCode()
			}
			if gotExit != tc.wantExit || !strings.Contains(string(output), tc.wantText) {
				t.Fatalf("delegate %s exit=%d output=%q, want exit=%d containing %q", tc.mode, gotExit, output, tc.wantExit, tc.wantText)
			}
			after := snapshotDelegateCommandTree(t, homePath)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("delegate %s materialized COMPOUND_HOME:\nbefore=%v\nafter=%v", tc.mode, before, after)
			}
		})
	}
}

func snapshotDelegateCommandTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.IsDir() {
			out[rel] = "dir"
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = "file:" + string(body)
		return nil
	}); err != nil {
		t.Fatalf("snapshot delegate command tree: %v", err)
	}
	return out
}

// Delegate cleanup has an explicit owner (`compound run-attempt sweep`). No
// delegate command may opportunistically mutate unrelated attempts, including
// an explicitly opted-in invocation that parses but fails semantic preflight.
// This subprocess exercises the real cmdWork wiring above the parser seam.
func TestWorkDelegateCommandsNeverImplicitlySweepRunAttempts(t *testing.T) {
	t.Parallel()
	if workID := os.Getenv(delegateNoImplicitSweepChildEnv); workID != "" {
		var args []string
		switch os.Getenv(delegateNoImplicitSweepModeEnv) {
		case "status":
			args = []string{"delegate", "status", workID, "--json"}
		case "dry-run":
			args = []string{"delegate", workID, "--harness", "claude-zai", "--dry-run", "--json"}
		case "default-off":
			args = []string{"delegate", workID, "--harness", "claude-zai", "--json"}
		case "semantic-refusal":
			args = []string{"delegate", workID, "--harness", "not-a-real-harness", work.ExperimentalDelegateMutationsFlag, "--json"}
		default:
			t.Fatal("unknown delegate no-sweep child mode")
		}
		cmdWork(args)
		return
	}

	tests := []struct {
		mode     string
		wantExit int
	}{
		{mode: "status", wantExit: 0},
		{mode: "dry-run", wantExit: 1},
		{mode: "default-off", wantExit: 2},
		{mode: "semantic-refusal", wantExit: 1},
	}
	for _, tc := range tests {
		t.Run(tc.mode, func(t *testing.T) {
			now := time.Now().UTC()
			homePath := t.TempDir()
			if err := home.Init(homePath, "test", now); err != nil {
				t.Fatalf("init home: %v", err)
			}
			store, err := work.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			w, err := store.Create(work.CreateInput{
				Owner: "orchestrator:test", SuccessPredicate: "delegate command cannot sweep unrelated attempts", State: "ready",
			}, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Claim(w.ID, "orchestrator:test", now); err != nil {
				t.Fatal(err)
			}
			stale, err := runattempt.Open(homePath).Mint(
				w.ID,
				"orchestrator:test",
				"claude-zai",
				now.Add(-runattempt.DefaultSweepThreshold-time.Hour),
			)
			if err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(os.Args[0], "-test.run=^TestWorkDelegateCommandsNeverImplicitlySweepRunAttempts$")
			cmd.Env = append(os.Environ(),
				"COMPOUND_HOME="+homePath,
				delegateNoImplicitSweepChildEnv+"="+w.ID,
				delegateNoImplicitSweepModeEnv+"="+tc.mode,
			)
			output, runErr := cmd.CombinedOutput()
			gotExit := 0
			if runErr != nil {
				exitErr, ok := runErr.(*exec.ExitError)
				if !ok {
					t.Fatalf("delegate %s subprocess: %v\n%s", tc.mode, runErr, output)
				}
				gotExit = exitErr.ExitCode()
			}
			if gotExit != tc.wantExit {
				t.Fatalf("exit=%d want=%d err=%v\n%s", gotExit, tc.wantExit, runErr, output)
			}
			got, err := runattempt.Open(homePath).Get(stale.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != runattempt.StatusPending {
				t.Fatalf("delegate %s changed unrelated stale attempt %s from pending to %s\n%s", tc.mode, stale.ID, got.Status, output)
			}
		})
	}
}
