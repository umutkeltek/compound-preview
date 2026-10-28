package harness

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"compound/internal/runattempt"
)

// TestAdapterRegistryNewHasBuiltins pins that the built-in adapters
// are pre-registered. The list is the public surface of the registry;
// the CLI's `compound harness list` and the architecture doctor both
// depend on it.
func TestAdapterRegistryNewHasBuiltins(t *testing.T) {
	r := NewAdapterRegistry(runattempt.Open(t.TempDir()))
	got := r.Names()
	sort.Strings(got)
	// One row per enrolled switchback launch profile plus the non-lane built-ins.
	// This list is the collateral inventory of NewAdapterRegistry: enrolling a lane
	// without adding it here fails loudly, which is the point.
	want := []string{
		"claude",
		"claude_code",
		"claude_deepseek_flash_api",
		"claude_deepseek_flash_nvidia",
		"claude_gpt",
		"claude_gpt56_sol_wpcom",
		"claude_gpt56_sol_wpcom_headroom",
		"claude_kimi",
		"claude_minimax",
		"claude_neuralwatt",
		"claude_qwen",
		"claude_zai",
		"claude_zai_full",
		"codex",
		"oh_my_pi",
		"omp-native-verifier",
		"omp-prompt-only",
		"opencode",
		"os-harness-entrypoints",
		"prime_agent",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NewAdapterRegistry names = %v, want %v", got, want)
	}
}

// TestAdapterRegistryAdapterByName: every built-in is reachable by Name.
func TestAdapterRegistryAdapterByName(t *testing.T) {
	r := NewAdapterRegistry(runattempt.Open(t.TempDir()))
	for _, name := range []string{
		"os-harness-entrypoints",
		"claude",
		"claude_code",
		"claude_minimax",
		"claude_qwen",
		"claude_zai",
		"claude_zai_full",
		"codex",
		"omp-native-verifier",
		"omp-prompt-only",
		"opencode",
	} {
		a, ok := r.AdapterByName(name)
		if !ok {
			t.Fatalf("AdapterByName(%q) returned !ok", name)
		}
		if a.Name() != name {
			t.Fatalf("adapter Name() = %q, want %q", a.Name(), name)
		}
	}
	// Lookup miss returns false + nil adapter.
	if a, ok := r.AdapterByName("nonexistent"); ok || a != nil {
		t.Fatalf("AdapterByName(nonexistent) = (%v, %v), want (nil, false)", a, ok)
	}
}

func TestClaudeFamilyAdaptersAdvertiseGovernedPermissionPosture(t *testing.T) {
	r := NewAdapterRegistry(runattempt.Open(t.TempDir()))
	for _, name := range []string{"claude", "claude_code", "claude_minimax", "claude_qwen", "claude_zai", "claude_zai_full"} {
		adapter, ok := r.AdapterByName(name)
		if !ok {
			t.Fatalf("missing adapter %q", name)
		}
		if _, ok := adapter.(GovernedPermissionAdapter); !ok {
			t.Fatalf("adapter %q does not advertise governed permission posture", name)
		}
	}
	for _, name := range []string{"codex", "os-harness-entrypoints"} {
		adapter, ok := r.AdapterByName(name)
		if !ok {
			t.Fatalf("missing adapter %q", name)
		}
		if _, ok := adapter.(GovernedPermissionAdapter); ok {
			t.Fatalf("adapter %q unexpectedly advertises Claude permission posture", name)
		}
	}
}

// TestAdapterRegistryRegisterOverride: a custom adapter can override a
// built-in. Pinned so future slices that wrap built-ins (e.g. a
// "claude-with-logging" wrapper) have a clear extension point.
func TestAdapterRegistryRegisterOverride(t *testing.T) {
	r := NewAdapterRegistry(runattempt.Open(t.TempDir()))
	stub := stubAdapter{name: "claude"}
	if err := r.RegisterAdapter(stub); err != nil {
		t.Fatalf("RegisterAdapter: %v", err)
	}
	got, ok := r.AdapterByName("claude")
	if !ok {
		t.Fatalf("claude adapter missing after override")
	}
	if got.Name() != "claude" {
		t.Fatalf("claude Name() = %q, want claude", got.Name())
	}
	if _, ok := got.(stubAdapter); !ok {
		t.Fatalf("override did not take effect: got %T, want stubAdapter", got)
	}
}

// TestAdapterRegistryRegisterRejectsNil: defensive contract -- the
// registry refuses to register a nil or nameless adapter.
func TestAdapterRegistryRegisterRejectsNil(t *testing.T) {
	r := NewAdapterRegistry(runattempt.Open(t.TempDir()))
	if err := r.RegisterAdapter(nil); err == nil {
		t.Fatalf("RegisterAdapter(nil) must fail")
	}
	if err := r.RegisterAdapter(stubAdapter{name: ""}); err == nil {
		t.Fatalf("RegisterAdapter(name=\"\") must fail")
	}
}

// stubAdapter is a no-op Adapter used to test RegisterOverride. It
// exists only in tests.
type stubAdapter struct{ name string }

func (s stubAdapter) Name() string { return s.name }
func (s stubAdapter) Run(_ context.Context, _ RunInput) (RunOutput, error) {
	return RunOutput{}, nil
}
