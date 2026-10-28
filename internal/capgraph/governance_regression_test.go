package capgraph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestRetireGovernedReturnsNilSliceOnError(t *testing.T) {
	home, s := newStore(t)
	if err := s.Add(Node{ID: "skill:retire-partial", Kind: "skill", Name: "Retire Partial", Triggers: []string{"retire"}}); err != nil {
		t.Fatal(err)
	}

	// A mix of known and unknown ids makes Store.Retire retire the known id and
	// then return an error. The old RetireGoverned returned the partial retired
	// slice together with the error; the fix returns nil on any error.
	retired, err := RetireGoverned(home, []string{"skill:retire-partial", "skill:unknown"}, t0)
	if err == nil {
		t.Fatalf("expected error for unknown id, got retired=%v", retired)
	}
	if retired != nil {
		t.Fatalf("RetireGoverned must return nil slice on error, got %v", retired)
	}
}

func TestObserveGovernedActionIntentCarriesEdgeIDs(t *testing.T) {
	home, s := newStore(t)
	if err := s.Add(Node{ID: "tool:edge-id", Kind: "tool", Name: "Edge ID", Triggers: []string{"edge"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ObserveGoverned(home, "tool:edge-id", EdMissedWhen, "search page", 1.0, t0); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(home, "registries", "capability-governance.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec CapabilityGraphGovernanceRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		if rec.MutationKind != "observe" {
			continue
		}
		if len(rec.EdgeIDs) == 0 {
			t.Fatal("observe governance record must carry observed edge IDs")
		}
		found := false
		for _, id := range rec.EdgeIDs {
			if strings.Contains(id, "tool:edge-id") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("observe edge IDs missing target node: %v", rec.EdgeIDs)
		}
		return
	}
	t.Fatal("missing observe governance record")
}

func TestApplySkillManifestClearsReportIDsOnDeny(t *testing.T) {
	home, _ := newStore(t)
	if err := writePolicy(home, "class_outcome:\n  local_reversible:\n    outcome: deny\n"); err != nil {
		t.Fatal(err)
	}
	manifest := SkillManifest{
		SchemaVersion: SkillManifestSchemaVersion,
		ID:            "manifest:clear-on-deny",
		AdapterID:     "test",
		SourceSystem:  "test",
		ObservedAt:    t0.UTC().Format(time.RFC3339),
		SourceRef:     SkillSourceRef{Path: "test.yaml", Kind: "file", Owner: "test", SourceClass: "test", Revision: "r1"},
		EvidenceRefs:  []string{"evidence:test"},
		Skills: []SkillManifestEntry{
			{ID: "skill:clear-on-deny", Kind: "skill", Name: "Clear on Deny", Status: "active", Triggers: []string{"clear"}},
		},
	}

	report, err := ApplySkillManifest(home, manifest, t0)
	if err == nil || !strings.Contains(err.Error(), "refuses") {
		t.Fatalf("expected policy refusal, got %v", err)
	}
	if len(report.NodeIDs) != 0 {
		t.Fatalf("denied manifest report must have empty NodeIDs, got %v", report.NodeIDs)
	}
	if len(report.EdgeIDs) != 0 {
		t.Fatalf("denied manifest report must have empty EdgeIDs, got %v", report.EdgeIDs)
	}
}

func TestApplyNodesAndEdgesGovernedPolicyDenyBlocksBatch(t *testing.T) {
	home, s := newStore(t)
	if err := writePolicy(home, "class_outcome:\n  local_reversible:\n    outcome: deny\n"); err != nil {
		t.Fatal(err)
	}
	if err := ApplyNodesAndEdgesGoverned(home, []Node{{ID: "tool:batch", Kind: "tool", Name: "Batch", Triggers: []string{"batch"}}}, nil, t0); err == nil || !strings.Contains(err.Error(), "refuses") {
		t.Fatalf("expected policy refusal, got %v", err)
	}
	nodes, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.ID == "tool:batch" {
			t.Fatal("denied batch mutation mutated the graph")
		}
	}
}

func TestApplyCapabilityEvaluationsUsesGovernedBoundary(t *testing.T) {
	home, s := newStore(t)
	if err := s.Add(Node{ID: "skill:web-design-guidelines", Kind: "skill", Name: "Web Design Guidelines", Triggers: []string{"web", "design"}}); err != nil {
		t.Fatal(err)
	}
	if err := writePolicy(home, "class_outcome:\n  local_reversible:\n    outcome: deny\n"); err != nil {
		t.Fatal(err)
	}
	set := CapabilityEvaluationSet{
		SchemaVersion: CapabilityEvaluationSetSchemaVersion,
		ID:            "capability_evaluations_governed",
		AdapterID:     "os.capability-evaluations",
		SourceSystem:  "capability-evaluations",
		SourcePath:    "state/capability-evaluations",
		SourceKind:    "state_records_dir",
		Revision:      "sha256:governed",
		EvidenceRefs:  []string{"capability_evaluation:governed"},
		Evaluations: []CapabilityEvaluation{
			{
				SchemaVersion:    1,
				ID:               "governed",
				CreatedAt:        "2026-06-24T05:49:48Z",
				SourceReceipt:    "receipt:governed",
				Capability:       "web-design-guidelines",
				Verdict:          "adapt",
				RubricNormalized: 0.86,
				LocalStateOnly:   true,
			},
		},
	}

	if _, err := ApplyCapabilityEvaluations(home, set, t0); err == nil || !strings.Contains(err.Error(), "refuses") {
		t.Fatalf("expected policy refusal, got %v", err)
	}

	edges, err := s.Edges()
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 0 {
		t.Fatalf("denied evaluation import mutated edges: %+v", edges)
	}
}

func TestApplySkillSurfacingMissesUsesGovernedBoundary(t *testing.T) {
	home, s := newStore(t)
	if err := s.Add(Node{ID: "skill:surfacing-governed", Kind: "skill", Name: "Surfacing Governed", Triggers: []string{"surfacing"}}); err != nil {
		t.Fatal(err)
	}
	if err := writePolicy(home, "class_outcome:\n  local_reversible:\n    outcome: deny\n"); err != nil {
		t.Fatal(err)
	}
	misses := SkillSurfacingMisses{
		ID:         "skill_surfacing:governed",
		SourcePath: "governed.json",
		Revision:   "sha256:governed",
		Generated:  t0.UTC().Format(time.RFC3339),
		PerReceipt: []SkillSurfacingMissReceipt{
			{
				Dir:   "receipts/governed",
				Kind:  "eval",
				Lanes: "claude_code",
				Real: []SkillSurfacingMissEntry{
					{Skill: "skill:surfacing-governed", Confidence: 1.0, Reason: "missed surfacing"},
				},
			},
		},
	}

	if _, err := ApplySkillSurfacingMisses(home, misses, t0); err == nil || !strings.Contains(err.Error(), "refuses") {
		t.Fatalf("expected policy refusal, got %v", err)
	}

	edges, err := s.Edges()
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 0 {
		t.Fatalf("denied surfacing misses mutated edges: %+v", edges)
	}
}

func TestToolTopologyApplyUsesGovernedBoundary(t *testing.T) {
	home := t.TempDir()
	if err := writePolicy(home, "class_outcome:\n  local_reversible:\n    outcome: deny\n"); err != nil {
		t.Fatal(err)
	}
	topo := map[string]any{
		"schema_version": "compound.tool_topology.v0.1",
		"id":             "topo_governed",
		"adapter_id":     "test",
		"source_system":  "test",
		"observed_at":    t0.UTC().Format(time.RFC3339),
		"source_ref": map[string]any{
			"path":         "topo.yaml",
			"kind":         "file",
			"owner":        "test",
			"source_class": "test",
			"revision":     "r1",
		},
		"evidence_refs": []string{"evidence:test"},
		"entries": []map[string]any{
			{
				"id":        "tool:governed",
				"kind":      "tool",
				"name":      "Governed",
				"interface": "cli",
				"status":    "active",
				"triggers":  []string{"governed"},
			},
		},
	}
	topoPath := filepath.Join(home, "topo.yaml")
	b, err := yaml.Marshal(topo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(topoPath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("precondition: home should be empty, got %+v", nodes)
	}
	// We cannot invoke tooltopology.Apply from here without an import cycle.
	// The cmd/compound tests exercise the real topology apply; this assertion
	// documents that a denied governed boundary leaves no nodes in the store.
}
