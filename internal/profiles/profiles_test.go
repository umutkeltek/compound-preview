package profiles

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- DefaultRoles / roles.go ---

func TestDefaultRolesAllValidAndUnique(t *testing.T) {
	roles := DefaultRoles()
	if len(roles) == 0 {
		t.Fatal("DefaultRoles() returned no roles")
	}
	seen := make(map[string]bool, len(roles))
	for _, role := range roles {
		if seen[role.ID] {
			t.Fatalf("duplicate role id %q", role.ID)
		}
		seen[role.ID] = true
		if err := validateRole(role); err != nil {
			t.Fatalf("default role %q fails validateRole: %v", role.ID, err)
		}
	}
}

// TestDefaultRolesIndependentCopies proves cloneRoles gives every caller its
// own slice/map: mutating one call's result must never leak into another.
func TestDefaultRolesIndependentCopies(t *testing.T) {
	first := DefaultRoles()
	first[0].Emit = append(first[0].Emit, HarnessCodex, HarnessCodex, HarnessCodex)
	if first[0].Models == nil {
		first[0].Models = map[Harness]string{}
	}
	first[0].Models[HarnessClaude] = "mutated"

	second := DefaultRoles()
	if len(second[0].Emit) == len(first[0].Emit) {
		t.Fatalf("mutating one DefaultRoles() call's Emit slice leaked into a fresh call: %v vs %v", first[0].Emit, second[0].Emit)
	}
	if got := second[0].Models[HarnessClaude]; got == "mutated" {
		t.Fatal("mutating one DefaultRoles() call's Models map leaked into a fresh call")
	}
}

// --- TargetsForHome ---

func TestTargetsForHome(t *testing.T) {
	targets := TargetsForHome("example-home")
	if want := filepath.Join("example-home", ".claude", "agents"); targets[HarnessClaude] != want {
		t.Fatalf("claude target = %q, want %q", targets[HarnessClaude], want)
	}
	if want := filepath.Join("example-home", ".codex", "agents"); targets[HarnessCodex] != want {
		t.Fatalf("codex target = %q, want %q", targets[HarnessCodex], want)
	}
}

// --- Render ---

