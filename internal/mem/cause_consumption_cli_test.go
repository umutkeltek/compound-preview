package mem

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
)

func TestRunCauseConsumptionCLIClockReadsMatchRecordBoundary(t *testing.T) {
	start := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		args      []string
		wantReads int
	}{
		{name: "read only", args: []string{"--json"}, wantReads: 1},
		{name: "record", args: []string{"--record", "--json"}, wantReads: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			homePath := t.TempDir()
			if err := home.Init(homePath, "test", start); err != nil {
				t.Fatal(err)
			}
			clockReads := 0
			clock := func() time.Time {
				value := start.Add(time.Duration(clockReads) * time.Second)
				clockReads++
				return value
			}
			var stdout, stderr bytes.Buffer
			if err := RunCauseConsumptionCLI(homePath, tc.args, &stdout, &stderr, clock); err != nil {
				t.Fatal(err)
			}
			if clockReads != tc.wantReads {
				t.Fatalf("clock reads = %d, want %d", clockReads, tc.wantReads)
			}
			if !strings.Contains(stdout.String(), `"generated_at": "2026-07-17T12:00:00Z"`) {
				t.Fatalf("report did not use first clock read:\n%s", stdout.String())
			}
			runDir := filepath.Join(homePath, "state", "runs", "cause-consumption")
			if tc.wantReads == 1 {
				if stderr.Len() != 0 {
					t.Fatalf("unexpected stderr: %q", stderr.String())
				}
				if _, err := os.Stat(runDir); !os.IsNotExist(err) {
					t.Fatalf("read-only run created record directory: %v", err)
				}
				return
			}
			expectedPath := filepath.Join(runDir, "20260717T120001Z.json")
			if stderr.String() != "recorded: "+expectedPath+"\n" {
				t.Fatalf("stderr = %q, want exact recorded path", stderr.String())
			}
			if _, err := os.Stat(expectedPath); err != nil {
				t.Fatalf("record artifact missing: %v", err)
			}
		})
	}
}

func TestRunCauseConsumptionCLIRendersHumanSummary(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := RunCauseConsumptionCLI(homePath, nil, &stdout, &stderr, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		"# memory cause consumption",
		"- coverage:",
		"promoted",
		"pre_adjudication",
		"closed",
		"observed_raw",
		"non_memory",
		"motion_from_unpromoted_ratio",
		"stranger dealt->caused",
		"transitions: (none observed)",
	} {
		if !strings.Contains(stdout.String(), fragment) {
			t.Fatalf("human output missing %q:\n%s", fragment, stdout.String())
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
}

type causeConsumptionErrorWriter struct{}

func (causeConsumptionErrorWriter) Write([]byte) (int, error) {
	return 0, errors.New("synthetic output failure")
}

func TestRunCauseConsumptionCLIPropagatesOutputFailure(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	err := RunCauseConsumptionCLI(homePath, []string{"--json"}, causeConsumptionErrorWriter{}, &bytes.Buffer{}, func() time.Time {
		return now
	})
	if err == nil || !strings.Contains(err.Error(), "synthetic output failure") {
		t.Fatalf("output error = %v, want propagated writer failure", err)
	}
}
