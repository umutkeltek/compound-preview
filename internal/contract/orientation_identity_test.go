package contract

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"compound/internal/harness"
)

func TestResolveOrientationIdentityCanonicalizesRegisteredHarnessAndCWD(t *testing.T) {
	realCWD := t.TempDir()
	requestedCWD := realCWD
	if runtime.GOOS != "windows" {
		alias := filepath.Join(t.TempDir(), "workspace-alias")
		if err := os.Symlink(realCWD, alias); err != nil {
			t.Fatal(err)
		}
		requestedCWD = alias
	}

	got, err := ResolveOrientationIdentity(Input{
		Harness: "  claude-code  ",
		Cwd:     requestedCWD,
	}, harness.DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if got.Harness != "claude_code" {
		t.Fatalf("harness = %q, want canonical claude_code", got.Harness)
	}
	wantCWD, err := filepath.EvalSymlinks(realCWD)
	if err != nil {
		t.Fatal(err)
	}
	wantCWD, err = filepath.Abs(wantCWD)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cwd != filepath.Clean(wantCWD) {
		t.Fatalf("cwd = %q, want canonical %q", got.Cwd, filepath.Clean(wantCWD))
	}
}

func TestResolveOrientationIdentityRejectsFictitiousHarnessAndCWD(t *testing.T) {
	reg := harness.DefaultRegistry()
	validCWD := t.TempDir()
	fileCWD := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(fileCWD, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		in   Input
		want string
	}{
		{
			name: "unknown harness",
			in:   Input{Harness: "invented-agent", Cwd: validCWD},
			want: "is not registered",
		},
		{
			name: "missing cwd",
			in:   Input{Harness: "codex", Cwd: filepath.Join(t.TempDir(), "missing")},
			want: "is unavailable",
		},
		{
			name: "cwd is a file",
			in:   Input{Harness: "codex", Cwd: fileCWD},
			want: "is not a directory",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ResolveOrientationIdentity(tc.in, reg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ResolveOrientationIdentity error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestResolveOrientationIdentityLeavesMissingHarnessForCLIUsageHandling(t *testing.T) {
	got, err := ResolveOrientationIdentity(Input{Cwd: t.TempDir()}, harness.DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if got.Harness != "" {
		t.Fatalf("harness = %q, want empty for caller usage handling", got.Harness)
	}
}

func TestRepoKindRegistryMatchesCanonicalizedWorkspaceIdentity(t *testing.T) {
	realRoot := t.TempDir()
	realCWD := filepath.Join(realRoot, "project")
	if err := os.Mkdir(realCWD, 0o700); err != nil {
		t.Fatal(err)
	}
	registryRoot := realRoot
	requestedCWD := realCWD
	if runtime.GOOS != "windows" {
		aliasParent := t.TempDir()
		aliasRoot := filepath.Join(aliasParent, "workspace-root")
		if err := os.Symlink(realRoot, aliasRoot); err != nil {
			t.Fatal(err)
		}
		registryRoot = aliasRoot
	}

	kind, via := DetectRepoKindFromRegistry(RepoKindRegistry{
		Repos: []RepoKindEntry{{Kind: "control-plane", Roots: []string{registryRoot}}},
	}, "registry.yaml", requestedCWD)
	if kind != "control-plane" || via != "repo-kind-registry:registry.yaml" {
		t.Fatalf("registry resolution = (%q, %q), want canonical control-plane match", kind, via)
	}
}
