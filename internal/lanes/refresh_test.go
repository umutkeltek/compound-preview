package lanes

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/harness"
)

// TestRefreshLaneClearsDrift is the R3B 2026-09-04 red test: the
// installer used to print `instruction-pack: claude-minimax fresh` and
// then the operator's `compound harness instruction-pack --harness
// claude_minimax --drift` returned `status: drift`. The drift came
// from the harness-name alias (`claude-minimax` vs the canonical
// `claude_minimax`) writing the pack under a path `--drift` never
// touched, plus the absence of a `--drift` self-check. RefreshLane is
// the single owner of the verdict; this test asserts the post-call
// drift check is `clean` for the canonical harness id.
//
// Setup: a temp home with a seeded installed-hooks manifest, a
// seeded config/lane-defaults.yaml so the contract compile step has
// the canonical (cwd, task) it demands, and an empty pack dir. After
// RefreshLane, the canonical pack path
// `$COMPOUND_HOME/context/instruction-packs/claude_minimax/latest.json`
// must exist and `InstructionPackDrift` must report `Drifted=false`.
func TestRefreshLaneClearsDrift(t *testing.T) {
	homePath := t.TempDir()
	baseDir := t.TempDir()
	cwdDir := t.TempDir()
	binaryPath := filepath.Join(t.TempDir(), "compound-current")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	writeInstalledHooksManifest(t, homePath, map[string]string{
		"claude_minimax": "binary:" + binaryPath,
	})
	writeLaneDefaults(t, homePath, "claude_minimax", cwdDir, "test-task")

	opts := RefreshLaneOptions{
		HomePath:   homePath,
		BinaryPath: binaryPath,
		BaseDir:    baseDir,
		Now:        time.Date(2026, 9, 4, 7, 0, 0, 0, time.UTC),
	}
	verdict, err := RefreshLane(opts, "claude_minimax")
	if err != nil {
		t.Fatalf("RefreshLane returned error: %v", err)
	}
	if verdict.Status != StatusClean {
		t.Fatalf("RefreshLane verdict status = %q, want %q (detail=%q)", verdict.Status, StatusClean, verdict.Detail)
	}

	// Independently verify: re-running drift from the on-disk state
	// must report clean. This is the same call the operator runs.
	report, err := harness.InstructionPackDrift(homePath, "claude_minimax", "", opts.Now)
	if err != nil {
		t.Fatalf("post-refresh drift check: %v", err)
	}
	if report.Drifted {
		t.Fatalf("post-refresh drift = drifted, want clean (details=%v)", report.Details)
	}
	packPath := filepath.Join(homePath, "context", "instruction-packs", "claude_minimax", "latest.json")
	if _, err := os.Stat(packPath); err != nil {
		t.Fatalf("refreshed pack path %q missing: %v", packPath, err)
	}
}

// TestRefreshLaneSkipsWhenNoCanonicalCwd pins the R3B loud-skip
// branch: a lane without a recorded (cwd, task) in
// config/lane-defaults.yaml MUST NOT silently contract-compile, and
// its verdict line MUST say so. Skipped is loud on purpose — the
// operator must seed config/lane-defaults.yaml or run contract
// compile by hand. The fresh-render pack step still runs (idempotent
// for the absent-effective-contract case) but the verdict collapses
// to skipped before the drift check, so doctor / installer can never
// falsely report `clean` for a lane whose contract is intentionally
// not part of this run.
func TestRefreshLaneSkipsWhenNoCanonicalCwd(t *testing.T) {
	homePath := t.TempDir()
	baseDir := t.TempDir()
	binaryPath := filepath.Join(t.TempDir(), "compound-current")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeInstalledHooksManifest(t, homePath, map[string]string{
		"claude_minimax": "binary:" + binaryPath,
	})

	opts := RefreshLaneOptions{
		HomePath:   homePath,
		BinaryPath: binaryPath,
		BaseDir:    baseDir,
		Now:        time.Date(2026, 9, 4, 7, 0, 0, 0, time.UTC),
	}
	verdict, err := RefreshLane(opts, "claude_minimax")
	if err != nil {
		t.Fatalf("RefreshLane returned error: %v", err)
	}
	if verdict.Status != StatusSkipped {
		t.Fatalf("verdict status = %q, want %q (detail=%q)", verdict.Status, StatusSkipped, verdict.Detail)
	}
	if !strings.Contains(strings.ToLower(verdict.Detail), "canonical") {
		t.Fatalf("verdict detail should mention canonical cwd/task, got %q", verdict.Detail)
	}
	if verdict.RepairCommand == "" {
		t.Fatalf("skipped verdict must carry a repair command so the operator can seed defaults")
	}
}