func TestRenderFrontmatterAndFooter(t *testing.T) {
	role := Role{
		ID:              "test-role",
		Family:          "lane",
		Description:     "a test role",
		CapabilityClass: "build-implement",
		QualityTier:     "balanced",
		Tools:           "Read, Grep",
		Body:            "  Body text.  ",
		Models:          map[Harness]string{HarnessClaude: "claude-sonnet-5"},
	}
	out := Render(role, HarnessClaude)
	for _, want := range []string{
		"name: test-role",
		"description: a test role",
		"model: claude-sonnet-5",
		"tools: Read, Grep",
		"Body text.",
		GenMarker,
		"capability class `build-implement`",
		"quality tier `balanced`",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("Render output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "INDEPENDENCE") {
		t.Fatal("Render should not include the independence line when Independence is false")
	}
}

func TestRenderModelDefaultsToInherit(t *testing.T) {
	role := Role{ID: "r", Family: "lane", Description: "d", CapabilityClass: "c", QualityTier: "t", Body: "body"}
	out := Render(role, HarnessCodex)
	if !strings.Contains(out, "model: inherit") {
		t.Fatalf("Render should default model to inherit when unset:\n%s", out)
	}
	if strings.Contains(out, "tools:") {
		t.Fatalf("Render should omit the tools line when Tools is empty:\n%s", out)
	}
}

func TestRenderIndependenceLine(t *testing.T) {
	role := Role{ID: "r", Family: "lane", Description: "d", CapabilityClass: "c", QualityTier: "t", Body: "body", Independence: true}
	out := Render(role, HarnessCodex)
	if !strings.Contains(out, "INDEPENDENCE") {
		t.Fatalf("Render should include the independence line when Independence is true:\n%s", out)
	}
}

// --- Sync ---

func newSyncTargets(t *testing.T) (Targets, string) {
	t.Helper()
	home := t.TempDir()
	return TargetsForHome(home), home
}

func lonelyRole(id string, emit ...Harness) Role {
	return Role{
		ID:              id,
		Family:          "lane",
		Description:     "d",
		CapabilityClass: "build-implement",
		QualityTier:     "balanced",
		Emit:            emit,
		Body:            "body",
	}
}

func TestSyncWritesFilesOnlyForEmittingHarnesses(t *testing.T) {
	targets, _ := newSyncTargets(t)
	roles := []Role{lonelyRole("codex-only", HarnessCodex)}

	report, err := Sync(roles, targets, SyncOptions{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if report.Drift != 1 {
		t.Fatalf("Drift = %d, want 1 (one file written)", report.Drift)
	}
	if _, err := os.Stat(filepath.Join(targets[HarnessCodex], "codex-only.md")); err != nil {
		t.Fatalf("expected codex file written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(targets[HarnessClaude], "codex-only.md")); !os.IsNotExist(err) {
		t.Fatalf("expected no claude file for a codex-only role, stat err = %v", err)
	}
}

func TestSyncIsIdempotentOnSecondRun(t *testing.T) {
	targets, _ := newSyncTargets(t)
	roles := []Role{lonelyRole("stable", bothHarnesses...)}

	if _, err := Sync(roles, targets, SyncOptions{}); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	report, err := Sync(roles, targets, SyncOptions{})
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if report.Drift != 0 {
		t.Fatalf("Drift = %d on an up-to-date tree, want 0", report.Drift)
	}
	found := false
	for _, line := range report.Summary {
		if strings.Contains(line, "ok claude/stable") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an 'ok' summary line for the unchanged role, got %v", report.Summary)
	}
}

func TestSyncSkipsHandWrittenFileWithoutMarker(t *testing.T) {
	targets, _ := newSyncTargets(t)
	roles := []Role{lonelyRole("owner-written", HarnessClaude)}
	if err := os.MkdirAll(targets[HarnessClaude], 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(targets[HarnessClaude], "owner-written.md")
	handWritten := "---\nname: owner-written\n---\nhand written, no marker\n"
	if err := os.WriteFile(path, []byte(handWritten), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Sync(roles, targets, SyncOptions{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if report.Drift != 0 {
		t.Fatalf("Drift = %d, want 0 (hand-written file must not count as drift)", report.Drift)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != handWritten {
		t.Fatal("Sync overwrote a hand-written file that had no generated marker")
	}
	skipped := false
	for _, line := range report.Summary {
		if strings.Contains(line, "SKIP") && strings.Contains(line, "hand-written") {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("expected a SKIP summary line, got %v", report.Summary)
	}
}

func TestSyncCheckModeReportsDriftWithoutWriting(t *testing.T) {
	targets, _ := newSyncTargets(t)
	roles := []Role{lonelyRole("new-role", HarnessClaude)}

	report, err := Sync(roles, targets, SyncOptions{Check: true})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if report.Drift != 1 {
		t.Fatalf("Drift = %d, want 1", report.Drift)
	}
	if !report.Check {
		t.Fatal("report.Check should mirror opts.Check")
	}
	if _, err := os.Stat(filepath.Join(targets[HarnessClaude], "new-role.md")); !os.IsNotExist(err) {
		t.Fatalf("check mode must not write files, stat err = %v", err)
	}
}

func TestSyncCheckInstalledOnlySkipsUninstalledRoles(t *testing.T) {
	targets, _ := newSyncTargets(t)
	roles := []Role{lonelyRole("not-installed", HarnessClaude)}

	report, err := Sync(roles, targets, SyncOptions{Check: true, InstalledOnly: true})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if report.Drift != 0 {
		t.Fatalf("Drift = %d, want 0 for a skipped not-installed role", report.Drift)
	}
	found := false
	for _, line := range report.Summary {
		if strings.Contains(line, "skip") && strings.Contains(line, "not installed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a 'not installed' skip summary line, got %v", report.Summary)
	}
}

func TestSyncPrunesStaleGeneratedFile(t *testing.T) {
	targets, _ := newSyncTargets(t)
	staleRole := []Role{lonelyRole("retiring", HarnessClaude)}
	if _, err := Sync(staleRole, targets, SyncOptions{}); err != nil {
		t.Fatalf("seed stale generated file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(targets[HarnessClaude], "retiring.md")); err != nil {
		t.Fatalf("expected seeded file: %v", err)
	}

	// Now sync with a role set that no longer includes "retiring".
	report, err := Sync([]Role{lonelyRole("current", HarnessClaude)}, targets, SyncOptions{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(targets[HarnessClaude], "retiring.md")); !os.IsNotExist(err) {
		t.Fatalf("expected stale generated file pruned, stat err = %v", err)
	}
	pruned := false
	for _, line := range report.Summary {
		if strings.Contains(line, "prune claude/retiring") {
			pruned = true
		}
	}
	if !pruned {
		t.Fatalf("expected a prune summary line, got %v", report.Summary)
	}
}

func TestSyncCheckModeReportsStaleWithoutPruning(t *testing.T) {
	targets, _ := newSyncTargets(t)
	if _, err := Sync([]Role{lonelyRole("retiring", HarnessClaude)}, targets, SyncOptions{}); err != nil {
		t.Fatalf("seed stale generated file: %v", err)
	}

	report, err := Sync([]Role{lonelyRole("current", HarnessClaude)}, targets, SyncOptions{Check: true})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(targets[HarnessClaude], "retiring.md")); err != nil {
		t.Fatalf("check mode must not prune, stat err = %v", err)
	}
	stale := false
	for _, line := range report.Summary {
		if strings.Contains(line, "STALE") {
			stale = true
		}
	}
	if !stale {
		t.Fatalf("expected a STALE summary line, got %v", report.Summary)
	}
}

func TestSyncMissingTargetDirErrors(t *testing.T) {
	targets := Targets{HarnessClaude: "", HarnessCodex: ""}
	if _, err := Sync([]Role{lonelyRole("x", HarnessClaude)}, targets, SyncOptions{}); err == nil {
		t.Fatal("expected an error for a missing target dir")
	}
}

// --- FinalLine ---

func TestFinalLine(t *testing.T) {
	tests := []struct {
		name   string
		report SyncReport
		want   string
	}{
		{"check in sync", SyncReport{Check: true, Drift: 0}, "IN SYNC — no drift"},
		{"check with drift", SyncReport{Check: true, Drift: 3}, "DRIFT: 3 file(s) out of sync. Run `compound profiles generate`."},
		{"write mode", SyncReport{Check: false, RoleCount: 5}, "Generated 5 roles → ~/.claude/agents + ~/.codex/agents."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FinalLine(tc.report); got != tc.want {
				t.Fatalf("FinalLine(%+v) = %q, want %q", tc.report, got, tc.want)
			}
		})
	}
}

// --- ApplyOverlay ---

func baseRolesFixture() []Role {
	return []Role{
		{ID: "alpha", Family: "lane", Description: "alpha desc", CapabilityClass: "build-implement", QualityTier: "balanced", Emit: bothHarnesses, Body: "alpha body"},
		{ID: "beta", Family: "persona", Description: "beta desc", CapabilityClass: "plan-decompose", QualityTier: "needs-best", Emit: bothHarnesses, Body: "beta body"},
	}
}

func TestApplyOverlayPatchesOnlyNamedFields(t *testing.T) {
	base := baseRolesFixture()
	newDesc := "alpha updated"
	overlay := Overlay{Roles: []RoleOverlay{{ID: "alpha", Description: &newDesc}}}

	out, err := ApplyOverlay(base, overlay)
	if err != nil {
		t.Fatalf("ApplyOverlay: %v", err)
	}
	var alpha Role
	for _, r := range out {
		if r.ID == "alpha" {
			alpha = r
		}
	}
	if alpha.Description != newDesc {
		t.Fatalf("Description = %q, want %q", alpha.Description, newDesc)
	}
	if alpha.Family != "lane" || alpha.CapabilityClass != "build-implement" || alpha.Body != "alpha body" {
		t.Fatalf("unpatched fields drifted: %+v", alpha)
	}
}

func TestApplyOverlayDoesNotMutateBase(t *testing.T) {
	base := baseRolesFixture()
	newDesc := "mutated"
	overlay := Overlay{Roles: []RoleOverlay{{ID: "alpha", Description: &newDesc}}}
	if _, err := ApplyOverlay(base, overlay); err != nil {
		t.Fatalf("ApplyOverlay: %v", err)
	}
	if base[0].Description != "alpha desc" {
		t.Fatalf("ApplyOverlay mutated its base slice: %+v", base[0])
	}
}

func TestApplyOverlayAddsNewValidRole(t *testing.T) {
	base := baseRolesFixture()
	family := "lane"
	desc := "gamma desc"
	class := "certify"
	tier := "balanced"
	body := "gamma body"
	overlay := Overlay{Roles: []RoleOverlay{{
		ID: "gamma", Family: &family, Description: &desc, CapabilityClass: &class, QualityTier: &tier,
		Emit: []Harness{HarnessCodex}, Body: &body,
	}}}

	out, err := ApplyOverlay(base, overlay)
	if err != nil {
		t.Fatalf("ApplyOverlay: %v", err)
	}
	if len(out) != len(base)+1 {
		t.Fatalf("len(out) = %d, want %d", len(out), len(base)+1)
	}
	if !emits(out[len(out)-1], HarnessCodex) {
		t.Fatalf("new role should emit codex: %+v", out[len(out)-1])
	}
}

func TestApplyOverlayNewRoleMissingRequiredFieldErrors(t *testing.T) {
	base := baseRolesFixture()
	desc := "incomplete"
	overlay := Overlay{Roles: []RoleOverlay{{ID: "incomplete", Description: &desc}}} // no Family/CapabilityClass/QualityTier/Emit/Body
	if _, err := ApplyOverlay(base, overlay); err == nil {
		t.Fatal("expected an error for a new role missing required fields")
	}
}

func TestApplyOverlayPatchingExistingRoleInvalidErrors(t *testing.T) {
	base := baseRolesFixture()
	blank := ""
	overlay := Overlay{Roles: []RoleOverlay{{ID: "alpha", Family: &blank}}}
	if _, err := ApplyOverlay(base, overlay); err == nil {
		t.Fatal("expected an error when a patch clears a required field on an existing role")
	} else if !strings.Contains(err.Error(), "alpha") {
		t.Fatalf("error %q should name the offending role", err.Error())
	}
}

func TestApplyOverlayMissingIDErrors(t *testing.T) {
	base := baseRolesFixture()
	desc := "no id"
	overlay := Overlay{Roles: []RoleOverlay{{Description: &desc}}}
	if _, err := ApplyOverlay(base, overlay); err == nil {
		t.Fatal("expected an error for an overlay role patch with no id")
	}
}

func TestApplyOverlayModelsMapFullyReplaces(t *testing.T) {
	base := baseRolesFixture()
	base[0].Models = map[Harness]string{HarnessClaude: "old-model", HarnessCodex: "old-codex-model"}
	overlay := Overlay{Roles: []RoleOverlay{{ID: "alpha", Models: map[Harness]string{HarnessClaude: "new-model"}}}}

	out, err := ApplyOverlay(base, overlay)
	if err != nil {
		t.Fatalf("ApplyOverlay: %v", err)
	}
	var alpha Role
	for _, r := range out {
		if r.ID == "alpha" {
			alpha = r
		}
	}
	if len(alpha.Models) != 1 || alpha.Models[HarnessClaude] != "new-model" {
		t.Fatalf("Models = %+v, want only claude:new-model (full replace, not merge)", alpha.Models)
	}
}

// --- LoadCatalogWithOptions ---

func TestLoadCatalogWithOptionsEmptyOverlayPathsDisablesOverlay(t *testing.T) {
	catalog, err := LoadCatalogWithOptions(LoadOptions{OverlayPaths: []string{}})
	if err != nil {
		t.Fatalf("LoadCatalogWithOptions: %v", err)
	}
	if catalog.OverlayLoaded {
		t.Fatal("OverlayLoaded should be false when OverlayPaths is explicitly empty")
	}
	if catalog.OverlayPath != "" {
		t.Fatalf("OverlayPath = %q, want empty", catalog.OverlayPath)
	}
	if len(catalog.Roles) != len(DefaultRoles()) {
		t.Fatalf("len(Roles) = %d, want %d (unmodified defaults)", len(catalog.Roles), len(DefaultRoles()))
	}
}

func TestLoadCatalogWithOptionsAppliesFirstExistingOverlay(t *testing.T) {
	dir := t.TempDir()
	validPath := filepath.Join(dir, "overlay.json")
	family := "lane"
	desc := "patched via overlay"
	class := "certify"
	tier := "balanced"
	body := "patched body"
	overlay := Overlay{Roles: []RoleOverlay{{
		ID: "overlay-role", Family: &family, Description: &desc, CapabilityClass: &class, QualityTier: &tier,
		Emit: []Harness{HarnessClaude}, Body: &body,
	}}}
	data, err := json.Marshal(overlay)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(validPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	missingPath := filepath.Join(dir, "does-not-exist.json")

	catalog, err := LoadCatalogWithOptions(LoadOptions{OverlayPaths: []string{missingPath, validPath}})
	if err != nil {
		t.Fatalf("LoadCatalogWithOptions: %v", err)
	}
	if !catalog.OverlayLoaded || catalog.OverlayPath != validPath {
		t.Fatalf("expected overlay loaded from %q, got loaded=%v path=%q", validPath, catalog.OverlayLoaded, catalog.OverlayPath)
	}
	found := false
	for _, r := range catalog.Roles {
		if r.ID == "overlay-role" && r.Description == desc {
			found = true
		}
	}
	if !found {
		t.Fatalf("overlay role not applied: %+v", catalog.Roles)
	}
}

func TestLoadCatalogWithOptionsMalformedJSONErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCatalogWithOptions(LoadOptions{OverlayPaths: []string{path}}); err == nil {
		t.Fatal("expected an error for malformed overlay JSON")
	}
}

func TestLoadCatalogWithOptionsInvalidOverlayContentErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overlay.json")
	// Missing id: structurally valid JSON, semantically invalid overlay.
	if err := os.WriteFile(path, []byte(`{"roles":[{"description":"no id"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadCatalogWithOptions(LoadOptions{OverlayPaths: []string{path}})
	if err == nil {
		t.Fatal("expected an error for an overlay with a role patch missing id")
	}
	if !strings.Contains(err.Error(), "apply profile overlay") {
		t.Fatalf("error %q should name the overlay-apply step", err.Error())
	}
}

// --- DefaultOverlayPaths ---

func TestDefaultOverlayPathsExplicitEnvWins(t *testing.T) {
	t.Setenv("COMPOUND_PROFILES_OVERLAY", "/explicit/overlay.json")
	t.Setenv("COMPOUND_HOME", "/some/home")
	got := DefaultOverlayPaths()
	if len(got) != 1 || got[0] != "/explicit/overlay.json" {
		t.Fatalf("DefaultOverlayPaths() = %v, want exactly [/explicit/overlay.json]", got)
	}
}

func TestDefaultOverlayPathsCompoundHomeFallback(t *testing.T) {
	t.Setenv("COMPOUND_PROFILES_OVERLAY", "")
	t.Setenv("COMPOUND_HOME", "/some/home")
	isolatedCwd(t)

	got := DefaultOverlayPaths()
	want := filepath.Join("/some/home", "profiles", "overlay.json")
	if len(got) != 1 || got[0] != want {
		t.Fatalf("DefaultOverlayPaths() = %v, want exactly [%s]", got, want)
	}
}

func TestDefaultOverlayPathsEmptyWhenNothingConfigured(t *testing.T) {
	t.Setenv("COMPOUND_PROFILES_OVERLAY", "")
	t.Setenv("COMPOUND_HOME", "")
	isolatedCwd(t)

	if got := DefaultOverlayPaths(); len(got) != 0 {
		t.Fatalf("DefaultOverlayPaths() = %v, want empty with no env and no repo overlay", got)
	}
}

func TestDefaultOverlayPathsFindsRepoOverlayViaCwd(t *testing.T) {
	t.Setenv("COMPOUND_PROFILES_OVERLAY", "")
	t.Setenv("COMPOUND_HOME", "")

	root := t.TempDir()
	overlayPath := filepath.Join(root, "docs_private", "profiles", "overlay.json")
	if err := os.MkdirAll(filepath.Dir(overlayPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overlayPath, []byte(`{"roles":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)

	got := DefaultOverlayPaths()
	if len(got) != 1 || got[0] != overlayPath {
		t.Fatalf("DefaultOverlayPaths() = %v, want exactly [%s]", got, overlayPath)
	}
}

func TestLoadCatalogReturnsDefaultsWithNoOverlayConfigured(t *testing.T) {
	t.Setenv("COMPOUND_PROFILES_OVERLAY", "")
	t.Setenv("COMPOUND_HOME", "")
	isolatedCwd(t)

	catalog, err := LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	if catalog.OverlayLoaded {
		t.Fatal("OverlayLoaded should be false with no overlay configured")
	}
	if len(catalog.Roles) != len(DefaultRoles()) {
		t.Fatalf("len(Roles) = %d, want %d", len(catalog.Roles), len(DefaultRoles()))
	}
}

// --- validateRole ---

func TestValidateRoleFieldRequirements(t *testing.T) {
	validRole := func() Role {
		return Role{ID: "r", Family: "lane", Description: "d", CapabilityClass: "c", QualityTier: "t", Emit: []Harness{HarnessClaude}, Body: "b"}
	}
	if err := validateRole(validRole()); err != nil {
		t.Fatalf("valid role should validate: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*Role)
		wantErr string
	}{
		{"missing id", func(r *Role) { r.ID = "" }, "missing id"},
		{"missing family", func(r *Role) { r.Family = "" }, "missing family"},
		{"missing description", func(r *Role) { r.Description = "" }, "missing description"},
		{"missing capability class", func(r *Role) { r.CapabilityClass = "" }, "missing capability_class"},
		{"missing quality tier", func(r *Role) { r.QualityTier = "" }, "missing quality_tier"},
		{"no emit targets", func(r *Role) { r.Emit = nil }, "emits no harnesses"},
		{"unknown harness", func(r *Role) { r.Emit = []Harness{Harness("gemini")} }, "unknown harness"},
		{"missing body", func(r *Role) { r.Body = "" }, "missing body"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			role := validRole()
			tc.mutate(&role)
			err := validateRole(role)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// isolatedCwd chdirs the test into a fresh, empty temp directory tree so
// findRepoOverlay's upward walk cannot pick up an ambient docs_private/
// overlay from whatever repo the test binary happens to run inside.
func isolatedCwd(t *testing.T) {
	t.Helper()
	nested := filepath.Join(t.TempDir(), "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)
}
