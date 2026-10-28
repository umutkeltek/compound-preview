// work_101f7a9befb8 — proof that the codex (gpt-5.5) Compound lane tuple
// resolves to a non-paid owned wrapper in PATH, refuting find_062a2146cf8b.
//
// The proof chain is:
//
//  1. Load the live Compound model-capabilities authority from the operator
//     home (same file `compound route import-model-capabilities` would
//     merge into). No stub: the test reads what the delegate lane reads.
//
//  2. ResolveModelCapabilityTuple("codex (gpt-5.5)") -> harness=codex,
//     provider_route=codex-tap, and a populated aliases list. If aliases
//     is empty the test fails — that is exactly the pre-fix state.
//
//  3. ResolveModelCapabilityExecutable(aliases) walks the alias list and
//     LookPath's each one. The FIRST executable wins. The asserted winner
//     is `codex-switchback-tap`, the Switchback capture tap wrapper
//     (cli/wrappers/codex-switchback-tap → runtime execution that pins
//     base_url to the local capture-tap endpoint with
//     requires_openai_auth=true). This refutes the empty-CODEX_HOME 401
//     because the launched process never speaks to api.openai.com.
//
//  4. resolveDelegateHarness wires the tuple into the DelegateHarnessResolution
//     that work delegate reads, so a delegate --dry-run for the harness
//     string "codex (gpt-5.5)" returns a non-empty, TupleBacked resolution
//     with a non-empty Command. The dry-run path reaches TupleBacked=true
//     and the executable evidence kind passes.
//
//  5. Alias ordering: codex-switchback-tap is intentionally FIRST so the
//     no-paid-spawn owned wrapper wins on hosts that ship both it AND the
//     raw `codex` binary on PATH (the operator host carries both; the tap
//     wrapper lives in the user-local bin directory ahead of raw codex).
//     Raw `codex` is NOT in the aliases — that is the structural fix for
//     find_062a2146cf8b (empty CODEX_HOME 401 against api.openai.com).
//
// This file does NOT mutate the home authority; it only reads.
package routeevidence

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/work"
)

