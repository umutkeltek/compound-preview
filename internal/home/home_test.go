package home

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var archiveTestNow = time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)

func TestInitAndDoctor(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "local", time.Unix(0, 0)); err != nil {
		t.Fatalf("init: %v", err)
	}
	// idempotent: a second init must not error or destroy state.
	if err := Init(h, "local", time.Unix(0, 0)); err != nil {
		t.Fatalf("re-init: %v", err)
	}
	r := Doctor(h)
	if !r.ManifestOK {
		t.Fatal("manifest not ok")
	}
	if r.DirsOK != len(HabitatDirs()) {
		for _, d := range r.Dirs {
			if !d.OK {
				t.Errorf("missing dir: %s", d.Name)
			}
		}
		t.Fatalf("dirs %d/%d ok", r.DirsOK, len(HabitatDirs()))
	}
	if !r.OK {
		t.Fatalf("doctor not ok: %+v", r)
	}
	if r.Manifest.SchemaVersion != SchemaVersion {
		t.Fatalf("schema = %q, want %q", r.Manifest.SchemaVersion, SchemaVersion)
	}
	// the durable ledger dir must exist under the home, not the repo.
	if _, err := os.Stat(filepath.Join(h, "ledger")); err != nil {
		t.Fatalf("ledger dir missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h, "inbox", "knowledge-extractions")); err != nil {
		t.Fatalf("knowledge extraction inbox missing: %v", err)
	}
}

func TestDoctorMissingIsNotOK(t *testing.T) {
	h := t.TempDir() // empty: no manifest, no dirs
	r := Doctor(h)
	if r.OK {
		t.Fatal("empty dir reported OK")
	}
	if r.ManifestOK {
		t.Fatal("missing manifest reported OK")
	}
}

func TestDoctorSetupReportsMissingInstallDirOnPath(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example\n\ngo 1.26.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(t.TempDir(), "bin")
	report := DoctorSetup(SetupOptions{
		RepoRoot:        repo,
		HomeDir:         t.TempDir(),
		InstallDir:      installDir,
		Path:            "/usr/bin:/bin",
		GoVersionOutput: "go version go1.26.4 darwin/arm64",
		LookPath: func(name string) (string, error) {
			switch name {
			case "go":
				return "/usr/bin/go", nil
			case "compound":
				return filepath.Join(installDir, "compound"), nil
			case "bun":
				return "", os.ErrNotExist
			default:
				return "", os.ErrNotExist
			}
		},
	})
	if report.OK {
		t.Fatalf("setup report should fail when install dir missing from PATH: %+v", report)
	}
	check := setupCheckNamed(report, "setup.install_dir_on_path")
	if check.OK || check.Fix != `export PATH="`+installDir+`:$PATH"` {
		t.Fatalf("install-dir check = %+v", check)
	}
}

func TestDoctorSetupRejectsOldGoVersion(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example\n\ngo 1.26.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(t.TempDir(), "bin")
	report := DoctorSetup(SetupOptions{
		RepoRoot:        repo,
		InstallDir:      installDir,
		Path:            installDir,
		GoVersionOutput: "go version go1.25.0 darwin/arm64",
		LookPath: func(name string) (string, error) {
			switch name {
			case "go":
				return "/usr/bin/go", nil
			case "compound":
				return filepath.Join(installDir, "compound"), nil
			default:
				return "", os.ErrNotExist
			}
		},
	})
	if report.OK {
		t.Fatalf("setup report should fail old Go version: %+v", report)
	}
	check := setupCheckNamed(report, "setup.go_toolchain")
	if check.OK || !strings.Contains(check.Detail, "go1.25.0") || !strings.Contains(check.Fix, "1.26.4") {
		t.Fatalf("go check = %+v", check)
	}
}

func TestDoctorSetupReadsGoVersionWithLocalToolchain(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example\n\ngo 1.26.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(t.TempDir(), "bin")
	var sawLocal bool
	report := DoctorSetup(SetupOptions{
		RepoRoot:   repo,
		InstallDir: installDir,
		Path:       installDir,
		GoVersion: func(env []string) (string, error) {
			for _, item := range env {
				if item == "GOTOOLCHAIN=local" {
					sawLocal = true
				}
			}
			return "go version go1.26.4 darwin/arm64", nil
		},
		LookPath: func(name string) (string, error) {
			switch name {
			case "go":
				return "/usr/bin/go", nil
			case "compound":
				return filepath.Join(installDir, "compound"), nil
			default:
				return "", os.ErrNotExist
			}
		},
	})
	if !sawLocal {
		t.Fatal("DoctorSetup did not force GOTOOLCHAIN=local for go version")
	}
	if check := setupCheckNamed(report, "setup.go_toolchain"); !check.OK || !strings.Contains(check.Detail, "go1.26.4") {
		t.Fatalf("go check = %+v", check)
	}
}

func TestResolvePrecedence(t *testing.T) {
	env := t.TempDir()
	t.Setenv("COMPOUND_HOME", env)
	got, err := Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if got != env {
		t.Fatalf("env home = %q, want %q", got, env)
	}
	flag := t.TempDir()
	if got, _ = Resolve(flag); got != flag {
		t.Fatalf("--home = %q, want %q (flag must beat env)", got, flag)
	}
}

func TestResolveHomePrecedenceProvenanceAndCanonicalPaths(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	envRel := filepath.Join("env-home")
	t.Setenv("COMPOUND_HOME", envRel)
	got, err := ResolveHome("")
	if err != nil {
		t.Fatal(err)
	}
	wantEnv, err := filepath.Abs(envRel)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != wantEnv || got.Source != ResolutionSourceEnvironment {
		t.Fatalf("env resolution = %+v, want path %q source %q", got, wantEnv, ResolutionSourceEnvironment)
	}

	flagRel := filepath.Join("flag-home")
	got, err = ResolveHome(flagRel)
	if err != nil {
		t.Fatal(err)
	}
	wantFlag, err := filepath.Abs(flagRel)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != wantFlag || got.Source != ResolutionSourceFlag {
		t.Fatalf("flag resolution = %+v, want path %q source %q", got, wantFlag, ResolutionSourceFlag)
	}
}

func TestResolveHomeExplicitPathsDoNotValidateManifest(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	envRel := filepath.Join("bad-env-home")
	writeManifestFile(t, filepath.Join(envRel, ManifestName), "schema_version: compound.home.v9.9\nhome_kind: wrong\n")
	t.Setenv("COMPOUND_HOME", envRel)
	got, err := ResolveHome("")
	if err != nil {
		t.Fatal(err)
	}
	wantEnv, err := filepath.Abs(envRel)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != wantEnv || got.Source != ResolutionSourceEnvironment {
		t.Fatalf("env resolution = %+v, want path %q source %q", got, wantEnv, ResolutionSourceEnvironment)
	}

	flagRel := filepath.Join("bad-flag-home")
	writeManifestFile(t, filepath.Join(flagRel, ManifestName), "schema_version: [")
	got, err = ResolveHome(flagRel)
	if err != nil {
		t.Fatal(err)
	}
	wantFlag, err := filepath.Abs(flagRel)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != wantFlag || got.Source != ResolutionSourceFlag {
		t.Fatalf("flag resolution = %+v, want path %q source %q", got, wantFlag, ResolutionSourceFlag)
	}
}

