package hookplan

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"compound/internal/harness"
)

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return m
}

// TestHookCommandQuotesSpacedPath proves HookCommand shell-quotes a binaryPath containing a space
// (e.g. an app-bundle install path) so a real POSIX shell execs the FULL path as one token instead
// of silently truncating at the first space. It fails against a naive
// fmt.Sprintf("%s ...", binaryPath) because sh -c would then try (and fail) to
// execute the truncated prefix.
func TestHookCommandQuotesSpacedPath(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on PATH to prove real shell word-splitting")
	}
	dir := t.TempDir()
	spacedDir := filepath.Join(dir, "Compound Apps")
	if err := os.MkdirAll(spacedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(spacedDir, "compound")
	outFile := filepath.Join(dir, "invoked-as.txt")
	// A stand-in "compound" binary that records the exact path it was invoked as ($0), so we can
	// tell whether the shell resolved the first word of the emitted command to the full spaced
	// path or to a truncated prefix.
	script := "#!/bin/sh\nprintf '%s' \"$0\" > \"" + outFile + "\"\n"
	if err := os.WriteFile(binaryPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := HookCommand(binaryPath, "codex")
	if !strings.Contains(cmd, hookCommandMarker) {
		t.Fatalf("HookCommand() = %q, missing marker %q", cmd, hookCommandMarker)
	}

	if err := exec.Command("sh", "-c", cmd).Run(); err != nil {
		t.Fatalf("sh -c %q failed (spaced binary path was split at the space): %v", cmd, err)
	}
	got, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("stand-in binary never ran (spaced path split by the shell): %v", err)
	}
	if string(got) != binaryPath {
		t.Fatalf("shell resolved the hook binary to %q, want the exact spaced path %q", got, binaryPath)
	}
}

