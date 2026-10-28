package work

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

const experimentalSweepStaleMutationsFlagForTest = "--experimental-sweep-stale-mutations"

func TestRunWorkSweepStaleCLIParsesExperimentalMutationGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantOutput string
	}{
		{
			name:       "default remains dry-run",
			wantCode:   0,
			wantOutput: "sweep-stale dry-run",
		},
		{
			name:       "explicit dry-run remains read-only",
			args:       []string{"--dry-run"},
			wantCode:   0,
			wantOutput: "sweep-stale dry-run",
		},
		{
			name:       "ungated yes is refused",
			args:       []string{"--yes"},
			wantCode:   2,
			wantOutput: experimentalSweepStaleMutationsFlagForTest,
		},
		{
			name:       "dry-run then yes is conflicting",
			args:       []string{"--dry-run", "--yes", experimentalSweepStaleMutationsFlagForTest},
			wantCode:   2,
			wantOutput: "mutually exclusive",
		},
		{
			name:       "yes then dry-run is conflicting",
			args:       []string{"--yes", "--dry-run", experimentalSweepStaleMutationsFlagForTest},
			wantCode:   2,
			wantOutput: "mutually exclusive",
		},
		{
			name:       "experimental gate without yes is refused",
			args:       []string{experimentalSweepStaleMutationsFlagForTest},
			wantCode:   2,
			wantOutput: "requires --yes",
		},
		{
			name:       "invalid threshold remains a parser error",
			args:       []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--threshold", "not-a-duration"},
			wantCode:   2,
			wantOutput: "not a valid duration",
		},
		{
			name:       "missing threshold value remains a parser error",
			args:       []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--threshold"},
			wantCode:   2,
			wantOutput: "--threshold requires a value",
		},
		{
			name:       "empty threshold value remains a parser error",
			args:       []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--threshold", ""},
			wantCode:   2,
			wantOutput: "not a valid duration",
		},
		{
			name:       "invalid then valid duplicate threshold is rejected",
			args:       []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--threshold", "invalid", "--threshold", "1h"},
			wantCode:   2,
			wantOutput: "may only be provided once",
		},
		{
			name:       "valid duplicate threshold is rejected",
			args:       []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--threshold", "1h", "--threshold", "2h"},
			wantCode:   2,
			wantOutput: "may only be provided once",
		},
		{
			name:       "zero threshold cannot admit mutation",
			args:       []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--threshold", "0s"},
			wantCode:   2,
			wantOutput: "greater than zero",
		},
		{
			name:       "negative threshold cannot admit mutation",
			args:       []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--threshold", "-1s"},
			wantCode:   2,
			wantOutput: "greater than zero",
		},
		{
			name:       "unknown flag cannot admit mutation",
			args:       []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--unknown"},
			wantCode:   2,
			wantOutput: "unexpected argument",
		},
		{
			name:       "stray positional cannot admit mutation",
			args:       []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "work_not_a_sweep_target"},
			wantCode:   2,
			wantOutput: "unexpected argument",
		},
		{
			name:       "gate token consumed as threshold value is not authority",
			args:       []string{"--yes", "--threshold", experimentalSweepStaleMutationsFlagForTest},
			wantCode:   2,
			wantOutput: "not a valid duration",
		},
		{
			name:       "valid gated yes selects apply",
			args:       []string{"--yes", experimentalSweepStaleMutationsFlagForTest, "--threshold", "1h"},
			wantCode:   0,
			wantOutput: "sweep-stale applied",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			store, err := OpenReadOnly(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := RunWorkSweepStaleCLI(test.args, store, t.TempDir(), time.Time{}, &stdout, &stderr)
			combined := stdout.String() + stderr.String()
			if code != test.wantCode || !strings.Contains(combined, test.wantOutput) {
				t.Fatalf("RunWorkSweepStaleCLI(%q) code=%d output=%q, want code=%d containing %q", test.args, code, combined, test.wantCode, test.wantOutput)
			}
		})
	}
}
