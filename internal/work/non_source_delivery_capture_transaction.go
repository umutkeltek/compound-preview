package work

import (
	"compound/internal/envelope"
	homepkg "compound/internal/home"
	"compound/internal/policy"
	"fmt"
	"time"
)

type deliveryCaptureGeneration struct{ State, PolicyID, ActionID string }

// captureDeliveryBodies uses the existing receipted-action reservation, not a
// new journal. Only the typed capture producers construct its readback input.
func (s *Store) captureDeliveryBodies(w envelope.WorkEnvelope, input ReceiptedActionInput, bodies map[string][]byte, generation deliveryCaptureGeneration, now time.Time) (PrivateRuntimeDeliveryCaptureResult, ReceiptedActionResult, error) {
	out := PrivateRuntimeDeliveryCaptureResult{Status: "capture_incomplete"}
	var consumer ReceiptedActionResult
	var err error
	workID, action := w.ID, input.Action
	action, _, err = s.preflightReceiptedAction(action)
	if err != nil {
		return out, consumer, err
	}
	out.ActionID = action.ID
	cfg, _, err := policy.LoadConfig(s.home)
	if err != nil {
		return out, consumer, err
	}
	decision := policy.Evaluate(action, cfg, now)
	if err := validatePreEvaluatedReceiptedAction(s, w, action, decision, now); err != nil {
		return out, consumer, err
	}
	input.Action, input.PreEvaluatedDecision = action, &decision
	authorizedAt := now
	reserved, reservationAt, pending, err := s.ReadReceiptedActionReservationInput(workID, action.ID)
	if err != nil {
		return out, consumer, err
	}
	completion, completed, _, err := s.preflightReceiptedActionCompletion(workID, action.ID)
	if err != nil {
		return out, consumer, err
	}
	if pending {
		input.Action.CreatedAt = reserved.Action.CreatedAt
		input.PreEvaluatedDecision = reserved.PreEvaluatedDecision
		authorizedAt = reservationAt
		expected, err := newReceiptedActionReservation(workID, input, authorizedAt)
		if err != nil {
			return out, consumer, err
		}
		previous, err := newReceiptedActionReservation(workID, reserved, reservationAt)
		if err != nil || !receiptedActionReservationEqual(previous, expected) {
			return out, consumer, fmt.Errorf("capture retry conflicts with reserved transaction: %v", err)
		}
	} else if completed {
		var recordedDecision envelope.PolicyDecision
		if err := s.readEventJSON(completion.PolicyDecisionID, &recordedDecision); err != nil {
			return out, consumer, err
		}
		input.PreEvaluatedDecision = &recordedDecision
		authorizedAt, err = time.Parse(time.RFC3339Nano, completion.AuthorizedAt)
		if err != nil {
			return out, consumer, err
		}
	}
	if !completed && !pending {
		err = s.ReserveReceiptedAction(workID, input, authorizedAt, func() error {
			current, err := s.Get(workID)
			if err != nil {
				return err
			}
			if current.State != generation.State || current.PolicyDecisionID != generation.PolicyID || latestActionIntentID(current) != generation.ActionID {
				return fmt.Errorf("capture work generation changed")
			}
			return validatePreEvaluatedReceiptedAction(s, current, action, decision, now)
		})
		if err != nil {
			return out, consumer, err
		}
	} else if completed {
		expected, err := newReceiptedActionReservation(workID, input, authorizedAt)
		if err != nil || !receiptedActionReservationEqual(completion.reservation(), expected) {
			return out, consumer, fmt.Errorf("capture retry conflicts with completed transaction: %v", err)
		}
	}
	artifacts, err := homepkg.NewArtifactStore(s.home)
	if err != nil {
		return out, consumer, err
	}
	for ref, body := range bodies {
		stored, err := artifacts.PutPrivate(body)
		if err != nil {
			return out, consumer, err
		}
		if stored != ref {
			return out, consumer, fmt.Errorf("capture custody content address differs")
		}
		out.CustodiedRefs = append(out.CustodiedRefs, stored)
		if privateDeliveryCaptureAfterCustody != nil {
			if err := privateDeliveryCaptureAfterCustody("body"); err != nil {
				return out, consumer, err
			}
		}
	}
	consumer, err = s.RecordReservedReceiptedAction(workID, input, authorizedAt)
	if err != nil {
		// Diagnostic references only: an interrupted transaction is not a
		// completed consumer. Surface the records that actually made it to disk.
		if expected, identityErr := newReceiptedActionReservation(workID, input, authorizedAt); identityErr == nil {
			var receipt envelope.Receipt
			if readErr := s.readEventJSON(expected.ReceiptID, &receipt); readErr == nil && receipt.Validate() == nil && receipt.ID == expected.ReceiptID && receipt.IDs.WorkID == workID {
				out.ReceiptID = receipt.ID
			}
			var transition envelope.Transition
			if readErr := s.readEventJSON(expected.TransitionID, &transition); readErr == nil && transition.Validate() == nil && transition.ID == expected.TransitionID && transition.IDs.WorkID == workID {
				out.TransitionID = transition.ID
			}
		}
		return out, consumer, err
	}
	out.ReceiptID, out.TransitionID = consumer.Receipt.ID, consumer.Transition.ID
	return out, consumer, nil
}
