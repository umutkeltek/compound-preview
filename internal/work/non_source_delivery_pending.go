package work

import (
	"bytes"
	"crypto/sha256"
	"fmt"

	"compound/internal/envelope"
	homepkg "compound/internal/home"
)

func closeUsesNonSourceDelivery(p closePending) bool {
	return p.Receipt.Outcome == "completed" && p.Receipt.MoveType != "audit_only"
}

func (s *Store) captureCloseNonSourceEvidence(p *closePending) error {
	if !closeUsesNonSourceDelivery(*p) || len(p.SourceWork.NonSourceDelivery) == 0 {
		return nil
	}
	reader, err := s.privateDeliveryArtifactReader()
	if err != nil {
		return err
	}
	p.NonSourceEvidence = make(map[string][]byte, len(p.SourceWork.NonSourceDelivery))
	for _, binding := range p.SourceWork.NonSourceDelivery {
		body, err := reader.GetPrivateBounded(binding.EvidenceRef, privateDeliveryMetadataLimit)
		if err != nil {
			return err
		}
		p.NonSourceEvidence[binding.EvidenceRef] = body
	}
	return validateFrozenCloseNonSourceEvidence(*p)
}

// The frame holds exact immutable metadata, so recovery can finish a transaction
// that already published terminal authority without reinterpreting later edits.
func validateFrozenCloseNonSourceEvidence(p closePending) error {
	expected := map[string]bool{}
	if closeUsesNonSourceDelivery(p) {
		for _, binding := range p.SourceWork.NonSourceDelivery {
			expected[binding.EvidenceRef] = true
		}
	}
	if len(expected) != len(p.NonSourceEvidence) {
		return fmt.Errorf("frozen non-source evidence inventory differs from source work")
	}
	for ref := range expected {
		body, found := p.NonSourceEvidence[ref]
		digest, err := homepkg.ArtifactHex(ref)
		if err != nil || !found || len(body) == 0 || len(body) > 1<<20 || fmt.Sprintf("%x", sha256.Sum256(body)) != digest {
			return fmt.Errorf("frozen non-source evidence does not match its content address")
		}
	}
	return nil
}

func (s *Store) revalidateCloseNonSourceEvidence(p closePending) error {
	if err := validateFrozenCloseNonSourceEvidence(p); err != nil {
		return err
	}
	if len(p.NonSourceEvidence) == 0 {
		return nil
	}
	reader, err := s.privateDeliveryArtifactReader()
	if err != nil {
		return err
	}
	for ref, frozen := range p.NonSourceEvidence {
		current, err := reader.GetPrivateBounded(ref, privateDeliveryMetadataLimit)
		if err != nil || !bytes.Equal(current, frozen) {
			return fmt.Errorf("non-source evidence custody changed during close")
		}
	}
	for _, binding := range p.SourceWork.NonSourceDelivery {
		var original envelope.ActionIntent
		if err := s.readEventJSON(binding.ActionIntentID, &original); err != nil {
			return err
		}
		covered, err := s.nonSourceDeliveryCovers(p.SourceWork, original)
		if err != nil {
			return err
		}
		if !covered {
			return fmt.Errorf("frozen non-source delivery is no longer covered")
		}
	}
	return nil
}
