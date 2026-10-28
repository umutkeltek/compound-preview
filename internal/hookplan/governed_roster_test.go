package hookplan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLaneSettings(t *testing.T, profileDir, hooks string) string {
	t.Helper()
	settingsPath := filepath.Join(profileDir, "settings.json")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{
  "model": "minimax/MiniMax-M3",
  "hooks": ` + hooks + `
}
`
	if err := os.WriteFile(settingsPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return settingsPath
}

func laneSettingsHooks(events ...string) string {
	// The command value is irrelevant to hook-event observation (ObserveRegisteredHookEvents
	// keys on event names), but it must look like a real hook so the fixture reads honestly.
	var b strings.Builder
	b.WriteString("{")
	for i, event := range events {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"` + event + `":[{"hooks":[{"type":"command","command":"/fixture/compound hook --live"}]}]`)
	}
	b.WriteString("}")
	return b.String()
}

func TestGovernedFullRosterRefusal(t *testing.T) {
	base := t.TempDir()
	t.Setenv("HOME", base)

	// Not a hook-governed lane: native claude and codex never refuse at this gate.
	if reason := GovernedFullRosterRefusal("claude_code"); reason != "" {
		t.Fatalf("claude_code refused: %q, want skip", reason)
	}
	if reason := GovernedFullRosterRefusal("codex"); reason != "" {
		t.Fatalf("codex refused: %q, want skip", reason)
	}
	if reason := GovernedFullRosterRefusal("os-harness-entrypoints"); reason != "" {
		t.Fatalf("os-harness-entrypoints refused: %q, want skip", reason)
	}

	// No profile installed: skip (tuple preflight + permission mode govern).
	if reason := GovernedFullRosterRefusal("claude_minimax"); reason != "" {
		t.Fatalf("minimax with no profile refused: %q, want skip", reason)
	}

	// Minimal roster: refusal naming the missing events.
	profile := filepath.Join(base, ".config", "switchback", "claude", "_providers", "minimax-lane")
	writeLaneSettings(t, profile, laneSettingsHooks("SessionStart", "PreToolUse"))
	reason := GovernedFullRosterRefusal("claude_minimax")
	if reason == "" {
		t.Fatal("minimax with minimal roster must refuse governed execution")
	}
	for _, event := range []string{"UserPromptSubmit", "PostToolUse", "SessionEnd"} {
		if !strings.Contains(reason, event) {
			t.Fatalf("refusal does not name missing %s: %q", event, reason)
		}
	}
	if !strings.Contains(reason, "full hook roster") {
		t.Fatalf("refusal is not actionable: %q", reason)
	}

	// Full roster: proceed.
	writeLaneSettings(t, profile, laneSettingsHooks("SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "SessionEnd"))
	if reason := GovernedFullRosterRefusal("claude_minimax"); reason != "" {
		t.Fatalf("minimax with full roster refused: %q", reason)
	}
}

// TestLaneHookSettingsPathPinsStandardProfilePaths pins the one place that names
// a governed lane's hook settings path. Governed spawn (GovernedFullRosterRefusal)
// observes the roster ONLY from os.UserHomeDir() + LaneHookSettingsPath — the
// standard profile path — so a custom --config-dir install is intentionally
// invisible to the governed gate. This test makes that contract observable: the
// exact paths the gate reads are fixed, and non-governed harnesses resolve to
// ("", false).
func TestLaneHookSettingsPathPinsStandardProfilePaths(t *testing.T) {
	base := t.TempDir()

	cases := []struct {
		harness string
		want    string
	}{
		{"claude-minimax", filepath.Join(ClaudeMinimaxProfileDir(base), "settings.json")},
		{"claude_minimax", filepath.Join(ClaudeMinimaxProfileDir(base), "settings.json")},
		{"claude-qwen", filepath.Join(ClaudeQwenProfileDir(base), "settings.json")},
		{"claude_qwen", filepath.Join(ClaudeQwenProfileDir(base), "settings.json")},
		{"claude-zai", filepath.Join(ClaudeZaiProfileDir(base), "settings.json")},
		{"claude_zai", filepath.Join(ClaudeZaiProfileDir(base), "settings.json")},
		// The full zai lane runs a separate subscription profile, not the
		// zai-direct profile the base zai lane shares with the direct tap.
		{"claude_zai_full", filepath.Join(base, ".config", "switchback", "claude", "_providers", "zai-lane", "settings.json")},
	}
	for _, tc := range cases {
		t.Run(tc.harness, func(t *testing.T) {
			got, ok := LaneHookSettingsPath(base, tc.harness)
			if !ok {
				t.Fatalf("LaneHookSettingsPath(%q) = ok false, want true", tc.harness)
			}
			if got != tc.want {
				t.Fatalf("LaneHookSettingsPath(%q) = %q, want %q", tc.harness, got, tc.want)
			}
		})
	}

	// Non-governed harnesses carry no hook profile path compound reads for the
	// governed gate; they must resolve to ("", false), never a default.
	for _, harness := range []string{"claude_code", "codex", "opencode", "claude-native", "os-harness-entrypoints"} {
		if got, ok := LaneHookSettingsPath(base, harness); ok {
			t.Fatalf("LaneHookSettingsPath(%q) = (%q, true), want (\"\", false)", harness, got)
		}
	}
}
