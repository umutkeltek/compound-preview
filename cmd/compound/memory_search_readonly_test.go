package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestMemorySearchDoesNotMutateCompoundHome pins the read-only invariant for
// `compound memory search`. The subcommand is a corpus query and must NOT open
// the mutable memory store: mem.Open() creates ledger/, inbox/, and events/
// under COMPOUND_HOME, so a failing search would scatter partial state into a
// fresh home before the operator ever admits anything. Catches a regression
// where someone re-adds `search` to the non-consumption mem.Open guard.
//
// Falsifier: reverting the guard in cmd/compound/memory_cli.go so that
// `sub != "consumption" && sub != "search"` becomes `sub != "consumption"`
// turns this test RED with "created ledger/" -- verified 2026-07-20.
func TestMemorySearchDoesNotMutateCompoundHome(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	homePath := filepath.Join(t.TempDir(), "absent-home")
	cmd := exec.Command(compoundTestBinary(t), "memory", "search", "--lane", "nonexistent", "query")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
	// The lane does not exist; the command is expected to exit non-zero. We
	// only care about the side effects it leaves behind on disk.
	_, _ = cmd.CombinedOutput()

	for _, name := range []string{"ledger", "inbox", "events"} {
		if _, err := os.Stat(filepath.Join(homePath, name)); err == nil {
			t.Fatalf("memory search created %s/ under COMPOUND_HOME: read-only invariant violated", name)
		}
	}
}
