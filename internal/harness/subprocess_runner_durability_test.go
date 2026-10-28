package harness

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
)

func TestRunnerCertifyingCaptureFailureRefusesFinalization(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(subprocessRunOptions, error) subprocessRunOptions
	}{
		{
			name: "flush",
			apply: func(options subprocessRunOptions, injected error) subprocessRunOptions {
				options.flushTranscript = func(writer *lockedWriter) error {
					if err := writer.Flush(); err != nil {
						return err
					}
					return injected
				}
				return options
			},
		},
		{
			name: "sync",
			apply: func(options subprocessRunOptions, injected error) subprocessRunOptions {
				options.syncTranscript = func(file *os.File) error {
					if err := file.Sync(); err != nil {
						return err
					}
					return injected
				}
				return options
			},
		},
		{
			name: "close",
			apply: func(options subprocessRunOptions, injected error) subprocessRunOptions {
				options.closeTranscript = func(file *os.File) error {
					if err := file.Close(); err != nil {
						return err
					}
					return injected
				}
				return options
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			store := runattempt.Open(home)
			injected := errors.New("injected " + tc.name + " failure")
			finalized := false
			options := tc.apply(subprocessRunOptions{FinalizeEvidence: func(subprocessEvidenceContext) ([]string, error) {
				finalized = true
				return []string{"artifact:must-not-attach"}, nil
			}}, injected)

			out, err := runWithSubprocessOptions(context.Background(), store, "os-harness-entrypoints", durabilityRunInput(home), durabilityBuild, defaultHeartbeatInterval, options)
			if !errors.Is(err, injected) || !strings.Contains(err.Error(), "durable transcript capture") {
				t.Fatalf("durability error = %v, want wrapped %v", err, injected)
			}
			if finalized || out.RunAttempt.Status != runattempt.StatusFailed || out.ExitCode != 0 || out.Duration <= 0 {
				t.Fatalf("finalized=%t output=%+v", finalized, out)
			}
			if body, readErr := os.ReadFile(out.TranscriptPath); readErr != nil || !strings.Contains(string(body), "stdout: durable-capture") {
				t.Fatalf("partial transcript read=%v body=%q", readErr, body)
			}
			persisted, getErr := store.Get(out.RunAttempt.ID)
			if getErr != nil || persisted.Status != runattempt.StatusFailed {
				t.Fatalf("persisted=%+v get=%v", persisted, getErr)
			}
		})
	}
}

func TestRunnerGenericCaptureFailureRetainsZeroOptionBehavior(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	injected := errors.New("injected generic flush failure")
	out, err := runWithSubprocessOptions(context.Background(), store, "os-harness-entrypoints", durabilityRunInput(home), durabilityBuild, defaultHeartbeatInterval, subprocessRunOptions{
		flushTranscript: func(writer *lockedWriter) error {
			if err := writer.Flush(); err != nil {
				return err
			}
			return injected
		},
	})
	if err != nil || out.RunAttempt.Status != runattempt.StatusSucceeded || out.ExitCode != 0 {
		t.Fatalf("zero-option capture failure changed generic result: out=%+v err=%v", out, err)
	}
	persisted, err := store.Get(out.RunAttempt.ID)
	if err != nil || !strings.Contains(strings.Join(persisted.ProofRefs, "\n"), "transcript_flush_error:") {
		t.Fatalf("generic capture audit = %+v, %v", persisted, err)
	}
}

func TestRunnerFinalizesOnlyAfterDurableCaptureBeforeTerminalization(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	finalized := false
	out, err := runWithSubprocessOptions(context.Background(), store, "os-harness-entrypoints", durabilityRunInput(home), durabilityBuild, defaultHeartbeatInterval, subprocessRunOptions{
		FinalizeEvidence: func(evidence subprocessEvidenceContext) ([]string, error) {
			body, err := os.ReadFile(evidence.TranscriptPath)
			if err != nil || !strings.Contains(string(body), "stdout: durable-capture") {
				t.Fatalf("finalizer durable transcript body=%q err=%v", body, err)
			}
			attempt, err := store.Get(evidence.RunAttemptID)
			if err != nil || runattempt.IsTerminal(attempt.Status) {
				t.Fatalf("finalizer attempt=%+v err=%v", attempt, err)
			}
			finalized = true
			return []string{"artifact:durable-ordering"}, nil
		},
	})
	if err != nil || !finalized || out.RunAttempt.Status != runattempt.StatusSucceeded {
		t.Fatalf("durable ordering out=%+v err=%v finalized=%t", out, err, finalized)
	}
}

func durabilityRunInput(home string) RunInput {
	return RunInput{
		WorkID:    "work_durable_capture",
		Harness:   "os-harness-entrypoints",
		Workspace: home,
		Command:   "sh",
		Now:       time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
	}
}

func durabilityBuild(ctx context.Context, _ RunInput) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", "printf durable-capture")
}