func TestResolveHomeNearestValidAncestorIsNotAuthority(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "repo")
	child := filepath.Join(parent, "pkg")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAuthoritativeManifest(t, filepath.Join(parent, ".compound", ManifestName))
	writeManifestFile(t, filepath.Join(child, ".compound", ManifestName), "schema_version: compound.home.v0.1\nhome_kind: global\n")

	t.Setenv("COMPOUND_HOME", "")
	t.Chdir(child)
	got, err := ResolveHome("")
	if err != nil {
		t.Fatal(err)
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	wantGlobal := filepath.Join(homeDir, ".compound")
	if got.Path == filepath.Join(parent, ".compound") || got.Source == ResolutionSourceEnrolledLocal {
		t.Fatalf("authoritative Resolve adopted ancestor authority: %+v", got)
	}
	if got.Path != wantGlobal || got.Source != ResolutionSourceGlobalDefault {
		t.Fatalf("authoritative Resolve = %+v, want global default %q source %q", got, wantGlobal, ResolutionSourceGlobalDefault)
	}
	if d := DiscoverAncestorHome(); d != filepath.Join(parent, ".compound") {
		t.Fatalf("DiscoverAncestorHome() = %q, want ancestor %q", d, filepath.Join(parent, ".compound"))
	}
}

func TestResolveHomeLegacyLocalInitIsNotAuthority(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	child := filepath.Join(repo, "pkg")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", "")
	t.Chdir(repo)
	localHome, err := LocalPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := Init(localHome, "local", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	t.Chdir(child)

	got, err := ResolveHome("")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path == localHome || got.Source == ResolutionSourceEnrolledLocal {
		t.Fatalf("authoritative Resolve adopted legacy-local ancestor authority: %+v", got)
	}
	if d := DiscoverAncestorHome(); d != localHome {
		t.Fatalf("DiscoverAncestorHome() = %q, want legacy-local ancestor %q", d, localHome)
	}
}

func TestResolveHomeSkipsInvalidAncestorManifests(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, manifestPath string)
	}{
		{
			name: "malformed",
			setup: func(t *testing.T, manifestPath string) {
				writeManifestFile(t, manifestPath, "schema_version: [")
			},
		},
		{
			name: "future-schema",
			setup: func(t *testing.T, manifestPath string) {
				writeManifestFile(t, manifestPath, "schema_version: compound.home.v9.9\nhome_kind: local_monolith\n")
			},
		},
		{
			name: "wrong-kind",
			setup: func(t *testing.T, manifestPath string) {
				writeManifestFile(t, manifestPath, "schema_version: compound.home.v0.1\nhome_kind: global\n")
			},
		},
		{
			name: "oversized",
			setup: func(t *testing.T, manifestPath string) {
				writeManifestFile(t, manifestPath, strings.Repeat("a", manifestMaxBytes+1))
			},
		},
		{
			name: "symlink",
			setup: func(t *testing.T, manifestPath string) {
				target := filepath.Join(t.TempDir(), "manifest.yaml")
				writeManifestFile(t, target, "schema_version: compound.home.v0.1\nhome_kind: local_monolith\n")
				if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, manifestPath); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "nonregular",
			setup: func(t *testing.T, manifestPath string) {
				if err := os.MkdirAll(manifestPath, 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			cwd := filepath.Join(root, "repo", "pkg")
			if err := os.MkdirAll(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(cwd, ".compound", ManifestName)
			tc.setup(t, manifestPath)

			homeDir := filepath.Join(root, "home")
			t.Setenv("HOME", homeDir)
			t.Setenv("COMPOUND_HOME", "")
			t.Chdir(cwd)
			got, err := ResolveHome("")
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(homeDir, ".compound")
			if got.Path != want || got.Source != ResolutionSourceGlobalDefault {
				t.Fatalf("resolution = %+v, want path %q source %q", got, want, ResolutionSourceGlobalDefault)
			}
		})
	}
}

func TestResolveHomeGlobalFallback(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "repo")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	homeDir := filepath.Join(root, "home")
	t.Setenv("HOME", homeDir)
	t.Setenv("COMPOUND_HOME", "")
	t.Chdir(cwd)

	got, err := ResolveHome("")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(homeDir, ".compound")
	if got.Path != want || got.Source != ResolutionSourceGlobalDefault {
		t.Fatalf("global fallback = %+v, want path %q source %q", got, want, ResolutionSourceGlobalDefault)
	}
}

// TestResolveDoesNotAdoptAncestorHome covers the strict-resolver guarantee:
// a valid ancestor .compound manifest must never be authority for an
// ambient cwd. Authoritative Resolve only consults --home, $COMPOUND_HOME,
// and the global default; ancestor discovery belongs to the non-authoritative
// compatibility API (DiscoverAncestorHome).
func TestResolveDoesNotAdoptAncestorHome(t *testing.T) {
	t.Setenv("COMPOUND_HOME", "")
	root := t.TempDir()
	ancestor := filepath.Join(root, ".compound")
	if err := Init(ancestor, "local", archiveTestNow); err != nil {
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

	got, err := Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if got == ancestor {
		t.Fatalf("Resolve adopted ancestor authority %q", got)
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(homeDir, ".compound")
	if got != want {
		t.Fatalf("Resolve() = %q, want global default %q", got, want)
	}
}

// TestResolveExplicitFlagAndEnvStillSelectLocalHome locks in the explicit
// selectors: a --home path or $COMPOUND_HOME path still selects the local
// home an operator explicitly pointed at, even when a sibling .compound
// exists.
func TestResolveExplicitFlagAndEnvStillSelectLocalHome(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), ".compound")
	if err := Init(explicit, "local", archiveTestNow); err != nil {
		t.Fatal(err)
	}
	if got, err := Resolve(explicit); err != nil || got != explicit {
		t.Fatalf("Resolve(flag) = %q, %v; want %q", got, err, explicit)
	}
	t.Setenv("COMPOUND_HOME", explicit)
	if got, err := Resolve(""); err != nil || got != explicit {
		t.Fatalf("Resolve(env) = %q, %v; want %q", got, err, explicit)
	}
}

