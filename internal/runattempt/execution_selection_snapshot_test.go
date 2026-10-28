package runattempt

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func validSnapshot() ExecutionSelectionSnapshot {
	return ExecutionSelectionSnapshot{
		Source:                "saved",
		SelectionRevision:     2,
		LaunchProfileID:       "claude-minimax",
		LaunchProfileRevision: "sha256:eeb7c22957a53d0876ede9bf0a28515c27665f5f2e329e83d93768d715400f4a",
		Harness:               "claude_code",
		HarnessVersion:        "2.1.268",
		ProviderRoute:         "minimax/MiniMax-M3",
		RequestedModelID:      "MiniMax-M3",
		RequestedEffort:       "xhigh",
		AppliedEffort:         "xhigh",
		EffortEvidence:        "env:SB_LANE_REQUESTED_EFFORT",
		SettingsDigest:        "sha256:0000000000000000000000000000000000000000000000000000000000000001",
		CredentialRef:         "env:MINIMAX_API_KEY",
		NativeSessionID:       "3d1f8e2a-1111-4c44-9999-abcdefabcdef",
		NativeSessionEvidence: "pinned:--session-id",
	}
}

// The snapshot is stamped once, persisted, and reloads byte-equal; a record
// minted before the field existed re-marshals without it.
func TestSetExecutionSelectionSnapshotPersistsOnce(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	now := time.Date(2026, 9, 11, 18, 0, 0, 0, time.UTC)
	attempt, err := store.Mint("work_sel", "orchestrator:claude_minimax", "claude_minimax", now)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(attempt)
	if strings.Contains(string(raw), "execution_selection") {
		t.Fatalf("fresh mint must not carry an execution_selection: %s", raw)
	}

	snap := validSnapshot()
	updated, err := store.SetExecutionSelectionSnapshot(attempt.ID, snap)
	if err != nil {
		t.Fatalf("SetExecutionSelectionSnapshot: %v", err)
	}
	if updated.ExecutionSelection == nil || *updated.ExecutionSelection != snap {
		t.Fatalf("snapshot not persisted exactly: %+v", updated.ExecutionSelection)
	}
	if updated.ModelID != "" || updated.ProviderRoute != "" {
		t.Fatalf("requested snapshot populated observed identity: model=%q route=%q", updated.ModelID, updated.ProviderRoute)
	}
	reloaded, err := Open(home).Get(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ExecutionSelection == nil || *reloaded.ExecutionSelection != snap {
		t.Fatalf("snapshot did not survive reload: %+v", reloaded.ExecutionSelection)
	}

	// Idempotent same-content re-stamp is a no-op; different content is refused.
	if _, err := store.SetExecutionSelectionSnapshot(attempt.ID, snap); err != nil {
		t.Fatalf("identical re-stamp must be a no-op, got %v", err)
	}
	changed := snap
	changed.RequestedModelID = "MiniMax-M4"
	if _, err := store.SetExecutionSelectionSnapshot(attempt.ID, changed); err == nil ||
		!strings.Contains(err.Error(), "already set") {
		t.Fatalf("rewriting a historical snapshot must be refused, got %v", err)
	}
	again, _ := Open(home).Get(attempt.ID)
	if again.ExecutionSelection.RequestedModelID != "MiniMax-M3" {
		t.Fatalf("refused rewrite leaked into the record: %+v", again.ExecutionSelection)
	}
}

// Terminal attempts are immutable, exactly like every other S2/S3 setter.
func TestSetExecutionSelectionSnapshotRefusesOnTerminal(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 9, 11, 18, 0, 0, 0, time.UTC)
	attempt, err := store.Mint("work_sel_term", "codex", "claude_code", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(attempt.ID, StatusFailed, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetExecutionSelectionSnapshot(attempt.ID, validSnapshot()); err == nil {
		t.Fatal("terminal attempt accepted a selection snapshot")
	}
}

// The validator is what keeps the record honest: it names each over-claim.
func TestExecutionSelectionSnapshotValidateNamesEveryOverclaim(t *testing.T) {
	cases := map[string]func(s *ExecutionSelectionSnapshot){
		"source must be one of":               func(s *ExecutionSelectionSnapshot) { s.Source = "guess" },
		"selection_revision must be >= 1":     func(s *ExecutionSelectionSnapshot) { s.SelectionRevision = 0 },
		"selection_revision must be 0 unless": func(s *ExecutionSelectionSnapshot) { s.Source = "override" },
		"harness is required":                 func(s *ExecutionSelectionSnapshot) { s.Harness = " " },
		"effort_evidence must carry one of":   func(s *ExecutionSelectionSnapshot) { s.EffortEvidence = "high" },
		"applied_effort must be empty when effort_evidence is unsupported": func(s *ExecutionSelectionSnapshot) {
			s.EffortEvidence = "unsupported:harness has no effort control"
		},
		"applied_effort must be empty when effort_evidence is unobservable": func(s *ExecutionSelectionSnapshot) {
			s.EffortEvidence = "unobservable:label passed through wrapper only"
		},
		"native_session_evidence must carry one of": func(s *ExecutionSelectionSnapshot) { s.NativeSessionEvidence = "yes" },
		"native_session_id must be empty when native_session_evidence is unsupported": func(s *ExecutionSelectionSnapshot) {
			s.NativeSessionEvidence = "unsupported:--no-session"
		},
		"launch_profile_revision must be sha256:": func(s *ExecutionSelectionSnapshot) { s.LaunchProfileRevision = "eeb7" },
		"settings_digest must be sha256:":         func(s *ExecutionSelectionSnapshot) { s.SettingsDigest = "abc" },
		"credential_ref must be a reference":      func(s *ExecutionSelectionSnapshot) { s.CredentialRef = "sk-live-123" },
	}
	for want, mutate := range cases {
		snap := validSnapshot()
		mutate(&snap)
		err := snap.Validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: expected refusal, got %v", want, err)
		}
	}
	// An honest "unsupported" record with empty applied values is valid.
	honest := validSnapshot()
	honest.AppliedEffort = ""
	honest.EffortEvidence = "unsupported:wrapper --no-session and no effort flag"
	honest.NativeSessionID = ""
	honest.NativeSessionEvidence = "unsupported:wrapper passes --no-session"
	if err := honest.Validate(); err != nil {
		t.Fatalf("honest unsupported record must validate: %v", err)
	}
	// A fresh override launch with no effort or session claims is valid.
	minimal := ExecutionSelectionSnapshot{Source: "override", Harness: "os-harness-entrypoints"}
	if err := minimal.Validate(); err != nil {
		t.Fatalf("minimal override record must validate: %v", err)
	}
}

func TestExecutionSelectionSnapshotUnobservableSessionCannotClaimID(t *testing.T) {
	snap := validSnapshot()
	snap.NativeSessionEvidence = "unobservable:adapter did not capture id"
	snap.NativeSessionID = ""
	if err := snap.Validate(); err != nil {
		t.Fatalf("unobservable session without id must validate: %v", err)
	}
	snap.NativeSessionID = "uncaptured-id"
	if err := snap.Validate(); err == nil || !strings.Contains(err.Error(), "native_session_id must be empty") {
		t.Fatalf("unobservable session cannot claim id, got %v", err)
	}
	snap.NativeSessionID = ""
	snap.NativeSessionEvidence = "unobservable:"
	if err := snap.Validate(); err == nil || !strings.Contains(err.Error(), "native_session_evidence must carry one of") {
		t.Fatalf("unobservable session requires a reason, got %v", err)
	}
	snap.NativeSessionEvidence = "unknown:adapter did not capture id"
	if err := snap.Validate(); err == nil || !strings.Contains(err.Error(), "native_session_evidence must carry one of") {
		t.Fatalf("unrecognized session evidence must be rejected, got %v", err)
	}
}
