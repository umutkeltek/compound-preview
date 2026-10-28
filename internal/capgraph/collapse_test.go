package capgraph

import (
	"strings"
	"testing"
)

// The collapse test. Two tasks that share most of their words but need
// different capabilities must get different top offers, and each must get the
// right one. Measured on the live graph 2026-10-25 before this test existed:
// the right capability was top-1 for 4 of 12 realistic tasks, 21 of 60 top-5
// slots were won on tokens like "s", "user", "one", "new", and the whole
// product-pipeline bundle was invisible to claude_code because its edges
// named "harness:claude-code" while the harness node is "harness:claude_code".
//
// Triggers below mirror live nodes (same words, same weights) so a pass here
// says something about the real graph, not about a fixture tuned to pass.
func collapseStore(t *testing.T) *Store {
	t.Helper()
	_, s := newStore(t)
	add := func(n Node) {
		t.Helper()
		if n.Weight == 0 {
			n.Weight = 1
		}
		if err := s.Add(n); err != nil {
			t.Fatal(err)
		}
	}
	edge := func(e Edge) {
		t.Helper()
		if err := s.AddEdge(e, t0); err != nil {
			t.Fatal(err)
		}
	}
	// Capabilities an operator actually has.
	add(Node{ID: "skill:diagnosing-bugs", Kind: "skill", Name: "diagnosing-bugs",
		Triggers: []string{"debug", "diagnose", "root cause", "failing test", "regression", "broken", "throwing", "slow", "flaky", "fails"}})
	add(Node{ID: "skill:golang-testing", Kind: "skill", Name: "golang-testing",
		Triggers: []string{"golang", "go test", "testing", "table-driven", "testify"}})
	add(Node{ID: "skill:app-store-screenshots", Kind: "skill", Name: "app-store-screenshots",
		Triggers: []string{"app", "store", "screenshots", "captions", "marketing screenshots"}})
	add(Node{ID: "skill:release-notes", Kind: "skill", Name: "release-notes",
		Triggers: []string{"release notes", "what's new", "changelog"}})
	add(Node{ID: "skill:aso", Kind: "skill", Name: "aso",
		Triggers: []string{"aso", "subtitle", "keywords", "app store listing"}})
	add(Node{ID: "skill:pricing", Kind: "skill", Name: "pricing",
		Triggers: []string{"pricing", "price", "monetization", "plan packaging"}})
	add(Node{ID: "procedure:compound-land-ceremony", Kind: "procedure", Name: "land", Weight: 2,
		Triggers: []string{"land", "landing", "merge to main", "commit to main", "ff-only", "fast-forward", "get this onto main"}})
	add(Node{ID: "procedure:worktree-retire", Kind: "procedure", Name: "worktree-retire",
		Triggers: []string{"retire", "worktree", "stale branch", "cleanup", "remove worktree"}})
	// The product bundle, bound the way the live import bound it: with the
	// dashed harness spelling. The harness node itself is snake_case.
	add(Node{ID: "skill:product-pipeline", Kind: "skill", Name: "product-pipeline",
		Triggers: []string{"lifecycle", "interconnected", "branching", "product", "app", "business", "design", "connect", "build", "automate", "revise", "plan", "full", "whole"},
		When:     []InstructionRuleWhen{{Kind: "task-class", Values: []string{"product", "app", "business"}}, {Kind: "task-class", Values: []string{"lifecycle", "interconnected", "branching"}}}})
	add(Node{ID: "harness:claude_code", Kind: "harness", Name: "claude_code", Triggers: []string{"claude_code"}})
	add(Node{ID: "harness:codex", Kind: "harness", Name: "codex", Triggers: []string{"codex"}})
	edge(Edge{Kind: EdSurfacesWith, From: "skill:product-pipeline", To: "harness:codex"})
	edge(Edge{Kind: EdSurfacesWith, From: "skill:product-pipeline", To: "harness:claude-code"})
	// An instruction rule whose triggers echo harness names, as the live
	// codex-first rule does. The harness name must not count as a task word.
	add(Node{ID: "instruction_rule:codex-first-route", Kind: "instruction_rule", Name: "codex first", Tier: 1,
		Triggers: []string{"codex", "claude", "code", "implement", "test"},
		When:     []InstructionRuleWhen{{Kind: "harness", Values: []string{"claude_code"}}},
		Read:     []string{"doc.md"}, Boundary: []string{"offer only"}})
	// Tool descriptions imported verbatim as triggers, weight 1.2, as the
	// mcporter import does. These won top-5 slots on the live graph on "s",
	// "user", "one", "new".
	add(Node{ID: "tool:analytics.get-account-summaries", Kind: "tool", Name: "analytics", Weight: 1.2,
		Triggers: strings.Fields("google analytics get account summaries retrieves information about the user s accounts and properties mcp_tool")})
	add(Node{ID: "tool:design.apply-design-system", Kind: "tool", Name: "apply design system", Weight: 1.2,
		Triggers: strings.Fields("stitch apply design system applies a to list of screens use this tool when the user wants update one or more match style selected s")})
	add(Node{ID: "tool:search-console.submit-sitemap", Kind: "tool", Name: "submit sitemap", Weight: 1.2,
		Triggers: strings.Fields("google search console submit sitemap a new or resubmit an existing one to args site url exact property from list")})
	return s
}

