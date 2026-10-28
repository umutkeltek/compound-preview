// agent_lane_launch_profiles.go — lane IDENTITY from the launch-profile authority.
//
// Placement: the launch-profile owner publishes a non-secret projection that names compound the
// consumer and carries a revision per profile:
//
//	authority: {owner: "<profile owner>", compound_role: "consumer", ...}
//	profiles:  [{id, harness, provider_lane, requested_model, requested_effort, capture_mode, ...}]
//
// compound imports that projection and never authors a lane's model or effort. The overlay's
// hand-written lanes[] predates this surface: it could only describe a lane in prose, and the
// tuple's Model came from the ROLE's preferred model for the harness family. That minted
// confidently wrong identities — a provider-swapped lane claimed the base harness's flagship
// model while actually running a different provider's — which is worse than no node at all for
// the per-lane comparison this selection surface exists to support.
//
// The split introduced here, and the reason a role can no longer mint a model label:
//
//	IDENTITY    what a lane IS  — harness + profile id + real model + real effort — imported.
//	SUITABILITY what it is FOR  — capability class + tendencies — overlay-authored, attached
//	                              as edges onto the imported identity.
//
// Fail-closed by construction: a projection that does not name the expected owner, or a profile
// missing any identity component, is refused rather than imported under a default. A lane whose
// effort silently became "unknown" would collide with every other unknown-effort lane on the
// same harness, which is the same wrong-label failure in a new costume.
package capgraph

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// compoundConsumerRole is the role a launch-profile authority must assign compound. The check is
// on the RELATIONSHIP, not on a vendor name: this package stays owner-agnostic so a second
// profile owner needs no code change, and the governance property that actually matters is that
// the projection treats compound as a consumer rather than asserting authority over it.
const compoundConsumerRole = "consumer"

// LaunchProfileAuthority is compound's read of the served launch-profile projection.
type LaunchProfileAuthority struct {
	Authority         LaunchProfileAuthorityMeta `json:"authority"`
	AuthorityPath     string                     `json:"authority_path"`
	AuthorityRevision string                     `json:"authority_revision"`
	Profiles          []LaunchProfile            `json:"profiles"`
}

// LaunchProfileAuthorityMeta is the ownership header the projection carries about itself.
type LaunchProfileAuthorityMeta struct {
	Owner        string `json:"owner"`
	CompoundRole string `json:"compound_role"`
	Projection   string `json:"projection"`
}

// LaunchProfile is one launch profile. Field names mirror the served projection so a reader can
// diff this struct against the source without a translation table.
type LaunchProfile struct {
	ID              string `json:"id"`
	Harness         string `json:"harness"`
	ProviderLane    string `json:"provider_lane"`
	RequestedModel  string `json:"requested_model"`
	RequestedEffort string `json:"requested_effort"`
	CaptureMode     string `json:"capture_mode"`
	Route           string `json:"route"`
	Revision        string `json:"revision"`
}

// LaunchProfileTuple is the lane identity a profile denotes. The profile id — not a prose route
// description — is the Route component, because the id is what resolves back to a launchable
// thing; prose resolves to nothing.
func LaunchProfileTuple(p LaunchProfile) AgentLaneTuple {
	return AgentLaneTuple{
		Harness: strings.TrimSpace(p.Harness),
		Route:   strings.TrimSpace(p.ID),
		Model:   strings.TrimSpace(p.RequestedModel),
		Effort:  strings.TrimSpace(p.RequestedEffort),
	}
}

