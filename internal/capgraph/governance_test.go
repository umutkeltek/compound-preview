package capgraph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGovernedAddPolicyDenyBlocksMutation(t *testing.T) {
	home, s := newStore(t)
	if err := writePolicy(home, "class_outcome:\n  local_reversible:\n    outcome: deny\n"); err != nil {
		t.Fatal(err)
	}

	if err := AddGoverned(home, Node{ID: "tool:gbrain.search", Kind: "tool", Name: "gbrain search", Triggers: []string{"search"}}, t0); err == nil || !strings.Contains(err.Error(), "refuses") {
		t.Fatalf("expected policy refusal, got %v", err)
	}

	nodes, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.ID == "tool:gbrain.search" {
			t.Fatal("denied add mutated the graph")
		}
	}
	if !hasGovernanceRecord(home, "add_node", false) {
		t.Fatal("missing denied governance record")
	}
}

func TestGovernedBindCredentialPolicyDenyBlocksMutation(t *testing.T) {
	home, s := newStore(t)
	// Seed a surface so the bind has something to attach to; then deny the
	// local_reversible class so the governed bind must refuse before writing.
	if _, err := s.AddConnectionSurface(ConnectionSurface{NodeID: "conn:github"}); err != nil {
		t.Fatal(err)
	}
	if err := writePolicy(home, "class_outcome:\n  local_reversible:\n    outcome: deny\n"); err != nil {
		t.Fatal(err)
	}

	h := CredentialHandle{Ref: "home-secret://github/token", Custodian: "compound", EnvName: "GITHUB_TOKEN"}
	if _, err := SetConnectionSurfaceCredentialGoverned(home, "conn:github", h, t0); err == nil || !strings.Contains(err.Error(), "refuses") {
		t.Fatalf("expected policy refusal, got %v", err)
	}

	surfaces, err := s.loadConnectionSurfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, cs := range surfaces {
		if cs.NodeID == "conn:github" && cs.CredentialHandle.Ref == "home-secret://github/token" {
			t.Fatal("denied credential bind mutated the connection surface")
		}
	}
	if !hasGovernanceRecord(home, "bind_credential", false) {
		t.Fatal("missing denied governance record for bind_credential")
	}
}

func TestGovernedRetirePolicyDenyBlocksMutation(t *testing.T) {
	home, s := newStore(t)
	if err := s.Add(Node{ID: "tool:gbrain.search", Kind: "tool", Name: "gbrain search", Triggers: []string{"search"}}); err != nil {
		t.Fatal(err)
	}
	if err := writePolicy(home, "class_outcome:\n  destructive:\n    outcome: deny\n"); err != nil {
		t.Fatal(err)
	}

	if _, err := RetireGoverned(home, []string{"tool:gbrain.search"}, t0); err == nil || !strings.Contains(err.Error(), "refuses") {
		t.Fatalf("expected policy refusal, got %v", err)
	}

	nodes, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.ID == "tool:gbrain.search" && n.Retired {
			t.Fatal("denied retire mutated the graph")
		}
	}
	if !hasGovernanceRecord(home, "retire_nodes", false) {
		t.Fatal("missing denied governance record")
	}
}

func TestGovernedObservePolicyDenyBlocksMutation(t *testing.T) {
	home, s := newStore(t)
	if err := s.Add(Node{ID: "tool:gbrain.search", Kind: "tool", Name: "gbrain search", Triggers: []string{"search"}}); err != nil {
		t.Fatal(err)
	}
	if err := writePolicy(home, "class_outcome:\n  local_reversible:\n    outcome: deny\n"); err != nil {
		t.Fatal(err)
	}

	if _, err := ObserveGoverned(home, "tool:gbrain.search", EdMissedWhen, "search page", 1.0, t0); err == nil || !strings.Contains(err.Error(), "refuses") {
		t.Fatalf("expected policy refusal, got %v", err)
	}

	edges, err := s.Edges()
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 0 {
		t.Fatalf("denied observe mutated edges: %+v", edges)
	}
	if !hasGovernanceRecord(home, "observe", false) {
		t.Fatal("missing denied governance record")
	}
}