func topOffer(t *testing.T, hits []Surfaced) Surfaced {
	t.Helper()
	if len(hits) == 0 {
		t.Fatal("no offers at all")
	}
	// Surface returns offers sorted by score; a tie at the top is a collapse
	// in its own right, so refuse it instead of trusting the tiebreak.
	if len(hits) > 1 && hits[0].Score == hits[1].Score {
		t.Fatalf("top-1 is a tie: %s and %s both %.2f", hits[0].Node.ID, hits[1].Node.ID, hits[0].Score)
	}
	return hits[0]
}

func TestCollapsePairsGetDifferentRightAnswers(t *testing.T) {
	s := collapseStore(t)
	pairs := []struct {
		name         string
		taskA, wantA string
		taskB, wantB string
	}{
		{"store copy",
			"Write the App Store screenshot captions for Ledgerly", "skill:app-store-screenshots",
			"Write the App Store What's New release notes for Ledgerly 1.0.5", "skill:release-notes"},
		{"branch end of life",
			"Land this branch onto main", "procedure:compound-land-ceremony",
			"Retire this stale worktree branch safely", "procedure:worktree-retire"},
		{"same product, different need",
			"Tidewatch first export fails for new users, find the root cause and propose one justified change", "skill:diagnosing-bugs",
			"Tidewatch Pro price: compare monetization options for the one-time purchase", "skill:pricing"},
		{"store listing vs store visuals",
			"Write the App Store subtitle and keywords for a tanning timer app", "skill:aso",
			"Produce 5 App Store marketing screenshots for a tanning timer app", "skill:app-store-screenshots"},
		{"test that fails vs test to write",
			"A battery went red on one hook test under load, decide if it is flaky", "skill:diagnosing-bugs",
			"Write a Go table-driven test for the new hook record field", "skill:golang-testing"},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			hitsA, err := s.SurfaceForHarness(p.taskA, "claude_code")
			if err != nil {
				t.Fatal(err)
			}
			hitsB, err := s.SurfaceForHarness(p.taskB, "claude_code")
			if err != nil {
				t.Fatal(err)
			}
			topA, topB := topOffer(t, hitsA), topOffer(t, hitsB)
			if topA.Node.ID != p.wantA {
				t.Errorf("task A %q: top-1 %s (matched %v), want %s", p.taskA, topA.Node.ID, topA.MatchedTokens, p.wantA)
			}
			if topB.Node.ID != p.wantB {
				t.Errorf("task B %q: top-1 %s (matched %v), want %s", p.taskB, topB.Node.ID, topB.MatchedTokens, p.wantB)
			}
			if topA.Node.ID == topB.Node.ID {
				t.Errorf("pair collapsed: both tasks got %s", topA.Node.ID)
			}
		})
	}
}