// loadLaunchProfiles reads and validates the projection. A missing file is (_, false, nil): a
// home with no launch-profile authority is a clean seam, matching loadAgentLaneProfiles.
func loadLaunchProfiles(path string) (LaunchProfileAuthority, bool, error) {
	if strings.TrimSpace(path) == "" {
		return LaunchProfileAuthority{}, false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return LaunchProfileAuthority{}, false, nil
		}
		return LaunchProfileAuthority{}, false, err
	}
	var auth LaunchProfileAuthority
	if err := json.Unmarshal(data, &auth); err != nil {
		return LaunchProfileAuthority{}, false, fmt.Errorf("parse launch-profile authority %s: %w", path, err)
	}
	if owner := strings.TrimSpace(auth.Authority.Owner); owner == "" {
		return LaunchProfileAuthority{}, false, fmt.Errorf(
			"launch-profile authority %s names no owner: an authority that will not identify itself is not importable", path)
	}
	if role := strings.TrimSpace(auth.Authority.CompoundRole); role != compoundConsumerRole {
		return LaunchProfileAuthority{}, false, fmt.Errorf(
			"launch-profile authority %s assigns compound role %q, want %q", path, role, compoundConsumerRole)
	}
	seen := map[string]string{}
	for _, p := range auth.Profiles {
		tuple := LaunchProfileTuple(p)
		for field, value := range map[string]string{
			"id":               tuple.Route,
			"harness":          tuple.Harness,
			"requested_model":  tuple.Model,
			"requested_effort": tuple.Effort,
		} {
			if value == "" {
				return LaunchProfileAuthority{}, false, fmt.Errorf(
					"launch profile %q missing %s: identity components are never defaulted", p.ID, field)
			}
		}
		nodeID := AgentLaneNodeID(tuple)
		if prior, ok := seen[nodeID]; ok {
			return LaunchProfileAuthority{}, false, fmt.Errorf(
				"launch profiles %q and %q collapse to one lane identity %s", prior, p.ID, nodeID)
		}
		seen[nodeID] = p.ID
	}
	return auth, true, nil
}

// appendLaunchProfileLanes mints one identity node per launch profile and attaches the overlay's
// role-authored suitability to it. Roles contribute capability class and tendencies only; the
// model and effort on the node are the profile's own, so adding a role can never restate what a
// lane runs.
func appendLaunchProfileLanes(
	nodesByID map[string]Node,
	edgesByKey map[string]Edge,
	roles []agentLaneProfileRole,
	profiles []LaunchProfile,
	now time.Time,
) error {
	sorted := append([]LaunchProfile(nil), profiles...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	stamp := now.UTC().Format(time.RFC3339)

	for _, profile := range sorted {
		tuple := LaunchProfileTuple(profile)
		nodeID := AgentLaneNodeID(tuple)
		node := nodesByID[nodeID]
		if node.ID == "" {
			node = Node{
				ID:           nodeID,
				Kind:         agentLaneNodeKind,
				Name:         agentLaneName(tuple),
				Weight:       0.25,
				EvidenceOnly: true,
			}
		}
		// The launch profile is a lane the harness can be started on, so it answers the same
		// selection question as an overlay lane and shares the lane vocabulary.
		lane := agentLaneProfileLane{
			ID:      profile.ID,
			Harness: profile.Harness,
			Route:   profile.ID,
			Note:    profile.ProviderLane,
		}
		for _, role := range roles {
			if strings.TrimSpace(role.Family) != "lane" {
				continue
			}
			capabilityClass := strings.TrimSpace(role.CapabilityClass)
			if capabilityClass == "" {
				return fmt.Errorf("agent-lane role %q missing capability_class", role.ID)
			}
			matched := false
			for _, emit := range cleanStrings(role.Emit) {
				if agentLaneEmitMatches(lane, emit) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			node.Triggers = mergeAgentLaneTriggers(node.Triggers, tuple, role, lane)
			addAgentLaneEdge(edgesByKey, Edge{
				Kind: EdCapableOf, From: nodeID,
				To: "task-class:" + slugSkill(capabilityClass), Confidence: 1, CreatedAt: stamp,
			})
			for _, tendency := range cleanStrings(role.Tendencies) {
				if err := validateAgentLaneTendency(tendency); err != nil {
					return err
				}
				addAgentLaneEdge(edgesByKey, Edge{
					Kind: EdTendsTo, From: nodeID,
					To: "tendency:" + tendency, Confidence: 1, CreatedAt: stamp,
				})
			}
			for _, ref := range cleanStrings(role.ValidatedBy) {
				addAgentLaneEdge(edgesByKey, Edge{
					Kind: EdValidatedBy, From: nodeID, To: ref, Confidence: 1, CreatedAt: stamp,
				})
			}
		}
		nodesByID[nodeID] = node
	}
	return nil
}
