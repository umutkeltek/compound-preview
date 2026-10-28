package actuator

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/harness"
	"compound/internal/home"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func TestRetainedFailureRequiresNewBoundTerminalAttempt(t *testing.T) {
	for _, variant := range []string{"missing", "wrong_work", "wrong_action", "wrong_origin", "pending", "running", "prior", "reservation_changed", "valid"} {
		t.Run(variant, func(t *testing.T) {
			now := time.Now().UTC()
			h := t.TempDir()
			if err := home.Init(h, "test", now); err != nil {
				t.Fatal(err)
			}
			s, err := work.Open(h)
			if err != nil {
				t.Fatal(err)
			}
			claimed := claimOMPPromptOnlyWork(t, s, "executor", now)
			running, err := s.Advance(claimed.ID, work.AdvanceInput{To: "running", Actor: "executor"}, now)
			if err != nil {
				t.Fatal(err)
			}
			action := running.ActionIntentIDs[len(running.ActionIntentIDs)-1]
			in := GovernedHarnessFinalizationInput{
				Store: s, WorkID: running.ID, Actor: "executor", HarnessName: "fixture",
				RunningPolicyID: running.PolicyDecisionID, RunningActionIntentID: action,
				ExistingRunAttemptIDs: running.RunAttemptIDs, RetainFailedAttempt: true,
				RunError: errors.New("synthetic interruption"), Now: now,
			}
			if variant != "missing" {
				workID, actionID := running.ID, action
				if variant == "wrong_work" {
					workID = "work_other"
				}
				if variant == "wrong_action" {
					actionID = "act_other"
				}
				attempts := runattempt.Open(h)
				a, err := attempts.MintWithActionIntent(workID, "executor", "fixture", actionID, now)
				if err != nil {
					t.Fatal(err)
				}
				if variant != "wrong_origin" {
					a, err = attempts.SetOrigin(a.ID, runattempt.OriginHarnessExecution)
					if err != nil {
						t.Fatal(err)
					}
				}
				if variant == "running" {
					a, err = attempts.MarkRunning(a.ID, now)
					if err != nil {
						t.Fatal(err)
					}
				} else if variant != "pending" {
					a, err = attempts.Complete(a.ID, runattempt.StatusFailed, now)
					if err != nil {
						t.Fatal(err)
					}
				}
				in.Output = harness.RunOutput{RunAttempt: a, ExitCode: 1}
				if variant == "prior" {
					in.ExistingRunAttemptIDs = append(in.ExistingRunAttemptIDs, a.ID)
				}
			}
			if variant == "reservation_changed" {
				if _, err := s.Advance(running.ID, work.AdvanceInput{To: "running", Actor: "executor", Note: "new generation"}, now.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.Get(running.ID)
			if err != nil {
				t.Fatal(err)
			}
			result, err := FinalizeGovernedHarnessRun(in)
			after, getErr := s.Get(running.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if variant == "valid" {
				if err != nil || result.Succeeded || result.FinalState != "verifying" || after.State != "verifying" {
					t.Fatalf("valid retained failure: %+v %v", result, err)
				}
				return
			}
			if variant == "reservation_changed" {
				if err != nil || !strings.Contains(strings.Join(result.Warnings, " "), "preserving the newer") {
					t.Fatalf("reservation loss: %+v %v", result, err)
				}
			} else if err == nil {
				t.Fatalf("invalid evidence accepted: %+v", result)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("refusal changed authoritative work")
			}
		})
	}
}
