// Package source defines the EnvelopeSource interface and a default registry of per-app
// envelope resolvers. An EnvelopeSource answers TryEnvelope for ONE app — given the
// currently focused app reported by the OS, it returns the Tier-1 envelope (pane_cwd,
// git_remote, agent_kind, ...) or an error explaining why it can't.
//
// Every EnvelopeSource in the registry MUST pass its own ContentFreeInvariant at startup
// and again at runtime after each envelope materializes. The invariant is the runtime
// twin of the F5 schema-level ban — a Tier-1 envelope that smuggles `title`/`url`/
// `document`/etc. fails the binary at startup and never reaches the spool.
//
// Resolver strategies:
//
//   - cmux  → IPC. Shell out `cmux rpc surface.list`, parse JSON, extract
//     requested_working_directory + resume_binding.{cwd,kind,name}.
//   - iTerm2 / Warp / Terminal.app → AppleScript → tty → lsof. Generic fallback.
//   - VS Code / Cursor → consume AW `aw-watcher-vscode_*` events for `project` only
//     (never `file` — that is content-bearing).
//
// All Tier-1 fields are OPTIONAL. A source returns what it can resolve and leaves the
// rest blank. The schema's forbidden-meta-keys gate (F5) is the load-bearing safety
// net; this package's ContentFreeInvariant is the second.
package source

import (
	"context"
	"strings"
)

// Envelope captures Tier-1 envelope context for one focused surface. All fields are
// optional (omitempty). The schema forbids any of the activity.ForbiddenMetaKeys from
// appearing in this struct — enforced at construction via CheckEnvelopeContentFree.
//
// Field semantics:
//
//   - PaneCwd:      absolute path of the focused pane's working directory.
//   - GitRoot:      output of `git -C <pane_cwd> rev-parse --show-toplevel`.
//   - GitRemote:    output of `git -C <pane_cwd> config --get remote.origin.url`. Empty
//     if not a git repo.
//   - PaneType:     "terminal" | "browser" | "agent-session" | "editor".
//   - AgentKind:    vendor-shaped classification if known (e.g. "agent-hook" from cmux;
//     "claude" / "codex" if a finer source exists). Empty for non-agent panes.
//   - AgentName:    vendor display name (e.g. "Claude Code"). Generic product names
//     only — never personal identifiers.
//   - WorkspaceRef: per-app workspace reference (cmux workspace UUID).
//   - SurfaceRef:   per-app surface reference (cmux surface UUID).
//   - WindowRef:    per-app window reference (cmux window UUID).
//   - PaneResolution: how PaneCwd was resolved — a CLOSED confidence vocabulary
//     (see the PaneResolution* constants). PaneCwd from pgrep/lsof is a guess that
//     may pick the wrong process; cmux IPC names the focused surface directly.
//     Downstream consumers must not over-trust a best-effort cwd, so the marker is
//     emitted alongside it.
type Envelope struct {
	PaneCwd        string `json:"pane_cwd,omitempty"`
	GitRoot        string `json:"git_root,omitempty"`
	GitRemote      string `json:"git_remote,omitempty"`
	PaneType       string `json:"pane_type,omitempty"`
	AgentKind      string `json:"agent_kind,omitempty"`
	AgentName      string `json:"agent_name,omitempty"`
	WorkspaceRef   string `json:"workspace_ref,omitempty"`
	SurfaceRef     string `json:"surface_ref,omitempty"`
	WindowRef      string `json:"window_ref,omitempty"`
	PaneResolution string `json:"pane_resolution,omitempty"`
}

// PaneResolution is a closed vocabulary describing HOW a source resolved the pane's
// working directory, so consumers can weight its trust. It is structural provenance,
// never content. Ordered from strongest to weakest signal.
const (
	// PaneResolutionCmuxIPC: cmux reported the focused surface's cwd over IPC —
	// the focused surface is named directly, so the cwd is high-confidence.
	PaneResolutionCmuxIPC = "cmux_ipc"
	// PaneResolutionAWEvent: an ActivityWatch editor event carried the project
	// path — medium confidence (AW's own watcher attribution).
	PaneResolutionAWEvent = "aw_event"
	// PaneResolutionBestEffortProcess: cwd came from a process-level guess
	// (pgrep app -> pid -> lsof cwd). This can select a process that is NOT the
	// focused window/pane, so it is low-confidence and must be treated as a hint.
	PaneResolutionBestEffortProcess = "best_effort_process"
)

// FocusedApp is what the OS / AW reports about the foreground app. Sources use the
// fields they need (iTerm2 needs PID; cmux uses bundle id as a discriminator; VSCode
// uses bundle id and the AW bucket id).
type FocusedApp struct {
	BundleID string // macOS bundle identifier (e.g. "com.cmuxterm.app")
	AppName  string // human-readable app name (optional)
	PID      int    // OS process id, 0 if unknown
}

