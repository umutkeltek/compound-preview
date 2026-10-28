// Package warp is a stub Terminal impl for Warp (dev.warp.Warp). Warp's
// IPC surface is not yet wired in compound; this stub provides a present
// name so the CLI lists it, with honest "not supported" errors for
// operator verbs and a doctor that reports installed state from
// /Applications/Warp.app.
package warp

import (
	"context"
	"fmt"
	"os"

	"compound/internal/terminal"
)

const (
	Name     = "warp"
	BundleID = "dev.warp.Warp"
)

func newWarp() terminal.Terminal { return &impl{} }

// New returns the production Warp Terminal. Exported for the registry.
func New() terminal.Terminal { return newWarp() }

// init registers this Terminal with the package-level default registry.
func init() { terminal.Register(New()) }

type impl struct{}

func (i *impl) Name() string     { return Name }
func (i *impl) BundleID() string { return BundleID }
func (i *impl) Hooks() terminal.HookSet {
	return terminal.HookSet{Install: "# warp: no bridge\n", Uninstall: "# no-op\n"}
}
func (i *impl) Workspace() terminal.WorkspaceAPI { return workspace{} }
func (i *impl) Feed() terminal.FeedAPI           { return feed{} }

func (i *impl) Doctor(_ context.Context) (terminal.DoctorReport, error) {
	rep := terminal.DoctorReport{IPCReachable: false}
	if _, err := os.Stat("/Applications/Warp.app"); err == nil {
		rep.Installed = true
		rep.Issues = append(rep.Issues, "warp IPC not yet wired; verbs return ErrNotSupported")
	} else {
		rep.Installed = false
		rep.Issues = append(rep.Issues, "warp not installed at /Applications/Warp.app")
	}
	return rep, nil
}

type workspace struct{}

func (workspace) Select(_ context.Context, _ terminal.WorkspaceRef) error {
	return fmt.Errorf("%w: warp workspace.Select", terminal.ErrNotSupported)
}
func (workspace) SetColor(_ context.Context, _ terminal.WorkspaceRef, _ string) error {
	return fmt.Errorf("%w: warp workspace.SetColor", terminal.ErrNotSupported)
}
func (workspace) SetIcon(_ context.Context, _ terminal.WorkspaceRef, _ string) error {
	return fmt.Errorf("%w: warp workspace.SetIcon", terminal.ErrNotSupported)
}

type feed struct{}

func (feed) Push(_ context.Context, _ string, _ terminal.FeedPushOpts) error {
	return fmt.Errorf("%w: warp feed.Push", terminal.ErrNotSupported)
}
func (feed) SetStatus(_ context.Context, _, _ string) error {
	return fmt.Errorf("%w: warp feed.SetStatus", terminal.ErrNotSupported)
}
