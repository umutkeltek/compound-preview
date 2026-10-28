package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/harness"
)

func TestHarnessPermissionModeReadsCompoundAuthority(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	targetPath := filepath.Join(t.TempDir(), "settings.json")
	policy := harness.PermissionAuthority{
		SchemaVersion: harness.PermissionAuthoritySchemaVersion,
		Harness:       "claude_code",
		AuthorityRef:  harness.PermissionAuthorityRef("claude_code"),
		TargetPath:    targetPath,
		Permissions: harness.PermissionRules{
			Allow:       []string{"Bash"},
			Deny:        []string{"Bash(rm -rf /)"},
			DefaultMode: "bypassPermissions",
		},
		AutoMode: harness.AutoModePolicy{Allow: []string{"$defaults"}},
	}
	body, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	path := harness.PermissionAuthorityPath(homePath, "claude_code")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdHarness([]string{"permission-mode", "--harness", "claude_code"})
	})
	if got := strings.TrimSpace(out); got != "bypassPermissions" {
		t.Fatalf("permission-mode output = %q, want bypassPermissions", got)
	}

	jsonOut := captureStdout(t, func() {
		cmdHarness([]string{"permission-mode", "--harness", "claude_code", "--json"})
	})
	var projection map[string]string
	if err := json.Unmarshal([]byte(jsonOut), &projection); err != nil {
		t.Fatalf("parse permission-mode JSON: %v\n%s", err, jsonOut)
	}
	if projection["harness"] != "claude_code" ||
		projection["permission_mode"] != "bypassPermissions" ||
		projection["authority_ref"] != harness.PermissionAuthorityRef("claude_code") ||
		!strings.HasPrefix(projection["authority_revision"], "sha256:") {
		t.Fatalf("permission-mode projection lost authority provenance: %+v", projection)
	}
}
