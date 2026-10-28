package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	sweepStaleCLIChildArgsEnv                  = "COMPOUND_TEST_SWEEP_STALE_CLI_ARGS"
	experimentalSweepStaleMutationsFlagForTest = "--experimental-sweep-stale-mutations"
)

func TestWorkSweepStaleAdmissionUsesValidatedParser(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "valid gated yes", args: []string{"--yes", experimentalSweepStaleMutationsFlagForTest}, want: true},
		{name: "valid gated yes with values", args: []string{"--threshold", "1h", "--json", experimentalSweepStaleMutationsFlagForTest, "--yes"}, want: true},
		{name: "ungated yes", args: []string{"--yes"}},
		{name: "default dry-run"},
		{name: "explicit dry-run", args: []string{"--dry-run"}},
		{name: "gate without yes", args: []string{experimentalSweepStaleMutationsFlagForTest}},
		{name: "conflicting modes", args: []string{"--dry-run", "--yes", experimentalSweepStaleMutationsFlagForTest}},
		{name: "invalid threshold", args: []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--threshold", "invalid"}},
		{name: "missing threshold value", args: []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--threshold"}},
		{name: "empty threshold value", args: []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--threshold", ""}},
		{name: "invalid then valid duplicate threshold", args: []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--threshold", "invalid", "--threshold", "1h"}},
		{name: "valid duplicate threshold", args: []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--threshold", "1h", "--threshold", "2h"}},
		{name: "zero threshold", args: []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--threshold", "0s"}},
		{name: "negative threshold", args: []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--threshold", "-1s"}},
		{name: "unknown flag", args: []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--unknown"}},
		{name: "stray positional", args: []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "work_not_a_sweep_target"}},
		{name: "gate token is a threshold value", args: []string{"--yes", "--threshold", experimentalSweepStaleMutationsFlagForTest}},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := workCommandNeedsWritableStore("sweep-stale", test.args); got != test.want {
				t.Fatalf("workCommandNeedsWritableStore(sweep-stale, %q) = %t, want %t", test.args, got, test.want)
			}
		})
	}
}

func TestWorkSweepStaleCLIOnlyValidExperimentalApplyMaterializesHome(t *testing.T) {
	t.Parallel()

	if rawArgs := os.Getenv(sweepStaleCLIChildArgsEnv); rawArgs != "" {
		var args []string
		if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
			t.Fatal(err)
		}
		cmdWork(append([]string{"sweep-stale"}, args...))
		return
	}

	tests := []struct {
		name         string
		args         []string
		wantExit     int
		wantOutput   string
		wantMutation bool
	}{
		{name: "default", wantExit: 0, wantOutput: "sweep-stale dry-run"},
		{name: "dry-run", args: []string{"--dry-run"}, wantExit: 0, wantOutput: "sweep-stale dry-run"},
		{name: "ungated yes", args: []string{"--yes"}, wantExit: 2, wantOutput: experimentalSweepStaleMutationsFlagForTest},
		{name: "conflicting modes", args: []string{"--dry-run", "--yes", experimentalSweepStaleMutationsFlagForTest}, wantExit: 2, wantOutput: "mutually exclusive"},
		{name: "gate without yes", args: []string{experimentalSweepStaleMutationsFlagForTest}, wantExit: 2, wantOutput: "requires --yes"},
		{name: "invalid threshold", args: []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--threshold", "invalid"}, wantExit: 2, wantOutput: "not a valid duration"},
		{name: "missing threshold value", args: []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--threshold"}, wantExit: 2, wantOutput: "--threshold requires a value"},
		{name: "empty threshold value", args: []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--threshold", ""}, wantExit: 2, wantOutput: "not a valid duration"},
		{name: "invalid then valid duplicate threshold", args: []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--threshold", "invalid", "--threshold", "1h"}, wantExit: 2, wantOutput: "may only be provided once"},
		{name: "valid duplicate threshold", args: []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--threshold", "1h", "--threshold", "2h"}, wantExit: 2, wantOutput: "may only be provided once"},
		{name: "zero threshold", args: []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--threshold", "0s"}, wantExit: 2, wantOutput: "greater than zero"},
		{name: "negative threshold", args: []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--threshold", "-1s"}, wantExit: 2, wantOutput: "greater than zero"},
		{name: "unknown flag", args: []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--unknown"}, wantExit: 2, wantOutput: "unexpected argument"},
		{name: "stray positional", args: []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "work_not_a_sweep_target"}, wantExit: 2, wantOutput: "unexpected argument"},
		{name: "gate token as value", args: []string{"--yes", "--threshold", experimentalSweepStaleMutationsFlagForTest}, wantExit: 2, wantOutput: "not a valid duration"},
		{name: "valid gated apply", args: []string{"--yes", experimentalSweepStaleMutationsFlagForTest}, wantExit: 0, wantOutput: "sweep-stale applied", wantMutation: true},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			homePath := initializedTestHome(t)
			for _, relative := range []string{"work", "events"} {
				if err := os.RemoveAll(filepath.Join(homePath, relative)); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Remove(filepath.Join(homePath, "lock", "land-lease.lock")); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			before := snapshotSweepStaleCLIHome(t, homePath)

			encodedArgs, err := json.Marshal(test.args)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestWorkSweepStaleCLIOnlyValidExperimentalApplyMaterializesHome$")
			cmd.Env = append(os.Environ(),
				"COMPOUND_HOME="+homePath,
				sweepStaleCLIChildArgsEnv+"="+string(encodedArgs),
			)
			output, runErr := cmd.CombinedOutput()
			gotExit := 0
			if runErr != nil {
				var exitErr *exec.ExitError
				if !errors.As(runErr, &exitErr) {
					t.Fatalf("sweep-stale subprocess: %v\n%s", runErr, output)
				}
				gotExit = exitErr.ExitCode()
			}
			if gotExit != test.wantExit || !strings.Contains(string(output), test.wantOutput) {
				t.Fatalf("sweep-stale %s exit=%d output=%q, want exit=%d containing %q", test.name, gotExit, output, test.wantExit, test.wantOutput)
			}

			after := snapshotSweepStaleCLIHome(t, homePath)
			if test.wantMutation {
				if reflect.DeepEqual(before, after) {
					t.Fatalf("valid gated apply did not open writable work store")
				}
				for _, relative := range []string{"work/work", "work/leases", "events"} {
					if after[relative] != "dir" {
						t.Fatalf("valid gated apply did not materialize %s: %v", relative, after)
					}
				}
				return
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("sweep-stale %s mutated COMPOUND_HOME before admission:\nbefore=%v\nafter=%v", test.name, before, after)
			}
		})
	}
}

func snapshotSweepStaleCLIHome(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if entry.IsDir() {
			result[filepath.ToSlash(relative)] = "dir"
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[filepath.ToSlash(relative)] = "file:" + string(body)
		return nil
	}); err != nil {
		t.Fatalf("snapshot sweep-stale home: %v", err)
	}
	return result
}
