package harness

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"compound/internal/envelope"
	homepkg "compound/internal/home"
	"compound/internal/work"
)

type ambiguousPermissionRootedCAS struct{}

func (ambiguousPermissionRootedCAS) State() homepkg.RootedFileCASState {
	return homepkg.RootedFileCASAmbiguous
}

func (ambiguousPermissionRootedCAS) Commit() error   { return errors.New("ambiguous publication") }
func (ambiguousPermissionRootedCAS) Rollback() error { return errors.New("ambiguous publication") }

func TestPermissionAuthorityValidation(t *testing.T) {
	tests := []struct {
		name, wantErr string
		policy        PermissionAuthority
	}{
		{"rejects global MCP wildcard", "mcp__*", permissionFixture([]string{"mcp__*"})},
		{"accepts literal server wildcard", "", permissionFixture([]string{"mcp__gbrain__*"})},
		{"rejects legacy path-scoped Write rule", "Edit(~/.claude/**)", permissionFixture([]string{"Write(~/.claude/**)"})},
		{"rejects legacy path-scoped MultiEdit rule", "Edit(~/Projects/**)", permissionFixture([]string{"MultiEdit(~/Projects/**)"})},
		{"rejects duplicates", "duplicate", permissionFixture([]string{"Read", "Read"})},
	}
	badDefaults := permissionFixture([]string{"Read"})
	badDefaults.AutoMode.Allow = []string{"Read", "$defaults"}
	badOwnership := permissionFixture([]string{"Read"})
	badOwnership.Permissions.Ownership = "sometimes"
	tests = append(tests, struct {
		name, wantErr string
		policy        PermissionAuthority
	}{"requires defaults first", "$defaults", badDefaults}, struct {
		name, wantErr string
		policy        PermissionAuthority
	}{"rejects ambiguous permission ownership", "ownership", badOwnership})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.policy.Validate()
			if tt.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestRenderPermissionsExactOwnershipPrunesUnmanagedRulesAndPreservesUnrelatedSettings(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{
		"theme": "dark",
		"permissions": map[string]any{
			"allow":       []string{"Read", "Edit(~/Projects/**)", "Bash"},
			"deny":        []string{"HandDeny"},
			"ask":         []string{"HandAsk"},
			"defaultMode": "acceptEdits",
		},
		"hooks": map[string]any{"PreToolUse": []any{map[string]any{"matcher": "Edit"}}},
	})
	policy := permissionFixture([]string{"Agent(*)", "Bash"})
	policy.Permissions.Ownership = "exact"
	writePermissionFixture(t, homePath, targetPath, policy)

	render, err := RenderPermissions(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	got := decodeTestJSON(t, render.Content)
	if got["theme"] != "dark" {
		t.Fatalf("unrelated top-level setting changed: %+v", got)
	}
	if _, ok := got["hooks"].(map[string]any)["PreToolUse"]; !ok {
		t.Fatalf("hooks were not preserved: %+v", got["hooks"])
	}
	permissions := got["permissions"].(map[string]any)
	assertStrings(t, permissions["allow"], []string{"Agent(*)", "Bash"})
	assertStrings(t, permissions["deny"], []string{"Bash(rm:*)"})
	assertStrings(t, permissions["ask"], []string{"WebFetch"})
	if permissions["defaultMode"] != "default" {
		t.Fatalf("defaultMode = %v, want default", permissions["defaultMode"])
	}
	if !render.Changed {
		t.Fatal("exact ownership render changed = false, want stale rules pruned")
	}
}

func TestPermissionApplyPathsUseCatalogOwnedHabitats(t *testing.T) {
	homePath := filepath.Join(string(os.PathSeparator), "owned", ".compound")
	if got, want := PermissionApplyPendingPath(homePath, "claude_code"), filepath.Join(homePath, "work", "transactions", "harness-permissions", "claude_code.json"); got != want {
		t.Fatalf("permission pending path = %q, want %q", got, want)
	}
	if got, want := permissionApplyLockPath(homePath), filepath.Join(homePath, "lock", "harness-permissions.lock"); got != want {
		t.Fatalf("permission lock path = %q, want %q", got, want)
	}
}

func TestLoadPermissionAuthorityRejectsWrongHarnessFile(t *testing.T) {
	homePath := t.TempDir()
	policy := permissionFixture([]string{"Read"})
	policy.Harness = "codex"
	policy.AuthorityRef = PermissionAuthorityRef("codex")
	writeTestJSON(t, PermissionAuthorityPath(homePath, "claude_code"), policy)
	_, _, err := LoadPermissionAuthority(homePath, "claude_code")
	if err == nil || !strings.Contains(err.Error(), "claude_code") {
		t.Fatalf("LoadPermissionAuthority error = %v, want harness mismatch", err)
	}
}

func TestRenderPermissionsReconcilesManagedSliceAndIsIdempotent(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{
		"theme":       "dark",
		"permissions": map[string]any{"allow": []string{"mcp__*", "HandOwned", "mcp__gbrain__*"}, "deny": []string{"HandDeny"}, "ask": []string{"HandAsk"}, "defaultMode": "legacy"},
		"autoMode":    map[string]any{"allow": []string{"$defaults", "LegacyManaged", "HandAuto"}, "other": true},
	})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read", "mcp__gbrain__*"}))
	err := WriteHarnessArtifactManifest(homePath, []HarnessArtifact{{
		Path: targetPath, Harness: "claude_code", SourceAuthorityRef: PermissionAuthorityRef("claude_code"),
		ManagedEntries: map[string][]string{"permissions.allow": {"mcp__*", "mcp__gbrain__*"}, "autoMode.allow": {"LegacyManaged"}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	render, err := RenderPermissions(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	got := decodeTestJSON(t, render.Content)
	if got["theme"] != "dark" || got["autoMode"].(map[string]any)["other"] != true {
		t.Fatalf("unrelated keys changed: %#v", got)
	}
	permissions := got["permissions"].(map[string]any)
	assertStrings(t, permissions["allow"], []string{"HandOwned", "Read", "mcp__gbrain__*"})
	assertStrings(t, permissions["deny"], []string{"HandDeny", "Bash(rm:*)"})
	assertStrings(t, permissions["ask"], []string{"HandAsk", "WebFetch"})
	if permissions["defaultMode"] != "default" || strings.Contains(string(render.Content), "\"mcp__*\"") {
		t.Fatalf("managed reconciliation failed:\n%s", render.Content)
	}
	assertStrings(t, got["autoMode"].(map[string]any)["allow"], []string{"$defaults", "HandAuto", "Read"})

	if err := os.WriteFile(targetPath, render.Content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteHarnessArtifactManifest(homePath, []HarnessArtifact{render.Artifact}); err != nil {
		t.Fatal(err)
	}
	second, err := RenderPermissions(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(second.Content, render.Content) {
		t.Fatalf("second render differs:\n%s\n---\n%s", render.Content, second.Content)
	}
}

func TestRenderPermissionsCanonicalizesUnmanagedLegacyFileEditRules(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{
		"permissions": map[string]any{"allow": []string{
			"Read",
			"Write(~/.claude/**)",
			"MultiEdit(~/.claude/**)",
			"Edit(~/Projects/**)",
			"Write(~/Projects/**)",
			"MultiEdit(~/gbrain/**)",
			"Write",
			"Bash(echo Write(example))",
		}},
	})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Bash"}))

	render, err := RenderPermissions(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	permissions := decodeTestJSON(t, render.Content)["permissions"].(map[string]any)
	assertStrings(t, permissions["allow"], []string{
		"Read",
		"Edit(~/.claude/**)",
		"Edit(~/Projects/**)",
		"Edit(~/gbrain/**)",
		"Write",
		"Bash(echo Write(example))",
		"Bash",
	})
	if strings.Contains(string(render.Content), "Write(~/.claude/**)") ||
		strings.Contains(string(render.Content), "MultiEdit(") {
		t.Fatalf("legacy file-edit rules survived:\n%s", render.Content)
	}
}

func TestPermissionDriftRedGreenAndIgnoresUnrelatedKeys(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read", "mcp__gbrain__*"}))
	render, err := RenderPermissions(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, render.Content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteHarnessArtifactManifest(homePath, []HarnessArtifact{render.Artifact}); err != nil {
		t.Fatal(err)
	}
	assertPermissionDrift(t, homePath, false, "")

	settings := decodeTestJSON(t, render.Content)
	settings["theme"] = "light"
	writeTestJSON(t, targetPath, settings)
	assertPermissionDrift(t, homePath, false, "")

	permissions := settings["permissions"].(map[string]any)
	permissions["allow"] = []string{"Read", "mcp__gbrain__*", "mcp__*"}
	writeTestJSON(t, targetPath, settings)
	assertPermissionDrift(t, homePath, true, "mcp__*")

	permissions["allow"] = []string{"mcp__gbrain__*"}
	writeTestJSON(t, targetPath, settings)
	assertPermissionDrift(t, homePath, true, "Read")

	settings = decodeTestJSON(t, render.Content)
	settings["autoMode"].(map[string]any)["allow"] = []string{"Read", "$defaults"}
	writeTestJSON(t, targetPath, settings)
	assertPermissionDrift(t, homePath, true, "$defaults")
}

func TestPermissionDriftReportsRevisionOfExactValidatedTargetBytes(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	render, err := RenderPermissions(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, render.Content, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := PermissionDrift(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if report.Drifted {
		t.Fatalf("clean target reported drift: %+v", report)
	}
	want := "sha256:" + digest(render.Content)
	if report.TargetRevision != want {
		t.Fatalf("validated target revision = %q, want %q", report.TargetRevision, want)
	}
}

func TestApplyPermissionsWritesManagedRulesAndArtifactIdempotently(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{
		"theme":    "dark",
		"autoMode": map[string]any{"allow": []string{"$defaults", "HandOwned"}, "other": true},
		"hooks":    map[string]any{"PreToolUse": []any{map[string]any{"matcher": "Write"}}},
	})
	policy := permissionFixture([]string{"Read", "mcp__gbrain__*"})
	policy.AutoMode.Allow = []string{"$defaults", "Routine local shell is auto-approved."}
	writePermissionFixture(t, homePath, targetPath, policy)

	first, err := applyGovernedPermissions(t, homePath, "claude_code")
	if err != nil {
		t.Fatalf("apply permissions: %v", err)
	}
	if !first.Changed {
		t.Fatal("first apply changed = false, want true")
	}
	body, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeTestJSON(t, body)
	if got["theme"] != "dark" || got["autoMode"].(map[string]any)["other"] != true {
		t.Fatalf("hand-owned settings not preserved: %+v", got)
	}
	if _, ok := got["hooks"].(map[string]any)["PreToolUse"]; !ok {
		t.Fatalf("hooks not preserved: %+v", got["hooks"])
	}
	assertStrings(t, got["autoMode"].(map[string]any)["allow"], []string{"$defaults", "HandOwned", "Routine local shell is auto-approved."})
	artifact, ok, err := latestHarnessArtifact(homePath, "claude_code", targetPath)
	if err != nil || !ok {
		t.Fatalf("latest artifact ok=%t err=%v", ok, err)
	}
	if artifact.ContentSHA256 != first.Artifact.ContentSHA256 {
		t.Fatalf("artifact sha = %q, want %q", artifact.ContentSHA256, first.Artifact.ContentSHA256)
	}
	manifestBefore, err := os.ReadFile(HarnessArtifactManifestPath(homePath))
	if err != nil {
		t.Fatal(err)
	}

	second, err := applyGovernedPermissions(t, homePath, "claude_code")
	if err != nil {
		t.Fatalf("second apply permissions: %v", err)
	}
	if second.Changed {
		t.Fatal("second apply changed = true, want idempotent false")
	}
	manifestAfter, err := os.ReadFile(HarnessArtifactManifestPath(homePath))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(manifestAfter, manifestBefore) {
		t.Fatalf("second apply rewrote identical manifest:\n%s\n---\n%s", manifestBefore, manifestAfter)
	}
}

func TestApplyPermissionsCreatesNoUnknownTopLevelHabitats(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	if _, err := applyGovernedPermissions(t, homePath, "claude_code"); err != nil {
		t.Fatal(err)
	}
	inventory, err := homepkg.InspectHabitat(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if !inventory.OK || len(inventory.UnknownRoots) != 0 || len(inventory.UnknownEntries) != 0 {
		t.Fatalf("permission apply created unknown habitat: %+v", inventory)
	}
	if _, err := os.Stat(filepath.Join(homePath, "locks")); !os.IsNotExist(err) {
		t.Fatalf("permission apply perpetuated legacy lock root: %v", err)
	}
}

func TestApplyPermissionsFirstAdoptionOfPreexistingManagedRulesIsImmediatelyStable(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{
		"permissions": map[string]any{
			"allow":       []string{"Bash", "HandAllow", "Agent(*)"},
			"deny":        []string{"Bash(rm -rf /)", "HandDeny", "Bash(dd *)"},
			"ask":         []string{"WebFetch"},
			"defaultMode": "default",
		},
		"autoMode": map[string]any{"allow": []string{"$defaults", "Read"}},
	})
	policy := permissionFixture([]string{"Agent(*)", "Bash"})
	policy.Permissions.Deny = []string{"Bash(dd *)", "Bash(rm -rf /)"}
	writePermissionFixture(t, homePath, targetPath, policy)

	applied, err := applyGovernedPermissions(t, homePath, "claude_code")
	if err != nil {
		t.Fatalf("apply permissions: %v", err)
	}
	immediate, err := RenderPermissions(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if immediate.Changed {
		t.Fatal("render immediately after first adoption changed = true, want stable false")
	}
	if immediate.Artifact.ContentSHA256 != applied.Artifact.ContentSHA256 {
		t.Fatalf("immediate artifact sha = %q, want applied %q", immediate.Artifact.ContentSHA256, applied.Artifact.ContentSHA256)
	}
	report, err := PermissionDrift(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if report.Drifted || len(report.Details) > 0 {
		t.Fatalf("drift immediately after first adoption = %+v, want clean", report)
	}
}

func TestApplyPermissionsRollsBackSettingsWhenArtifactManifestFails(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
	before, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	previousWriter := writeHarnessArtifactManifestForApply
	writeHarnessArtifactManifestForApply = func(string, []HarnessArtifact) error {
		return os.ErrPermission
	}
	t.Cleanup(func() { writeHarnessArtifactManifestForApply = previousWriter })

	if _, err := applyGovernedPermissions(t, homePath, "claude_code"); err == nil || !strings.Contains(err.Error(), "restored") {
		t.Fatalf("apply error = %v, want explicit restore evidence", err)
	}
	after, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("settings changed after manifest failure:\n%s\n---\n%s", before, after)
	}
}

func TestApplyPermissionsInterruptedWriteIsVisibleAndRecovered(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	approved, action, decision := governedPermissionApproval(t, homePath, "claude_code")
	previousHook := applyPermissionsAfterTargetWrite
	applyPermissionsAfterTargetWrite = func() error { return os.ErrClosed }
	if _, err := applyPermissionsForTest(homePath, approved, action, decision, allowPermissionApply, finalizePermissionApply); err == nil {
		t.Fatal("interrupted apply error = nil")
	}
	pendingInfo, err := os.Stat(PermissionApplyPendingPath(homePath, "claude_code"))
	if err != nil {
		t.Fatal(err)
	}
	if got := pendingInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("pending transaction mode = %o, want 600", got)
	}
	report, err := PermissionDrift(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if !report.Drifted || !strings.Contains(strings.Join(report.Details, " "), "incomplete permission apply transaction") {
		t.Fatalf("interrupted drift = %+v", report)
	}

	applyPermissionsAfterTargetWrite = previousHook
	t.Cleanup(func() { applyPermissionsAfterTargetWrite = previousHook })
	if _, err := applyPermissionsForTest(homePath, approved, action, decision, allowPermissionApply, finalizePermissionApply); err != nil {
		t.Fatalf("recover apply: %v", err)
	}
	report, err = PermissionDrift(homePath, "claude_code")
	if err != nil || report.Drifted {
		t.Fatalf("drift after recovery = %+v err=%v", report, err)
	}
	if _, err := os.Stat(PermissionApplyPendingPath(homePath, "claude_code")); !os.IsNotExist(err) {
		t.Fatalf("pending transaction remains after recovery: %v", err)
	}
}

func TestApplyPermissionsRecoversLegacyPendingTransaction(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	approved, action, decision := governedPermissionApproval(t, homePath, "claude_code")
	previousHook := applyPermissionsAfterTargetWrite
	applyPermissionsAfterTargetWrite = func() error { return os.ErrClosed }
	t.Cleanup(func() { applyPermissionsAfterTargetWrite = previousHook })
	if _, err := applyPermissionsForTest(homePath, approved, action, decision, allowPermissionApply, finalizePermissionApply); err == nil {
		t.Fatal("interrupted apply error = nil")
	}
	applyPermissionsAfterTargetWrite = previousHook

	canonicalPath := filepath.Join(homePath, "work", "transactions", "harness-permissions", "claude_code.json")
	legacyPath := filepath.Join(homePath, "transactions", "harness-permissions", "claude_code.json")
	if canonicalPath != PermissionApplyPendingPath(homePath, "claude_code") {
		t.Fatalf("test canonical path disagrees with constructor: %q", PermissionApplyPendingPath(homePath, "claude_code"))
	}
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(canonicalPath, legacyPath); err != nil {
		t.Fatal(err)
	}
	report, err := PermissionDrift(homePath, "claude_code")
	if err != nil || !report.Drifted || !strings.Contains(strings.Join(report.Details, " "), "incomplete permission apply transaction") {
		t.Fatalf("legacy pending drift = %+v err=%v", report, err)
	}
	if _, err := applyPermissionsForTest(homePath, approved, action, decision, allowPermissionApply, finalizePermissionApply); err != nil {
		t.Fatalf("recover legacy apply: %v", err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy pending remains after recovery: %v", err)
	}
	if _, err := os.Stat(canonicalPath); !os.IsNotExist(err) {
		t.Fatalf("canonical pending remains after recovery: %v", err)
	}
}

func TestApplyPermissionsRefusesCoexistingCanonicalAndLegacyPendingTransactions(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	approved, action, decision := governedPermissionApproval(t, homePath, "claude_code")
	previousHook := applyPermissionsAfterTargetWrite
	applyPermissionsAfterTargetWrite = func() error { return os.ErrClosed }
	t.Cleanup(func() { applyPermissionsAfterTargetWrite = previousHook })
	if _, err := applyPermissionsForTest(homePath, approved, action, decision, allowPermissionApply, finalizePermissionApply); err == nil {
		t.Fatal("interrupted apply error = nil")
	}
	applyPermissionsAfterTargetWrite = previousHook

	canonicalPath := PermissionApplyPendingPath(homePath, "claude_code")
	legacyPath := filepath.Join(homePath, "transactions", "harness-permissions", "claude_code.json")
	body, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePermissionPrivateFileAtomic(legacyPath, body); err != nil {
		t.Fatal(err)
	}
	if _, err := applyPermissionsForTest(homePath, approved, action, decision, allowPermissionApply, finalizePermissionApply); err == nil || !strings.Contains(err.Error(), "canonical and legacy") {
		t.Fatalf("coexisting pending apply error = %v, want fail-closed ambiguity", err)
	}
	for _, path := range []string{canonicalPath, legacyPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("coexisting pending state was not preserved at %s: %v", path, err)
		}
	}
}

func TestPermissionApplyLockWaitsForPreexistingLegacyLock(t *testing.T) {
	homePath := t.TempDir()
	legacyPath := filepath.Join(homePath, "locks", "harness-permissions.lock")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	legacyFile, err := os.OpenFile(legacyPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(legacyFile.Fd()), syscall.LOCK_EX); err != nil {
		_ = legacyFile.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Flock(int(legacyFile.Fd()), syscall.LOCK_UN)
		_ = legacyFile.Close()
	})

	started := make(chan struct{})
	entered := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- withPermissionApplyLock(homePath, func() error {
			entered <- struct{}{}
			return nil
		})
	}()
	<-started
	select {
	case <-entered:
		t.Fatal("permission mutation lifecycle entered while pre-existing legacy lock was held")
	case err := <-done:
		t.Fatalf("permission lock returned before legacy release: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	if err := syscall.Flock(int(legacyFile.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("permission mutation lifecycle did not enter after legacy lock release")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("permission lock did not complete after legacy lock release")
	}
}

func TestApplyPermissionsRefusesConcurrentTargetChange(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	previousHook := applyPermissionsBeforeTargetCommit
	applyPermissionsBeforeTargetCommit = func() {
		writeTestJSON(t, targetPath, map[string]any{"theme": "manual-change"})
	}
	t.Cleanup(func() { applyPermissionsBeforeTargetCommit = previousHook })

	if _, err := applyGovernedPermissions(t, homePath, "claude_code"); err == nil || !strings.Contains(err.Error(), "preimage changed") {
		t.Fatalf("apply error = %v, want contention refusal", err)
	}
	body, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeTestJSON(t, body)["theme"]; got != "manual-change" {
		t.Fatalf("concurrent edit overwritten: theme=%v", got)
	}
	if _, err := os.Stat(PermissionApplyPendingPath(homePath, "claude_code")); !os.IsNotExist(err) {
		t.Fatalf("contention left pending transaction: %v", err)
	}
}

func TestApplyPermissionsRequiresExactGovernanceAndApprovedRender(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	approved, action, decision := governedPermissionApproval(t, homePath, "claude_code")
	before, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		mutate   func(*PermissionRender, *envelope.ActionIntent, *envelope.PolicyDecision)
		wantText string
	}{
		{"missing decision", func(_ *PermissionRender, _ *envelope.ActionIntent, d *envelope.PolicyDecision) {
			*d = envelope.PolicyDecision{}
		}, "policy decision"},
		{"wrong capability", func(_ *PermissionRender, a *envelope.ActionIntent, _ *envelope.PolicyDecision) {
			a.Capability = "other"
		}, "capability"},
		{"wrong action", func(_ *PermissionRender, a *envelope.ActionIntent, _ *envelope.PolicyDecision) { a.Action = "other" }, "action"},
		{"wrong target", func(_ *PermissionRender, a *envelope.ActionIntent, _ *envelope.PolicyDecision) {
			a.Target.Path += ".other"
		}, "target"},
		{"wrong class", func(_ *PermissionRender, a *envelope.ActionIntent, _ *envelope.PolicyDecision) {
			a.MutationClass = "local_reversible"
		}, "security"},
		{"held decision", func(_ *PermissionRender, _ *envelope.ActionIntent, d *envelope.PolicyDecision) { d.Outcome = "hold" }, "allow"},
		{"wrong binding", func(_ *PermissionRender, _ *envelope.ActionIntent, d *envelope.PolicyDecision) {
			d.ActionIntentID = "act_other"
		}, "bind"},
		{"tampered content", func(r *PermissionRender, _ *envelope.ActionIntent, _ *envelope.PolicyDecision) {
			r.Content = append(r.Content, ' ')
		}, "content"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, a, d := approved, action, decision
			tt.mutate(&r, &a, &d)
			if _, err := applyPermissionsForTest(homePath, r, a, d, allowPermissionApply, finalizePermissionApply); err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.wantText)) {
				t.Fatalf("ApplyPermissions error = %v, want containing %q", err, tt.wantText)
			}
			after, err := os.ReadFile(targetPath)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatal("rejected governed apply mutated target")
			}
		})
	}
}

func TestApplyPermissionsRejectsApprovedRenderWhenAuthorityOrTargetChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, homePath, targetPath string)
	}{
		{"authority revision", func(t *testing.T, homePath, targetPath string) {
			writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read", "Write"}))
		}},
		{"target path", func(t *testing.T, homePath, targetPath string) {
			writePermissionFixture(t, homePath, targetPath+".other", permissionFixture([]string{"Read"}))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
			writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
			writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
			approved, action, decision := governedPermissionApproval(t, homePath, "claude_code")
			before, _ := os.ReadFile(targetPath)
			tc.mutate(t, homePath, targetPath)
			if _, err := applyPermissionsForTest(homePath, approved, action, decision, allowPermissionApply, finalizePermissionApply); err == nil || !strings.Contains(err.Error(), "approved render") {
				t.Fatalf("ApplyPermissions error = %v, want stale approved render refusal", err)
			}
			after, _ := os.ReadFile(targetPath)
			if !bytesEqual(before, after) {
				t.Fatal("stale approval mutated original target")
			}
			if _, err := os.Stat(targetPath + ".other"); !os.IsNotExist(err) {
				t.Fatalf("stale approval created changed target: %v", err)
			}
		})
	}
}

func TestPermissionRenderFormattingDifferencePublishesCanonicalBytes(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	compact := []byte(`{"permissions":{"allow":["Read"],"deny":["Bash(rm:*)"],"ask":["WebFetch"],"defaultMode":"default"},"autoMode":{"allow":["$defaults","Read"]}}`)
	if err := os.WriteFile(targetPath, compact, 0o644); err != nil {
		t.Fatal(err)
	}
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	approved, action, decision := governedPermissionApproval(t, homePath, "claude_code")
	if !approved.Changed {
		t.Fatal("byte-different semantic JSON reported Changed=false")
	}
	applied, err := applyPermissionsForTest(homePath, approved, action, decision, allowPermissionApply, finalizePermissionApply)
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := os.ReadFile(targetPath)
	if !bytesEqual(actual, approved.Content) || applied.Artifact.ContentSHA256 != "sha256:"+digest(actual) {
		t.Fatalf("target/artifact do not attest canonical bytes:\n%s", actual)
	}
}

func TestApplyPermissionsManifestFailurePreservesConcurrentManualEditAndPending(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	previousWriter := writeHarnessArtifactManifestForApply
	writeHarnessArtifactManifestForApply = func(string, []HarnessArtifact) error {
		writeTestJSON(t, targetPath, map[string]any{"theme": "manual-after-publication"})
		return os.ErrPermission
	}
	t.Cleanup(func() { writeHarnessArtifactManifestForApply = previousWriter })
	if _, err := applyGovernedPermissions(t, homePath, "claude_code"); err == nil || !strings.Contains(err.Error(), "manual") {
		t.Fatalf("ApplyPermissions error = %v, want ambiguous rollback evidence", err)
	}
	body, _ := os.ReadFile(targetPath)
	if got := decodeTestJSON(t, body)["theme"]; got != "manual-after-publication" {
		t.Fatalf("manual edit overwritten: %v", got)
	}
	if _, err := os.Stat(PermissionApplyPendingPath(homePath, "claude_code")); err != nil {
		t.Fatalf("pending recovery evidence missing: %v", err)
	}
	report, err := PermissionDrift(homePath, "claude_code")
	if err != nil || !report.Drifted || !strings.Contains(strings.Join(report.Details, " "), "incomplete") {
		t.Fatalf("drift=%+v err=%v", report, err)
	}
}

func TestApplyPermissionsSerializesSharedManifestAcrossHarnesses(t *testing.T) {
	homePath := t.TempDir()
	var approvals []struct {
		r PermissionRender
		a envelope.ActionIntent
		d envelope.PolicyDecision
	}
	for _, name := range []string{"claude_code", "codex"} {
		targetPath := filepath.Join(t.TempDir(), name+".json")
		writeTestJSON(t, targetPath, map[string]any{"theme": name})
		policy := permissionFixture([]string{"Read"})
		policy.Harness, policy.AuthorityRef = name, PermissionAuthorityRef(name)
		writePermissionFixture(t, homePath, targetPath, policy)
		r, a, d := governedPermissionApproval(t, homePath, name)
		approvals = append(approvals, struct {
			r PermissionRender
			a envelope.ActionIntent
			d envelope.PolicyDecision
		}{r, a, d})
	}
	previousWriter := writeHarnessArtifactManifestForApply
	var active, maxActive atomic.Int32
	writeHarnessArtifactManifestForApply = func(home string, artifacts []HarnessArtifact) error {
		n := active.Add(1)
		defer active.Add(-1)
		if n > maxActive.Load() {
			maxActive.Store(n)
		}
		time.Sleep(20 * time.Millisecond)
		return WriteHarnessArtifactManifest(home, artifacts)
	}
	t.Cleanup(func() { writeHarnessArtifactManifestForApply = previousWriter })
	var wg sync.WaitGroup
	errs := make(chan error, len(approvals))
	for _, approval := range approvals {
		approval := approval
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := applyPermissionsForTest(homePath, approval.r, approval.a, approval.d, allowPermissionApply, finalizePermissionApply)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if maxActive.Load() != 1 {
		t.Fatalf("shared manifest writer concurrency = %d, want 1", maxActive.Load())
	}
	artifacts, err := loadHarnessArtifacts(homePath)
	if err != nil || len(artifacts) != 2 {
		t.Fatalf("manifest artifacts=%+v err=%v", artifacts, err)
	}
}

func TestApplyPermissionsSyncsEveryPublicationBoundary(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	previous := permissionApplySyncBoundary
	var got []string
	permissionApplySyncBoundary = func(boundary, _ string) error {
		got = append(got, boundary)
		return nil
	}
	t.Cleanup(func() { permissionApplySyncBoundary = previous })
	if _, err := applyGovernedPermissions(t, homePath, "claude_code"); err != nil {
		t.Fatal(err)
	}
	want := []string{"pending_write", "target_publish", "manifest_write", "pending_remove"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sync boundaries = %v, want %v", got, want)
	}
}

func TestApplyPermissionsPendingSyncFailureDoesNotPublishTarget(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	before, _ := os.ReadFile(targetPath)
	previous := permissionApplySyncBoundary
	permissionApplySyncBoundary = func(boundary, _ string) error {
		if boundary == "pending_write" {
			return os.ErrInvalid
		}
		return nil
	}
	t.Cleanup(func() { permissionApplySyncBoundary = previous })
	if _, err := applyGovernedPermissions(t, homePath, "claude_code"); err == nil {
		t.Fatal("pending sync failure error = nil")
	}
	after, _ := os.ReadFile(targetPath)
	if !bytesEqual(before, after) {
		t.Fatal("pending sync failure published target")
	}
}

func TestPendingRecoveryRejectsChangedAuthorityRevision(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	approved, action, decision := governedPermissionApproval(t, homePath, "claude_code")
	previousHook := applyPermissionsAfterTargetWrite
	applyPermissionsAfterTargetWrite = func() error { return os.ErrClosed }
	if _, err := applyPermissionsForTest(homePath, approved, action, decision, allowPermissionApply, finalizePermissionApply); err == nil {
		t.Fatal("interrupted apply error = nil")
	}
	applyPermissionsAfterTargetWrite = previousHook
	t.Cleanup(func() { applyPermissionsAfterTargetWrite = previousHook })
	writePermissionFixture(t, homePath, targetPath+".changed", permissionFixture([]string{"Write"}))
	before, _ := os.ReadFile(targetPath)
	if _, err := applyPermissionsForTest(homePath, approved, action, decision, allowPermissionApply, finalizePermissionApply); err == nil || !strings.Contains(err.Error(), "source revision") {
		t.Fatalf("recovery error = %v, want source revision refusal", err)
	}
	after, _ := os.ReadFile(targetPath)
	if !bytesEqual(before, after) {
		t.Fatal("changed-authority recovery mutated prior target")
	}
}

func TestPendingRecoveryRejectsNonLeafRecoveryName(t *testing.T) {
	base := t.TempDir()
	homePath := filepath.Join(base, "home")
	targetPath := filepath.Join(base, "target", "settings.json")
	writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	approved, action, decision := governedPermissionApproval(t, homePath, "claude_code")
	before, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	sentinelPath := filepath.Join(base, "sentinel")
	if err := os.WriteFile(sentinelPath, []byte("must survive"), 0o600); err != nil {
		t.Fatal(err)
	}
	pending := permissionApplyPending{
		SchemaVersion: permissionApplyPendingSchemaVersion, Harness: "claude_code", TargetPath: targetPath,
		RecoveryName: "../sentinel", BeforeExisted: true, BeforeContent: before, BeforeSHA256: "sha256:" + digest(before),
		IntendedSHA256: approved.Artifact.ContentSHA256, SourceAuthorityRef: approved.Artifact.SourceAuthorityRef, SourceRevision: approved.Artifact.SourceRevision,
	}
	writeTestJSON(t, PermissionApplyPendingPath(homePath, "claude_code"), pending)
	if _, err := applyPermissionsForTest(homePath, approved, action, decision, allowPermissionApply, finalizePermissionApply); err == nil || !strings.Contains(err.Error(), "recovery name") {
		t.Fatalf("ApplyPermissions error = %v, want invalid recovery name", err)
	}
	if body, err := os.ReadFile(sentinelPath); err != nil || string(body) != "must survive" {
		t.Fatalf("non-leaf recovery name touched sentinel: body=%q err=%v", body, err)
	}
}

func TestApplyPermissionsRevalidatesAuthorizationAfterWaitingForMutationLock(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	approved, action, decision := governedPermissionApproval(t, homePath, "claude_code")
	before, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}

	lockHeld := make(chan struct{})
	releaseLock := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- withPermissionApplyLock(homePath, func() error {
			close(lockHeld)
			<-releaseLock
			return nil
		})
	}()
	<-lockHeld

	authorityCurrent := atomic.Bool{}
	authorityCurrent.Store(true)
	applyDone := make(chan error, 1)
	go func() {
		_, err := applyPermissionsForTest(homePath, approved, action, decision, func(_ envelope.ActionIntent, _ envelope.PolicyDecision, _ time.Time) error {
			if !authorityCurrent.Load() {
				return os.ErrPermission
			}
			return nil
		}, finalizePermissionApply)
		applyDone <- err
	}()
	authorityCurrent.Store(false)
	close(releaseLock)
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	if err := <-applyDone; err == nil || !strings.Contains(err.Error(), "authorization") {
		t.Fatalf("ApplyPermissions error = %v, want in-lock authorization refusal", err)
	}
	after, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesEqual(before, after) {
		t.Fatal("authorization revoked while waiting but target was published")
	}
	if _, err := os.Stat(HarnessArtifactManifestPath(homePath)); !os.IsNotExist(err) {
		t.Fatalf("authorization refusal wrote manifest: %v", err)
	}
}

