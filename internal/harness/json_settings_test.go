package harness

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

func decodeSettings(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The whole safety argument for sharing settings.json with the harness itself:
// keys Compound does not declare are never read, written, or removed.
func TestJSONSettingsLeavesUnownedKeysUntouched(t *testing.T) {
	settings := decodeSettings(t, `{
		"model": "harness-chosen",
		"statusLine": {"type": "command", "command": "mine"},
		"recentDirs": ["a", "b"]
	}`)
	owned := map[string]any{"env.COMPOUND_LANE": "primary"}

	if _, err := applyJSONSettingsPaths(settings, owned, nil); err != nil {
		t.Fatal(err)
	}
	if settings["model"] != "harness-chosen" {
		t.Fatalf("unowned model was modified: %v", settings["model"])
	}
	status, _ := settings["statusLine"].(map[string]any)
	if status["command"] != "mine" {
		t.Fatalf("unowned statusLine was modified: %v", status)
	}
	if got := settings["recentDirs"]; !reflect.DeepEqual(got, []any{"a", "b"}) {
		t.Fatalf("unowned recentDirs was modified: %v", got)
	}
	env, _ := settings["env"].(map[string]any)
	if env["COMPOUND_LANE"] != "primary" {
		t.Fatalf("owned path not written: %v", settings["env"])
	}
}

// A path Compound owned and no longer declares must be removed. Abandoning it
// leaves a Compound-authored value that nothing claims, which is how an
// authority silently accumulates orphans.
func TestJSONSettingsRemovesRelinquishedPaths(t *testing.T) {
	settings := decodeSettings(t, `{"env": {"OLD": "1", "KEEP": "2"}, "model": "x"}`)
	previous := map[string][]string{
		jsonManagedSettingPrefix + "env.OLD": {`"1"`},
		"permissions.allow":                  {"Bash"},
	}
	owned := map[string]any{"model": "y"}

	if _, err := applyJSONSettingsPaths(settings, owned, previous); err != nil {
		t.Fatal(err)
	}
	env, _ := settings["env"].(map[string]any)
	if _, still := env["OLD"]; still {
		t.Fatal("relinquished path env.OLD was not removed")
	}
	if env["KEEP"] != "2" {
		t.Fatalf("unowned sibling env.KEEP was disturbed: %v", env)
	}
	if settings["model"] != "y" {
		t.Fatalf("owned model not reconciled: %v", settings["model"])
	}
}

// Removing the last owned key under a parent must not strand an empty object,
// which would read as deliberate configuration on the next load.
func TestJSONSettingsPrunesEmptiedParents(t *testing.T) {
	settings := decodeSettings(t, `{"env": {"ONLY": "1"}, "keep": true}`)
	previous := map[string][]string{jsonManagedSettingPrefix + "env.ONLY": {`"1"`}}

	if _, err := applyJSONSettingsPaths(settings, map[string]any{}, previous); err != nil {
		t.Fatal(err)
	}
	if _, present := settings["env"]; present {
		t.Fatalf("emptied parent survived: %v", settings)
	}
	if settings["keep"] != true {
		t.Fatal("sibling key was pruned")
	}
}

func TestJSONSettingsCreatesNestedPaths(t *testing.T) {
	settings := map[string]any{}
	owned := map[string]any{"a.b.c": "deep", "top": false}

	managed, err := applyJSONSettingsPaths(settings, owned, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := settings["a"].(map[string]any)
	b, _ := a["b"].(map[string]any)
	if b["c"] != "deep" {
		t.Fatalf("nested path not created: %v", settings)
	}
	if settings["top"] != false {
		t.Fatalf("scalar false not written: %v", settings["top"])
	}
	if got := managed[jsonManagedSettingPrefix+"a.b.c"]; !reflect.DeepEqual(got, []string{`"deep"`}) {
		t.Fatalf("managed entry not recorded: %v", got)
	}
}

// The dotted path is the nesting mechanism; a structured value would express
// nesting twice and make relinquish-removal ambiguous.
func TestJSONSettingsRejectsStructuredValues(t *testing.T) {
	for name, value := range map[string]any{
		"map":   map[string]any{"k": "v"},
		"slice": []any{"a"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateJSONSettingsPaths(map[string]any{"env": value}); err == nil {
				t.Fatal("expected structured value to be rejected")
			}
		})
	}
	if err := ValidateJSONSettingsPaths(map[string]any{"a..b": "x"}); err == nil {
		t.Fatal("expected empty path segment to be rejected")
	}
	if err := ValidateJSONSettingsPaths(map[string]any{"ok.path": "x", "n": 3, "b": true}); err != nil {
		t.Fatalf("valid scalars rejected: %v", err)
	}
}

func TestJSONSettingsDriftNamesTheKey(t *testing.T) {
	owned := map[string]any{"model": "want", "env.LANE": "primary"}

	clean := decodeSettings(t, `{"model": "want", "env": {"LANE": "primary"}}`)
	if details := jsonSettingsDrift(clean, owned); len(details) != 0 {
		t.Fatalf("clean settings reported drift: %v", details)
	}

	changed := decodeSettings(t, `{"model": "other", "env": {"LANE": "primary"}}`)
	details := jsonSettingsDrift(changed, owned)
	if len(details) != 1 {
		t.Fatalf("expected exactly one drift detail, got %v", details)
	}
	if got := details[0]; got != `settings.model is "other", want "want"` {
		t.Fatalf("drift detail does not name key and values: %q", got)
	}

	missing := decodeSettings(t, `{"env": {"LANE": "primary"}}`)
	if details := jsonSettingsDrift(missing, owned); len(details) != 1 {
		t.Fatalf("missing owned key not reported: %v", details)
	}
}

// End-to-end through the real render path: a JSON-target authority that
// declares owned settings paths must reconcile them into settings.json while
// leaving the harness's own keys alone, and must record them as managed so a
// later relinquish can find them.
func TestRenderPermissionsAppliesOwnedJSONSettingsPaths(t *testing.T) {
	homePath, targetPath := t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeTestJSON(t, targetPath, map[string]any{
		"theme":      "dark",
		"recentDirs": []string{"/a"},
		"env":        map[string]any{"HARNESS_OWNED": "keep"},
	})
	policy := permissionFixture([]string{"Bash"})
	policy.Settings = map[string]any{
		"model":             "compound-pinned",
		"env.COMPOUND_LANE": "primary",
	}
	writePermissionFixture(t, homePath, targetPath, policy)

	render, err := RenderPermissions(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	got := decodeTestJSON(t, render.Content)
	if got["model"] != "compound-pinned" {
		t.Fatalf("owned scalar path not applied: %+v", got["model"])
	}
	env, _ := got["env"].(map[string]any)
	if env["COMPOUND_LANE"] != "primary" {
		t.Fatalf("owned nested path not applied: %+v", env)
	}
	if env["HARNESS_OWNED"] != "keep" {
		t.Fatalf("harness-owned sibling key was disturbed: %+v", env)
	}
	if got["theme"] != "dark" {
		t.Fatalf("unrelated setting changed: %+v", got)
	}
	if _, recorded := render.Artifact.ManagedEntries[jsonManagedSettingPrefix+"model"]; !recorded {
		t.Fatalf("owned path not recorded as managed: %+v", render.Artifact.ManagedEntries)
	}
	if _, kept := render.Artifact.ManagedEntries["permissions.allow"]; !kept {
		t.Fatalf("existing managed entries were dropped: %+v", render.Artifact.ManagedEntries)
	}
}