// TestDiscoverAncestorHomeIsNonAuthoritativeCompatibilityAPI confirms the
// ancestor-discovery API still surfaces the nearest valid manifest (read-
// only), while authoritative Resolve ignores it.
func TestDiscoverAncestorHomeIsNonAuthoritativeCompatibilityAPI(t *testing.T) {
	t.Setenv("COMPOUND_HOME", "")
	root := t.TempDir()
	ancestor := filepath.Join(root, ".compound")
	if err := Init(ancestor, "local", archiveTestNow); err != nil {
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

	gotDiscovery := DiscoverAncestorHome()
	gotDiscoveryReal, err := filepath.EvalSymlinks(gotDiscovery)
	if err != nil {
		t.Fatal(err)
	}
	ancestorReal, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		t.Fatal(err)
	}
	if gotDiscoveryReal != ancestorReal {
		t.Fatalf("DiscoverAncestorHome() = %q, want %q", gotDiscovery, ancestor)
	}
	if got, err := Resolve(""); err != nil || got == gotDiscovery {
		t.Fatalf("Resolve() = %q, %v; discovery API must not affect authority", got, err)
	}
}

func writeAuthoritativeManifest(t *testing.T, path string) {
	t.Helper()
	writeManifestFile(t, path, "schema_version: compound.home.v0.1\nhome_kind: local_monolith\n")
}

func writeManifestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBackupVerifyRestoreRoundTrip(t *testing.T) {
	now := archiveTestNow
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "local", now); err != nil {
		t.Fatal(err)
	}
	writeHomeFile(t, h, "ledger/claims.json", `{"claim":"durable"}`)
	writeHomeFile(t, h, "events/receipt.json", `{"receipt":"durable"}`)
	writeHomeFile(t, h, "tmp/cache.txt", "drop")
	writeHomeFile(t, h, "indexes/cache.db", "drop")
	writeHomeFile(t, h, "projections/view.json", "drop")

	archivePath := filepath.Join(t.TempDir(), "backup.tar.gz")
	report, err := Backup(h, archivePath, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Kind != ArchiveKindBackup || report.Files == 0 {
		t.Fatalf("backup report = %+v", report)
	}
	if !slices.Contains(report.Skipped, "tmp") || !slices.Contains(report.Skipped, "indexes") {
		t.Fatalf("backup skipped roots = %v", report.Skipped)
	}
	verify, err := VerifyArchive(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if !verify.OK || verify.Kind != ArchiveKindBackup {
		t.Fatalf("verify = %+v", verify)
	}

	restored := filepath.Join(t.TempDir(), ".compound-restored")
	restore, err := RestoreArchive(archivePath, restored, now)
	if err != nil {
		t.Fatal(err)
	}
	if restore.Kind != ArchiveKindBackup || !Doctor(restored).OK {
		t.Fatalf("restore = %+v doctor=%+v", restore, Doctor(restored))
	}
	if got := mustReadHomeFile(t, restored, "ledger/claims.json"); got != `{"claim":"durable"}` {
		t.Fatalf("restored ledger = %q", got)
	}
	assertMissing(t, filepath.Join(restored, "tmp/cache.txt"))
	assertMissing(t, filepath.Join(restored, "indexes/cache.db"))
	assertMissing(t, filepath.Join(restored, "projections/view.json"))
}

func TestExportPublicFixtureOmitsRuntimeData(t *testing.T) {
	now := archiveTestNow
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "global", now); err != nil {
		t.Fatal(err)
	}
	writeHomeFile(t, h, "events/private.json", `{"private":true}`)
	writeHomeFile(t, h, "ledger/private.json", `{"private":true}`)
	writeHomeFile(t, h, "context/packs/private.json", `{"private":true}`)

	archivePath := filepath.Join(t.TempDir(), "fixture.tar.gz")
	report, err := ExportPublicFixture(h, archivePath, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Kind != ArchiveKindPublicFixture || report.Files != 2 {
		t.Fatalf("fixture report = %+v", report)
	}
	verify, err := VerifyArchive(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if !verify.OK || verify.Kind != ArchiveKindPublicFixture {
		t.Fatalf("verify = %+v", verify)
	}
	names := archiveNames(t, archivePath)
	for _, want := range []string{ManifestName, PublicFixtureSummaryName, ArchiveManifestName} {
		if !slices.Contains(names, want) {
			t.Fatalf("fixture archive missing %s in %v", want, names)
		}
	}
	for _, name := range names {
		if strings.Contains(name, "private.json") {
			t.Fatalf("public fixture leaked private runtime entry %s", name)
		}
	}
	restoreParent := t.TempDir()
	restored := filepath.Join(restoreParent, ".compound-fixture")
	_, restoreErr := RestoreArchive(archivePath, restored, now)
	if restoreErr == nil || !strings.Contains(restoreErr.Error(), "restore requires archive kind backup") {
		t.Fatalf("restore public fixture err = %v", restoreErr)
	}
	assertMissing(t, restored)
	assertNoRestoreStaging(t, restoreParent)
}

func TestCompactClearsTransientAndOptionalRebuildableDirs(t *testing.T) {
	now := archiveTestNow
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "global", now); err != nil {
		t.Fatal(err)
	}
	writeHomeFile(t, h, "ledger/keep.json", "keep")
	writeHomeFile(t, h, "tmp/cache.txt", "drop")
	writeHomeFile(t, h, "lock/autonomy-admission.lock", "keep stable")
	writeHomeFile(t, h, "indexes/cache.db", "drop")
	writeHomeFile(t, h, "projections/view.json", "drop")
	lockBefore, err := os.Stat(filepath.Join(h, "lock", "autonomy-admission.lock"))
	if err != nil {
		t.Fatal(err)
	}

	report, err := Compact(h, CompactOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.RemovedFiles != 1 || report.RemovedBytes == 0 || slices.Contains(report.ClearedDirs, "lock") {
		t.Fatalf("compact report = %+v", report)
	}
	assertMissing(t, filepath.Join(h, "tmp/cache.txt"))
	lockAfter, err := os.Stat(filepath.Join(h, "lock", "autonomy-admission.lock"))
	if err != nil {
		t.Fatalf("coordination lock removed by compact: %v", err)
	}
	if !os.SameFile(lockBefore, lockAfter) {
		t.Fatal("compact replaced the autonomy admission lock inode")
	}
	if got := mustReadHomeFile(t, h, "lock/autonomy-admission.lock"); got != "keep stable" {
		t.Fatalf("coordination lock changed: %q", got)
	}
	if got := mustReadHomeFile(t, h, "ledger/keep.json"); got != "keep" {
		t.Fatalf("ledger changed: %q", got)
	}
	if got := mustReadHomeFile(t, h, "indexes/cache.db"); got != "drop" {
		t.Fatalf("indexes should remain without rebuildable compact: %q", got)
	}

	report, err = Compact(h, CompactOptions{Rebuildable: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.RemovedFiles != 2 {
		t.Fatalf("rebuildable compact report = %+v", report)
	}
	assertMissing(t, filepath.Join(h, "indexes/cache.db"))
	assertMissing(t, filepath.Join(h, "projections/view.json"))
	for _, dir := range []string{"tmp", "lock", "indexes", "projections"} {
		if fi, err := os.Stat(filepath.Join(h, dir)); err != nil || !fi.IsDir() {
			t.Fatalf("compacted dir %s not recreated: %v", dir, err)
		}
	}
}

func TestOperationalArchiveMemoryCandidateLandfill(t *testing.T) {
	now := archiveTestNow
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "global", now); err != nil {
		t.Fatal(err)
	}
	// Terminal landfill fuel.
	writeHomeFile(t, h, "inbox/memory-candidates/cand_expired.json", `{"id":"cand_expired","state":"no_promote","reason":"parked_stale_never_reinforced"}`)
	writeHomeFile(t, h, "inbox/memory-candidates/cand_rejected.json", `{"id":"cand_rejected","state":"rejected"}`)
	writeHomeFile(t, h, "inbox/memory-candidates/cand_promoted.json", `{"id":"cand_promoted","state":"promoted"}`)
	// Hot — must not landfill.
	writeHomeFile(t, h, "inbox/memory-candidates/cand_parked.json", `{"id":"cand_parked","state":"parked"}`)
	writeHomeFile(t, h, "inbox/memory-candidates/cand_proposed.json", `{"id":"cand_proposed","state":"proposed"}`)
	// Claims ledger must remain untouched (provenance).
	writeHomeFile(t, h, "ledger/claims.jsonl", `{"id":"claim_1","entity":"e","predicate":"p","object":"o"}`+"\n")

	// Default kinds must NOT sweep memory-candidates.
	defaultReport, err := ArchiveOperationalHistory(h, OperationalArchiveOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if defaultReport.Counts["memory-candidate"] != 0 {
		t.Fatalf("default archive-history must not landfill memory-candidates: %+v", defaultReport)
	}
	if got := mustReadHomeFile(t, h, "inbox/memory-candidates/cand_expired.json"); !strings.Contains(got, "no_promote") {
		t.Fatalf("default sweep touched memory landfill fuel: %q", got)
	}

	report, err := ArchiveOperationalHistory(h, OperationalArchiveOptions{Kinds: []string{"memory-candidate"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.ArchivedCount != 3 || report.Counts["memory-candidate"] != 3 {
		t.Fatalf("memory landfill report = %+v, want 3 memory-candidates", report)
	}
	assertMissing(t, filepath.Join(h, "inbox/memory-candidates/cand_expired.json"))
	assertMissing(t, filepath.Join(h, "inbox/memory-candidates/cand_rejected.json"))
	assertMissing(t, filepath.Join(h, "inbox/memory-candidates/cand_promoted.json"))
	if got := mustReadHomeFile(t, h, "inbox/memory-candidates/cand_parked.json"); !strings.Contains(got, "parked") {
		t.Fatalf("parked must stay hot: %q", got)
	}
	if got := mustReadHomeFile(t, h, "inbox/memory-candidates/cand_proposed.json"); !strings.Contains(got, "proposed") {
		t.Fatalf("proposed must stay hot: %q", got)
	}
	if got := mustReadHomeFile(t, h, "ledger/claims.jsonl"); !strings.Contains(got, "claim_1") {
		t.Fatalf("claims ledger must stay: %q", got)
	}

	restore, err := RestoreOperationalArchive(h, OperationalRestoreOptions{All: true}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if restore.RestoredCount != 3 {
		t.Fatalf("restore count = %d, want 3", restore.RestoredCount)
	}
	if got := mustReadHomeFile(t, h, "inbox/memory-candidates/cand_expired.json"); !strings.Contains(got, "no_promote") {
		t.Fatalf("restored landfill row missing: %q", got)
	}
}

func TestOperationalArchiveMovesTerminalHistoryAndRestores(t *testing.T) {
	now := archiveTestNow
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "global", now); err != nil {
		t.Fatal(err)
	}
	writeHomeFile(t, h, "work/work/work_done.json", `{"id":"work_done","state":"completed"}`)
	writeHomeFile(t, h, "work/work/work_ready.json", `{"id":"work_ready","state":"ready"}`)
	writeHomeFile(t, h, "reviews/panel-verdicts/pv.json", `{"id":"pv","status":"recorded"}`)
	writeHomeFile(t, h, "reviews/panel-verdicts/pv.md", "# pv\n")
	writeHomeFile(t, h, "reviews/panel-verdicts/pv.reviewed.json", `{"id":"pv_review","status":"recorded"}`)
	writeHomeFile(t, h, "inbox/comparisons/cmp_judged.json", `{"id":"cmp_judged","state":"judged"}`)
	writeHomeFile(t, h, "inbox/comparisons/cmp_awaiting.json", `{"id":"cmp_awaiting","state":"awaiting_verdict"}`)
	writeHomeFile(t, h, "reviews/panel-verdicts/panel_verdict_cmp.json", `{"schema_version":"compound.panel_verdict.v0.1","id":"panel_verdict_cmp","target":{"kind":"comparison","ref":"cmp_judged"}}`)
	writeHomeFile(t, h, "reviews/panel-verdicts/panel_verdict_cmp.md", "# verdict\n")
	writeHomeFile(t, h, "inbox/memory-candidates/cand_memory.json", `{"id":"cand_memory","state":"parked"}`)
	writeHomeFile(t, h, "reviews/panel-verdicts/panel_verdict_memory.json", `{"schema_version":"compound.panel_verdict.v0.1","id":"panel_verdict_memory","target":{"kind":"memory_candidate","ref":"cand_memory"}}`)
	writeHomeFile(t, h, "reviews/panel-verdicts/panel_verdict_memory.md", "# memory verdict\n")

	report, err := ArchiveOperationalHistory(h, OperationalArchiveOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.ArchivedCount != 9 || report.Counts["work"] != 1 || report.Counts["review"] != 7 || report.Counts["comparison"] != 1 {
		t.Fatalf("archive report = %+v", report)
	}
	assertMissing(t, filepath.Join(h, "work/work/work_done.json"))
	assertMissing(t, filepath.Join(h, "reviews/panel-verdicts/pv.json"))
	assertMissing(t, filepath.Join(h, "reviews/panel-verdicts/pv.md"))
	assertMissing(t, filepath.Join(h, "reviews/panel-verdicts/pv.reviewed.json"))
	assertMissing(t, filepath.Join(h, "inbox/comparisons/cmp_judged.json"))
	assertMissing(t, filepath.Join(h, "reviews/panel-verdicts/panel_verdict_cmp.json"))
	assertMissing(t, filepath.Join(h, "reviews/panel-verdicts/panel_verdict_cmp.md"))
	assertMissing(t, filepath.Join(h, "reviews/panel-verdicts/panel_verdict_memory.json"))
	assertMissing(t, filepath.Join(h, "reviews/panel-verdicts/panel_verdict_memory.md"))
	if got := mustReadHomeFile(t, h, "work/work/work_ready.json"); !strings.Contains(got, "work_ready") {
		t.Fatalf("ready work should remain hot, got %q", got)
	}
	if got := mustReadHomeFile(t, h, "inbox/comparisons/cmp_awaiting.json"); !strings.Contains(got, "awaiting_verdict") {
		t.Fatalf("awaiting comparison should remain hot, got %q", got)
	}
	active, err := LoadOperationalArchiveIndex(h)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 9 {
		t.Fatalf("active archive entries = %d, want 9", len(active))
	}

	restore, err := RestoreOperationalArchive(h, OperationalRestoreOptions{All: true}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if restore.RestoredCount != 9 {
		t.Fatalf("restore report = %+v", restore)
	}
	if got := mustReadHomeFile(t, h, "work/work/work_done.json"); !strings.Contains(got, "completed") {
		t.Fatalf("restored work = %q", got)
	}
	if got := mustReadHomeFile(t, h, "reviews/panel-verdicts/pv.reviewed.json"); !strings.Contains(got, "recorded") {
		t.Fatalf("restored review sidecar = %q", got)
	}
	if got := mustReadHomeFile(t, h, "inbox/comparisons/cmp_judged.json"); !strings.Contains(got, "judged") {
		t.Fatalf("restored comparison = %q", got)
	}
	if got := mustReadHomeFile(t, h, "reviews/panel-verdicts/panel_verdict_cmp.json"); !strings.Contains(got, "cmp_judged") {
		t.Fatalf("restored panel verdict = %q", got)
	}
	if got := mustReadHomeFile(t, h, "reviews/panel-verdicts/panel_verdict_memory.json"); !strings.Contains(got, "cand_memory") {
		t.Fatalf("restored memory panel verdict = %q", got)
	}
	active, err = LoadOperationalArchiveIndex(h)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("active archive after restore = %+v", active)
	}
}

func TestOperationalArchiveMovesStaleCreatePlansAndRestores(t *testing.T) {
	now := archiveTestNow
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "global", now); err != nil {
		t.Fatal(err)
	}
	staleBody := `{"schema_version":"compound.work_create_plan.v0.1","id":"wcrp_stale","generated_at":"2026-06-29T10:00:00Z","status":"create_policy_allowed","work_id":"work_stale_plan","policy":{"allowed":true}}` + "\n"
	freshBody := `{"schema_version":"compound.work_create_plan.v0.2","id":"wcrp_fresh","generated_at":"2026-06-30T11:30:00Z","status":"create_policy_allowed","work_id":"work_fresh_plan","policy":{"allowed":true}}` + "\n"
	admittedBody := `{"schema_version":"compound.work_create_plan.v0.1","id":"wcrp_admitted","generated_at":"2026-06-29T10:00:00Z","status":"create_policy_allowed","work_id":"work_admitted_plan","policy":{"allowed":true}}` + "\n"
	writeHomeFile(t, h, "create-plans/wcrp_stale.json", staleBody)
	writeHomeFile(t, h, "create-plans/wcrp_fresh.json", freshBody)
	writeHomeFile(t, h, "create-plans/wcrp_admitted.json", admittedBody)
	writeHomeFile(t, h, "work/work/work_admitted_plan.json", `{"id":"work_admitted_plan","state":"ready"}`)

	report, err := ArchiveOperationalHistory(h, OperationalArchiveOptions{Kinds: []string{"create-plan"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.ArchivedCount != 1 || report.Counts["create-plan"] != 1 || report.Entries[0].OriginalPath != "create-plans/wcrp_stale.json" {
		t.Fatalf("create-plan archive report = %+v", report)
	}
	assertMissing(t, filepath.Join(h, "create-plans/wcrp_stale.json"))
	if got := mustReadHomeFile(t, h, "create-plans/wcrp_fresh.json"); got != freshBody {
		t.Fatalf("fresh create-plan changed: %q", got)
	}
	if got := mustReadHomeFile(t, h, "create-plans/wcrp_admitted.json"); got != admittedBody {
		t.Fatalf("admitted create-plan changed: %q", got)
	}

	restore, err := RestoreOperationalArchive(h, OperationalRestoreOptions{IDs: []string{report.Entries[0].ID}}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if restore.RestoredCount != 1 {
		t.Fatalf("restore report = %+v", restore)
	}
	if got := mustReadHomeFile(t, h, "create-plans/wcrp_stale.json"); got != staleBody {
		t.Fatalf("restored stale create-plan not byte-identical:\n got %q\nwant %q", got, staleBody)
	}
	report, err = ArchiveOperationalHistory(h, OperationalArchiveOptions{
		Kinds:  []string{"create-plan"},
		Paths:  []string{"create-plans/wcrp_fresh.json"},
		DryRun: true,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.ArchivedCount != 1 || report.Counts["create-plan"] != 1 || report.Entries[0].Status != "explicit_unadmitted" || report.Entries[0].OriginalPath != "create-plans/wcrp_fresh.json" {
		t.Fatalf("explicit fresh create-plan dry-run = %+v", report)
	}
	if got := mustReadHomeFile(t, h, "create-plans/wcrp_fresh.json"); got != freshBody {
		t.Fatalf("dry-run moved fresh create-plan:\n got %q\nwant %q", got, freshBody)
	}
	report, err = ArchiveOperationalHistory(h, OperationalArchiveOptions{
		Kinds: []string{"create-plan"},
		Paths: []string{"create-plans/wcrp_fresh.json"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.ArchivedCount != 1 || report.Entries[0].Status != "explicit_unadmitted" {
		t.Fatalf("explicit fresh create-plan archive = %+v", report)
	}
	if _, err := os.Stat(filepath.Join(h, "create-plans", "wcrp_fresh.json")); !os.IsNotExist(err) {
		t.Fatalf("fresh create-plan should move out of hot store: %v", err)
	}
	report, err = ArchiveOperationalHistory(h, OperationalArchiveOptions{
		Kinds:  []string{"create-plan"},
		Paths:  []string{"create-plans/wcrp_admitted.json"},
		DryRun: true,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.ArchivedCount != 0 {
		t.Fatalf("admitted create-plan must not archive by explicit path: %+v", report)
	}
	if got := mustReadHomeFile(t, h, "create-plans/wcrp_admitted.json"); got != admittedBody {
		t.Fatalf("admitted create-plan changed:\n got %q\nwant %q", got, admittedBody)
	}
}

func TestOperationalArchiveKeepsOperatorPendingPanelVerdictsHot(t *testing.T) {
	now := archiveTestNow
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "global", now); err != nil {
		t.Fatal(err)
	}

	writeHomeFile(t, h, "inbox/comparisons/cmp_judged.json", `{"id":"cmp_judged","state":"judged"}`)
	writeHomeFile(t, h, "reviews/panel-verdicts/panel_verdict_park.json", `{"schema_version":"compound.panel_verdict.v0.1","id":"panel_verdict_park","target":{"kind":"comparison","ref":"cmp_judged"},"synthesis":{"decision":"park"}}`)
	writeHomeFile(t, h, "reviews/panel-verdicts/panel_verdict_park.md", "# park\n")
	writeHomeFile(t, h, "reviews/panel-verdicts/panel_verdict_needs_human.json", `{"schema_version":"compound.panel_verdict.v0.1","id":"panel_verdict_needs_human","target":{"kind":"comparison","ref":"cmp_judged"},"synthesis":{"decision":"needs_human"}}`)
	writeHomeFile(t, h, "reviews/panel-verdicts/panel_verdict_needs_human.md", "# needs human\n")
	writeHomeFile(t, h, "reviews/panel-verdicts/panel_verdict_needs_trial.json", `{"schema_version":"compound.panel_verdict.v0.1","id":"panel_verdict_needs_trial","target":{"kind":"comparison","ref":"cmp_judged"},"synthesis":{"decision":"needs_trial"}}`)
	writeHomeFile(t, h, "reviews/panel-verdicts/panel_verdict_needs_trial.md", "# needs trial\n")
	writeHomeFile(t, h, "reviews/panel-verdicts/panel_verdict_legacy_reviewed.json", `{"schema_version":"compound.panel_verdict.v0.1","id":"panel_verdict_legacy_reviewed","target":{"kind":"comparison","ref":"cmp_judged"},"synthesis":{"decision":"needs_human"}}`)
	writeHomeFile(t, h, "reviews/panel-verdicts/panel_verdict_legacy_reviewed.md", "# legacy reviewed\n")
	writeHomeFile(t, h, "reviews/panel-verdicts/panel_verdict_legacy_reviewed.reviewed.json", `{"schema_version":"compound.panel_verdict_human_review.v0.1","id":"pvr_legacy","status":"recorded"}`)
	writeHomeFile(t, h, "reviews/panel-verdicts/panel_verdict_operator_reviewed.json", `{"schema_version":"compound.panel_verdict.v0.1","id":"panel_verdict_operator_reviewed","target":{"kind":"comparison","ref":"cmp_judged"},"synthesis":{"decision":"needs_human"}}`)
	writeHomeFile(t, h, "reviews/panel-verdicts/panel_verdict_operator_reviewed.md", "# operator reviewed\n")
	writeHomeFile(t, h, "reviews/panel-verdicts/panel_verdict_operator_reviewed.reviewed.json", `{"schema_version":"compound.panel_verdict_human_review.v0.1","id":"pvr_operator","status":"recorded","reviewed_by":"operator"}`)

	report, err := ArchiveOperationalHistory(h, OperationalArchiveOptions{Kinds: []string{"review"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.ArchivedCount != 5 || report.Counts["review"] != 5 {
		t.Fatalf("archive report = %+v, want only park plus operator-reviewed sidecar group", report)
	}

	assertMissing(t, filepath.Join(h, "reviews/panel-verdicts/panel_verdict_park.json"))
	assertMissing(t, filepath.Join(h, "reviews/panel-verdicts/panel_verdict_park.md"))
	assertMissing(t, filepath.Join(h, "reviews/panel-verdicts/panel_verdict_operator_reviewed.json"))
	assertMissing(t, filepath.Join(h, "reviews/panel-verdicts/panel_verdict_operator_reviewed.md"))
	assertMissing(t, filepath.Join(h, "reviews/panel-verdicts/panel_verdict_operator_reviewed.reviewed.json"))
	if got := mustReadHomeFile(t, h, "reviews/panel-verdicts/panel_verdict_needs_human.json"); !strings.Contains(got, "needs_human") {
		t.Fatalf("needs_human verdict should remain hot, got %q", got)
	}
	if got := mustReadHomeFile(t, h, "reviews/panel-verdicts/panel_verdict_needs_trial.json"); !strings.Contains(got, "needs_trial") {
		t.Fatalf("needs_trial verdict should remain hot, got %q", got)
	}
	if got := mustReadHomeFile(t, h, "reviews/panel-verdicts/panel_verdict_legacy_reviewed.reviewed.json"); !strings.Contains(got, "pvr_legacy") {
		t.Fatalf("review sidecar without reviewed_by should remain hot, got %q", got)
	}
}

// A disposition recorded at the legacy doubled stem (`<id>.<id>.reviewed.json`)
// still terminalizes its core item. Without this the review area can never
// drain: the core item keeps its non-terminal producer status forever and the
// only recorded evidence of the operator's decision is invisible to the sweep.
func TestArchiveHistoryDrainsReviewWithDoubledStemDisposition(t *testing.T) {
	now := archiveTestNow
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "global", now); err != nil {
		t.Fatal(err)
	}

	writeHomeFile(t, h, "reviews/source-control-cleanup/scr_a1b2c3d4e5f6.json",
		`{"schema_version":"compound.source_control_review_plan.v0.1","id":"scr_a1b2c3d4e5f6","status":"review_required"}`)
	writeHomeFile(t, h, "reviews/source-control-cleanup/scr_a1b2c3d4e5f6.review.yaml", "decision: parked\n")
	writeHomeFile(t, h, "reviews/source-control-cleanup/scr_a1b2c3d4e5f6.scr_a1b2c3d4e5f6.reviewed.json",
		`{"schema_version":"compound.source_control_review_report.v0.1","id":"scrr_a1b2c3d4e5f6","status":"recorded"}`)
	writeHomeFile(t, h, "reviews/source-control-cleanup/scr_f6e5d4c3b2a1.json",
		`{"schema_version":"compound.source_control_review_plan.v0.1","id":"scr_f6e5d4c3b2a1","status":"review_required"}`)

	report, err := ArchiveOperationalHistory(h, OperationalArchiveOptions{Kinds: []string{"review"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.ArchivedCount != 3 || report.Counts["review"] != 3 {
		t.Fatalf("archive report = %+v, want the doubled-stem disposition group drained", report)
	}
	assertMissing(t, filepath.Join(h, "reviews/source-control-cleanup/scr_a1b2c3d4e5f6.json"))
	assertMissing(t, filepath.Join(h, "reviews/source-control-cleanup/scr_a1b2c3d4e5f6.review.yaml"))
	assertMissing(t, filepath.Join(h, "reviews/source-control-cleanup/scr_a1b2c3d4e5f6.scr_a1b2c3d4e5f6.reviewed.json"))
	if got := mustReadHomeFile(t, h, "reviews/source-control-cleanup/scr_f6e5d4c3b2a1.json"); !strings.Contains(got, "review_required") {
		t.Fatalf("undisposed source-control review should remain hot, got %q", got)
	}
}

func TestArchiveHistoryDrainsAllTerminalWorkInOnePass(t *testing.T) {
	now := archiveTestNow
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "global", now); err != nil {
		t.Fatal(err)
	}
	writeHomeFile(t, h, "work/work/work_completed.json", `{"id":"work_completed","state":"completed"}`)
	writeHomeFile(t, h, "work/work/work_failed.json", `{"id":"work_failed","state":"failed"}`)
	writeHomeFile(t, h, "work/work/work_cancelled.json", `{"id":"work_cancelled","state":"cancelled"}`)
	running := `{"id":"work_running","state":"running"}`
	writeHomeFile(t, h, "work/work/work_running.json", running)

	report, err := ArchiveOperationalHistory(h, OperationalArchiveOptions{Kinds: []string{"work"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.ArchivedCount != 3 || report.Counts["work"] != 3 {
		t.Fatalf("archive report = %+v, want one pass to drain all 3 terminal works", report)
	}
	entries, err := os.ReadDir(filepath.Join(h, "work", "work"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "work_running.json" {
		t.Fatalf("hot work dir after sweep = %v, want only work_running.json", entries)
	}
	if got := mustReadHomeFile(t, h, "work/work/work_running.json"); got != running {
		t.Fatalf("running work mutated by sweep: %q", got)
	}
	active, err := LoadOperationalArchiveIndex(h)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 3 {
		t.Fatalf("archive index rows = %d (%+v), want exactly 3", len(active), active)
	}
	for _, entry := range active {
		if entry.Kind != "work" {
			t.Fatalf("archive index row kind = %+v, want work", entry)
		}
	}
}

func TestArchiveHistorySweepsStaleLeasesAndKeepsLiveLeases(t *testing.T) {
	now := archiveTestNow
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "global", now); err != nil {
		t.Fatal(err)
	}
	// live work (non-terminal, hot): its lease must survive the sweep.
	writeHomeFile(t, h, "work/work/work_running.json", `{"id":"work_running","state":"running"}`)
	writeHomeFile(t, h, "work/leases/work_running.json", `{"work_id":"work_running","by":"codex","claimed_at":"1"}`)
	writeHomeFile(t, h, "work/work/work_ready.json", `{"id":"work_ready","state":"ready"}`)
	writeHomeFile(t, h, "work/leases/work_ready.json", `{"work_id":"work_ready","by":"codex","claimed_at":"2"}`)
	// terminal-but-hot work: the work and its lease archive in the same pass.
	writeHomeFile(t, h, "work/work/work_done.json", `{"id":"work_done","state":"completed"}`)
	writeHomeFile(t, h, "work/leases/work_done.json", `{"work_id":"work_done","by":"codex","claimed_at":"3"}`)
	// orphan lease: no work file at all.
	writeHomeFile(t, h, "work/leases/work_ghost.json", `{"work_id":"work_ghost","by":"codex","claimed_at":"4"}`)

	report, err := ArchiveOperationalHistory(h, OperationalArchiveOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.ArchivedCount != 3 || report.Counts["work"] != 1 || report.Counts["lease"] != 2 {
		t.Fatalf("archive report = %+v, want 1 terminal work + 2 stale leases", report)
	}
	assertMissing(t, filepath.Join(h, "work/work/work_done.json"))
	assertMissing(t, filepath.Join(h, "work/leases/work_done.json"))
	assertMissing(t, filepath.Join(h, "work/leases/work_ghost.json"))
	if got := mustReadHomeFile(t, h, "work/leases/work_running.json"); !strings.Contains(got, "work_running") {
		t.Fatalf("lease for running work must survive sweep, got %q", got)
	}
	if got := mustReadHomeFile(t, h, "work/leases/work_ready.json"); !strings.Contains(got, "work_ready") {
		t.Fatalf("lease for ready work must survive sweep, got %q", got)
	}
	active, err := LoadOperationalArchiveIndex(h)
	if err != nil {
		t.Fatal(err)
	}
	statusByPath := map[string]string{}
	for _, entry := range active {
		statusByPath[entry.OriginalPath] = entry.Status
	}
	if statusByPath["work/leases/work_done.json"] != "completed" {
		t.Fatalf("terminal-work lease status = %q, want completed", statusByPath["work/leases/work_done.json"])
	}
	if statusByPath["work/leases/work_ghost.json"] != "orphaned" {
		t.Fatalf("orphan lease status = %q, want orphaned", statusByPath["work/leases/work_ghost.json"])
	}
}

func TestArchiveHistoryLeaseKindSweepsLeaseForArchivedWork(t *testing.T) {
	now := archiveTestNow
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "global", now); err != nil {
		t.Fatal(err)
	}
	writeHomeFile(t, h, "work/work/work_done.json", `{"id":"work_done","state":"completed"}`)
	writeHomeFile(t, h, "work/leases/work_done.json", `{"work_id":"work_done","by":"codex","claimed_at":"1"}`)

	// explicit --kind work stays exact: it archives work envelopes only, so the
	// CLI contract (main_test archive-history --kind work --kind review counts)
	// is unchanged. The lease is left behind — the verified live gap.
	first, err := ArchiveOperationalHistory(h, OperationalArchiveOptions{Kinds: []string{"work"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.ArchivedCount != 1 || first.Counts["work"] != 1 || first.Counts["lease"] != 0 {
		t.Fatalf("work-kind report = %+v, want exactly the work envelope", first)
	}
	if got := mustReadHomeFile(t, h, "work/leases/work_done.json"); !strings.Contains(got, "work_done") {
		t.Fatalf("explicit work-kind pass must not touch leases, got %q", got)
	}

	// the standalone lease sweep drains the lease whose work is already archived.
	second, err := ArchiveOperationalHistory(h, OperationalArchiveOptions{Kinds: []string{"lease"}}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if second.ArchivedCount != 1 || second.Counts["lease"] != 1 {
		t.Fatalf("lease-kind report = %+v, want the leftover lease", second)
	}
	assertMissing(t, filepath.Join(h, "work/leases/work_done.json"))
	active, err := LoadOperationalArchiveIndex(h)
	if err != nil {
		t.Fatal(err)
	}
	var lease OperationalArchiveEntry
	for _, entry := range active {
		if entry.OriginalPath == "work/leases/work_done.json" {
			lease = entry
		}
	}
	if lease.Kind != "lease" || lease.Status != "work_archived" || lease.SHA256 == "" {
		t.Fatalf("lease archive entry = %+v, want kind=lease status=work_archived with sha256", lease)
	}
	if lease.ArchivePath != "archives/operational/files/work/leases/work_done.json" {
		t.Fatalf("lease archive path = %q", lease.ArchivePath)
	}
}

func TestDoctorOperationalHygieneFailsDirtyAndPassesAfterSweep(t *testing.T) {
	now := archiveTestNow
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "global", now); err != nil {
		t.Fatal(err)
	}
	writeHomeFile(t, h, "work/work/work_done.json", `{"id":"work_done","state":"completed"}`)
	writeHomeFile(t, h, "work/leases/work_done.json", `{"work_id":"work_done","by":"codex","claimed_at":"1"}`)
	writeHomeFile(t, h, "work/leases/work_ghost.json", `{"work_id":"work_ghost","by":"codex","claimed_at":"2"}`)
	writeHomeFile(t, h, "work/work/work_running.json", `{"id":"work_running","state":"running"}`)
	writeHomeFile(t, h, "work/leases/work_running.json", `{"work_id":"work_running","by":"codex","claimed_at":"3"}`)

	dirty := Doctor(h)
	if !dirty.OK {
		t.Fatalf("hygiene must not gate doctor OK (the sweep needs a healthy home to run): %+v", dirty)
	}
	if dirty.HygieneOK {
		t.Fatal("dirty home reported hygiene OK")
	}
	hygiene, err := OperationalHygiene(h)
	if err != nil {
		t.Fatal(err)
	}
	if hygiene.OK || len(hygiene.TerminalHotWork) != 1 || len(hygiene.StaleLeases) != 2 {
		t.Fatalf("hygiene report = %+v, want 1 terminal hot work + 2 stale leases", hygiene)
	}
	if hygiene.TerminalHotWork[0] != "work/work/work_done.json" {
		t.Fatalf("terminal hot work = %v", hygiene.TerminalHotWork)
	}

	if _, err := ArchiveOperationalHistory(h, OperationalArchiveOptions{}, now); err != nil {
		t.Fatal(err)
	}

	swept := Doctor(h)
	if !swept.OK || !swept.HygieneOK {
		t.Fatalf("swept home doctor = OK %v HygieneOK %v, want both true", swept.OK, swept.HygieneOK)
	}
	for _, item := range swept.Hygiene {
		if !item.OK {
			t.Fatalf("hygiene check %s still failing after sweep", item.Name)
		}
	}
	if got := mustReadHomeFile(t, h, "work/work/work_running.json"); !strings.Contains(got, "running") {
		t.Fatalf("running work must stay hot, got %q", got)
	}
	if got := mustReadHomeFile(t, h, "work/leases/work_running.json"); !strings.Contains(got, "work_running") {
		t.Fatalf("running work lease must stay hot, got %q", got)
	}
}

func TestRestoreHistoryRestoresSweptLeaseByteIdentical(t *testing.T) {
	now := archiveTestNow
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "global", now); err != nil {
		t.Fatal(err)
	}
	leaseBody := `{"work_id":"work_ghost","by":"codex","claimed_at":"archive-fixture"}` + "\n"
	writeHomeFile(t, h, "work/leases/work_ghost.json", leaseBody)
	wantSum := sha256.Sum256([]byte(leaseBody))

	report, err := ArchiveOperationalHistory(h, OperationalArchiveOptions{Kinds: []string{"lease"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.ArchivedCount != 1 || len(report.Entries) != 1 {
		t.Fatalf("lease sweep report = %+v", report)
	}
	entry := report.Entries[0]
	if entry.SHA256 != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("archived lease sha256 = %q, want %q", entry.SHA256, hex.EncodeToString(wantSum[:]))
	}
	assertMissing(t, filepath.Join(h, "work/leases/work_ghost.json"))

	restore, err := RestoreOperationalArchive(h, OperationalRestoreOptions{IDs: []string{entry.ID}}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if restore.RestoredCount != 1 {
		t.Fatalf("restore report = %+v", restore)
	}
	got := mustReadHomeFile(t, h, "work/leases/work_ghost.json")
	if got != leaseBody {
		t.Fatalf("restored lease not byte-identical:\n got %q\nwant %q", got, leaseBody)
	}
	gotSum := sha256.Sum256([]byte(got))
	if hex.EncodeToString(gotSum[:]) != entry.SHA256 {
		t.Fatalf("restored lease sha256 = %q, want %q", hex.EncodeToString(gotSum[:]), entry.SHA256)
	}
	active, err := LoadOperationalArchiveIndex(h)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("active archive entries after restore = %+v, want none", active)
	}
}

func TestDoctorBackupFreshnessWarningsFollowHomeConfig(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		configure  bool
		archiveAge time.Duration
		wantRow    string
	}{
		{name: "stale row present", configure: true, archiveAge: 72 * time.Hour, wantRow: "backup.stale"},
		{name: "fresh row absent", configure: true, archiveAge: time.Hour},
		{name: "no config row absent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := filepath.Join(t.TempDir(), ".compound")
			if err := Init(h, "global", now); err != nil {
				t.Fatal(err)
			}
			if tt.configure {
				backupDir := filepath.Join(t.TempDir(), "backups")
				if err := os.MkdirAll(backupDir, 0o755); err != nil {
					t.Fatal(err)
				}
				writeHomeFile(t, h, "config/home.yaml", "schema_version: "+HomeConfigSchemaVersion+"\nbackup_dir: "+backupDir+"\n")
				archivePath := filepath.Join(backupDir, "home-fixture.tar.gz")
				if err := os.WriteFile(archivePath, []byte("fixture"), 0o644); err != nil {
					t.Fatal(err)
				}
				mod := now.Add(-tt.archiveAge)
				if err := os.Chtimes(archivePath, mod, mod); err != nil {
					t.Fatal(err)
				}
			}

			report := DoctorWithOptions(h, DoctorOptions{Now: now})
			if tt.wantRow == "" {
				if len(report.BackupWarnings) != 0 {
					t.Fatalf("backup warnings = %+v, want none", report.BackupWarnings)
				}
				return
			}
			if !checkItemNamed(report.BackupWarnings, tt.wantRow) {
				t.Fatalf("backup warnings = %+v, want %s", report.BackupWarnings, tt.wantRow)
			}
		})
	}
}

func checkItemNamed(items []CheckItem, name string) bool {
	for _, item := range items {
		if item.Name == name {
			return true
		}
	}
	return false
}

func setupCheckNamed(report SetupReport, name string) SetupCheck {
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	return SetupCheck{}
}

func writeHomeFile(t *testing.T, homePath, rel, content string) {
	t.Helper()
	path := filepath.Join(homePath, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustReadHomeFile(t *testing.T, homePath, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(homePath, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s exists or returned unexpected error: %v", path, err)
	}
}

func archiveNames(t *testing.T, archivePath string) []string {
	t.Helper()
	f, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	names := []string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
		if _, err := io.Copy(io.Discard, tr); err != nil {
			t.Fatal(err)
		}
	}
	return names
}
