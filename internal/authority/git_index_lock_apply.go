package authority

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"compound/internal/policy"
	"compound/internal/work"
	"golang.org/x/sys/unix"
)

type GitIndexLockResult struct {
	PlanID       string                                `json:"plan_id"`
	Outcome      string                                `json:"outcome"`
	Recovery     gitrepository.IndexLockRecoveryResult `json:"recovery"`
	ReceiptID    string                                `json:"receipt_id"`
	TransitionID string                                `json:"transition_id"`
	Replayed     bool                                  `json:"replayed"`
}

type gitIndexLockOutcome struct {
	Digest       string                                `json:"digest"`
	Failure      string                                `json:"failure,omitempty"`
	PlanID       string                                `json:"plan_id"`
	ReviewID     string                                `json:"review_id"`
	AuthorizedAt time.Time                             `json:"authorized_at"`
	Pending      work.ReceiptedActionInput             `json:"pending"`
	Final        work.ReceiptedActionInput             `json:"final"`
	Recovery     gitrepository.IndexLockRecoveryResult `json:"recovery"`
}

// ApplyGitIndexLockRecovery is the only runtime effect owner for index locks.
// Review validation precedes logical-repository policy admission; this is not
// an exception to raw .git path protection. Interrupted attempts are reconciled
// read-only and receipted, never automatically repeated against a new lock.
func ApplyGitIndexLockRecovery(ctx context.Context, home, planID, reviewID, actor string, idle gitrepository.IndexLockLiveness, clock func() time.Time) (GitIndexLockResult, error) {
	// Serialize effect/reconciliation for this review family, independently of
	// short work-store transactions. A second apply must not seal failure while
	// the first invocation is still moving the same lock.
	fd, err := unix.Open(filepath.Join(home, "reviews", gitIndexLockArea), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return GitIndexLockResult{}, err
	}
	dir := os.NewFile(uintptr(fd), "index-lock-reviews")
	defer dir.Close()
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return GitIndexLockResult{}, fmt.Errorf("another index-lock apply or reconciliation is active: %w", err)
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	return applyGitIndexLockRecovery(ctx, home, planID, reviewID, actor, idle, clock)
}

