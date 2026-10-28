package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/home"
)

func writeLaneManifest(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lane.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimaxLaneManifest = `lane: minimax
harness: claude-code
model: minimax/MiniMax-M3
effort: xhigh
capture: segmented_full_wire
permission: bypass
api:
  enabled: true
  route: minimax/*
hooks:
  roster: minimal
`

func TestCmdHookInstallLaneFileDerivesHarnessAndRoster(t *testing.T) {
	compoundHome := t.TempDir()
	base := t.TempDir()
	profileDir := filepath.Join(t.TempDir(), "minimax-lane")
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
	if err := os.WriteFile(settingsPath, []byte("{\n  \"model\": \"minimax/MiniMax-M3\",\n  \"effortLevel\": \"xhigh\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", compoundHome)
	t.Setenv("HOME", t.TempDir())
	manifestPath := writeLaneManifest(t, minimaxLaneManifest)

	out := captureStdout(t, func() {
		cmdHookInstall([]string{
			"--lane-file", manifestPath,
			"--config-dir", profileDir,
			"--scope", "global",
			"--base", base,
			"--binary", binaryPath,
			"--json",
		})
	})
	var report struct {
		Entries []struct {
			Harness string `json:"harness"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode hook install report: %v\n%s", err, out)
	}
	if len(report.Entries) != 1 || report.Entries[0].Harness != "claude-minimax" {
		t.Fatalf("lane-file report harness = %+v, want claude-minimax derived from the manifest", report.Entries)
	}
	settings := readCLISettings(t, settingsPath)
	if settings["model"] != "minimax/MiniMax-M3" || settings["effortLevel"] != "xhigh" {
		t.Fatalf("lane-file install clobbered switchback profile identity: %+v", settings)
	}
	groups, _ := settings["hooks"].(map[string]any)
	if len(groups) != 2 {
		t.Fatalf("minimal roster installed %d events, want 2: %v", len(groups), groups)
	}
	if _, ok := groups["PostToolUse"]; ok {
		t.Fatal("minimal roster must not install PostToolUse")
	}
}

func TestCmdHookInstallLaneFileRosterFlagOverridesManifest(t *testing.T) {
	compoundHome := t.TempDir()
	base := t.TempDir()
	profileDir := filepath.Join(t.TempDir(), "minimax-lane")
	binaryPath := filepath.Join(base, "bin", "compound")
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryPath, []byte("synthetic compound binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", compoundHome)
	t.Setenv("HOME", t.TempDir())
	manifestPath := writeLaneManifest(t, minimaxLaneManifest)

	_ = captureStdout(t, func() {
		cmdHookInstall([]string{
			"--lane-file", manifestPath,
			"--config-dir", profileDir,
			"--scope", "global",
			"--base", base,
			"--binary", binaryPath,
			"--roster", "full",
			"--json",
		})
	})
	settings := readCLISettings(t, filepath.Join(profileDir, "settings.json"))
	groups, _ := settings["hooks"].(map[string]any)
	if len(groups) != 5 {
		t.Fatalf("--roster full installed %d events, want 5: %v", len(groups), groups)
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "SessionEnd"} {
		if _, ok := groups[event]; !ok {
			t.Fatalf("--roster full is missing event %s: %v", event, groups)
		}
	}
}

func TestCmdHookInstallLaneFileCheckReportsRosterLossWithoutWriting(t *testing.T) {
	// Two-writers regression at the CLI seam: switchback regen overwrites the
	// profile with no hooks; --check must report the loss and write nothing.
	compoundHome := t.TempDir()
	base := t.TempDir()
	profileDir := filepath.Join(t.TempDir(), "minimax-lane")
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
	regen := "{\n  \"model\": \"minimax/MiniMax-M3\"\n}\n"
	if err := os.WriteFile(settingsPath, []byte(regen), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", compoundHome)
	t.Setenv("HOME", t.TempDir())
	manifestPath := writeLaneManifest(t, minimaxLaneManifest)

	out := captureStdout(t, func() {
		cmdHookInstall([]string{
			"--lane-file", manifestPath,
			"--config-dir", profileDir,
			"--scope", "global",
			"--base", base,
			"--binary", binaryPath,
			"--check",
			"--json",
		})
	})
	var report struct {
		Entries []struct {
			Action string `json:"action"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode check report: %v\n%s", err, out)
	}
	if report.Entries[0].Action == "ok" {
		t.Fatal("--check reported ok after switchback regen dropped the roster")
	}
	if got, _ := os.ReadFile(settingsPath); string(got) != regen {
		t.Fatalf("--check mutated the config: %s", got)
	}
}

func TestCmdHookInstallLaneFileRejectsHarnessConflict(t *testing.T) {
	// The subprocess runs the real CLI dispatch, whose governed-command
	// admission requires a compatible home manifest — a bare $HOME fails
	// before flag validation (fresh checkouts, CI runners, containers).
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", homePath)
	manifestPath := writeLaneManifest(t, minimaxLaneManifest)
	repoRoot, _ := filepath.Abs(filepath.Join("..", ".."))
	cmd := exec.Command(compoundTestBinary(t), "hook", "install",
		"--lane-file", manifestPath, "--harness", "codex")
	cmd.Dir = repoRoot
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Fatalf("expected --lane-file/--harness conflict to fail, output:\n%s", out)
	}
	if !strings.Contains(string(out), "mutually exclusive") {
		t.Fatalf("conflict error is not actionable:\n%s", out)
	}
}

func TestCmdHookInstallLaneFileRejectsUnsupportedHarness(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", homePath)
	manifestPath := writeLaneManifest(t, "lane: opencode-oss\nharness: opencode\n")
	repoRoot, _ := filepath.Abs(filepath.Join("..", ".."))
	cmd := exec.Command(compoundTestBinary(t), "hook", "install",
		"--lane-file", manifestPath, "--config-dir", t.TempDir())
	cmd.Dir = repoRoot
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Fatalf("expected opencode manifest to fail, output:\n%s", out)
	}
	if !strings.Contains(string(out), "opencode") {
		t.Fatalf("opencode error is not actionable:\n%s", out)
	}
}

func readCLISettings(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatalf("parse settings %s: %v\n%s", path, err, body)
	}
	return settings
}
