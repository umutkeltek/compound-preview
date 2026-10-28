package contract

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenCodeHookRegistrationProducesOnlyNonBlockingBindings(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("HOME", homePath)
	source := filepath.Join("..", "..", "adapters", "opencode", "hook-registration.ts")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := parseHookRegistrationTS(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := StageSyntheticHookHandlers(homePath, source); err != nil {
		t.Fatal(err)
	}
	attestInstalledHookHandlers(&manifest)
	plan, err := hookPlanFromInstalledManifest(manifest, homePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Bindings) != 3 {
		t.Fatalf("opencode plan bindings = %d, want 3", len(plan.Bindings))
	}
	for _, binding := range plan.Bindings {
		if binding.Harness != "opencode" {
			t.Fatalf("binding harness = %q, want opencode", binding.Harness)
		}
		if binding.Blocking {
			t.Fatalf("opencode binding %s unexpectedly marked blocking", binding.Event)
		}
		if binding.Event == "PreToolUse" {
			t.Fatal("opencode plan must not register PreToolUse")
		}
	}

	readiness := InstalledHookGateReadiness(manifest, map[string]bool{"opencode": false})
	if len(readiness) != 1 {
		t.Fatalf("opencode pre-tool readiness rows = %d, want 1", len(readiness))
	}
	row := readiness[0]
	if row.Status != "non_blocking" || row.Installed || row.NativeBlock {
		t.Fatalf("opencode pre-tool readiness = %+v, want explicit non-blocking lane", row)
	}
	if row.AdapterRequired || row.AdapterCommand != "" || len(row.Actions) != 0 {
		t.Fatalf("opencode pre-tool readiness recommends an enforcement adapter: %+v", row)
	}
}
