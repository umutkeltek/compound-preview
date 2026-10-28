package runattempt

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSetExecutionProfileIntentPersistsExactRequestedIdentity(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	attempt, err := store.Mint("work_profile", "codex", "claude_code", now)
	if err != nil {
		t.Fatal(err)
	}

	updated, err := store.SetExecutionProfileIntent(
		attempt.ID,
		"epplan_1234",
		"claude-code",
		"switchback",
		"xhigh",
	)
	if err != nil {
		t.Fatalf("SetExecutionProfileIntent: %v", err)
	}
	if updated.ExecutionProfilePlanID != "epplan_1234" ||
		updated.RequestedClientHarnessID != "claude-code" ||
		updated.RequestedTransportID != "switchback" ||
		updated.RequestedNativeEffort != "xhigh" {
		t.Fatalf("requested execution identity was not persisted exactly: %+v", updated)
	}
	if updated.ModelID != "" || updated.ProviderRoute != "" {
		t.Fatalf("requested intent populated observed identity: model=%q provider=%q", updated.ModelID, updated.ProviderRoute)
	}

	reloaded, err := Open(home).Get(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ExecutionProfilePlanID != updated.ExecutionProfilePlanID ||
		reloaded.RequestedClientHarnessID != updated.RequestedClientHarnessID ||
		reloaded.RequestedTransportID != updated.RequestedTransportID ||
		reloaded.RequestedNativeEffort != updated.RequestedNativeEffort {
		t.Fatalf("requested execution identity did not round-trip: %+v", reloaded)
	}
}

func TestSetExecutionProfileIntentRefusesTerminalAttempt(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	attempt, err := store.Mint("work_profile", "codex", "codex", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(attempt.ID, StatusFailed, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetExecutionProfileIntent(attempt.ID, "epplan_1234", "codex", "native", "high"); err == nil {
		t.Fatal("terminal attempt accepted requested execution-profile mutation")
	}
}

func TestExecutionProfileIntentFieldsAreHashNeutralForOldRecords(t *testing.T) {
	fixture := []byte(`{
  "schema_version": "compound.runattempt.v0.1",
  "id": "ratt_20260720T000000Z_deadbeef",
  "work_id": "work_abc",
  "actor": "codex",
  "harness": "fake",
  "status": "succeeded",
  "created_at": "2026-07-20T00:00:00Z",
  "completed_at": "2026-07-20T00:01:00Z"
}`)
	var attempt RunAttempt
	if err := json.Unmarshal(fixture, &attempt); err != nil {
		t.Fatal(err)
	}
	remarshaled, err := json.MarshalIndent(attempt, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(remarshaled) != string(fixture) {
		t.Fatalf("old record changed after re-marshal:\n%s", remarshaled)
	}
}
