package harness

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
)

type failingRetryPredecessorStore struct{ *runattempt.Store }

func (s *failingRetryPredecessorStore) SetPredecessor(string, string) (runattempt.RunAttempt, error) {
	return runattempt.RunAttempt{}, errors.New("injected predecessor write failure")
}

func TestRunnerRetryPredecessorBeforeBuild(t *testing.T) {
	for _, failLink := range []bool{false, true} {
		t.Run(map[bool]string{false: "linked", true: "failure_terminalizes"}[failLink], func(t *testing.T) {
			testHome := t.TempDir()
			store := runattempt.Open(testHome)
			at := time.Now().UTC()
			previous, err := store.Mint("work_retry_fixture", "codex", "os-harness-entrypoints", at.Add(-time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Complete(previous.ID, runattempt.StatusFailed, at); err != nil {
				t.Fatal(err)
			}
			var runnerStore runAttemptStore = store
			if failLink {
				runnerStore = &failingRetryPredecessorStore{Store: store}
			}
			built := false
			build := func(ctx context.Context, in RunInput) *exec.Cmd {
				built = true
				attempts, err := store.List()
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, attempt := range attempts {
					if attempt.ID != previous.ID && attempt.PredecessorID == previous.ID && !runattempt.IsTerminal(attempt.Status) {
						found = true
					}
				}
				if !found {
					t.Fatal("command constructed before immutable retry linkage")
				}
				return exec.CommandContext(ctx, "/usr/bin/true")
			}
			out, err := runWithSubprocess(context.Background(), runnerStore, "os-harness-entrypoints", RunInput{
				WorkID: "work_retry_fixture", Harness: "os-harness-entrypoints", Workspace: testHome,
				Command: "/usr/bin/true", PredecessorID: previous.ID, Now: time.Now().UTC(),
			}, build, defaultHeartbeatInterval)
			if failLink {
				if err == nil || !strings.Contains(err.Error(), "predecessor write failure") || built || out.RunAttempt.Status != runattempt.StatusFailed {
					t.Fatalf("failed link escaped lifecycle: built=%v out=%+v err=%v", built, out, err)
				}
			} else if err != nil || !built || out.RunAttempt.Status != runattempt.StatusSucceeded || out.RunAttempt.PredecessorID != previous.ID {
				t.Fatalf("retry lineage not consumed: built=%v out=%+v err=%v", built, out, err)
			}
			persisted, err := store.Get(out.RunAttempt.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !runattempt.IsTerminal(persisted.Status) {
				t.Fatalf("stranded attempt: %+v", persisted)
			}
		})
	}
}
