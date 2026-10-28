package runattempt

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSetLaunchProfileIntentPersistsOpaqueRequestedIdentity(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	attempt, err := store.Mint("work_launch_profile", "codex", "claude_code", now)
	if err != nil {
		t.Fatal(err)
	}

	updated, err := store.SetLaunchProfileIntent(
		attempt.ID,
		"claude-minimax",
		"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"claude-minimax-default",
	)
	if err != nil {
		t.Fatalf("SetLaunchProfileIntent: %v", err)
	}
	if updated.LaunchProfileID != "claude-minimax" ||
		updated.LaunchProfileRevision != "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" ||
		updated.RequestedClientProfileID != "claude-minimax-default" {
		t.Fatalf("requested launch identity was not persisted exactly: %+v", updated)
	}
	if updated.ModelID != "" || updated.ProviderRoute != "" {
		t.Fatalf("requested launch intent populated observed identity: model=%q provider=%q", updated.ModelID, updated.ProviderRoute)
	}

	reloaded, err := Open(home).Get(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.LaunchProfileID != updated.LaunchProfileID ||
		reloaded.LaunchProfileRevision != updated.LaunchProfileRevision ||
		reloaded.RequestedClientProfileID != updated.RequestedClientProfileID {
		t.Fatalf("requested launch identity did not round-trip: %+v", reloaded)
	}
}

func TestSetLaunchProfileIntentRefusesTerminalAttempt(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	attempt, err := store.Mint("work_launch_profile", "codex", "claude_code", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(attempt.ID, StatusFailed, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetLaunchProfileIntent(
		attempt.ID,
		"claude-minimax",
		"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"claude-minimax-default",
	); err == nil {
		t.Fatal("terminal attempt accepted requested launch-profile mutation")
	}
}

func TestLaunchProfileIntentFieldsAreHashNeutralForOldRecords(t *testing.T) {
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
