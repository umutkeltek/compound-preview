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

type failingLaunchProfileStore struct{ *runattempt.Store }

func (s *failingLaunchProfileStore) SetLaunchProfileIntent(string, string, string, string) (runattempt.RunAttempt, error) {
	return runattempt.RunAttempt{}, errors.New("injected launch profile write failure")
}

func TestRunnerLaunchProfileIntentBeforeBuild(t *testing.T) {
	for _, failStamp := range []bool{false, true} {
		t.Run(map[bool]string{false: "stamped", true: "failure_terminalizes"}[failStamp], func(t *testing.T) {
			testHome := t.TempDir()
			store := runattempt.Open(testHome)
			var runnerStore runAttemptStore = store
			if failStamp {
				runnerStore = &failingLaunchProfileStore{Store: store}
			}
			built := false
			build := func(ctx context.Context, in RunInput) *exec.Cmd {
				built = true
				attempts, err := store.List()
				if err != nil || len(attempts) != 1 || attempts[0].LaunchProfileID != "fixture-owner" {
					t.Fatalf("command built before profile intent stamp: attempts=%+v err=%v", attempts, err)
				}
				return exec.CommandContext(ctx, "/usr/bin/true")
			}
			out, err := runWithSubprocess(context.Background(), runnerStore, "os-harness-entrypoints", RunInput{
				WorkID: "work_profile_fixture", Workspace: testHome, Command: "/usr/bin/true",
				LaunchProfileID: "fixture-owner", Now: time.Now().UTC(),
				ExecutionSelectionSnapshot: &runattempt.ExecutionSelectionSnapshot{Source: "override", Harness: "os-harness-entrypoints", LaunchProfileID: "fixture-owner"},
			}, build, time.Hour)
			if failStamp {
				if err == nil || built || out.RunAttempt.Status != runattempt.StatusFailed {
					t.Fatalf("profile stamp failure escaped: built=%v out=%+v err=%v", built, out, err)
				}
			} else if err != nil || !built || out.RunAttempt.Status != runattempt.StatusSucceeded || out.RunAttempt.LaunchProfileID != "fixture-owner" {
				t.Fatalf("profile intent lost: built=%v out=%+v err=%v", built, out, err)
			}
			persisted, readErr := store.Get(out.RunAttempt.ID)
			if readErr != nil || !runattempt.IsTerminal(persisted.Status) {
				t.Fatalf("attempt not terminal: %+v err=%v", persisted, readErr)
			}
		})
	}
}

func TestRunnerRejectsConflictingProfileSelectionBeforeMint(t *testing.T) {
	testHome := t.TempDir()
	store := runattempt.Open(testHome)
	built := false
	selection := &runattempt.ExecutionSelectionSnapshot{Source: "override", Harness: "os-harness-entrypoints", LaunchProfileID: "different-owner"}
	if err := selection.Validate(); err != nil {
		t.Fatal(err)
	}
	_, err := runWithSubprocess(context.Background(), store, "os-harness-entrypoints", RunInput{
		WorkID: "work_profile_fixture", Workspace: testHome, Command: "/usr/bin/true",
		LaunchProfileID: "requested-owner", Now: time.Now().UTC(),
		ExecutionSelectionSnapshot: selection,
	}, func(ctx context.Context, in RunInput) *exec.Cmd {
		built = true
		return exec.CommandContext(ctx, "/usr/bin/true")
	}, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "launch profile disagrees") || built {
		t.Fatalf("conflicting profile selection reached construction: built=%v err=%v", built, err)
	}
	attempts, listErr := store.List()
	if listErr != nil || len(attempts) != 0 {
		t.Fatalf("conflicting profile selection minted attempts: %+v err=%v", attempts, listErr)
	}
}
