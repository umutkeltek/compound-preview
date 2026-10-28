package runattempt

import (
	"os"
	"strings"
	"testing"
	"time"
)

func nativeLookupAttempt(t *testing.T, store *Store, session string, terminal bool) RunAttempt {
	t.Helper()
	at := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	attempt, err := store.Mint("work_session_fixture", "actor:fixture", "claude-code", at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetExecutionSelectionSnapshot(attempt.ID, ExecutionSelectionSnapshot{
		Source: "override", Harness: "claude-code",
		NativeSessionID: session, NativeSessionEvidence: "pinned:--session-id",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitLaunch(attempt.ID, at.Add(time.Second), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if terminal {
		if _, err := store.Complete(attempt.ID, StatusSucceeded, at.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	return attempt
}

func TestListByNativeSessionIDIncludesTerminalAndSkipsUnrelatedSidecars(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	first := nativeLookupAttempt(t, store, "target-session", false)
	second := nativeLookupAttempt(t, store, "target-session", true)
	other := nativeLookupAttempt(t, store, "other-session", true)
	corruptPreparedLaunchEvidence(t, home, other.ID, "malformed-json")
	if _, err := Open(home).List(); err == nil {
		t.Fatal("control: full listing must notice unrelated corrupt launch evidence")
	}
	got, err := Open(home).ListByNativeSessionID("target-session")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, attempt := range got {
		ids[attempt.ID] = true
	}
	if len(got) != 2 || !ids[first.ID] || !ids[second.ID] {
		t.Fatalf("matching live and terminal attempts = %+v", got)
	}
}

func TestListByNativeSessionIDMatchedAndUnreadableEvidenceFailClosed(t *testing.T) {
	t.Run("matched sidecar is validated", func(t *testing.T) {
		home := t.TempDir()
		store := Open(home)
		attempt := nativeLookupAttempt(t, store, "target-session", true)
		corruptPreparedLaunchEvidence(t, home, attempt.ID, "malformed-json")
		if _, err := Open(home).ListByNativeSessionID("target-session"); err == nil || !strings.Contains(err.Error(), attempt.ID) {
			t.Fatalf("matched corrupt evidence became absence: %v", err)
		}
	})
	t.Run("unreadable main record cannot be silently skipped", func(t *testing.T) {
		home := t.TempDir()
		store := Open(home)
		attempt := nativeLookupAttempt(t, store, "other-session", true)
		if err := os.WriteFile(store.path(attempt.ID), []byte("{"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(home).ListByNativeSessionID("target-session"); err == nil || !strings.Contains(err.Error(), attempt.ID) {
			t.Fatalf("unreadable record became absence: %v", err)
		}
	})
	t.Run("empty lookup refuses", func(t *testing.T) {
		if _, err := Open(t.TempDir()).ListByNativeSessionID(" "); err == nil {
			t.Fatal("empty lookup could scan unpinned history")
		}
	})
}
