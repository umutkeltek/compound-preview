package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
)

func captureCmdHomeStdout(t *testing.T, args []string) string {
	t.Helper()
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = oldStdout })
	cmdHome(args)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestHomeWhereDoesNotAdoptAncestorLocalHome is the strict-resolver CLI
// guarantee: `compound home where` must never follow a `.compound` ancestor
// up the cwd; it resolves only --home, $COMPOUND_HOME, and the global default.
func TestHomeWhereDoesNotAdoptAncestorLocalHome(t *testing.T) {
	t.Setenv("COMPOUND_HOME", "")
	root := t.TempDir()
	ancestor := filepath.Join(root, ".compound")
	if err := home.Init(ancestor, "local", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "repo", "nested")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(child); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })

	out := strings.TrimSpace(captureCmdHomeStdout(t, []string{"where"}))
	if out == ancestor {
		t.Fatalf("compound home where adopted ancestor authority %q", out)
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(homeDir, ".compound")
	if out != want {
		t.Fatalf("compound home where = %q, want global default %q", out, want)
	}
}

// TestHomeLocalFlagExplicitlySelectsLocalHome covers the operator-initiated
// --local path: `compound home init --local` must select <cwd>/.compound and
// `compound home where --local` must echo it. Without --local, `where` must
// not implicitly fall through to the freshly-init'd local home.
func TestHomeLocalFlagExplicitlySelectsLocalHome(t *testing.T) {
	t.Setenv("COMPOUND_HOME", "")
	repo := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })

	out := captureCmdHomeStdout(t, []string{"init", "--local"})
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(cwd, ".compound")
	if !strings.Contains(out, local) {
		t.Fatalf("home init --local output %q did not name %q", out, local)
	}
	if _, err := os.Stat(filepath.Join(local, home.ManifestName)); err != nil {
		t.Fatalf("local home manifest not created: %v", err)
	}
	out = strings.TrimSpace(captureCmdHomeStdout(t, []string{"where", "--local"}))
	if out != local {
		t.Fatalf("compound home where --local = %q, want %q", out, local)
	}
	out = strings.TrimSpace(captureCmdHomeStdout(t, []string{"where"}))
	if out == local {
		t.Fatalf("compound home where implicitly used local home after init: %q", out)
	}
}

// TestHomeEnvStillExplicitlySelectsLocalHome covers the $COMPOUND_HOME
// override: an operator who exports COMPOUND_HOME to a local home keeps
// their explicit selection even from inside an unrelated cwd.
func TestHomeEnvStillExplicitlySelectsLocalHome(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(explicit, "local", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", explicit)
	out := strings.TrimSpace(captureCmdHomeStdout(t, []string{"where"}))
	if out != explicit {
		t.Fatalf("compound home where with COMPOUND_HOME = %q, want %q", out, explicit)
	}
}

// TestHomeLocalFlagBeatsCompoundHomeForWhere is the CLI precedence rule:
// --local at the composition root beats $COMPOUND_HOME, and the env home
// must remain untouched.
func TestHomeLocalFlagBeatsCompoundHomeForWhere(t *testing.T) {
	envHome := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(envHome, "global", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envHome, "env-marker"), []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", envHome)

	repo := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(cwd, ".compound")
	out := strings.TrimSpace(captureCmdHomeStdout(t, []string{"where", "--local"}))
	if out != local {
		t.Fatalf("compound home where --local with COMPOUND_HOME = %q, want %q", out, local)
	}
	if _, err := os.Stat(filepath.Join(envHome, "env-marker")); err != nil {
		t.Fatalf("env home was touched unexpectedly: %v", err)
	}
}

// TestHomeInitLocalBeatsCompoundHomeAndLeavesEnvHomeUntouched closes the
// precedence proof for the mutating case: `home init --local` must never
// write through an inherited $COMPOUND_HOME, and the env home must stay
// byte-for-byte identical.
func TestHomeInitLocalBeatsCompoundHomeAndLeavesEnvHomeUntouched(t *testing.T) {
	envHome := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(envHome, "global", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	manifestBefore, err := os.ReadFile(filepath.Join(envHome, home.ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envHome, "env-marker"), []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", envHome)

	repo := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })

	out := captureCmdHomeStdout(t, []string{"init", "--local"})
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(cwd, ".compound")
	if !strings.Contains(out, local) {
		t.Fatalf("home init --local with COMPOUND_HOME output %q did not name %q", out, local)
	}
	if _, err := os.Stat(filepath.Join(local, home.ManifestName)); err != nil {
		t.Fatalf("local home manifest not created: %v", err)
	}
	manifestAfter, err := os.ReadFile(filepath.Join(envHome, home.ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if string(manifestAfter) != string(manifestBefore) {
		t.Fatalf("env home manifest changed during home init --local")
	}
	if got, err := os.ReadFile(filepath.Join(envHome, "env-marker")); err != nil || string(got) != "untouched" {
		t.Fatalf("env home marker changed: %q, %v", string(got), err)
	}
}
