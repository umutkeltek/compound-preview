package capgraph

import (
	"testing"
	"time"
)

// TestCodexFirstManifestIsClaudeCodeOnly guards the tracked
// fixtures/skills/codex-first.yaml artifact, not the generic harness mechanism.
// The codex-first skill exists to route implementation work from Claude Code to
// Codex; its whole reason for being is "Claude Code sessions only — never
// self-delegate." This test fails if a future edit widens compatible_harnesses
// (or drops it, making the skill generic), which would let a codex session offer
// itself the delegation skill — a self-delegation loop the skill forbids.
func TestCodexFirstManifestIsClaudeCodeOnly(t *testing.T) {
	const manifestPath = "../../fixtures/skills/codex-first.yaml"
	manifest, err := LoadSkillManifest(manifestPath)
	if err != nil {
		t.Fatalf("load %s: %v", manifestPath, err)
	}
	if report := ValidateSkillManifest(manifest); report.Status != "ok" {
		t.Fatalf("manifest invalid: %+v", report.Checks)
	}

	home, s := newStore(t)
	if _, err := ApplySkillManifest(home, manifest, time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("apply: %v", err)
	}

	const task = "implement this feature from the frozen spec, refactor and write tests"

	claudeHits, err := s.SurfaceForHarness(task, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"skill:codex-first", "instruction_rule:codex-first-route"} {
		if !hasSurfaced(claudeHits, id) {
			t.Fatalf("%s must surface for an implementation task under claude_code", id)
		}
	}

	// The never-self-delegate invariant: codex sessions must never be offered the
	// delegation skill or its routing rule.
	codexHits, err := s.SurfaceForHarness(task, "codex")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"skill:codex-first", "instruction_rule:codex-first-route"} {
		if hasSurfaced(codexHits, id) {
			t.Fatalf("%s must NOT surface under the codex harness (self-delegation forbidden)", id)
		}
	}

	// The routing rule must delegate execution to Codex and keep verification in
	// Claude, and the skill must specialize the route-evidence doctrine.
	edges := readCapEdges(t, home)
	if !hasEdge(edges, EdSpecializes, "skill:codex-first", "doctrine:epistemics-route-evidence-not-success") {
		t.Fatal("skill:codex-first must specialize the route-evidence-not-success doctrine")
	}
	if !hasEdge(edges, EdSurfacesWith, "skill:codex-first", "harness:claude_code") {
		t.Fatal("skill:codex-first must carry a claude_code compatible-harness edge")
	}
}