func TestApplySkillManifestPolicyDenyBlocksMutation(t *testing.T) {
	home, s := newStore(t)
	if err := writePolicy(home, "class_outcome:\n  local_reversible:\n    outcome: deny\n"); err != nil {
		t.Fatal(err)
	}
	manifest := SkillManifest{
		SchemaVersion: SkillManifestSchemaVersion,
		ID:            "manifest:deny-test",
		AdapterID:     "test",
		SourceSystem:  "test",
		ObservedAt:    t0.UTC().Format(time.RFC3339),
		SourceRef:     SkillSourceRef{Path: "test.yaml", Kind: "file", Owner: "test", SourceClass: "test", Revision: "r1"},
		EvidenceRefs:  []string{"evidence:test"},
		Skills: []SkillManifestEntry{
			{ID: "skill:deny-test", Kind: "skill", Name: "Deny Test", Status: "active", Triggers: []string{"deny"}},
		},
	}

	if _, err := ApplySkillManifest(home, manifest, t0); err == nil || !strings.Contains(err.Error(), "refuses") {
		t.Fatalf("expected policy refusal, got %v", err)
	}

	nodes, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("denied manifest mutated nodes: %+v", nodes)
	}
	if !hasGovernanceRecord(home, "apply_skill_manifest", false) {
		t.Fatal("missing denied governance record")
	}
}

func TestApplySkillManifestRejectsResurrectingRetiredNode(t *testing.T) {
	home, s := newStore(t)
	if err := s.Add(Node{ID: "skill:retired-once", Kind: "skill", Name: "Retired Once", Triggers: []string{"retired"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Retire("skill:retired-once"); err != nil {
		t.Fatal(err)
	}

	manifest := SkillManifest{
		SchemaVersion: SkillManifestSchemaVersion,
		ID:            "manifest:resurrect-test",
		AdapterID:     "test",
		SourceSystem:  "test",
		ObservedAt:    t0.UTC().Format(time.RFC3339),
		SourceRef:     SkillSourceRef{Path: "test.yaml", Kind: "file", Owner: "test", SourceClass: "test", Revision: "r1"},
		EvidenceRefs:  []string{"evidence:test"},
		Skills: []SkillManifestEntry{
			{ID: "skill:retired-once", Kind: "skill", Name: "Retired Once", Status: "active", Triggers: []string{"retired"}},
		},
	}

	if _, err := ApplySkillManifest(home, manifest, t0); err == nil || !strings.Contains(err.Error(), "resurrect") {
		t.Fatalf("expected resurrect error, got %v", err)
	}

	if hasGovernanceRecord(home, "apply_skill_manifest", true) {
		t.Fatal("governance should not record an allowed mutation for a blocked resurrect")
	}
}

func TestApplySkillManifestAllowsReImportOfRetiredNode(t *testing.T) {
	home, s := newStore(t)
	if err := s.Add(Node{ID: "skill:retired-once", Kind: "skill", Name: "Retired Once", Triggers: []string{"retired"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Retire("skill:retired-once"); err != nil {
		t.Fatal(err)
	}

	manifest := SkillManifest{
		SchemaVersion: SkillManifestSchemaVersion,
		ID:            "manifest:retired-still-test",
		AdapterID:     "test",
		SourceSystem:  "test",
		ObservedAt:    t0.UTC().Format(time.RFC3339),
		SourceRef:     SkillSourceRef{Path: "test.yaml", Kind: "file", Owner: "test", SourceClass: "test", Revision: "r1"},
		EvidenceRefs:  []string{"evidence:test"},
		Skills: []SkillManifestEntry{
			{ID: "skill:retired-once", Kind: "skill", Name: "Retired Once", Status: "retired", Triggers: []string{"retired"}},
		},
	}

	if _, err := ApplySkillManifest(home, manifest, t0); err != nil {
		t.Fatalf("re-importing a retired node as retired should be allowed: %v", err)
	}

	nodes, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range nodes {
		if n.ID == "skill:retired-once" {
			found = true
			if !n.Retired {
				t.Fatal("retired node should stay retired on re-import")
			}
		}
	}
	if !found {
		t.Fatal("retired node missing after re-import")
	}
}

func TestGovernedAddRejectsResurrectingRetiredNode(t *testing.T) {
	home, s := newStore(t)
	if err := s.Add(Node{ID: "tool:retired-tool", Kind: "tool", Name: "retired tool", Triggers: []string{"tool"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Retire("tool:retired-tool"); err != nil {
		t.Fatal(err)
	}

	if err := AddGoverned(home, Node{ID: "tool:retired-tool", Kind: "tool", Name: "retired tool", Triggers: []string{"tool"}}, t0); err == nil || !strings.Contains(err.Error(), "resurrect") {
		t.Fatalf("expected resurrect error, got %v", err)
	}

	nodes, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.ID == "tool:retired-tool" && !n.Retired {
			t.Fatal("governed add resurrected retired node")
		}
	}
}

func writePolicy(home, body string) error {
	path := filepath.Join(home, "config", "policy.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

func hasGovernanceRecord(home, kind string, allowed bool) bool {
	data, err := os.ReadFile(filepath.Join(home, "registries", "capability-governance.jsonl"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec CapabilityGraphGovernanceRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec.MutationKind == kind && rec.Allowed == allowed {
			return true
		}
	}
	return false
}
