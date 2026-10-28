package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"compound/internal/actuator"
	"compound/internal/envelope"
	"compound/internal/harness"
	"compound/internal/policy"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func resumeReservationInput(t *testing.T, f claimedExecutionFixture, w envelope.WorkEnvelope) actuator.GovernedHarnessReservationInput {
	t.Helper()
	adapter := harness.NewOSEntrypointsAdapter(runattempt.Open(f.homePath))
	return actuator.GovernedHarnessReservationInput{
		HomePath: f.homePath, Store: f.store, ClaimedWork: w, Adapter: adapter,
		RunInput: harness.RunInput{WorkID: w.ID, Harness: adapter.Name(), Workspace: t.TempDir(), Command: "/usr/bin/true", Now: time.Now().UTC()},
		Actor:    "codex", Target: "running", ResumeVerifying: true, Now: time.Now().UTC(),
	}
}

func TestWorkAdvanceResumeVerifyingExactReservation(t *testing.T) {
	for _, rotate := range []bool{false, true} {
		t.Run(map[bool]string{false: "concurrent", true: "rotated"}[rotate], func(t *testing.T) {
			f := newResumeFixture(t, "resume CAS has one winner")
			w := resumeRunFirst(t, f)
			in := resumeReservationInput(t, f, w)
			if rotate {
				if _, err := f.store.Advance(w.ID, work.AdvanceInput{To: "verifying", Actor: "codex", Note: "new source generation"}, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				before := resumeEnvelopeBytes(t, f)
				_, err := actuator.ReserveGovernedHarnessLaunch(in)
				if !errors.Is(err, work.ErrExecutionReservationChanged) {
					t.Fatalf("stale reservation error = %v", err)
				}
				if !bytes.Equal(before, resumeEnvelopeBytes(t, f)) {
					t.Fatal("stale reservation mutated current generation")
				}
				return
			}
			results := make(chan error, 2)
			var wg sync.WaitGroup
			for range 2 {
				wg.Add(1)
				go func() { defer wg.Done(); _, err := actuator.ReserveGovernedHarnessLaunch(in); results <- err }()
			}
			wg.Wait()
			close(results)
			winners, stale := 0, 0
			for err := range results {
				if err == nil {
					winners++
				} else if errors.Is(err, work.ErrExecutionReservationChanged) {
					stale++
				} else {
					t.Fatal(err)
				}
			}
			if winners != 1 || stale != 1 {
				t.Fatalf("winners=%d stale=%d", winners, stale)
			}
			attempts, err := runattempt.Open(f.homePath).List()
			if err != nil {
				t.Fatal(err)
			}
			if len(attempts) != len(w.RunAttemptIDs) {
				t.Fatal("reservation minted or lost attempts")
			}
		})
	}
}

func TestWorkAdvanceResumeVerifyingClaudePlanHaltBeforeStart(t *testing.T) {
	f := newGovernedClaudeExecutionFixture(t, "plan resume still observes autonomy halt")
	f.claimedExecutionFixture = newResumeFixture(t, "plan resume halt race")
	w := resumeRunFirst(t, f.claimedExecutionFixture)
	adapter, ok := harness.NewAdapterRegistry(runattempt.Open(f.homePath)).AdapterByName(f.harness)
	if !ok {
		t.Fatal("missing adapter")
	}
	input := harness.RunInput{WorkID: w.ID, Harness: f.harness, Workspace: f.workspace, Prompt: "read-only resume",
		PermissionMode: "plan", Now: time.Now().UTC()}
	if preflight, ok := adapter.(harness.PreflightAdapter); ok {
		var err error
		input, err = preflight.Preflight(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
	}
	reserved, err := actuator.ReserveGovernedHarnessLaunch(actuator.GovernedHarnessReservationInput{
		HomePath: f.homePath, Store: f.store, ClaimedWork: w, Adapter: adapter, RunInput: input,
		Actor: "codex", Target: "running", ResumeVerifying: true, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.WriteAutonomyHalt(f.homePath, policy.AutonomyHalt{DisabledClasses: map[string]string{
		"build-implement": "halt after plan reservation",
	}}); err != nil {
		t.Fatal(err)
	}
	out, runErr := adapter.Run(context.Background(), reserved.RunInput)
	if out.RunAttempt.Status != runattempt.StatusFailed {
		t.Fatalf("halted plan resume launched: status=%s err=%v", out.RunAttempt.Status, runErr)
	}
	if _, err := os.Stat(f.argvLog); !os.IsNotExist(err) {
		t.Fatalf("halted plan child created marker: %v", err)
	}
	if _, err := actuator.FinalizeGovernedHarnessRun(actuator.GovernedHarnessFinalizationInput{
		Store: f.store, WorkID: w.ID, Actor: "codex", HarnessName: adapter.Name(), ResumeVerifying: true,
		RunningPolicyID: reserved.RunningWork.PolicyDecisionID, RunningActionIntentID: reserved.ActionIntentID,
		ExistingRunAttemptIDs: w.RunAttemptIDs, Output: out, RunError: runErr, Now: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	current, err := f.store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != "verifying" {
		t.Fatalf("halted resume state=%s", current.State)
	}
}

func TestWorkAdvanceResumeVerifyingStartAndFinalizationRevocation(t *testing.T) {
	for _, revoke := range []string{"halt", "new_generation"} {
		t.Run(revoke, func(t *testing.T) {
			f := newResumeFixture(t, "resume start and finalization preserve authority")
			w := resumeRunFirst(t, f)
			in := resumeReservationInput(t, f, w)
			marker := filepath.Join(t.TempDir(), "no-child")
			in.RunInput.Command, in.RunInput.Args = "/usr/bin/touch", []string{marker}
			reserved, err := actuator.ReserveGovernedHarnessLaunch(in)
			if err != nil {
				t.Fatal(err)
			}
			if revoke == "halt" {
				if err := policy.WriteAutonomyHalt(f.homePath, policy.AutonomyHalt{DisabledClasses: map[string]string{"build-implement": "revoked after reserve"}}); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := f.store.Advance(w.ID, work.AdvanceInput{To: "held", Actor: "codex", Note: "newer lifecycle owns work"}, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
			before := resumeEnvelopeBytes(t, f)
			out, runErr := in.Adapter.Run(context.Background(), reserved.RunInput)
			if out.RunAttempt.ID == "" || out.RunAttempt.Status != runattempt.StatusFailed {
				t.Fatalf("revocation not terminalized: %+v err=%v", out, runErr)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("revoked child launched: %v", err)
			}
			if !bytes.Equal(before, resumeEnvelopeBytes(t, f)) {
				t.Fatal("start guard changed work")
			}
			result, err := actuator.FinalizeGovernedHarnessRun(actuator.GovernedHarnessFinalizationInput{
				ResumeVerifying: true,
				Store:           f.store, WorkID: w.ID, Actor: "codex", HarnessName: in.Adapter.Name(),
				RunningPolicyID: reserved.RunningWork.PolicyDecisionID, RunningActionIntentID: reserved.ActionIntentID,
				ExistingRunAttemptIDs: w.RunAttemptIDs, Output: out, RunError: runErr, Now: time.Now().UTC(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if revoke == "new_generation" {
				if !bytes.Equal(before, resumeEnvelopeBytes(t, f)) {
					t.Fatal("stale finalizer mutated newer generation")
				}
				if len(result.Warnings) == 0 {
					t.Fatal("reverse-link-only disposition was hidden")
				}
			} else if result.Succeeded || result.FinalState != "verifying" {
				t.Fatalf("halted resume not retained as failed attempt on verifying work: %+v", result)
			}
		})
	}
}

func TestWorkAdvanceResumeVerifyingClaudeRequiresFreshReadiness(t *testing.T) {
	for _, mode := range []string{"", "bypass", "default", "acceptEdits"} {
		t.Run("mode_"+mode, func(t *testing.T) {
			f := newGovernedClaudeExecutionFixture(t, "resume cannot inherit bypass exception")
			f.claimedExecutionFixture = newResumeFixture(t, "resume uses current time and authority")
			w := resumeRunFirst(t, f.claimedExecutionFixture)
			stubDelegateHarnessReadiness(t, func(work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
				return work.DelegateHarnessReadinessResult{Ready: false, Details: []string{"current authority revoked"}, Evidence: governedClaudeReadyEvidenceFixture()}, nil
			})
			registry := harness.NewAdapterRegistry(runattempt.Open(f.homePath))
			adapter, ok := registry.AdapterByName(f.harness)
			if !ok {
				t.Fatal("Claude adapter missing")
			}
			input := harness.RunInput{WorkID: w.ID, Harness: adapter.Name(), Workspace: t.TempDir(), Prompt: "resume must refuse", PermissionMode: mode, Now: time.Now().UTC()}
			if preflight, ok := adapter.(harness.PreflightAdapter); ok {
				var err error
				input, err = preflight.Preflight(context.Background(), input)
				if err != nil {
					t.Fatal(err)
				}
			}
			before := resumeEnvelopeBytes(t, f.claimedExecutionFixture)
			_, err := actuator.ReserveGovernedHarnessLaunch(actuator.GovernedHarnessReservationInput{
				HomePath: f.homePath, Store: f.store, ClaimedWork: w, Adapter: adapter, RunInput: input,
				Actor: "codex", Target: "running", ResumeVerifying: true, Now: time.Now().UTC(),
			})
			var refusal *actuator.LaneRefusal
			if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "readiness") {
				t.Fatalf("unready resume not refused: %v", err)
			}
			if !bytes.Equal(before, resumeEnvelopeBytes(t, f.claimedExecutionFixture)) {
				t.Fatal("unready resume advanced work")
			}
			attempts, err := runattempt.Open(f.homePath).List()
			if err != nil {
				t.Fatal(err)
			}
			if len(attempts) != len(w.RunAttemptIDs) {
				t.Fatal("unready resume minted attempt")
			}
		})
	}
}

func TestWorkAdvanceResumeVerifyingClaudeRevalidatesExplicitBypass(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "revoked_before_start"}[revoke], func(t *testing.T) {
			f := newGovernedClaudeExecutionFixture(t, "resume revalidates explicit bypass")
			f.claimedExecutionFixture = newResumeFixture(t, "resume attests fresh authority")
			w := resumeRunFirst(t, f.claimedExecutionFixture)
			calls := 0
			stubDelegateHarnessReadiness(t, func(work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
				calls++
				return work.DelegateHarnessReadinessResult{Ready: !revoke || calls == 1, Evidence: governedClaudeReadyEvidenceFixture()}, nil
			})
			adapter, ok := harness.NewAdapterRegistry(runattempt.Open(f.homePath)).AdapterByName(f.harness)
			if !ok {
				t.Fatal("missing adapter")
			}
			input := harness.RunInput{WorkID: w.ID, Harness: f.harness, Workspace: f.workspace, Prompt: "fresh resume", PermissionMode: "bypass", Now: time.Now().UTC()}
			if preflight, ok := adapter.(harness.PreflightAdapter); ok {
				var err error
				input, err = preflight.Preflight(context.Background(), input)
				if err != nil {
					t.Fatal(err)
				}
			}
			reserved, err := actuator.ReserveGovernedHarnessLaunch(actuator.GovernedHarnessReservationInput{
				HomePath: f.homePath, Store: f.store, ClaimedWork: w, Adapter: adapter, RunInput: input,
				Actor: "codex", Target: "running", ResumeVerifying: true, Now: time.Now().UTC(),
			})
			if err != nil {
				t.Fatal(err)
			}
			out, runErr := adapter.Run(context.Background(), reserved.RunInput)
			if calls != 2 {
				t.Fatalf("readiness calls=%d, want fresh admission + start", calls)
			}
			want := runattempt.StatusSucceeded
			if revoke {
				want = runattempt.StatusFailed
			}
			if out.RunAttempt.Status != want {
				t.Fatalf("status=%s err=%v", out.RunAttempt.Status, runErr)
			}
			if revoke {
				if _, err := os.Stat(f.argvLog); !os.IsNotExist(err) {
					t.Fatalf("revoked Claude launched: %v", err)
				}
			} else {
				if _, err := os.Stat(f.argvLog); err != nil {
					t.Fatal(err)
				}
				if out.RunAttempt.ExecutionPosture == nil || out.RunAttempt.ExecutionPosture.PolicyDecisionID != reserved.RunningWork.PolicyDecisionID || len(out.RunAttempt.ExecutionPosture.AuthorityEvidence) != 4 {
					t.Fatalf("fresh authority not bound: %+v", out.RunAttempt.ExecutionPosture)
				}
			}
			if _, err := actuator.FinalizeGovernedHarnessRun(actuator.GovernedHarnessFinalizationInput{
				ResumeVerifying: true,
				Store:           f.store, WorkID: w.ID, Actor: "codex", HarnessName: adapter.Name(),
				RunningPolicyID: reserved.RunningWork.PolicyDecisionID, RunningActionIntentID: reserved.ActionIntentID,
				ExistingRunAttemptIDs: w.RunAttemptIDs, Output: out, RunError: runErr, Now: time.Now().UTC(),
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
