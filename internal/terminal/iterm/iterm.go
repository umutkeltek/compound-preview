// Package iterm is the Terminal impl for iTerm2 (com.googlecode.iterm2).
// iTerm2 has no IPC fast path; the operator surface here is best-effort —
// Doctor reports IPC=false and the Workspace/Feed verbs return
// ErrNotSupported honestly.
//
// The Tier-1 envelope capture for iTerm2 already lives in
// internal/activity/tier1/source/applescript.go (a generic
// AppleScriptSource constructed with iTerm2's bundle id). We do NOT wrap
// that source here — the Terminal interface is for operator verbs, not
// activity capture, and reusing the Tier-1 source would couple the two.
package iterm

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"compound/internal/terminal"
)

const (
	Name     = "iterm2"
	BundleID = "com.googlecode.iterm2"
)

func newIterm() terminal.Terminal { return &impl{} }

// New returns the production iTerm2 Terminal. Exported because the registry
// (internal/terminal/registry.go) lists constructors explicitly.
func New() terminal.Terminal { return newIterm() }

// init registers this Terminal with the package-level default registry.
func init() { terminal.Register(New()) }

type impl struct{}

// Name returns "iterm2".
func (i *impl) Name() string { return Name }

// BundleID returns "com.googlecode.iterm2".
func (i *impl) BundleID() string { return BundleID }

// Doctor reports installation (via /Applications/iTerm.app) and an honest
// "no IPC" status. We do NOT probe AppleScript here — that would couple
// this verb to the Tier-1 capture path. The Tier-1 source is the right
// place for TCC / lsof interaction; Doctor just reports what an operator
// can verify at a glance.
func (i *impl) Doctor(_ context.Context) (terminal.DoctorReport, error) {
	rep := terminal.DoctorReport{}
	if _, err := os.Stat("/Applications/iTerm.app"); err == nil {
		rep.Installed = true
	} else {
		rep.Installed = false
		rep.Issues = append(rep.Issues, "iTerm.app not at /Applications/iTerm.app")
	}
	// iTerm2 has no IPC. The Tier-1 AppleScriptSource is the closest thing
	// to an IPC, but it's a read-only cwd probe, not operator-control.
	rep.IPCReachable = false
	rep.Issues = append(rep.Issues, "iTerm2 has no IPC; operator verbs unsupported")
	return rep, nil
}

// Hooks is empty — iTerm2 has no compound bridge hooks today.
func (i *impl) Hooks() terminal.HookSet {
	return terminal.HookSet{
		Install:   "# iTerm2: no bridge hooks wired. Hooks design not applicable.\n",
		Uninstall: "# no-op\n",
		Paths:     nil,
	}
}

// Workspace returns a stub that returns ErrNotSupported for all verbs.
func (i *impl) Workspace() terminal.WorkspaceAPI { return &workspace{} }

// Feed returns a stub. Feed.Push falls back to a no-op stderr line rather
// than ErrNotSupported because some terminals (e.g. cmux via set-status)
// have a graceful degradation path.
func (i *impl) Feed() terminal.FeedAPI { return &feed{} }

// workspace is a stub — iTerm2 has no programmatic workspace control
// suitable for the operator verbs in this interface.
type workspace struct{}

func (workspace) Select(_ context.Context, _ terminal.WorkspaceRef) error {
	return fmt.Errorf("%w: workspace.Select", terminal.ErrNotSupported)
}
func (workspace) SetColor(_ context.Context, _ terminal.WorkspaceRef, _ string) error {
	return fmt.Errorf("%w: workspace.SetColor", terminal.ErrNotSupported)
}
func (workspace) SetIcon(_ context.Context, _ terminal.WorkspaceRef, _ string) error {
	return fmt.Errorf("%w: workspace.SetIcon", terminal.ErrNotSupported)
}

// feed falls back to /usr/bin/logger for SetStatus (so the body is
// somewhere in the system log) and returns ErrNotSupported for Push.
type feed struct{}

func (feed) Push(_ context.Context, _ string, _ terminal.FeedPushOpts) error {
	return fmt.Errorf("%w: feed.Push", terminal.ErrNotSupported)
}

func (feed) SetStatus(_ context.Context, key, value string) error {
	// Use syslog as a graceful fallback so the operator's status update
	// isn't lost. Stdout-only fallback; no IPC error.
	cmd := exec.Command("/usr/bin/logger", "-t", "compound-terminal-iterm", fmt.Sprintf("%s=%s", key, value))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("iterm set-status fallback (syslog): %w", err)
	}
	return nil
}