func TestNoiseTokensDoNotCreateOffers(t *testing.T) {
	s := collapseStore(t)
	task := "Tidewatch first export fails for new users, find the root cause and propose one justified change"
	hits, err := s.SurfaceForHarness(task, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if strings.HasPrefix(h.Node.ID, "tool:") {
			t.Errorf("%s surfaced on %v; a tool description's filler words are not a match", h.Node.ID, h.MatchedTokens)
		}
		for _, tok := range h.MatchedTokens {
			if len(tok) < 3 {
				t.Errorf("%s matched on %q, a token too short to mean anything", h.Node.ID, tok)
			}
		}
	}
}

func TestHarnessSpellingDoesNotHideACapability(t *testing.T) {
	s := collapseStore(t)
	const task = "Design a product lifecycle pipeline."
	for _, harness := range []string{"claude_code", "claude-code", "codex", "harness:claude_code"} {
		hits, err := s.SurfaceForHarness(task, harness)
		if err != nil {
			t.Fatal(err)
		}
		if top := topOffer(t, hits); top.Node.ID != "skill:product-pipeline" {
			t.Errorf("harness %q: top-1 %s, want skill:product-pipeline; the README example must hold under every spelling", harness, top.Node.ID)
		}
	}
	// The bundle is still product-scoped: a branch task must not drag it in.
	hits, err := s.SurfaceForHarness("Retire this stale worktree branch safely", "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if hasSurfaced(hits, "skill:product-pipeline") {
		t.Error("product-pipeline surfaced for a worktree task; a visible bundle must still be task-conditioned")
	}
}

func TestSkillManifestWarnsOnUnknownHarnessAndNormalizesSpelling(t *testing.T) {
	home, s := newStore(t)
	if err := s.Add(Node{ID: "harness:claude_code", Kind: "harness", Name: "claude_code", Triggers: []string{"claude_code"}}); err != nil {
		t.Fatal(err)
	}
	manifest := SkillManifest{
		SchemaVersion: SkillManifestSchemaVersion, ID: "bundle-test", AdapterID: "test", SourceSystem: "test",
		ObservedAt: "2026-10-25T00:00:00Z",
		SourceRef:  SkillSourceRef{Path: "README.md", Kind: "bundle", Owner: "test", SourceClass: "evidence", Revision: "r1"},
		Skills: []SkillManifestEntry{{
			ID: "skill:bundle", Name: "bundle", Status: "active", Triggers: []string{"bundle"},
			CompatibleHarnesses: []string{"claude-code", "no-such-harness"},
		}},
		EvidenceRefs: []string{"README.md"},
	}
	report, err := ApplySkillManifest(home, manifest, t0)
	if err != nil {
		t.Fatalf("apply: %v (%+v)", err, report.Checks)
	}
	var warned bool
	for _, c := range report.Checks {
		if c.Name == "harness_nodes" && c.Status == "warn" && strings.Contains(c.Detail, "harness:no_such_harness") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("import did not warn about the unknown harness node; checks: %+v", report.Checks)
	}
	edges, err := s.loadEdges()
	if err != nil {
		t.Fatal(err)
	}
	var sawCanonical bool
	for _, e := range edges {
		if e.Kind != EdSurfacesWith || e.From != "skill:bundle" {
			continue
		}
		if strings.Contains(e.To, "-") {
			t.Errorf("edge written with dashed harness id %q; writes must be snake_case", e.To)
		}
		if e.To == "harness:claude_code" {
			sawCanonical = true
		}
		if !strings.HasPrefix(e.Provenance, "skill_manifest:") {
			t.Errorf("edge %s->%s carries no manifest provenance (%q)", e.From, e.To, e.Provenance)
		}
	}
	if !sawCanonical {
		t.Error("the dashed compatible_harnesses entry was not written as harness:claude_code")
	}
}
