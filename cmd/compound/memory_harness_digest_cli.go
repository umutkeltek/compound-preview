// memory_harness_digest_cli.go — CLI door for work_67d4af40bba0 (S5): compile
// the standing-law, trap, and harness:<harness> topic subtrees into ONE
// deterministic markdown digest for a harness's ambient memory surface
// (`compound memory render-harness-digest`), plus a read-only drift check
// against hand-edits (`compound memory digest-doctor`). Render logic and the
// visibility gates live in internal/mem (harness_memory_digest.go); this file
// owns argv parsing, the --out placement guard, and the write/print door.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/mem"
)

func cmdMemoryRenderHarnessDigest(store *mem.Store, args []string) {
	harness := strings.TrimSpace(valueFlag(args, "--harness"))
	outPath := strings.TrimSpace(valueFlag(args, "--out"))
	if harness == "" || outPath == "" {
		fmt.Fprintln(os.Stderr, memoryUsage("render-harness-digest"))
		os.Exit(2)
	}
	absOut, err := filepath.Abs(outPath)
	die(err)
	if err := validateDigestOutPath(outPath, absOut); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	regenerateCmd := fmt.Sprintf("compound memory render-harness-digest --harness %s --out %s", harness, absOut)
	report, body, err := store.RenderHarnessDigest(
		mem.HarnessDigestOptions{Harness: harness},
		mem.HarnessDigestMeta{GeneratedBy: versionString(), RegenerateCmd: regenerateCmd},
		time.Now(),
	)
	die(err)
	if err := mem.WriteHarnessDigestAtomic(absOut, body); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	report.OutPath = absOut

	if hasFlag(args, "--json") {
		printJSON(report)
		return
	}
	fmt.Printf("# memory render-harness-digest %s\n", report.Harness)
	fmt.Printf("- out: %s\n", report.OutPath)
	fmt.Printf("- source_ledger_seq: %d\n", report.SourceLedgerSeq)
	fmt.Printf("- content_sha256: %s\n", report.ContentSHA256)
	fmt.Printf("- positions: %d (excluded %d)\n", report.PositionCount, report.ExcludedCount)
	for _, g := range report.Groups {
		fmt.Printf("  - %s: %d\n", g.Topic, g.PositionCount)
	}
}

// cmdMemoryDigestDoctor reads an on-disk digest and reports drift, read-only.
// It intentionally exits 1 when drift is found (in BOTH --json and human
// modes) so a CI/hook gate can key off the exit code the same way
// `compound memory engine-compare` does for its own acceptability verdict —
// callers driving this in-process (rather than as a real subprocess) must
// account for that exit, exactly like any other exit-on-failure CLI verb.
func cmdMemoryDigestDoctor(args []string) {
	path := strings.TrimSpace(valueFlag(args, "--path"))
	if path == "" {
		path = firstMemoryPos(args, map[string]bool{"--path": true})
	}
	if strings.TrimSpace(path) == "" {
		fmt.Fprintln(os.Stderr, memoryUsage("digest-doctor"))
		os.Exit(2)
	}
	absPath, err := filepath.Abs(path)
	die(err)
	report, err := mem.VerifyHarnessDigestFile(absPath)
	die(err)

	if hasFlag(args, "--json") {
		printJSON(report)
		if report.Drifted {
			os.Exit(1)
		}
		return
	}
	fmt.Println("# memory digest-doctor")
	fmt.Printf("- path: %s\n", report.Path)
	fmt.Printf("- declared_content_sha256: %s\n", report.DeclaredContentSHA256)
	fmt.Printf("- recomputed_content_sha256: %s\n", report.RecomputedContentSHA256)
	fmt.Printf("- drifted: %t\n", report.Drifted)
	if report.Detail != "" {
		fmt.Printf("- detail: %s\n", report.Detail)
	}
	if report.Drifted {
		os.Exit(1)
	}
}

// validateDigestOutPath enforces spec point 1: --out is an "operator-local
// target, NEVER inside the repo". Absolute is required outright; "inside the
// repo" is decided by walking up from cwd looking for a .git entry (file or
// dir — a git worktree's .git is a file pointing at the real gitdir, so this
// still finds the worktree root) and refusing any --out that resolves at or
// under that root. If cwd isn't inside a git checkout at all, there is no
// repo tree to protect and the absolute-path requirement is the whole guard.
func validateDigestOutPath(rawOut, absOut string) error {
	if !filepath.IsAbs(rawOut) {
		return fmt.Errorf("--out must be an absolute path (operator-local target, never inside the repo); got %q", rawOut)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve cwd: %w", err)
	}
	repoRoot, found := findRepoRootFrom(cwd)
	if !found {
		return nil
	}
	// os.Getwd() returns the symlink-resolved cwd (e.g. macOS /var -> /private/var),
	// but absOut is whatever string the operator/caller passed — filepath.Abs never
	// resolves symlinks. Comparing the two as raw strings would let a repo-relative
	// --out slip through (or a real outside path get falsely blocked) purely because
	// of a "/var" vs "/private/var" prefix mismatch. Resolve both through the same
	// best-effort realpath before deciding "inside" vs "outside".
	realRepoRoot := realpathBestEffort(repoRoot)
	realOut := realpathBestEffort(absOut)
	rel, err := filepath.Rel(realRepoRoot, realOut)
	if err != nil {
		return nil
	}
	if rel == "." || !strings.HasPrefix(rel, "..") {
		return fmt.Errorf("--out %q resolves inside the repo (%s) — target an operator-local path outside the working tree", absOut, repoRoot)
	}
	return nil
}

// findRepoRootFrom walks up from dir looking for a .git entry.
func findRepoRootFrom(dir string) (string, bool) {
	dir = filepath.Clean(dir)
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// realpathBestEffort resolves symlinks in the longest existing prefix of path
// and rejoins whatever suffix doesn't exist yet (an --out target's file, and
// possibly some of its parent directories, legitimately doesn't exist before
// the write). Falls back to path itself if nothing resolves.
func realpathBestEffort(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(realpathBestEffort(parent), filepath.Base(path))
}
