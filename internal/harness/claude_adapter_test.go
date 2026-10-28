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

// fakeBinaryPath creates a temp directory containing a shell script named
// `name` (e.g. "claude") that writes its argv to argvLogPath and exits
// with exitCode. The returned path is suitable for prepending to PATH so
// exec.CommandContext finds the fake instead of the real binary.
//
// This pattern is the standard "mock a subprocess via PATH" idiom. The
// fake uses #!/bin/sh so it works on Linux and macOS; CI on Windows is
// out of scope for the harness adapter (the OSEntrypointsAdapter
// doesn't claim Windows support).
func fakeBinaryPath(t *testing.T, name, argvLogPath string, exitCode int) string {
	t.Helper()
	binDir := t.TempDir()
	script := `#!/bin/sh
echo "$@" >> ` + argvLogPath + `
exit ` + intToStr(exitCode) + `
`
	scriptPath := filepath.Join(binDir, name)
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	return binDir
}

func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	if n < 0 {
		return "-" + intToStr(-n)
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

// withPATH prepends dir to PATH for the duration of t.
func withPATH(t *testing.T, dir string) {
	t.Helper()
	old := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+old)
}

// TestClaudeAdapterHappyPath: a fake `claude` script on PATH is invoked
// with --print --permission-mode plan <prompt> -- the safe default posture
// (bypass is opt-in via --permission-mode bypass). The RunAttempt is
// succeeded, harness="claude", command="claude", and the argv log contains
// the expected flags.
func TestClaudeAdapterHappyPath(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	binDir := fakeBinaryPath(t, "claude", argvLog, 0)
	withPATH(t, binDir)

	home := t.TempDir()
	store := runattempt.Open(home)
	a := NewClaudeAdapter(store)

	out, err := a.Run(context.Background(), RunInput{
		WorkID:    "work_claude",
		Harness:   "claude",
		Workspace: home,
		Prompt:    "hello from claude",
		Now:       time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.RunAttempt.Status != runattempt.StatusSucceeded {
		t.Fatalf("status = %q, want succeeded", out.RunAttempt.Status)
	}
	if out.RunAttempt.Harness != "claude" {
		t.Fatalf("harness = %q, want %q", out.RunAttempt.Harness, "claude")
	}
	if out.RunAttempt.Command != "claude" {
		t.Fatalf("command = %q, want %q (adapter overrides Command to its Name)", out.RunAttempt.Command, "claude")
	}
	if out.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", out.ExitCode)
	}
	// Verify the argv log has the expected flags + prompt.
	argvBytes, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	argv := strings.TrimSpace(string(argvBytes))
	if !strings.Contains(argv, "--print") {
		t.Fatalf("argv log missing --print: %q", argv)
	}
	if !strings.Contains(argv, "--permission-mode plan") {
		t.Fatalf("argv log missing --permission-mode plan (safe default): %q", argv)
	}
	if strings.Contains(argv, "--dangerously-skip-permissions") {
		t.Fatalf("default run must NOT bypass permissions (bypass is opt-in): %q", argv)
	}
	if !strings.Contains(argv, "hello from claude") {
		t.Fatalf("argv log missing prompt: %q", argv)
	}
}

// TestClaudeAdapterFailedExit: a fake `claude` script that exits 1
// produces a failed RunAttempt with exit code 1.
func TestClaudeAdapterFailedExit(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	binDir := fakeBinaryPath(t, "claude", argvLog, 1)
	withPATH(t, binDir)

	home := t.TempDir()
	store := runattempt.Open(home)
	a := NewClaudeAdapter(store)

	out, err := a.Run(context.Background(), RunInput{
		WorkID:    "work_claude_fail",
		Harness:   "claude",
		Workspace: home,
		Prompt:    "this will fail",
		Now:       time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.RunAttempt.Status != runattempt.StatusFailed {
		t.Fatalf("status = %q, want failed (claude exits 1)", out.RunAttempt.Status)
	}
	if out.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", out.ExitCode)
	}
}

// TestClaudeAdapterPermissionMode: the argv reflects RunInput.PermissionMode.
// Default ("") is the safe "plan" posture (read-only, does not hang a
// non-interactive --print run); an explicit named mode swaps in
// --permission-mode <mode>; "bypass" is the explicit opt-in for
// --dangerously-skip-permissions. Pinned: the posture must be explicit and
// auditable. Unknown modes are covered separately by
// TestClaudeAdapterPermissionModeUnknownFailsClosed (F7: reject before
// mint, don't silently fall back) -- the old "unknown_falls_back_to_plan"
// case pinned the wrong behavior and was removed here (zip27 audit F7).
func TestClaudeAdapterPermissionMode(t *testing.T) {
	cases := []struct {
		name string
		mode string
		want string
		ban  string // substring that must NOT appear
	}{
		{"empty_defaults_to_plan", "", "--permission-mode plan", "--dangerously-skip-permissions"},
		{"explicit_plan", "plan", "--permission-mode plan", "--dangerously-skip-permissions"},
		{"explicit_default", "default", "--permission-mode default", "--dangerously-skip-permissions"},
		{"explicit_acceptEdits", "acceptEdits", "--permission-mode acceptEdits", "--dangerously-skip-permissions"},
		{"explicit_bypass_opt_in", "bypass", "--dangerously-skip-permissions", "--permission-mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argvLog := filepath.Join(t.TempDir(), "argv.log")
			binDir := fakeBinaryPath(t, "claude", argvLog, 0)
			withPATH(t, binDir)

			home := t.TempDir()
			store := runattempt.Open(home)
			a := NewClaudeAdapter(store)

			_, err := a.Run(context.Background(), RunInput{
				WorkID:         "work_permode",
				Harness:        "claude",
				Workspace:      home,
				Prompt:         "hi",
				PermissionMode: tc.mode,
				Now:            time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			argvBytes, _ := os.ReadFile(argvLog)
			argv := string(argvBytes)
			if !strings.Contains(argv, tc.want) {
				t.Fatalf("mode %q: argv missing %q; got: %s", tc.mode, tc.want, argv)
			}
			if tc.ban != "" && strings.Contains(argv, tc.ban) {
				t.Fatalf("mode %q: argv must not contain %q; got: %s", tc.mode, tc.ban, argv)
			}
		})
	}
}

// TestClaudeAdapterPermissionModeUnknownFailsClosed: F7 -- an unrecognized
// permission mode must be rejected BEFORE the RunAttempt is minted, not
// silently downgraded to "plan". Falsifier: the prior behavior (see the
// removed "unknown_falls_back_to_plan" case above) silently fell back to
// plan and still minted+ran the RunAttempt; this pins that a typo produces
// an error naming the bad value and mints ZERO ratt_ files (zip27 audit
// round F7).
func TestClaudeAdapterPermissionModeUnknownFailsClosed(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	binDir := fakeBinaryPath(t, "claude", argvLog, 0)
	withPATH(t, binDir)

	home := t.TempDir()
	store := runattempt.Open(home)
	a := NewClaudeAdapter(store)

	_, err := a.Run(context.Background(), RunInput{
		WorkID:         "work_permode_bad",
		Harness:        "claude",
		Workspace:      home,
		Prompt:         "hi",
		PermissionMode: "bogus-mode",
		Now:            time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
	})
	if err == nil {
		t.Fatalf("expected an error for unknown permission mode %q, got nil", "bogus-mode")
	}
	if !strings.Contains(err.Error(), "bogus-mode") {
		t.Fatalf("error must name the bad value %q, got: %v", "bogus-mode", err)
	}
	runAttemptFiles, _ := filepath.Glob(filepath.Join(home, "run_attempts", "ratt_*.json"))
	if len(runAttemptFiles) != 0 {
		t.Fatalf("unknown permission mode must mint ZERO RunAttempts, got %d", len(runAttemptFiles))
	}
}

// TestClaudeAdapterName: trivial contract pin. Pinned because the
// Registry slice (C.3) keys on this.
func TestClaudeAdapterName(t *testing.T) {
	a := NewClaudeAdapter(runattempt.Open(t.TempDir()))
	if a.Name() != "claude" {
		t.Fatalf("name = %q, want %q", a.Name(), "claude")
	}
}

// TestClaudeAdapterPromptFromArgs: when Prompt is empty, the adapter
// uses Args[0] as the prompt. Pinned: callers that already have argv
// prepared shouldn't have to duplicate into a Prompt field.
func TestClaudeAdapterPromptFromArgs(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	binDir := fakeBinaryPath(t, "claude", argvLog, 0)
	withPATH(t, binDir)

	home := t.TempDir()
	store := runattempt.Open(home)
	a := NewClaudeAdapter(store)

	_, err := a.Run(context.Background(), RunInput{
		WorkID:    "work_claude_args",
		Harness:   "claude",
		Workspace: home,
		Args:      []string{"prompt from args"},
		Now:       time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	argvBytes, _ := os.ReadFile(argvLog)
	if !strings.Contains(string(argvBytes), "prompt from args") {
		t.Fatalf("argv log missing Args[0] prompt: %q", argvBytes)
	}
}

// TestClaudeLaneAdapterResolvesTupleAndStampsAttribution proves the enrolled
// lane behavior through the same registry seam the CLI uses. The executable,
// model id, and provider route all come from Compound's model-router authority;
// none are parallel adapter configuration.
func TestClaudeLaneAdapterResolvesTupleAndStampsAttribution(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	binDir := fakeBinaryPath(t, "claude-minimax", argvLog, 0)
	withPATH(t, binDir)

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "registries"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `version: 1
purpose: fixture
capability_classes: [build-implement]
models:
  fixture-minimax:
    lane: fixture minimax lane
    harness: claude_minimax
    provider_route: minimax
    tier: cheap
    packet_ceiling: multi-file
    aliases: [claude-minimax]
    capabilities:
      build-implement: {level: capable, evidence: fixture}
routing_defaults:
  build-implement: [fixture-minimax]
`
	if err := os.WriteFile(filepath.Join(home, "registries", "model-capabilities.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	registry := NewAdapterRegistry(runattempt.Open(home))
	adapter, ok := registry.AdapterByName("claude_minimax")
	if !ok {
		t.Fatal("claude_minimax adapter is not enrolled")
	}
	out, err := adapter.Run(context.Background(), RunInput{
		WorkID:    "work_lane_tuple",
		Workspace: home,
		Prompt:    "execute the packet",
		Now:       time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.RunAttempt.Harness != "claude_minimax" {
		t.Fatalf("harness = %q, want claude_minimax", out.RunAttempt.Harness)
	}
	if filepath.Base(out.RunAttempt.Command) != "claude-minimax" {
		t.Fatalf("command = %q, want tuple alias claude-minimax", out.RunAttempt.Command)
	}
	if out.RunAttempt.LaneID != "claude_minimax" || out.RunAttempt.RequestedModelID != "fixture-minimax" || out.RunAttempt.RequestedProviderRoute != "minimax" {
		t.Fatalf("dispatch intent = lane:%q model:%q provider:%q", out.RunAttempt.LaneID, out.RunAttempt.RequestedModelID, out.RunAttempt.RequestedProviderRoute)
	}
	if out.RunAttempt.ModelID != "" || out.RunAttempt.ProviderRoute != "" {
		t.Fatalf("adapter guessed observed identity: model=%q provider=%q", out.RunAttempt.ModelID, out.RunAttempt.ProviderRoute)
	}
}

func TestClaudeLaneAdapterDoesNotRepeatPreflightAfterGovernedHandoff(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	binDir := fakeBinaryPath(t, "claude-minimax", argvLog, 0)
	withPATH(t, binDir)

	home := t.TempDir()
	manifestPath := filepath.Join(home, "registries", "model-capabilities.yaml")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `version: 1
purpose: fixture
capability_classes: [build-implement]
models:
  fixture-minimax:
    lane: fixture minimax lane
    harness: claude_minimax
    provider_route: minimax
    tier: cheap
    packet_ceiling: multi-file
    aliases: [claude-minimax]
    capabilities: {build-implement: {level: capable, evidence: fixture}}
routing_defaults:
  build-implement: [fixture-minimax]
`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := NewAdapterRegistry(runattempt.Open(home))
	adapter, _ := registry.AdapterByName("claude_minimax")
	preflight, ok := adapter.(PreflightAdapter)
	if !ok {
		t.Fatal("claude_minimax adapter has no preflight seam")
	}
	input, err := preflight.Preflight(context.Background(), RunInput{
		WorkID: "work_preflight_handoff", Workspace: home, Prompt: "execute once", Now: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	out, err := adapter.Run(context.Background(), input)
	if err != nil {
		t.Fatalf("Run repeated tuple/capacity preflight after governed handoff: %v", err)
	}
	if out.RunAttempt.Status != runattempt.StatusSucceeded {
		t.Fatalf("preflighted run status=%q", out.RunAttempt.Status)
	}
}

// TestClaudeLaneAdapterExhaustedDeclaredCapacityFailsBeforeMint proves an
// enrolled wrapper is still unavailable when its declared capacity source is
// fresh and exhausted. The adapter must refuse before the RunAttempt audit
// primitive is minted, because no harness run actually began.
func TestClaudeLaneAdapterExhaustedDeclaredCapacityFailsBeforeMint(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	binDir := fakeBinaryPath(t, "claude-minimax", argvLog, 0)
	withPATH(t, binDir)

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "registries"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `version: 1
purpose: fixture
capability_classes: [build-implement]
models:
  fixture-minimax:
    lane: fixture minimax lane
    harness: claude_minimax
    provider_route: minimax
    tier: cheap
    packet_ceiling: multi-file
    aliases: [claude-minimax]
    capacity_source: {kind: codexbar, freshness_minutes: 15, ref: "codexbar://provider/minimax"}
    capabilities:
      build-implement: {level: capable, evidence: fixture}
routing_defaults:
  build-implement: [fixture-minimax]
`
	if err := os.WriteFile(filepath.Join(home, "registries", "model-capabilities.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	usageDir := filepath.Join(userHome, "Library", "Application Support", "CodexBar")
	if err := os.MkdirAll(usageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	usage := `{"provider":"minimax","windowKind":"5h","windowMinutes":300,"usedPercent":100,"resetsAt":"2026-07-22T14:00:00Z","sampledAt":"2026-07-22T11:55:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(usageDir, "usage-history.jsonl"), []byte(usage), 0o644); err != nil {
		t.Fatal(err)
	}

	registry := NewAdapterRegistry(runattempt.Open(home))
	adapter, _ := registry.AdapterByName("claude_minimax")
	_, err := adapter.Run(context.Background(), RunInput{
		WorkID:    "work_exhausted_lane",
		Workspace: home,
		Prompt:    "must not start",
		Now:       now,
	})
	if err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("expected capacity refusal, got %v", err)
	}
	runAttemptFiles, _ := filepath.Glob(filepath.Join(home, "run_attempts", "ratt_*.json"))
	if len(runAttemptFiles) != 0 {
		t.Fatalf("exhausted capacity minted RunAttempts: %v", runAttemptFiles)
	}
	if _, err := os.Stat(argvLog); !os.IsNotExist(err) {
		t.Fatalf("exhausted capacity invoked the wrapper; argv log err=%v", err)
	}
}
