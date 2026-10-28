package work

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"slices"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	homepkg "compound/internal/home"
)

func (s *Store) privateDeliveryArtifactReader() (*homepkg.ArtifactReader, error) {
	canonical, err := filepath.EvalSymlinks(s.home)
	if err != nil {
		return nil, err
	}
	return homepkg.OpenArtifactReader(canonical)
}

func (s *Store) validatePrivateRuntimeDeliveryEvidence(w envelope.WorkEnvelope, action envelope.ActionIntent, ref string) error {
	reader, err := s.privateDeliveryArtifactReader()
	if err != nil {
		return err
	}
	body, err := reader.GetPrivateBounded(ref, privateDeliveryMetadataLimit)
	if err != nil {
		return fmt.Errorf("private delivery evidence custody: %w", err)
	}
	var evidence privateRuntimeDeliveryEvidence
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&evidence); err != nil {
		return fmt.Errorf("private delivery evidence: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("private delivery evidence has trailing content")
	}
	if evidence.SchemaVersion != privateRuntimeDeliveryEvidenceSchema || evidence.WorkID != w.ID || evidence.ActionIntentID != action.ID || evidence.TargetPath != action.Target.Path {
		return fmt.Errorf("private delivery evidence does not bind the exact work, action and target")
	}
	if action.MutationClass == "read" || !slices.Contains(w.ActionIntentIDs, action.ID) || action.IDs.WorkID != w.ID {
		return fmt.Errorf("private delivery must describe an owned mutation")
	}
	record, err := s.events.Read(action.ID)
	if err != nil {
		return err
	}
	if fmt.Sprintf("%x", sha256.Sum256(record.Bytes)) != evidence.ActionSHA256 {
		return fmt.Errorf("private delivery original action bytes changed")
	}
	decodedRef, err := hex.DecodeString(evidence.RepositoryRef)
	if err != nil || (len(decodedRef) != 20 && len(decodedRef) != 32) || hex.EncodeToString(decodedRef) != evidence.RepositoryRef {
		return fmt.Errorf("private delivery repository ref must be an exact commit identity")
	}
	identity, err := gitrepository.ReadIdentity(evidence.RepositoryRoot, evidence.RepositoryRef)
	if err != nil || identity.Fingerprint != evidence.RepositoryFingerprint {
		return fmt.Errorf("private delivery repository identity differs from bound evidence")
	}
	if len(evidence.Files) == 0 || len(evidence.Files) > maxPrivateRuntimeDeliveryFiles {
		return fmt.Errorf("private delivery file count outside bound")
	}
	files := make([]privateRuntimeDeliveryFile, 0, len(evidence.Files))
	afterRefs := make([]string, 0, len(evidence.Files))
	for _, entry := range evidence.Files {
		if entry.BeforeAbsent == (entry.BeforeArtifact != "") {
			return fmt.Errorf("private delivery needs exactly one before-image or prior absence")
		}
		if !entry.BeforeAbsent {
			if _, err := reader.GetPrivateBounded(entry.BeforeArtifact, privateDeliveryBodyLimit); err != nil {
				return fmt.Errorf("private delivery rollback body: %w", err)
			}
		}
		if _, err := reader.GetPrivateBounded(entry.AfterArtifact, privateDeliveryBodyLimit); err != nil {
			return fmt.Errorf("private delivery after-image custody: %w", err)
		}
		digest, err := homepkg.ArtifactHex(entry.AfterArtifact)
		if err != nil {
			return err
		}
		files = append(files, privateRuntimeDeliveryFile{Path: entry.Path, AfterSHA256: digest})
		afterRefs = append(afterRefs, entry.AfterArtifact)
	}
	if err := validatePrivateRuntimeDeliveryPaths(evidence.RepositoryRoot, evidence.TargetPath, files); err != nil {
		return err
	}
	consumer, err := s.validateDeliveryOutcomeEvidence(w, evidence.Consumer)
	if err != nil {
		return err
	}
	if consumer.Action.MutationClass != "read" || consumer.Action.Target.Path != action.Target.Path || consumer.Receipt.StateDelta.Changed {
		return fmt.Errorf("private delivery consumer must be a read-only check of the exact target")
	}
	for _, required := range append(afterRefs, action.ID) {
		if !slices.Contains(consumer.Action.EvidenceRefs, required) || !slices.Contains(consumer.Receipt.ProofRefs, required) {
			return fmt.Errorf("private delivery consumer did not bind the original action and every after-image")
		}
	}
	for _, required := range append(afterRefs, evidence.Consumer.ReportRef) {
		if !slices.Contains(consumer.Transition.AfterRefs, required) {
			return fmt.Errorf("private delivery consumer transition omits observed after-image or report")
		}
	}
	if !slices.Contains(consumer.Receipt.ProofRefs, evidence.Consumer.ReportRef) {
		return fmt.Errorf("private delivery consumed report is not in its receipt")
	}
	if _, err := reader.GetPrivateBounded(evidence.Consumer.ReportRef, privateDeliveryReportLimit); err != nil {
		return fmt.Errorf("private delivery consumed report custody: %w", err)
	}
	// Classification and body reads are separate operations. Revalidate the
	// relevant live footprint and logical identity before returning a proof.
	if err := validatePrivateRuntimeDeliveryPaths(evidence.RepositoryRoot, evidence.TargetPath, files); err != nil {
		return err
	}
	identity, err = gitrepository.ReadIdentity(evidence.RepositoryRoot, evidence.RepositoryRef)
	if err != nil || identity.Fingerprint != evidence.RepositoryFingerprint {
		return fmt.Errorf("private delivery repository changed during validation")
	}
	return nil
}

