package work

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/policy"
	"compound/internal/runattempt"
)

const QuiescentProofRecoveryHelp = "  --reconcile-quiescent PFR --work W --actor A --expected-run R  fail an exact fresh local proof after execution barriers close; never admit or certify"
const quiescentProofFailure = "proof execution-quiescent reconciliation v1: no committed result; producer exit unknown"

type QuiescentProofRecoveryInput struct {
	Actor                string
	ReservationID        string
	ExpectedRunAttemptID string
}

// ReconcileQuiescentProofResource fails an exact still-live local reservation.
// All refusal checks precede the existing finalizer under its lifecycle lock.
// Barrier quiescence is not evidence that the producer process exited.
func (s *Store) ReconcileQuiescentProofResource(workID string, in QuiescentProofRecoveryInput, now time.Time) (ProofResourceCompleteResult, error) {
	var result ProofResourceCompleteResult
	err := s.withLifecycleMutation(func() error {
		workID, in.Actor = strings.TrimSpace(workID), strings.TrimSpace(in.Actor)
		in.ReservationID, in.ExpectedRunAttemptID = strings.TrimSpace(in.ReservationID), strings.TrimSpace(in.ExpectedRunAttemptID)
		if workID == "" || in.Actor == "" || in.ReservationID == "" || in.ExpectedRunAttemptID == "" {
			return fmt.Errorf("quiescent recovery requires exact work, actor, reservation and expected run")
		}
		if _, err := os.Lstat(s.claimPendingPath(workID)); err == nil {
			return fmt.Errorf("quiescent recovery refuses pending claim for %s", workID)
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := s.fenceClosePendingMutation(workID); err != nil {
			return err
		}
		if err := s.fenceSweepStalePendingMutation(workID); err != nil {
			return err
		}
		if _, present, _, err := s.readProofResourceLegacyRecoveryPending(workID); err != nil {
			return err
		} else if present {
			return fmt.Errorf("quiescent recovery refuses legacy recovery journal")
		}

		w, err := s.resourceMutationOwner(workID, in.Actor)
		if err != nil {
			return err
		}
		var reservation ProofResourceReservation
		if err := s.readProofReservationInto(in.ReservationID, &reservation); err != nil {
			return err
		}
		if reservation.WorkID != workID || reservation.Actor != in.Actor || reservation.RunAttemptID != in.ExpectedRunAttemptID ||
			reservation.Status != ProofResourceAcquired || reservation.SchemaVersion != ProofResourceCoordinatedReservationSchemaVersion ||
			reservation.ProofCoordination == nil || reservation.Profile.SourceSnapshot == nil || reservation.Profile.RemoteGoTest != nil {
			return fmt.Errorf("quiescent recovery requires exact coordinated local acquired reservation %s", in.ReservationID)
		}
		if err := validateProofResourceProfileShape(reservation.Profile); err != nil {
			return err
		}
		digest, err := proofResourceProfileDigest(reservation.Profile)
		if err != nil {
			return err
		}
		if digest != reservation.Profile.Digest {
			return fmt.Errorf("quiescent recovery retained profile digest mismatch")
		}

		reservation, err = s.proofReservationWithLatestRefresh(reservation)
		if err != nil {
			return err
		}
		expectedAction, err := proofReservationExpectedAction(reservation)
		if err != nil {
			return err
		}
		if err := s.validateStoredProofGovernance(w, in.Actor, reservation.ActionIntentID, reservation.PolicyDecisionID, expectedAction); err != nil {
			return err
		}
		completionInput := ProofResourceCompleteInput{Actor: in.Actor, ReservationID: reservation.ID, FailureReason: quiescentProofFailure}
		pending, present, pendingPath, err := s.readProofResourceCompletionPending(reservation.ID)
		if err != nil {
			return err
		}
		if present {
			ra, err := s.runAttempts().Get(reservation.RunAttemptID)
			if err != nil {
				return err
			}
			if pending.Result.RunAttemptID != in.ExpectedRunAttemptID || pending.Result.FailureReason != quiescentProofFailure ||
				pending.Result.Status != ProofResourceFailed || pending.Result.ReportArtifactRef != "" || pending.Result.ProducerExitKnown ||
				pending.Result.ProducerExitCode != 0 || pending.VerificationRef != "" || pending.TerminalStatus != runattempt.StatusFailed ||
				ra.ID != reservation.RunAttemptID || ra.ActionIntentID != reservation.ActionIntentID ||
				ra.Workspace != reservation.Workspace || ra.Command != reservation.Command {
				return fmt.Errorf("quiescent recovery refuses different completion journal")
			}
			expect, err := s.deriveProofResourceCompletionExpectation(workID, reservation, completionInput)
			if err != nil {
				return err
			}
			normalized, migrated, err := s.validateAndNormalizeProofResourceCompletionPending(w, reservation, pending, ra, completionInput, expect)
			if err != nil {
				return err
			}
			if migrated || !reflect.DeepEqual(normalized, pending) {
				return fmt.Errorf("quiescent recovery refuses journal migration")
			}
			if ra.Status != runattempt.StatusRunning && !proofRunAttemptMatchesPreparedCompletion(ra, pending) {
				return fmt.Errorf("quiescent recovery original terminal state differs from pending failure")
			}
			// The general finalizer can adopt/create retry descendants. This
			// narrow door only resumes the original attempt, never that branch.
			attempts, err := s.runAttempts().List()
			if err != nil {
				return err
			}
			for _, attempt := range attempts {
				if attempt.PredecessorID == reservation.RunAttemptID {
					return fmt.Errorf("quiescent recovery refuses existing retry descendant %s", attempt.ID)
				}
			}
		} else if _, err := s.activeProofRunAttempt(reservation, workID, in.Actor); err != nil {
			return fmt.Errorf("quiescent recovery requires unresolved original run %s: %w", reservation.RunAttemptID, err)
		}

		currentAction := ""
		if len(w.ActionIntentIDs) > 0 {
			currentAction = w.ActionIntentIDs[len(w.ActionIntentIDs)-1]
		}
		if present && currentAction == pending.Action.ID && w.PolicyDecisionID == pending.Decision.ID {
			if err := s.validateStoredProofGovernance(w, in.Actor, currentAction, w.PolicyDecisionID, pending.Action.Action); err != nil {
				return err
			}
		} else if currentAction == reservation.ActionIntentID {
			if w.PolicyDecisionID != reservation.PolicyDecisionID {
				return fmt.Errorf("quiescent recovery work policy generation changed")
			}
		} else {
			actionTime, err := proofRefreshActionTime(reservation)
			if err != nil {
				return err
			}
			refresh := s.proofResourceAction(w, reservation.ID, in.Actor, "refresh", "", "", "", "", actionTime)
			if currentAction != refresh.ID {
				return fmt.Errorf("quiescent recovery work action generation changed")
			}
			if err := s.validateStoredProofGovernance(w, in.Actor, currentAction, w.PolicyDecisionID, "refresh_full_proof_resource"); err != nil {
				return err
			}
		}

		if !oneOf(reservation.RunAttemptID, w.RunAttemptIDs) {
			return fmt.Errorf("quiescent recovery run is not reachable from work")
		}
		rid, run, wid, ok := parseProofResourceHolder(reservation.Holder)
		if !ok || rid != reservation.ID || run != in.ExpectedRunAttemptID || wid != workID ||
			reservation.Holder != proofResourceHolder(reservation.ID, reservation.RunAttemptID, workID, in.Actor) {
			return fmt.Errorf("quiescent recovery holder binding mismatch")
		}
		// The reservation preserves the original proof window. A native refresh
		// advances the live lease's AcquiredAt; authenticate that exact window
		// from the refresh chain already validated above.
		acquiredAt := reservation.AcquiredAt
		refs, _, err := s.proofRefreshRefs(reservation)
		if err != nil {
			return err
		}
		latest := ProofResourceRefresh{}
		for _, ref := range refs {
			var refresh ProofResourceRefresh
			if err := s.readEventJSON(ref, &refresh); err != nil {
				return err
			}
			if refresh.ReservationID == reservation.ID && (latest.ID == "" || refresh.RefreshedAt > latest.RefreshedAt) {
				latest = refresh
			}
		}
		if latest.ID != "" {
			acquiredAt = latest.RefreshedAt
		}

		if err := s.validatePrivateSourceProfileAuthority(workID, in.Actor, reservation.Profile, now); err != nil {
			return err
		}
		_, decision, _, err := s.proofResourceCompletionGovernance(w, reservation, in.Actor, now)
		if err != nil {
			return err
		}
		if !policy.Allowed(decision) {
			return fmt.Errorf("quiescent recovery completion policy %s", decision.Outcome)
		}
		resource := landlease.ProofResourceForIdentity(s.home, reservation.ProofCoordination)
		status, err := resource.ObserveHistoricalHolder(reservation.Holder, now)
		if err != nil {
			return err
		}
		if status.Holder != reservation.Holder {
			if !present {
				return fmt.Errorf("quiescent recovery original holder is absent or changed")
			}
			if err := s.validateQuiescentProofCommittedCleanup(w, pending); err != nil {
				return err
			}
			if err := s.clearProofResourceCompletionPending(pendingPath, pending); err != nil {
				return err
			}
			result = ProofResourceCompleteResult{
				SchemaVersion: ProofResourceResultSchemaVersion, Result: pending.Result, Work: w,
				ReleaseWarning: "original holder already absent; exact committed failure journal cleared only",
			}
			return nil
		}

		if status.IsExpired {
			return fmt.Errorf("quiescent recovery refuses expired holder %s", reservation.ID)
		}
		if !status.Held || status.Holder != reservation.Holder ||
			!reflect.DeepEqual(status.ProofCoordination, reservation.ProofCoordination) ||
			status.AcquiredAt != acquiredAt || status.ExpiresAt != reservation.ExpiresAt ||
			status.TTLSeconds != reservation.TTLSeconds || status.PriorHolder != reservation.PriorHolder ||
			status.StealReason != reservation.StealReason || status.StolenAt != reservation.StolenAt {
			return fmt.Errorf("quiescent recovery live holder or lease generation changed")
		}
		if _, err := resource.AttestExecutionTerminated(reservation.Holder); err != nil {
			return err
		}
		// Do not call public CompleteProofResource: its error terminalizer deliberately
		// mutates failed execution attempts. Refusals above must leave them untouched.
		result, err = s.completeProofResourceLocked(workID, completionInput, now)
		return err
	})
	return result, err
}

func RunQuiescentProofRecoveryCLI(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("proof-check --reconcile-quiescent", flag.ContinueOnError)
	flags.SetOutput(stderr)
	reservation := flags.String("reconcile-quiescent", "", "exact retained local reservation")
	workID := flags.String("work", "", "owned work")
	actor := flags.String("actor", "", "current actor")
	expectedRun := flags.String("expected-run", "", "exact original running attempt")
	homeArg := flags.String("home", "", "owning home")
	flags.Bool("json", false, "emit JSON")
	seen := map[string]bool{}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			key := strings.SplitN(strings.TrimLeft(arg, "-"), "=", 2)[0]
			if seen[key] {
				fmt.Fprintln(stderr, "duplicate recovery option:", key)
				return 2
			}
			seen[key] = true
		}
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || strings.TrimSpace(*reservation) == "" || strings.TrimSpace(*workID) == "" || strings.TrimSpace(*actor) == "" || strings.TrimSpace(*expectedRun) == "" {
		fmt.Fprintln(stderr, "exact --reconcile-quiescent, --work, --actor and --expected-run required")
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
	result, err := store.ReconcileQuiescentProofResource(*workID, QuiescentProofRecoveryInput{
		Actor: *actor, ReservationID: *reservation, ExpectedRunAttemptID: *expectedRun,
	}, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// validateQuiescentProofCommittedCleanup authenticates a native committed failure.
// It authorizes only unlinking its exact old journal after the holder is gone.
func (s *Store) validateQuiescentProofCommittedCleanup(w envelope.WorkEnvelope, pending proofResourceCompletionPending) error {
	ra, err := s.runAttempts().Get(pending.Result.RunAttemptID)
	if err != nil {
		return err
	}
	if !proofRunAttemptMatchesPreparedCompletion(ra, pending) {
		return fmt.Errorf("quiescent cleanup lacks matching terminal original")
	}
	committed, ok := s.visibleProofCompletionCommit(w.ID, pending, ra)
	if !ok || !reflect.DeepEqual(committed, w) {
		return fmt.Errorf("quiescent cleanup lacks durable committed failure")
	}
	var action envelope.ActionIntent
	var decision envelope.PolicyDecision
	var result ProofResourceResult
	var completion ProofResourceCompletion
	records := []struct {
		id        string
		got, want any
	}{
		{pending.Action.ID, &action, &pending.Action},
		{pending.Decision.ID, &decision, &pending.Decision},
		{pending.Result.ID, &result, &pending.Result},
		{pending.Completion.ID, &completion, &pending.Completion},
	}
	for _, record := range records {
		if err := s.readEventJSON(record.id, record.got); err != nil {
			return err
		}
		if !reflect.DeepEqual(record.got, record.want) {
			return fmt.Errorf("quiescent cleanup committed record %s differs from sealed journal", record.id)
		}
	}
	events, err := s.eventsFor(w.ID)
	if err != nil {
		return err
	}
	for _, event := range events {
		if reflect.DeepEqual(event, pending.LifecycleEvent) {
			return nil
		}
	}
	return fmt.Errorf("quiescent cleanup lacks committed lifecycle evidence")
}
