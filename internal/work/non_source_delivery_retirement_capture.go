package work

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"compound/internal/envelope"
	homepkg "compound/internal/home"
)

// CaptureWorktreeRetirementDelivery reads existing Authority output and the
// authentic completed Work chain. It never reapplies retirement or binds work.
func (s *Store) CaptureWorktreeRetirementDelivery(workID string, request WorktreeRetirementDeliveryCaptureRequest, actor string, now time.Time) (PrivateRuntimeDeliveryCaptureResult, error) {
	out := PrivateRuntimeDeliveryCaptureResult{Status: "capture_incomplete"}
	if s.readOnly {
		return out, fmt.Errorf("retirement capture requires writable work store")
	}
	if request.SchemaVersion != WorktreeRetirementDeliveryCaptureSchema || request.CaptureID == "" || len(request.CaptureID) > 64 || strings.Trim(request.CaptureID, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") != "" ||
		request.ExpectedWorkState != "running" || request.ExpectedPolicyDecisionID == "" || request.ExpectedActionIntentID == "" || actor == "" || strings.TrimSpace(actor) != actor {
		return out, fmt.Errorf("retirement capture requires exact identity, running generation and actor")
	}
	w, err := s.Get(workID)
	if err != nil {
		return out, err
	}
	if err := w.Validate(); err != nil {
		return out, err
	}
	if w.State != "running" {
		return out, fmt.Errorf("retirement capture requires running work; no lifecycle transition is implicit")
	}
	snapshot, err := readRetirementDeliverySnapshot(request)
	if err != nil {
		return out, err
	}
	manifest := snapshot.Evidence
	result := snapshot.Delivery.Result
	var original envelope.ActionIntent
	if err := s.readEventJSON(result.ActionIntentID, &original); err != nil {
		return out, err
	}
	if err := original.Validate(); err != nil {
		return out, err
	}
	record, err := s.events.Read(original.ID)
	if err != nil {
		return out, err
	}
	manifest.ActionSHA256 = fmt.Sprintf("%x", sha256.Sum256(record.Bytes))
	manifest.Outcome, err = s.captureDeliveryOutcome(w, deliveryOutcomeEvidence{ActionID: result.ActionIntentID, PolicyDecisionID: result.PolicyDecisionID, ReceiptID: result.ReceiptID, TransitionID: result.TransitionID, ReportRef: manifest.ResultRef})
	if err != nil {
		return out, err
	}
	if err := s.validateRetirementDeliveryOutcome(w, original, manifest, snapshot.Delivery); err != nil {
		return out, err
	}
	if request.ActionIntentID != "" && request.ActionIntentID != original.ID {
		failed, err := s.captureRetirementFailure(w, request.ActionIntentID)
		if err != nil {
			return out, err
		}
		manifest.FailedOutcome = &failed
		var selected envelope.ActionIntent
		if err := s.readEventJSON(request.ActionIntentID, &selected); err != nil {
			return out, err
		}
		if err := s.validateRetirementFailureDelivery(w, selected, manifest, snapshot.Delivery); err != nil {
			return out, err
		}
	}

	requestBody, err := json.Marshal(request)
	if err != nil || len(requestBody) > privateDeliveryMetadataLimit {
		return out, fmt.Errorf("retirement request exceeds metadata contract: %v", err)
	}
	reportBody, err := json.Marshal(struct {
		SchemaVersion string                     `json:"schema_version"`
		RequestRef    string                     `json:"request_ref"`
		Evidence      retirementDeliveryEvidence `json:"evidence"`
		Observation   string                     `json:"observation"`
	}{"compound.retirement_delivery_readback.v0.1", privateDeliveryArtifactRef(requestBody), manifest, "Original retirement records, completed Work chain and recovery bytes verified and custodied; no Git operation or task acceptance"})
	if err != nil || len(reportBody) > privateDeliveryReportLimit {
		return out, fmt.Errorf("retirement report exceeds metadata contract: %v", err)
	}
	requestRef, reportRef := privateDeliveryArtifactRef(requestBody), privateDeliveryArtifactRef(reportBody)
	snapshot.Bodies[requestRef], snapshot.Bodies[reportRef] = requestBody, reportBody
	refs := make([]string, 0, len(snapshot.Bodies))
	for ref := range snapshot.Bodies {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	proofRefs := append([]string{original.ID}, refs...)
	action := envelope.ActionIntent{
		Header: newHeader(envelope.SVActionIntent, idFor("act_", "retirement-delivery-readback", workID, request.CaptureID), now, workID, w.IDs.ContextPackID, w.Privacy, w.Risk),
		Actor:  actor, Capability: "work.retirement-delivery", Action: "capture_retirement_delivery", Target: original.Target,
		MutationClass: "read", Reversibility: "easy", BlastRadius: "local", Preview: "Custody exact prior retirement evidence without repeating Git effects",
		RollbackPlan: "Retain immutable evidence; no source or retired surface is changed",
	}
	action.EvidenceRefs = proofRefs
	input := ReceiptedActionInput{Action: action, ExpectedWorkState: "running", Outcome: "completed", Changed: false, MoveType: "audit_only",
		Summary: "Original retirement proof and recovery bytes verified and custodied; no Git operation or task acceptance", ProofRefs: proofRefs, AfterRefs: refs}
	out, consumer, err := s.captureDeliveryBodies(w, input, snapshot.Bodies, deliveryCaptureGeneration{request.ExpectedWorkState, request.ExpectedPolicyDecisionID, request.ExpectedActionIntentID}, now)
	if err != nil {
		return out, err
	}
	body, err := json.Marshal(manifest)
	if err != nil || len(body) > privateDeliveryMetadataLimit {
		return out, fmt.Errorf("retirement manifest exceeds metadata contract: %v", err)
	}
	artifacts, err := homepkg.NewArtifactStore(s.home)
	if err != nil {
		return out, err
	}
	ref, err := artifacts.PutPrivate(body)
	if err != nil {
		return out, err
	}
	var selected envelope.ActionIntent
	if manifest.FailedOutcome != nil {
		if err := s.readEventJSON(manifest.FailedOutcome.ActionID, &selected); err != nil {
			return out, err
		}
	} else {
		selected = original
	}
	out.Delivery = ActionDeliveryRequest{ActionIntentID: selected.ID, Kind: envelope.DeliveryWorktreeRetirement, EvidenceRef: ref}
	out.CustodiedRefs = append(out.CustodiedRefs, ref)
	if privateDeliveryCaptureAfterCustody != nil {
		if err := privateDeliveryCaptureAfterCustody("manifest"); err != nil {
			return out, err
		}
	}
	if err := s.validateRetirementDeliveryEvidence(consumer.Work, selected, ref); err != nil {
		return out, err
	}
	out.Status = "captured_not_bound"
	return out, nil
}

func (s *Store) captureDeliveryOutcome(w envelope.WorkEnvelope, proof deliveryOutcomeEvidence) (deliveryOutcomeEvidence, error) {
	return s.captureDeliveryOutcomeForStatus(w, proof, "completed")
}

func (s *Store) captureDeliveryOutcomeForStatus(w envelope.WorkEnvelope, proof deliveryOutcomeEvidence, expectedOutcome string) (deliveryOutcomeEvidence, error) {
	var action envelope.ActionIntent
	var decision envelope.PolicyDecision
	var receipt envelope.Receipt
	var transition envelope.Transition
	for _, item := range []struct {
		id     string
		target interface{ Validate() error }
		digest *string
	}{
		{proof.ActionID, &action, &proof.ActionSHA256}, {proof.PolicyDecisionID, &decision, &proof.PolicySHA256},
		{proof.ReceiptID, &receipt, &proof.ReceiptSHA256}, {proof.TransitionID, &transition, &proof.TransitionSHA256},
	} {
		if err := s.readEventJSON(item.id, item.target); err != nil {
			return proof, err
		}
		if err := item.target.Validate(); err != nil {
			return proof, err
		}
		var err error
		*item.digest, err = exactReceiptedActionDigest(item.target)
		if err != nil {
			return proof, err
		}
	}
	_, events, err := s.Show(w.ID)
	if err != nil {
		return proof, err
	}
	event, err := completedReceiptedActionEvent(events, action, decision, receipt, transition)
	if err != nil {
		return proof, err
	}
	proof.EventSHA256, err = exactReceiptedActionDigest(event)
	if err != nil {
		return proof, err
	}
	_, err = s.validateDeliveryOutcomeForStatus(w, proof, expectedOutcome)
	return proof, err
}
