package authority

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/mem"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

func TestInstalledHarnessEntrypointBindingCanonicalizesHarnessFamilies(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), "compound-home")
	installedHome := t.TempDir()
	for rel, body := range map[string]string{
		osAbsorptionServedCodexEntrypoint: "codex", osAbsorptionServedClaudeEntrypoint: "claude",
	} {
		path := filepath.Join(installedHome, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	seedInstalledHarnessHomeRegistry(t, homePath, installedHome)
	for _, tc := range []struct{ harness, family string }{
		{"claude-code", "claude"}, {"claude-zai", "claude"}, {"claude_lane_opus", "claude"}, {"codex-tap", "codex"},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			source, _, found, err := InstalledHarnessEntrypointBinding(homePath, tc.harness)
			if err != nil || !found || source != InstalledHarnessEntrypointRevisionPrefix+tc.family {
				t.Fatalf("alias binding = source %q found=%v err=%v", source, found, err)
			}
		})
	}
}

func TestInstalledHarnessEntrypointBindingRejectsSymlinkTraversal(t *testing.T) {
	for _, tc := range []struct {
		harness, dir, file string
		intermediate       bool
	}{
		{"codex", ".codex", "AGENTS.md", false},
		{"codex", ".codex", "AGENTS.md", true},
		{"claude-code", ".claude", "CLAUDE.md", true},
	} {
		t.Run(fmt.Sprintf("%s/intermediate=%v", tc.harness, tc.intermediate), func(t *testing.T) {
			homePath := filepath.Join(t.TempDir(), "compound-home")
			installedHome := t.TempDir()
			externalDir := t.TempDir()
			external := filepath.Join(externalDir, tc.file)
			if err := os.WriteFile(external, []byte("stable external"), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.intermediate {
				if err := os.Symlink(externalDir, filepath.Join(installedHome, tc.dir)); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(filepath.Join(installedHome, tc.dir), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, filepath.Join(installedHome, tc.dir, tc.file)); err != nil {
					t.Fatal(err)
				}
			}
			seedInstalledHarnessHomeRegistry(t, homePath, installedHome)
			if _, _, _, err := InstalledHarnessEntrypointBinding(homePath, tc.harness); err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("symlink traversal must fail closed, got %v", err)
			}
		})
	}
}

func TestInstalledHarnessEntrypointBindingRejectsSwapBetweenValidationAndRead(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), "compound-home")
	installedHome := t.TempDir()
	entrypoint := filepath.Join(installedHome, ".codex", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(entrypoint), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entrypoint, []byte("trusted"), 0o644); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "AGENTS.md")
	if err := os.WriteFile(external, []byte("external"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedInstalledHarnessHomeRegistry(t, homePath, installedHome)
	ops := installedEntrypointReadOps{
		beforeOpen: func(path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(external, path); err != nil {
				t.Fatal(err)
			}
		},
	}
	if _, _, _, err := installedHarnessEntrypointBindingWithOps(homePath, "codex", ops); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("swap-at-read must fail closed, got %v", err)
	}
}