func TestInstallHooksAllHarnesses(t *testing.T) {
	base := t.TempDir()
	rep, err := InstallHooks(base, "/usr/local/bin/compound", "global", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Entries) != 5 {
		t.Fatalf("expected all managed harnesses, got %d entries", len(rep.Entries))
	}
	minimaxEntry := hookInstallEntryForHarness(t, rep, claudeMinimaxHarness)
	if !strings.HasSuffix(minimaxEntry.ConfigPath, filepath.Join("_providers", "minimax-lane", "settings.json")) {
		t.Fatalf("claude-minimax ConfigPath = %q, want suffix _providers/minimax-lane/settings.json", minimaxEntry.ConfigPath)
	}
	qwenEntry := hookInstallEntryForHarness(t, rep, claudeQwenHarness)
	if !strings.HasSuffix(qwenEntry.ConfigPath, filepath.Join("_providers", "qwen", "settings.json")) {
		t.Fatalf("claude-qwen ConfigPath = %q, want suffix _providers/qwen/settings.json", qwenEntry.ConfigPath)
	}
	reportJSON, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var publicReport struct {
		Entries []struct {
			Matcher  string `json:"matcher"`
			Command  string `json:"command"`
			Bindings []struct {
				Event   string `json:"event"`
				Matcher string `json:"matcher"`
				Command string `json:"command"`
			} `json:"bindings"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(reportJSON, &publicReport); err != nil {
		t.Fatal(err)
	}
	for i, entry := range publicReport.Entries {
		harnessName := rep.Entries[i].Harness
		// claude-code is one short because it drops per-turn UserPromptSubmit injection,
		// taking its orientation at SessionStart only. codex (post rule_delta, see
		// adapter_codex.go IntentionRecordReceipt) now binds SessionEnd too, so it matches
		// the full 5-binding shape like the isolated Claude lanes.
		wantBindings := 5
		if harnessName == "claude-code" {
			wantBindings = 4
		}
		if len(entry.Bindings) != wantBindings {
			t.Fatalf("%s reports %d bindings, want %d managed lifecycle events", harnessName, len(entry.Bindings), wantBindings)
		}
		foundPreTool := false
		for _, binding := range entry.Bindings {
			if binding.Event == "PreToolUse" {
				foundPreTool = true
				if harnessName == claudeZaiHarness || harnessName == claudeMinimaxHarness || harnessName == claudeQwenHarness {
					if binding.Matcher != entry.Matcher || binding.Command != entry.Command ||
						!strings.Contains(binding.Command, " --enforce") {
						t.Fatalf("isolated guard projection drifted from binding: entry=%+v binding=%+v", entry, binding)
					}
				} else {
					if entry.Matcher != "" || entry.Command != "" {
						t.Fatalf("%s observation-only posture advertised a blocking guard: %+v", harnessName, entry)
					}
					if strings.Contains(binding.Command, " --enforce") {
						t.Fatalf("%s observation-only PreToolUse command enforces: %q", harnessName, binding.Command)
					}
				}
			}
		}
		if !foundPreTool {
			t.Fatalf("%s omitted PreToolUse action observation: %+v", harnessName, entry.Bindings)
		}
	}
	for _, e := range rep.Entries {
		if e.Action != "installed" {
			t.Fatalf("%s action = %q, want installed", e.Harness, e.Action)
		}
		if e.Harness == "codex" || e.Harness == "claude-code" {
			if e.Matcher != "" || e.Command != "" {
				t.Fatalf("%s observation-only install advertised a guard: %+v", e.Harness, e)
			}
		} else if e.Matcher != "*" || !strings.Contains(e.Command, " --enforce") {
			t.Fatalf("%s blocking guard report = %+v", e.Harness, e)
		}
	}
	// PreToolUse observation and isolated-profile enforcement are intentionally
	// matcher-wide so newly added tools still produce action evidence.
	claude := readJSON(t, filepath.Join(base, ".claude", "settings.json"))
	if got := firstMatcher(t, claude); got != "*" {
		t.Fatalf("claude matcher = %q", got)
	}
	zai := readJSON(t, filepath.Join(ClaudeZaiProfileDir(base), "settings.json"))
	if got := firstMatcher(t, zai); got != "*" {
		t.Fatalf("claude-zai matcher = %q", got)
	}
	minimax := readJSON(t, filepath.Join(ClaudeMinimaxProfileDir(base), "settings.json"))
	if got := firstMatcher(t, minimax); got != "*" {
		t.Fatalf("claude-minimax matcher = %q", got)
	}
	qwen := readJSON(t, filepath.Join(ClaudeQwenProfileDir(base), "settings.json"))
	if got := firstMatcher(t, qwen); got != "*" {
		t.Fatalf("claude-qwen matcher = %q", got)
	}
	codex := readJSON(t, filepath.Join(base, ".codex", "hooks.json"))
	for harnessName, cfg := range map[string]map[string]any{
		"claude-code":    claude,
		"claude-zai":     zai,
		"claude-minimax": minimax,
		"claude-qwen":    qwen,
		"codex":          codex,
	} {
		entry := hookInstallEntryForHarness(t, rep, harnessName)
		for _, binding := range entry.Bindings {
			if !hasHookCommand(cfg, binding.Event, binding.Command) {
				t.Fatalf("%s install did not consume semantic %s binding", harnessName, binding.Event)
			}
		}
	}
}

func TestClaudeProfilesInstallOneBoundedSessionEndAndCodexDoesNot(t *testing.T) {
	wantHarness := map[string]string{
		"claude-code":    "claude_code",
		"claude-zai":     "claude_zai",
		"claude-minimax": "claude_minimax",
		"claude-qwen":    "claude_qwen",
	}
	for installerHarness, runtimeHarness := range wantHarness {
		installerHarness, runtimeHarness := installerHarness, runtimeHarness
		t.Run(installerHarness, func(t *testing.T) {
			base := t.TempDir()
			profileDir := t.TempDir()
			first, err := InstallHooksWithConfigDir(base, "/opt/compound", "global", []string{installerHarness}, profileDir, false)
			if err != nil {
				t.Fatal(err)
			}
			var sessionEnd []HookInstallBinding
			for _, binding := range first.Entries[0].Bindings {
				if binding.Event == "SessionEnd" {
					sessionEnd = append(sessionEnd, binding)
				}
			}
			if len(sessionEnd) != 1 {
				t.Fatalf("%s SessionEnd bindings = %+v, want exactly one", installerHarness, sessionEnd)
			}
			for _, want := range []string{" hook session-end-adapter --live", " --harness " + runtimeHarness} {
				if !strings.Contains(sessionEnd[0].Command, want) {
					t.Fatalf("%s SessionEnd command = %q, missing %q", installerHarness, sessionEnd[0].Command, want)
				}
			}
			second, err := InstallHooksWithConfigDir(base, "/opt/compound", "global", []string{installerHarness}, profileDir, false)
			if err != nil {
				t.Fatal(err)
			}
			if second.Entries[0].Action != "ok" {
				t.Fatalf("%s second install action = %q, want ok", installerHarness, second.Entries[0].Action)
			}
		})
	}

	report, err := InstallHooks(t.TempDir(), "/opt/compound", "global", []string{"codex"}, false)
	if err != nil {
		t.Fatal(err)
	}
	// codex now binds SessionEnd too (rule_delta, see adapter_codex.go IntentionRecordReceipt):
	// it routes through the generic `compound hook session-end --live --harness codex` verb
	// (NOT session-end-adapter, which is the Claude-family-only bounded adapter seam). The
	// command verb is the wire-level honesty signal: codex never claimed the bounded adapter.
	var sessionEnd []HookInstallBinding
	for _, binding := range report.Entries[0].Bindings {
		if binding.Event == "SessionEnd" {
			sessionEnd = append(sessionEnd, binding)
		}
	}
	if len(sessionEnd) != 1 {
		t.Fatalf("Codex SessionEnd bindings = %+v, want exactly one", sessionEnd)
	}
	for _, want := range []string{" hook session-end --live", " --harness codex"} {
		if !strings.Contains(sessionEnd[0].Command, want) {
			t.Fatalf("Codex SessionEnd command = %q, missing %q", sessionEnd[0].Command, want)
		}
	}
	if strings.Contains(sessionEnd[0].Command, "session-end-adapter") {
		t.Fatalf("Codex SessionEnd must NOT use the Claude-only session-end-adapter verb: %q", sessionEnd[0].Command)
	}
	// The rendered hooks.json must carry the SessionEnd key (F2 falsifier). t.TempDir() returns a
	// fresh dir per call, so we capture the install dir before issuing a second tempdir for the
	// re-install (the re-install is what asserts the *rendered* JSON; the first install's bindings
	// already prove the verb shape).
	if report.Entries[0].ConfigPath == "" {
		t.Fatalf("Codex install report missing ConfigPath: %+v", report.Entries[0])
	}
	fresh := t.TempDir()
	freshReport, err := InstallHooks(fresh, "/opt/compound", "global", []string{"codex"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if freshReport.Entries[0].ConfigPath != filepath.Join(fresh, ".codex", "hooks.json") {
		t.Fatalf("Codex config path = %q, want %q", freshReport.Entries[0].ConfigPath, filepath.Join(fresh, ".codex", "hooks.json"))
	}
	cfg := readJSON(t, freshReport.Entries[0].ConfigPath)
	hooks, _ := cfg["hooks"].(map[string]any)
	if _, ok := hooks["SessionEnd"]; !ok {
		t.Fatalf("Codex hooks.json missing SessionEnd key after install: %+v", hooks)
	}
}

func TestSessionEndInstallRetiresOnlyLegacyCompoundHooksAndPreservesForeignMixedGroups(t *testing.T) {
	base := t.TempDir()
	profileDir := t.TempDir()
	settingsPath := filepath.Join(profileDir, "settings.json")
	before := `{
  "model": "fixture/model",
  "hooks": {
    "SessionEnd": [
      {"hooks": [
        {"type": "command", "command": "\"$HOME/.claude/hooks/SessionReceipt.hook.ts\""},
        {"type": "command", "command": "foreign-mixed"}
      ]},
      {"hooks": [{"type": "command", "command": "/old/compound hook session-end --live --harness claude-code"}]},
      {"hooks": [{"type": "command", "command": "/old/compound hook session-end-adapter --live --harness claude-code"}]},
      {"hooks": [{"type": "command", "command": "foreign SessionReceipt.hook.ts --audit"}]},
	  {"hooks": [{"type": "command", "command": "foreign-wrapper hook session-end-adapter --live --harness claude_code"}]},
      {"hooks": [{"type": "command", "command": "foreign-only"}]}
    ]
  }
}`
	if err := os.WriteFile(settingsPath, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallHooksWithConfigDir(base, "/opt/compound", "global", []string{"claude-code"}, profileDir, false); err != nil {
		t.Fatal(err)
	}
	assertSessionEndInstallShape(t, settingsPath)
	second, err := InstallHooksWithConfigDir(base, "/opt/compound", "global", []string{"claude-code"}, profileDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if second.Entries[0].Action != "ok" {
		t.Fatalf("second install action = %q, want ok", second.Entries[0].Action)
	}
	assertSessionEndInstallShape(t, settingsPath)
}

func assertSessionEndInstallShape(t *testing.T, settingsPath string) {
	t.Helper()
	cfg := readJSON(t, settingsPath)
	hooks := cfg["hooks"].(map[string]any)
	groups := hooks["SessionEnd"].([]any)
	counts := map[string]int{}
	for _, rawGroup := range groups {
		group, ok := rawGroup.(map[string]any)
		if !ok {
			continue
		}
		for _, rawHook := range group["hooks"].([]any) {
			hook := rawHook.(map[string]any)
			command, _ := hook["command"].(string)
			switch {
			case command == HookCommandForEvent("/opt/compound", "claude-code", "SessionEnd"):
				counts["adapter"]++
			case command == `"$HOME/.claude/hooks/SessionReceipt.hook.ts"`:
				counts["legacy"]++
			case strings.Contains(command, " hook session-end --live"):
				counts["direct"]++
			default:
				counts[command]++
			}
		}
	}
	if counts["adapter"] != 1 || counts["legacy"] != 0 || counts["direct"] != 0 {
		t.Fatalf("managed SessionEnd counts = %+v, want one adapter and no retired hooks", counts)
	}
	for _, foreign := range []string{
		"foreign-mixed",
		"foreign SessionReceipt.hook.ts --audit",
		"foreign-wrapper hook session-end-adapter --live --harness claude_code",
		"foreign-only",
	} {
		if counts[foreign] != 1 {
			t.Fatalf("foreign SessionEnd hook %q count=%d; all counts=%+v", foreign, counts[foreign], counts)
		}
	}
}

func TestLegacyRecallContextClassifierIsExactAndSessionStartOnly(t *testing.T) {
	for _, command := range []string{
		`"$HOME/.claude/hooks/RecallContext.hook.ts"`,
		`/Users/example/.claude/hooks/RecallContext.hook.ts`,
		`/opt/bun $HOME/.codex/hooks/RecallContext.hook.ts`,
		`RECALL_HARNESS=codex /Users/example/.bun/bin/bun /Users/example/.codex/hooks/RecallContext.hook.ts`,
	} {
		hook := map[string]any{"type": "command", "command": command}
		if !hookIsCompoundEvent(hook, "SessionStart") {
			t.Errorf("canonical legacy SessionStart not recognized: %q", command)
		}
		if hookIsCompoundEvent(hook, "PreToolUse") {
			t.Errorf("legacy SessionStart misclassified for another event: %q", command)
		}
	}
	for _, command := range []string{
		`$HOME/.codex/hooks/RecallContext.hook.ts`,
		`node $HOME/.codex/hooks/RecallContext.hook.ts`,
		`RECALL_HARNESS=claude /opt/bun $HOME/.codex/hooks/RecallContext.hook.ts`,
		`RECALL_HARNESS=codex /opt/bun $HOME/.codex/hooks/RecallContext.hook.ts --extra`,
		`/foreign/RecallContext.hook.ts`,
		`foreign RecallContext.hook.ts --audit`,
	} {
		if hookIsCompoundEvent(map[string]any{"type": "command", "command": command}, "SessionStart") {
			t.Errorf("near-match foreign hook was classified as legacy: %q", command)
		}
	}
}

func TestClaudeSessionStartInstallRetiresRecallContextAndPreservesForeignHooks(t *testing.T) {
	base := t.TempDir()
	profileDir := t.TempDir()
	settingsPath := filepath.Join(profileDir, "settings.json")
	before := `{
  "hooks": {
    "SessionStart": [
      {"hooks": [
        {"type": "command", "command": "\"$HOME/.claude/hooks/RecallContext.hook.ts\""},
        {"type": "command", "command": "\"$HOME/.claude/hooks/KittyEnvPersist.hook.ts\""},
        {"type": "command", "command": "foreign RecallContext.hook.ts --audit"}
      ]},
      {"hooks": [{"type": "command", "command": "/old/compound hook session-start --live --harness claude_code"}]}
    ]
  }
}`
	if err := os.WriteFile(settingsPath, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	check, err := InstallHooksWithConfigDir(base, "/opt/compound", "global", []string{"claude-code"}, profileDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if check.Entries[0].Action != "drift" {
		t.Fatalf("pre-cutover check action = %q, want drift", check.Entries[0].Action)
	}
	if got, err := os.ReadFile(settingsPath); err != nil || string(got) != before {
		t.Fatalf("check mode changed settings: err=%v body=%q", err, got)
	}

	first, err := InstallHooksWithConfigDir(base, "/opt/compound", "global", []string{"claude-code"}, profileDir, false)
	if err != nil {
		t.Fatal(err)
	}
	assertSessionStartCutoverShape(t, settingsPath, HookCommandForEvent("/opt/compound", "claude-code", "SessionStart"), []string{
		`"$HOME/.claude/hooks/KittyEnvPersist.hook.ts"`,
		"foreign RecallContext.hook.ts --audit",
	})
	second, err := InstallHooksWithConfigDir(base, "/opt/compound", "global", []string{"claude-code"}, profileDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Entries[0].Action != "updated" || second.Entries[0].Action != "ok" {
		t.Fatalf("install actions first=%q second=%q", first.Entries[0].Action, second.Entries[0].Action)
	}
}

func TestCodexSessionStartInstallRetiresOnlyCanonicalRecallContext(t *testing.T) {
	base := t.TempDir()
	hooksPath := filepath.Join(base, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o755); err != nil {
		t.Fatal(err)
	}
	before := `{
  "hooks": {
    "SessionStart": [
      {"hooks": [
        {"type": "command", "command": "RECALL_HARNESS=codex /Users/example/.bun/bin/bun /Users/example/.codex/hooks/RecallContext.hook.ts"},
        {"type": "command", "command": "RECALL_HARNESS=codex /opt/bun /foreign/RecallContext.hook.ts"},
        {"type": "command", "command": "foreign-only"}
      ]},
      {"hooks": [{"type": "command", "command": "/old/compound hook session-start --live --harness codex"}]}
    ]
  }
}`
	if err := os.WriteFile(hooksPath, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := InstallHooks(base, "/opt/compound", "global", []string{"codex"}, false)
	if err != nil {
		t.Fatal(err)
	}
	assertSessionStartCutoverShape(t, hooksPath, HookCommandForEvent("/opt/compound", "codex", "SessionStart"), []string{
		"RECALL_HARNESS=codex /opt/bun /foreign/RecallContext.hook.ts",
		"foreign-only",
	})
	second, err := InstallHooks(base, "/opt/compound", "global", []string{"codex"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Entries[0].Action != "updated" || second.Entries[0].Action != "ok" {
		t.Fatalf("install actions first=%q second=%q", first.Entries[0].Action, second.Entries[0].Action)
	}
}

func assertSessionStartCutoverShape(t *testing.T, configPath, desired string, foreign []string) {
	t.Helper()
	cfg := readJSON(t, configPath)
	hooks := cfg["hooks"].(map[string]any)
	groups := hooks["SessionStart"].([]any)
	counts := map[string]int{}
	for _, rawGroup := range groups {
		group := rawGroup.(map[string]any)
		for _, rawHook := range group["hooks"].([]any) {
			hook := rawHook.(map[string]any)
			command, _ := hook["command"].(string)
			counts[command]++
		}
	}
	if counts[desired] != 1 {
		t.Fatalf("desired Compound SessionStart count=%d; counts=%+v", counts[desired], counts)
	}
	for command := range counts {
		if isLegacyRecallContextSessionStartCommand(command) {
			t.Fatalf("legacy RecallContext survived: %q counts=%+v", command, counts)
		}
	}
	for _, command := range foreign {
		if counts[command] != 1 {
			t.Fatalf("foreign hook %q count=%d; counts=%+v", command, counts[command], counts)
		}
	}
}

// Installer profile names use dashes because they are operator-facing launch labels,
// while HookEvent/ContextPack harness identities use the registry's snake_case keys.
// The installed command is the boundary between those namespaces: emitting the
// operator label here makes a real hook run through the generic fail-open harness.
func TestInstallHooksEmitsCanonicalRuntimeHarnessNames(t *testing.T) {
	rep, err := InstallHooks(t.TempDir(), "/usr/local/bin/compound", "global", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"claude-code":    "claude_code",
		"claude-zai":     "claude_zai",
		"claude-minimax": "claude_minimax",
		"claude-qwen":    "claude_qwen",
		"codex":          "codex",
	}
	for _, entry := range rep.Entries {
		runtimeHarness, ok := want[entry.Harness]
		if !ok {
			t.Fatalf("unexpected installer harness %q", entry.Harness)
		}
		needle := " --harness " + runtimeHarness
		for _, binding := range entry.Bindings {
			if !strings.Contains(binding.Command, needle) {
				t.Errorf("%s %s command = %q, want canonical runtime identity %q", entry.Harness, binding.Event, binding.Command, runtimeHarness)
			}
		}
	}
}

func TestClaudeFamilyAdapterRegistrationsUseCanonicalRuntimeHarnessNames(t *testing.T) {
	want := map[string]string{
		"claude-zai":     "claude_zai",
		"claude-minimax": "claude_minimax",
		"claude-qwen":    "claude_qwen",
	}
	for installerHarness, runtimeHarness := range want {
		installerHarness, runtimeHarness := installerHarness, runtimeHarness
		t.Run(installerHarness, func(t *testing.T) {
			path := filepath.Join("..", "..", "adapters", installerHarness, "hook-registration.ts")
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			canonical := "--harness " + runtimeHarness
			if got := strings.Count(string(body), canonical); got != 5 {
				t.Fatalf("%s contains %d canonical runtime commands, want 5\n%s", path, got, body)
			}
			if strings.Contains(string(body), "--harness "+installerHarness) {
				t.Fatalf("%s still passes the operator-facing installer label to live hooks\n%s", path, body)
			}
			if !strings.Contains(string(body), "hook session-end-adapter --live "+canonical) {
				t.Fatalf("%s omits the bounded SessionEnd adapter\n%s", path, body)
			}
		})
	}
}

func TestInstallHooksClaudeCodeAtExplicitProfileDir(t *testing.T) {
	base := t.TempDir()
	profileDir := filepath.Join(base, "switchback-profile")
	settingsPath := filepath.Join(profileDir, "settings.json")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	before := []byte("{\n  \"model\": \"fixture/model\",\n  \"effortLevel\": \"xhigh\"\n}\n")
	if err := os.WriteFile(settingsPath, before, 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := InstallHooksWithConfigDir(base, "/usr/local/bin/compound", "global", []string{"claude-code"}, profileDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Entries) != 1 || rep.Entries[0].ConfigPath != settingsPath {
		t.Fatalf("explicit profile install = %+v, want one entry at %s", rep, settingsPath)
	}
	if rep.Entries[0].Matcher != "" || rep.Entries[0].Command != "" {
		t.Fatalf("generic Claude profile must keep the canonical claude_code observation posture: %+v", rep.Entries[0])
	}
	cfg := readJSON(t, settingsPath)
	if cfg["model"] != "fixture/model" || cfg["effortLevel"] != "xhigh" {
		t.Fatalf("hook enrollment clobbered Switchback-owned model/effort: %+v", cfg)
	}
	for _, binding := range rep.Entries[0].Bindings {
		if !strings.Contains(binding.Command, " --harness claude_code") {
			t.Fatalf("profile hook command has noncanonical runtime identity: %q", binding.Command)
		}
		if strings.Contains(binding.Command, " --enforce") {
			t.Fatalf("generic Claude profile unexpectedly upgraded observation to blocking: %q", binding.Command)
		}
	}
}

func TestInstallHooksPreflightsEveryHarnessConfigBeforeWriting(t *testing.T) {
	base := t.TempDir()
	claudePath := filepath.Join(base, ".claude", "settings.json")
	codexPath := filepath.Join(base, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(claudePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	claudeBefore := []byte("{\n  \"permissions\": {\"defaultMode\": \"bypassPermissions\"}\n}\n")
	codexBefore := []byte("{not-json\n")
	if err := os.WriteFile(claudePath, claudeBefore, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexPath, codexBefore, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallHooks(base, "/bin/compound", "global", nil, false); err == nil {
		t.Fatal("all-harness install accepted an invalid later Codex config")
	}

	claudeAfter, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(claudeAfter) != string(claudeBefore) {
		t.Fatalf("Claude config changed before later-harness validation completed:\nbefore=%s\nafter=%s", claudeBefore, claudeAfter)
	}
	zaiPath := filepath.Join(ClaudeZaiProfileDir(base), "settings.json")
	if _, err := os.Stat(zaiPath); !os.IsNotExist(err) {
		t.Fatalf("isolated Claude config was written before later-harness validation completed: %v", err)
	}
	codexAfter, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(codexAfter) != string(codexBefore) {
		t.Fatalf("invalid Codex config changed during failed preflight:\nbefore=%s\nafter=%s", codexBefore, codexAfter)
	}
}

func TestInstallHooksRefusesSymlinkedConfigBeforeAnyWrite(t *testing.T) {
	base := t.TempDir()
	claudePath := filepath.Join(base, ".claude", "settings.json")
	codexPath := filepath.Join(base, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(claudePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	claudeBefore := []byte("{\n  \"permissions\": {\"defaultMode\": \"bypassPermissions\"}\n}\n")
	if err := os.WriteFile(claudePath, claudeBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkTarget := filepath.Join(t.TempDir(), "codex-hooks.json")
	targetBefore := []byte("{\n  \"custom_setting\": true\n}\n")
	if err := os.WriteFile(symlinkTarget, targetBefore, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(symlinkTarget, codexPath); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallHooks(base, "/bin/compound", "global", nil, false); err == nil ||
		!strings.Contains(err.Error(), "symlink") {
		t.Fatalf("all-harness install error = %v, want symlink refusal", err)
	}
	claudeAfter, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(claudeAfter) != string(claudeBefore) {
		t.Fatalf("symlink preflight failure changed earlier Claude config:\nbefore=%s\nafter=%s", claudeBefore, claudeAfter)
	}
	zaiPath := filepath.Join(ClaudeZaiProfileDir(base), "settings.json")
	if _, err := os.Stat(zaiPath); !os.IsNotExist(err) {
		t.Fatalf("symlink preflight failure created isolated config: %v", err)
	}
	info, err := os.Lstat(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("install replaced symlinked config with mode %v", info.Mode())
	}
	targetAfter, err := os.ReadFile(symlinkTarget)
	if err != nil {
		t.Fatal(err)
	}
	if string(targetAfter) != string(targetBefore) {
		t.Fatalf("install changed symlink target:\nbefore=%s\nafter=%s", targetBefore, targetAfter)
	}
}

func TestCommitPreparedHookConfigRefusesPreimageDrift(t *testing.T) {
	initial := []byte("{\n  \"initial\": true\n}\n")
	concurrent := []byte("{\n  \"concurrent\": true\n}\n")
	tests := []struct {
		name            string
		initiallyExists bool
		mutate          func(*testing.T, string)
		assertPreserved func(*testing.T, string)
	}{
		{
			name: "appearance",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, concurrent, 0o640); err != nil {
					t.Fatal(err)
				}
			},
			assertPreserved: func(t *testing.T, path string) {
				t.Helper()
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != string(concurrent) {
					t.Fatalf("appearance drift was clobbered:\nwant=%s\ngot=%s", concurrent, got)
				}
			},
		},
		{
			name:            "bytes",
			initiallyExists: true,
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, concurrent, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			assertPreserved: func(t *testing.T, path string) {
				t.Helper()
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != string(concurrent) {
					t.Fatalf("byte drift was clobbered:\nwant=%s\ngot=%s", concurrent, got)
				}
			},
		},
		{
			name:            "disappearance",
			initiallyExists: true,
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
			assertPreserved: func(t *testing.T, path string) {
				t.Helper()
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("disappearance drift was overwritten: %v", err)
				}
			},
		},
		{
			name:            "mode",
			initiallyExists: true,
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Chmod(path, 0o640); err != nil {
					t.Fatal(err)
				}
			},
			assertPreserved: func(t *testing.T, path string) {
				t.Helper()
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if got := info.Mode().Perm(); got != 0o640 {
					t.Fatalf("mode drift was clobbered: got %o want 640", got)
				}
			},
		},
		{
			name:            "symlink identity",
			initiallyExists: true,
			mutate: func(t *testing.T, path string) {
				t.Helper()
				target := filepath.Join(t.TempDir(), "replacement.json")
				if err := os.WriteFile(target, initial, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
			assertPreserved: func(t *testing.T, path string) {
				t.Helper()
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("symlink identity drift was replaced with mode %v", info.Mode())
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hooks.json")
			if test.initiallyExists {
				if err := os.WriteFile(path, initial, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			prepared, _, err := prepareHookBindings(
				"codex",
				path,
				[]desiredHookBinding{{
					Event:   "PreToolUse",
					Matcher: "*",
					Command: "compound hook pre-tool-use --live",
				}},
				false,
			)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, path)

			if err := commitPreparedHookConfig(prepared); err == nil ||
				!strings.Contains(err.Error(), "changed since preflight") {
				t.Fatalf("commit error = %v, want preimage drift refusal", err)
			}
			test.assertPreserved(t, path)
		})
	}
}

func TestHookInstallTransactionRollsBackPriorCommitsOnLaterPreimageDrift(t *testing.T) {
	base := t.TempDir()
	firstPath := filepath.Join(base, "first.json")
	laterPath := filepath.Join(base, "later.json")
	bindings := []desiredHookBinding{{
		Event:   "PreToolUse",
		Matcher: "*",
		Command: "compound hook pre-tool-use --live",
	}}
	first, _, err := prepareHookBindings("claude-code", firstPath, bindings, false)
	if err != nil {
		t.Fatal(err)
	}
	later, _, err := prepareHookBindings("codex", laterPath, bindings, false)
	if err != nil {
		t.Fatal(err)
	}
	concurrent := []byte("{\n  \"concurrent\": true\n}\n")

	err = commitHookInstallTransaction(
		[]preparedHookConfig{first, later},
		HookInstallReport{},
		nil,
		func(config preparedHookConfig) error {
			if config.path != laterPath {
				return nil
			}
			return os.WriteFile(laterPath, concurrent, 0o640)
		},
	)
	if err == nil || !strings.Contains(err.Error(), "changed since preflight") {
		t.Fatalf("transaction error = %v, want later preimage drift", err)
	}
	if _, err := os.Stat(firstPath); !os.IsNotExist(err) {
		t.Fatalf("later drift left earlier committed config at %s: %v", firstPath, err)
	}
	laterAfter, err := os.ReadFile(laterPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(laterAfter) != string(concurrent) {
		t.Fatalf("later concurrent config was clobbered:\nwant=%s\ngot=%s", concurrent, laterAfter)
	}
}

func TestInstallHooksRollsBackEveryNativeConfigWhenPublishFails(t *testing.T) {
	base := t.TempDir()
	claudePath := filepath.Join(base, ".claude", "settings.json")
	codexPath := filepath.Join(base, ".codex", "hooks.json")
	for _, path := range []string{claudePath, codexPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	claudeBefore := []byte(`{
  "permissions": {"defaultMode": "bypassPermissions"},
  "hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "my-own-claude-hook"}]}]}
}
`)
	codexBefore := []byte(`{
  "custom_setting": true,
  "hooks": {"PreToolUse": [{"matcher": "shell", "hooks": [{"type": "command", "command": "my-own-codex-hook"}]}]}
}
`)
	if err := os.WriteFile(claudePath, claudeBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexPath, codexBefore, 0o640); err != nil {
		t.Fatal(err)
	}

	publishErr := errors.New("synthetic authority publication failure")
	publisherCalled := false
	_, err := InstallHooksWithConfigDirAndPublish(
		base,
		"/bin/compound",
		"global",
		nil,
		"",
		false,
		func(report HookInstallReport) error {
			publisherCalled = true
			if len(report.Entries) != 5 {
				t.Fatalf("publisher received %d entries, want one complete all-harness report", len(report.Entries))
			}
			for _, entry := range report.Entries {
				data, readErr := os.ReadFile(entry.ConfigPath)
				if readErr != nil {
					t.Fatalf("publisher could not read committed %s config: %v", entry.Harness, readErr)
				}
				if got := hookConfigRevision(data); got != entry.ConfigRevision {
					t.Fatalf("%s committed revision = %s, report says %s", entry.Harness, got, entry.ConfigRevision)
				}
			}
			return publishErr
		},
	)
	if !errors.Is(err, publishErr) {
		t.Fatalf("install error = %v, want publication failure", err)
	}
	if !publisherCalled {
		t.Fatal("publisher was not called after native configs committed")
	}

	for _, fixture := range []struct {
		path string
		body []byte
		mode os.FileMode
	}{
		{path: claudePath, body: claudeBefore, mode: 0o600},
		{path: codexPath, body: codexBefore, mode: 0o640},
	} {
		after, readErr := os.ReadFile(fixture.path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(after) != string(fixture.body) {
			t.Fatalf("rollback changed %s:\nbefore=%s\nafter=%s", fixture.path, fixture.body, after)
		}
		info, statErr := os.Stat(fixture.path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if got := info.Mode().Perm(); got != fixture.mode {
			t.Fatalf("rollback mode for %s = %o, want %o", fixture.path, got, fixture.mode)
		}
	}
	zaiPath := filepath.Join(ClaudeZaiProfileDir(base), "settings.json")
	if _, statErr := os.Stat(zaiPath); !os.IsNotExist(statErr) {
		t.Fatalf("rollback left previously absent isolated config at %s: %v", zaiPath, statErr)
	}
	minimaxPath := filepath.Join(ClaudeMinimaxProfileDir(base), "settings.json")
	if _, statErr := os.Stat(minimaxPath); !os.IsNotExist(statErr) {
		t.Fatalf("rollback left previously absent isolated config at %s: %v", minimaxPath, statErr)
	}
	qwenPath := filepath.Join(ClaudeQwenProfileDir(base), "settings.json")
	if _, statErr := os.Stat(qwenPath); !os.IsNotExist(statErr) {
		t.Fatalf("rollback left previously absent isolated config at %s: %v", qwenPath, statErr)
	}
}

func TestInstallHooksRollbackDoesNotClobberConcurrentConfigChange(t *testing.T) {
	base := t.TempDir()
	concurrent := []byte("{\n  \"concurrent_user_edit\": true\n}\n")
	publishErr := errors.New("synthetic authority publication failure")
	var configPath string
	_, err := InstallHooksWithConfigDirAndPublish(
		base,
		"/bin/compound",
		"global",
		[]string{"codex"},
		"",
		false,
		func(report HookInstallReport) error {
			configPath = report.Entries[0].ConfigPath
			if writeErr := os.WriteFile(configPath, concurrent, 0o644); writeErr != nil {
				t.Fatal(writeErr)
			}
			return publishErr
		},
	)
	if !errors.Is(err, publishErr) {
		t.Fatalf("install error = %v, want publication failure", err)
	}
	if err == nil || !strings.Contains(err.Error(), "changed after commit") {
		t.Fatalf("install error = %v, want rollback concurrency conflict", err)
	}
	after, readErr := os.ReadFile(configPath)
	if readErr != nil {
		t.Fatalf("rollback removed concurrent edit: %v", readErr)
	}
	if string(after) != string(concurrent) {
		t.Fatalf("rollback clobbered concurrent edit:\nwant=%s\ngot=%s", concurrent, after)
	}
}

func TestCodexInstallReplacesOnlyCompoundPreToolUseBindingWithObservation(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{
  "hooks": {
    "PreToolUse": [
      {"matcher": "*", "hooks": [{"type": "command", "command": "'/old/compound' hook pre-tool-use --live --harness codex"}]},
      {"matcher": "apply_patch", "hooks": [{"type": "command", "command": "my-own-hook"}]}
    ]
  }
}`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallHooks(base, "/bin/compound", "global", []string{"codex"}, false); err != nil {
		t.Fatal(err)
	}

	got := readJSON(t, path)
	hooks := got["hooks"].(map[string]any)
	pre, _ := hooks["PreToolUse"].([]any)
	if len(pre) != 2 {
		t.Fatalf("Codex reconciliation must preserve user hook plus one Compound observation: %+v", pre)
	}
	foundUser, foundCompoundObservation := false, false
	for _, group := range pre {
		command := firstCommand(group)
		switch {
		case command == "my-own-hook":
			foundUser = true
		case strings.Contains(command, hookCommandMarker) && !strings.Contains(command, " --enforce"):
			foundCompoundObservation = true
		}
	}
	if !foundUser || !foundCompoundObservation {
		t.Fatalf("Codex reconciliation user=%t observation=%t: %+v", foundUser, foundCompoundObservation, pre)
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse"} {
		if !hasHookCommand(got, event, HookCommandForEvent("/bin/compound", "codex", event)) {
			t.Fatalf("Codex reconciliation lost desired %s binding", event)
		}
	}
}

func TestCodexInstallPreservesForeignHooksCoLocatedWithCompoundHook(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "*",
        "user_group": "keep-me",
        "hooks": [
          {"type": "command", "command": "foreign-before"},
          {"type": "command", "command": "'/old/compound' hook pre-tool-use --live --harness codex --enforce"},
          {"type": "command", "command": "foreign-after"}
        ]
      },
      {"matcher": "apply_patch", "hooks": [{"type": "command", "command": "foreign-later"}]}
    ]
  }
}`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallHooks(base, "/bin/compound", "global", []string{"codex"}, false); err != nil {
		t.Fatal(err)
	}

	cfg := readJSON(t, path)
	groups := hooksForEvent(t, cfg, "PreToolUse")
	var foreignCommands []string
	compoundCommands := 0
	foundMixedUserGroup := false
	for _, group := range groups {
		entry, _ := group.(map[string]any)
		inner, _ := entry["hooks"].([]any)
		var groupForeign []string
		for _, hook := range inner {
			hookMap, _ := hook.(map[string]any)
			command, _ := hookMap["command"].(string)
			if strings.Contains(command, hookCommandMarker) {
				compoundCommands++
				if command != HookCommandForEvent("/bin/compound", "codex", "PreToolUse") {
					t.Fatalf("installed Compound command = %q, want current observer", command)
				}
				continue
			}
			if command != "" {
				foreignCommands = append(foreignCommands, command)
				groupForeign = append(groupForeign, command)
			}
		}
		if entry["user_group"] == "keep-me" {
			foundMixedUserGroup = true
			if !slices.Equal(groupForeign, []string{"foreign-before", "foreign-after"}) {
				t.Fatalf("co-located foreign hook order = %v, want both hooks preserved", groupForeign)
			}
		}
	}
	if !foundMixedUserGroup {
		t.Fatal("reconciliation deleted the user-owned mixed hook group")
	}
	if !slices.Equal(foreignCommands, []string{"foreign-before", "foreign-after", "foreign-later"}) {
		t.Fatalf("foreign hook order = %v, want every user hook preserved", foreignCommands)
	}
	if compoundCommands != 1 {
		t.Fatalf("Compound PreToolUse commands = %d, want exactly one observer", compoundCommands)
	}
}

func TestCodexInstallCollapsesAllDuplicateLegacyPreToolUseHooks(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{
  "hooks": {
    "PreToolUse": [
      {"matcher": "*", "hooks": [{"type": "command", "command": "'/old/compound' hook pre-tool-use --live --harness codex"}]},
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "foreign-first"}]},
      {"matcher": "*", "hooks": [{"type": "command", "command": "'/old/compound' hook pre-tool-use --live --harness codex --enforce"}]},
      {
        "matcher": "*",
        "hooks": [
          {"type": "command", "command": "foreign-second"},
          {"type": "command", "command": "'/older/compound' hook pre-tool-use --live --harness codex --enforce"}
        ]
      }
    ]
  }
}`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallHooks(base, "/bin/compound", "global", []string{"codex"}, false); err != nil {
		t.Fatal(err)
	}

	cfg := readJSON(t, path)
	var foreignCommands []string
	var compoundCommands []string
	for _, group := range hooksForEvent(t, cfg, "PreToolUse") {
		entry, _ := group.(map[string]any)
		inner, _ := entry["hooks"].([]any)
		for _, hook := range inner {
			hookMap, _ := hook.(map[string]any)
			command, _ := hookMap["command"].(string)
			switch {
			case strings.Contains(command, hookCommandMarker):
				compoundCommands = append(compoundCommands, command)
			case command != "":
				foreignCommands = append(foreignCommands, command)
			}
		}
	}
	wantObserver := HookCommandForEvent("/bin/compound", "codex", "PreToolUse")
	if !slices.Equal(compoundCommands, []string{wantObserver}) {
		t.Fatalf("Compound PreToolUse commands = %q, want one current observer", compoundCommands)
	}
	if !slices.Equal(foreignCommands, []string{"foreign-first", "foreign-second"}) {
		t.Fatalf("foreign hook order = %v, want every user hook preserved", foreignCommands)
	}
}

