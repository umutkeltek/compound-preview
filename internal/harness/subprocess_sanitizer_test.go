package harness

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"compound/internal/runattempt"
)

func TestTranscriptSanitizerJoinsChunksAndFinalLines(t *testing.T) {
	const key = "synthetic-selected-credential"
	var body bytes.Buffer
	w := &lockedWriter{w: bufio.NewWriter(&body), sanitize: func(line string) string {
		return strings.ReplaceAll(line, key, "[REDACTED]")
	}}
	var group sync.WaitGroup
	for _, stream := range []string{"stdout", "stderr"} {
		group.Add(1)
		go func(label string) {
			defer group.Done()
			capture(io.MultiReader(strings.NewReader("before synthetic-selected-"),
				strings.NewReader("credential after\n"+key)), label, w)
		}(stream)
	}
	group.Wait()
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body.String(), key) || strings.Count(body.String(), "[REDACTED]") != 4 {
		t.Fatalf("unsafe/incomplete transcript: %q", body.String())
	}
	for _, label := range []string{"stdout", "stderr"} {
		if !strings.Contains(body.String(), label+": before [REDACTED] after\n") ||
			!strings.Contains(body.String(), label+": [REDACTED]\n") {
			t.Fatalf("missing sanitized stream %s: %q", label, body.String())
		}
	}
}

func TestTranscriptSanitizerNilPreservesExistingOutput(t *testing.T) {
	var body bytes.Buffer
	w := &lockedWriter{w: bufio.NewWriter(&body)}
	capture(strings.NewReader("unchanged\nlast"), "stdout", w)
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if body.String() != "stdout: unchanged\nstdout: last\n" {
		t.Fatal(body.String())
	}
}

func TestTranscriptSanitizerFailingChildCannotPersistSelectedLiteral(t *testing.T) {
	const key = "synthetic-selected-credential"
	store := runattempt.Open(t.TempDir())
	in := RunInput{WorkID: "work_sanitizer_failure", Workspace: t.TempDir(), Command: "/bin/sh", Now: time.Now()}
	out, err := runWithSubprocessOptions(context.Background(), store, "os-harness-entrypoints", in,
		func(ctx context.Context, input RunInput) *exec.Cmd {
			return exec.CommandContext(ctx, input.Command, "-c",
				"printf synthetic-selected-; printf credential; printf synthetic-selected-credential >&2; exit 7")
		}, time.Second, subprocessRunOptions{SanitizeTranscriptLine: func(line string) string {
			return strings.ReplaceAll(line, key, "[REDACTED]")
		}})
	if err != nil {
		t.Fatal(err)
	}
	if out.RunAttempt.Status != runattempt.StatusFailed || out.ExitCode != 7 {
		t.Fatalf("out=%+v", out)
	}
	body, err := os.ReadFile(out.TranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), key) || strings.Count(string(body), "[REDACTED]") != 2 {
		t.Fatalf("unsafe/incomplete failure transcript: %q", body)
	}
}
