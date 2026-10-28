package work

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"slices"

	"compound/internal/envelope"
)

func (s *Store) validateInstalledPackageDeliveryEvidence(w envelope.WorkEnvelope, original envelope.ActionIntent, ref string) error {
	reader, err := s.privateDeliveryArtifactReader()
	if err != nil {
		return err
	}
	body, err := reader.GetPrivateBounded(ref, privateDeliveryMetadataLimit)
	if err != nil {
		return err
	}
	var evidence installedPackageEvidence
	if err = decodeDeliveryCapture(body, &evidence); err != nil {
		return err
	}
	if evidence.SchemaVersion != installedPackageDeliverySchema || evidence.WorkID != w.ID || evidence.ActionIntentID != original.ID || evidence.Snapshot.TargetPath != original.Target.Path || original.IDs.WorkID != w.ID || original.MutationClass == "read" || !slices.Contains(w.ActionIntentIDs, original.ID) {
		return fmt.Errorf("package evidence differs from exact owned effect")
	}
	record, err := s.events.Read(original.ID)
	if err != nil {
		return err
	}
	if evidence.ActionSHA256 != fmt.Sprintf("%x", sha256.Sum256(record.Bytes)) {
		return fmt.Errorf("package original action bytes changed")
	}
	consumer, err := s.validateDeliveryOutcomeEvidence(w, evidence.Consumer)
	if err != nil {
		return err
	}
	if consumer.Action.Capability != "work.package-delivery" || consumer.Action.Action != "capture_installed_package_readback" || consumer.Action.MutationClass != "read" || consumer.Action.Target != original.Target || consumer.Receipt.StateDelta.Changed || consumer.Receipt.MoveType != "audit_only" {
		return fmt.Errorf("package evidence lacks owner readback consumer")
	}
	reportBody, err := reader.GetPrivateBounded(evidence.Consumer.ReportRef, privateDeliveryReportLimit)
	if err != nil {
		return err
	}
	var report installedPackageReport
	if err = decodeDeliveryCapture(reportBody, &report); err != nil {
		return err
	}
	if report.SchemaVersion != "compound.installed_package_readback.v0.1" || report.Snapshot != evidence.Snapshot {
		return fmt.Errorf("package report differs from captured snapshot")
	}
	requestBody, err := reader.GetPrivateBounded(report.RequestRef, privateDeliveryMetadataLimit)
	if err != nil {
		return err
	}
	var request InstalledPackageDeliveryCaptureRequest
	if err = decodeDeliveryCapture(requestBody, &request); err != nil {
		return err
	}
	if request.SchemaVersion != InstalledPackageDeliveryCaptureSchema || request.ActionIntentID != original.ID || request.Manager != evidence.Snapshot.Manager || request.Prefix != evidence.Snapshot.Prefix || request.Package != evidence.Snapshot.Package || request.Version != evidence.Snapshot.Version || consumer.Action.ID != idFor("act_", "installed-package-readback", w.ID, request.CaptureID) {
		return fmt.Errorf("package request differs from owner readback")
	}
	refs := []string{original.ID, report.RequestRef, evidence.Snapshot.ReceiptRef, evidence.Consumer.ReportRef}
	if !reflect.DeepEqual(consumer.Action.EvidenceRefs, refs) || !reflect.DeepEqual(consumer.Receipt.ProofRefs, mergeUnique(refs, []string{"work:" + w.ID})) || !slices.Contains(consumer.Transition.AfterRefs, evidence.Consumer.ReportRef) || !slices.Contains(consumer.Transition.AfterRefs, evidence.Snapshot.ReceiptRef) {
		return fmt.Errorf("package outcome does not consume exact observed evidence")
	}
	if _, err = reader.GetPrivateBounded(evidence.Snapshot.ReceiptRef, privateDeliveryMetadataLimit); err != nil {
		return err
	}
	current, _, err := readInstalledPackageSnapshot(request, original.Target.Path)
	if err != nil {
		return err
	}
	if current != evidence.Snapshot {
		return fmt.Errorf("installed package or owner receipt changed after readback")
	}
	return nil
}
