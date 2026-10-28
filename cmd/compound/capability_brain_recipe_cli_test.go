package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCapabilityImportBrainRecipeDryRunDoesNotCreateHome(t *testing.T) {
	t.Parallel()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	homePath := filepath.Join(t.TempDir(), "absent-home")
	recipeDir := filepath.Join(repoRoot, "fixtures", "skills", "brain-recipe-tree")
	cmd := exec.Command(compoundTestBinary(t), "capability", "import-skills", "--brain-recipe-dir", recipeDir, "--dry-run")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("recipe dry-run failed: %v\n%s", err, out)
	}
	for _, want := range []string{"# capability import-skills brain-recipe-tree dry-run", "- drift: true", "- nodes: 3"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("recipe dry-run output missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(homePath); !os.IsNotExist(err) {
		t.Fatalf("dry-run created COMPOUND_HOME %s, err=%v", homePath, err)
	}
}
