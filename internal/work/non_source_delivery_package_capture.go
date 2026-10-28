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

func (s *Store) CaptureInstalledPackageDelivery(workID string, r InstalledPackageDeliveryCaptureRequest, actor string, now time.Time) (NonSourceDeliveryCaptureResult, error) {
	out := NonSourceDeliveryCaptureResult{Status: "capture_incomplete"}
	if s.readOnly || r.SchemaVersion != InstalledPackageDeliveryCaptureSchema || r.CaptureID == "" || len(r.CaptureID) > 64 || strings.Trim(r.CaptureID, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") != "" || actor == "" || strings.TrimSpace(actor) != actor || r.ExpectedWorkState != "running" || r.ExpectedPolicyDecisionID == "" || r.ExpectedActionIntentID == "" {
		return out, fmt.Errorf("package capture requires writable store, exact identity, actor and running generation")
	}
	w, err := s.Get(workID)
	if err != nil {
		return out, err
	}
	if err = w.Validate(); err != nil {
		return out, err
	}
	if w.State != "running" {
		return out, fmt.Errorf("package capture requires running work; no lifecycle transition is implicit")
	}
	var original envelope.ActionIntent
	if err = s.readEventJSON(r.ActionIntentID, &original); err != nil {
		return out, err
	}
	if err = original.Validate(); err != nil {
		return out, err
	}
	if original.ID != r.ActionIntentID || original.IDs.WorkID != workID || original.MutationClass == "read" || !slices.Contains(w.ActionIntentIDs, original.ID) {
		return out, fmt.Errorf("package capture requires exact owned mutation")
	}
	snapshot, receiptBody, err := readInstalledPackageSnapshot(r, original.Target.Path)
	if err != nil {
		return out, err
	}
	record, err := s.events.Read(original.ID)
	if err != nil {
		return out, err
	}
	manifest := installedPackageEvidence{SchemaVersion: installedPackageDeliverySchema, WorkID: workID, ActionIntentID: original.ID, ActionSHA256: fmt.Sprintf("%x", sha256.Sum256(record.Bytes)), Snapshot: snapshot}
	requestBody, err := json.Marshal(r)
	if err != nil {
		return out, err
	}
	requestRef := privateDeliveryArtifactRef(requestBody)
	reportBody, err := json.Marshal(installedPackageReport{SchemaVersion: "compound.installed_package_readback.v0.1", RequestRef: requestRef, Snapshot: snapshot})
	if err != nil {
		return out, err
	}
	reportRef := privateDeliveryArtifactRef(reportBody)
	refs := []string{original.ID, requestRef, snapshot.ReceiptRef, reportRef}
	action := envelope.ActionIntent{Header: newHeader(envelope.SVActionIntent, idFor("act_", "installed-package-readback", workID, r.CaptureID), now, workID, w.IDs.ContextPackID, w.Privacy, w.Risk), Actor: actor, Capability: "work.package-delivery", Action: "capture_installed_package_readback", Target: original.Target, MutationClass: "read", Reversibility: "easy", BlastRadius: "local", Preview: "Read owner installation receipt and hash the exact installed tree; no installer or source mutation", RollbackPlan: "Retain immutable read evidence; no installed files are changed"}
	action.EvidenceRefs = refs
	input := ReceiptedActionInput{Action: action, ExpectedWorkState: "running", Outcome: "completed", Changed: false, MoveType: "audit_only", Summary: "Installed package owner receipt and bounded tree read back; no install or task acceptance", ProofRefs: refs, AfterRefs: []string{reportRef, snapshot.ReceiptRef}}
	bodies := map[string][]byte{requestRef: requestBody, reportRef: reportBody, snapshot.ReceiptRef: receiptBody}
	out, consumer, err := s.captureDeliveryBodies(w, input, bodies, deliveryCaptureGeneration{r.ExpectedWorkState, r.ExpectedPolicyDecisionID, r.ExpectedActionIntentID}, now)
	if err != nil {
		return out, err
	}
	manifest.Consumer, err = s.captureDeliveryOutcome(consumer.Work, deliveryOutcomeEvidence{ActionID: consumer.Action.ID, PolicyDecisionID: consumer.Decision.ID, ReceiptID: consumer.Receipt.ID, TransitionID: consumer.Transition.ID, ReportRef: reportRef})
	if err != nil {
		return out, err
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return out, err
	}
	artifacts, err := homepkg.NewArtifactStore(s.home)
	if err != nil {
		return out, err
	}
	ref, err := artifacts.PutPrivate(body)
	if err != nil {
		return out, err
	}
	out.Delivery = ActionDeliveryRequest{ActionIntentID: original.ID, Kind: envelope.DeliveryInstalledPackage, EvidenceRef: ref}
	out.CustodiedRefs = append(out.CustodiedRefs, ref)
	if err = s.validateInstalledPackageDeliveryEvidence(consumer.Work, original, ref); err != nil {
		return out, err
	}
	out.Status = "captured_not_bound"
	return out, nil
}
