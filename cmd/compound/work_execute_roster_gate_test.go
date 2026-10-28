package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/routeevidence"
	"compound/internal/work"
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

// TestWorkAdvanceExecuteGovernedFullRosterRefusalNoStateChange proves the gate
// fires end-to-end: a tuple-backed governed execute on a lane whose observed
// roster is only minimal is refused BEFORE any state change -- the envelope
// stays claimed and no execution RunAttempt is minted. The refusal logic itself
// is unit-tested in internal/hookplan/governed_roster_test.go.
func TestWorkAdvanceExecuteGovernedFullRosterRefusalNoStateChange(t *testing.T) {
	t.Parallel()
	homePath := initializedTestHome(t)
	now := time.Now().UTC().Truncate(time.Second)
	source := filepath.Join(t.TempDir(), "minimax.yaml")
	manifest := `version: 1
purpose: fixture
capability_classes: [build-implement]
models:
  fixture-minimax:
    lane: fixture minimax
    harness: claude_minimax
    provider_route: minimax
    tier: cheap
    packet_ceiling: multi-file
    aliases: [sh]
    capabilities: {build-implement: {level: capable, evidence: fixture}}
routing_defaults:
  build-implement: [fixture-minimax]
`
	if err := os.WriteFile(source, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := routeevidence.ImportModelCapabilitiesYAML(homePath, source, now); err != nil {
		t.Fatal(err)
	}

	ws, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := ws.Create(work.CreateInput{Owner: "codex", SuccessPredicate: "governed roster refusal leaves work claimed"}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Claim(w.ID, "codex", cliTestNow); err != nil {
		t.Fatal(err)
	}

	userHome := t.TempDir()
	minimaxProfile := filepath.Join(userHome, ".config", "switchback", "claude", "_providers", "minimax-lane")
	writeLaneSettings(t, minimaxProfile, laneSettingsHooks("SessionStart", "PreToolUse"))

	repoRoot, _ := filepath.Abs(filepath.Join("..", ".."))
	cmd := exec.Command(compoundTestBinary(t), "work", "advance", w.ID,
		"--to", "running", "--execute", "--harness", "claude_minimax",
		"--workspace", repoRoot, "--prompt", "execute fixture")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "HOME="+userHome, "COMPOUND_HOME="+homePath)
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Fatalf("expected governed roster refusal to fail, output:\n%s", out)
	}
	if !strings.Contains(string(out), "governed_full_roster") || !strings.Contains(string(out), "full hook roster is not installed") {
		t.Fatalf("roster refusal is not actionable:\n%s", out)
	}

	final, err := ws.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != "claimed" {
		t.Fatalf("governed roster refusal changed work state to %q, want claimed", final.State)
	}
	runAttemptFiles, _ := filepath.Glob(filepath.Join(homePath, "run_attempts", "ratt_*.json"))
	if len(runAttemptFiles) != 1 {
		t.Fatalf("governed roster refusal minted an execution RunAttempt; files=%v", runAttemptFiles)
	}
}