func applyGitIndexLockRecovery(ctx context.Context, home, planID, reviewID, actor string, idle gitrepository.IndexLockLiveness, clock func() time.Time) (GitIndexLockResult, error) {
	var result GitIndexLockResult
	if clock == nil {
		clock = time.Now
	}
	if idle == nil {
		idle = gitrepository.LSOFIndexLockLiveness
	}
	plan, err := loadGitIndexLockPlan(home, planID)
	if err != nil {
		return result, err
	}
	if actor != plan.Actor {
		return result, fmt.Errorf("recovery actor does not own plan")
	}
	var review GitIndexLockReview
	if err := readIndexRecoveryArtifact(home, reviewID+".review.json", &review); err != nil {
		return result, err
	}
	if review.ID != reviewID {
		return result, fmt.Errorf("review filename identity mismatch")
	}
	// Historical replay may happen after expiry, but it cannot repeat an effect.
	if err := validateGitIndexLockReview(plan, review, review.IssuedAt); err != nil {
		return result, err
	}
	store, err := work.Open(home)
	if err != nil {
		return result, err
	}
	at := clock()
	action := indexRecoveryAction(plan, review.ID, "recover_reviewed_stale_index_lock", plan.State.Snapshot.Repo, at)
	action.EvidenceRefs = append(action.EvidenceRefs, "git-index-lock-review:"+review.ID, review.Window.EvidenceRef)
	stem := action.ID
	var saved gitIndexLockOutcome
	if err := readIndexRecoveryArtifact(home, stem+".outcome.json", &saved); err == nil {
		if err := validateIndexLockOutcome(plan, review, saved); err != nil {
			return result, err
		}
		return finishGitIndexLockOutcome(ctx, store, plan, saved, true, idle)
	} else if !os.IsNotExist(err) {
		return result, err
	}
	existing, authorizedAt, present, err := store.ReadReceiptedActionReservationInput(plan.WorkID, action.ID)
	if err != nil {
		return result, err
	}
	if present {
		// A reservation means a previous attempt reached admission. The durable
		// locator was required before movement. Missing/corrupt evidence never
		// licenses a second attempt or removal of a replacement lock.
		saved = gitIndexLockOutcome{PlanID: plan.ID, ReviewID: review.ID, AuthorizedAt: authorizedAt, Pending: existing}
		if err := validateIndexLockPending(plan, review, saved); err != nil {
			return result, err
		}
		err := readIndexRecoveryArtifact(home, stem+".prepared.json", &saved.Recovery)
		if err != nil && !os.IsNotExist(err) {
			return result, err
		}
		cause := fmt.Errorf("interrupted before a verified terminal outcome; retained recovery evidence")
		if err == nil {
			saved.Recovery.Changed = true // At least the private preimage exists.
			if verifyErr := gitrepository.VerifyIndexLockRecoveryEvidence(ctx, plan.State, saved.Recovery, idle); verifyErr == nil {
				saved.Recovery.Phase = "verified"
				cause = nil
			}
		}
		saved.Final = indexLockFinalInput(existing, saved.Recovery, cause)
		if cause != nil {
			saved.Failure = cause.Error()
		}
		saved.Digest = indexRecoveryDigest(saved)
		if err := writeIndexRecoveryArtifact(home, stem+".outcome.json", saved); err != nil {
			return result, err
		}
		return finishGitIndexLockOutcome(ctx, store, plan, saved, true, idle)
	}
	if err := validateGitIndexLockReview(plan, review, at); err != nil {
		return result, err
	}
	cfg, _, err := policy.LoadConfig(home)
	if err != nil {
		return result, err
	}
	decision := policy.Evaluate(action, cfg, at)
	if !policy.Allowed(decision) {
		return result, fmt.Errorf("recovery policy %s: %s", decision.ID, decision.Outcome)
	}
	in := work.ReceiptedActionInput{Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running", OutcomePending: true, Outcome: "blocked", Summary: "awaiting exact index-lock recovery outcome", MoveType: "control_plane_repair", ProofRefs: []string{"git-index-lock-review:" + review.ID, review.Window.EvidenceRef}, BeforeRefs: []string{"sha256:" + plan.Digest}}
	authorize := func() error {
		now := clock()
		if err := validateGitIndexLockReview(plan, review, now); err != nil {
			return err
		}
		fresh, _, err := policy.LoadConfig(home)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(fresh, cfg) {
			return fmt.Errorf("policy authority changed during recovery")
		}
		if !policy.Allowed(policy.Evaluate(action, fresh, now)) {
			return fmt.Errorf("fresh recovery policy denied")
		}
		for _, class := range []string{"build-implement", action.Capability} {
			halted, _, err := policy.AutonomyHalted(home, class)
			if err != nil {
				return err
			}
			if halted {
				return fmt.Errorf("recovery autonomy halted")
			}
		}
		return nil
	}
	if err := store.ReserveReceiptedAction(plan.WorkID, in, at, authorize); err != nil {
		return result, err
	}
	freshAdmission := func() error {
		// Unlike same-reservation admission, this seam always runs authorize
		// and revalidates current running work and the actor's current lease.
		return store.ReauthorizeReceiptedActionReservation(plan.WorkID, in, at, in, at, authorize)
	}
	recovery, moveErr := gitrepository.RecoverIndexLockJournaled(ctx, plan.State, idle, freshAdmission, func(locator gitrepository.IndexLockRecoveryResult) error {
		return writeIndexRecoveryArtifact(home, stem+".prepared.json", locator)
	})
	saved = gitIndexLockOutcome{PlanID: plan.ID, ReviewID: review.ID, AuthorizedAt: at, Pending: in, Final: indexLockFinalInput(in, recovery, moveErr), Recovery: recovery}
	if moveErr != nil {
		saved.Failure = moveErr.Error()
	}
	saved.Digest = indexRecoveryDigest(saved)
	if err := writeIndexRecoveryArtifact(home, stem+".outcome.json", saved); err != nil {
		return result, fmt.Errorf("recovery outcome unrecorded; keep reservation and reconcile: %w", err)
	}
	result, err = finishGitIndexLockOutcome(ctx, store, plan, saved, false, idle)
	if err != nil {
		return result, err
	}
	return result, moveErr
}

func indexLockFinalInput(in work.ReceiptedActionInput, recovery gitrepository.IndexLockRecoveryResult, cause error) work.ReceiptedActionInput {
	in.OutcomePending = false
	in.Changed = recovery.Changed
	in.Outcome = "completed"
	in.Summary = "exact stale index lock recovered; index and tracked WIP unchanged"
	if recovery.RecoveryDir != "" {
		in.AfterRefs = []string{"artifact:" + recovery.RecoveryDir}
	}
	if cause != nil {
		in.Outcome = "failed"
		in.Summary = "index-lock recovery incomplete; retain evidence and do not overwrite any replacement"
		in.FailureCause = &envelope.FailureCause{SurfaceOutcome: "index-lock recovery incomplete", Mechanism: cause.Error()}
	}
	return in
}

func validateIndexLockPending(plan GitIndexLockPlan, review GitIndexLockReview, saved gitIndexLockOutcome) error {
	expected := indexRecoveryAction(plan, review.ID, "recover_reviewed_stale_index_lock", plan.State.Snapshot.Repo, saved.AuthorizedAt)
	expected.EvidenceRefs = append(expected.EvidenceRefs, "git-index-lock-review:"+review.ID, review.Window.EvidenceRef)
	if saved.PlanID != plan.ID || saved.ReviewID != review.ID || !reflect.DeepEqual(expected, saved.Pending.Action) || !saved.Pending.OutcomePending {
		return fmt.Errorf("pending recovery transaction does not bind exact reviewed action")
	}
	return validateGitIndexLockReview(plan, review, saved.AuthorizedAt)
}

func validateIndexLockOutcome(plan GitIndexLockPlan, review GitIndexLockReview, saved gitIndexLockOutcome) error {
	if err := validateIndexLockPending(plan, review, saved); err != nil {
		return err
	}
	if !reflect.DeepEqual(saved.Pending.Action, saved.Final.Action) || saved.Final.OutcomePending {
		return fmt.Errorf("recovery outcome action mismatch")
	}
	copy := saved
	copy.Digest = ""
	if saved.Digest != indexRecoveryDigest(copy) {
		return fmt.Errorf("recovery outcome digest mismatch")
	}
	var cause error
	if saved.Failure != "" {
		cause = fmt.Errorf("%s", saved.Failure)
	}
	if !reflect.DeepEqual(saved.Final, indexLockFinalInput(saved.Pending, saved.Recovery, cause)) {
		return fmt.Errorf("recovery outcome fields are inconsistent")
	}
	if saved.Final.Outcome == "completed" && (saved.Recovery.Phase != "verified" || !saved.Recovery.Changed || saved.Recovery.RecoveryDir == "") {
		return fmt.Errorf("completed recovery requires verified changed-state evidence")
	}
	return nil
}

func finishGitIndexLockOutcome(ctx context.Context, store *work.Store, plan GitIndexLockPlan, saved gitIndexLockOutcome, replayed bool, idle gitrepository.IndexLockLiveness) (GitIndexLockResult, error) {
	result := GitIndexLockResult{PlanID: plan.ID, Outcome: saved.Final.Outcome, Recovery: saved.Recovery, Replayed: replayed}
	current, _, present, err := store.ReadReceiptedActionReservationInput(plan.WorkID, saved.Pending.Action.ID)
	if err != nil {
		return result, err
	}
	if present {
		if current.OutcomePending && saved.Final.Outcome == "completed" {
			if err := gitrepository.ValidateIndexLockRecoveryEvidence(plan.State, saved.Recovery); err != nil {
				// A crash before the low-level verified marker can be reconciled
				// only by fresh exact postconditions, never by the saved claim.
				if err := gitrepository.VerifyIndexLockRecoveryEvidence(ctx, plan.State, saved.Recovery, idle); err != nil {
					return result, err
				}
			}
		}
		if err := store.SealReceiptedActionOutcome(plan.WorkID, saved.Pending, saved.Final, saved.AuthorizedAt); err != nil {
			return result, err
		}
	}
	recorded, err := store.RecordReservedReceiptedAction(plan.WorkID, saved.Final, saved.AuthorizedAt)
	if err != nil {
		return result, err
	}
	result.ReceiptID, result.TransitionID = recorded.Receipt.ID, recorded.Transition.ID
	if result.Outcome != "completed" {
		return result, fmt.Errorf("recovery failed; exact outcome receipted; retained evidence requires reconciliation")
	}
	return result, nil
}
