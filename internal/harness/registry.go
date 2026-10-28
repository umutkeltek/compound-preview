package harness

import (
	"context"
	"fmt"
	"sync"

	"compound/internal/runattempt"
)

// AdapterRegistry is the runtime lookup table for harness adapters. The
// Registry type (Harness -> Capabilities) in harness.go and this
// AdapterRegistry (Name -> Adapter) are deliberately separate:
// capabilities are a static manifest, executors are a runtime
// construct. A future slice joins them -- "given the work envelope,
// which adapter drives it?" -- but today they live side by side.
//
// NewAdapterRegistry returns a registry pre-populated with the built-in
// adapters (os-entrypoints, claude, codex). Callers may RegisterAdapter
// to add custom adapters (e.g. a test harness) or override the
// built-ins (e.g. a wrapper that adds structured logging).
type AdapterRegistry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

// NewAdapterRegistry returns an AdapterRegistry with the built-in
// adapters pre-registered. The built-ins share a single runattempt.Store;
// callers that need per-adapter stores can construct adapters manually
// and RegisterAdapter them.
func NewAdapterRegistry(store *runattempt.Store) *AdapterRegistry {
	r := &AdapterRegistry{adapters: make(map[string]Adapter)}
	r.RegisterAdapter(NewOSEntrypointsAdapter(store))
	r.RegisterAdapter(NewClaudeAdapter(store))
	for _, name := range []string{
		"claude_code",
		"claude_minimax",
		"claude_qwen",
		"claude_zai",
		"claude_zai_full",
		"claude_neuralwatt",
		"claude_gpt56_sol_wpcom",
		"claude_gpt56_sol_wpcom_headroom",
		"claude_gpt",
		"claude_kimi",
		"claude_deepseek_flash_api",
		"claude_deepseek_flash_nvidia",
	} {
		r.RegisterAdapter(newClaudeLaneAdapter(store, name, defaultHeartbeatInterval))
	}
	r.RegisterAdapter(NewCodexAdapter(store))
	r.RegisterAdapter(NewOpenCodeAdapter(store))
	// The pi coding-agent family. Both forks share one adapter implementation
	// (pi_coding_agent_adapter.go); registering each by its own name keeps the
	// AdapterRegistry key, the DefaultRegistry row and RunAttempt.Harness
	// spelled the same way, which is what makes a routing miss loud instead of
	// a silent fall-through to the generic stand-in.
	//
	// qwen_code and deepseek_harness are deliberately NOT here. They have
	// DefaultRegistry identity rows and no executor: qwen-code is a different
	// lineage with no --cwd, and deepseek-harness's headless surface is one
	// string in and one string out with nothing for an adapter to hold onto.
	// A registry row without an adapter is an honest "known but not dispatchable";
	// an adapter without a proven contract is a routing hole.
	r.RegisterAdapter(NewPrimeAgentAdapter(store))
	r.RegisterAdapter(NewOhMyPiAdapter(store))
	r.RegisterAdapter(NewOMPNativeVerifierAdapter(store))
	r.RegisterAdapter(NewOMPPromptOnlyAdapter(store))
	return r
}

// ReadOnlyLaunchReadiness reports adapter-owned launch prerequisites without
// minting attempts or recording refusals. Projections use the same read-only
// revalidation as the final governed process-start guard, not mutating Preflight.
// This snapshot is evidence, not a reservation: execution must still revalidate.
func ReadOnlyLaunchReadiness(home string, expected RunInput) []string {
	adapter, ok := NewAdapterRegistry(runattempt.Open(home)).AdapterByName(expected.Harness)
	if !ok {
		return []string{"adapter_preflight: no adapter for " + expected.Harness}
	}
	if guard, ok := adapter.(GovernedPermissionAdapter); ok {
		resolved, err := guard.RevalidateGovernedRun(context.Background(), expected)
		if err != nil {
			return []string{"adapter_preflight: " + err.Error()}
		}
		// A capacity check on tuple B cannot attest the request selected from A.
		// Raw Claude has no tuple and deliberately normalizes its command instead.
		if lane, ok := adapter.(*ClaudeAdapter); ok && lane.tupleBacked {
			if canonicalHarnessName(resolved.Harness) != canonicalHarnessName(expected.Harness) ||
				resolved.Command != expected.Command || resolved.LaneID != expected.LaneID ||
				resolved.RequestedModelID != expected.RequestedModelID ||
				resolved.RequestedProviderRoute != expected.RequestedProviderRoute {
				return []string{"adapter_preflight: lane tuple changed during readiness evaluation"}
			}
		}
	}
	return nil
}

// RegisterAdapter adds (or replaces) an adapter under its Name(). Returns
// an error if the adapter is nil or has an empty Name.
func (r *AdapterRegistry) RegisterAdapter(a Adapter) error {
	if a == nil {
		return fmt.Errorf("adapter registry: cannot register nil adapter")
	}
	if a.Name() == "" {
		return fmt.Errorf("adapter registry: adapter has empty Name()")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[a.Name()] = a
	return nil
}

// AdapterByName returns the adapter registered under name, plus a bool
// indicating whether the lookup succeeded. Read-only; safe for
// concurrent callers.
func (r *AdapterRegistry) AdapterByName(name string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[name]
	if ok {
		return a, true
	}
	if canonical := canonicalHarnessName(name); canonical != name {
		a, ok = r.adapters[canonical]
	}
	return a, ok
}

// Names returns the sorted list of registered adapter names. Useful for
// `compound harness list` and for the architecture doctor checks
// (every harness in the manifest must have an executor).
func (r *AdapterRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.adapters))
	for n := range r.adapters {
		out = append(out, n)
	}
	// Sort for deterministic output (operator-visible list, doctor
	// check, test assertions).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
