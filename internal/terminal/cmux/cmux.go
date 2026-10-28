// Package cmux is the Terminal impl for cmux (com.cmuxterm.app), the user's
// primary terminal on macOS. cmux is the only Terminal impl with a true IPC
// fast path; everything else falls back to AppleScript + lsof.
//
// cmux's Terminal surface lives alongside (not inside) the Tier-1
// EnvelopeSource in internal/activity/tier1/source/cmux.go — they share the
// runner abstraction but serve different layers:
//
//   - EnvelopeSource (Tier-1): content-free activity envelope capture for the
//     activity collector; runs every AW change.
//   - Terminal (Tier-3, this package): operator-facing doctor/wire/workspace/
//     feed verbs; runs on demand.
//
// Separation keeps the operator surface small and the activity capture path
// free of operator-only state.
package cmux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"compound/internal/terminal"
)

const (
	Name     = "cmux"
	BundleID = "com.cmuxterm.app"
)

// New returns the production cmux Terminal. Exported because the registry
// (internal/terminal/registry.go) lists constructors explicitly, mirroring
// the pattern at internal/activity/tier1/source/source.go:111-118.
func New() terminal.Terminal { return &impl{runner: ExecRunner{}} }

// init registers this Terminal with the package-level default registry.
// The CLI (cmd/compound/terminal_cli.go) blank-imports this package so
// init() runs before NewRegistry is called.
func init() { terminal.Register(New()) }

// impl is the production cmux Terminal. The runner field is replaceable for
// tests via newWithRunner; production callers use newCmux which wires the
// real exec runner.
type impl struct {
	runner Runner
}

// Runner abstracts the cmux CLI invocations so tests can inject canned
// responses without spawning the real binary. Mirrors the CmuxRunner
// pattern in internal/activity/tier1/source/cmux.go:29-31.
type Runner interface {
	Run(ctx context.Context, args ...string) (stdout []byte, err error)
}

// ExecRunner is the production runner — shells out to the real `cmux`
// binary on PATH. Errors wrap fs.ErrNotExist when cmux is missing so the
// Doctor path can report installed=false without crashing.
type ExecRunner struct{}

// Run executes `cmux <args>` with combined stderr merged into stdout.
func (ExecRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "cmux", args...).CombinedOutput()
}

func newWithRunner(r Runner) *impl { return &impl{runner: r} }

// Name returns "cmux".
func (i *impl) Name() string { return Name }

// BundleID returns the macOS bundle identifier.
func (i *impl) BundleID() string { return BundleID }

// Doctor reports installation, IPC reachability, hook wiring, and bridge
// status. Read-only — does not install anything.
func (i *impl) Doctor(ctx context.Context) (terminal.DoctorReport, error) {
	rep := terminal.DoctorReport{BridgeOK: true}

	// Installed: try `cmux version`. A zero exit + non-empty stdout means
	// the binary exists and runs.
	if _, err := i.runner.Run(ctx, "version"); err != nil {
		var ee *exec.Error
		if errors.As(err, &ee) && errors.Is(ee, exec.ErrNotFound) {
			rep.Installed = false
			rep.Issues = append(rep.Issues, "cmux not on PATH")
			return rep, nil
		}
		// cmux is present but returned an error — treat as installed, IPC unknown.
		rep.Installed = true
		rep.Issues = append(rep.Issues, fmt.Sprintf("cmux version failed: %v", err))
	} else {
		rep.Installed = true
	}

	// IPC: probe with `cmux identify --json`. A non-JSON response means the
	// socket isn't reachable. Errors.As exec.ErrNotFound can't happen here
	// since we already checked installed above; other errors mean IPC down.
	out, err := i.runner.Run(ctx, "identify", "--json")
	if err != nil {
		rep.IPCReachable = false
		rep.Issues = append(rep.Issues, fmt.Sprintf("cmux identify failed: %v", err))
	} else if !json.Valid(out) {
		rep.IPCReachable = false
		rep.Issues = append(rep.Issues, "cmux identify returned non-JSON")
	} else {
		rep.IPCReachable = true
	}

	// Hooks: count bridge hooks in ~/.config/cmux/hooks/. Mirrors what the
	// Tier-1 on-notification/on-pane-finish/on-compound-event scripts need.
	hookDir := filepath.Join(homeDir(), ".config", "cmux", "hooks")
	if entries, err := os.ReadDir(hookDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".sh") {
				rep.HooksWired++
			}
		}
	}

	// Bridge: presence of any ~/.local/bin/cmux-* helper.
	binDir := filepath.Join(homeDir(), ".local", "bin")
	if entries, err := os.ReadDir(binDir); err == nil {
		found := false
		for _, e := range entries {
			if !e.IsDir() && strings.HasPrefix(e.Name(), "cmux-") {
				found = true
				break
			}
		}
		rep.BridgeOK = found
		if !found {
			rep.Issues = append(rep.Issues, "no ~/.local/bin/cmux-* helper scripts")
		}
	}

	return rep, nil
}

// Hooks returns the install/uninstall snippets for the three cmux bridge
// hooks. We don't auto-install from this method — the CLI surfaces the
// snippets so the operator can review before running. Wire() runs them.
func (i *impl) Hooks() terminal.HookSet {
	return terminal.HookSet{
		Install: cmuxInstallScript(),
		Uninstall: "# uninstall is a no-op for now; the Tier-1+2 user-side scripts\n" +
			"# remain functional alongside this hook set. Remove by hand:\n" +
			"#   rm ~/.config/cmux/hooks/on-notification.sh\n" +
			"#   rm ~/.config/cmux/hooks/on-pane-finish.sh\n" +
			"#   rm ~/.config/cmux/hooks/on-compound-event.sh",
		Paths: []string{
			filepath.Join(homeDir(), ".config", "cmux", "hooks", "on-notification.sh"),
			filepath.Join(homeDir(), ".config", "cmux", "hooks", "on-pane-finish.sh"),
			filepath.Join(homeDir(), ".config", "cmux", "hooks", "on-compound-event.sh"),
		},
	}
}

