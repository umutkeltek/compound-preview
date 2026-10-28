package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const proofCheckGoCachePruneSubEnv = "COMPOUND_PROOF_GOCACHE_PRUNE_SUB"

// The entry point prunes the per-worktree proof Go caches exactly once, before
// argument errors can stop the run, and never in dry-run or shared-cache mode
// or for a repo under the temp dir. HOME is isolated, so the real user cache
// root is never read or pruned; the child gets its own TMPDIR so the fixture
// repo counts as a real worktree.
func TestProofCheckEntryPrunesProofGoCachesOncePerRun(t *testing.T) {
	if os.Getenv(proofCheckGoCachePruneSubEnv) == "1" {
		args := append(strings.Fields(os.Getenv("COMPOUND_PROOF_GOCACHE_PRUNE_ARGS")), "--repo", os.Getenv("COMPOUND_PROOF_GOCACHE_PRUNE_REPO"))
		cmdProofCheck(args)
		os.Exit(0)
	}

	userHome := isolatedTestUserHome(t)
	t.Setenv("HOME", userHome)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(userHome, "xdg-cache"))
	cacheBase, err := os.UserCacheDir()
	if err != nil || !strings.HasPrefix(cacheBase, userHome+string(filepath.Separator)) {
		t.Fatalf("user cache dir %q (err %v) is not isolated under %q", cacheBase, err, userHome)
	}
	root := filepath.Join(cacheBase, "compound-proof-gocache")
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	childTemp := filepath.Join(base, "tmp")
	for _, dir := range []string{repo, childTemp} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	seedOrphan := func(name string) string {
		t.Helper()
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "owner-path"), []byte(filepath.Join(userHome, "retired-worktree")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		lastUsed := filepath.Join(dir, "last-used")
		if err := os.WriteFile(lastUsed, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-24 * time.Hour)
		if err := os.Chtimes(lastUsed, old, old); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	run := func(args, shared, tmpdir string) string {
		t.Helper()
		child := exec.Command(os.Args[0], "-test.run=^TestProofCheckEntryPrunesProofGoCachesOncePerRun$")
		child.Env = append(os.Environ(),
			proofCheckGoCachePruneSubEnv+"=1",
			"COMPOUND_PROOF_GOCACHE_PRUNE_ARGS="+args,
			"COMPOUND_PROOF_GOCACHE_PRUNE_REPO="+repo,
			"COMPOUND_PROOF_SHARED_GOCACHE="+shared,
			"TMPDIR="+tmpdir,
		)
		var stderr bytes.Buffer
		child.Stderr = &stderr
		err := child.Run()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
			t.Fatalf("proof-check %s exit = %v, want 2; stderr=%s", args, err, stderr.String())
		}
		var lines []string
		for _, line := range strings.Split(stderr.String(), "\n") {
			if strings.HasPrefix(line, "proof-gocache:") {
				lines = append(lines, line)
			}
		}
		return strings.Join(lines, "\n")
	}

	// --ci with --ci-console is refused right after the prune, before any gate.
	orphan := seedOrphan("feedfacecafe00a1")
	if got, want := run("--ci --ci-console", "", childTemp), "proof-gocache: kept 0, removed 1 (orphan 1, idle 0, lru 0)"; got != want {
		t.Fatalf("prune output = %q, want %q", got, want)
	}
	if _, err := os.Lstat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan cache survived the run: %v", err)
	}

	orphan = seedOrphan("feedfacecafe00a2")
	for _, mode := range []struct{ args, shared, tmpdir string }{
		{"--dry-run --ci --ci-console", "", childTemp},
		{"--ci --ci-console", "1", childTemp},
		{"--ci --ci-console", "", os.TempDir()}, // repo is a temp fixture
	} {
		if got := run(mode.args, mode.shared, mode.tmpdir); got != "" {
			t.Fatalf("mode %+v pruned: %q", mode, got)
		}
		if _, err := os.Lstat(orphan); err != nil {
			t.Fatalf("mode %+v removed the orphan: %v", mode, err)
		}
	}
}
