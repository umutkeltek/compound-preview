package routeevidence

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func laneManifest() ModelCapabilitiesManifest {
	return ModelCapabilitiesManifest{
		Version:           1,
		Purpose:           "test",
		Rules:             []string{"test"},
		CapabilityClasses: []string{"build-implement"},
		Models: map[string]ModelCapabilityEntry{
			"claude-fixture-primary": {
				Lane:              "claude-fixture-primary",
				Harness:           "claude_code",
				ProviderRoute:     "provider.example/model-primary",
				ProviderRouteKind: "direct_model",
				EffortDefault:     "high",
				Tier:              "premium",
				PacketCeiling:     "repo-scoped",
				CostClass:         "api-metered",
				ContextWindow:     1000000,
				Aliases:           []string{"model-primary"},
				Capabilities: map[string]CapabilityClaim{
					"build-implement": {Level: "capable", Evidence: "test"},
				},
			},
			"codex-fixture-economy": {
				Lane:              "codex-fixture-economy",
				Harness:           "codex",
				ProviderRoute:     "model-economy",
				ProviderRouteKind: "provider_class",
				EffortDefault:     "medium",
				Tier:              "cheap",
				PacketCeiling:     "multi-file",
				Capabilities: map[string]CapabilityClaim{
					"build-implement": {Level: "capable", Evidence: "test"},
				},
			},
			// Harness-less entry: a provider/model fact, not a launchable lane.
			"provider.example/model-reference": {
				Lane:          "provider.example/model-reference",
				ProviderRoute: "provider-reference",
				Tier:          "cheap",
				PacketCeiling: "multi-file",
				Capabilities: map[string]CapabilityClaim{
					"build-implement": {Level: "capable", Evidence: "test"},
				},
			},
		},
		RoutingDefaults: map[string][]string{"build-implement": {"claude-fixture-primary"}},
	}
}

func TestLaneProjectionCarriesModelAndEffortSeparately(t *testing.T) {
	got := BuildLaneProjection(laneManifest(), "sha256:abc", "")

	var primary *LaneProjectionEntry
	for i := range got.Lanes {
		if got.Lanes[i].ModelID == "claude-fixture-primary" {
			primary = &got.Lanes[i]
		}
	}
	if primary == nil {
		t.Fatalf("claude-fixture-primary missing from projection: %+v", got.Lanes)
	}
	// The whole point: model/route and effort are DISTINCT fields. If effort ever collapses
	// back into the lane name, this is the test that should fail.
	if primary.Route != "provider.example/model-primary" {
		t.Fatalf("route = %q", primary.Route)
	}
	if primary.ProviderRouteKind != "direct_model" {
		t.Fatalf("provider_route_kind = %q", primary.ProviderRouteKind)
	}
	if primary.NativeEffort != "high" {
		t.Fatalf("native_effort = %q", primary.NativeEffort)
	}
	if primary.Lane != "claude-fixture-primary" {
		t.Fatalf("lane name must not encode effort, got %q", primary.Lane)
	}
}

func TestLaneProjectionCanonicalizesProviderRouteKind(t *testing.T) {
	manifest := laneManifest()
	entry := manifest.Models["claude-fixture-primary"]
	entry.ProviderRouteKind = " direct_model "
	manifest.Models["claude-fixture-primary"] = entry

	got := BuildLaneProjection(manifest, "sha256:abc", "")
	for _, lane := range got.Lanes {
		if lane.ModelID == "claude-fixture-primary" {
			if lane.ProviderRouteKind != "direct_model" {
				t.Fatalf("provider_route_kind = %q, want canonical enum", lane.ProviderRouteKind)
			}
			return
		}
	}
	t.Fatalf("claude-fixture-primary missing from projection: %+v", got.Lanes)
}

// A lane with no harness cannot be launched; emitting it would invite the executor to
// materialize something unlaunchable.
func TestLaneProjectionSkipsHarnesslessEntries(t *testing.T) {
	got := BuildLaneProjection(laneManifest(), "sha256:abc", "")
	for _, lane := range got.Lanes {
		if lane.ModelID == "provider.example/model-reference" {
			t.Fatalf("harness-less provider fact must not project as a lane: %+v", lane)
		}
	}
	if got.LaneCount != 2 {
		t.Fatalf("want 2 launchable lanes, got %d", got.LaneCount)
	}
}