func TestIsolatedBlockingPreToolUseCommandMakesEnforcementExplicit(t *testing.T) {
	bindings, _, err := compiledManagedHookBindings("/bin/compound", claudeZaiHarness, "global", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range bindings {
		if binding.Event != "PreToolUse" {
			continue
		}
		if !strings.Contains(binding.Command, " --enforce") {
			t.Fatalf("blocking PreToolUse command = %q, want explicit --enforce", binding.Command)
		}
		return
	}
	t.Fatal("isolated Claude posture omitted PreToolUse")
}

func TestInstallerAcceptsOnlyExplicitInteractiveObservationalDefaults(t *testing.T) {
	observational := CompiledPosture{
		Intentions: []CompiledIntention{
			{Intention: IntentionPrepareContext, Disposition: DispositionNative, EnforcementStrength: "context"},
			{Intention: IntentionObserveAction, Disposition: DispositionNative, EnforcementStrength: "observe"},
			{Intention: IntentionObserveOutcome, Disposition: DispositionNative, EnforcementStrength: "observe"},
		},
	}
	for _, harnessName := range []string{"codex", "claude-code"} {
		observational.CompiledFrom.PostureID = "hpost_default_" + strings.ReplaceAll(harnessName, "-", "_")
		if err := validateInstallableDefaultPosture(harnessName, observational); err != nil {
			t.Fatalf("explicit %s observational default rejected: %v", harnessName, err)
		}
	}
	observational.CompiledFrom.PostureID = "hpost_default_claude_zai"
	if err := validateInstallableDefaultPosture(claudeZaiHarness, observational); err == nil {
		t.Fatal("isolated non-mutation-ready Claude posture was silently accepted")
	}
	downgraded := observational
	downgraded.Intentions = append(downgraded.Intentions, CompiledIntention{
		Intention: IntentionGuardMutation, Disposition: DispositionUnsupported,
	})
	if err := validateInstallableDefaultPosture("codex", downgraded); err == nil {
		t.Fatal("unsupported guard was mistaken for an interactive observational default")
	}
	fallback := observational
	fallback.Intentions = append([]CompiledIntention(nil), observational.Intentions...)
	fallback.Intentions[0].Disposition = DispositionFallback
	if err := validateInstallableDefaultPosture("codex", fallback); err == nil {
		t.Fatal("degraded action observation was mistaken for the native default")
	}
}

func TestClaudeZaiInstallBindingsComeFromSemanticPosture(t *testing.T) {
	t.Parallel()

	bindings, compiled, err := compiledManagedHookBindings("/fixture/compound", claudeZaiHarness, "global", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !compiled.MutationReady {
		t.Fatalf("attested claude-zai posture is not mutation ready: %+v", compiled)
	}
	gotEvents := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		gotEvents = append(gotEvents, binding.Event)
	}
	wantEvents := []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "SessionEnd"}
	if !slices.Equal(gotEvents, wantEvents) {
		t.Fatalf("installed native events = %v, want compiled %v", gotEvents, wantEvents)
	}
}

