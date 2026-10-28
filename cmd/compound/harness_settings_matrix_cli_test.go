package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"compound/internal/harness"
)

func TestHarnessSettingsMatrixLiveRegistryAndOwnedKeys(t *testing.T) {
	if os.Getenv("COMPOUND_MATRIX_TEST_CHILD") == "1" {
		cmdHarness([]string{"settings-matrix", "--json"})
		os.Exit(0)
	}
	h := t.TempDir()
	path := harness.PermissionAuthorityPath(h, "codex")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(target, []byte("model = \"gpt-example\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	authority := harness.PermissionAuthority{
		SchemaVersion: harness.PermissionAuthoritySchemaVersion, Harness: "codex", AuthorityRef: harness.PermissionAuthorityRef("codex"), TargetPath: target,
		AutoMode: harness.AutoModePolicy{Allow: []string{"$defaults"}}, Settings: map[string]any{"model": "gpt-example"},
	}
	write := func() {
		t.Helper()
		b, err := json.Marshal(authority)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	probe := func() map[string]map[string]any {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestHarnessSettingsMatrixLiveRegistryAndOwnedKeys$")
		cmd.Env = append(os.Environ(), "COMPOUND_HOME="+h, "COMPOUND_MATRIX_TEST_CHILD=1")
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("live matrix command unavailable: %v %s", err, b)
		}
		var report struct {
			SchemaVersion string           `json:"schema_version"`
			Rows          []map[string]any `json:"rows"`
		}
		if err := json.Unmarshal(b, &report); err != nil {
			t.Fatalf("matrix protocol: %v %s", err, b)
		}
		if report.SchemaVersion != "compound.harness_settings_matrix.v0.1" {
			t.Fatal("missing matrix protocol")
		}
		rows := map[string]map[string]any{}
		for _, row := range report.Rows {
			rows[row["harness"].(string)] = row
		}
		if len(rows) != len(harness.DefaultRegistry().Names()) {
			t.Fatal("matrix omitted registered identities")
		}
		for _, name := range harness.DefaultRegistry().Names() {
			if rows[name] == nil {
				t.Fatalf("missing %s", name)
			}
		}
		return rows
	}
	rows := probe()
	if rows["codex"]["authority_status"] != "owned" || rows["codex"]["native_status"] != "matches" {
		t.Fatalf("wrong owned row: %v", rows["codex"])
	}
	if rows["claude_code"]["authority_status"] != "missing_authority" {
		t.Fatal("ownership gap was hidden")
	}
	if len(rows["codex"]["settings"].([]any)) != 1 {
		t.Fatal("owned settings missing")
	}
	authority.Settings["model_reasoning_effort"] = "xhigh"
	write()
	if err := os.WriteFile(target, []byte("model = \"gpt-example\"\nmodel_reasoning_effort = \"xhigh\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rows = probe()
	if len(rows["codex"]["settings"].([]any)) != 2 {
		t.Fatal("matrix did not refresh authority")
	}
}