func TestApplyPermissionsRevalidatesAuthorizationBeforeManifestOnlyMutation(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	initial, _, _ := governedPermissionApproval(t, homePath, "claude_code")
	if err := os.WriteFile(targetPath, initial.Content, 0o644); err != nil {
		t.Fatal(err)
	}
	approved, action, decision := governedPermissionApproval(t, homePath, "claude_code")
	if approved.Changed {
		t.Fatal("fixture target is not canonical")
	}
	if _, err := applyPermissionsForTest(homePath, approved, action, decision, func(_ envelope.ActionIntent, _ envelope.PolicyDecision, _ time.Time) error {
		return os.ErrPermission
	}, finalizePermissionApply); err == nil || !strings.Contains(err.Error(), "authorization") {
		t.Fatalf("ApplyPermissions error = %v, want manifest authorization refusal", err)
	}
	if _, err := os.Stat(HarnessArtifactManifestPath(homePath)); !os.IsNotExist(err) {
		t.Fatalf("authorization refusal wrote manifest: %v", err)
	}
}

func TestApplyPermissionsPersistsPendingBeforeReservationAdmission(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	approved, action, decision := governedPermissionApproval(t, homePath, "claude_code")
	before, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("authorization revoked")
	aborted := false
	_, err = ApplyPermissions(
		homePath, approved, action, decision,
		func(_ envelope.ActionIntent, _ envelope.PolicyDecision, _ time.Time) error { return injected },
		func(outcome PermissionApplyOutcome, authorize PermissionApplyAuthorization) error {
			if _, err := os.Stat(PermissionApplyPendingPath(homePath, "claude_code")); err != nil {
				t.Fatalf("reservation ran before durable permission pending: %v", err)
			}
			if current, err := os.ReadFile(targetPath); err != nil || !bytesEqual(current, before) {
				t.Fatalf("target changed before reservation: body=%q err=%v", current, err)
			}
			return authorize(outcome.Action, outcome.Decision, mustPermissionAuthorizedAt(t, outcome))
		},
		func(PermissionApplyOutcome) error { aborted = true; return nil },
		finalizePermissionApply,
	)
	if !errors.Is(err, injected) {
		t.Fatalf("ApplyPermissionsReserved error = %v", err)
	}
	if !aborted {
		t.Fatal("failed reservation admission did not idempotently abort partial reservation state")
	}
	if _, err := os.Stat(PermissionApplyPendingPath(homePath, "claude_code")); !os.IsNotExist(err) {
		t.Fatalf("failed reservation admission retained permission pending: %v", err)
	}
}

