package capgraph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The overlay below authors a role whose preferred model for the "claude" family is "opus".
// A provider-swapped lane binds to that role for SUITABILITY, and the whole point of the
// launch-profile import is that binding must not restate what the lane RUNS.
const laneTestOverlay = `{
  "roles": [
    {
      "id": "executor",
      "family": "lane",
      "capability_class": "build-implement",
      "quality_tier": "balanced",
      "emit": ["claude"],
      "models": {"claude": "opus"}
    }
  ],
  "lanes": []
}`

func writeLaneFixtures(t *testing.T, profiles any) (registry, overlay, profilesPath string) {
	t.Helper()
	dir := t.TempDir()
	registry = filepath.Join(dir, "registry.ts")
	overlay = filepath.Join(dir, "overlay.json")
	profilesPath = filepath.Join(dir, "launch-profiles.json")
	if err := os.WriteFile(registry, []byte("export const registry = {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overlay, []byte(laneTestOverlay), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(profiles)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profilesPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return registry, overlay, profilesPath
}

func laneAuthority(profiles ...LaunchProfile) LaunchProfileAuthority {
	return LaunchProfileAuthority{
		Authority:         LaunchProfileAuthorityMeta{Owner: "example-profile-owner", CompoundRole: "consumer"},
		AuthorityRevision: "sha256:fixture",
		Profiles:          profiles,
	}
}

// TestLaunchProfileLaneKeepsProviderModel is the regression that would have caught the shipped
// bug: a provider-swapped lane rendered with the ROLE's flagship model instead of its own, so
// every per-lane comparison ran against a mislabeled model.
func TestLaunchProfileLaneKeepsProviderModel(t *testing.T) {
	registry, overlay, profiles := writeLaneFixtures(t, laneAuthority(LaunchProfile{
		ID:              "claude-minimax",
		Harness:         "claude-code",
		ProviderLane:    "minimax-coding",
		RequestedModel:  "minimax/MiniMax-M3",
		RequestedEffort: "xhigh",
		CaptureMode:     "segmented_full_wire",
	}))

	seed, ok, err := loadAgentLaneProfiles(registry, overlay, profiles, time.Unix(0, 0).UTC())
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}

	want := AgentLaneNodeID(AgentLaneTuple{
		Harness: "claude-code", Route: "claude-minimax",
		Model: "minimax/MiniMax-M3", Effort: "xhigh",
	})
	var got *Node
	for i := range seed.nodes {
		if seed.nodes[i].ID == want {
			got = &seed.nodes[i]
		}
		// The role's model must not leak into any identity minted for this profile.
		if strings.Contains(seed.nodes[i].ID, "opus") {
			t.Errorf("role model leaked into lane identity: %s", seed.nodes[i].ID)
		}
	}
	if got == nil {
		ids := make([]string, 0, len(seed.nodes))
		for _, n := range seed.nodes {
			ids = append(ids, n.ID)
		}
		t.Fatalf("missing lane identity %s; got %v", want, ids)
	}

	// Suitability still arrives — the role binds by harness family and contributes the task
	// class, which is the half that legitimately comes from the overlay.
	wantEdge := "task-class:" + slugSkill("build-implement")
	found := false
	for _, e := range seed.edges {
		if e.From == want && e.Kind == EdCapableOf && e.To == wantEdge {
			found = true
		}
	}
	if !found {
		t.Errorf("lane %s never received its role capability edge %s", want, wantEdge)
	}
}

// A blank identity component must refuse the import. Defaulting effort to "unknown" would
// collapse every such lane onto one id — the same wrong-label failure in a new costume.
func TestLaunchProfileRefusesBlankIdentityComponent(t *testing.T) {
	registry, overlay, profiles := writeLaneFixtures(t, laneAuthority(LaunchProfile{
		ID:             "claude-minimax",
		Harness:        "claude-code",
		RequestedModel: "minimax/MiniMax-M3",
		// RequestedEffort deliberately absent.
	}))
	if _, _, err := loadAgentLaneProfiles(registry, overlay, profiles, time.Unix(0, 0).UTC()); err == nil {
		t.Fatal("expected refusal for a profile missing requested_effort")
	}
}

// An authority that will not identify itself is refused: provenance with no owner is not
// provenance, and the source ref would record an anonymous claim as if it were attributable.
func TestLaunchProfileRefusesAnonymousAuthority(t *testing.T) {
	auth := laneAuthority(LaunchProfile{
		ID: "lane-a", Harness: "claude-code",
		RequestedModel: "vendor/model-a", RequestedEffort: "xhigh",
	})
	auth.Authority.Owner = ""
	registry, overlay, profiles := writeLaneFixtures(t, auth)
	if _, _, err := loadAgentLaneProfiles(registry, overlay, profiles, time.Unix(0, 0).UTC()); err == nil {
		t.Fatal("expected refusal for an authority that names no owner")
	}
}

// The relationship is the gate: a projection that casts compound as the OWNER rather than the
// consumer is asserting authority over compound, and importing it would invert the ownership
// this whole surface exists to keep straight.
func TestLaunchProfileRefusesAuthorityAssertingOverCompound(t *testing.T) {
	auth := laneAuthority(LaunchProfile{
		ID: "lane-a", Harness: "claude-code",
		RequestedModel: "vendor/model-a", RequestedEffort: "xhigh",
	})
	auth.Authority.CompoundRole = "owner"
	registry, overlay, profiles := writeLaneFixtures(t, auth)
	if _, _, err := loadAgentLaneProfiles(registry, overlay, profiles, time.Unix(0, 0).UTC()); err == nil {
		t.Fatal("expected refusal for a projection asserting authority over compound")
	}
}

// Two profiles that collapse to one identity must fail loudly: silently merging them would hide
// a real lane behind another and make its evidence unattributable.
func TestLaunchProfileRefusesCollidingIdentities(t *testing.T) {
	registry, overlay, profiles := writeLaneFixtures(t, laneAuthority(
		LaunchProfile{ID: "dup", Harness: "claude-code", RequestedModel: "m", RequestedEffort: "xhigh"},
		LaunchProfile{ID: "dup", Harness: "claude-code", RequestedModel: "m", RequestedEffort: "xhigh"},
	))
	if _, _, err := loadAgentLaneProfiles(registry, overlay, profiles, time.Unix(0, 0).UTC()); err == nil {
		t.Fatal("expected refusal for colliding lane identities")
	}
}

// Absent launch-profile authority is a clean seam, not a failure: overlay-only import still works.
func TestLaunchProfilesAbsentIsNotAnError(t *testing.T) {
	registry, overlay, _ := writeLaneFixtures(t, laneAuthority())
	if _, ok, err := loadAgentLaneProfiles(registry, overlay, "", time.Unix(0, 0).UTC()); err != nil || !ok {
		t.Fatalf("overlay-only load: ok=%v err=%v", ok, err)
	}
}
