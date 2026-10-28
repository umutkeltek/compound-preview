package hookplan

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed omp_hook_shim.ts
var ompHookShimSource string

// OmpShimMarker is the ownership line compound writes at the top of the shim.
// An update replaces only compound-owned bytes; a file without the marker is a
// user's own hook and is never clobbered. Bumped to @2 by slice 3 (the shim
// gained transcript_path and session_id derivation).
const OmpShimMarker = "// compound-owned: oh-my-pi-hook-shim@2"

// OmpShimRelPath is the shim's location inside omp's agent dir. omp's builtin
// discovery loads every non-dotfile under hooks/{pre,post} as a Bun module;
// the pre/ directory is right because the shim registers handlers (it is not
// a shell matcher whose filename binds a tool).
const OmpShimRelPath = ".omp/agent/hooks/pre/compound.ts"

// InstallOhMyPiShim materializes the compound ↔ oh-my-pi hook shim into
// baseDir (the operator home). Idempotent: identical bytes are a no-op;
// compound-owned content is refreshed wholesale; a foreign file at the path
// fails closed rather than being clobbered. check=true performs the
// comparison without writing. The shim is deliberately NOT published into the
// installed-hooks authority: that authority reconciles settings-style hook
// entries, and omp's TS-module mechanism has no settings surface to reconcile.
// InstallOhMyPiShimWithConfigDir is the per-profile variant: when
// configDir is non-empty the shim is installed under
// <baseDir>/<configDir>/.omp/agent/hooks/pre/compound.ts instead of the
// default operator-home location. This lets  wire each isolated
// profile (e.g. an isolated lane profile) to its own shim copy.
func InstallOhMyPiShimWithConfigDir(baseDir, configDir string, check bool) (HookInstallReport, error) {
	if strings.TrimSpace(configDir) == "" {
		return InstallOhMyPiShim(baseDir, check)
	}
	report := HookInstallReport{Check: check, Scope: "global"}
	if strings.TrimSpace(baseDir) == "" {
		return report, fmt.Errorf("base dir required")
	}
	if !strings.HasPrefix(ompHookShimSource, OmpShimMarker) {
		return report, fmt.Errorf("embedded shim lost its ownership marker")
	}
	target := filepath.Join(baseDir, configDir, ".omp", "agent", "hooks", "pre", "compound.ts")
	existing, readErr := os.ReadFile(target)
	switch {
	case readErr == nil && !strings.Contains(string(existing), OmpShimMarker):
		return report, fmt.Errorf("%s exists and is not compound-owned; refusing to clobber it", target)
	case readErr != nil && !os.IsNotExist(readErr):
		return report, fmt.Errorf("read %s: %w", target, readErr)
	}
	changed := string(existing) != ompHookShimSource
	bindings := []HookInstallBinding{
		{Event: "SessionStart", Matcher: "*", Command: "omp hook shim: session_start -> compound hook session-start", Blocking: false},
		{Event: "UserPromptSubmit", Matcher: "*", Command: "omp hook shim: before_agent_start -> compound hook user-prompt-submit", Blocking: false},
		{Event: "PreToolUse", Matcher: "*", Command: "omp hook shim: tool_call -> compound hook pre-tool-use", Blocking: true},
		{Event: "PostToolUse", Matcher: "*", Command: "omp hook shim: tool_result -> compound hook post-tool-use", Blocking: false},
		{Event: "SessionEnd", Matcher: "*", Command: "omp hook shim: session_shutdown -> compound hook session-end", Blocking: false},
	}
	report.Entries = append(report.Entries, HookInstallEntry{
		Harness:    "oh-my-pi",
		ConfigPath: target,
		Action:     shimAction(changed, check, readErr == nil),
		Bindings:   bindings,
	})
	if check || !changed {
		return report, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return report, fmt.Errorf("create shim dir: %w", err)
	}
	if err := os.WriteFile(target, []byte(ompHookShimSource), 0o644); err != nil {
		return report, fmt.Errorf("write shim: %w", err)
	}
	return report, nil
}

func InstallOhMyPiShim(baseDir string, check bool) (HookInstallReport, error) {
	report := HookInstallReport{Check: check, Scope: "global"}
	if strings.TrimSpace(baseDir) == "" {
		return report, fmt.Errorf("base dir required")
	}
	if !strings.HasPrefix(ompHookShimSource, OmpShimMarker) {
		return report, fmt.Errorf("embedded shim lost its ownership marker")
	}
	target := filepath.Join(baseDir, OmpShimRelPath)
	existing, readErr := os.ReadFile(target)
	switch {
	case readErr == nil && !strings.Contains(string(existing), OmpShimMarker):
		return report, fmt.Errorf("%s exists and is not compound-owned; refusing to clobber it", target)
	case readErr != nil && !os.IsNotExist(readErr):
		return report, fmt.Errorf("read %s: %w", target, readErr)
	}
	changed := string(existing) != ompHookShimSource
	bindings := []HookInstallBinding{
		{Event: "SessionStart", Matcher: "*", Command: "omp hook shim: session_start → compound hook session-start", Blocking: false},
		{Event: "UserPromptSubmit", Matcher: "*", Command: "omp hook shim: before_agent_start → compound hook user-prompt-submit", Blocking: false},
		{Event: "PreToolUse", Matcher: "*", Command: "omp hook shim: tool_call → compound hook pre-tool-use", Blocking: true},
		{Event: "PostToolUse", Matcher: "*", Command: "omp hook shim: tool_result → compound hook post-tool-use", Blocking: false},
		{Event: "SessionEnd", Matcher: "*", Command: "omp hook shim: session_shutdown → compound hook session-end", Blocking: false},
	}
	report.Entries = append(report.Entries, HookInstallEntry{
		Harness:    "oh-my-pi",
		ConfigPath: target,
		Action:     shimAction(changed, check, readErr == nil),
		Bindings:   bindings,
	})
	if check || !changed {
		return report, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return report, fmt.Errorf("create shim dir: %w", err)
	}
	if err := os.WriteFile(target, []byte(ompHookShimSource), 0o644); err != nil {
		return report, fmt.Errorf("write shim: %w", err)
	}
	return report, nil
}

func shimAction(changed, check, existed bool) string {
	switch {
	case check && !existed:
		return "absent"
	case check && changed:
		return "drift"
	case check:
		return "ok"
	case !existed:
		return "installed"
	case changed:
		return "updated"
	default:
		return "ok"
	}
}
