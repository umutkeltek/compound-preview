package capgraph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func addRoutingBoostFixture(t *testing.T, store *Store, ownerID, token string) {
	t.Helper()
	evalID := "eval:routing-boost-" + strings.ReplaceAll(ownerID, ":", "-")
	if err := store.Add(Node{
		ID:           evalID,
		Kind:         "eval",
		Name:         "Synthetic routing evaluation",
		Triggers:     []string{token},
		Weight:       1,
		EvidenceOnly: true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, edge := range []Edge{
		{
			Kind: EdValidatedBy, From: "context:" + token, To: ownerID,
			Confidence: 1, Weight: 1,
		},
		{
			Kind: EdMissedWhen, From: "context:" + token, To: ownerID,
			Confidence: 1, Weight: 1,
		},
		{
			Kind: EdEvaluatedBy, From: ownerID, To: evalID,
			Confidence: 1, Weight: 1,
		},
	} {
		if err := store.AddEdge(edge, t0); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSurfaceSkillSourceReachabilityIsAHardGate(t *testing.T) {
	_, store := newStore(t)
	unreachable := Node{
		ID:       "skill:source-stamped",
		Kind:     "skill",
		Name:     "Shared Routing Skill",
		Triggers: []string{"routing"},
		Weight:   1,
		Source: &NodeSource{
			Root:      "/synthetic/skills",
			Path:      "/synthetic/skills/shared-routing",
			Reachable: false,
			Reason:    "missing",
		},
	}
	legacy := unreachable
	legacy.ID = "skill:legacy-without-source"
	legacy.Source = nil

	for _, node := range []Node{unreachable, legacy} {
		if err := store.Add(node); err != nil {
			t.Fatal(err)
		}
	}
	addRoutingBoostFixture(t, store, unreachable.ID, "routing")

	task := "routing"
	embedder := newSemanticStub("stub:source-hard-gate:2:f32")
	embedder.vectors[task] = []float32{1, 0}
	embedder.defaultVec = []float32{1, 0}

	hits, err := store.SurfaceForContext(SurfaceContext{
		Task: task, Embedder: embedder,
	})
	if err != nil {
		t.Fatal(err)
	}
	if hit := surfacedByID(hits, unreachable.ID); hit != nil {
		t.Fatalf("unreachable skill surfaced despite lexical, semantic, feedback, and evaluation boosts: %+v", hit)
	}
	if hit := surfacedByID(hits, legacy.ID); hit == nil {
		t.Fatalf("nil Source must retain legacy surfacing compatibility: %+v", hits)
	}

	// The two nodes deliberately share display name and triggers. Recovery is
	// per identity/source stamp, not a name-level merge or suppression.
	unreachable.Source.Reachable = true
	unreachable.Source.Reason = "ok"
	if err := store.Add(unreachable); err != nil {
		t.Fatal(err)
	}
	hits, err = store.SurfaceForContext(SurfaceContext{
		Task: task, Embedder: embedder,
	})
	if err != nil {
		t.Fatal(err)
	}
	hit := surfacedByID(hits, unreachable.ID)
	if hit == nil {
		t.Fatalf("reachable replacement did not recover surfacing: %+v", hits)
	}
	for _, contribution := range []string{
		"+semantic=", "+validated_by=", "+missed_when=", "+evidence=",
	} {
		if !strings.Contains(hit.Why, contribution) {
			t.Fatalf("fixture did not exercise %s bonus: %+v", contribution, hit)
		}
	}
	if surfacedByID(hits, legacy.ID) == nil {
		t.Fatalf("same-name identities were incorrectly deduplicated: %+v", hits)
	}
}

func TestApplySkillManifestSourceStampControlsSurfaceAndRecovers(t *testing.T) {
	home, store := newStore(t)
	root := t.TempDir()
	skillPath := filepath.Join(root, "conditional")
	if err := os.Symlink(filepath.Join(root, "missing-target"), skillPath); err != nil {
		t.Fatal(err)
	}

	manifest := SkillManifest{
		SchemaVersion: SkillManifestSchemaVersion,
		ID:            "skill_manifest_surface_source_fixture",
		AdapterID:     "fixture.surface-source",
		SourceSystem:  "fixture",
		ObservedAt:    t0.UTC().Format("2006-01-02T15:04:05Z07:00"),
		SourceRef: SkillSourceRef{
			Path:        root,
			Kind:        "fixture",
			Owner:       "capability-graph-test",
			SourceClass: "fixture",
			Revision:    "sha256:synthetic",
		},
		EvidenceRefs: []string{"fixture:surface-source"},
		Skills: []SkillManifestEntry{{
			ID:        "skill:conditional",
			Kind:      "skill",
			Name:      "Conditional",
			Status:    "active",
			Triggers:  []string{"routing"},
			SkillPath: skillPath,
		}},
	}

	if _, err := ApplySkillManifest(home, manifest, t0); err != nil {
		t.Fatal(err)
	}
	if hit := surfacedByID(mustSurface(t, store, "routing"), "skill:conditional"); hit != nil {
		t.Fatalf("manifest-stamped unreachable skill surfaced: %+v", hit)
	}
	nodes, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Source == nil || nodes[0].Source.Reachable {
		t.Fatalf("first import source stamp = %+v, want unreachable", nodes)
	}

	if err := os.Remove(skillPath); err != nil {
		t.Fatal(err)
	}
	writeSkillSource(t, root, "conditional", `---
name: conditional
description: "Synthetic conditional skill"
---
# Conditional
`)
	if _, err := ApplySkillManifest(home, manifest, t0); err != nil {
		t.Fatal(err)
	}
	hit := surfacedByID(mustSurface(t, store, "routing"), "skill:conditional")
	if hit == nil {
		t.Fatal("reimport after source repair did not restore surfacing")
	}
	if hit.Node.Source == nil || !hit.Node.Source.Reachable {
		t.Fatalf("repaired source stamp = %+v, want reachable", hit.Node.Source)
	}
}

func TestSurfaceSkillWhenUsesConjunctiveDimensionsAndORAlternatives(t *testing.T) {
	_, store := newStore(t)
	conditional := Node{
		ID:       "skill:conditioned",
		Kind:     "skill",
		Name:     "Conditioned Routing",
		Triggers: []string{"routing"},
		Weight:   1,
		When: []InstructionRuleWhen{
			{Kind: "task-class", Values: []string{"debug", "audit"}},
			{Kind: "repo-kind", Values: []string{"go-service"}},
			{Kind: "harness", Values: []string{"codex"}},
			{Kind: "file-path", Values: []string{"internal/capgraph"}},
			{Kind: "phase", Values: []string{"verify"}},
		},
	}
	unknown := Node{
		ID:       "skill:unknown-condition",
		Kind:     "skill",
		Name:     "Unknown Condition",
		Triggers: []string{"routing"},
		Weight:   1,
		When: []InstructionRuleWhen{{
			Kind: "audience", Values: []string{"operators"},
		}},
	}
	for _, node := range []Node{conditional, unknown} {
		if err := store.Add(node); err != nil {
			t.Fatal(err)
		}
	}
	addRoutingBoostFixture(t, store, conditional.ID, "routing")

	embedder := newSemanticStub("stub:skill-when-hard-gate:2:f32")
	embedder.defaultVec = []float32{1, 0}
	for _, task := range []string{"audit routing", "plan routing"} {
		embedder.vectors[task] = []float32{1, 0}
	}
	matching := SurfaceContext{
		Task:     "audit routing", // second task-class alternative proves OR.
		RepoKind: "go_service",
		Harness:  "codex",
		FilePath: "/workspace/internal/capgraph/capgraph.go",
		Phase:    "verify",
		Embedder: embedder,
	}
	hits, err := store.SurfaceForContext(matching)
	if err != nil {
		t.Fatal(err)
	}
	hit := surfacedByID(hits, conditional.ID)
	if hit == nil {
		t.Fatalf("skill satisfying every When dimension did not surface: %+v", hits)
	}
	for _, contribution := range []string{
		"+semantic=", "+validated_by=", "+missed_when=", "+evidence=",
	} {
		if !strings.Contains(hit.Why, contribution) {
			t.Fatalf("fixture did not exercise %s bonus: %+v", contribution, hit)
		}
	}
	if surfacedByID(hits, unknown.ID) != nil {
		t.Fatalf("unknown skill When kind must fail closed even with lexical and semantic matches: %+v", hits)
	}

	cases := []struct {
		name   string
		mutate func(*SurfaceContext)
	}{
		{"task-class", func(ctx *SurfaceContext) { ctx.Task = "plan routing" }},
		{"repo-kind", func(ctx *SurfaceContext) { ctx.RepoKind = "ios-app" }},
		{"harness", func(ctx *SurfaceContext) { ctx.Harness = "claude_code" }},
		{"file-path", func(ctx *SurfaceContext) { ctx.FilePath = "/workspace/cmd/compound/main.go" }},
		{"phase", func(ctx *SurfaceContext) { ctx.Phase = "execute" }},
	}
	for _, tc := range cases {
		t.Run("mismatched "+tc.name, func(t *testing.T) {
			ctx := matching
			tc.mutate(&ctx)
			got, err := store.SurfaceForContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if hit := surfacedByID(got, conditional.ID); hit != nil {
				t.Fatalf("%s mismatch was bypassed by routing bonuses: %+v", tc.name, hit)
			}
		})
	}
}

func TestSurfaceSkillTaskClassPreservesExplicitSkillTerms(t *testing.T) {
	_, store := newStore(t)
	node := Node{
		ID:   "skill:authoring-router",
		Kind: "skill",
		Name: "Skill Authoring Router",
		Triggers: []string{
			"write", "create", "modify", "update", "improve",
			"product", "page", "pricing", "onboarding", "screenshots",
		},
		Weight: 1,
		When: []InstructionRuleWhen{{
			Kind:   "task-class",
			Values: []string{"skill", "creator", "triggering"},
		}},
	}
	if err := store.Add(node); err != nil {
		t.Fatal(err)
	}
	addRoutingBoostFixture(t, store, node.ID, "improve")

	positive := []string{
		"write a skill",
		"create a skill",
		"modify an existing skill",
		"update agent skills",
		"review the skill creator",
		"debug capability triggering",
	}
	negative := []string{
		"create a product page",
		"improve the pricing page",
		"improve onboarding activation",
		"improve app store screenshots",
		"improve a product landing page",
	}

	embedder := newSemanticStub("stub:explicit-skill-task-class:2:f32")
	embedder.defaultVec = []float32{1, 0}
	for _, task := range append(append([]string{}, positive...), negative...) {
		embedder.vectors[task] = []float32{1, 0}
	}
	controlTask := "improve skill creator routing"
	embedder.vectors[controlTask] = []float32{1, 0}

	for _, task := range positive {
		t.Run("admits "+task, func(t *testing.T) {
			hits, err := store.SurfaceForContext(SurfaceContext{
				Task: task, Embedder: embedder,
			})
			if err != nil {
				t.Fatal(err)
			}
			if surfacedByID(hits, node.ID) == nil {
				t.Fatalf("explicit skill-authoring task %q was rejected: %+v", task, hits)
			}
		})
	}

	// Prove every scoring lane in the negative cases is genuinely armed.
	// The only material difference is whether the task-class condition matches.
	control, err := store.SurfaceForContext(SurfaceContext{
		Task: controlTask, Embedder: embedder,
	})
	if err != nil {
		t.Fatal(err)
	}
	controlHit := surfacedByID(control, node.ID)
	if controlHit == nil {
		t.Fatalf("matching control did not surface: %+v", control)
	}
	for _, contribution := range []string{
		"+semantic=", "+validated_by=", "+missed_when=", "+evidence=",
	} {
		if !strings.Contains(controlHit.Why, contribution) {
			t.Fatalf("fixture did not exercise %s bonus: %+v", contribution, controlHit)
		}
	}

	for _, task := range negative {
		t.Run("rejects "+task, func(t *testing.T) {
			hits, err := store.SurfaceForContext(SurfaceContext{
				Task: task, Embedder: embedder,
			})
			if err != nil {
				t.Fatal(err)
			}
			if hit := surfacedByID(hits, node.ID); hit != nil {
				t.Fatalf("generic product task %q bypassed skill task-class gate: %+v", task, hit)
			}
		})
	}
}

func TestApplySkillManifestValidatesSkillWhenBeforeMutation(t *testing.T) {
	invalid := []struct {
		name string
		when InstructionRuleWhen
	}{
		{
			name: "unknown kind",
			when: InstructionRuleWhen{
				Kind: "audience", Values: []string{"operators"},
			},
		},
		{
			name: "blank kind",
			when: InstructionRuleWhen{
				Kind: " \t ", Values: []string{"audit"},
			},
		},
		{
			name: "empty values",
			when: InstructionRuleWhen{
				Kind: "task-class",
			},
		},
		{
			name: "all-whitespace values",
			when: InstructionRuleWhen{
				Kind: "phase", Values: []string{"", " ", "\t"},
			},
		},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			home, store := newStore(t)
			manifest := validSkillManifestForTest()
			manifest.Skills[0].When = []InstructionRuleWhen{tc.when}

			report, err := ApplySkillManifest(home, manifest, t0)
			if err == nil {
				t.Fatalf("ApplySkillManifest accepted malformed skill When: %+v", tc.when)
			}
			if report.Status != "fail" {
				t.Fatalf("validation status = %q, want fail: %+v", report.Status, report.Checks)
			}
			nodes, listErr := store.List()
			if listErr != nil {
				t.Fatal(listErr)
			}
			if len(nodes) != 0 {
				t.Fatalf("malformed skill When mutated graph: %+v", nodes)
			}
		})
	}

	t.Run("valid dimensions survive import and legacy no-When stays compatible", func(t *testing.T) {
		home, store := newStore(t)
		manifest := validSkillManifestForTest()
		manifest.Skills = []SkillManifestEntry{
			{
				ID:       "skill:conditioned-import",
				Kind:     "skill",
				Name:     "Conditioned Import",
				Status:   "active",
				Triggers: []string{"routing"},
				When: []InstructionRuleWhen{
					{Kind: "task-class", Values: []string{"pricing"}},
					{Kind: "repo-kind", Values: []string{"go-service"}},
					{Kind: "harness", Values: []string{"codex"}},
					{Kind: "file-path", Values: []string{"internal/capgraph"}},
					{Kind: "phase", Values: []string{"verify"}},
				},
			},
			{
				ID:       "skill:legacy-no-when",
				Kind:     "skill",
				Name:     "Legacy No When",
				Status:   "active",
				Triggers: []string{"routing"},
			},
		}

		report, err := ApplySkillManifest(home, manifest, t0)
		if err != nil {
			t.Fatalf("valid skill When rejected: report=%+v err=%v", report, err)
		}
		nodes, err := store.List()
		if err != nil {
			t.Fatal(err)
		}
		var conditioned *Node
		for i := range nodes {
			if nodes[i].ID == "skill:conditioned-import" {
				conditioned = &nodes[i]
				break
			}
		}
		if conditioned == nil || len(conditioned.When) != 5 {
			t.Fatalf("valid skill When did not survive import: %+v", conditioned)
		}

		base := SurfaceContext{
			Task:     "routing",
			RepoKind: "go_service",
			Harness:  "codex",
			FilePath: "/workspace/internal/capgraph/capgraph.go",
			Phase:    "verify",
		}
		hits, err := store.SurfaceForContext(base)
		if err != nil {
			t.Fatal(err)
		}
		if surfacedByID(hits, "skill:conditioned-import") != nil {
			t.Fatalf("imported task-class restriction was ignored: %+v", hits)
		}
		if surfacedByID(hits, "skill:legacy-no-when") == nil {
			t.Fatalf("legacy no-When skill lost compatibility: %+v", hits)
		}

		base.Task = "pricing routing"
		hits, err = store.SurfaceForContext(base)
		if err != nil {
			t.Fatal(err)
		}
		if surfacedByID(hits, "skill:conditioned-import") == nil {
			t.Fatalf("valid imported conditions did not admit matching context: %+v", hits)
		}
		if surfacedByID(hits, "skill:legacy-no-when") == nil {
			t.Fatalf("legacy no-When skill disappeared in matching context: %+v", hits)
		}
	})
}
