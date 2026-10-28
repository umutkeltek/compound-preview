package routeevidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCF(t *testing.T, root, id, route string, aliases map[string]string) {
	t.Helper()
	cf := conformanceFile{Schema: "switchback/profile-conformance@1", ConformanceRevision: "sha256:" + id, Profile: conformanceProfile{ID: id, Harness: "claude-code", Route: route, RequestedModel: route, RequestedEffort: "xhigh", ModelAliases: aliases}}
	b, _ := json.Marshal(cf)
	if err := os.WriteFile(filepath.Join(root, id+".json"), b, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestPlanRouteExperimentSeparatesRoleAliasFromObservation(t *testing.T) {
	root := t.TempDir()
	writeCF(t, root, "claude-gpt", "codex/gpt-5.6-sol", map[string]string{"opus": "codex/gpt-5.6-sol"})
	writeCF(t, root, "claude-minimax", "minimax/MiniMax-M3", map[string]string{"opus": "MiniMax-M3[1m]"})
	p, err := PlanRouteExperiment(root, "opus", []string{"claude-minimax", "claude-gpt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Lanes) != 2 || p.Lanes[0].ProfileID != "claude-gpt" {
		t.Fatalf("lanes not deterministic: %#v", p.Lanes)
	}
	if p.Lanes[1].RequestedAlias != "MiniMax-M3[1m]" || p.Lanes[1].ObservedModel != "" || p.Lanes[1].EvidenceStatus != "unknown_until_route_evidence" {
		t.Fatalf("role/observation conflated: %#v", p.Lanes[1])
	}
	if p.RoleAlias != "opus" || len(p.RoleAliases) != 0 {
		t.Fatalf("legacy role fields changed: role_alias=%q role_aliases=%v", p.RoleAlias, p.RoleAliases)
	}
	legacySeed, _ := json.Marshal(struct {
		Role  string
		Lanes []ExperimentLane
	}{"opus", p.Lanes})
	legacySum := sha256.Sum256(legacySeed)
	if want := "rexp_" + hex.EncodeToString(legacySum[:6]); p.ID != want {
		t.Fatalf("legacy plan id = %q, want %q", p.ID, want)
	}
	if p.Execution != "experimental only (unsupported in v0.1): execute each lane through compound work delegate with --experimental-delegate-mutations; import Switchback trace evidence before comparison" {
		t.Fatalf("legacy execution changed: %q", p.Execution)
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"role_alias":"opus"`)) || bytes.Contains(body, []byte(`"role_aliases"`)) {
		t.Fatalf("legacy JSON role fields changed: %s", body)
	}
	var text bytes.Buffer
	WriteExperimentPlanText(&text, p)
	if !strings.Contains(text.String(), "- role_alias: opus\n") || strings.Contains(text.String(), "- role_aliases:") ||
		!strings.Contains(text.String(), "- execution: "+p.Execution+"\n") {
		t.Fatalf("legacy text output changed:\n%s", text.String())
	}
}

func TestPlanRouteExperimentMatrixKeepsLegacyAliasAndAddsRoleAliases(t *testing.T) {
	root := t.TempDir()
	writeCF(t, root, "claude-qwen", "qwen/qwen3.8-max", map[string]string{
		"opus": "qwen3.8-max", "sonnet": "qwen3.7-plus",
	})
	p, err := PlanRouteExperimentMatrix(root, []string{"opus", "sonnet"}, nil, []string{"claude-qwen"})
	if err != nil {
		t.Fatal(err)
	}
	if p.RoleAlias != "opus" || strings.Join(p.RoleAliases, ",") != "opus,sonnet" {
		t.Fatalf("matrix role fields = role_alias=%q role_aliases=%v", p.RoleAlias, p.RoleAliases)
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"role_alias":"opus"`)) || !bytes.Contains(body, []byte(`"role_aliases":["opus","sonnet"]`)) {
		t.Fatalf("matrix JSON did not add aliases without emptying legacy alias: %s", body)
	}
	var text bytes.Buffer
	WriteExperimentPlanText(&text, p)
	if !strings.Contains(text.String(), "- role_aliases: opus,sonnet\n") || strings.Contains(text.String(), "- role_alias: ") {
		t.Fatalf("matrix text output did not use plural roles:\n%s", text.String())
	}
}
func TestPlanRouteExperimentFailsMissingAlias(t *testing.T) {
	root := t.TempDir()
	writeCF(t, root, "a", "x/a", map[string]string{})
	writeCF(t, root, "b", "x/b", map[string]string{"opus": "b"})
	if _, err := PlanRouteExperiment(root, "opus", []string{"a", "b"}); err == nil {
		t.Fatal("expected missing alias refusal")
	}
}
