package sourceobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"compound/internal/envelope"
	"compound/internal/eventstore"
)

func SourceLifecycleGateEvidenceRef(sourceFamily, gate string) string {
	return "source-gate:" + url.PathEscape(strings.TrimSpace(sourceFamily)) + ":" + strings.TrimSpace(gate)
}

func bindSourceLifecycleEvidence(homePath string, manifest SourceManifest, proofs []SourceLifecycleProof) ([]SourceLifecycleProof, error) {
	out := append([]SourceLifecycleProof{}, proofs...)
	for i := range out {
		if !sourceLifecycleGateRequiresEnvelopeEvidence(out[i].Gate) {
			out[i].EvidenceBinding = nil
			continue
		}
		binding, err := resolveSourceLifecycleEvidenceBinding(homePath, manifest, out[i])
		if err != nil {
			return nil, err
		}
		out[i].EvidenceBinding = &binding
	}
	return out, nil
}

func resolveSourceLifecycleEvidenceBinding(homePath string, manifest SourceManifest, proof SourceLifecycleProof) (SourceLifecycleEvidenceBinding, error) {
	transitionID, ok := strings.CutPrefix(strings.TrimSpace(proof.EvidenceRef), "transition:")
	if !ok {
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("proof %s requires transition:<id> evidence", proof.Gate)
	}
	transitionID = strings.TrimSpace(transitionID)
	if transitionID == "" {
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("proof %s requires a transition evidence id", proof.Gate)
	}
	if !validLifecycleEnvelopeStorageID(transitionID, "tr_") {
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("proof %s has invalid evidence transition id %q", proof.Gate, transitionID)
	}
	store, err := eventstore.Open(homePath)
	if err != nil {
		return SourceLifecycleEvidenceBinding{}, err
	}
	transitionRecord, err := store.Read(transitionID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SourceLifecycleEvidenceBinding{}, fmt.Errorf("evidence transition %s not found", transitionID)
		}
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("read evidence transition %s: %w", transitionID, err)
	}
	transitionBody := transitionRecord.Bytes
	var transition envelope.Transition
	if err := json.Unmarshal(transitionBody, &transition); err != nil {
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("decode evidence transition %s: %w", transitionID, err)
	}
	if transition.ID != transitionID {
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("evidence transition id %q does not match ref %q", transition.ID, transitionID)
	}
	if err := transition.Validate(); err != nil {
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("evidence transition %s invalid: %w", transitionID, err)
	}

	receiptID := ""
	for _, ref := range transition.AfterRefs {
		if !strings.HasPrefix(strings.TrimSpace(ref), "rec_") {
			continue
		}
		if receiptID != "" {
			return SourceLifecycleEvidenceBinding{}, fmt.Errorf("evidence transition %s has multiple receipt refs", transitionID)
		}
		receiptID = strings.TrimSpace(ref)
	}
	if receiptID == "" {
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("evidence transition %s has no receipt ref", transitionID)
	}
	if !validLifecycleEnvelopeStorageID(receiptID, "rec_") {
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("evidence transition %s has invalid receipt ref %q", transitionID, receiptID)
	}
	receiptRecord, err := store.Read(receiptID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SourceLifecycleEvidenceBinding{}, fmt.Errorf("evidence receipt %s not found", receiptID)
		}
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("read evidence receipt %s: %w", receiptID, err)
	}
	receiptBody := receiptRecord.Bytes
	var receipt envelope.Receipt
	if err := json.Unmarshal(receiptBody, &receipt); err != nil {
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("decode evidence receipt %s: %w", receiptID, err)
	}
	if receipt.ID != receiptID {
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("evidence receipt id %q does not match ref %q", receipt.ID, receiptID)
	}
	if err := receipt.Validate(); err != nil {
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("evidence receipt %s invalid: %w", receiptID, err)
	}
	if receipt.IDs.WorkID != transition.IDs.WorkID {
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("evidence receipt work_id %q does not match transition work_id %q", receipt.IDs.WorkID, transition.IDs.WorkID)
	}
	if receipt.Outcome != "completed" || !receipt.EffectiveChanged() {
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("evidence receipt %s must be completed with an effective delta", receiptID)
	}

	workID := strings.TrimSpace(transition.IDs.WorkID)
	if !validLifecycleEnvelopeStorageID(workID, "work_") {
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("evidence transition %s has invalid work_id %q", transitionID, workID)
	}
	workPath := filepath.Join(homePath, "work", "work", workID+".json")
	workBody, err := os.ReadFile(workPath)
	if err != nil {
		if os.IsNotExist(err) {
			return SourceLifecycleEvidenceBinding{}, fmt.Errorf("evidence work %s not found", workID)
		}
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("read evidence work %s: %w", workID, err)
	}
	var evidenceWork envelope.WorkEnvelope
	if err := json.Unmarshal(workBody, &evidenceWork); err != nil {
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("decode evidence work %s: %w", workID, err)
	}
	if evidenceWork.ID != workID {
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("evidence work id %q does not match transition work_id %q", evidenceWork.ID, workID)
	}
	if err := evidenceWork.Validate(); err != nil {
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("evidence work %s invalid: %w", workID, err)
	}
	if evidenceWork.State != "completed" {
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("evidence work %s must be completed, got %s", workID, evidenceWork.State)
	}
	if evidenceWork.IDs.TransitionID != transitionID {
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("evidence work %s transition_id %q does not match %q", workID, evidenceWork.IDs.TransitionID, transitionID)
	}
	if strings.TrimSpace(evidenceWork.CanonicalSurface) != manifest.SourceFamily {
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("evidence work %s canonical_surface %q does not match source family %q", workID, evidenceWork.CanonicalSurface, manifest.SourceFamily)
	}

	requiredGateRef := SourceLifecycleGateEvidenceRef(manifest.SourceFamily, proof.Gate)
	if !receiptCarriesSourceLifecycleGate(receipt, requiredGateRef) {
		return SourceLifecycleEvidenceBinding{}, fmt.Errorf("evidence receipt %s does not carry exact gate proof %s", receiptID, requiredGateRef)
	}
	return SourceLifecycleEvidenceBinding{
		SchemaVersion:        SourceLifecycleEvidenceBindingSchemaVersion,
		EvidenceWorkID:       workID,
		EvidenceWorkSHA256:   lifecycleEvidenceSHA256(workBody),
		ReceiptID:            receiptID,
		ReceiptSHA256:        lifecycleEvidenceSHA256(receiptBody),
		TransitionID:         transitionID,
		TransitionSHA256:     lifecycleEvidenceSHA256(transitionBody),
		CanonicalSurface:     manifest.SourceFamily,
		RequiredGateProofRef: requiredGateRef,
	}, nil
}

