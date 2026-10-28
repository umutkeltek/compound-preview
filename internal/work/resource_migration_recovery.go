package work

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"compound/internal/envelope"
	"compound/internal/landlease"
	"compound/internal/policy"
)

const resourceMigrationPendingSchema = "compound.resource_migration_preparation.v0.1"

// This record bridges publication of a permanent fence and Work's audit writes.
// The marker is the physical outcome; neither this record nor an absent result
// alone proves publication.
type resourceMigrationPending struct {
	PredecessorPreparationRef string `json:",omitempty"`
	PolicyRevision            string
	SchemaVersion             string
	Work                      envelope.WorkEnvelope
	Input                     ResourceMigrationInput
	BackingRoot               string
	Action                    envelope.ActionIntent
	Decision                  envelope.PolicyDecision
}

type ResourceMigrationRecoveryRequired struct {
	ActionIntentID, PolicyDecisionID string
	Cause                            error
}

func (e *ResourceMigrationRecoveryRequired) Error() string {
	return fmt.Sprintf("cutover recovery required: action=%s policy=%s; retry the identical migration to reconcile its exact marker: %v", e.ActionIntentID, e.PolicyDecisionID, e.Cause)
}
func (e *ResourceMigrationRecoveryRequired) Unwrap() error { return e.Cause }
func (s *Store) resourceMigrationRecoveryError(p resourceMigrationPending, err error) error {
	return &ResourceMigrationRecoveryRequired{ActionIntentID: p.Action.ID, PolicyDecisionID: p.Decision.ID, Cause: err}
}
func (s *Store) resourceMigrationPendingPath() string {
	return filepath.Join(s.home, "work", "resource-leases", "migration-preparation.json")
}
func (s *Store) writeResourceMigrationPending(p resourceMigrationPending) error {
	if err := os.MkdirAll(filepath.Dir(s.resourceMigrationPendingPath()), 0700); err != nil {
		return err
	}
	return s.writeJSON(s.resourceMigrationPendingPath(), p)
}

func (s *Store) resumeResourceMigrationLocked(w envelope.WorkEnvelope, in ResourceMigrationInput, root string, now time.Time) (bool, error) {
	data, err := os.ReadFile(s.resourceMigrationPendingPath())
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	var p resourceMigrationPending
	if err := json.Unmarshal(data, &p); err != nil {
		return true, err
	}
	if p.SchemaVersion != resourceMigrationPendingSchema || p.Work.ID != w.ID || p.BackingRoot != root || !reflect.DeepEqual(p.Input, in) {
		return true, fmt.Errorf("migration preparation does not match this work and exact migration inputs")
	}
	if err := s.validateResourceMigrationPreparation(p); err != nil {
		return true, err
	}
	layout, err := landlease.LayoutStatus(s.home)
	if err != nil {
		return true, s.resourceMigrationRecoveryError(p, err)
	}
	if layout.Mode == "legacy" {
		// No fence was published. A one-use grant cannot be refunded or replayed.
		failure := fmt.Errorf("migration interrupted before publication; prepare a fresh exact approval")
		outcomeErr := s.finishResourceMutationLocked(w, p.Action, p.Decision, failure, now)
		return true, errors.Join(outcomeErr, os.Remove(s.resourceMigrationPendingPath()))
	}
	if err := s.finishResourceMigrationLocked(w, p, now); err != nil {
		return true, s.resourceMigrationRecoveryError(p, err)
	}
	return true, nil
}

