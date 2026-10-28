package runattempt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMintWithActionIntentBindsIntentAtomicallyAtMintTime proves the
// action_intent_id is present in the persisted JSON before any setter runs,
// and that re-binding a different intent is rejected.
func TestMintWithActionIntentBindsIntentAtomicallyAtMintTime(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	workID := "work_mint_intent"
	actor := "codex"
	harness := "fake"
	actionIntentID := "act_9f2c1d4e"

	ra, err := s.MintWithActionIntent(workID, actor, harness, actionIntentID, now)
	if err != nil {
		t.Fatalf("MintWithActionIntent: %v", err)
	}
	if ra.ActionIntentID != actionIntentID {
		t.Fatalf("returned ActionIntentID = %q, want %q", ra.ActionIntentID, actionIntentID)
	}

	// The raw persisted JSON must already carry the action_intent_id; no setter
	// has been called. This is the regression guard against a refactor that moves
	// the binding to AttachActionIntent only.
	path := filepath.Join(home, "run_attempts", ra.ID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted attempt: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode persisted attempt: %v", err)
	}
	if got, _ := raw["action_intent_id"].(string); got != actionIntentID {
		t.Fatalf("persisted action_intent_id = %q, want %q (raw=%s)", got, actionIntentID, string(data))
	}

	// Re-binding the same intent is idempotent success.
	if err := s.AttachActionIntent(ra.ID, actionIntentID); err != nil {
		t.Fatalf("same intent re-attach should succeed: %v", err)
	}

	// Re-binding a different intent must be refused with ErrAlreadyBound.
	if err := s.AttachActionIntent(ra.ID, "act_aaaaaaaa"); !strings.Contains(err.Error(), ErrAlreadyBound.Error()) {
		t.Fatalf("different intent re-bind should fail with ErrAlreadyBound, got: %v", err)
	}

	got, err := Open(home).Get(ra.ID)
	if err != nil {
		t.Fatalf("reload attempt: %v", err)
	}
	if got.ActionIntentID != actionIntentID {
		t.Fatalf("attempt was re-bound: got %q, want %q", got.ActionIntentID, actionIntentID)
	}
}

// TestMintWithActionIntentRejectsInvalidIntentID proves the fail-closed guard
// at mint time: a non-act_ id is refused before any file is written.
func TestMintWithActionIntentRejectsInvalidIntentID(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)

	_, err := s.MintWithActionIntent("work_x", "actor", "fake", "not-an-act-id", now)
	if err == nil || !strings.Contains(err.Error(), "action_intent_id") {
		t.Fatalf("invalid intent id err = %v, want action_intent_id rejection", err)
	}

	entries, err := os.ReadDir(filepath.Join(home, "run_attempts"))
	if err != nil {
		if !os.IsNotExist(err) {
			t.Fatalf("read run_attempts: %v", err)
		}
		return
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			t.Fatalf("invalid intent id wrote file: %s", e.Name())
		}
	}
}

// TestMintWithActionIntentEmptyIsAllowed proves passing an empty action intent
// id still mints a normal RunAttempt (the governed path supplies the intent
// later via AttachActionIntent), and the field stays empty in persisted JSON.
func TestMintWithActionIntentEmptyIsAllowed(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	now := time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)

	ra, err := s.MintWithActionIntent("work_y", "actor", "fake", "", now)
	if err != nil {
		t.Fatalf("MintWithActionIntent(empty): %v", err)
	}
	if ra.ActionIntentID != "" {
		t.Fatalf("empty mint produced ActionIntentID = %q", ra.ActionIntentID)
	}

	path := filepath.Join(home, "run_attempts", ra.ID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted attempt: %v", err)
	}
	if strings.Contains(string(data), "action_intent_id") {
		t.Fatalf("empty mint persisted action_intent_id field: %s", string(data))
	}
}