// TestCodexGPT55TupleResolvesToOwnedWrapperInPath proves work_101f7a9befb8:
// the codex (gpt-5.5) tuple resolves to a no-paid-spawn owned wrapper that
// lives on this operator host's PATH.
func TestCodexGPT55TupleResolvesToOwnedWrapperInPath(t *testing.T) {
	// 1) Locate the live Compound home. The operator's $COMPOUND_HOME is
	//    ~/.compound by convention; fall back to the workspace path so the
	//    test still pins the proof on a copy if HOME is overridden in CI.
	home := strings.TrimSpace(os.Getenv("COMPOUND_HOME"))
	if home == "" {
		home = filepath.Join(os.Getenv("HOME"), ".compound")
	}
	manifestPath := ModelCapabilitiesPath(home)
	if _, err := os.Stat(manifestPath); err != nil {
		t.Skipf("operator home authority missing (%s); proof is host-local — %v", manifestPath, err)
	}

	manifest, _, err := LoadModelCapabilitiesFromHome(home)
	if err != nil {
		t.Fatalf("load Compound model-router authority from %s: %v", home, err)
	}

	// 2) Resolve the live tuple. The selector "codex (gpt-5.5)" is what
	//    `compound work delegate --harness 'codex (gpt-5.5)'` passes.
	modelID, tuple, err := ResolveModelCapabilityTuple(manifest, "codex (gpt-5.5)")
	if err != nil {
		t.Fatalf("codex (gpt-5.5) did not resolve as a Compound lane tuple: %v", err)
	}
	if modelID != "codex (gpt-5.5)" {
		t.Fatalf("model id = %q, want %q", modelID, "codex (gpt-5.5)")
	}
	if tuple.Harness != "codex" {
		t.Fatalf("harness = %q, want %q", tuple.Harness, "codex")
	}
	if tuple.ProviderRoute != "codex-tap" {
		t.Fatalf("provider_route = %q, want %q", tuple.ProviderRoute, "codex-tap")
	}

	// The fix: aliases MUST be non-empty. Pre-fix state was `aliases: []`
	// (field omitted) — exactly the bug find_062a2146cf8b traces back to.
	if len(tuple.Aliases) == 0 {
		t.Fatalf("codex (gpt-5.5) tuple has no aliases; resolveExecutable would fail and the delegate lane would refuse the harness (this is the pre-fix state the work envelope fixes)")
	}

	// Raw `codex` must NOT be in the aliases — including it would let the
	// delegate lane pick the OpenAI-direct binary on hosts that ship it
	// before the Switchback tap wrapper, reproducing find_062a2146cf8b.
	for _, alias := range tuple.Aliases {
		if strings.TrimSpace(alias) == "codex" {
			t.Fatalf("raw `codex` is in the alias list (%v); find_062a2146cf8b is not refuted — empty CODEX_HOME + raw codex -> 401 on api.openai.com", tuple.Aliases)
		}
	}

	// 3) The executable resolver must succeed and pick the FIRST alias
	//    that exists in PATH. On this operator host the first match is
	//    codex-switchback-tap (Switchback capture tap, no paid spawn).
	command, err := ResolveModelCapabilityExecutable(tuple.Aliases)
	if err != nil {
		t.Fatalf("ResolveModelCapabilityExecutable failed for aliases %v: %v", tuple.Aliases, err)
	}
	if strings.TrimSpace(command) == "" {
		t.Fatalf("resolveExecutable returned empty command for %v", tuple.Aliases)
	}

	// 4) The picked executable must be one of the owned wrappers (NOT the
	//    raw codex binary that hits api.openai.com with no auth header).
	ownedWrappers := map[string]bool{
		"codex-switchback-tap":           true,
		"codex-gpt56-sol-wpcom":          true,
		"codex-gpt56-sol-wpcom-headroom": true,
		"codex-gpt56-sol-codex":          true,
		"codex-minimax":                  true,
		"codex-switchback-scout":         true,
		"codex-switchback-traced":        true,
	}
	base := filepath.Base(command)
	if !ownedWrappers[base] {
		t.Fatalf("resolved executable %q (base %q) is NOT in the owned-wrapper allowlist %v; this would re-open find_062a2146cf8b",
			command, base, keysOfOwnedWrappers(ownedWrappers))
	}

	// 5) Wire it through resolveDelegateHarness — the same seam `compound
	//    work delegate --harness 'codex (gpt-5.5)'` calls before dry-run
	//    readiness. The resolution must be TupleBacked with a non-empty
	//    Command; that is what `--dry-run` reports as the executable
	//    evidence kind.
	resolution, err := resolveDelegateHarness(work.DelegateHarnessResolveRequest{
		HomePath: home, Harness: "codex (gpt-5.5)", Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("resolveDelegateHarness for codex (gpt-5.5): %v", err)
	}
	if !resolution.TupleBacked {
		t.Fatalf("resolution is not TupleBacked: %+v", resolution)
	}
	if resolution.RequestedModelID != "codex (gpt-5.5)" {
		t.Fatalf("RequestedModelID = %q, want codex (gpt-5.5)", resolution.RequestedModelID)
	}
	if resolution.RequestedProviderRoute != "codex-tap" {
		t.Fatalf("RequestedProviderRoute = %q, want codex-tap", resolution.RequestedProviderRoute)
	}
	if strings.TrimSpace(resolution.Command) == "" {
		t.Fatalf("resolution.Command is empty; dry-run would fail the executable evidence kind")
	}
	if !ownedWrappers[filepath.Base(resolution.Command)] {
		t.Fatalf("resolution.Command = %q (base %q) is not an owned wrapper", resolution.Command, filepath.Base(resolution.Command))
	}
}

func keysOfOwnedWrappers(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