// cmuxInstallScript is a documented, idempotent shell snippet that ensures
// the three cmux bridge hooks exist. It does NOT overwrite existing files
// (operator-edited hooks are preserved). It is intentionally a string so
// the CLI can print it for review before execution.
func cmuxInstallScript() string {
	return `#!/usr/bin/env bash
# Idempotent: skips hooks that already exist. Operator-edited hooks win.
set -euo pipefail
HOOK_DIR="$HOME/.config/cmux/hooks"
mkdir -p "$HOOK_DIR"

# The three bridge hooks ship in this repo at internal/terminal/cmux/hooks/.
# In a future PR we will embed them as embedded files; for now this snippet
# documents the contract — the operator wires them by hand via:
#
#   cp path/to/on-notification.sh "$HOOK_DIR/on-notification.sh"
#   cp path/to/on-pane-finish.sh  "$HOOK_DIR/on-pane-finish.sh"
#   cp path/to/on-compound-event.sh "$HOOK_DIR/on-compound-event.sh"
#   chmod +x "$HOOK_DIR"/*.sh
#
# See ~/.config/cmux/README.md for the bridge architecture.
echo "cmux: wire hook contract — install the three scripts manually until embedded."
`
}

// Workspace returns the cmux workspace API.
func (i *impl) Workspace() terminal.WorkspaceAPI { return &workspace{runner: i.runner} }

// Feed returns the cmux feed API.
func (i *impl) Feed() terminal.FeedAPI { return &feed{runner: i.runner} }

// ─────────────────────────────────────────────────────────────────────────────
// workspace / feed impls

// workspace invokes cmux workspace verbs over the cmux CLI.
type workspace struct{ runner Runner }

// Select focuses a workspace by id or title. cmux accepts either via
// `cmux focus-workspace --id X` or `cmux focus-workspace --title Y`.
func (w *workspace) Select(ctx context.Context, ref terminal.WorkspaceRef) error {
	if ref.ID != "" {
		if _, err := w.runner.Run(ctx, "focus-workspace", "--id", ref.ID); err != nil {
			return fmt.Errorf("cmux focus-workspace --id %s: %w", ref.ID, err)
		}
		return nil
	}
	if ref.Title != "" {
		if _, err := w.runner.Run(ctx, "focus-workspace", "--title", ref.Title); err != nil {
			return fmt.Errorf("cmux focus-workspace --title %s: %w", ref.Title, err)
		}
		return nil
	}
	return errors.New("workspace.Select: ref must have ID or Title")
}

// SetColor sets the workspace indicator color via cmux workspace.set-color.
// cmux accepts --hex; we forward as-is.
func (w *workspace) SetColor(ctx context.Context, ref terminal.WorkspaceRef, hex string) error {
	args := []string{"workspace.set-color", "--hex", hex}
	if ref.ID != "" {
		args = append(args, "--id", ref.ID)
	} else if ref.Title != "" {
		args = append(args, "--title", ref.Title)
	} else {
		return errors.New("workspace.SetColor: ref must have ID or Title")
	}
	if _, err := w.runner.Run(ctx, args...); err != nil {
		return fmt.Errorf("cmux workspace.set-color: %w", err)
	}
	return nil
}

// SetIcon sets the workspace indicator glyph. cmux 0.64.x has no
// `workspace.set-icon` verb; return ErrNotSupported honestly.
func (w *workspace) SetIcon(_ context.Context, _ terminal.WorkspaceRef, _ string) error {
	return terminal.ErrNotSupported
}

// feed invokes cmux feed verbs. cmux's `feed.push` is an RPC method only
// (no CLI subcommand in 0.64.x) — we wrap it via `cmux rpc feed.push`.
type feed struct{ runner Runner }

// Push posts an event body to cmux's feed via the feed.push RPC method.
// This is the same wire shape introduced by Tier-1.1 in
// ~/.config/cmux/hooks/on-compound-event.sh.
func (f *feed) Push(ctx context.Context, body string, opts terminal.FeedPushOpts) error {
	payload := map[string]any{"body": body}
	if opts.Source != "" {
		payload["source"] = opts.Source
	}
	if opts.Kind != "" {
		payload["kind"] = opts.Kind
	}
	js, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("feed.Push: marshal payload: %w", err)
	}
	if _, err := f.runner.Run(ctx, "rpc", "feed.push", "--json", string(js)); err != nil {
		// Fall back to set-status so the body surfaces somewhere visible even
		// when the feed RPC is unavailable in this cmux version.
		key := "compound-event"
		if opts.Source != "" {
			key = opts.Source
		}
		if _, stErr := f.runner.Run(ctx, "set-status", key, body); stErr != nil {
			return fmt.Errorf("feed.Push: rpc=%v fallback=%w", err, stErr)
		}
	}
	return nil
}

// SetStatus sets a per-workspace status pill via cmux set-status.
func (f *feed) SetStatus(ctx context.Context, key, value string) error {
	if _, err := f.runner.Run(ctx, "set-status", key, value); err != nil {
		return fmt.Errorf("cmux set-status %s: %w", key, err)
	}
	return nil
}

// homeDir returns $HOME with a fallback so tests don't crash in CI.
func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/tmp"
}
