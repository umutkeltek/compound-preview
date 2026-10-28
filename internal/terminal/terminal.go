// Package terminal defines a unified Terminal interface and registry so that
// compound can drive any installed terminal (cmux, iTerm2, Warp, Terminal.app,
// or a future one) through the same operator surface: `compound terminal <name>
// doctor | wire | unwire | workspace … | feed …`.
//
// Adding a new terminal = one new file implementing Terminal + one line in
// NewRegistry. The interface is intentionally small (5 methods); terminals
// that don't support a verb return a typed ErrNotSupported error rather than
// silently no-op, so the operator surface is honest about capability.
//
// This package is operator-facing and Tier-1-orthogonal: cmux's EnvelopeSource
// in internal/activity/tier1/source/cmux.go continues to do its own activity
// capture, and the Terminal impl here is a separate, simpler wrapper that
// only handles doctor/health, hook installation, workspace control, and feed.
package terminal

import (
	"context"
	"errors"
)

// ErrNotSupported signals that a Terminal implementation does not implement a
// particular verb. The CLI surface translates this to a clean exit with a
// helpful message rather than crashing.
var ErrNotSupported = errors.New("terminal: operation not supported by this terminal")

// DoctorReport is the structured health report returned by Doctor. It is
// designed for JSON output (--json) so external tooling (e.g. operator-deck)
// can ingest it.
type DoctorReport struct {
	Installed    bool     `json:"installed"`        // binary present on disk
	IPCReachable bool     `json:"ipc_reachable"`    // IPC / RPC works (false for non-cmux terminals)
	HooksWired   int      `json:"hooks_wired"`      // count of bridge hook scripts found
	BridgeOK     bool     `json:"bridge_ok"`        // user-side bridge scripts (e.g. ~/.local/bin/cmux-*) present
	Issues       []string `json:"issues,omitempty"` // human-readable blockers
}

// WorkspaceRef identifies a workspace or surface. Either ID or Title may be
// set; cmux accepts either, AppleScript-based terminals require a window
// reference (we use Title today).
type WorkspaceRef struct {
	ID    string `json:"id,omitempty"`
	Title string `json:"title,omitempty"`
}

// HookSet describes the install/uninstall shell snippets for a terminal's
// bridge hooks. Install is the script body that drops hooks into the user's
// home directory; Uninstall is its inverse. Paths lists the resolved
// absolute paths of installed hooks (informational).
type HookSet struct {
	Install   string   `json:"install"`
	Uninstall string   `json:"uninstall"`
	Paths     []string `json:"paths,omitempty"`
}

// FeedPushOpts carries optional metadata for a feed push. Source identifies
// the producing hook / event class; Kind is a short verb (e.g. "event",
// "task_done"). Both are optional; the underlying transport has its own
// defaults.
type FeedPushOpts struct {
	Source string `json:"source,omitempty"`
	Kind   string `json:"kind,omitempty"`
}

// WorkspaceAPI exposes workspace control verbs. Implementations may return
// ErrNotSupported if their terminal does not support a particular verb.
type WorkspaceAPI interface {
	Select(ctx context.Context, ref WorkspaceRef) error
	SetColor(ctx context.Context, ref WorkspaceRef, hex string) error
	SetIcon(ctx context.Context, ref WorkspaceRef, glyph string) error
}

// FeedAPI exposes feed / status push verbs. Implementations may return
// ErrNotSupported if their terminal does not expose a feed.
type FeedAPI interface {
	Push(ctx context.Context, body string, opts FeedPushOpts) error
	SetStatus(ctx context.Context, key, value string) error
}

// Terminal is the contract every concrete terminal implements. Methods are
// small and orthogonal so adding a new terminal is a focused change.
//
// Name and BundleID are pure metadata (no I/O). Doctor and Hooks return
// without external mutation (read-only). Workspace and Feed are the only
// verb families that may invoke the terminal's IPC or write state.
type Terminal interface {
	Name() string
	BundleID() string
	Doctor(ctx context.Context) (DoctorReport, error)
	Hooks() HookSet
	Workspace() WorkspaceAPI
	Feed() FeedAPI
}