func TestApplyPermissionsAbortsReservationAfterSafePublicationRollback(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	approved, action, decision := governedPermissionApproval(t, homePath, "claude_code")
	before, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("target publication sync failed")
	previousSync := permissionApplySyncBoundary
	permissionApplySyncBoundary = func(stage, _ string) error {
		if stage == "target_publish" {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { permissionApplySyncBoundary = previousSync })
	reserved := false
	_, err = ApplyPermissions(
		homePath, approved, action, decision,
		allowPermissionApply,
		func(outcome PermissionApplyOutcome, authorize PermissionApplyAuthorization) error {
			if err := authorize(outcome.Action, outcome.Decision, mustPermissionAuthorizedAt(t, outcome)); err != nil {
				return err
			}
			reserved = true
			return nil
		},
		func(PermissionApplyOutcome) error {
			if !reserved {
				t.Fatal("abort ran without an active reservation")
			}
			if current, err := os.ReadFile(targetPath); err != nil || !bytesEqual(current, before) {
				t.Fatalf("reservation aborted before pre-image restoration: body=%q err=%v", current, err)
			}
			reserved = false
			return nil
		},
		finalizePermissionApply,
	)
	if !errors.Is(err, injected) {
		t.Fatalf("ApplyPermissionsReserved error = %v", err)
	}
	if reserved {
		t.Fatal("safe rollback left reservation active")
	}
	if _, err := os.Stat(PermissionApplyPendingPath(homePath, "claude_code")); !os.IsNotExist(err) {
		t.Fatalf("safe rollback retained permission pending: %v", err)
	}
}

func TestApplyPermissionsRetainsReservationForAmbiguousBeginPublication(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	if err := homepkg.Init(homePath, "test", time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	before, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{Owner: "operator:test", Actor: "operator:test", SuccessPredicate: "permission outcome", State: "ready"}, time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "operator:test", time.Date(2026, 7, 15, 12, 0, 1, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "operator:test"}, time.Date(2026, 7, 15, 12, 0, 2, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	approved, action, decision := governedPermissionApproval(t, homePath, "claude_code")
	action.IDs.WorkID = w.ID
	decision.IDs.WorkID = w.ID
	decision.ActionIntentID = action.ID

	inputFor := func(outcome PermissionApplyOutcome) (work.ReceiptedActionInput, time.Time, error) {
		authorizedAt, err := time.Parse(time.RFC3339Nano, outcome.AuthorizedAt)
		if err != nil {
			return work.ReceiptedActionInput{}, time.Time{}, err
		}
		return work.ReceiptedActionInput{
			Action: outcome.Action, PreEvaluatedDecision: &outcome.Decision, ExpectedWorkState: "running",
			Outcome: "completed", Changed: outcome.Changed, Summary: "permission activation completed", MoveType: "control_plane_repair",
			BeforeRefs: []string{"sha256:" + digest(outcome.Render.Preimage)}, AfterRefs: []string{outcome.Render.Artifact.ContentSHA256},
		}, authorizedAt, nil
	}
	reserve := func(outcome PermissionApplyOutcome, authorize PermissionApplyAuthorization) error {
		in, at, err := inputFor(outcome)
		if err != nil {
			return err
		}
		return store.ReserveReceiptedAction(w.ID, in, at, func() error {
			return authorize(outcome.Action, outcome.Decision, at)
		})
	}
	abort := func(outcome PermissionApplyOutcome) error {
		return store.AbortReceiptedActionReservation(w.ID, outcome.Action.ID)
	}
	finalize := func(outcome PermissionApplyOutcome) error {
		in, at, err := inputFor(outcome)
		if err != nil {
			return err
		}
		_, err = store.RecordReservedReceiptedAction(w.ID, in, at)
		return err
	}

	injected := errors.New("rooted CAS returned ambiguous after publication")
	previousBegin := beginPermissionRootedFileCAS
	beginPermissionRootedFileCAS = func(root *os.Root, canonicalName, recoveryName string, _ []byte, _ os.FileInfo, _ []byte, _ os.FileInfo) (permissionRootedCAS, error) {
		preimage, err := root.ReadFile(canonicalName)
		if err != nil {
			t.Fatal(err)
		}
		postimage, err := root.ReadFile(recoveryName)
		if err != nil {
			t.Fatal(err)
		}
		if err := root.WriteFile(canonicalName, postimage, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := root.WriteFile(recoveryName, preimage, 0o644); err != nil {
			t.Fatal(err)
		}
		return ambiguousPermissionRootedCAS{}, injected
	}
	t.Cleanup(func() { beginPermissionRootedFileCAS = previousBegin })
	_, err = ApplyPermissions(homePath, approved, action, decision, allowPermissionApply, reserve, abort, finalize)
	if !errors.Is(err, injected) || !IsPermissionApplyFinalizationPending(err) {
		t.Fatalf("ApplyPermissions error = %v, want ambiguous publication pending recovery", err)
	}
	if actual, err := os.ReadFile(targetPath); err != nil || !bytesEqual(actual, approved.Content) {
		t.Fatalf("ambiguous publication canonical = %q err=%v", actual, err)
	}
	if _, err := os.Stat(PermissionApplyPendingPath(homePath, "claude_code")); err != nil {
		t.Fatalf("ambiguous publication removed pending evidence: %v", err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "verifying", Actor: "operator:test"}, time.Date(2026, 7, 15, 12, 2, 0, 0, time.UTC)); err == nil || !strings.Contains(err.Error(), "pending receipted action "+action.ID) {
		t.Fatalf("ambiguous publication released work reservation: %v", err)
	}

	beginPermissionRootedFileCAS = previousBegin
	rejected := errors.New("stop after recovering ambiguous publication")
	_, err = ApplyPermissions(homePath, approved, action, decision, func(envelope.ActionIntent, envelope.PolicyDecision, time.Time) error {
		return rejected
	}, reserve, abort, finalize)
	if !errors.Is(err, rejected) {
		t.Fatalf("recovery retry error = %v", err)
	}
	if actual, err := os.ReadFile(targetPath); err != nil || !bytesEqual(actual, before) {
		t.Fatalf("ambiguous publication recovery did not restore preimage: body=%q err=%v", actual, err)
	}
	if _, err := os.Stat(PermissionApplyPendingPath(homePath, "claude_code")); !os.IsNotExist(err) {
		t.Fatalf("recovered ambiguous publication retained pending evidence: %v", err)
	}
	if advanced, err := store.Advance(w.ID, work.AdvanceInput{To: "verifying", Actor: "operator:test"}, time.Date(2026, 7, 15, 12, 3, 0, 0, time.UTC)); err != nil || advanced.State != "verifying" {
		t.Fatalf("recovered ambiguous publication retained work reservation: work=%+v err=%v", advanced, err)
	}
}

func mustPermissionAuthorizedAt(t *testing.T, outcome PermissionApplyOutcome) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, outcome.AuthorizedAt)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func TestApplyPermissionsFinalizationFailureRecoversExactPublishedOutcome(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	approved, action, decision := governedPermissionApproval(t, homePath, "claude_code")
	injected := errors.New("injected receipt interruption")
	var attempted PermissionApplyOutcome
	if _, err := applyPermissionsForTest(homePath, approved, action, decision, allowPermissionApply, func(outcome PermissionApplyOutcome) error {
		attempted = outcome
		return injected
	}); !errors.Is(err, injected) || !IsPermissionApplyFinalizationPending(err) {
		t.Fatalf("ApplyPermissions error = %v, want recoverable finalization failure", err)
	}
	if !attempted.Changed || !attempted.TargetChanged || !attempted.ManifestChanged ||
		!reflect.DeepEqual(attempted.FilesChanged, []string{targetPath, HarnessArtifactManifestPath(homePath)}) {
		t.Fatalf("attempted outcome = %+v", attempted)
	}
	if _, err := os.Stat(PermissionApplyPendingPath(homePath, "claude_code")); err != nil {
		t.Fatalf("published outcome did not retain pending identity: %v", err)
	}
	current, err := RenderPermissions(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	retryAction, retryDecision := action, decision
	retryAction.ID = "act_permission_retry"
	retryDecision.ID = "pol_permission_retry"
	retryDecision.ActionIntentID = retryAction.ID
	var recovered PermissionApplyOutcome
	applied, err := applyPermissionsForTest(homePath, current, retryAction, retryDecision, func(_ envelope.ActionIntent, _ envelope.PolicyDecision, _ time.Time) error {
		return errors.New("new action must not authorize before prior outcome recovery")
	}, func(outcome PermissionApplyOutcome) error {
		recovered = outcome
		return nil
	})
	if err != nil {
		t.Fatalf("recover finalized permission outcome: %v", err)
	}
	if !applied.Changed || recovered.Action.ID != action.ID || recovered.Decision.ID != decision.ID ||
		recovered.AuthorizedAt == "" || !recovered.Changed || !recovered.TargetChanged || !recovered.ManifestChanged ||
		!reflect.DeepEqual(recovered.FilesChanged, attempted.FilesChanged) {
		t.Fatalf("recovered outcome lost exact identity: applied=%+v recovered=%+v", applied, recovered)
	}
	if _, err := os.Stat(PermissionApplyPendingPath(homePath, "claude_code")); !os.IsNotExist(err) {
		t.Fatalf("recovered outcome left pending identity: %v", err)
	}
}

func TestApplyPermissionsManifestOnlyOutcomeIsChanged(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	initial, _, _ := governedPermissionApproval(t, homePath, "claude_code")
	if err := os.WriteFile(targetPath, initial.Content, 0o644); err != nil {
		t.Fatal(err)
	}
	approved, action, decision := governedPermissionApproval(t, homePath, "claude_code")
	if approved.Changed {
		t.Fatal("fixture target is not canonical")
	}
	var finalized PermissionApplyOutcome
	if _, err := applyPermissionsForTest(homePath, approved, action, decision, allowPermissionApply, func(outcome PermissionApplyOutcome) error {
		finalized = outcome
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !finalized.Changed || finalized.TargetChanged || !finalized.ManifestChanged ||
		!reflect.DeepEqual(finalized.FilesChanged, []string{HarnessArtifactManifestPath(homePath)}) {
		t.Fatalf("manifest-only outcome = %+v", finalized)
	}
}

func TestPermissionRecoveryCleanupPreservesReplacementLeaf(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	approved, action, decision := governedPermissionApproval(t, homePath, "claude_code")
	injected := errors.New("receipt unavailable")
	if _, err := applyPermissionsForTest(homePath, approved, action, decision, allowPermissionApply, func(PermissionApplyOutcome) error { return injected }); !errors.Is(err, injected) {
		t.Fatalf("ApplyPermissions error = %v", err)
	}
	var pending permissionApplyPending
	body, err := os.ReadFile(PermissionApplyPendingPath(homePath, "claude_code"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &pending); err != nil {
		t.Fatal(err)
	}
	replacement := []byte("concurrent replacement must survive\n")
	previous := permissionApplyRecoveryAfterQuarantine
	permissionApplyRecoveryAfterQuarantine = func(_ string, recoveryName string) {
		if recoveryName == pending.RecoveryName {
			if err := os.WriteFile(filepath.Join(filepath.Dir(targetPath), recoveryName), replacement, 0o600); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(func() { permissionApplyRecoveryAfterQuarantine = previous })
	current, _, _ := governedPermissionApproval(t, homePath, "claude_code")
	if _, err := applyPermissionsForTest(homePath, current, action, decision, allowPermissionApply, finalizePermissionApply); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(filepath.Dir(targetPath), pending.RecoveryName))
	if err != nil || !bytesEqual(got, replacement) {
		t.Fatalf("replacement recovery leaf was removed: body=%q err=%v", got, err)
	}
}

func TestPendingRecoveryRollsBackInterruptedCreationToAbsent(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "target", "settings.json")
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	approved, action, decision := governedPermissionApproval(t, homePath, "claude_code")
	previous := applyPermissionsAfterTargetWrite
	applyPermissionsAfterTargetWrite = func() error { return os.ErrClosed }
	if _, err := applyPermissionsForTest(homePath, approved, action, decision, allowPermissionApply, finalizePermissionApply); err == nil {
		t.Fatal("interrupted creation error = nil")
	}
	applyPermissionsAfterTargetWrite = previous
	t.Cleanup(func() { applyPermissionsAfterTargetWrite = previous })
	if _, err := os.Stat(targetPath); err != nil {
		t.Fatalf("interruption did not publish creation: %v", err)
	}
	refuseNew := func(_ envelope.ActionIntent, _ envelope.PolicyDecision, _ time.Time) error { return os.ErrPermission }
	if _, err := applyPermissionsForTest(homePath, approved, action, decision, refuseNew, finalizePermissionApply); err == nil || !strings.Contains(err.Error(), "authorization") {
		t.Fatalf("post-recovery apply error = %v, want new-action authorization refusal", err)
	}
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Fatalf("interrupted creation was not rolled back to absent: %v", err)
	}
	if _, err := os.Stat(PermissionApplyPendingPath(homePath, "claude_code")); !os.IsNotExist(err) {
		t.Fatalf("creation recovery left pending transaction: %v", err)
	}
}

func TestPendingRecoveryClearsPrePublicationAbsentCreation(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "missing", "settings.json")
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	approved, action, decision := governedPermissionApproval(t, homePath, "claude_code")
	outcome := newPermissionApplyOutcome(homePath, approved, action, decision, time.Date(2026, 7, 15, 12, 1, 0, 0, time.UTC), true)
	writeTestJSON(t, PermissionApplyPendingPath(homePath, "claude_code"), newPermissionApplyPending(outcome))
	refuseNew := func(_ envelope.ActionIntent, _ envelope.PolicyDecision, _ time.Time) error { return os.ErrPermission }
	if _, err := applyPermissionsForTest(homePath, approved, action, decision, refuseNew, finalizePermissionApply); err == nil || !strings.Contains(err.Error(), "authorization") {
		t.Fatalf("post-recovery apply error = %v, want new-action authorization refusal", err)
	}
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Fatalf("pre-publication recovery created target: %v", err)
	}
	if _, err := os.Stat(PermissionApplyPendingPath(homePath, "claude_code")); !os.IsNotExist(err) {
		t.Fatalf("pre-publication recovery left pending transaction: %v", err)
	}
}

func TestPendingCreationRecoveryPreservesConcurrentCanonicalReplacement(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "target", "settings.json")
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	approved, action, decision := governedPermissionApproval(t, homePath, "claude_code")
	previousInterrupt := applyPermissionsAfterTargetWrite
	applyPermissionsAfterTargetWrite = func() error { return os.ErrClosed }
	if _, err := applyPermissionsForTest(homePath, approved, action, decision, allowPermissionApply, finalizePermissionApply); err == nil {
		t.Fatal("interrupted creation error = nil")
	}
	applyPermissionsAfterTargetWrite = previousInterrupt
	t.Cleanup(func() { applyPermissionsAfterTargetWrite = previousInterrupt })
	replacement := map[string]any{"theme": "manual-after-quarantine"}
	previousQuarantine := permissionApplyAfterCanonicalQuarantine
	permissionApplyAfterCanonicalQuarantine = func(path string) {
		writeTestJSON(t, path, replacement)
	}
	t.Cleanup(func() { permissionApplyAfterCanonicalQuarantine = previousQuarantine })
	if _, err := applyPermissionsForTest(homePath, approved, action, decision, allowPermissionApply, finalizePermissionApply); err == nil || !strings.Contains(err.Error(), "approved render") {
		t.Fatalf("post-recovery apply error = %v, want stale render after concurrent replacement", err)
	}
	body, err := os.ReadFile(targetPath)
	if err != nil || decodeTestJSON(t, body)["theme"] != "manual-after-quarantine" {
		t.Fatalf("concurrent canonical replacement lost: body=%q err=%v", body, err)
	}
}

func TestPendingCreationRecoveryRejectsMismatchedRecoveryContent(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "target", "settings.json")
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	approved, action, decision := governedPermissionApproval(t, homePath, "claude_code")
	previous := applyPermissionsAfterTargetWrite
	applyPermissionsAfterTargetWrite = func() error { return os.ErrClosed }
	if _, err := applyPermissionsForTest(homePath, approved, action, decision, allowPermissionApply, finalizePermissionApply); err == nil {
		t.Fatal("interrupted creation error = nil")
	}
	applyPermissionsAfterTargetWrite = previous
	t.Cleanup(func() { applyPermissionsAfterTargetWrite = previous })
	var pending permissionApplyPending
	readBody, err := os.ReadFile(PermissionApplyPendingPath(homePath, "claude_code"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(readBody, &pending); err != nil {
		t.Fatal(err)
	}
	recoveryPath := filepath.Join(filepath.Dir(targetPath), pending.RecoveryName)
	if err := os.Remove(recoveryPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recoveryPath, []byte("mismatched recovery evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := applyPermissionsForTest(homePath, approved, action, decision, allowPermissionApply, finalizePermissionApply); err == nil || !strings.Contains(strings.ToLower(err.Error()), "recovery") {
		t.Fatalf("mismatched recovery error = %v", err)
	}
	if body, err := os.ReadFile(recoveryPath); err != nil || string(body) != "mismatched recovery evidence\n" {
		t.Fatalf("mismatched recovery evidence was not preserved: body=%q err=%v", body, err)
	}
	if _, err := os.Stat(PermissionApplyPendingPath(homePath, "claude_code")); err != nil {
		t.Fatalf("mismatched recovery removed pending evidence: %v", err)
	}
}

func TestPendingCreationRecoveryRejectsCanonicalRecoveryAliasBeforeFinalization(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "target", "settings.json")
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	approved, action, decision := governedPermissionApproval(t, homePath, "claude_code")
	injected := errors.New("receipt unavailable")
	if _, err := applyPermissionsForTest(homePath, approved, action, decision, allowPermissionApply, func(PermissionApplyOutcome) error { return injected }); !errors.Is(err, injected) {
		t.Fatalf("ApplyPermissions error = %v", err)
	}
	pendingPath := PermissionApplyPendingPath(homePath, "claude_code")
	var pending permissionApplyPending
	body, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &pending); err != nil {
		t.Fatal(err)
	}
	pending.RecoveryName = filepath.Base(targetPath)
	writeTestJSON(t, pendingPath, pending)
	current, _, _ := governedPermissionApproval(t, homePath, "claude_code")
	finalized := false
	if _, err := applyPermissionsForTest(homePath, current, action, decision, allowPermissionApply, func(PermissionApplyOutcome) error {
		finalized = true
		return nil
	}); err == nil || !strings.Contains(strings.ToLower(err.Error()), "canonical") {
		t.Fatalf("aliased recovery error = %v finalized=%t, want pre-finalization canonical alias refusal", err, finalized)
	}
	if finalized {
		t.Fatal("canonical recovery alias reached outcome finalization")
	}
	if actual, err := os.ReadFile(targetPath); err != nil || !bytesEqual(actual, approved.Content) {
		t.Fatalf("canonical target was deleted or changed: body=%q err=%v", actual, err)
	}
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("canonical alias refusal removed pending evidence: %v", err)
	}
}

func TestValidatePermissionApplyPendingRejectsAliasedAndDerivedIdentity(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{"theme": "dark"})
	writePermissionFixture(t, homePath, targetPath, permissionFixture([]string{"Read"}))
	render, action, decision := governedPermissionApproval(t, homePath, "claude_code")
	base := newPermissionApplyPending(newPermissionApplyOutcome(homePath, render, action, decision, time.Date(2026, 7, 15, 12, 1, 0, 0, time.UTC), true))
	canonicalName := filepath.Base(targetPath)
	for _, tc := range []struct {
		name   string
		mutate func(*permissionApplyPending)
	}{
		{name: "recovery aliases canonical", mutate: func(p *permissionApplyPending) { p.RecoveryName = canonicalName }},
		{name: "rollback aliases canonical", mutate: func(p *permissionApplyPending) { p.RollbackName = canonicalName }},
		{name: "recovery aliases rollback", mutate: func(p *permissionApplyPending) { p.RollbackName = p.RecoveryName }},
		{name: "target change omitted", mutate: func(p *permissionApplyPending) {
			p.TargetChanged = false
			p.RecoveryName = ""
			p.RollbackName = ""
			p.FilesChanged = []string{HarnessArtifactManifestPath(homePath)}
		}},
		{name: "false target change claimed", mutate: func(p *permissionApplyPending) {
			p.BeforeContent = append([]byte(nil), p.IntendedContent...)
			p.BeforeSHA256 = p.IntendedSHA256
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pending := base
			tc.mutate(&pending)
			if _, err := validatePermissionApplyPending(homePath, "claude_code", pending); err == nil {
				t.Fatalf("validatePermissionApplyPending accepted %+v", pending)
			}
		})
	}
}

func TestPermissionRecoveryCleanupRejectsAliasedLeavesBeforeMutation(t *testing.T) {
	cleanups := []struct {
		name string
		run  func(permissionApplyPending) error
	}{
		{name: "published", run: cleanupPublishedPermissionRecovery},
		{name: "unused", run: cleanupUnusedPermissionRecovery},
		{name: "interrupted creation", run: func(p permissionApplyPending) error {
			return cleanupInterruptedPermissionCreation(p, true)
		}},
	}
	aliases := []struct {
		name   string
		mutate func(*permissionApplyPending, string)
	}{
		{name: "canonical", mutate: func(p *permissionApplyPending, canonical string) {
			p.RecoveryName = canonical
		}},
		{name: "duplicate cleanup leaves", mutate: func(p *permissionApplyPending, _ string) {
			p.RollbackName = p.RecoveryName
		}},
	}
	for _, cleanup := range cleanups {
		for _, alias := range aliases {
			t.Run(cleanup.name+"/"+alias.name, func(t *testing.T) {
				dir := t.TempDir()
				targetPath := filepath.Join(dir, "settings.json")
				intended := []byte("published permissions\n")
				if err := os.WriteFile(targetPath, intended, 0o600); err != nil {
					t.Fatal(err)
				}
				pending := permissionApplyPending{
					TargetPath: targetPath, RecoveryName: ".settings.recovery", RollbackName: ".settings.rollback",
					BeforeExisted: false, IntendedContent: intended,
				}
				if err := os.Link(targetPath, filepath.Join(dir, pending.RecoveryName)); err != nil {
					t.Fatal(err)
				}
				alias.mutate(&pending, filepath.Base(targetPath))
				if err := cleanup.run(pending); err == nil || !strings.Contains(strings.ToLower(err.Error()), "alias") {
					t.Fatalf("cleanup error = %v, want alias refusal", err)
				}
				if actual, err := os.ReadFile(targetPath); err != nil || !bytesEqual(actual, intended) {
					t.Fatalf("cleanup mutated canonical target: body=%q err=%v", actual, err)
				}
			})
		}
	}
}

func governedPermissionApproval(t *testing.T, homePath, harnessName string) (PermissionRender, envelope.ActionIntent, envelope.PolicyDecision) {
	t.Helper()
	render, err := RenderPermissions(homePath, harnessName)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	action := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent, ID: "act_permission_test", CreatedAt: now.Format(time.RFC3339),
			Producer: envelope.Producer{Kind: "cli", Name: "compound"}, IDs: envelope.IDs{WorkID: "work_permission_test"},
			Authority: envelope.Authority{SourceClass: "runtime", Owner: "compound", Status: "current"}, Privacy: "private", Risk: "tier1",
			EvidenceRefs: []string{render.Artifact.SourceAuthorityRef, render.Artifact.SourceRevision}, Uncertainty: envelope.Uncertainty{Level: "known"},
		},
		Actor: "operator:test", Capability: "harness.permissions.apply", Action: "reconcile_harness_permissions",
		Target:        envelope.ActionTarget{Kind: "harness_settings", ID: harnessName, Path: render.TargetPath},
		MutationClass: "security", Reversibility: "easy", BlastRadius: "local", RollbackPlan: "restore exact pre-image",
	}
	decision := envelope.PolicyDecision{
		Header: envelope.Header{
			SchemaVersion: envelope.SVPolicyDecision, ID: "pol_permission_test", CreatedAt: now.Format(time.RFC3339),
			Producer: envelope.Producer{Kind: "cli", Name: "compound"}, IDs: envelope.IDs{WorkID: "work_permission_test"},
			Authority: envelope.Authority{SourceClass: "runtime", Status: "current"}, Privacy: "private", Risk: "tier1",
			EvidenceRefs: []string{}, Uncertainty: envelope.Uncertainty{Level: "known"},
		},
		ActionIntentID: action.ID, Outcome: "allow_with_obligations", Reasons: []string{"test grant"}, Obligations: []string{"emit receipt"}, StandingGrantID: "grant_test",
	}
	return render, action, decision
}

