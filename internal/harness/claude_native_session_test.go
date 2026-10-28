package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
)

// claudeNativeSessionSnapshot is the wire-level plan snapshot a governed
// launch carries before the adapter sees it: no session id, and the wire's
// honest statement that the wire itself pins nothing.
func claudeNativeSessionSnapshot() *runattempt.ExecutionSelectionSnapshot {
	return &runattempt.ExecutionSelectionSnapshot{
		Source:        "override",
		Harness:       "claude",
		ProviderRoute: "fixture/route",
	}
}

// argvSessionID reads the --session-id value the fake claude binary
// actually received. It returns "" when the flag is absent.
func argvSessionID(t *testing.T, argvLog string) string {
	t.Helper()
	body, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	fields := strings.Fields(string(body))
	for i, field := range fields {
		if field != "--session-id" {
			continue
		}
		if i+1 >= len(fields) {
			t.Fatalf("--session-id has no value: %q", string(body))
		}
		return fields[i+1]
	}
	return ""
}

// TestClaudeAdapterPinsNativeSessionOnArgvAndSnapshot proves the launch and
// the record agree: the id the harness received on the argv is exactly the id
// the persisted per-run snapshot claims, and the evidence says it was pinned
// rather than merely observed.
func TestClaudeAdapterPinsNativeSessionOnArgvAndSnapshot(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	binDir := fakeBinaryPath(t, "claude", argvLog, 0)
	withPATH(t, binDir)

	home := t.TempDir()
	store := runattempt.Open(home)
	a := NewClaudeAdapter(store)

	out, err := a.Run(context.Background(), RunInput{
		WorkID:                     "work_claude",
		Harness:                    "claude",
		Workspace:                  home,
		Prompt:                     "hello from claude",
		Now:                        time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
		ExecutionSelectionSnapshot: claudeNativeSessionSnapshot(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	sessionID := argvSessionID(t, argvLog)
	if sessionID == "" {
		t.Fatal("launch did not pin a native session id on the argv")
	}
	if !validNativeSessionUUID(sessionID) {
		t.Fatalf("pinned native session id is not a valid uuid: %q", sessionID)
	}
	stored, err := store.Get(out.RunAttempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ExecutionSelection == nil {
		t.Fatal("attempt carries no execution selection snapshot")
	}
	if stored.ExecutionSelection.NativeSessionID != sessionID {
		t.Fatalf("snapshot session id = %q, argv session id = %q",
			stored.ExecutionSelection.NativeSessionID, sessionID)
	}
	if stored.ExecutionSelection.NativeSessionEvidence != claudeNativeSessionEvidence {
		t.Fatalf("session evidence = %q, want %q",
			stored.ExecutionSelection.NativeSessionEvidence, claudeNativeSessionEvidence)
	}
}

// TestClaudeAdapterNativeSessionIsFreshPerLaunch proves isolation: two
// launches never share a native session, and a successor never inherits its
// predecessor's.
func TestClaudeAdapterNativeSessionIsFreshPerLaunch(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	binDir := fakeBinaryPath(t, "claude", argvLog, 0)
	withPATH(t, binDir)

	home := t.TempDir()
	store := runattempt.Open(home)
	a := NewClaudeAdapter(store)

	run := func(workID string) string {
		t.Helper()
		out, err := a.Run(context.Background(), RunInput{
			WorkID:                     workID,
			Harness:                    "claude",
			Workspace:                  home,
			Prompt:                     "hello from claude",
			Now:                        time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
			ExecutionSelectionSnapshot: claudeNativeSessionSnapshot(),
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return out.RunAttempt.ID
	}
	first := run("work_claude_first")
	second := run("work_claude_second")
	if first == second {
		t.Fatal("two launches share one run attempt")
	}
	firstSession := argvSessionID(t, argvLog)
	if firstSession == "" {
		t.Fatal("first launch pinned no native session")
	}
	// The second launch appended its own --session-id; the last value in the
	// log is the second launch's.
	fields := strings.Fields(strings.TrimSpace(mustReadFile(t, argvLog)))
	var sessions []string
	for i, field := range fields {
		if field == "--session-id" && i+1 < len(fields) {
			sessions = append(sessions, fields[i+1])
		}
	}
	if len(sessions) != 2 {
		t.Fatalf("expected two pinned sessions, got %v", sessions)
	}
	if sessions[0] == sessions[1] {
		t.Fatalf("two launches pinned the same native session: %v", sessions)
	}
}

// TestClaudeAdapterDoesNotMutateCallerSelectionSnapshot proves the copy: the
// caller's plan is not this launch's record, so the adapter must not write
// the pinned session back into it.
func TestClaudeAdapterDoesNotMutateCallerSelectionSnapshot(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	binDir := fakeBinaryPath(t, "claude", argvLog, 0)
	withPATH(t, binDir)

	home := t.TempDir()
	store := runattempt.Open(home)
	a := NewClaudeAdapter(store)

	caller := claudeNativeSessionSnapshot()
	if _, err := a.Run(context.Background(), RunInput{
		WorkID:                     "work_claude",
		Harness:                    "claude",
		Workspace:                  home,
		Prompt:                     "hello from claude",
		Now:                        time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
		ExecutionSelectionSnapshot: caller,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if caller.NativeSessionID != "" {
		t.Fatalf("caller snapshot was mutated with the launch session id: %q", caller.NativeSessionID)
	}
}

// TestClaudeLaneCapabilityReportsSessionPinnable pins the registry row: the
// Claude-family lane can pin a native session, so the registry must not keep
// claiming the launch shape cannot produce one.
func TestClaudeLaneCapabilityReportsSessionPinnable(t *testing.T) {
	capability, ok := SelectionCapabilityFor("claude_minimax")
	if !ok {
		t.Fatal("claude_minimax has no selection capability row")
	}
	if !capability.SessionPinnable {
		t.Fatal("claude_minimax does not report a pinnable native session")
	}
}

// validNativeSessionUUID is a shape check for an RFC 4122 uuid: eight hex
// digits, four, four, four, twelve, with the version nibble at 4.
func validNativeSessionUUID(value string) bool {
	parts := strings.Split(value, "-")
	if len(parts) != 5 {
		return false
	}
	lengths := []int{8, 4, 4, 4, 12}
	for i, part := range parts {
		if len(part) != lengths[i] {
			return false
		}
		for _, r := range part {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				return false
			}
		}
	}
	return parts[2][0] == '4'
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
