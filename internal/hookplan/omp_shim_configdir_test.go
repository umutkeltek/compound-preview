package hookplan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOmpShimInstallsIntoProfileAgentDir pins the per-profile install path:
// `compound hook install --harness oh-my-pi --config-dir <dir>` writes the
// shim to <baseDir>/<configDir>/.omp/agent/hooks/pre/compound.ts. The
// default (empty configDir) path is unchanged. This is the seam that lets
// each isolated profile (e.g. an isolated lane profile) own its own shim copy without
// clobbering a sibling profile's hook surface.
func TestOmpShimInstallsIntoProfileAgentDir(t *testing.T) {
	base := t.TempDir()
	profileDir := "claude/omp/profiles/example-lane/agent"
	rep, err := InstallOhMyPiShimWithConfigDir(base, profileDir, false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	want := filepath.Join(base, profileDir, ".omp", "agent", "hooks", "pre", "compound.ts")
	if len(rep.Entries) != 1 {
		t.Fatalf("entries=%d, want 1", len(rep.Entries))
	}
	if got := rep.Entries[0].ConfigPath; got != want {
		t.Fatalf("ConfigPath=%q, want %q", got, want)
	}
	if rep.Entries[0].Harness != "oh-my-pi" {
		t.Fatalf("harness=%q, want oh-my-pi", rep.Entries[0].Harness)
	}
	body, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read shim: %v", err)
	}
	if !strings.Contains(string(body), OmpShimMarker) {
		t.Fatalf("shim missing ownership marker %q", OmpShimMarker)
	}
	// Idempotent: same bytes -> ok, no churn.
	rep, err = InstallOhMyPiShimWithConfigDir(base, profileDir, false)
	if err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if rep.Entries[0].Action != "ok" {
		t.Fatalf("idempotent reinstall action=%q, want ok", rep.Entries[0].Action)
	}

	// Default path (empty configDir) is unchanged -- still baseDir-rooted.
	base2 := t.TempDir()
	rep2, err := InstallOhMyPiShimWithConfigDir(base2, "", false)
	if err != nil {
		t.Fatalf("default install: %v", err)
	}
	want2 := filepath.Join(base2, OmpShimRelPath)
	if rep2.Entries[0].ConfigPath != want2 {
		t.Fatalf("default path=%q, want %q (empty configDir MUST delegate to InstallOhMyPiShim)", rep2.Entries[0].ConfigPath, want2)
	}
}

// TestResolveInstallRequestAcceptsOhMyPiWithConfigDir pins the install_request
// gate: `--config-dir <profile>` is now accepted for oh-my-pi in addition to
// the Claude-profile harnesses. The gate refuses other harnesses -- the
// install surface for claude-code etc. does not have a per-profile shape.
func TestResolveInstallRequestAcceptsOhMyPiWithConfigDir(t *testing.T) {
	req, err := ResolveInstallRequest("", "oh-my-pi", "", "claude/omp/profiles/example-lane/agent")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(req.Harnesses) != 1 || req.Harnesses[0] != "oh-my-pi" {
		t.Fatalf("harnesses = %v, want [oh-my-pi]", req.Harnesses)
	}
	if req.ConfigDir != "claude/omp/profiles/example-lane/agent" {
		t.Fatalf("configDir=%q, want %q", req.ConfigDir, "claude/omp/profiles/example-lane/agent")
	}
}
