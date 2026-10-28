package actuator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"compound/internal/harness"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func TestGovernedStartRejectsGrantExpiringUnderCommitLaunchLock(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprintf("resume=%t", resume), func(t *testing.T) {
			homePath := t.TempDir()
			s, err := work.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			const actor = "codex:launch-clock"
			w, err := s.Create(work.CreateInput{Owner: actor, Actor: actor, SuccessPredicate: "synthetic launch boundary"}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			w, err = s.Claim(w.ID, actor, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if resume {
				w, err = s.Advance(w.ID, work.AdvanceInput{Actor: actor, To: "running"}, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				w, err = s.Advance(w.ID, work.AdvanceInput{Actor: actor, To: "verifying"}, time.Now())
				if err != nil {
					t.Fatal(err)
				}
			}
			admittedAt := time.Now()
			expiry := admittedAt.Add(5 * time.Second)
			body := fmt.Sprintf("class_outcome:\n  local_reversible:\n    outcome: hold\nstanding_grants:\n  - id: grant_synthetic_launch_boundary\n    capability: work.advance\n    mutation_class: local_reversible\n    reversibility: easy\n    blast_radius: local\n    expires_at: %q\n", expiry.UTC().Format(time.RFC3339Nano))
			if err = os.MkdirAll(filepath.Join(homePath, "config"), 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(homePath, "config", "policy.yaml"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			claimed := w
			w, err = s.Advance(w.ID, work.AdvanceInput{Actor: actor, To: "running"}, admittedAt)
			if err != nil {
				t.Fatal(err)
			}
			actionID := w.ActionIntentIDs[len(w.ActionIntentIDs)-1]
			attempts := runattempt.Open(homePath)
			input, err := PrepareGovernedHarnessLaunch(GovernedHarnessLaunchInput{
				HomePath: homePath, Adapter: harness.NewOSEntrypointsAdapter(attempts),
				ClaimedWork: claimed, RunningWork: w, ActionIntentID: actionID, ResumeVerifying: resume, Now: time.Now(),
				RunInput: harness.RunInput{WorkID: w.ID, Harness: "os-harness-entrypoints", Workspace: homePath},
			})
			if err != nil {
				t.Fatal(err)
			}
			ra, err := attempts.MintWithActionIntent(w.ID, actor, "os-harness-entrypoints", actionID, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(filepath.Join(homePath, "lock", "runattempts", ra.ID+".lock"), os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
				t.Fatal(err)
			}
			locked := true
			defer func() {
				if locked {
					_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				}
			}()
			entered := make(chan struct{})
			done := make(chan error, 1)
			started := false
			go func() {
				done <- input.StartGuard(context.Background(), func() error {
					close(entered)
					_, err := attempts.CommitLaunch(ra.ID, time.Now(), func() error {
						if input.StartRevalidate != nil {
							if err := input.StartRevalidate(); err != nil {
								return err
							}
						}
						started = true
						return nil
					})
					return err
				})
			}()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("guard refused before lock boundary: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("guard never reached CommitLaunch")
			}
			select {
			case err := <-done:
				t.Fatalf("CommitLaunch bypassed held per-attempt lock: %v", err)
			case <-time.After(25 * time.Millisecond):
			}
			if delay := time.Until(expiry.Add(25 * time.Millisecond)); delay > 0 {
				time.Sleep(delay)
			}
			if err = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
				t.Fatal(err)
			}
			locked = false
			select {
			case err := <-done:
				if err == nil || started {
					t.Fatalf("expired grant crossed inner launch lock: started=%t err=%v", started, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("start revalidation deadlocked after inner lock released")
			}
		})
	}
}
