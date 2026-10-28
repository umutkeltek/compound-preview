package hookplan

import (
	"os"
	"path/filepath"
	"testing"
)

// The launch stamp's live posture comes from the same installed-binding reader
// doctor probes: enforced wins, observation-only is observe, a config without a
// Compound PreToolUse binding (or no config) is none, and a harness Compound
// installs no hooks for is unknown (ok=false), never "none".
func TestReadLiveHookPostureUsesInstalledBindings(t *testing.T) {
	base := t.TempDir()
	path, ok := HookConfigPath(base, "claude_minimax")
	if !ok {
		t.Fatal("claude_minimax has no hook config path")
	}
	if live, ok := ReadLiveHookPosture(base, "claude_minimax"); !ok || live.Posture != "none" || live.ConfigDigest != "" {
		t.Fatalf("absent config = %+v ok=%v, want none", live, ok)
	}
	write := func(command string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		body := `{"hooks":{"PreToolUse":[{"hooks":[{"command":"` + command + `"}]}]}}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("/opt/bin/compound hook pre-tool-use --live --harness claude_minimax")
	if live, _ := ReadLiveHookPosture(base, "claude-minimax"); live.Posture != "observe" || live.ConfigPath != path || live.ConfigDigest == "" {
		t.Fatalf("observation binding = %+v, want observe", live)
	}
	write("/opt/bin/compound hook pre-tool-use --live --harness claude_minimax --enforce")
	if live, _ := ReadLiveHookPosture(base, "claude_minimax"); live.Posture != "enforce" {
		t.Fatalf("enforced binding = %+v, want enforce", live)
	}
	if got := InstalledHookPostures(path); len(got) != 1 || got[0].Posture != HookPostureEnforced {
		t.Fatalf("doctor reader disagrees with the live posture: %+v", got)
	}
	if _, ok := ReadLiveHookPosture(base, "claude_kimi"); ok {
		t.Fatal("a harness with no known hook config must read as unknown, not none")
	}
}
