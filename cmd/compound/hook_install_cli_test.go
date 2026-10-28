package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCmdHookInstallHelpDoesNotMutateHarnessConfig(t *testing.T) {
	base := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdHookInstall([]string{
			"--help",
			"--base", base,
			"--binary", binary,
			"--harness", "codex",
		})
	})

	if !strings.Contains(out, "usage: compound hook install") {
		t.Fatalf("help output missing usage: %s", out)
	}
	if _, err := os.Stat(filepath.Join(base, ".codex", "hooks.json")); !os.IsNotExist(err) {
		t.Fatalf("help mutated Codex hooks config: %v", err)
	}
}

func TestCmdHookInstallEnrollsGenericClaudeProfileConfigDir(t *testing.T) {
	compoundHome := t.TempDir()
	base := t.TempDir()
	profileDir := filepath.Join(t.TempDir(), "gpt-profile")
	settingsPath := filepath.Join(profileDir, "settings.json")
	binaryPath := filepath.Join(base, "bin", "compound")
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryPath, []byte("synthetic compound binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte("{\n  \"model\": \"fixture/gpt\",\n  \"effortLevel\": \"xhigh\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", compoundHome)
	t.Setenv("HOME", t.TempDir())

	out := captureStdout(t, func() {
		cmdHookInstall([]string{
			"--harness", "claude-code",
			"--config-dir", profileDir,
			"--scope", "global",
			"--base", base,
			"--binary", binaryPath,
			"--json",
		})
	})
	var report struct {
		Entries []struct {
			Harness    string `json:"harness"`
			ConfigPath string `json:"config_path"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode hook install report: %v\n%s", err, out)
	}
	if len(report.Entries) != 1 || report.Entries[0].Harness != "claude-code" || report.Entries[0].ConfigPath != settingsPath {
		t.Fatalf("generic profile enrollment report = %+v", report)
	}
	b, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(b, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["model"] != "fixture/gpt" || settings["effortLevel"] != "xhigh" {
		t.Fatalf("CLI enrollment clobbered Switchback profile identity: %+v", settings)
	}
	if !strings.Contains(string(b), "--harness claude_code") {
		t.Fatalf("CLI enrollment emitted a noncanonical runtime harness:\n%s", b)
	}
}
