package work

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
)

// SealReceiptedActionOutcome replaces a pending-outcome reservation once, after
// the adapter durably records what actually happened. It cannot change action,
// policy, authorization time, work admission or preimages. The reservation is
// never removed, including on partial failure. Replay must match exactly.
// This is outcome recording, not permission to retry an external effect.
func (s *Store) SealReceiptedActionOutcome(workID string, pending, final ReceiptedActionInput, authorizedAt time.Time) error {
	if !pending.OutcomePending || pending.Outcome != "blocked" || pending.Changed || pending.ChangeKnowledge != "" || final.OutcomePending || !oneOf(final.Outcome, envelope.ReceiptOutcomes) || strings.TrimSpace(final.Summary) == "" {
		return fmt.Errorf("pending reservation and terminal outcome required")
	}
	stable := final
	stable.OutcomePending, stable.Outcome, stable.Changed, stable.Summary = pending.OutcomePending, pending.Outcome, pending.Changed, pending.Summary
	stable.ChangeKnowledge = pending.ChangeKnowledge
	stable.AfterRefs, stable.FilesChanged, stable.FailureCause = pending.AfterRefs, pending.FilesChanged, pending.FailureCause
	stable.ProofRefs = pending.ProofRefs
	for _, ref := range pending.ProofRefs {
		if !slices.Contains(final.ProofRefs, ref) {
			return fmt.Errorf("outcome must retain admission evidence")
		}
	}
	if !reflect.DeepEqual(stable, pending) {
		return fmt.Errorf("outcome seal cannot change action, authority, work admission or preimages")
	}
	return s.withLifecycleMutation(func() error {
		previous, err := newReceiptedActionReservation(workID, pending, authorizedAt)
		if err != nil {
			return err
		}
		next, err := newReceiptedActionReservation(workID, final, authorizedAt)
		if err != nil {
			return err
		}
		actual, present, path, err := s.preflightReceiptedActionReservation(workID)
		if err != nil {
			return err
		}
		if !present {
			return fmt.Errorf("no pending reservation to seal")
		}
		if receiptedActionReservationEqual(actual, next) {
			return nil
		}
		if !receiptedActionReservationEqual(actual, previous) {
			return fmt.Errorf("pending outcome reservation changed")
		}
		if _, published, _, err := s.preflightReceiptedActionPending(workID); err != nil {
			return err
		} else if published {
			return fmt.Errorf("receipt publication already began")
		}
		if publishedAction, published, err := s.preflightReceiptedAction(final.Action); err != nil {
			return err
		} else if published {
			if final.PreEvaluatedDecision == nil || !reflect.DeepEqual(publishedAction, final.Action) ||
				policy.NewExactActionApprovalStore(s.home).ValidateConsumedDecision(final.Action, *final.PreEvaluatedDecision) != nil {
				return fmt.Errorf("action already published")
			}
		}
		w, err := s.recoverPendingClaimBeforeMutation(workID)
		if err != nil {
			return err
		}
		if err := validateReceiptedActionReservationAdmission(s, w, workID, final, authorizedAt); err != nil {
			return err
		}
		return s.writeReceiptedActionBoundary("reservation_outcome", path, &next)
	})
}
