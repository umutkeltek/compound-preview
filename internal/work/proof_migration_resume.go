package work

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"compound/internal/envelope"
	"compound/internal/landlease"
)

// Completed v2 preparation is immutable history for a separately approved v3
// successor. Every other preparation remains owned by its original generation.
func (s *Store) resumeProofMigrationLocked(w envelope.WorkEnvelope, in ResourceMigrationInput, backing string, now func() time.Time, window func() error) (bool, []byte, error) {
	data, err := os.ReadFile(s.proofMigrationPendingPath())
	if errors.Is(err, os.ErrNotExist) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	var prior resourceMigrationPending
	if err := json.Unmarshal(data, &prior); err != nil {
		return false, nil, err
	}
	if prior.Work.ID != w.ID {
		// Only a DISTINCT Work's completed preparation is predecessor history.
		if err := s.validateProofMigrationHistory(prior, w, in, backing, true); err != nil {
			return false, nil, err
		}
		return false, data, nil
	}
	if prior.Input.ProofTargetRevision != in.ProofTargetRevision {
		// Same Work, different exact raw target (an empty target is not an
		// explicit revision). This preparation belongs to another exact request,
		// and a pending preparation of the same Work is never completed history,
		// so the exact preparation mismatch is refused instead of reclassified.
		return false, nil, fmt.Errorf("proof migration preparation differs from exact work and enrollment")
	}
	if prior.SchemaVersion != resourceMigrationPendingSchema || !prior.Input.ProofClasses || prior.BackingRoot != backing || !reflect.DeepEqual(prior.Input.ProofHomes, in.ProofHomes) {
		return false, nil, fmt.Errorf("proof migration preparation differs from exact work and enrollment")
	}
	if err := s.validateResourceMigrationPreparation(prior); err != nil {
		return false, nil, err
	}
	before, err := workEnvelopeSHA256(prior.Work)
	if err != nil {
		return false, nil, err
	}
	current, err := workEnvelopeSHA256(w)
	if err != nil {
		return false, nil, err
	}
	status, err := landlease.ReadProofDomainStatus(backing)
	if err != nil {
		return false, nil, s.resourceMigrationRecoveryError(prior, err)
	}
	target, _ := resourceProofMigrationRevision(prior.Input)
	if status.Phase == "active" && status.SchemaVersion == string(target) {
		return true, nil, s.finishResourceMigrationLocked(w, prior, now())
	}
	if before != current {
		return false, nil, fmt.Errorf("proof migration work generation changed; preserve newer authority")
	}
	if reflect.DeepEqual(prior.Input, in) && prior.Input.ExpiresAt.After(now()) && validatePreEvaluatedReceiptedActionAt(prior.Action, prior.Decision, now()) == nil {
		return true, nil, s.performProofMigrationLocked(w, prior, now, window)
	}
	// Expired approval may not activate a prepared domain. Preserve the original
	// predecessor reference while preparing the newly approved successor.
	var history []byte
	if prior.PredecessorPreparationRef != "" {
		history, err = s.readProofMigrationHistory(prior)
		if err != nil {
			return false, nil, err
		}
	}
	return false, history, nil
}
