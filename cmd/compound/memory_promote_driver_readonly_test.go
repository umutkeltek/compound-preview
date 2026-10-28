package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestMemoryPromoteDriverDryRunDoesNotMutateCompoundHome pins the read-only
// invariant for `compound memory promote-driver` (dry-run by default). The
// registry classifies the bare verb as a READ command, but the parent
// cmdMemory dispatcher used to call mem.Open(homePath) for every subcommand
// outside {consumption, search, digest-doctor} — materializing ledger/,
// inbox/, and events/ under a fresh home before the driver's own read-only
// plan pass ever ran. The read path must actually be read-only, not merely
// non-mutating by convention. Mirrors
// TestMemorySearchDoesNotMutateCompoundHome.
//
// Falsifier: reverting the exclusion in cmd/compound/memory_cli.go (dropping
// `&& sub != "promote-driver"`) turns this RED with "created ledger/".
func TestMemoryPromoteDriverDryRunDoesNotMutateCompoundHome(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("reexec: skipped in -short mode")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	homePath := filepath.Join(t.TempDir(), "absent-home")
	cmd := exec.Command(compoundTestBinary(t), "memory", "promote-driver", "--dry-run", "--json")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
	output, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("dry-run over absent home failed: %v\n%s", runErr, output)
	}

	for _, name := range []string{"ledger", "inbox", "events", filepath.Join("state", "promote-driver")} {
		if _, err := os.Stat(filepath.Join(homePath, name)); err == nil {
			t.Fatalf("memory promote-driver --dry-run created %s under COMPOUND_HOME: read-only invariant violated", name)
		}
	}
}
