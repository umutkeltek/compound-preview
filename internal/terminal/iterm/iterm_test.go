package iterm

import (
	"context"
	"errors"
	"testing"

	"compound/internal/terminal"
)

func TestNameAndBundleID(t *testing.T) {
	t1 := New()
	if t1.Name() != Name {
		t.Fatalf("Name() = %q; want %q", t1.Name(), Name)
	}
	if t1.BundleID() != BundleID {
		t.Fatalf("BundleID() = %q; want %q", t1.BundleID(), BundleID)
	}
}

func TestDoctor_ReportsNoIPC(t *testing.T) {
	rep, err := New().Doctor(context.Background())
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if rep.IPCReachable {
		t.Fatal("IPCReachable=true; want false for iTerm2")
	}
	if len(rep.Issues) == 0 {
		t.Fatal("Issues empty; want at least one explaining no IPC")
	}
}

func TestWorkspace_Unsupported(t *testing.T) {
	t1 := New()
	api := t1.Workspace()
	if err := api.Select(context.Background(), terminal.WorkspaceRef{Title: "x"}); !errors.Is(err, terminal.ErrNotSupported) {
		t.Fatalf("Select err = %v; want ErrNotSupported", err)
	}
	if err := api.SetColor(context.Background(), terminal.WorkspaceRef{Title: "x"}, "#fff"); !errors.Is(err, terminal.ErrNotSupported) {
		t.Fatalf("SetColor err = %v; want ErrNotSupported", err)
	}
	if err := api.SetIcon(context.Background(), terminal.WorkspaceRef{Title: "x"}, "●"); !errors.Is(err, terminal.ErrNotSupported) {
		t.Fatalf("SetIcon err = %v; want ErrNotSupported", err)
	}
}

func TestFeed_PushUnsupported(t *testing.T) {
	t1 := New()
	if err := t1.Feed().Push(context.Background(), "body", terminal.FeedPushOpts{}); !errors.Is(err, terminal.ErrNotSupported) {
		t.Fatalf("Push err = %v; want ErrNotSupported", err)
	}
}

func TestHooks_Empty(t *testing.T) {
	hs := New().Hooks()
	if hs.Paths != nil && len(hs.Paths) > 0 {
		t.Fatalf("Hooks.Paths = %v; want nil/empty", hs.Paths)
	}
}
