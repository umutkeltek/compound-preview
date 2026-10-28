package work

import (
	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/policy"
	"compound/internal/runattempt"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"time"
)

var remoteProofTransportCommand = exec.CommandContext

func (s *Store) ReconcileRemoteProof(ctx context.Context, workID, actor, reservationID string, cancel bool, now time.Time) (evidence *RemoteProofEvidence, err error) {
	var reservation ProofResourceReservation
	if err := s.readProofReservationInto(reservationID, &reservation); err != nil {
		return nil, err
	}
	if reservation.WorkID != workID || reservation.Actor != actor {
		return nil, fmt.Errorf("remote reconciliation owner mismatch")
	}
	request, err := remoteRequestFor(reservation)
	if err != nil {
		return nil, err
	}
	operation := "inspect"
	if cancel {
		operation = "cancel"
	}
	var w envelope.WorkEnvelope
	var action envelope.ActionIntent
	var decision envelope.PolicyDecision
	var attempt runattempt.RunAttempt
	var workDigest string
	err = s.withLifecycleMutation(func() error {
		var e error
		w, e = s.resourceMutationOwner(workID, actor)
		if e != nil {
			return e
		}
		workDigest, e = workEnvelopeSHA256(w)
		if e != nil {
			return e
		}
		requestDigest, _ := remoteDigest(request)
		action = s.resourceMutationAction(w, actor, "proof.remote-"+operation, "reconcile exact remote request "+request.ID+" digest="+requestDigest+" profile="+reservation.Profile.Digest, reservation.Workspace, now)
		decision, e = s.authorizeResourceMutationLocked(w, action, now)
		if e != nil {
			return e
		}
		attempt, e = s.runAttempts().MintWithActionIntent(workID, actor, "proof-remote-reconcile", action.ID, now)
		if e != nil {
			return e
		}
		if _, e = s.runAttempts().SetOrigin(attempt.ID, runattempt.OriginHarnessExecution); e != nil {
			return e
		}
		if _, e = s.runAttempts().SetWorkspace(attempt.ID, reservation.Workspace); e != nil {
			return e
		}
		if _, e = s.runAttempts().SetCommand(attempt.ID, "compound proof-check --remote-reconcile "+reservation.ID+" "+operation); e != nil {
			return e
		}
		if _, e = s.runAttempts().SetPredecessor(attempt.ID, reservation.RunAttemptID); e != nil {
			return e
		}
		return nil
	})
	if err != nil {
		if attempt.ID != "" {
			_, terminalErr := s.runAttempts().CompleteWithReason(attempt.ID, runattempt.StatusFailed, "reconciliation admission failed", time.Now())
			err = errors.Join(err, terminalErr)
		}
		return nil, err
	}
	defer func() {
		status := runattempt.StatusSucceeded
		reason := "exact remote receipt reconciled"
		if err != nil {
			status = runattempt.StatusFailed
			reason = "remote reconciliation did not complete"
		}
		_, terminalErr := s.runAttempts().CompleteWithReason(attempt.ID, status, reason, time.Now())
		err = errors.Join(err, terminalErr)
		finishErr := s.withLifecycleMutation(func() error {
			current, e := s.resourceMutationOwner(workID, actor)
			if e != nil {
				return e
			}
			digest, e := workEnvelopeSHA256(current)
			if e != nil {
				return e
			}
			if digest != workDigest {
				return s.appendEvent(Event{TS: ts(time.Now()), WorkID: workID, From: current.State, To: current.State, Note: "remote reconciliation result retained; newer work generation preserved", Attached: []string{action.ID, decision.ID, attempt.ID}})
			}
			return s.finishResourceMutationLocked(w, action, decision, err, time.Now())
		})
		err = errors.Join(err, finishErr)
	}()
	validate := func() error {
		current, e := s.Get(workID)
		if e != nil {
			return e
		}
		digest, e := workEnvelopeSHA256(current)
		if e != nil {
			return e
		}
		if digest != workDigest {
			return fmt.Errorf("remote reconciliation work changed before launch")
		}
		if e := validatePreEvaluatedReceiptedActionAt(action, decision, time.Now()); e != nil {
			return e
		}
		for _, class := range []string{"build-implement", action.Capability} {
			halted, _, e := policy.AutonomyHalted(s.home, class)
			if e != nil {
				return e
			}
			if halted {
				return fmt.Errorf("remote reconciliation halted")
			}
		}
		return nil
	}
	state, err := remoteTransport(ctx, reservation.Profile.RemoteGoTest.Route, request, operation, "", remoteProofTransportCommand, func(cmd *exec.Cmd) error {
		_, e := s.runAttempts().CommitLaunch(attempt.ID, time.Now(), func() error {
			if e := validate(); e != nil {
				return e
			}
			return cmd.Start()
		})
		if e != nil {
			return e
		}
		return cmd.Wait()
	})
	if err != nil {
		return nil, err
	}
	evidence, err = s.remoteProofCustody(reservation, request, state, validate)
	if err == nil {
		_, err = s.runAttempts().SetProofRefs(attempt.ID, []string{evidence.RequestArtifactRef, evidence.ReceiptArtifactRef})
	}
	if err == nil {
		err = s.finishInterruptedRemoteProof(reservation, evidence, time.Now())
	}
	return evidence, err
}
func RunRemoteProofRecoveryCLI(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("proof-check --remote-reconcile", flag.ContinueOnError)
	flags.SetOutput(stderr)
	reservation := flags.String("remote-reconcile", "", "exact retained reservation")
	workID := flags.String("work", "", "owned work")
	actor := flags.String("actor", "", "current actor")
	homeArg := flags.String("home", "", "owning home")
	cancel := flags.Bool("cancel", false, "request exact cancellation and reconcile quiescence")
	flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *reservation == "" || *workID == "" || *actor == "" {
		fmt.Fprintln(stderr, "exact --remote-reconcile, --work, --actor required")
		return 2
	}
	root, err := home.Resolve(*homeArg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	store, err := Open(root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	evidence, err := store.ReconcileRemoteProof(context.Background(), *workID, *actor, *reservation, *cancel, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(evidence); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func (s *Store) finishInterruptedRemoteProof(reservation ProofResourceReservation, evidence *RemoteProofEvidence, now time.Time) error {
	if err := validateRemoteProofEvidence(evidence, reservation); err != nil {
		return err
	}
	resource := landlease.ProofResourceForIdentity(s.home, reservation.ProofCoordination)
	if _, err := resource.AttestExecutionTerminated(reservation.Holder); err != nil {
		return fmt.Errorf("original local proof must terminate before remote recovery finalizes: %w", err)
	}
	pending, present, _, err := s.readProofResourceCompletionPending(reservation.ID)
	if err != nil {
		return err
	}
	var result ProofResourceCompleteResult
	if present {
		if pending.Result.Status == ProofResourceVerified || pending.VerificationRef != "" || pending.TerminalStatus != runattempt.StatusFailed {
			return fmt.Errorf("remote recovery cannot donate successful proof")
		}
		var found bool
		result, found, err = s.ResumeProofResourceCompletion(reservation.WorkID, ProofResourceResumeInput{Actor: reservation.Actor, Workspace: reservation.Workspace, Command: reservation.Command, Profile: reservation.Profile}, now)
		if err == nil && !found {
			err = fmt.Errorf("remote interrupted completion disappeared")
		}
	} else {
		status, e := resource.Status(now)
		if e != nil {
			return e
		}
		original, e := s.runAttempts().Get(reservation.RunAttemptID)
		if e != nil {
			return e
		}
		if runattempt.IsTerminal(original.Status) {
			if original.Status != runattempt.StatusFailed && original.Status != runattempt.StatusAbandoned {
				return fmt.Errorf("remote journal-free predecessor is not a failed or abandoned attempt")
			}
			if original.WorkID != reservation.WorkID || original.Actor != reservation.Actor ||
				original.ActionIntentID != reservation.ActionIntentID || original.Harness != proofResourceHarness ||
				original.Origin != runattempt.OriginHarnessExecution || original.Workspace != reservation.Workspace ||
				original.Command != reservation.Command || status.Holder != reservation.Holder {
				return fmt.Errorf("remote journal-free predecessor identity or holder changed")
			}
			recoveryStatus := "waiting_for_expiry"
			if status.IsExpired {
				recoveryStatus = "ready_for_fresh_proof"
			}
			evidence.Recovery = &RemoteProofRecovery{Status: recoveryStatus, OriginalAttemptStatus: original.Status,
				ResourceReleased: false, EligibleAfter: status.ExpiresAt, NextCommand: reservation.Command}
			return nil
		}
		if status.IsExpired && status.Holder == reservation.Holder {
			if _, e := resource.RearmExpiredExact(status, now); e != nil {
				return e
			}
		}
		result, err = s.CompleteProofResource(reservation.WorkID, ProofResourceCompleteInput{Actor: reservation.Actor, ReservationID: reservation.ID, FailureReason: "remote proof interrupted; exact quiescence reconciled; fresh proof requires fresh reservation", ProducerExitKnown: true, ProducerExitCode: 1}, now)
	}
	if err != nil {
		return err
	}
	if result.Result.Status == ProofResourceVerified || result.VerificationRef != "" || result.Result.RunAttemptStatus != runattempt.StatusFailed || !result.ResourceReleased {
		return fmt.Errorf("remote recovery did not fail and release original proof")
	}
	evidence.Recovery = &RemoteProofRecovery{Status: "failed_proof_released", OriginalAttemptStatus: runattempt.StatusFailed, ResourceReleased: true, NextCommand: reservation.Command}
	return nil
}
