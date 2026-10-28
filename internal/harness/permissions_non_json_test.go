package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyPermissionsRejectsExistingNonJSONTargetWithoutMutation(t *testing.T) {
	homePath := t.TempDir()
	targetPath := filepath.Join(t.TempDir(), "config.toml")
	writePermissionFixture(t, homePath, targetPath, permissionFixture(nil))
	approved, action, decision := governedPermissionApproval(t, homePath, "claude_code")

	original := []byte("approval_policy = \"never\"\nsandbox_mode = \"workspace-write\"\n")
	if err := os.WriteFile(targetPath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := applyPermissionsForTest(homePath, approved, action, decision, allowPermissionApply, finalizePermissionApply)
	if err == nil {
		t.Fatal("ApplyPermissions error = nil, want non-JSON target refusal")
	}
	after, readErr := os.ReadFile(targetPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("non-JSON target changed:\nbefore=%q\nafter=%q", original, after)
	}
	t.Logf("non-JSON target remained byte-identical: sha256:%s", digest(after))
	if !strings.Contains(err.Error(), targetPath) || !strings.Contains(strings.ToLower(err.Error()), "not valid json") {
		t.Fatalf("ApplyPermissions error = %v, want target path and explicit non-JSON reason", err)
	}
}

func TestApplyPermissionsBootstrapsAbsentTarget(t *testing.T) {
	homePath := t.TempDir()
	targetPath := filepath.Join(t.TempDir(), "settings.json")
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))

	render, err := applyGovernedPermissions(t, homePath, "claude_code")
	if err != nil {
		t.Fatalf("ApplyPermissions absent target: %v", err)
	}
	if render.TargetPath != targetPath {
		t.Fatalf("render target = %q, want %q", render.TargetPath, targetPath)
	}
	body, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(body) {
		t.Fatalf("bootstrapped target is not JSON: %q", body)
	}
}

func TestLoadPermissionAuthorityAcceptsCodexRealHomeBody(t *testing.T) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(userHome, ".codex", "config.toml")
	homePath := t.TempDir()
	authorityPath := PermissionAuthorityPath(homePath, "codex")
	if err := os.MkdirAll(filepath.Dir(authorityPath), 0o755); err != nil {
		t.Fatal(err)
	}
	body := codexPermissionAuthorityBody(targetPath)
	if err := os.WriteFile(authorityPath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	policy, revision, err := LoadPermissionAuthority(homePath, "codex")
	if err != nil {
		t.Fatalf("LoadPermissionAuthority codex body: %v\n%s", err, body)
	}
	if err := policy.Validate(); err != nil {
		t.Fatalf("Validate loaded codex body: %v\n%s", err, body)
	}
	if policy.TargetPath != targetPath || revision != "sha256:"+digest(body) {
		t.Fatalf("loaded codex authority = %+v revision=%q, want target=%q exact body digest", policy, revision, targetPath)
	}
}

func codexPermissionAuthorityBody(targetPath string) []byte {
	return []byte(fmt.Sprintf(`schema_version: compound.harness_permission_authority.v0.1
harness: codex
authority_ref: compound://config/harness-permissions/codex
target_path: %q
permissions:
  allow: []
  deny: []
  ask: []
  default_mode: ""
auto_mode:
  allow:
    - $defaults
`, targetPath))
}
