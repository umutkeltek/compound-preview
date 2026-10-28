// Package terminalapp is a stub Terminal impl for macOS Terminal.app
// (com.apple.Terminal). Like Warp, this is a stub so the CLI lists the
// name honestly. Terminal.app ships with macOS so Installed=true is the
// default on this host; everything else returns ErrNotSupported.
package terminalapp

import (
	"context"
	"fmt"
	"runtime"

	"compound/internal/terminal"
)

const (
	Name     = "terminal-app"
	BundleID = "com.apple.Terminal"
)

func newTerminalApp() terminal.Terminal { return &impl{} }

// New returns the production Terminal.app Terminal. Exported for the registry.
func New() terminal.Terminal { return newTerminalApp() }

// init registers this Terminal with the package-level default registry.
func init() { terminal.Register(New()) }

type impl struct{}

func (i *impl) Name() string     { return Name }
func (i *impl) BundleID() string { return BundleID }
func (i *impl) Hooks() terminal.HookSet {
	return terminal.HookSet{Install: "# terminal-app: no bridge\n", Uninstall: "# no-op\n"}
}
func (i *impl) Workspace() terminal.WorkspaceAPI { return workspace{} }
func (i *impl) Feed() terminal.FeedAPI           { return feed{} }

func (i *impl) Doctor(_ context.Context) (terminal.DoctorReport, error) {
	rep := terminal.DoctorReport{IPCReachable: false}
	if runtime.GOOS == "darwin" {
		rep.Installed = true
		rep.Issues = append(rep.Issues, "Terminal.app has no IPC; verbs return ErrNotSupported")
	} else {
		rep.Installed = false
		rep.Issues = append(rep.Issues, "Terminal.app is macOS-only")
	}
	return rep, nil
}

type workspace struct{}

func (workspace) Select(_ context.Context, _ terminal.WorkspaceRef) error {
	return fmt.Errorf("%w: terminal-app workspace.Select", terminal.ErrNotSupported)
}
func (workspace) SetColor(_ context.Context, _ terminal.WorkspaceRef, _ string) error {
	return fmt.Errorf("%w: terminal-app workspace.SetColor", terminal.ErrNotSupported)
}
func (workspace) SetIcon(_ context.Context, _ terminal.WorkspaceRef, _ string) error {
	return fmt.Errorf("%w: terminal-app workspace.SetIcon", terminal.ErrNotSupported)
}

type feed struct{}

func (feed) Push(_ context.Context, _ string, _ terminal.FeedPushOpts) error {
	return fmt.Errorf("%w: terminal-app feed.Push", terminal.ErrNotSupported)
}
func (feed) SetStatus(_ context.Context, _, _ string) error {
	return fmt.Errorf("%w: terminal-app feed.SetStatus", terminal.ErrNotSupported)
}