func applyGovernedPermissions(t *testing.T, homePath, harnessName string) (PermissionRender, error) {
	t.Helper()
	render, action, decision := governedPermissionApproval(t, homePath, harnessName)
	return applyPermissionsForTest(homePath, render, action, decision, allowPermissionApply, finalizePermissionApply)
}

func applyPermissionsForTest(homePath string, approved PermissionRender, action envelope.ActionIntent, decision envelope.PolicyDecision, authorize PermissionApplyAuthorization, finalize PermissionApplyFinalize) (PermissionRender, error) {
	return ApplyPermissions(
		homePath, approved, action, decision, authorize,
		func(outcome PermissionApplyOutcome, authorize PermissionApplyAuthorization) error {
			at, err := time.Parse(time.RFC3339Nano, outcome.AuthorizedAt)
			if err != nil {
				return err
			}
			return authorize(outcome.Action, outcome.Decision, at)
		},
		func(PermissionApplyOutcome) error { return nil },
		finalize,
	)
}

func allowPermissionApply(_ envelope.ActionIntent, _ envelope.PolicyDecision, _ time.Time) error {
	return nil
}

func finalizePermissionApply(PermissionApplyOutcome) error { return nil }

func bytesEqual(a, b []byte) bool { return reflect.DeepEqual(a, b) }