// The authority carries BOTH spellings today (claude-code on the native lane, claude_code on
// the profile lanes). A filter honoring only one returns an empty set, which reads as "no such
// lanes" rather than "you spelled it the other way".
func TestLaneProjectionHarnessFilterFoldsSpellings(t *testing.T) {
	for _, spelling := range []string{"claude_code", "claude-code", "Claude Code", "CLAUDE_CODE"} {
		got := BuildLaneProjection(laneManifest(), "sha256:abc", spelling)
		if got.LaneCount != 1 {
			t.Fatalf("filter %q: want 1 lane, got %d", spelling, got.LaneCount)
		}
		if got.Lanes[0].ModelID != "claude-fixture-primary" {
			t.Fatalf("filter %q selected %q", spelling, got.Lanes[0].ModelID)
		}
	}
}

// The ownership seam must be readable from the JSON alone — a boundary documented only in Go
// comments is a boundary the executor cannot see.
func TestLaneProjectionNamesExecutorOwnedFieldsAndEmitsNoneOfThem(t *testing.T) {
	got := BuildLaneProjection(laneManifest(), "sha256:abc", "")
	if len(got.ExecutorOwned) == 0 {
		t.Fatal("projection must name the executor-owned fields")
	}
	for _, want := range []string{"anthropic_tap_port", "credential_ref", "min_fallbacks", "capture_policy"} {
		found := false
		for _, have := range got.ExecutorOwned {
			if have == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("executor_owned missing %q: %v", want, got.ExecutorOwned)
		}
	}
	// Repeated per entry so a consumer processing one lane in isolation keeps the boundary.
	for _, lane := range got.Lanes {
		if len(lane.ExecutorOwned) != len(got.ExecutorOwned) {
			t.Fatalf("lane %q lost the ownership boundary", lane.Lane)
		}
	}
}

// Consumers diff and commit this output, so map iteration order must never reach it.
func TestLaneProjectionIsDeterministic(t *testing.T) {
	first := BuildLaneProjection(laneManifest(), "sha256:abc", "")
	for i := 0; i < 8; i++ {
		next := BuildLaneProjection(laneManifest(), "sha256:abc", "")
		for j := range first.Lanes {
			if first.Lanes[j].ModelID != next.Lanes[j].ModelID {
				t.Fatalf("run %d diverged at %d: %q vs %q", i, j, first.Lanes[j].ModelID, next.Lanes[j].ModelID)
			}
		}
	}
}

// REGRESSION: LoadModelCapabilitiesFromHome returns the authority PATH as its second value,
// not the revision. Stamping that into source_revision would make every projection look
// identical regardless of what the authority said — an unverifiable claim in the field whose
// entire job is verifiability.
func TestLaneProjectionFromHomeStampsRevisionNotPath(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(t.TempDir(), "model-capabilities.yaml")
	body := []byte(`version: 1
purpose: test
rules: [test]
capability_classes: [build-implement]
models:
  claude-fixture-primary:
    lane: claude-fixture-primary
    harness: claude_code
    provider_route: provider.example/model-primary
    resolution: {mode: ''}
    effort_default: high
    tier: premium
    packet_ceiling: repo-scoped
    capabilities:
      build-implement: {level: capable, evidence: test}
routing_defaults:
  build-implement: [claude-fixture-primary]
`)
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if _, err := ImportModelCapabilitiesYAML(home, src, time.Now()); err != nil {
		t.Fatalf("import: %v", err)
	}

	got, err := LaneProjectionFromHome(home, "")
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	if got.LaneCount != 1 {
		t.Fatalf("want 1 lane, got %d", got.LaneCount)
	}
	if len(got.SourceRevision) < 8 || got.SourceRevision[:7] != "sha256:" {
		t.Fatalf("source_revision must be a sha256 digest, got %q", got.SourceRevision)
	}
	if filepath.IsAbs(got.SourceRevision) {
		t.Fatalf("source_revision is a filesystem path, not a revision: %q", got.SourceRevision)
	}
}