func TestInstalledHarnessEntrypointBindingRejectsAncestorSwapBeforeRootOpen(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), "compound-home")
	container := t.TempDir()
	installedParent := filepath.Join(container, "installed-parent")
	installedHome := filepath.Join(installedParent, "home")
	entrypoint := filepath.Join(installedHome, ".codex", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(entrypoint), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entrypoint, []byte("trusted"), 0o644); err != nil {
		t.Fatal(err)
	}

	externalParent := t.TempDir()
	externalEntrypoint := filepath.Join(externalParent, "home", ".codex", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(externalEntrypoint), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(externalEntrypoint, []byte("external"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedInstalledHarnessHomeRegistry(t, homePath, installedHome)

	ops := installedEntrypointReadOps{
		beforeRootOpen: func(string) {
			if err := os.Rename(installedParent, installedParent+"-original"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(externalParent, installedParent); err != nil {
				t.Fatal(err)
			}
		},
	}
	if _, _, _, err := installedHarnessEntrypointBindingWithOps(homePath, "codex", ops); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("ancestor swap must fail closed, got %v", err)
	}
}

func TestInstalledHarnessEntrypointBindingRejectsFIFOWithoutBlocking(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), "compound-home")
	installedHome := t.TempDir()
	entrypoint := filepath.Join(installedHome, ".codex", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(entrypoint), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entrypoint, []byte("trusted"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedInstalledHarnessHomeRegistry(t, homePath, installedHome)

	fifoSetup := make(chan error, 1)
	ops := installedEntrypointReadOps{
		beforeOpen: func(path string) {
			if err := os.Remove(path); err != nil {
				fifoSetup <- err
				return
			}
			if err := unix.Mkfifo(path, 0o600); err != nil {
				fifoSetup <- err
				return
			}
			fifoSetup <- nil
		},
	}
	result := make(chan error, 1)
	go func() {
		_, _, _, err := installedHarnessEntrypointBindingWithOps(homePath, "codex", ops)
		result <- err
	}()
	if err := <-fifoSetup; err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("FIFO must fail closed as non-regular, got %v", err)
		}
	case <-time.After(2 * time.Second):
		writer, err := os.OpenFile(entrypoint, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		blockedErr := <-result
		t.Fatalf("FIFO rejection blocked in open; eventual error: %v", blockedErr)
	}
}

// TestAbsorbedOSRootUsesGenericRegistryKey pins the absorbed-OS seam to a
// role-named registry key. The key is the only absorbed-OS identifier in
// tracked source; WHICH control plane sits behind it is operator data, held
// as a promoted current_path claim in the local COMPOUND_HOME ledger. A key
// naming a specific operator or a specific control plane would leak private
// identity into a public clone and is the regression this falsifies.
func TestAbsorbedOSRootUsesGenericRegistryKey(t *testing.T) {
	if AbsorbedOSRegistryKey != "absorbed_os" {
		t.Fatalf("absorbed OS registry key must be role-named, got %q", AbsorbedOSRegistryKey)
	}
	// The ref must be DERIVED from the key, not a second literal that can
	// drift away from it (that drift is how a stale personal name survives).
	wantRef := "source:" + strings.ReplaceAll(AbsorbedOSRegistryKey, "_", "-")
	if absorbedOSEntityRef != wantRef || absorbedOSEntityRef != "source:absorbed-os" {
		t.Fatalf("absorbed OS entity ref = %q, want %q", absorbedOSEntityRef, wantRef)
	}
	if !strings.Contains(osAbsorptionAbsorbedOSSkipReason, AbsorbedOSRegistryKey) {
		t.Fatalf("skip reason must name the registry key: %q", osAbsorptionAbsorbedOSSkipReason)
	}
}

func TestResolveInstalledHarnessHomePropagatesMalformedLedgerOpenFailure(t *testing.T) {
	homePath := t.TempDir()
	if err := os.WriteFile(filepath.Join(homePath, "ledger"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, found, err := ResolveInstalledHarnessHome(homePath); err == nil || found {
		t.Fatalf("malformed ledger must fail closed, found=%v err=%v", found, err)
	}
}

func TestResolveInstalledHarnessHomePropagatesDanglingLedgerSymlink(t *testing.T) {
	homePath := t.TempDir()
	if err := os.Symlink(filepath.Join(homePath, "missing-ledger"), filepath.Join(homePath, "ledger")); err != nil {
		t.Fatal(err)
	}
	if _, found, err := ResolveInstalledHarnessHome(homePath); err == nil || found {
		t.Fatalf("dangling ledger symlink must fail closed, found=%v err=%v", found, err)
	}
}

// seedAbsorbedOSRegistry writes a minimal os-path-authority.yaml declaring
// osRoot as the active absorbed_os canonical root and imports it into
// compoundHome's mem ledger exactly the way `compound authority
// path-registry` does in production (RegisterEntity + Capture + Adjudicate
// promote — see cmd/compound/main.go's "path-registry" handler). Every
// absorbed-OS proof fixture seeds through this one door instead of writing
// to the ledger by hand, so the persistence trace this package's resolver
// depends on is proven, not assumed.
func seedAbsorbedOSRegistry(t *testing.T, compoundHome, osRoot string) {
	t.Helper()
	registryPath := filepath.Join(t.TempDir(), "os-path-authority.yaml")
	body, err := yaml.Marshal(osPathRegistryFile{
		Version: 1,
		CanonicalRoots: map[string]osPathRoot{
			AbsorbedOSRegistryKey: {Path: osRoot, Status: "active"},
		},
	})
	if err != nil {
		t.Fatalf("marshal absorbed-os registry fixture: %v", err)
	}
	if err := os.WriteFile(registryPath, body, 0o644); err != nil {
		t.Fatalf("write absorbed-os registry fixture: %v", err)
	}

	store, err := mem.Open(compoundHome)
	if err != nil {
		t.Fatalf("open store to seed absorbed-os registry: %v", err)
	}
	existing, err := store.Entities()
	if err != nil {
		t.Fatalf("load entities to seed absorbed-os registry: %v", err)
	}
	_, entities, facts, report, err := ImportOSPathRegistry(registryPath, existing)
	if err != nil {
		t.Fatalf("import absorbed-os registry fixture: %v", err)
	}
	if report.Status != "ok" {
		t.Fatalf("import absorbed-os registry fixture: status=%s", report.Status)
	}
	now := time.Now()
	for _, entity := range entities {
		if _, err := store.RegisterEntity(entity, now); err != nil {
			t.Fatalf("register absorbed-os entity: %v", err)
		}
	}
	captured, err := store.Capture(facts, now)
	if err != nil {
		t.Fatalf("capture absorbed-os facts: %v", err)
	}
	for _, c := range captured {
		if _, err := store.Adjudicate(c.ID, "promote", "test-seed-absorbed-os-registry", now); err != nil {
			t.Fatalf("promote absorbed-os fact %s: %v", c.ID, err)
		}
	}
}

// TestResolveAbsorbedOSRootNoLedgerSkips is the public-clone falsifier: a
// COMPOUND_HOME with no ledger at all (fresh clone, no
// os-path-authority.yaml ever imported) must resolve found=false with a nil
// error, never an error and never a fabricated root.
func TestResolveAbsorbedOSRootNoLedgerSkips(t *testing.T) {
	compoundHome := filepath.Join(t.TempDir(), "compound-home") // deliberately never created
	root, found, err := ResolveAbsorbedOSRoot(compoundHome)
	if err != nil {
		t.Fatalf("no ledger should skip, not error: %v", err)
	}
	if found {
		t.Fatalf("no ledger should resolve found=false, got root=%q", root)
	}
	if root != "" {
		t.Fatalf("no ledger should resolve an empty root, got %q", root)
	}
}

// TestResolveAbsorbedOSRootSeededRegistryResolves proves the read side of
// the single-owner data flow: once an operator has imported
// os-path-authority.yaml (the seed helper mirrors that import), the resolver
// reads the promoted current_path claim back and reports the exact
// operator-declared root.
func TestResolveAbsorbedOSRootSeededRegistryResolves(t *testing.T) {
	root := t.TempDir()
	osRoot := filepath.Join(root, "absorbed-os")
	if err := os.MkdirAll(osRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	compoundHome := filepath.Join(root, "compound-home")
	if err := home.Init(compoundHome, "test", time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	seedAbsorbedOSRegistry(t, compoundHome, osRoot)

	got, found, err := ResolveAbsorbedOSRoot(compoundHome)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want, err := filepath.EvalSymlinks(osRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !found || got != want {
		t.Fatalf("ResolveAbsorbedOSRoot = %q found=%v, want %q found=true", got, found, want)
	}
}

func TestResolveAbsorbedOSRootRejectsPostImportSymlinkRepointing(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	osRoot := filepath.Join(root, "absorbed-os")
	if err := os.MkdirAll(osRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	compoundHome := filepath.Join(root, "compound-home")
	if err := home.Init(compoundHome, "test", time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	seedAbsorbedOSRegistry(t, compoundHome, osRoot)

	movedRoot := filepath.Join(root, "absorbed-os-original")
	if err := os.Rename(osRoot, movedRoot); err != nil {
		t.Fatal(err)
	}
	repointedTarget := filepath.Join(root, "repointed-target")
	if err := os.MkdirAll(repointedTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repointedTarget, osRoot); err != nil {
		t.Fatal(err)
	}

	got, found, err := ResolveAbsorbedOSRoot(compoundHome)
	if err == nil {
		t.Fatalf("post-import symlink repointing must fail closed, got root=%q found=%v", got, found)
	}
	if !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("repointing error must name canonical path drift, got %v", err)
	}
}

// TestResolveAbsorbedOSEntrypointResolvesUnderDeclaredRoot proves the
// sub-path join for a root that is declared AND present on disk.
func TestResolveAbsorbedOSEntrypointResolvesUnderDeclaredRoot(t *testing.T) {
	root := t.TempDir()
	osRoot := filepath.Join(root, "absorbed-os")
	if err := os.MkdirAll(osRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	compoundHome := filepath.Join(root, "compound-home")
	if err := home.Init(compoundHome, "test", time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	seedAbsorbedOSRegistry(t, compoundHome, osRoot)

	got, found, err := ResolveAbsorbedOSEntrypoint(compoundHome, AbsorbedOSExecLaneEntrypoint)
	if err != nil {
		t.Fatalf("resolve entrypoint: %v", err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(osRoot)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(canonicalRoot, "exec-lane", "os.ts")
	if !found || got != want {
		t.Fatalf("ResolveAbsorbedOSEntrypoint = %q found=%v, want %q found=true", got, found, want)
	}
}

// TestResolveAbsorbedOSEntrypointDeclaredButAbsentSkips is the second
// public-clone falsifier: a declared root that is absent on disk (stale or
// moved registry entry) must skip exactly like an absent registry — never a
// hard failure.
func TestResolveAbsorbedOSEntrypointDeclaredButAbsentSkips(t *testing.T) {
	root := t.TempDir()
	osRoot := filepath.Join(root, "absorbed-os-moved-away")
	if err := os.MkdirAll(osRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	compoundHome := filepath.Join(root, "compound-home")
	if err := home.Init(compoundHome, "test", time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	seedAbsorbedOSRegistry(t, compoundHome, osRoot)
	if err := os.Remove(osRoot); err != nil {
		t.Fatal(err)
	}

	path, found, err := ResolveAbsorbedOSEntrypoint(compoundHome, AbsorbedOSExecLaneEntrypoint)
	if err != nil {
		t.Fatalf("declared-but-absent should skip, not error: %v", err)
	}
	if found {
		t.Fatalf("declared-but-absent root should resolve found=false, got path=%q", path)
	}
}
