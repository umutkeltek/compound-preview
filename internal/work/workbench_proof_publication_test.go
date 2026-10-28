package work

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkbenchRejectsPartialInstalledRuntimePublication(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	refsDir := filepath.Join(s.home, "artifacts", "refs")
	if err := os.MkdirAll(refsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	partial := "# interrupted publication\n\n" +
		"`proof-check-installed-runtime` adjacent Workbench action verified installed runtime proof.\n" +
		"Do not rerun adjacent Workbench actions.\n"
	if err := os.WriteFile(
		filepath.Join(refsDir, "proof-check-installed-runtime-20260724.md"),
		[]byte(partial),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	refs, warnings := s.adjacentActionProofRefs("proof-check-installed-runtime")
	if len(warnings) != 0 {
		t.Fatalf("partial artifact warnings = %v", warnings)
	}
	if len(refs) != 0 {
		t.Fatalf("partial artifact suppressed action through refs %v", refs)
	}
}
