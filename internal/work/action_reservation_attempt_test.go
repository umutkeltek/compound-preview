package work

import (
	"compound/internal/policy"
	"compound/internal/runattempt"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRequiredAttemptCompletionRetainsBlockerAcrossFinalizerFailure(t *testing.T) {
	s := newStore(t)
	w := governedActionRunningWork(t, s, "operator:test")
	action, decision := governedActionApproval(w, "operator:test", fixedNow.Add(time.Minute))
	attempts := runattempt.Open(s.home)
	attempt, e := attempts.MintWithID(runattempt.MintDeterministic("required-attempt-fixture", fixedNow), w.ID, "operator:test", "compound", fixedNow)
	if e != nil {
		t.Fatal(e)
	}
	if e = attempts.AttachActionIntent(attempt.ID, action.ID); e != nil {
		t.Fatal(e)
	}
	in := ReceiptedActionInput{Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running", Outcome: "completed", Changed: true, Summary: "observed fixture filesystem change", MoveType: "control_plane_repair", RequiredAttemptCompletion: &RequiredAttemptCompletion{AttemptID: attempt.ID, Owner: "fixture.closed-owner"}}
	at := fixedNow.Add(2 * time.Minute)
	if e = s.ReserveReceiptedAction(w.ID, in, at, func() error { return nil }); e != nil {
		t.Fatal(e)
	}
	result, e := s.RecordReservedReceiptedAction(w.ID, in, at)
	if !errors.Is(e, ErrRequiredAttemptCompletion) || result.Receipt.ID == "" {
		t.Fatalf("wanted genuine durable outcome with retained proof blocker: %v", e)
	}
	for i := 0; i < 2; i++ {
		if _, e = s.RecordReservedReceiptedAction(w.ID, in, at); !errors.Is(e, ErrRequiredAttemptCompletion) {
			t.Fatalf("legacy retry skipped required finalizer: %v", e)
		}
		if _, e = s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "operator:test"}, at); e == nil {
			t.Fatal("work advanced before required attempt proof")
		}
	}
	omitted := in
	omitted.RequiredAttemptCompletion = nil
	if _, e = s.RecordReservedReceiptedAction(w.ID, omitted, at); e == nil {
		t.Fatal("retry stripped durable requirement")
	}
	if e = s.AbortReceiptedActionReservation(w.ID, action.ID); e == nil {
		t.Fatal("abort erased published reservation")
	}
	// Native attempt failure before persistence leaves its state pending. The
	// failed finalizer is represented above by missing durable owner evidence;
	// no receipt or native completion is manufactured to let work continue.
	if _, e = attempts.CompleteWithOwnerProof(attempt.ID, runattempt.StatusSucceeded, "fixture.closed-owner", result.Receipt.ID, result.Transition.ID, at); e != nil {
		t.Fatal(e)
	}
	completed, e := s.RecordReservedReceiptedAction(w.ID, in, at)
	if e != nil {
		t.Fatal(e)
	}
	if completed.Receipt.ID != result.Receipt.ID {
		t.Fatal("retry changed native receipt")
	}
	if _, _, present, e := s.ReadReceiptedActionReservationInput(w.ID, action.ID); e != nil || present {
		t.Fatalf("completed proof did not clear blocker: %v", e)
	}
	if _, e = s.RecordReservedReceiptedAction(w.ID, in, at); e != nil {
		t.Fatalf("exact completed retry failed: %v", e)
	}
}