func validateStoredSourceLifecycleEvidenceBindings(sourceFamily string, proofs []SourceLifecycleProof) error {
	for _, proof := range proofs {
		binding := proof.EvidenceBinding
		if binding == nil {
			continue
		}
		if !sourceLifecycleGateRequiresEnvelopeEvidence(proof.Gate) {
			return fmt.Errorf("proof %s carries evidence_binding outside a high-risk gate", proof.Gate)
		}
		if binding.SchemaVersion != SourceLifecycleEvidenceBindingSchemaVersion || strings.TrimSpace(binding.EvidenceWorkID) == "" || strings.TrimSpace(binding.ReceiptID) == "" || strings.TrimSpace(binding.TransitionID) == "" {
			return fmt.Errorf("proof %s has incomplete evidence_binding identity", proof.Gate)
		}
		if !validLifecycleEvidenceSHA256(binding.EvidenceWorkSHA256) || !validLifecycleEvidenceSHA256(binding.ReceiptSHA256) || !validLifecycleEvidenceSHA256(binding.TransitionSHA256) {
			return fmt.Errorf("proof %s has invalid evidence_binding hash", proof.Gate)
		}
		if binding.CanonicalSurface != sourceFamily || binding.RequiredGateProofRef != SourceLifecycleGateEvidenceRef(sourceFamily, proof.Gate) || proof.EvidenceRef != "transition:"+binding.TransitionID {
			return fmt.Errorf("proof %s evidence_binding does not match source/gate/ref", proof.Gate)
		}
	}
	return nil
}

func doctorSourceLifecycleEvidence(homePath string, manifest SourceManifest) (bound, missing, mismatched int) {
	if manifest.Lifecycle == nil {
		return 0, 0, 0
	}
	for _, transition := range manifest.Lifecycle.History {
		for _, proof := range transition.Proofs {
			if !sourceLifecycleGateRequiresEnvelopeEvidence(proof.Gate) {
				continue
			}
			if proof.EvidenceBinding == nil {
				missing++
				continue
			}
			resolved, err := resolveSourceLifecycleEvidenceBinding(homePath, manifest, proof)
			if err != nil || !reflect.DeepEqual(resolved, *proof.EvidenceBinding) {
				mismatched++
				continue
			}
			bound++
		}
	}
	return bound, missing, mismatched
}

func receiptCarriesSourceLifecycleGate(receipt envelope.Receipt, required string) bool {
	refs := append([]string{}, receipt.ProofRefs...)
	refs = append(refs, receipt.EvidenceRefs...)
	refs = append(refs, receipt.StateDelta.Artifacts...)
	refs = append(refs, receipt.StateDelta.Verification...)
	for _, ref := range refs {
		if strings.TrimSpace(ref) == required {
			return true
		}
	}
	return false
}

func lifecycleEvidenceSHA256(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validLifecycleEvidenceSHA256(value string) bool {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "sha256:") {
		return false
	}
	value = strings.TrimPrefix(value, "sha256:")
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validLifecycleEnvelopeStorageID(value, prefix string) bool {
	if value != strings.TrimSpace(value) || !strings.HasPrefix(value, prefix) || len(value) == len(prefix) {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func sourceLifecycleGateRequiresEnvelopeEvidence(gate string) bool {
	switch strings.TrimSpace(gate) {
	case SourceLifecycleGateIsolatedRestore,
		SourceLifecycleGateSemanticParity,
		SourceLifecycleGateRealConsumerReplay,
		SourceLifecycleGateLegacyUnavailableNegativeControl,
		SourceLifecycleGateWriterRoster,
		SourceLifecycleGateWriterDestination,
		SourceLifecycleGateRollbackRehearsal,
		SourceLifecycleGatePrimaryWriterCutover,
		SourceLifecycleGateLegacyFreeze,
		SourceLifecycleGateQuietWindowMeasurement,
		SourceLifecycleGateArchiveVerified:
		return true
	default:
		return false
	}
}
