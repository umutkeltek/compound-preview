package work

import (
	"fmt"
	"slices"
	"strings"

	"compound/internal/envelope"
	"compound/internal/policy"
)

// Called under the work-event lock after ordinary pending-event validation.
// A durable same-state advance is not interchangeable with a binding request.
func (s *Store) validatePendingDeliveryRequest(event Event, current, pending envelope.WorkEnvelope, request *ActionDeliveryRequest) error {
	if request == nil {
		return nil
	}
	if event.From != event.To || !policy.HasOperatorProvenance(event.By) || len(event.Attached) < 2 {
		return fmt.Errorf("pending advance does not authorize the requested non-source delivery binding")
	}
	matched := 0
	for _, binding := range pending.NonSourceDelivery {
		if binding.ActionIntentID == request.ActionIntentID && binding.Kind == request.Kind && binding.EvidenceRef == request.EvidenceRef &&
			(binding.BindingActionIntentID == event.Attached[0] || slices.Contains(current.NonSourceDelivery, binding)) {
			matched++
		}
	}
	if matched != 1 || !slices.Contains(event.Attached, request.EvidenceRef) {
		return fmt.Errorf("pending advance conflicts with requested non-source delivery binding")
	}
	var action envelope.ActionIntent
	if err := s.readEventJSON(event.Attached[0], &action); err != nil {
		return err
	}
	if !slices.Contains(action.EvidenceRefs, request.ActionIntentID) || !slices.Contains(action.EvidenceRefs, request.EvidenceRef) {
		return fmt.Errorf("pending advance action lacks exact delivery evidence")
	}
	return nil
}

func (s *Store) preflightActionDeliveryBinding(w envelope.WorkEnvelope, actor, to string, request *ActionDeliveryRequest) (*envelope.ActionDeliveryBinding, error) {
	if request == nil {
		return nil, nil
	}
	if !policy.HasOperatorProvenance(actor) {
		return nil, fmt.Errorf("non-source delivery binding requires explicit operator provenance")
	}
	if terminalStates[w.State] || to != w.State {
		return nil, fmt.Errorf("non-source delivery binding requires a nonterminal same-state advance")
	}
	if request.Kind != envelope.DeliveryPrivateRuntime && request.Kind != envelope.DeliveryWorktreeRetirement && request.Kind != envelope.DeliveryInstalledPackage {
		return nil, fmt.Errorf("unsupported non-source delivery evidence kind %q", request.Kind)
	}
	if !slices.Contains(w.ActionIntentIDs, request.ActionIntentID) {
		return nil, fmt.Errorf("non-source delivery original action is not owned by this work")
	}
	for _, bound := range w.NonSourceDelivery {
		if bound.ActionIntentID != request.ActionIntentID {
			continue
		}
		if bound.Kind != request.Kind || bound.EvidenceRef != request.EvidenceRef {
			return nil, fmt.Errorf("non-source delivery binding is immutable")
		}
		var original envelope.ActionIntent
		if err := s.readEventJSON(request.ActionIntentID, &original); err != nil {
			return nil, err
		}
		if _, err := s.nonSourceDeliveryCovers(w, original); err != nil {
			return nil, err
		}
		return nil, nil
	}
	var action envelope.ActionIntent
	if err := s.readEventJSON(request.ActionIntentID, &action); err != nil {
		return nil, err
	}
	if err := action.Validate(); err != nil {
		return nil, err
	}
	if action.ID != request.ActionIntentID || action.IDs.WorkID != w.ID || action.MutationClass == "read" {
		return nil, fmt.Errorf("non-source delivery requires the exact owned mutation")
	}
	if err := s.validateNonSourceDeliveryEvidence(w, action, request.Kind, request.EvidenceRef); err != nil {
		return nil, err
	}
	return &envelope.ActionDeliveryBinding{ActionIntentID: request.ActionIntentID, Kind: request.Kind, EvidenceRef: request.EvidenceRef}, nil
}

func (s *Store) nonSourceDeliveryCovers(w envelope.WorkEnvelope, original envelope.ActionIntent) (bool, error) {
	var binding *envelope.ActionDeliveryBinding
	for i := range w.NonSourceDelivery {
		if w.NonSourceDelivery[i].ActionIntentID != original.ID {
			continue
		}
		if binding != nil {
			return false, fmt.Errorf("duplicate non-source delivery binding")
		}
		binding = &w.NonSourceDelivery[i]
	}
	if binding == nil {
		return false, nil
	}
	if binding.Kind != envelope.DeliveryPrivateRuntime && binding.Kind != envelope.DeliveryWorktreeRetirement && binding.Kind != envelope.DeliveryInstalledPackage {
		return false, fmt.Errorf("unsupported non-source delivery binding kind %q", binding.Kind)
	}
	var action envelope.ActionIntent
	if err := s.readEventJSON(binding.BindingActionIntentID, &action); err != nil {
		return false, err
	}
	if err := action.Validate(); err != nil {
		return false, err
	}
	if action.ID != binding.BindingActionIntentID || action.IDs.WorkID != w.ID || !slices.Contains(w.ActionIntentIDs, action.ID) || !policy.HasOperatorProvenance(action.Actor) || action.Capability != "work.advance" || action.Target.ID != w.ID || action.Target.Path != s.workPath(w.ID) || !slices.Contains(action.EvidenceRefs, binding.ActionIntentID) || !slices.Contains(action.EvidenceRefs, binding.EvidenceRef) {
		return false, fmt.Errorf("non-source delivery lacks exact operator binding authority")
	}
	_, events, err := s.Show(w.ID)
	if err != nil {
		return false, err
	}
	matched := 0
	for _, event := range events {
		if !slices.Contains(event.Attached, action.ID) {
			continue
		}
		if event.WorkID != w.ID || event.From != event.To || event.By != action.Actor || !slices.Contains(event.Attached, binding.EvidenceRef) {
			return false, fmt.Errorf("non-source delivery binding lifecycle does not match")
		}
		decisions := 0
		for _, ref := range event.Attached {
			if !strings.HasPrefix(ref, "pol_") {
				continue
			}
			var decision envelope.PolicyDecision
			if err := s.readEventJSON(ref, &decision); err != nil {
				return false, err
			}
			if err := decision.Validate(); err != nil {
				return false, err
			}
			if decision.ID == ref && decision.IDs.WorkID == w.ID && decision.ActionIntentID == action.ID && policy.Allowed(decision) {
				decisions++
			}
		}
		if decisions != 1 {
			return false, fmt.Errorf("non-source delivery binding lacks its allowing decision")
		}
		matched++
	}
	if matched != 1 {
		return false, fmt.Errorf("non-source delivery binding lifecycle count is %d", matched)
	}
	if err := s.validateNonSourceDeliveryEvidence(w, original, binding.Kind, binding.EvidenceRef); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) validateNonSourceDeliveryEvidence(w envelope.WorkEnvelope, action envelope.ActionIntent, kind, ref string) error {
	switch kind {
	case envelope.DeliveryInstalledPackage:
		return s.validateInstalledPackageDeliveryEvidence(w, action, ref)
	case envelope.DeliveryPrivateRuntime:
		return s.validatePrivateRuntimeDeliveryEvidence(w, action, ref)
	case envelope.DeliveryWorktreeRetirement:
		return s.validateRetirementDeliveryEvidence(w, action, ref)
	default:
		return fmt.Errorf("unsupported non-source delivery kind %q", kind)
	}
}
