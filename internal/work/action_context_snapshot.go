package work

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"compound/internal/envelope"
)

const workActionContextSnapshotSchemaVersion = "compound.work_action_context_snapshot.v0.1"
const workActionContextSnapshotRefPrefix = "action_context_snapshot:"

// workActionContextSnapshot is the immutable admitted copy of a create-plan's
// action context. Create plans remain proposal sidecars; work consumers read
// this content-addressed snapshot once the plan has been admitted.
type workActionContextSnapshot struct {
	SchemaVersion string             `json:"schema_version"`
	ID            string             `json:"id"`
	WorkID        string             `json:"work_id"`
	PlanID        string             `json:"plan_id"`
	ActionContext *WorkActionContext `json:"action_context"`
}

func (s *Store) recordActionContextSnapshot(plan CreatePlan) (string, error) {
	ctx := normalizeWorkActionContext(plan.ActionContext)
	contextBytes, err := json.Marshal(ctx)
	if err != nil {
		return "", err
	}
	id := idFor("wacs_", plan.WorkID, plan.ID, string(contextBytes))
	snapshot := workActionContextSnapshot{
		SchemaVersion: workActionContextSnapshotSchemaVersion,
		ID:            id,
		WorkID:        plan.WorkID,
		PlanID:        plan.ID,
		ActionContext: ctx,
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	dir := filepath.Join(s.home, "work", "action-contexts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, id+".json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if !os.IsExist(err) {
			return "", err
		}
		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return "", readErr
		}
		if !bytes.Equal(existing, data) {
			return "", fmt.Errorf("action context snapshot %s content-address collision", id)
		}
		return workActionContextSnapshotRefPrefix + id, nil
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return workActionContextSnapshotRefPrefix + id, nil
}

func (s *Store) loadActionContextSnapshot(id string) (workActionContextSnapshot, error) {
	var snapshot workActionContextSnapshot
	id = strings.TrimSpace(id)
	if id == "" {
		return snapshot, fmt.Errorf("action context snapshot id required")
	}
	path := filepath.Join(s.home, "work", "action-contexts", id+".json")
	if err := s.readJSON(path, &snapshot); err != nil {
		return snapshot, err
	}
	if snapshot.SchemaVersion != workActionContextSnapshotSchemaVersion || snapshot.ID != id ||
		strings.TrimSpace(snapshot.WorkID) == "" || strings.TrimSpace(snapshot.PlanID) == "" {
		return snapshot, fmt.Errorf("action context snapshot %s invalid", id)
	}
	normalized := normalizeWorkActionContext(snapshot.ActionContext)
	contextBytes, err := json.Marshal(normalized)
	if err != nil {
		return snapshot, err
	}
	if expectedID := idFor("wacs_", snapshot.WorkID, snapshot.PlanID, string(contextBytes)); expectedID != id {
		return snapshot, fmt.Errorf("action context snapshot %s content digest mismatch", id)
	}
	return snapshot, nil
}

func (s *Store) actionContextSnapshotForWork(w envelope.WorkEnvelope, events []Event) (*WorkActionContext, bool, []string) {
	ref := lastAttachedWithPrefix(events, workActionContextSnapshotRefPrefix)
	if ref == "" {
		return nil, false, nil
	}
	id := strings.TrimSpace(strings.TrimPrefix(ref, workActionContextSnapshotRefPrefix))
	snapshot, err := s.loadActionContextSnapshot(id)
	if err != nil {
		return nil, true, []string{"action context snapshot " + id + " unreadable: " + err.Error()}
	}
	if snapshot.WorkID != w.ID {
		return nil, true, []string{"action context snapshot " + id + " belongs to " + snapshot.WorkID + ", not " + w.ID}
	}
	if planID := lastCreatePlanID(events); planID != "" && snapshot.PlanID != planID {
		return nil, true, []string{"action context snapshot " + id + " belongs to create plan " + snapshot.PlanID + ", not " + planID}
	}
	return cloneWorkActionContext(snapshot.ActionContext), true, nil
}

// ValidateActionContextSnapshotForWork proves that an admitted WorkEnvelope's
// immutable action-context snapshot is readable and exactly matches the current
// expected semantics. Callers crossing an automated review boundary must fail
// closed on a missing, unreadable, or stale snapshot.
func (s *Store) ValidateActionContextSnapshotForWork(w envelope.WorkEnvelope, expected *WorkActionContext) error {
	events, err := s.eventsFor(w.ID)
	if err != nil {
		return fmt.Errorf("action context lifecycle unreadable: %w", err)
	}
	actual, found, warnings := s.actionContextSnapshotForWork(w, events)
	if len(warnings) > 0 {
		return fmt.Errorf("action context snapshot invalid: %s", strings.Join(warnings, "; "))
	}
	if !found {
		return fmt.Errorf("immutable action context snapshot missing")
	}
	if !sameWorkActionContext(actual, expected) {
		return fmt.Errorf("immutable action context snapshot does not match expected semantics")
	}
	return nil
}

func sameWorkActionContext(a, b *WorkActionContext) bool {
	a = normalizeWorkActionContext(a)
	b = normalizeWorkActionContext(b)
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	aBytes, _ := json.Marshal(a)
	bBytes, _ := json.Marshal(b)
	return bytes.Equal(aBytes, bBytes)
}