func TestFailedLaunchedAttemptRequiresSeparateNativeRecovery(t *testing.T) {
	for _, scenario := range []string{"missing", "failed", "unlaunched", "wrong_owner", "wrong_hash", "wrong_target", "unbound", "caller_only", "valid"} {
		t.Run(scenario, func(t *testing.T) {
			s := newStore(t)
			w := governedActionRunningWork(t, s, "operator:test")
			rw, err := s.Create(CreateInput{Owner: "operator:test", Actor: "operator:test", SuccessPredicate: "separate recovery", State: "ready"}, fixedNow.Add(3*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Claim(rw.ID, "operator:test", fixedNow.Add(4*time.Second)); err != nil {
				t.Fatal(err)
			}
			if rw, err = s.Advance(rw.ID, AdvanceInput{To: "running", Actor: "operator:test"}, fixedNow.Add(5*time.Second)); err != nil {
				t.Fatal(err)
			}
			at := fixedNow.Add(2 * time.Minute)
			ai, pd := governedActionApproval(w, "operator:test", at)
			ai.ArgumentsHash = strings.Repeat("a", 64)
			attempts := runattempt.Open(s.home)
			ra, err := attempts.MintWithID(runattempt.MintDeterministic("uncertain-original", at), w.ID, "operator:test", "compound", at)
			if err != nil {
				t.Fatal(err)
			}
			if err = attempts.AttachActionIntent(ra.ID, ai.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = attempts.CommitLaunch(ra.ID, at, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			required := &RequiredAttemptCompletion{AttemptID: ra.ID, Owner: "fixture.install", Recovery: &RequiredRecoveryCompletion{WorkID: rw.ID, Owner: "fixture.recover", Capability: ai.Capability, ArgumentsHashes: []string{strings.Repeat("b", 64)}}}
			in := ReceiptedActionInput{Action: ai, PreEvaluatedDecision: &pd, ExpectedWorkState: "running", Outcome: "failed", ChangeKnowledge: "unknown", Summary: "configuration effect unknown; separate recovery required", MoveType: "control_plane_repair", RequiredAttemptCompletion: required}
			if err = s.ReserveReceiptedAction(w.ID, in, at, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			original, err := s.RecordReservedReceiptedAction(w.ID, in, at)
			body, jsonErr := json.Marshal(original.Receipt)
			if jsonErr != nil || strings.Contains(string(body), `"changed"`) || original.Receipt.StateDelta.ChangeKnowledge != "unknown" {
				t.Fatalf("native uncertainty receipt asserted known effects: %s %v", body, jsonErr)
			}

			if !errors.Is(err, ErrRequiredAttemptCompletion) {
				t.Fatal(err)
			}
			if _, err = attempts.CompleteWithOwnerProof(ra.ID, runattempt.StatusFailed, required.Owner, original.Receipt.ID, original.Transition.ID, at); err != nil {
				t.Fatal(err)
			}
			if _, err = s.RecordReservedReceiptedAction(w.ID, in, at); !errors.Is(err, ErrRequiredAttemptCompletion) {
				t.Fatalf("original failed proof released uncertainty: %v", err)
			}
			omitted := in
			copyRequired := *required
			copyRequired.Recovery = nil
			omitted.RequiredAttemptCompletion = &copyRequired
			if _, err = s.RecordReservedReceiptedAction(w.ID, omitted, at); err == nil {
				t.Fatal("legacy retry omitted frozen recovery obligation")
			}
			if scenario != "missing" {
				recoverAI, recoverPD := governedActionApproval(rw, "operator:test", at)
				recoverAI.ID = "act_fixture_recovery"
				recoverAI.ArgumentsHash = strings.Repeat("b", 64)
				recoverPD.ID = "pol_fixture_recovery"
				recoverPD.ActionIntentID = recoverAI.ID
				if scenario == "wrong_hash" {
					recoverAI.ArgumentsHash = strings.Repeat("c", 64)
				}
				if scenario == "wrong_target" {
					recoverAI.Target.Path = "/synthetic/other.json"
				}
				rra, err := attempts.MintWithID(runattempt.MintDeterministic("separate-recovery", at), rw.ID, "operator:test", "compound", at)
				if err != nil {
					t.Fatal(err)
				}
				if err = attempts.AttachActionIntent(rra.ID, recoverAI.ID); err != nil {
					t.Fatal(err)
				}
				if scenario != "unlaunched" {
					if _, err = attempts.CommitLaunch(rra.ID, at, func() error { return nil }); err != nil {
						t.Fatal(err)
					}
				}
				refs := []string{ra.ID, ai.ID, ai.ArgumentsHash, original.Receipt.ID, original.Transition.ID}
				if scenario == "unbound" {
					refs = refs[:3]
				}
				outcome, status, owner := "completed", runattempt.StatusSucceeded, "fixture.recover"
				if scenario == "failed" {
					outcome, status = "failed", runattempt.StatusFailed
				}
				if scenario == "wrong_owner" {
					owner = "fixture.other-owner"
				}
				if scenario != "caller_only" {
					cfg := policy.DefaultConfig()
					if _, err = s.events.PutJSON(recoverAI.ID, recoverAI, "  "); err != nil {
						t.Fatal(err)
					}
					approvals := policy.NewExactActionApprovalStore(s.home)
					plan, _, _, err := approvals.EnsurePlanForIntent(recoverAI, cfg, 5*time.Minute, at)
					if err != nil {
						t.Fatal(err)
					}
					if _, _, err = approvals.Approve(plan.ID, "operator:test", cfg, at); err != nil {
						t.Fatal(err)
					}
					var matched bool
					recoverPD, matched, err = approvals.ApplyMatching(recoverAI, cfg, policy.Evaluate(recoverAI, cfg, at), at)
					if err != nil || !matched {
						t.Fatalf("native exact recovery approval: %v matched=%v", err, matched)
					}
					if _, err = s.events.PutJSON(recoverPD.ID, recoverPD, "  "); err != nil {
						t.Fatal(err)
					}
				}
				recoveryIn := ReceiptedActionInput{Action: recoverAI, PreEvaluatedDecision: &recoverPD, ExpectedWorkState: "running", Outcome: outcome, Summary: "synthetic bounded recovery proof", MoveType: "control_plane_repair", ProofRefs: refs}
				if err = s.ReserveReceiptedAction(rw.ID, recoveryIn, at, func() error { return nil }); err != nil {
					t.Fatal(err)
				}
				recovered, err := s.RecordReservedReceiptedAction(rw.ID, recoveryIn, at)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = attempts.CompleteWithOwnerProof(rra.ID, status, owner, recovered.Receipt.ID, recovered.Transition.ID, at); err != nil {
					t.Fatal(err)
				}
			}
			_, err = s.RecordReservedReceiptedAction(w.ID, in, at)
			if scenario == "valid" {
				if err != nil {
					t.Fatalf("genuine separate recovery did not clear: %v", err)
				}
			} else if !errors.Is(err, ErrRequiredAttemptCompletion) {
				t.Fatalf("invalid recovery released uncertainty: %v", err)
			}
			originalAttempt, err := attempts.Get(ra.ID)
			if err != nil || originalAttempt.Status != runattempt.StatusFailed || originalAttempt.TerminalizationReceiptID != original.Receipt.ID {
				t.Fatal("recovery rewrote original failed terminality", err)
			}
		})
	}
}
