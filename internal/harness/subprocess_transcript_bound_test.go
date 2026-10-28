package harness

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"compound/internal/runattempt"
)

func TestTranscriptBudgetBoundaryAndSingleMarker(t *testing.T) {
	for _, extra := range []bool{false, true} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			var body bytes.Buffer
			w := &lockedWriter{w: bufio.NewWriter(&body), maxBytes: 16}
			w.writeLine("stdout", "1234567") // exactly sixteen bytes including prefix/newline
			if extra {
				var wg sync.WaitGroup
				for i := 0; i < 8; i++ {
					wg.Add(1)
					go func() { defer wg.Done(); w.writeLine("stderr", "overflow") }()
				}
				wg.Wait()
			}
			_ = w.Flush()
			if w.truncated != extra || strings.Count(body.String(), "OUTPUT_TRUNCATED") != boolInt(extra) {
				t.Fatalf("bound/marker mismatch: %q", body.String())
			}
			if len(body.String()) > 16+len("\ntranscript: OUTPUT_TRUNCATED\n") {
				t.Fatal("unbounded transcript")
			}
		})
	}
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func TestTranscriptBudgetSanitizesBeforePartialRetention(t *testing.T) {
	var body bytes.Buffer
	w := &lockedWriter{w: bufio.NewWriter(&body), maxBytes: 18, sanitize: func(line string) string { return strings.ReplaceAll(line, "synthetic-secret", "[REDACTED]") }}
	w.writeLine("stdout", "synthetic-secret repeated")
	_ = w.Flush()
	if strings.Contains(body.String(), "synthetic") || !w.truncated {
		t.Fatalf("unsafe truncated text: %q", body.String())
	}
}

func TestTranscriptBoundChildHelper(t *testing.T) {
	mode := os.Getenv("COMPOUND_TRANSCRIPT_BOUND_CHILD")
	if mode == "" {
		return
	}
	fmt.Fprintln(os.Stdout, "partial retained")
	fmt.Fprint(os.Stdout, strings.Repeat("x", (1<<20)+1024))
	fmt.Fprintln(os.Stderr, "stderr drained")
	if mode == "failure" {
		os.Exit(7)
	}
	os.Exit(0)
}

func TestTranscriptBudgetGiantLineDrainsAndFailsCompleteEvidence(t *testing.T) {
	for _, mode := range []string{"success", "failure"} {
		t.Run(mode, func(t *testing.T) {
			store := runattempt.Open(t.TempDir())
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			in := RunInput{WorkID: "work_transcript_bound", Workspace: t.TempDir(), Command: exe, Now: time.Now(), Timeout: 5 * time.Second}
			finalized := false
			out, runErr := runWithSubprocessOptions(context.Background(), store, "os-harness-entrypoints", in, func(ctx context.Context, in RunInput) *exec.Cmd {
				cmd := exec.CommandContext(ctx, in.Command, "-test.run=^TestTranscriptBoundChildHelper$")
				cmd.Env = append(os.Environ(), "COMPOUND_TRANSCRIPT_BOUND_CHILD="+mode)
				return cmd
			}, time.Second, subprocessRunOptions{MaxTranscriptBytes: 64, FinalizeEvidence: func(e subprocessEvidenceContext) ([]string, error) {
				finalized = true
				if !e.CaptureFailed || !e.TranscriptTruncated {
					t.Error("giant scanner line was not marked")
				}
				return nil, errors.New("CAPTURE_INCOMPLETE")
			}})
			if out.TimedOut || out.RunAttempt.Status != runattempt.StatusFailed || finalized != (mode == "success") || (runErr != nil) != (mode == "success") {
				t.Fatalf("incorrect bound lifecycle: status=%s timeout=%v finalized=%v err=%v", out.RunAttempt.Status, out.TimedOut, finalized, runErr)
			}
			body, err := os.ReadFile(out.TranscriptPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), "partial retained") || strings.Count(string(body), "OUTPUT_TRUNCATED") != 1 || len(body) > 256 {
				t.Fatalf("partial bound evidence missing or unbounded: len=%d", len(body))
			}
		})
	}
}