// TestClaudeMinimaxInstallBindingsComeFromSemanticPosture proves the raised claude_minimax
// registry floor (internal/harness) compiles to a mutation-ready posture with a blocking
// PreToolUse binding, the same way claude_zai does. Falsifier for work_769bf567f529 gate 3: at
// base, with every hook flag false, compiledManagedHookBindings refuses with "compiled posture is
// not mutation-ready" before this raise.
func TestClaudeMinimaxInstallBindingsComeFromSemanticPosture(t *testing.T) {
	t.Parallel()

	bindings, compiled, err := compiledManagedHookBindings("/fixture/compound", claudeMinimaxHarness, "global", nil)
	if err != nil {
		t.Fatalf("compiled posture for claude-minimax: %v", err)
	}
	if !compiled.MutationReady {
		t.Fatalf("attested claude-minimax posture is not mutation ready: %+v", compiled)
	}
	foundBlockingPreTool := false
	for _, binding := range bindings {
		if binding.Event == "PreToolUse" && binding.Blocking && strings.Contains(binding.Command, " --enforce") {
			foundBlockingPreTool = true
		}
	}
	if !foundBlockingPreTool {
		t.Fatalf("claude-minimax posture omitted a blocking PreToolUse binding: %+v", bindings)
	}
	gotEvents := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		gotEvents = append(gotEvents, binding.Event)
	}
	wantEvents := []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "SessionEnd"}
	if !slices.Equal(gotEvents, wantEvents) {
		t.Fatalf("installed native events = %v, want compiled %v", gotEvents, wantEvents)
	}
}