func (s *Store) validateResourceMigrationPreparation(p resourceMigrationPending) error {
	before, err := workEnvelopeSHA256(p.Work)
	if err != nil {
		return err
	}
	operation := "coordination.migrate"
	if p.Input.ProofClasses {
		operation = "coordination.proof-migrate"
		if p.Action.Preview != proofMigrationPreviewWithHistory(p.Input, p.PolicyRevision, p.PredecessorPreparationRef) {
			return fmt.Errorf("proof migration input authority mismatch")
		}
	}
	if _, err := s.readProofMigrationHistory(p); err != nil {
		return err
	}
	if p.Action.IDs.WorkID != p.Work.ID || p.Action.Actor != p.Input.Actor ||
		p.Action.Action != operation || p.Action.Target.Path != p.BackingRoot ||
		p.Action.ArgumentsHash != before || p.Decision.ActionIntentID != p.Action.ID || !policy.Allowed(p.Decision) {
		return fmt.Errorf("migration preparation authority mismatch")
	}
	if p.Input.ProofClasses {
		if p.PolicyRevision == "" {
			return fmt.Errorf("proof migration lacks policy revision")
		}
		if err := policy.NewExactActionApprovalStore(s.home).ValidateConsumedDecision(p.Action, p.Decision); err != nil {
			return err
		}
	}
	if err := p.Action.Validate(); err != nil {
		return err
	}
	if err := p.Decision.Validate(); err != nil {
		return err
	}
	for _, item := range []struct {
		id    string
		value any
	}{{p.Action.ID, p.Action}, {p.Decision.ID, p.Decision}} {
		record, err := s.events.Read(item.id)
		if err != nil {
			return err
		}
		var actual, expected any
		if err := json.Unmarshal(record.Bytes, &actual); err != nil {
			return err
		}
		encoded, err := json.Marshal(item.value)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(encoded, &expected); err != nil {
			return err
		}
		if !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("migration preparation does not match durable authority %s", item.id)
		}
	}
	return nil
}

func (s *Store) finishResourceMigrationLocked(w envelope.WorkEnvelope, p resourceMigrationPending, now time.Time) error {
	layout, err := landlease.LayoutStatus(s.home)
	if err != nil {
		return err
	}
	if layout.Mode != "scoped" || layout.BackingRoot != p.BackingRoot {
		return fmt.Errorf("exact published migration marker is absent or changed")
	}
	if p.Input.ProofClasses {
		status, err := landlease.ReadProofDomainStatus(p.BackingRoot)
		if err != nil {
			return err
		}
		target, err := resourceProofMigrationRevision(p.Input)
		if err != nil {
			return err
		}
		if status.SchemaVersion != string(target) || status.Phase != "active" || status.Generation != landlease.ProofDomainMigrationGeneration(p.BackingRoot, p.Input.ProofHomes) || !reflect.DeepEqual(status.Homes, p.Input.ProofHomes) {
			return fmt.Errorf("exact active proof-domain cutover is absent or changed")
		}
	}
	before, err := workEnvelopeSHA256(p.Work)
	if err != nil {
		return err
	}
	post := p.Work
	post.ActionIntentIDs = append(append([]string{}, post.ActionIntentIDs...), p.Action.ID)
	post.PolicyDecisionID = p.Decision.ID
	after, err := workEnvelopeSHA256(post)
	if err != nil {
		return err
	}
	current, err := workEnvelopeSHA256(w)
	if err != nil {
		return err
	}
	if current != before && current != after {
		return fmt.Errorf("migration work generation changed; refusing to overwrite newer authority")
	}
	const note = "coordination cutover published; exact marker reconciled"
	events, err := s.eventsFor(w.ID)
	if err != nil {
		return err
	}
	found := false
	for _, event := range events {
		if event.Note == note && reflect.DeepEqual(event.Attached, []string{p.Action.ID, p.Decision.ID}) {
			found = true
			break
		}
	}
	if !found {
		if s.resourceMigrationFault != nil {
			if err := s.resourceMigrationFault("before-result"); err != nil {
				return err
			}
		}
		if err := s.appendEvent(Event{TS: ts(now), WorkID: w.ID, From: w.State, To: w.State, Note: note, Attached: []string{p.Action.ID, p.Decision.ID}}); err != nil {
			return err
		}
	}
	if current == before {
		if s.resourceMigrationFault != nil {
			if err := s.resourceMigrationFault("before-work"); err != nil {
				return err
			}
		}
		_, err = s.recordActionLocked(w.ID, p.Action, p.Decision, now)
	}
	return err
}
