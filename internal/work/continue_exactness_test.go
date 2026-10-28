package work

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"compound/internal/envelope"
)

func exactnessExecuteFixture(t *testing.T) *Store {
	t.Helper()
	s := newStore(t)
	seedReadyExecuteWork(t, s, "work_exact_a")
	installExecuteGrant(t, s.home, true)
	restoreAllExecuteHooks(t)
	executeStageRoutePlanFn = func(string, string) (string, bool) { return "claude-minimax", true }
	executeStageCapacityFn = func(string, string, time.Time) (float64, bool, error) { return 30, true, nil }
	executeStagePolicyEvalFn = func(policyActionIntentShim, policyConfigShim, time.Time) policyDecisionShim {
		return policyDecisionShim{Outcome: "allow", ID: "pol_exact_allow"}
	}
	launchStub = func(*Store, string, string, string, string, string, time.Time) (string, error) {
		t.Fatal("unexpected executor launch")
		return "", nil
	}
	return s
}

func TestContinuePreviewRunsPreflightBeforeWouldExecute(t *testing.T) {
	for _, refusal := range []string{"adapter_missing:fixture", "workspace_unresolvable:fixture", "lane_unready:fixture", "unexpected preflight failure"} {
		t.Run(refusal, func(t *testing.T) {
			s := exactnessExecuteFixture(t)
			called := 0
			t.Cleanup(SwapExecuteStagePreflight(func(*Store, string, string, time.Time) error { called++; return errors.New(refusal) }))
			before, err := s.Get("work_exact_a")
			if err != nil {
				t.Fatal(err)
			}
			result, err := s.Continue(fixedNow.Add(time.Minute), ContinueOptions{Actor: "codex", DryRun: true, MaxExecuteSteps: 1})
			if err != nil {
				t.Fatal(err)
			}
			if called == 0 {
				t.Error("preview skipped preflight")
			}
			if result.Execute == nil || result.Execute.WouldExecute != "" || result.Status != "refused" {
				t.Errorf("preview claimed execution readiness: %+v", result)
			}
			after, err := s.Get(before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Error("preflight refusal mutated work")
			}
		})
	}
}

