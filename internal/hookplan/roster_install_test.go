package hookplan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"compound/internal/lanemanifest"
)

func TestInstallForLaneMinimalRosterWritesOnlyMinimalEvents(t *testing.T) {
	base := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "minimax-lane")

	rep, err := InstallHooksForLane(base, "/fixture/compound", "global",
		"claude-minimax", configDir, lanemanifest.RosterMinimal, false, nil)
	if err != nil {
		t.Fatalf("InstallHooksForLane minimal: %v", err)
	}
	if len(rep.Entries) != 1 || rep.Entries[0].Harness != "claude-minimax" {
		t.Fatalf("minimal report = %+v, want exactly claude-minimax", rep.Entries)
	}
	got := installedEvents(rep)
	want := []string{"PreToolUse", "SessionStart"}
	if !slices.Equal(got, want) {
		t.Fatalf("minimal installed events = %v, want %v", got, want)
	}
	// A minimal lane still carries the blocking mutation guard: the landing/deny
	// gate is the point of the floor.
	settings := readSettingsJSON(t, configDir)
	groups, _ := settings["hooks"].(map[string]any)
	if _, ok := groups["UserPromptSubmit"]; ok {
		t.Fatal("minimal roster must not install UserPromptSubmit")
	}
	if _, ok := groups["PostToolUse"]; ok {
		t.Fatal("minimal roster must not install PostToolUse")
	}
}

func TestInstallForLaneFullRosterWritesEveryLifecycleEvent(t *testing.T) {
	base := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "minimax-lane")

	rep, err := InstallHooksForLane(base, "/fixture/compound", "global",
		"claude-minimax", configDir, lanemanifest.RosterFull, false, nil)
	if err != nil {
		t.Fatalf("InstallHooksForLane full: %v", err)
	}
	want := []string{"PostToolUse", "PreToolUse", "SessionEnd", "SessionStart", "UserPromptSubmit"}
	if got := installedEvents(rep); !slices.Equal(got, want) {
		t.Fatalf("full installed events = %v, want %v", got, want)
	}
}

// TestInstallForLaneTwoWritersRegression is the slice's two-writers probe: an
// external writer (switchback conformance regen) rewrites settings.json from its
// own model, dropping compound's hooks. Compound's check must report the loss as
// drift/absent, and its re-apply must restore the hooks WITHOUT clobbering the
// external keys -- the same NO-CLOBBER contract json_settings.go argues for.
func TestInstallForLaneTwoWritersRegression(t *testing.T) {
	base := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "minimax-lane")
	settingsPath := filepath.Join(configDir, "settings.json")

	// First writer: compound installs the full roster into a switchback-owned profile.
	rep, err := InstallHooksForLane(base, "/fixture/compound", "global",
		"claude-minimax", configDir, lanemanifest.RosterFull, false, nil)
	if err != nil {
		t.Fatalf("initial install: %v", err)
	}
	if rep.Entries[0].Action != "installed" {
		t.Fatalf("initial action = %q, want installed", rep.Entries[0].Action)
	}

	// Second writer: switchback regen overwrites the file with its own model and
	// no hooks -- conformance regen does not know compound's keys.
	regen := `{
  "model": "minimax/MiniMax-M3",
  "effortLevel": "xhigh",
  "apiKeyHelper": "env://CLAUDE_ANTHROPIC_API_KEY"
}
`
	if err := os.WriteFile(settingsPath, []byte(regen), 0o644); err != nil {
		t.Fatal(err)
	}

	// Compound's check must NOT rewrite; it must report the lost roster.
	checkRep, err := InstallHooksForLane(base, "/fixture/compound", "global",
		"claude-minimax", configDir, lanemanifest.RosterFull, true, nil)
	if err != nil {
		t.Fatalf("check after regen: %v", err)
	}
	if checkRep.Entries[0].Action == "ok" {
		t.Fatalf("check after regen reported ok; the regen dropped the roster")
	}
	if got := readFileString(t, settingsPath); got != regen {
		t.Fatalf("check mutated the config: got %s", got)
	}

	// Re-apply restores the hooks and preserves every regen-owned key.
	rep, err = InstallHooksForLane(base, "/fixture/compound", "global",
		"claude-minimax", configDir, lanemanifest.RosterFull, false, nil)
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if rep.Entries[0].Action != "updated" && rep.Entries[0].Action != "installed" {
		t.Fatalf("re-apply action = %q, want updated/installed", rep.Entries[0].Action)
	}
	settings := readSettingsJSON(t, configDir)
	for _, key := range []string{"model", "effortLevel", "apiKeyHelper"} {
		if _, ok := settings[key]; !ok {
			t.Fatalf("re-apply clobbered switchback key %q: %+v", key, settings)
		}
	}
	want := []string{"PostToolUse", "PreToolUse", "SessionEnd", "SessionStart", "UserPromptSubmit"}
	groups, _ := settings["hooks"].(map[string]any)
	var got []string
	for event := range groups {
		got = append(got, event)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("re-applied events = %v, want %v", got, want)
	}
}

func TestInstallForLaneUnknownRosterRejected(t *testing.T) {
	base := t.TempDir()
	if _, err := InstallHooksForLane(base, "/fixture/compound", "global",
		"claude-minimax", filepath.Join(t.TempDir(), "minimax-lane"),
		lanemanifest.Roster("exhaustive"), false, nil); err == nil {
		t.Fatal("expected unknown roster to be rejected")
	} else if !strings.Contains(err.Error(), "roster") {
		t.Fatalf("roster error is not actionable: %v", err)
	}
}

func installedEvents(rep HookInstallReport) []string {
	events := make([]string, 0, len(rep.Entries[0].Bindings))
	for _, binding := range rep.Entries[0].Bindings {
		events = append(events, binding.Event)
	}
	slices.Sort(events)
	return events
}

func readSettingsJSON(t *testing.T, configDir string) map[string]any {
	t.Helper()
	body := readFileString(t, filepath.Join(configDir, "settings.json"))
	var settings map[string]any
	if err := json.Unmarshal([]byte(body), &settings); err != nil {
		t.Fatalf("parse settings: %v\n%s", err, body)
	}
	return settings
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}
