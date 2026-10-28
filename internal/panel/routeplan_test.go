package panel

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/routeevidence"
)

var panelTestNow = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

func TestSelectLanesEligiblePathOmitsFreeJudge(t *testing.T) {
	home := t.TempDir()
	_, err := routeevidence.ImportModelCapabilitiesYAML(home, filepath.Join("..", "..", "fixtures", "panel", "model-capabilities.yaml"), panelTestNow)
	if err != nil {
		t.Fatalf("import capabilities: %v", err)
	}
	plan, err := SelectLanes(home, false, true, panelTestNow)
	if err != nil {
		t.Fatalf("select lanes: %v", err)
	}
	if plan.AuthorityRef != routeevidence.ModelCapabilitiesAuthorityRef || plan.LensPlanID == "" || plan.JudgePlanID == "" {
		t.Fatalf("route authority/ids not populated: %+v", plan)
	}
	if plan.LensLane.ModelID != "panel/free-extract" || plan.LensLane.Tier != "free" || plan.LensLane.GateRole != "eligible" {
		t.Fatalf("lens lane = %+v", plan.LensLane)
	}
	if plan.JudgeLane.ModelID != "panel/cheap-judge" || plan.JudgeLane.Tier != "cheap" || plan.JudgeLane.GateRole != "eligible" {
		t.Fatalf("judge lane = %+v", plan.JudgeLane)
	}
	if !omittedReason(plan.OmittedLanes, "panel/free-judge-tripwire", "free lane may not judge") {
		t.Fatalf("omitted lanes = %+v, want free judge omission", plan.OmittedLanes)
	}
}

func TestSelectLanesPrivacyNoLocalJudgeBlocksJudgeClass(t *testing.T) {
	home := t.TempDir()
	manifest := filepath.Join(t.TempDir(), "model-capabilities.yaml")
	if err := os.WriteFile(manifest, []byte(`version: 1
purpose: privacy fixture
rules: []
capability_classes: [classify-extract, judge-critique]
models:
  local/extract:
    lane: local/extract
    provider_route: direct
    resolution: {mode: direct, targets: [{provider: fixture, model: local/extract}]}
    tier: local-batch
    packet_ceiling: single-target
    capabilities:
      classify-extract: {level: primary, evidence: fixture}
  paid/judge:
    lane: paid/judge
    provider_route: direct
    resolution: {mode: direct, targets: [{provider: fixture, model: paid/judge}]}
    tier: cheap
    packet_ceiling: single-target
    capabilities:
      judge-critique: {level: primary, evidence: fixture}
routing_defaults:
  classify-extract: [local/extract]
  judge-critique: [paid/judge]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := routeevidence.ImportModelCapabilitiesYAML(home, manifest, panelTestNow)
	if err != nil {
		t.Fatalf("import capabilities: %v", err)
	}
	_, err = SelectLanes(home, true, true, panelTestNow)
	var blocked ErrBlockedNoLane
	if !errors.As(err, &blocked) || blocked.Class != "judge-critique" {
		t.Fatalf("err = %v, want blocked judge-critique", err)
	}
}

func omittedReason(omitted []OmittedLane, modelID, reason string) bool {
	for _, lane := range omitted {
		if lane.ModelID == modelID && strings.Contains(lane.Reason, reason) {
			return true
		}
	}
	return false
}

func TestSelectLanesSupervisedOnlyJudgeRequiresOperatorSupervision(t *testing.T) {
	home := t.TempDir()
	manifest := filepath.Join(t.TempDir(), "model-capabilities.yaml")
	body := []byte(`version: 1
purpose: supervised judge fixture
capability_classes:
  - classify-extract
  - judge-critique
models:
  panel/extract:
    lane: "panel/extract"
    provider_route: direct
    resolution: {mode: direct, targets: [{provider: fixture, model: panel/extract}]}
    tier: free
    capabilities:
      classify-extract:
        level: primary
        evidence: fixture
  panel/supervised-judge:
    lane: "panel/supervised-judge"
    provider_route: direct
    resolution: {mode: direct, targets: [{provider: fixture, model: panel/supervised-judge}]}
    tier: cheap
    capabilities:
      judge-critique:
        level: assumed
        evidence: fixture
  panel/free-judge:
    lane: "panel/free-judge"
    provider_route: direct
    resolution: {mode: direct, targets: [{provider: fixture, model: panel/free-judge}]}
    tier: free
    capabilities:
      judge-critique:
        level: primary
        evidence: fixture
routing_defaults:
  classify-extract:
    - panel/extract
  judge-critique:
    - panel/supervised-judge
    - panel/free-judge
`)
	if err := os.WriteFile(manifest, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := routeevidence.ImportModelCapabilitiesYAML(home, manifest, panelTestNow); err != nil {
		t.Fatalf("import capabilities: %v", err)
	}
	supervised, err := SelectLanes(home, false, true, panelTestNow)
	if err != nil {
		t.Fatalf("supervised select lanes: %v", err)
	}
	if supervised.JudgeLane.ModelID != "panel/supervised-judge" || supervised.JudgeLane.GateRole != "supervised_only" {
		t.Fatalf("supervised judge lane = %+v", supervised.JudgeLane)
	}
	unsupervised, err := SelectLanes(home, false, false, panelTestNow)
	if err == nil {
		t.Fatalf("unsupervised selected judge lane unexpectedly: %+v", unsupervised.JudgeLane)
	}
	var blocked ErrBlockedNoLane
	if !errors.As(err, &blocked) || blocked.Class != judgeCapabilityClass {
		t.Fatalf("unsupervised err = %v, want judge blocked", err)
	}
	if !omittedReason(unsupervised.OmittedLanes, "panel/supervised-judge", "supervised_only judge lane requires operator supervision") {
		t.Fatalf("unsupervised omitted lanes = %+v", unsupervised.OmittedLanes)
	}
	if !omittedReason(unsupervised.OmittedLanes, "panel/free-judge", "free lane may not judge") {
		t.Fatalf("free judge omission missing: %+v", unsupervised.OmittedLanes)
	}
}