// TestClaudeMinimaxRegistryFloorIsLoadBearing proves the raised registry flags are load-bearing,
// not decorative: the real (raised) registry compiles a mutation-ready posture, and an in-test
// scratch copy with PreToolUseBlock lowered back to false must be refused by the same installer
// gate. Falsifier 5 for work_769bf567f529 gate 3. At base (registry unraised), the first
// assertion below fails because the real registry is not yet mutation-ready.
func TestClaudeMinimaxRegistryFloorIsLoadBearing(t *testing.T) {
	raised := harness.DefaultRegistry()
	compiled, err := CompileDefaultPosture(DefaultPostureRequest{
		Harness:         claudeMinimaxHarness,
		Registry:        raised,
		HandlerRef:      "compound",
		HandlerRevision: "binary:/fixture/compound",
		ConfigScope:     "profile",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateInstallableDefaultPosture(claudeMinimaxHarness, compiled); err != nil {
		t.Fatalf("raised claude_minimax registry must be mutation-ready: %v", err)
	}

	lowered := harness.DefaultRegistry()
	h, ok := lowered.Harnesses["claude_minimax"]
	if !ok {
		t.Fatal("claude_minimax not enrolled in registry")
	}
	h.Capabilities.PreToolUseBlock = false
	lowered.Harnesses["claude_minimax"] = h
	downCompiled, err := CompileDefaultPosture(DefaultPostureRequest{
		Harness:         claudeMinimaxHarness,
		Registry:        lowered,
		HandlerRef:      "compound",
		HandlerRevision: "binary:/fixture/compound",
		ConfigScope:     "profile",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateInstallableDefaultPosture(claudeMinimaxHarness, downCompiled); err == nil {
		t.Fatal("scratch-lowered PreToolUseBlock was silently accepted; registry floor is decorative")
	}
}

func TestInstallHooksNoClobber(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-existing config: unrelated top-level keys AND an unrelated user PreToolUse hook.
	existing := `{
  "permissions": {"allow": ["Bash(ls)"]},
  "hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "my-own-hook"}]}]}
}`
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallHooks(base, "/bin/compound", "global", []string{"claude-code"}, false); err != nil {
		t.Fatal(err)
	}
	got := readJSON(t, path)
	// Unrelated key preserved.
	if _, ok := got["permissions"]; !ok {
		t.Fatal("no-clobber broken: permissions key dropped")
	}
	// Both the user's hook AND compound's are present.
	pre := got["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(pre) != 2 {
		t.Fatalf("expected user hook + compound hook = 2 PreToolUse entries, got %d", len(pre))
	}
	foundUser, foundCompound := false, false
	for _, e := range pre {
		cmd := firstCommand(e)
		if cmd == "my-own-hook" {
			foundUser = true
		}
		if strings.Contains(cmd, hookCommandMarker) {
			foundCompound = true
		}
	}
	if !foundUser || !foundCompound {
		t.Fatalf("no-clobber broken: user=%v compound=%v", foundUser, foundCompound)
	}
}

func TestInstallHooksIdempotentAndUpdate(t *testing.T) {
	base := t.TempDir()
	if _, err := InstallHooks(base, "/bin/compound", "global", []string{"codex"}, false); err != nil {
		t.Fatal(err)
	}
	// Second identical install -> ok, no duplicate.
	rep, err := InstallHooks(base, "/bin/compound", "global", []string{"codex"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Entries[0].Action != "ok" {
		t.Fatalf("re-install action = %q, want ok", rep.Entries[0].Action)
	}
	// New binary path -> update in place (not a second entry).
	rep, err = InstallHooks(base, "/opt/compound", "global", []string{"codex"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Entries[0].Action != "updated" {
		t.Fatalf("moved-binary action = %q, want updated", rep.Entries[0].Action)
	}
	got := readJSON(t, filepath.Join(base, ".codex", "hooks.json"))
	sessionStart := got["hooks"].(map[string]any)["SessionStart"].([]any)
	if len(sessionStart) != 1 {
		t.Fatalf("update must replace in place, got %d entries", len(sessionStart))
	}
	if command := firstCommand(sessionStart[0]); command != HookCommandForEvent("/opt/compound", "codex", "SessionStart") {
		t.Fatalf("updated SessionStart command = %q", command)
	}
}

func TestInstallHooksCheckModeDoesNotWrite(t *testing.T) {
	base := t.TempDir()
	rep, err := InstallHooks(base, "/bin/compound", "global", []string{"claude-code"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Entries[0].Action != "absent" {
		t.Fatalf("check on a fresh base = %q, want absent", rep.Entries[0].Action)
	}
	if _, err := os.Stat(filepath.Join(base, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatal("check mode must not write the config")
	}
}

// TestInstallHooksCheckDetectsStaleUserPromptSubmitOnClaudeCode is the falsifier for
// burst-guarddrift-20260817. Commit 711ce64f (2026-08-07) removed UserPromptSubmit from
// claude-code's desired event set; pre-711ce64f binaries (and the settings.json they
// wrote) still carry a UserPromptSubmit compound hook. The doctor reports this as
// guard=drift for claude-code lanes; claude-zai lanes are unaffected because their
// desired event set is unchanged.
//
// This test pins the real defect: the check MUST read Action="drift" when the actual
// settings.json carries an event the current desired bindings no longer want. If a
// future change ever weakens the drift signal (e.g. silently reconciles extra compound
// events, demotes drift to ok, or stops comparing the event set), this test fails and
// the vanished symptom of "guard column is wrong" is caught before it can come back.
//
// Reference evidence: the burst-guarddrift-20260817 lane REPORT.md, kept as an
// untracked workspace artifact alongside this branch's worktree.
func TestInstallHooksCheckDetectsStaleUserPromptSubmitOnClaudeCode(t *testing.T) {
	base := t.TempDir()
	binary := "/opt/compound/compound"

	// Pre-populate settings.json with the OLD pre-711ce64f shape: claude-code desired
	// events were 5 (SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, SessionEnd).
	// The current main (18645ea7) wants 4 — UserPromptSubmit is gone. This is exactly the
	// shape an installed binary older than 711ce64f would have written.
	//
	// Commands are shell-quoted (HookCommandForEvent wraps the binary path in single
	// quotes via shellQuote) so they byte-equal the desired bindings the current main
	// computes; the only difference between actual and desired is the event set.
	settingsPath := filepath.Join(base, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	oldShape := `{
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "'` + binary + `' hook session-start --live --harness claude_code"}]}
    ],
    "UserPromptSubmit": [
      {"hooks": [{"type": "command", "command": "'` + binary + `' hook user-prompt-submit --live --harness claude_code"}]}
    ],
    "PreToolUse": [
      {"matcher": "*", "hooks": [{"type": "command", "command": "'` + binary + `' hook pre-tool-use --live --harness claude_code"}]}
    ],
    "PostToolUse": [
      {"matcher": "*", "hooks": [{"type": "command", "command": "'` + binary + `' hook post-tool-use --live --harness claude_code"}]}
    ],
    "SessionEnd": [
      {"hooks": [{"type": "command", "command": "'` + binary + `' hook session-end-adapter --live --harness claude_code"}]}
    ]
  }
}`
	if err := os.WriteFile(settingsPath, []byte(oldShape), 0o644); err != nil {
		t.Fatal(err)
	}

	// Check mode (writes nothing). The desired bindings from the current main will be
	// 4 events for claude-code; the actual has 5. The check must report drift.
	rep, err := InstallHooks(base, binary, "global", []string{"claude-code"}, true)
	if err != nil {
		t.Fatalf("check returned err: %v", err)
	}
	if len(rep.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d: %+v", len(rep.Entries), rep.Entries)
	}
	// The honest reading. If this ever flips to "ok", the drift signal is broken and
	// the burst-guarddrift-20260817 symptom returns silently.
	if rep.Entries[0].Action != "drift" {
		t.Fatalf("stale UserPromptSubmit must read drift, got %q (action=%q binding shape on disk does not match current main's desired event set; check signal is broken)",
			rep.Entries[0].Action, rep.Entries[0].Action)
	}

	// Check mode must still be read-only: a previous regression here would mask the
	// drift by silently rewriting the settings.json to the new shape.
	if _, err := os.Stat(settingsPath); err != nil {
		t.Fatalf("check mode must not delete the file: %v", err)
	}
	got := readJSON(t, settingsPath)
	if _, ok := got["hooks"].(map[string]any)["UserPromptSubmit"]; !ok {
		t.Fatal("check mode must not modify the on-disk file: UserPromptSubmit was present before the check and must remain")
	}

	// Negative falsifier: the same shape with a different harness (claude-zai) where
	// the event set is unchanged must NOT read drift. This protects against a future
	// change that over-reports drift and false-positives the column.
	base2 := t.TempDir()
	settingsPath2 := filepath.Join(base2, ".config", "switchback", "claude", "zai-direct", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath2), 0o755); err != nil {
		t.Fatal(err)
	}
	zaiShape := `{
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "'` + binary + `' hook session-start --live --harness claude_zai"}]}
    ],
    "UserPromptSubmit": [
      {"hooks": [{"type": "command", "command": "'` + binary + `' hook user-prompt-submit --live --harness claude_zai"}]}
    ],
    "PreToolUse": [
      {"matcher": "*", "hooks": [{"type": "command", "command": "'` + binary + `' hook pre-tool-use --live --harness claude_zai --enforce"}]}
    ],
    "PostToolUse": [
      {"matcher": "*", "hooks": [{"type": "command", "command": "'` + binary + `' hook post-tool-use --live --harness claude_zai"}]}
    ],
    "SessionEnd": [
      {"hooks": [{"type": "command", "command": "'` + binary + `' hook session-end-adapter --live --harness claude_zai"}]}
    ]
  }
}`
	if err := os.WriteFile(settingsPath2, []byte(zaiShape), 0o644); err != nil {
		t.Fatal(err)
	}
	rep2, err := InstallHooks(base2, binary, "global", []string{"claude-zai"}, true)
	if err != nil {
		t.Fatalf("claude-zai check returned err: %v", err)
	}
	if rep2.Entries[0].Action == "drift" {
		t.Fatalf("claude-zai shape with UserPromptSubmit is the CURRENT desired set and must NOT read drift; got %q (a change has falsely demoted claude-zai's UserPromptSubmit from desired)",
			rep2.Entries[0].Action)
	}
}

func TestInstallHooksProjectScope(t *testing.T) {
	base := t.TempDir()
	if _, err := InstallHooks(base, "/bin/compound", "project", []string{"claude-code"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, ".claude", claudeSettingsFile("project"))); err != nil {
		t.Fatalf("project scope must write the local settings overlay: %v", err)
	}
}

func TestInstallClaudeZaiProfileFullRosterNoClobberAndIdempotent(t *testing.T) {
	base := t.TempDir()
	profileDir := filepath.Join(base, "zai-profile")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(profileDir, "settings.json")
	existing := `{
  "permissions": {"defaultMode": "bypassPermissions"},
  "hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "my-own-hook"}]}]}
}`
	if err := os.WriteFile(settingsPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	first, err := InstallHooksWithConfigDir(base, "/opt/Compound Apps/compound", "global", []string{"claude-zai"}, profileDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Entries) != 1 || first.Entries[0].Harness != "claude-zai" || first.Entries[0].Action != "installed" {
		t.Fatalf("first claude-zai install = %+v", first)
	}
	cfg := readJSON(t, settingsPath)
	if _, ok := cfg["permissions"]; !ok {
		t.Fatal("claude-zai install clobbered permissions")
	}
	hooks, ok := cfg["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("settings hooks = %#v", cfg["hooks"])
	}
	wantCommands := map[string]string{}
	for _, binding := range first.Entries[0].Bindings {
		wantCommands[binding.Event] = binding.Command
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "SessionEnd"} {
		groups, ok := hooks[event].([]any)
		if !ok || len(groups) == 0 {
			t.Fatalf("claude-zai settings missing %s: %#v", event, hooks[event])
		}
		wantCommand := wantCommands[event]
		found := false
		for _, group := range groups {
			if firstCommand(group) == wantCommand {
				found = true
			}
		}
		if !found {
			t.Fatalf("claude-zai %s missing command %q: %#v", event, wantCommand, groups)
		}
	}
	if got := matcherForEvent(t, hooks, "PreToolUse"); got != "*" {
		t.Fatalf("claude-zai PreToolUse matcher = %q", got)
	}
	if got := matcherForEvent(t, hooks, "PostToolUse"); got != "*" {
		t.Fatalf("claude-zai PostToolUse matcher = %q", got)
	}

	second, err := InstallHooksWithConfigDir(base, "/opt/Compound Apps/compound", "global", []string{"claude-zai"}, profileDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if second.Entries[0].Action != "ok" {
		t.Fatalf("idempotent claude-zai install action = %q, want ok", second.Entries[0].Action)
	}
	if got := len(hooksForEvent(t, readJSON(t, settingsPath), "SessionStart")); got != 2 {
		t.Fatalf("idempotent install duplicated SessionStart hooks: got %d groups, want user + Compound", got)
	}
}

// TestInstallHooksClaudeMinimaxProfile is falsifier 1 for work_769bf567f529 gate 3: installing
// with --harness claude-minimax into a temp isolated profile dir writes all 5 lifecycle bindings,
// the PreToolUse binding is a blocking --enforce command, and a pre-seeded permissions+env key
// survives untouched (the NO-CLOBBER law). At base, --harness claude-minimax is rejected by the
// --config-dir gate before any write.
func TestInstallHooksClaudeMinimaxProfile(t *testing.T) {
	base := t.TempDir()
	profileDir := filepath.Join(base, "minimax-profile")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(profileDir, "settings.json")
	existing := `{
  "permissions": {"defaultMode": "bypassPermissions"},
  "env": {"ANTHROPIC_BASE_URL": "https://minimax.example.invalid"},
  "hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "my-own-hook"}]}]}
}`
	if err := os.WriteFile(settingsPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	first, err := InstallHooksWithConfigDir(base, "/opt/Compound Apps/compound", "global", []string{"claude-minimax"}, profileDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Entries) != 1 || first.Entries[0].Harness != "claude-minimax" || first.Entries[0].Action != "installed" {
		t.Fatalf("first claude-minimax install = %+v", first)
	}
	if first.Entries[0].ConfigPath != settingsPath {
		t.Fatalf("claude-minimax ConfigPath = %q, want %q", first.Entries[0].ConfigPath, settingsPath)
	}
	cfg := readJSON(t, settingsPath)
	if _, ok := cfg["permissions"]; !ok {
		t.Fatal("claude-minimax install clobbered permissions")
	}
	env, ok := cfg["env"].(map[string]any)
	if !ok || env["ANTHROPIC_BASE_URL"] != "https://minimax.example.invalid" {
		t.Fatalf("claude-minimax install clobbered env: %#v", cfg["env"])
	}
	hooks, ok := cfg["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("settings hooks = %#v", cfg["hooks"])
	}
	if len(first.Entries[0].Bindings) != 5 {
		t.Fatalf("claude-minimax install reported %d bindings, want 5", len(first.Entries[0].Bindings))
	}
	wantCommands := map[string]string{}
	for _, binding := range first.Entries[0].Bindings {
		wantCommands[binding.Event] = binding.Command
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "SessionEnd"} {
		groups, ok := hooks[event].([]any)
		if !ok || len(groups) == 0 {
			t.Fatalf("claude-minimax settings missing %s: %#v", event, hooks[event])
		}
		wantCommand := wantCommands[event]
		found := false
		for _, group := range groups {
			if firstCommand(group) == wantCommand {
				found = true
			}
		}
		if !found {
			t.Fatalf("claude-minimax %s missing command %q: %#v", event, wantCommand, groups)
		}
	}
	preToolCommand := wantCommands["PreToolUse"]
	if !strings.HasSuffix(preToolCommand, " --enforce") {
		t.Fatalf("claude-minimax PreToolUse command = %q, want --enforce suffix", preToolCommand)
	}
	preGroups, _ := hooks["PreToolUse"].([]any)
	var preMatcher string
	for _, group := range preGroups {
		if strings.Contains(firstCommand(group), "--harness claude_minimax") {
			entry, _ := group.(map[string]any)
			preMatcher, _ = entry["matcher"].(string)
		}
	}
	if preMatcher != "*" {
		t.Fatalf("claude-minimax PreToolUse matcher = %q", preMatcher)
	}
	if first.Entries[0].Matcher != "*" || !strings.Contains(first.Entries[0].Command, " --enforce") {
		t.Fatalf("claude-minimax guard projection = %+v", first.Entries[0])
	}

	second, err := InstallHooksWithConfigDir(base, "/opt/Compound Apps/compound", "global", []string{"claude-minimax"}, profileDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if second.Entries[0].Action != "ok" {
		t.Fatalf("idempotent claude-minimax install action = %q, want ok", second.Entries[0].Action)
	}
	if got := len(hooksForEvent(t, readJSON(t, settingsPath), "SessionStart")); got != 2 {
		t.Fatalf("idempotent install duplicated SessionStart hooks: got %d groups, want user + Compound", got)
	}
}

// TestInstallHooksClaudeQwenProfile is the qwen falsifier for
// work_qwen_harness_enrollment_20260731: installing with --harness claude-qwen
// into a temp isolated profile dir writes all 5 lifecycle bindings, the
// PreToolUse binding is a blocking --enforce command, and a pre-seeded
// permissions+env key survives untouched (the NO-CLOBBER law). At base,
// --harness claude-qwen is rejected by the --config-dir gate before any write.
func TestInstallHooksClaudeQwenProfile(t *testing.T) {
	base := t.TempDir()
	profileDir := filepath.Join(base, "qwen-profile")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(profileDir, "settings.json")
	existing := `{
  "permissions": {"defaultMode": "bypassPermissions"},
  "env": {"ANTHROPIC_BASE_URL": "https://qwen.example.invalid"},
  "hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "my-own-hook"}]}]}
}`
	if err := os.WriteFile(settingsPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	first, err := InstallHooksWithConfigDir(base, "/opt/Compound Apps/compound", "global", []string{"claude-qwen"}, profileDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Entries) != 1 || first.Entries[0].Harness != "claude-qwen" || first.Entries[0].Action != "installed" {
		t.Fatalf("first claude-qwen install = %+v", first)
	}
	if first.Entries[0].ConfigPath != settingsPath {
		t.Fatalf("claude-qwen ConfigPath = %q, want %q", first.Entries[0].ConfigPath, settingsPath)
	}
	cfg := readJSON(t, settingsPath)
	if _, ok := cfg["permissions"]; !ok {
		t.Fatal("claude-qwen install clobbered permissions")
	}
	env, ok := cfg["env"].(map[string]any)
	if !ok || env["ANTHROPIC_BASE_URL"] != "https://qwen.example.invalid" {
		t.Fatalf("claude-qwen install clobbered env: %#v", cfg["env"])
	}
	hooks, ok := cfg["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("settings hooks = %#v", cfg["hooks"])
	}
	if len(first.Entries[0].Bindings) != 5 {
		t.Fatalf("claude-qwen install reported %d bindings, want 5", len(first.Entries[0].Bindings))
	}
	wantCommands := map[string]string{}
	for _, binding := range first.Entries[0].Bindings {
		wantCommands[binding.Event] = binding.Command
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "SessionEnd"} {
		groups, ok := hooks[event].([]any)
		if !ok || len(groups) == 0 {
			t.Fatalf("claude-qwen settings missing %s: %#v", event, hooks[event])
		}
		wantCommand := wantCommands[event]
		found := false
		for _, group := range groups {
			if firstCommand(group) == wantCommand {
				found = true
			}
		}
		if !found {
			t.Fatalf("claude-qwen %s missing command %q: %#v", event, wantCommand, groups)
		}
	}
	preToolCommand := wantCommands["PreToolUse"]
	if !strings.HasSuffix(preToolCommand, " --enforce") {
		t.Fatalf("claude-qwen PreToolUse command = %q, want --enforce suffix", preToolCommand)
	}
	preGroups, _ := hooks["PreToolUse"].([]any)
	var preMatcher string
	for _, group := range preGroups {
		if strings.Contains(firstCommand(group), "--harness claude_qwen") {
			entry, _ := group.(map[string]any)
			preMatcher, _ = entry["matcher"].(string)
		}
	}
	if preMatcher != "*" {
		t.Fatalf("claude-qwen PreToolUse matcher = %q", preMatcher)
	}
	if first.Entries[0].Matcher != "*" || !strings.Contains(first.Entries[0].Command, " --enforce") {
		t.Fatalf("claude-qwen guard projection = %+v", first.Entries[0])
	}

	second, err := InstallHooksWithConfigDir(base, "/opt/Compound Apps/compound", "global", []string{"claude-qwen"}, profileDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if second.Entries[0].Action != "ok" {
		t.Fatalf("idempotent claude-qwen install action = %q, want ok", second.Entries[0].Action)
	}
	if got := len(hooksForEvent(t, readJSON(t, settingsPath), "SessionStart")); got != 2 {
		t.Fatalf("idempotent install duplicated SessionStart hooks: got %d groups, want user + Compound", got)
	}

	// The --config-dir gate must accept claude-qwen alone, same carve-out as
	// claude-zai and claude-minimax.
	qwenDir := t.TempDir()
	if _, err := InstallHooksWithConfigDir(base, "/bin/compound", "global", []string{"claude-qwen"}, qwenDir, false); err != nil {
		t.Fatalf("--config-dir with claude-qwen alone must be accepted: %v", err)
	}
	if _, err := InstallHooksWithConfigDir(base, "/bin/compound", "global", []string{"claude-zai", "claude-qwen"}, qwenDir, false); err == nil {
		t.Fatal("--config-dir with two harnesses (zai+qwen) must be refused")
	}
}

// TestConfigDirGateAcceptsExactlyOneIsolatedProfileHarness is falsifier 3 for
// work_769bf567f529 gate 3: --config-dir is accepted with claude-minimax alone (mirroring the
// existing claude-zai carve-out), and still refused with two harnesses or with codex. At base,
// claude-minimax alone is refused because the gate names only claude-zai.
func TestConfigDirGateAcceptsExactlyOneIsolatedProfileHarness(t *testing.T) {
	base := t.TempDir()
	minimaxDir := t.TempDir()
	if _, err := InstallHooksWithConfigDir(base, "/bin/compound", "global", []string{"claude-minimax"}, minimaxDir, false); err != nil {
		t.Fatalf("--config-dir with claude-minimax alone must be accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(minimaxDir, "settings.json")); err != nil {
		t.Fatalf("claude-minimax config-dir install did not write: %v", err)
	}

	if _, err := InstallHooksWithConfigDir(base, "/bin/compound", "global", []string{"claude-zai", "claude-minimax"}, minimaxDir, false); err == nil {
		t.Fatal("--config-dir with two harnesses must be refused")
	}
	if _, err := InstallHooksWithConfigDir(base, "/bin/compound", "global", []string{"codex"}, minimaxDir, false); err == nil {
		t.Fatal("--config-dir with codex must be refused")
	}
}

func hookInstallEntryForHarness(t *testing.T, report HookInstallReport, harnessName string) HookInstallEntry {
	t.Helper()
	for _, entry := range report.Entries {
		if entry.Harness == harnessName {
			return entry
		}
	}
	t.Fatalf("install report omitted harness %q: %+v", harnessName, report.Entries)
	return HookInstallEntry{}
}

func matcherForEvent(t *testing.T, hooks map[string]any, event string) string {
	t.Helper()
	groups, ok := hooks[event].([]any)
	if !ok {
		t.Fatalf("%s groups = %#v", event, hooks[event])
	}
	for _, group := range groups {
		entry, _ := group.(map[string]any)
		if strings.Contains(firstCommand(group), "--harness claude_zai") {
			matcher, _ := entry["matcher"].(string)
			return matcher
		}
	}
	return ""
}

func hooksForEvent(t *testing.T, cfg map[string]any, event string) []any {
	t.Helper()
	hooks, ok := cfg["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("settings hooks = %#v", cfg["hooks"])
	}
	groups, ok := hooks[event].([]any)
	if !ok {
		t.Fatalf("%s groups = %#v", event, hooks[event])
	}
	return groups
}

func firstMatcher(t *testing.T, cfg map[string]any) string {
	t.Helper()
	pre := cfg["hooks"].(map[string]any)["PreToolUse"].([]any)
	return pre[0].(map[string]any)["matcher"].(string)
}

func firstCommand(entry any) string {
	em, ok := entry.(map[string]any)
	if !ok {
		return ""
	}
	inner, _ := em["hooks"].([]any)
	for _, h := range inner {
		if hm, ok := h.(map[string]any); ok {
			if cmd, _ := hm["command"].(string); cmd != "" {
				return cmd
			}
		}
	}
	return ""
}

func hasHookCommand(cfg map[string]any, event, command string) bool {
	hooks, _ := cfg["hooks"].(map[string]any)
	groups, _ := hooks[event].([]any)
	for _, group := range groups {
		if firstCommand(group) == command {
			return true
		}
	}
	return false
}

// findCompoundHookGroup returns the first hook group whose entry-level matcher
// (when present) matches the compound matcher "*" and whose inner hook command
// contains the live compound marker. It returns the group map and true on a
// match, or nil/false when no such group is present. The red test uses it to
// inspect the timeout field at the group level after a re-install.
func findCompoundHookGroup(cfg map[string]any, event string) (map[string]any, bool) {
	hooks, _ := cfg["hooks"].(map[string]any)
	groups, _ := hooks[event].([]any)
	for _, group := range groups {
		entry, ok := group.(map[string]any)
		if !ok {
			continue
		}
		inner, _ := entry["hooks"].([]any)
		if len(inner) == 0 {
			continue
		}
		first, ok := inner[0].(map[string]any)
		if !ok {
			continue
		}
		cmd, _ := first["command"].(string)
		if !strings.Contains(cmd, hookCommandMarker) {
			continue
		}
		return entry, true
	}
	return nil, false
}

// TestReinstallPreservesHandSetTimeoutOnCompoundBinding is the falsifier for
// the per-binding timeout regression. A user (or an operator) hand-sets a
// `timeout` integer (seconds) on the live Compound binding in settings.json;
// `compound hook install` re-runs and the installer replaces Compound-owned
// entries wholesale. The timeout must SURVIVE the wholesale replace because
// timeout containment is a known hook-cost blowup we explicitly want to keep
// in place. The non-Compound hooks and every other settings key stay
// byte-preserved (existing no-clobber invariant).
func TestReinstallPreservesHandSetTimeoutOnCompoundBinding(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Hand-set state: a Compound binding (live PreToolUse marker) with the
	// harness-schema sibling `timeout` integer (seconds); an unrelated
	// user-owned hook; an unrelated top-level key (`permissions`) that the
	// no-clobber invariant must preserve.
	existing := `{
  "permissions": {"allow": ["Bash(ls)"]},
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "*",
        "timeout": 30,
        "hooks": [
          {"type": "command", "command": "'/old/compound' hook pre-tool-use --live --harness claude-code --enforce"}
        ]
      },
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "my-own-hook"}]}
    ]
  }
}`
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	// Reinstall: the installer identifies Compound's own entry by the live
	// marker and replaces it wholesale. The hand-set timeout must persist on
	// the replacement entry.
	rep, err := InstallHooks(base, "/bin/compound", "global", []string{"claude-code"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Entries[0].Action != "updated" {
		t.Fatalf("reinstall action = %q, want updated (Compound entry was replaced in place)", rep.Entries[0].Action)
	}

	got := readJSON(t, path)
	// Existing no-clobber invariant: unrelated top-level key preserved.
	if _, ok := got["permissions"]; !ok {
		t.Fatalf("no-clobber broken: permissions key dropped (top-level byte preservation)")
	}
	// Non-Compound hook survives.
	preGroups := got["hooks"].(map[string]any)["PreToolUse"].([]any)
	foundUser := false
	for _, group := range preGroups {
		entry, _ := group.(map[string]any)
		if firstCommand(entry) == "my-own-hook" {
			foundUser = true
		}
	}
	if !foundUser {
		t.Fatalf("no-clobber broken: user hook dropped from PreToolUse: %+v", preGroups)
	}

	// The core falsifier: the Compound binding must still carry the timeout
	// integer (seconds) after reinstall. The harness hook JSON schema allows
	// a sibling `timeout` on the hook entry; losing it on reinstall is a
	// known hook-cost blowup.
	entry, ok := findCompoundHookGroup(got, "PreToolUse")
	if !ok {
		t.Fatalf("reinstall dropped the Compound binding entirely: %+v", preGroups)
	}
	timeout, ok := entry["timeout"]
	if !ok {
		t.Fatalf("reinstall lost the hand-set timeout on the Compound binding: entry=%+v", entry)
	}
	// JSON numbers decode to float64; an integer timeout is the schema.
	asFloat, ok := timeout.(float64)
	if !ok {
		t.Fatalf("Compound binding timeout = %T(%v), want integer seconds", timeout, timeout)
	}
	if asFloat != 30 {
		t.Fatalf("Compound binding timeout = %v, want 30 (seconds)", asFloat)
	}
	// The new Compound command must reflect the current binary path so the
	// entry is genuinely replaced, not just left as-is.
	inner, _ := entry["hooks"].([]any)
	first, _ := inner[0].(map[string]any)
	cmd, _ := first["command"].(string)
	if cmd != HookCommandForEvent("/bin/compound", "claude-code", "PreToolUse") {
		t.Fatalf("replaced Compound command = %q, want current binary", cmd)
	}
}
