package harness

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"compound/internal/runattempt"
)

func TestRunnerRevalidatesInsideCommitLaunchBeforeCommandStart(t *testing.T) {
	homePath := t.TempDir()
	store := runattempt.Open(homePath)
	authorityErr := errors.New("synthetic authority expired at actual launch")
	called := false
	out, err := runWithSubprocess(context.Background(), store, "os-harness-entrypoints", RunInput{
		WorkID: "work_launch_authority", Harness: "os-harness-entrypoints", Workspace: homePath,
		Command: "compound-test-command.invalid", Now: time.Now(),
		StartRevalidate: func() error { called = true; return authorityErr },
	}, func(ctx context.Context, _ RunInput) *exec.Cmd {
		return exec.CommandContext(ctx, "compound-test-command.invalid")
	}, defaultHeartbeatInterval)
	if !called || !errors.Is(err, authorityErr) {
		t.Fatalf("actual launch callback did not revalidate: called=%t err=%v", called, err)
	}
	if out.RunAttempt.Status != runattempt.StatusFailed {
		t.Fatalf("refused launch is not terminal: %+v", out.RunAttempt)
	}
}
