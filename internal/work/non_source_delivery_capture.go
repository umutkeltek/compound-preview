package work

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	homepkg "compound/internal/home"
)

// Failure injection stays at custody/publication boundaries; production leaves it nil.
var privateDeliveryCaptureAfterCustody func(string) error

// CapturePrivateRuntimeDeliveryFile accepts one bounded, regular JSON request.
// Invalid input cannot enter the mutating capture path.
func (s *Store) CapturePrivateRuntimeDeliveryFile(workID, requestPath, actor string, now time.Time) (PrivateRuntimeDeliveryCaptureResult, error) {
	out := PrivateRuntimeDeliveryCaptureResult{Status: "capture_incomplete"}
	body, err := readDeliveryCaptureFile(requestPath, privateDeliveryMetadataLimit)
	if err != nil {
		return out, err
	}
	var request PrivateRuntimeDeliveryCaptureRequest
	if err := decodeDeliveryCapture(body, &request); err != nil {
		return out, err
	}
	return s.CapturePrivateRuntimeDelivery(workID, request, actor, now)
}

// CapturePrivateRuntimeDelivery observes exact bytes and records a readback
// transaction using the existing receipted-action owner. It does not bind delivery,
// certify previous tests, change lifecycle state or accept the work.
func (s *Store) CapturePrivateRuntimeDelivery(workID string, request PrivateRuntimeDeliveryCaptureRequest, actor string, now time.Time) (PrivateRuntimeDeliveryCaptureResult, error) {
	out := PrivateRuntimeDeliveryCaptureResult{Status: "capture_incomplete"}
	if s.readOnly {
		return out, fmt.Errorf("private delivery capture requires writable work store")
	}
	if request.SchemaVersion != PrivateRuntimeDeliveryCaptureSchema || request.CaptureID == "" || len(request.CaptureID) > 64 || strings.Trim(request.CaptureID, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") != "" {
		return out, fmt.Errorf("invalid private delivery capture schema or identity")
	}
	if request.ExpectedWorkState != "running" || request.ExpectedPolicyDecisionID == "" || request.ExpectedActionIntentID == "" || actor == "" || strings.TrimSpace(actor) != actor {
		return out, fmt.Errorf("capture requires exact running work generation and actor")
	}
	w, err := s.Get(workID)
	if err != nil {
		return out, err
	}
	if err := w.Validate(); err != nil {
		return out, err
	}
	if w.State != "running" {
		return out, fmt.Errorf("private delivery capture requires running work; no lifecycle transition is implicit")
	}
	if !slices.Contains(w.ActionIntentIDs, request.ActionIntentID) {
		return out, fmt.Errorf("capture original action is not owned by work")
	}
	var original envelope.ActionIntent
	if err := s.readEventJSON(request.ActionIntentID, &original); err != nil {
		return out, err
	}
	if err := original.Validate(); err != nil {
		return out, err
	}
	if original.ID != request.ActionIntentID || original.IDs.WorkID != workID || original.MutationClass == "read" {
		return out, fmt.Errorf("capture requires exact owned mutation")
	}
	refBytes, err := hex.DecodeString(request.RepositoryRef)
	if err != nil || (len(refBytes) != 20 && len(refBytes) != 32) || hex.EncodeToString(refBytes) != request.RepositoryRef {
		return out, fmt.Errorf("capture repository revision must be exact full commit")
	}
	identity, err := gitrepository.ReadIdentity(request.RepositoryRoot, request.RepositoryRef)
	if err != nil {
		return out, err
	}
	entries, bodies, err := capturePrivateDeliverySources(request, original.Target.Path)
	if err != nil {
		return out, err
	}
	originalRecord, err := s.events.Read(original.ID)
	if err != nil {
		return out, err
	}
	manifest := privateRuntimeDeliveryEvidence{SchemaVersion: privateRuntimeDeliveryEvidenceSchema,
		WorkID: workID, ActionIntentID: original.ID, ActionSHA256: fmt.Sprintf("%x", sha256.Sum256(originalRecord.Bytes)),
		RepositoryRoot: request.RepositoryRoot, RepositoryRef: request.RepositoryRef, RepositoryFingerprint: identity.Fingerprint,
		TargetPath: original.Target.Path, Files: entries}
	requestBody, err := json.Marshal(request)
	if err != nil || len(requestBody) > privateDeliveryMetadataLimit {
		return out, fmt.Errorf("capture request exceeds metadata contract: %v", err)
	}
	requestRef := privateDeliveryArtifactRef(requestBody)
	reportBody, err := json.Marshal(struct {
		SchemaVersion         string                        `json:"schema_version"`
		WorkID                string                        `json:"work_id"`
		RequestRef            string                        `json:"request_ref"`
		OriginalActionSHA256  string                        `json:"original_action_sha256"`
		RepositoryFingerprint string                        `json:"repository_fingerprint"`
		Observation           string                        `json:"observation"`
		Files                 []privateRuntimeDeliveryEntry `json:"files"`
	}{"compound.private_runtime_readback.v0.1", workID, requestRef, manifest.ActionSHA256, identity.Fingerprint,
		"After-image and supplied preimage bytes read and matched their expected hashes. Prior absences are declarations, not new observations. No previous test result or task acceptance is asserted.", entries})
	if err != nil || len(reportBody) > privateDeliveryReportLimit {
		return out, fmt.Errorf("capture report exceeds metadata contract: %v", err)
	}
	reportRef := privateDeliveryArtifactRef(reportBody)
	bodies[requestRef], bodies[reportRef] = requestBody, reportBody
	proofRefs, beforeRefs, afterRefs := []string{original.ID, requestRef, reportRef}, []string{}, []string{reportRef}
	for _, entry := range entries {
		proofRefs = append(proofRefs, entry.AfterArtifact)
		afterRefs = append(afterRefs, entry.AfterArtifact)
		if entry.BeforeArtifact != "" {
			beforeRefs = append(beforeRefs, entry.BeforeArtifact)
		}
	}
	action := envelope.ActionIntent{
		Header: newHeader(envelope.SVActionIntent, idFor("act_", "private-runtime-readback", workID, request.CaptureID), now, workID, w.IDs.ContextPackID, w.Privacy, w.Risk),
		Actor:  actor, Capability: "work.private-runtime-delivery", Action: "capture_private_runtime_readback",
		Target: original.Target, MutationClass: "read", Reversibility: "easy", BlastRadius: "local",
		Preview:      "Read and custody exact private delivery evidence; no source, lifecycle or acceptance mutation",
		RollbackPlan: "Keep immutable evidence; no source files are changed",
	}
	action.EvidenceRefs = proofRefs
	input := ReceiptedActionInput{Action: action, ExpectedWorkState: "running",
		Outcome: "completed", Changed: false, MoveType: "audit_only", Summary: "Private delivery after-images and supplied preimages read, hash-compared and custodied; no task acceptance",
		ProofRefs: proofRefs, BeforeRefs: beforeRefs, AfterRefs: afterRefs}
	out, consumer, err := s.captureDeliveryBodies(w, input, bodies, deliveryCaptureGeneration{request.ExpectedWorkState, request.ExpectedPolicyDecisionID, request.ExpectedActionIntentID}, now)
	if err != nil {
		return out, err
	}
	artifacts, err := homepkg.NewArtifactStore(s.home)
	if err != nil {
		return out, err
	}
	_, events, err := s.Show(workID)
	if err != nil {
		return out, err
	}
	event, err := completedReceiptedActionEvent(events, consumer.Action, consumer.Decision, consumer.Receipt, consumer.Transition)
	if err != nil {
		return out, err
	}
	manifest.Consumer = deliveryOutcomeEvidence{ActionID: consumer.Action.ID, PolicyDecisionID: consumer.Decision.ID,
		ReceiptID: consumer.Receipt.ID, TransitionID: consumer.Transition.ID, ReportRef: reportRef}
	for _, item := range []struct {
		value  any
		target *string
	}{
		{consumer.Action, &manifest.Consumer.ActionSHA256}, {consumer.Decision, &manifest.Consumer.PolicySHA256},
		{consumer.Receipt, &manifest.Consumer.ReceiptSHA256}, {consumer.Transition, &manifest.Consumer.TransitionSHA256}, {event, &manifest.Consumer.EventSHA256},
	} {
		*item.target, err = exactReceiptedActionDigest(item.value)
		if err != nil {
			return out, err
		}
	}
	manifestBody, err := json.Marshal(manifest)
	if err != nil || len(manifestBody) > privateDeliveryMetadataLimit {
		return out, fmt.Errorf("capture manifest exceeds metadata contract: %v", err)
	}
	evidenceRef, err := artifacts.PutPrivate(manifestBody)
	if err != nil {
		return out, err
	}
	out.Delivery = ActionDeliveryRequest{ActionIntentID: original.ID, Kind: envelope.DeliveryPrivateRuntime, EvidenceRef: evidenceRef}
	out.CustodiedRefs = append(out.CustodiedRefs, evidenceRef)
	if privateDeliveryCaptureAfterCustody != nil {
		if err := privateDeliveryCaptureAfterCustody("manifest"); err != nil {
			return out, err
		}
	}
	if err := s.validatePrivateRuntimeDeliveryEvidence(consumer.Work, original, evidenceRef); err != nil {
		return out, err
	}
	out.Status = "captured_not_bound"
	return out, nil
}