func permissionFixture(allow []string) PermissionAuthority {
	return PermissionAuthority{
		SchemaVersion: PermissionAuthoritySchemaVersion, Harness: "claude_code", AuthorityRef: PermissionAuthorityRef("claude_code"),
		TargetPath:  "/synthetic/settings.json",
		Permissions: PermissionRules{Allow: allow, Deny: []string{"Bash(rm:*)"}, Ask: []string{"WebFetch"}, DefaultMode: "default"},
		AutoMode:    AutoModePolicy{Allow: []string{"$defaults", "Read"}},
	}
}

func writePermissionFixture(t *testing.T, homePath, targetPath string, policy PermissionAuthority) {
	t.Helper()
	policy.TargetPath = targetPath
	writeTestJSON(t, PermissionAuthorityPath(homePath, policy.Harness), policy)
}

func writeTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func decodeTestJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func assertStrings(t *testing.T, value any, want []string) {
	t.Helper()
	raw := value.([]any)
	got := make([]string, len(raw))
	for i := range raw {
		got[i] = raw[i].(string)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func assertPermissionDrift(t *testing.T, homePath string, want bool, detail string) {
	t.Helper()
	report, err := PermissionDrift(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if report.Drifted != want {
		t.Fatalf("drifted=%v want=%v details=%v", report.Drifted, want, report.Details)
	}
	if detail != "" && !strings.Contains(strings.Join(report.Details, " "), detail) {
		t.Fatalf("details=%v want %q", report.Details, detail)
	}
}
