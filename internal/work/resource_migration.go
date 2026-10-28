package work

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/runattempt"
)

type ResourceMigrationInput struct {
	Actor, Operator, EvidenceRef, BackingHome string
	WritersPaused                             bool
	ProofClasses                              bool
	ProofHomes                                []string
	ProofTargetRevision                       landlease.ProofDomainRevision `json:",omitempty"`
	ExpiresAt                                 time.Time
}

// MigrateCoordinationResources publishes a one-way old-client fence only after
// current work authority and the complete legacy proof census are clear.
// All participating homes must bind the same backing home during one quiescent
// cutover. The API cannot fence an unrelated home that has not joined that cutover.
func (s *Store) MigrateCoordinationResources(workID string, in ResourceMigrationInput, now time.Time) (landlease.LayoutStatusResult, error) {
	if in.ProofTargetRevision != "" && !in.ProofClasses {
		return landlease.LayoutStatusResult{}, fmt.Errorf("proof target requires --proof-classes")
	}
	if _, err := resourceProofMigrationRevision(in); err != nil {
		return landlease.LayoutStatusResult{}, err
	}
	started := time.Now()
	currentTime := func() time.Time { return now.Add(time.Since(started)) }
	validateWindow := func() error {
		if !in.WritersPaused || !strings.HasPrefix(in.Operator, "operator:") || len(in.Operator) <= len("operator:") || strings.TrimSpace(in.EvidenceRef) == "" {
			return fmt.Errorf("operator provenance and actual all-writer pause evidence required")
		}
		if !in.ExpiresAt.After(currentTime()) || in.ExpiresAt.After(now.Add(5*time.Minute)) {
			return fmt.Errorf("writer pause must be current and expire within five minutes")
		}
		return nil
	}
	backingHome := in.BackingHome
	if strings.TrimSpace(backingHome) == "" {
		return landlease.LayoutStatusResult{}, fmt.Errorf("explicit shared --backing-home required for every participating home")
	}
	absolute, err := filepath.Abs(backingHome)
	if err != nil {
		return landlease.LayoutStatusResult{}, err
	}
	backingHome, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return landlease.LayoutStatusResult{}, err
	}
	in.BackingHome = backingHome
	in.ExpiresAt = in.ExpiresAt.UTC()
	backingRoot, err := home.HabitatPath(backingHome, "work.resource-leases")
	if err != nil {
		return landlease.LayoutStatusResult{}, err
	}
	if in.ProofClasses {
		return s.migrateProofCoordinationResources(workID, in, backingRoot, currentTime, validateWindow)
	}
	err = s.withLifecycleMutation(func() error {
		w, err := s.resourceMutationOwner(workID, in.Actor)
		if err != nil {
			return err
		}
		if resumed, err := s.resumeResourceMigrationLocked(w, in, backingRoot, currentTime()); resumed || err != nil {
			return err
		}
		if err := validateWindow(); err != nil {
			return err
		}
		action := s.resourceMutationAction(w, in.Actor, "coordination.migrate",
			"bind shared proof capacity and repository/ref landing leases; fence legacy clients; operator="+in.Operator+" evidence="+in.EvidenceRef+" expires="+in.ExpiresAt.UTC().Format(time.RFC3339Nano),
			backingRoot, currentTime())
		action.MutationClass = "security"
		action.Reversibility = "irreversible"
		action.BlastRadius = "host"
		action.RollbackPlan = "one-way legacy fence; retain marker and shared root; recover forward under writer quiescence"
		action.ArgumentsHash, err = workEnvelopeSHA256(w)
		if err != nil {
			return err
		}
		action.EvidenceRefs = append(action.EvidenceRefs, in.EvidenceRef)
		decision, err := s.authorizeResourceMutationLocked(w, action, currentTime())
		if err != nil {
			return err
		}
		pending := resourceMigrationPending{SchemaVersion: resourceMigrationPendingSchema, Work: w, Input: in, BackingRoot: backingRoot, Action: action, Decision: decision}
		if err := s.writeResourceMigrationPending(pending); err != nil {
			return err
		}
		if s.resourceMigrationFault != nil {
			if err := s.resourceMigrationFault("prepared"); err != nil {
				return s.resourceMigrationRecoveryError(pending, err)
			}
		}
		err = landlease.MigrateLegacyLayout(s.home, backingRoot, func() error {
			// Under autonomy admission and the legacy lease flock. Never call
			// another landlease operation from this callback.
			if err := s.validateResourceMigrationCensus(); err != nil {
				return err
			}
			return validateWindow()
		})
		if err != nil {
			layout, statusErr := landlease.LayoutStatus(s.home)
			if statusErr != nil {
				return s.resourceMigrationRecoveryError(pending, errors.Join(err, statusErr))
			}
			if layout.Mode != "scoped" {
				outcomeErr := s.finishResourceMutationLocked(w, action, decision, err, currentTime())
				return errors.Join(outcomeErr, os.Remove(s.resourceMigrationPendingPath()))
			}
		}
		if s.resourceMigrationFault != nil {
			if err := s.resourceMigrationFault("published"); err != nil {
				return s.resourceMigrationRecoveryError(pending, err)
			}
		}
		if err := s.finishResourceMigrationLocked(w, pending, currentTime()); err != nil {
			return s.resourceMigrationRecoveryError(pending, err)
		}
		return nil
	})
	if err != nil {
		return landlease.LayoutStatusResult{}, err
	}
	return landlease.LayoutStatus(s.home)
}

func (s *Store) validateResourceMigrationCensus() error {
	// Journals are authority even if their associated lease or attempt was lost.
	for _, area := range []string{"proof-resource-completions", "proof-resource-legacy-recoveries", "proof-resource-retirement-pending"} {
		entries, err := os.ReadDir(filepath.Join(s.home, "work", area))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return fmt.Errorf("resource migration blocked by pending work/%s records", area)
		}
	}
	attempts, err := s.runAttempts().List()
	if err != nil {
		return fmt.Errorf("resource migration RunAttempt census: %w", err)
	}
	byID := make(map[string]runattempt.RunAttempt, len(attempts))
	for _, attempt := range attempts {
		byID[attempt.ID] = attempt
		if attempt.Harness == proofResourceHarness && !runattempt.IsTerminal(attempt.Status) {
			return fmt.Errorf("resource migration blocked by active proof attempt %s", attempt.ID)
		}
	}
	entries, err := s.events.Scan(eventstore.ScanOptions{Prefixes: []string{"pfr_"}, RegularOnly: true})
	if err != nil {
		return err
	}
	for _, entry := range entries {
		record, err := entry.Read()
		if err != nil {
			return err
		}
		var reservation ProofResourceReservation
		if err := json.Unmarshal(record.Bytes, &reservation); err != nil {
			return err
		}
		if reservation.ID != entry.ID || reservation.Kind != "work.proof_resource.reservation" || reservation.RunAttemptID == "" {
			return fmt.Errorf("resource migration cannot establish reservation %s identity", entry.ID)
		}
		attempt, ok := byID[reservation.RunAttemptID]
		if !ok || attempt.WorkID != reservation.WorkID || !runattempt.IsTerminal(attempt.Status) {
			return fmt.Errorf("resource migration blocked by unresolved reservation %s", reservation.ID)
		}
	}
	return nil
}