func TestContinueRefusesChangedDispatchSnapshotBeforeMutation(t *testing.T) {
	for _, change := range []string{"queue_target", "same_state_generation"} {
		t.Run(change, func(t *testing.T) {
			s := exactnessExecuteFixture(t)
			var snapshot map[string]envelope.WorkEnvelope
			var beforeEvents []byte
			called := false
			// The policy callback is already a production seam between the outer
			// workbench read and lifecycle dispatch. No sleeps or synthetic scheduler.
			executeStagePolicyEvalFn = func(policyActionIntentShim, policyConfigShim, time.Time) policyDecisionShim {
				if called {
					t.Fatal("unexpected repeated policy probe")
				}
				called = true
				if change == "queue_target" {
					seedReadyExecuteWork(t, s, "work_exact_b")
					if _, err := s.Claim("work_exact_b", "codex", fixedNow.Add(time.Second)); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := s.Advance("work_exact_a", AdvanceInput{To: "ready", Actor: "codex", Note: "concurrent revision"}, fixedNow.Add(time.Second)); err != nil {
						t.Fatal(err)
					}
				}
				snapshot = map[string]envelope.WorkEnvelope{}
				for _, id := range []string{"work_exact_a", "work_exact_b"} {
					if w, err := s.Get(id); err == nil {
						snapshot[id] = w
					}
				}
				var err error
				beforeEvents, err = os.ReadFile(filepath.Join(s.home, "events", "work.jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				return policyDecisionShim{Outcome: "hold", ID: "pol_exact_hold"}
			}
			result, err := s.Continue(fixedNow.Add(time.Minute), ContinueOptions{Actor: "codex", ExpectWorkID: "work_exact_a"})
			if err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("interleaving was not exercised")
			}
			if result.Status != "target_changed" {
				t.Errorf("status=%q, want target_changed", result.Status)
			}
			for id, before := range snapshot {
				after, err := s.Get(id)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Errorf("stale request mutated %s", id)
				}
			}
			afterEvents, err := os.ReadFile(filepath.Join(s.home, "events", "work.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if string(beforeEvents) != string(afterEvents) {
				t.Error("stale dispatch appended lifecycle events")
			}
		})
	}
}

func TestContinueExecuteRefusesChangedGenerationBeforeClaim(t *testing.T) {
	s := exactnessExecuteFixture(t)
	var afterOtherWriter envelope.WorkEnvelope
	t.Cleanup(SwapExecuteStagePreflight(func(*Store, string, string, time.Time) error {
		var err error
		afterOtherWriter, err = s.Advance("work_exact_a", AdvanceInput{To: "ready", Actor: "codex", Note: "revision during preflight"}, fixedNow.Add(time.Second))
		return err
	}))
	result, err := s.Continue(fixedNow.Add(time.Minute), ContinueOptions{Actor: "codex", ExpectWorkID: "work_exact_a"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "target_changed" {
		t.Errorf("status=%s, want target_changed", result.Status)
	}
	after, err := s.Get("work_exact_a")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterOtherWriter, after) {
		t.Error("preflight drift was overwritten by claim")
	}
}

func TestDispatchNestedMutationRejectsStaleGeneration(t *testing.T) {
	for _, operation := range []string{"claim", "advance", "close"} {
		t.Run(operation, func(t *testing.T) {
			s := newStore(t)
			w, err := s.Create(CreateInput{Owner: "codex", Actor: "codex", State: "ready", SuccessPredicate: "nested dispatch keeps original generation"}, fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			if operation != "claim" {
				w, err = s.Claim(w.ID, "codex", fixedNow.Add(time.Second))
				if err != nil {
					t.Fatal(err)
				}
			}
			if operation == "close" {
				w, err = s.Advance(w.ID, AdvanceInput{To: "running", Actor: "codex"}, fixedNow.Add(2*time.Second))
				if err != nil {
					t.Fatal(err)
				}
				w, err = s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "codex", VerificationID: "check:exact-close"}, fixedNow.Add(3*time.Second))
				if err != nil {
					t.Fatal(err)
				}
			}
			lastAction := w.ActionIntentIDs[len(w.ActionIntentIDs)-1]
			before, err := s.Advance(w.ID, AdvanceInput{To: w.State, Actor: "codex", Note: "new same-state generation"}, fixedNow.Add(4*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			beforeEvents, err := os.ReadFile(filepath.Join(s.home, "events", "work.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			opts := DispatchPlanOptions{Actor: "codex", WorkID: w.ID, ExpectedWorkState: w.State, ExpectedPolicyDecisionID: w.PolicyDecisionID, ExpectedActionIntentID: lastAction}
			switch operation {
			case "claim":
				_, err = s.DispatchClaim(fixedNow.Add(time.Minute), opts)
			case "advance":
				_, err = s.DispatchAdvance(fixedNow.Add(time.Minute), opts)
			case "close":
				_, err = s.DispatchClose(fixedNow.Add(time.Minute), DispatchCloseOptions{Actor: opts.Actor, WorkID: opts.WorkID, ExpectedWorkState: opts.ExpectedWorkState, ExpectedPolicyDecisionID: opts.ExpectedPolicyDecisionID, ExpectedActionIntentID: opts.ExpectedActionIntentID})
			}
			if !errors.Is(err, ErrExecutionReservationChanged) {
				t.Fatalf("stale %s: %v", operation, err)
			}
			after, err := s.Get(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Errorf("stale %s mutated work", operation)
			}
			afterEvents, err := os.ReadFile(filepath.Join(s.home, "events", "work.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if string(beforeEvents) != string(afterEvents) {
				t.Error("stale nested dispatch appended lifecycle event")
			}
		})
	}
}

func TestExpectedGenerationRefusesBeforePendingClaimRecovery(t *testing.T) {
	for _, operation := range []string{"advance", "close", "dispatch_advance"} {
		t.Run(operation, func(t *testing.T) {
			s := newStore(t)
			original, err := s.Create(CreateInput{Owner: "codex", Actor: "codex", State: "ready", SuccessPredicate: "stale calls never recover another generation"}, fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			crashClaimAtBoundary(t, s, original.ID, "codex", fixedNow.Add(time.Second), "lease")
			paths := []string{s.workPath(original.ID), s.leasePath(original.ID), s.claimPendingPath(original.ID), filepath.Join(s.home, "events", "work.jsonl")}
			before := map[string]string{}
			for _, path := range paths {
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				before[path] = string(body)
			}
			lastAction := original.ActionIntentIDs[len(original.ActionIntentIDs)-1]
			switch operation {
			case "advance":
				_, err = s.Advance(original.ID, AdvanceInput{Actor: "codex", To: "running", ExpectedWorkState: original.State, ExpectedPolicyDecisionID: original.PolicyDecisionID, ExpectedActionIntentID: lastAction}, fixedNow.Add(time.Minute))
			case "close":
				_, err = s.Close(original.ID, CloseInput{Actor: "codex", Outcome: "failed", MoveType: "audit_only", ExpectedWorkState: original.State, ExpectedPolicyDecisionID: original.PolicyDecisionID, ExpectedActionIntentID: lastAction}, fixedNow.Add(time.Minute))
			case "dispatch_advance":
				_, err = s.DispatchAdvance(fixedNow.Add(time.Minute), DispatchPlanOptions{Actor: "codex", WorkID: original.ID, ExpectedWorkState: original.State, ExpectedPolicyDecisionID: original.PolicyDecisionID, ExpectedActionIntentID: lastAction})
			}
			if !errors.Is(err, ErrExecutionReservationChanged) {
				t.Fatalf("stale %s: %v", operation, err)
			}
			for _, path := range paths {
				body, err := os.ReadFile(path)
				if err != nil || string(body) != before[path] {
					t.Errorf("stale %s changed %s before refusing: %v", operation, filepath.Base(path), err)
				}
			}
		})
	}
}

func TestContinuePolicyAwareDispatchSelection(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		for _, pinned := range []bool{false, true} {
			t.Run(fmt.Sprintf("preview=%t/pinned=%t", dryRun, pinned), func(t *testing.T) {
				s := newStore(t)
				for i := 0; i < 2; i++ {
					_, err := s.Create(CreateInput{WorkID: fmt.Sprintf("work_policy_%d", i), Owner: "codex", Actor: "codex", State: "ready", SuccessPredicate: "blocked work must not strand allowed work"}, fixedNow.Add(time.Duration(i)*time.Second))
					if err != nil {
						t.Fatal(err)
					}
				}
				now := fixedNow.Add(time.Minute)
				bench, err := s.Workbench(now, WorkbenchOptions{Actor: "codex"})
				if err != nil {
					t.Fatal(err)
				}
				blocked, allowed := bench.Open[0].WorkID, bench.Open[1].WorkID
				if err := os.MkdirAll(filepath.Join(s.home, "config"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(s.home, "config", "policy.yaml"), []byte(fmt.Sprintf("forbidden_paths:\n  - %q\n", s.workPath(blocked))), 0o644); err != nil {
					t.Fatal(err)
				}
				plan, err := s.DispatchPlan(now, DispatchPlanOptions{Actor: "codex", ReadOnly: true})
				if err != nil || plan.Selected == nil || plan.Selected.WorkID != allowed {
					t.Fatalf("fixture did not select allowed work: %+v %v", plan, err)
				}
				opts := ContinueOptions{Actor: "codex", DryRun: dryRun}
				if pinned {
					opts.ExpectWorkID = blocked
				}
				result, err := s.Continue(now, opts)
				if err != nil {
					t.Fatal(err)
				}
				if dryRun {
					if result.Preview == nil {
						t.Fatal("missing preview")
					}
					if pinned {
						if result.Preview.Status != "refused" || result.Preview.WorkID != blocked {
							t.Errorf("blocked pinned preview: %+v", result.Preview)
						}
					} else if result.Preview.Status != "eligible" || result.Preview.WorkID != allowed {
						t.Errorf("policy-aware preview: %+v", result.Preview)
					}
				} else if !pinned && result.WorkID != allowed {
					t.Errorf("Continue stranded allowed work: %+v", result)
				}
				for _, id := range []string{blocked, allowed} {
					w, err := s.Get(id)
					if err != nil {
						t.Fatal(err)
					}
					want := "ready"
					if !dryRun && !pinned && id == allowed {
						want = "claimed"
					}
					if w.State != want {
						t.Errorf("%s state=%s want %s", id, w.State, want)
					}
				}
			})
		}
	}
}

func TestContinuePreviewRejectsLifecyclePolicy(t *testing.T) {
	for _, state := range []string{"claimed", "verifying"} {
		t.Run(state, func(t *testing.T) {
			s := newStore(t)
			w, err := s.Create(CreateInput{Owner: "codex", Actor: "codex", State: "ready", SuccessPredicate: "preview obeys lifecycle policy"}, fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			w, err = s.Claim(w.ID, "codex", fixedNow.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if state == "verifying" {
				for _, to := range []string{"running", "verifying"} {
					w, err = s.Advance(w.ID, AdvanceInput{Actor: "codex", To: to, VerificationID: "check:fixture"}, fixedNow.Add(2*time.Second))
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := os.MkdirAll(filepath.Join(s.home, "config"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(s.home, "config", "policy.yaml"), []byte("class_outcome:\n  local_reversible:\n    outcome: hold\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			result, err := s.Continue(fixedNow.Add(time.Minute), ContinueOptions{Actor: "codex", ExpectWorkID: w.ID, DryRun: true})
			if err != nil {
				t.Fatal(err)
			}
			if result.Preview == nil || result.Preview.Status != "refused" || result.Preview.WorkID != w.ID || len(result.Preview.ProposedEffects) != 0 {
				t.Errorf("blocked %s preview: %+v", state, result.Preview)
			}
			after, err := s.Get(w.ID)
			if err != nil || !reflect.DeepEqual(w, after) {
				t.Fatalf("blocked preview mutated work: %v", err)
			}
		})
	}
}

func TestContinueSkipsForeignVerifyingWork(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(fmt.Sprintf("pinned=%t", pinned), func(t *testing.T) {
			s := newStore(t)
			foreign, err := s.Create(CreateInput{WorkID: "work_foreign_verifying", Owner: "foreign", State: "ready", SuccessPredicate: "foreign work keeps its owner"}, fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			foreign, err = s.Claim(foreign.ID, "foreign", fixedNow.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			for i, state := range []string{"running", "verifying"} {
				foreign, err = s.Advance(foreign.ID, AdvanceInput{Actor: "foreign", To: state, VerificationID: "check:foreign"}, fixedNow.Add(time.Duration(i+2)*time.Second))
				if err != nil {
					t.Fatal(err)
				}
			}
			ready, err := s.Create(CreateInput{WorkID: "work_owned_ready", Owner: "codex", Actor: "codex", State: "ready", SuccessPredicate: "authorized work progresses beside foreign work"}, fixedNow.Add(4*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			opts := ContinueOptions{Actor: "codex", DryRun: true}
			if pinned {
				opts.ExpectWorkID = ready.ID
			}
			preview, err := s.Continue(fixedNow.Add(time.Minute), opts)
			if err != nil || preview.Preview == nil || preview.Preview.Status != "eligible" || preview.Preview.WorkID != ready.ID || preview.Preview.Action != "claim" {
				t.Fatalf("preview: %+v %v", preview, err)
			}
			opts.DryRun = false
			result, err := s.Continue(fixedNow.Add(time.Minute), opts)
			if err != nil || result.WorkID != ready.ID || result.Status != "claimed" {
				t.Errorf("apply disagrees with preview: %+v %v", result, err)
			}
			after, err := s.Get(foreign.ID)
			if err != nil || !reflect.DeepEqual(foreign, after) {
				t.Errorf("foreign work changed: %v", err)
			}
		})
	}
}
