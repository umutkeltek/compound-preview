package work

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/landlease"
)

func proofMigrationHistoryRef(body []byte) string {
	return fmt.Sprintf("%s%x", home.ArtifactRefPrefix, sha256.Sum256(body))
}

// Validation is read-only. A completed Work can have later actions than the
// migration snapshot; its exact migration action and completion must survive.
func (s *Store) validateProofMigrationHistory(prior resourceMigrationPending, successor envelope.WorkEnvelope, in ResourceMigrationInput, backing string, requireV2 bool) error {
	target, err := resourceProofMigrationRevision(in)
	if err != nil {
		return err
	}
	oldTarget, err := resourceProofMigrationRevision(prior.Input)
	if err != nil {
		return err
	}
	if target != landlease.ProofDomainFencedV3 || oldTarget != landlease.ProofDomainLocalV2 ||
		prior.SchemaVersion != resourceMigrationPendingSchema || !prior.Input.ProofClasses ||
		prior.PredecessorPreparationRef != "" || prior.Work.ID == successor.ID ||
		prior.BackingRoot != backing || !reflect.DeepEqual(prior.Input.ProofHomes, in.ProofHomes) {
		return fmt.Errorf("proof migration history is not a distinct completed v2 predecessor")
	}
	if err := s.validateResourceMigrationPreparation(prior); err != nil {
		return err
	}
	body, err := s.orphanPredecessorWorkBody(prior.Work.ID)
	if err != nil {
		return err
	}
	var old envelope.WorkEnvelope
	if err := json.Unmarshal(body, &old); err != nil {
		return err
	}
	if err := old.Validate(); err != nil {
		return err
	}
	prefix := append(append([]string(nil), prior.Work.ActionIntentIDs...), prior.Action.ID)
	if old.ID != prior.Work.ID || old.State != "completed" || len(old.ActionIntentIDs) < len(prefix) || !slices.Equal(old.ActionIntentIDs[:len(prefix)], prefix) {
		return fmt.Errorf("proof migration predecessor lacks completed retained action authority")
	}
	events, err := s.eventsFor(old.ID)
	if err != nil {
		return err
	}
	matched := 0
	for _, event := range events {
		if event.WorkID == old.ID && event.From == prior.Work.State && event.To == prior.Work.State &&
			event.Note == "coordination cutover published; exact marker reconciled" &&
			slices.Equal(event.Attached, []string{prior.Action.ID, prior.Decision.ID}) {
			matched++
		}
	}
	if matched != 1 {
		return fmt.Errorf("proof migration predecessor lacks exact completed cutover event")
	}
	if !requireV2 {
		return nil
	}
	layout, err := landlease.LayoutStatus(s.home)
	if err != nil {
		return err
	}
	status, err := landlease.ReadProofDomainStatus(backing)
	if err != nil {
		return err
	}
	if layout.Mode != "scoped" || layout.BackingRoot != backing || status.Phase != "active" ||
		status.SchemaVersion != string(landlease.ProofDomainLocalV2) ||
		status.Generation != landlease.ProofDomainMigrationGeneration(backing, prior.Input.ProofHomes) ||
		!slices.Equal(status.Homes, prior.Input.ProofHomes) {
		return fmt.Errorf("proof migration completed history does not match current active v2 domain")
	}
	return nil
}

func (s *Store) readProofMigrationHistory(p resourceMigrationPending) ([]byte, error) {
	if p.PredecessorPreparationRef == "" {
		return nil, nil
	}
	if !slices.Contains(p.Action.EvidenceRefs, p.PredecessorPreparationRef) {
		return nil, fmt.Errorf("proof migration history is not bound by successor action")
	}
	canonicalHome, err := filepath.EvalSymlinks(s.home)
	if err != nil {
		return nil, err
	}
	reader, err := home.OpenArtifactReader(canonicalHome)
	if err != nil {
		return nil, err
	}
	body, err := reader.GetPrivate(p.PredecessorPreparationRef)
	if err != nil {
		return nil, err
	}
	var prior resourceMigrationPending
	if err := json.Unmarshal(body, &prior); err != nil {
		return nil, err
	}
	if err := s.validateProofMigrationHistory(prior, p.Work, p.Input, p.BackingRoot, false); err != nil {
		return nil, err
	}
	return body, nil
}

func (s *Store) preserveProofMigrationHistory(p resourceMigrationPending, body []byte) error {
	if len(body) == 0 {
		return nil
	}
	if proofMigrationHistoryRef(body) != p.PredecessorPreparationRef {
		return fmt.Errorf("proof migration history bytes changed")
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		return err
	}
	ref, err := artifacts.PutPrivate(body)
	if err != nil {
		return err
	}
	if ref != p.PredecessorPreparationRef {
		return fmt.Errorf("proof migration history custody identity changed")
	}
	if s.resourceMigrationFault != nil {
		return s.resourceMigrationFault("proof-history-custodied")
	}
	return nil
}