func (s *Store) validateDeliveryOutcomeEvidence(w envelope.WorkEnvelope, proof deliveryOutcomeEvidence) (ReceiptedActionResult, error) {
	return s.validateDeliveryOutcomeForStatus(w, proof, "completed")
}

// Only typed retirement failure classification calls this with "failed".
func (s *Store) validateDeliveryOutcomeForStatus(w envelope.WorkEnvelope, proof deliveryOutcomeEvidence, expectedOutcome string) (ReceiptedActionResult, error) {
	var out ReceiptedActionResult
	for _, item := range []struct {
		id, digest string
		value      interface{ Validate() error }
	}{
		{proof.ActionID, proof.ActionSHA256, &out.Action},
		{proof.PolicyDecisionID, proof.PolicySHA256, &out.Decision},
		{proof.ReceiptID, proof.ReceiptSHA256, &out.Receipt},
		{proof.TransitionID, proof.TransitionSHA256, &out.Transition},
	} {
		if err := s.readEventJSON(item.id, item.value); err != nil {
			return out, err
		}
		if err := item.value.Validate(); err != nil {
			return out, err
		}
		actual, err := exactReceiptedActionDigest(item.value)
		if err != nil || actual != item.digest {
			return out, fmt.Errorf("delivery outcome envelope %s differs from captured evidence", item.id)
		}
	}
	if out.Action.ID != proof.ActionID || out.Decision.ID != proof.PolicyDecisionID || out.Receipt.ID != proof.ReceiptID || out.Transition.ID != proof.TransitionID {
		return out, fmt.Errorf("delivery outcome envelope identity mismatch")
	}
	if out.Action.IDs.WorkID != w.ID || out.Decision.IDs.WorkID != w.ID || out.Receipt.IDs.WorkID != w.ID || out.Transition.IDs.WorkID != w.ID || !slices.Contains(w.ActionIntentIDs, out.Action.ID) {
		return out, fmt.Errorf("delivery outcome belongs to another work")
	}
	if out.Decision.ActionIntentID != out.Action.ID || (out.Decision.Outcome != "allow" && out.Decision.Outcome != "allow_with_obligations") || out.Receipt.Outcome != expectedOutcome {
		return out, fmt.Errorf("delivery outcome is not an allowed %s action", expectedOutcome)
	}
	if !reflect.DeepEqual(out.Transition.ActionRefs, []string{out.Action.ID}) ||
		!slices.Contains(out.Transition.AfterRefs, out.Receipt.ID) ||
		out.Transition.StateDelta != out.Receipt.StateDelta.Summary ||
		!reflect.DeepEqual(out.Receipt.EvidenceRefs, out.Receipt.ProofRefs) ||
		!reflect.DeepEqual(out.Transition.EvidenceRefs, out.Receipt.ProofRefs) ||
		out.Transition.SecondOrder.FutureBehaviorChanged != out.Receipt.StateDelta.Changed {
		return out, fmt.Errorf("delivery outcome receipt and transition do not form one consumption chain")
	}
	_, events, err := s.Show(w.ID)
	if err != nil {
		return out, err
	}
	event, err := completedReceiptedActionEvent(events, out.Action, out.Decision, out.Receipt, out.Transition)
	if err != nil {
		return out, err
	}
	digest, err := exactReceiptedActionDigest(event)
	if err != nil || digest != proof.EventSHA256 {
		return out, fmt.Errorf("delivery outcome lifecycle differs from captured evidence")
	}
	return out, nil
}
