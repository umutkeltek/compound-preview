package harness

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"compound/internal/runattempt"
)

func TestRunnerCancellationRetriesPipeHoldingGroup(t *testing.T) {
	for _, mode := range []string{"context", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			ready := filepath.Join(home, "ready")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var signals atomic.Int32
			in := RunInput{
				WorkID: "work_cancel_retry", Harness: "cancel-retry", Workspace: home,
				Command: "sh", Args: []string{"-c", "sleep 30 & printf ready > \"$1\"; wait", "fixture", ready},
				Now: time.Now().UTC(),
			}
			if mode == "timeout" {
				in.Timeout = 300 * time.Millisecond
			}
			type result struct {
				out RunOutput
				err error
			}
			done := make(chan result, 1)
			go func() {
				out, err := runWithSubprocessOptions(ctx, runattempt.Open(home), in.Harness, in,
					func(_ context.Context, in RunInput) *exec.Cmd { return exec.Command(in.Command, in.Args...) },
					defaultHeartbeatInterval, subprocessRunOptions{killProcessTree: func(cmd *exec.Cmd) {
						// Model the observed OS boundary: the leader dies but a descendant
						// missed by the first group signal retains both capture pipes.
						if signals.Add(1) == 1 {
							_ = cmd.Process.Kill()
							return
						}
						killProcessTree(cmd)
					}})
				done <- result{out, err}
			}()
			deadline := time.After(3 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				select {
				case <-deadline:
					t.Fatal("child did not become ready")
				case <-time.After(10 * time.Millisecond):
				}
			}
			if mode == "context" {
				cancel()
			}
			select {
			case got := <-done:
				if got.err != nil || got.out.RunAttempt.Status != runattempt.StatusFailed || !got.out.TimedOut {
					t.Fatalf("cancellation result: status=%s timed_out=%v err=%v", got.out.RunAttempt.Status, got.out.TimedOut, got.err)
				}
				if signals.Load() < 2 {
					t.Fatal("pipe-holding descendant was not signalled again")
				}
				before := signals.Load()
				time.Sleep(250 * time.Millisecond)
				if after := signals.Load(); after != before {
					t.Fatalf("group signalled after reap: before=%d after=%d", before, after)
				}
			case <-time.After(5 * time.Second):
				// Let the finite fixture exit naturally so even the red control
				// does not leave a runner, heartbeat, or orphan behind the test.
				select {
				case <-done:
				case <-time.After(30 * time.Second):
					t.Error("finite fixture failed to exit during cleanup")
				}
				t.Fatal("cancellation left a pipe-holding descendant alive")
			}
		})
	}
}