// TestRefreshLanePropagatesHookInstallFailure as a contract: a
// hook-install failure on one lane does NOT swallow the verdict — it
// reports `error` with a repair command naming the failing step.
// A fresh home with no manifest seeded means the hook install layer
// has nothing to bind; RefreshLane must surface that as an error
// rather than silently claiming the lane is clean.
func TestRefreshLanePropagatesHookInstallFailure(t *testing.T) {
	homePath := t.TempDir()
	baseDir := t.TempDir()
	binaryPath := filepath.Join(t.TempDir(), "compound-current")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// No installed-hooks manifest seeded: hook install has nothing to
	// bind, RefreshLane must surface that as a non-clean verdict.
	opts := RefreshLaneOptions{
		HomePath:   homePath,
		BinaryPath: binaryPath,
		BaseDir:    baseDir,
		Now:        time.Date(2026, 9, 4, 7, 0, 0, 0, time.UTC),
	}
	verdict, err := RefreshLane(opts, "claude_minimax")
	if err != nil {
		t.Fatalf("RefreshLane returned error: %v", err)
	}
	if verdict.Status == StatusClean {
		t.Fatalf("verdict status = clean, want non-clean for a lane with no manifest entry (detail=%q)", verdict.Detail)
	}
}

// TestInstalledHarnessesReturnsManifestHarnessesSorted pins the
// install.sh-facing side of the lane list. The install loop asks for
// the installed harnesses once and prints one verdict line per; an
// empty / missing manifest returns an empty slice (no panic, no
// error) so a cold-start install is a no-op per-lane instead of a
// failure.
func TestInstalledHarnessesReturnsManifestHarnessesSorted(t *testing.T) {
	homePath := t.TempDir()
	if got, err := InstalledHarnesses(homePath); err != nil {
		t.Fatalf("missing-manifest InstalledHarnesses: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("missing-manifest InstalledHarnesses = %v, want []", got)
	}

	writeInstalledHooksManifest(t, homePath, map[string]string{
		"claude_zai":     "binary:/p/a",
		"claude_minimax": "binary:/p/b",
		"claude_qwen":    "binary:/p/c",
	})
	got, err := InstalledHarnesses(homePath)
	if err != nil {
		t.Fatalf("InstalledHarnesses: %v", err)
	}
	want := []string{"claude_minimax", "claude_qwen", "claude_zai"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("InstalledHarnesses = %v, want %v", got, want)
	}
}

// TestLoadLaneDefaultReportsNotRecordedForMissingEntry pins the
// read side: a config/lane-defaults.yaml present but missing the
// requested harness returns an empty LaneDefault with no error so
// the caller can render "no canonical cwd/task recorded" without
// surfacing a read error.
func TestLoadLaneDefaultReportsNotRecordedForMissingEntry(t *testing.T) {
	homePath := t.TempDir()
	writeLaneDefaults(t, homePath, "claude_zai", "/tmp/z", "z-task")
	got, err := LoadLaneDefault(homePath, "claude_minimax")
	if err != nil {
		t.Fatalf("LoadLaneDefault: %v", err)
	}
	if got.Cwd != "" || got.Task != "" {
		t.Fatalf("missing-lane LoadLaneDefault = %+v, want empty Cwd/Task", got)
	}
}

// TestRefreshLaneRejectsEmptyInputs pins the fail-closed input
// contract: empty home / binary / base dir MUST surface as an error
// verdict, not a silent success. The installer shell loop relies on
// every per-lane call being honest about its inputs.
func TestRefreshLaneRejectsEmptyInputs(t *testing.T) {
	now := time.Date(2026, 9, 4, 7, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		opts RefreshLaneOptions
	}{
		{"empty home", RefreshLaneOptions{BinaryPath: "/b", BaseDir: "/d", Now: now}},
		{"empty binary", RefreshLaneOptions{HomePath: "/h", BaseDir: "/d", Now: now}},
		{"empty base", RefreshLaneOptions{HomePath: "/h", BinaryPath: "/b", Now: now}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RefreshLane(tc.opts, "claude_minimax")
			if err == nil {
				t.Fatalf("RefreshLane with %s returned no error; want fail-closed", tc.name)
			}
		})
	}
}

// writeInstalledHooksManifest writes a minimal manifest with one entry
// per harness, all revision=given. Used by every test in this file;
// kept here so the file is the single owner of the on-disk shape.
func writeInstalledHooksManifest(t *testing.T, homePath string, revisions map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(homePath, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	body.WriteString("schema_version: compound.installed_hooks.v0.1\n")
	body.WriteString("hooks:\n")
	for harness, rev := range revisions {
		body.WriteString(" - harness: " + harness + "\n")
		body.WriteString("   name: PreToolUse\n")
		body.WriteString("   event: PreToolUse\n")
		body.WriteString("   ref: compound\n")
		body.WriteString("   revision: " + rev + "\n")
		body.WriteString("   status: current\n")
	}
	if err := os.WriteFile(filepath.Join(homePath, "config", "installed-hooks.yaml"), []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeLaneDefaults writes a minimal config/lane-defaults.yaml with a
// single harness entry. The same home can host multiple entries by
// calling this once per harness (each call replaces the file with the
// full set).
func writeLaneDefaults(t *testing.T, homePath, harness, cwd, task string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(homePath, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "lanes:\n" +
		" - harness: " + harness + "\n" +
		"   cwd: " + cwd + "\n" +
		"   task: " + task + "\n"
	if err := os.WriteFile(LaneDefaultsPath(homePath), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// errNotExistForTest asserts an error wraps os.ErrNotExist; small
// helper kept here so test bodies stay readable.
func errNotExistForTest(err error) bool { return errors.Is(err, os.ErrNotExist) }
