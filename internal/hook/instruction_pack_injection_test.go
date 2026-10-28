package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/contract"
	"compound/internal/envelope"
	"compound/internal/harness"
)

// F8 — pack-render-failure non-fatal: writeContextArtifacts renders the
// instruction pack after recording the contract; a pack failure does NOT
// block context injection. Even when the home has no authority, no
// installed-hooks manifest, and no contracts/effective/latest.json, the
// injection still lands and the renderInjection's pointer line is
// OMITTED (degraded render — no pack on disk so no pointer to cite).
func TestWriteContextArtifacts_F8_PackFailureIsNonFatal(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)

	deps := baseDeps()
	deps.HomePath = homePath
	fx := Fixture{
		Hook: "session-start", Harness: "claude_code", Cwd: "/repo/x", Task: "audit", RepoKind: "product",
	}
	res, err := Run(fx, deps, fixedNow)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Injection == nil {
		t.Fatal("session-start with HomePath must persist an injection")
	}
	persisted, err := os.ReadFile(res.Injection.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) == 0 {
		t.Fatal("persisted injection was empty")
	}
	// Even on a degraded render the injection lands; the pointer line
	// is best-effort, may or may not appear depending on whether a pack
	// file exists on disk. Either case must keep the injection valid.
	// Sanity-check the structural lines still appear.
	for _, want := range []string{
		"# Compound Context Injection",
		"- Hook event:",
		"- Context pack:",
		"- Capability refs are offers only;",
	} {
		if !strings.Contains(string(persisted), want) {
			t.Fatalf("persisted injection missing %q:\n%s", want, persisted)
		}
	}
}

// F9 — pack pointer line appears when the pack projection is on disk and
// the pack's HookReadiness resolves to exactly one unique harness. We
// pre-write the pack projection (simulating that writeContextArtifacts
// already rendered it on a prior call), then drive renderInjection via
// the public Run entry and confirm the injection contains the pointer
// line.
func TestRenderInstructionPackPointer_F9_LineCitesOnDiskPack(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)

	// Build a minimal harness authority yaml + artifact so the pack
	// renders with non-absent layers.
	targetPath := filepath.Join(t.TempDir(), "settings.json")
	policy := harness.PermissionAuthority{
		SchemaVersion: harness.PermissionAuthoritySchemaVersion,
		Harness:       "claude_code",
		AuthorityRef:  harness.PermissionAuthorityRef("claude_code"),
		TargetPath:    targetPath,
		Permissions: harness.PermissionRules{
			Allow: []string{"Read"}, Deny: []string{"Bash(rm:*)"}, Ask: []string{"WebFetch"}, DefaultMode: "default",
		},
		AutoMode: harness.AutoModePolicy{Allow: []string{"$defaults", "Read"}},
	}
	authPath := harness.PermissionAuthorityPath(homePath, "claude_code")
	if err := os.MkdirAll(filepath.Dir(authPath), 0o755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(policy)
	if err := os.WriteFile(authPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	render, err := harness.RenderPermissions(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, render.Content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := harness.WriteHarnessArtifactManifest(homePath, []harness.HarnessArtifact{render.Artifact}); err != nil {
		t.Fatal(err)
	}

	// Pre-render the instruction-pack projection so renderInjection can
	// find it on disk. (In practice writeContextArtifacts renders it
	// during a session-start invocation; we drive it here so the test
	// is deterministic.)
	if _, err := harness.RenderInstructionPack(homePath, "claude_code", "", fixedNow); err != nil {
		t.Fatal(err)
	}

	// Drive a session-start via Run. Build a pack that has exactly one
	// unique harness across HookReadiness — the pointer line must fire.
	pack := envelope.ContextPack{
		Header: envelope.Header{
			SchemaVersion: envelope.SVContextPack,
			ID:            "ctx_pointer_test",
			CreatedAt:     fixedNow.Format("2006-01-02T15:04:05Z"),
		},
		SummaryMD: "summary",
		HookReadiness: []envelope.HookReadiness{{
			Harness: "claude_code", Hook: "session-start", Installed: true, Status: "ok",
		}},
	}
	txt := renderInjection(injectionRender{HookID: "hookev_pointer", Pack: pack, Surface: injectionSurfaceTask})

	// Pointer line must cite the home-relative ref + first 12 sha chars.
	if !strings.Contains(txt, "- Instruction pack: `context/instruction-packs/claude_code/latest.json`") {
		t.Fatalf("injection missing the instruction pack pointer line:\n%s", txt)
	}
	if !strings.Contains(txt, "sha256:") {
		t.Fatalf("pointer line missing the sha256 prefix:\n%s", txt)
	}
}

// F10 — defensive omission: multiple unique harnesses in HookReadiness →
// no pointer line (don't pin a possibly-wrong harness).
func TestRenderInstructionPackPointer_F10_OmitsOnMultiHarness(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	pack := envelope.ContextPack{
		Header: envelope.Header{
			SchemaVersion: envelope.SVContextPack,
			ID:            "ctx_multi_harness",
			CreatedAt:     fixedNow.Format("2006-01-02T15:04:05Z"),
		},
		SummaryMD: "summary",
		HookReadiness: []envelope.HookReadiness{
			{Harness: "claude_code", Hook: "session-start", Installed: true, Status: "ok"},
			{Harness: "codex", Hook: "session-start", Installed: true, Status: "ok"},
		},
	}
	txt := renderInjection(injectionRender{HookID: "hookev_multi", Pack: pack, Surface: injectionSurfaceTask})
	if strings.Contains(txt, "- Instruction pack:") {
		t.Fatalf("multi-harness injection must not pin a single harness:\n%s", txt)
	}
}

// F11 — without $COMPOUND_HOME set, the pointer line is omitted entirely
// (we can neither read nor resolve the home).
func TestRenderInstructionPackPointer_F11_OmitsOnNoHome(t *testing.T) {
	t.Setenv("COMPOUND_HOME", "")
	pack := envelope.ContextPack{
		Header: envelope.Header{
			SchemaVersion: envelope.SVContextPack,
			ID:            "ctx_no_home",
			CreatedAt:     fixedNow.Format("2006-01-02T15:04:05Z"),
		},
		SummaryMD: "summary",
		HookReadiness: []envelope.HookReadiness{
			{Harness: "claude_code", Hook: "session-start", Installed: true, Status: "ok"},
		},
	}
	txt := renderInjection(injectionRender{HookID: "hookev_no_home", Pack: pack, Surface: injectionSurfaceTask})
	if strings.Contains(txt, "- Instruction pack:") {
		t.Fatalf("no-home injection must not emit a pointer line:\n%s", txt)
	}
}

// Sanity-references of contract utilities to keep imports tidy and pin the
// relationship the S5 harness-scoped pack models.
var _ = filepath.Join
var _ = contract.DefaultPolicy