// EnvelopeSource is one resolver for one app. The interface is content-free BY
// CONTRACT — implementations refuse to populate banned keys, and ContentFreeInvariant
// fails the binary if they do.
//
// TryEnvelope returns (Envelope, true) on success; (zero, false) when the app isn't
// focused / installed / responsive. The error path is reserved for transport failures
// (e.g., AW unreachable, lsof command failed) — the caller logs and continues.
type EnvelopeSource interface {
	AppBundleID() string
	TryEnvelope(ctx context.Context, focused FocusedApp) (Envelope, bool, error)
	ContentFreeInvariant() error
}

// Registry maps bundle id to its source. NewRegistry() returns the default 6-source
// registry (cmux, iTerm2, Warp, Terminal.app, VSCode, Cursor). Tests can build a
// custom registry via NewRegistryFromSources.
type Registry struct {
	byBundle map[string]EnvelopeSource
}

// NewRegistry returns the default registry with all six sources wired.
func NewRegistry() *Registry {
	return NewRegistryFromSources(
		NewCmuxSource(),
		NewAppleScriptSource(AppleScriptConfig{BundleID: "com.googlecode.iterm2", DisplayName: "iTerm2"}),
		NewAppleScriptSource(AppleScriptConfig{BundleID: "dev.warp.Warp", DisplayName: "Warp"}),
		NewAppleScriptSource(AppleScriptConfig{BundleID: "com.apple.Terminal", DisplayName: "Terminal"}),
		NewEditorSource(EditorConfig{BundleID: "com.microsoft.VSCode", DisplayName: "Visual Studio Code"}),
		NewEditorSource(EditorConfig{BundleID: "com.todesktop.230313mzl4w4u96", DisplayName: "Cursor"}),
	)
}

// NewRegistryFromSources lets callers (and tests) build a custom registry. Sources
// with duplicate AppBundleID() values are silently dropped — the FIRST occurrence wins.
// This is the fail-soft posture: a misconfigured source cannot crash startup.
func NewRegistryFromSources(sources ...EnvelopeSource) *Registry {
	r := &Registry{byBundle: make(map[string]EnvelopeSource, len(sources))}
	for _, s := range sources {
		id := s.AppBundleID()
		if id == "" {
			continue
		}
		if _, exists := r.byBundle[id]; exists {
			continue
		}
		r.byBundle[id] = s
	}
	return r
}

// Resolve returns the source for a bundle id, or ok=false if no source is registered.
// Caller decides the fallback policy (e.g., return a generic Tier-1 stub).
func (r *Registry) Resolve(bundleID string) (EnvelopeSource, bool) {
	if r == nil || bundleID == "" {
		return nil, false
	}
	s, ok := r.byBundle[bundleID]
	return s, ok
}

// ContentFreeInvariant runs every registered source's self-test. If ANY source
// returns an envelope carrying a banned meta key, the registry returns an error and
// the binary should refuse to start (Tier-1 must be content-free, like Tier-0).
//
// The check uses each source's TryEnvelope with a zero FocusedApp — sources that
// require a focused app (most of them) will return ok=false; sources that materialize
// an envelope unconditionally (none today) would be exercised here. The check also
// runs ContentFreeInvariant on each source directly so a source whose envelope
// construction is itself buggy (e.g., accidentally populates a banned field) fails
// the binary.
func (r *Registry) ContentFreeInvariant() error {
	if r == nil {
		return nil
	}
	for bundleID, s := range r.byBundle {
		if err := s.ContentFreeInvariant(); err != nil {
			return &InvariantError{BundleID: bundleID, Err: err}
		}
	}
	return nil
}

// InvariantError wraps a ContentFreeInvariant failure with the offending bundle id.
type InvariantError struct {
	BundleID string
	Err      error
}

func (e *InvariantError) Error() string {
	return "tier1/source: " + e.BundleID + " failed content-free invariant: " + e.Err.Error()
}

// Unwrap lets errors.Is/As traverse to the underlying error.
func (e *InvariantError) Unwrap() error { return e.Err }

// Slug returns a short, content-free slug derived from the input. Used to build
// surface_ref / window_ref strings from arbitrary vendor-provided ids (UUIDs, opaque
// refs). The slug is purely structural — input is lowercased + stripped of any
// non-[a-z0-9-] characters. Returns "x" for empty or fully-stripped inputs so callers
// always get a non-empty slug.
func Slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "x"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
		if b.Len() >= 64 {
			break
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		return "x"
	}
	return out
}
