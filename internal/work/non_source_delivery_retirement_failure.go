package work

import (
	"fmt"
	"reflect"
	"slices"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
)

// A failure is classified separately from the completed witness. This proves
// the same sealed operation, not cryptographic ancestry of overwritten results.
func (s *Store) captureRetirementFailure(w envelope.WorkEnvelope, actionID string) (deliveryOutcomeEvidence, error) {
	_, events, err := s.Show(w.ID)
	if err != nil {
		return deliveryOutcomeEvidence{}, err
	}
	var proof deliveryOutcomeEvidence
	matches := 0
	for _, event := range events {
		if len(event.Attached) != 4 || event.Attached[0] != actionID {
			continue
		}
		matches++
		proof = deliveryOutcomeEvidence{
			ActionID: actionID, PolicyDecisionID: event.Attached[1],
			ReceiptID: event.Attached[2], TransitionID: event.Attached[3],
		}
	}
	if matches != 1 {
		return proof, fmt.Errorf("retirement failed action must have one exact owned outcome")
	}
	return s.captureDeliveryOutcomeForStatus(w, proof, "failed")
}

func (s *Store) validateRetirementFailureDelivery(w envelope.WorkEnvelope, selected envelope.ActionIntent, evidence retirementDeliveryEvidence, delivery gitrepository.RetirementDelivery) error {
	if evidence.FailedOutcome == nil || evidence.FailedOutcome.ActionID != selected.ID ||
		selected.ID == evidence.Outcome.ActionID || evidence.FailedOutcome.ReportRef != "" {
		return fmt.Errorf("retirement failure must name one distinct owned action without invented result bytes")
	}
	if err := validateRetirementDeliveryAction(selected, delivery); err != nil {
		return err
	}
	failed, err := s.validateDeliveryOutcomeForStatus(w, *evidence.FailedOutcome, "failed")
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(failed.Action, selected) || failed.Receipt.MoveType != "control_plane_repair" ||
		failed.Receipt.FailureCause == nil || failed.Receipt.FailureCause.Mechanism == "" {
		return fmt.Errorf("retirement failure does not record its repair outcome")
	}
	surface := "failed"
	if failed.Receipt.StateDelta.Changed {
		surface = "partial"
	}
	if failed.Receipt.FailureCause.SurfaceOutcome != surface {
		return fmt.Errorf("retirement failure effect contradicts its surface outcome")
	}
	wantRefs := mergeUnique(gitrepository.WorktreeRetirementApplyProofRefs(delivery.Plan, delivery.Authorization),
		selected.EvidenceRefs, []string{"work:" + w.ID})
	// A later session may legitimately attach a different context to the Work.
	// The historical transaction owns its own context, not today's envelope.
	if failed.Receipt.IDs.ContextPackID != failed.Transition.IDs.ContextPackID ||
		!reflect.DeepEqual(failed.Receipt.ProofRefs, wantRefs) ||
		!slices.Equal(failed.Transition.BeforeRefs, mergeUnique([]string{failed.Receipt.IDs.ContextPackID})) ||
		!reflect.DeepEqual(failed.Transition.AfterRefs, []string{failed.Receipt.ID}) {
		return fmt.Errorf("retirement failure proof inventory differs from producer")
	}
	_, events, err := s.Show(w.ID)
	if err != nil {
		return err
	}
	failedIndex, completedIndex := -1, -1
	for i, event := range events {
		if slices.Contains(event.Attached, failed.Receipt.ID) {
			failedIndex = i
		}
		if slices.Contains(event.Attached, evidence.Outcome.ReceiptID) {
			completedIndex = i
		}
	}
	if failedIndex < 0 || completedIndex <= failedIndex {
		return fmt.Errorf("retirement failure must precede completed witness in owned lifecycle")
	}
	return nil
}
