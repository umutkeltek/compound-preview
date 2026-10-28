package release

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGitHubWorkflowReferencesAreExplicitlyInactive(t *testing.T) {
	for _, name := range []string{"proof", "release"} {
		body, err := os.ReadFile("../../.github/workflows/" + name + ".yml")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(body), "# Inactive in this repository since 2026-10-16; retained as a test reference.\n# Active owned-CI and maintainer publication procedure: docs/release.md.\n") {
			t.Errorf("%s workflow lacks inactive-reference notice", name)
		}
		var document map[string]any
		if err := yaml.Unmarshal(body, &document); err != nil {
			t.Fatalf("parse retained %s workflow: %v", name, err)
		}
	}
}
