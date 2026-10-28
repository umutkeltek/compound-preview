package routeevidence

// Lane projection — Compound declares a lane's meaning; an executor materializes its plumbing.
//
// Lane identity, model route, and effort are orthogonal authority fields. This projection keeps
// them separate and emits only the Compound-owned half of a launch profile. Transport, ports,
// credentials, fallback topology, and capture configuration remain executor-owned and are named
// explicitly so consumers do not infer them.

import (
	"fmt"
	"sort"
	"strings"
)

const (
	// LaneProjectionSchema tags the emitted document.
	LaneProjectionSchema = "compound.lane_projection.v0.1"
)

// executorOwnedFields is the fixed list of launch-profile fields Compound deliberately does
// NOT emit. It ships in every projection so a consumer reading only the JSON still learns the
// boundary — a seam documented solely in Go comments is a seam the executor cannot see.
var executorOwnedFields = []string{
	"anthropic_tap_port",
	"capture_policy",
	"credential_ref",
	"min_fallbacks",
	"transport",
	"wire_api",
}

// LaneProjectionEntry is one declared lane, split by ownership.
type LaneProjectionEntry struct {
	Lane    string `json:"lane"`
	ModelID string `json:"model_id"`
	Harness string `json:"harness"`

	// Compound-owned meaning.
	Route             string   `json:"route,omitempty"`
	ProviderRouteKind string   `json:"provider_route_kind,omitempty"`
	NativeEffort      string   `json:"native_effort,omitempty"`
	Aliases           []string `json:"aliases,omitempty"`
	Tier              string   `json:"tier,omitempty"`
	CostClass         string   `json:"cost_class,omitempty"`
	PacketCeiling     string   `json:"packet_ceiling,omitempty"`
	ContextWindow     int      `json:"context_window,omitempty"`
	Notes             string   `json:"notes,omitempty"`

	// ExecutorOwned repeats the boundary per entry so a consumer processing one lane in
	// isolation cannot lose it.
	ExecutorOwned []string `json:"executor_owned"`
}

// LaneProjection is the emitted document.
type LaneProjection struct {
	SchemaVersion  string                `json:"schema_version"`
	AuthorityRef   string                `json:"authority_ref"`
	SourceRevision string                `json:"source_revision,omitempty"`
	HarnessFilter  string                `json:"harness_filter,omitempty"`
	LaneCount      int                   `json:"lane_count"`
	ExecutorOwned  []string              `json:"executor_owned"`
	Lanes          []LaneProjectionEntry `json:"lanes"`
}

// BuildLaneProjection renders the Compound-owned half of every declared lane.
//
// harnessFilter is optional; empty means all harnesses. Matching is normalized so
// `claude_code`, `claude-code`, and `Claude Code` all select the same lanes — the authority
// itself carries both spellings today (`claude-code` on the native lane, `claude_code` on the
// profile lanes), and a filter that honored only one would silently return an empty set, which
// reads as "no such lanes" rather than "you spelled it the other way".
func BuildLaneProjection(manifest ModelCapabilitiesManifest, sourceRevision, harnessFilter string) LaneProjection {
	want := normalizeHarnessKey(harnessFilter)

	entries := make([]LaneProjectionEntry, 0, len(manifest.Models))
	for modelID, entry := range manifest.Models {
		// A lane with no harness is a provider/model fact, not a launchable lane; emitting it
		// would invite the executor to materialize something that cannot be launched.
		if strings.TrimSpace(entry.Harness) == "" {
			continue
		}
		if want != "" && normalizeHarnessKey(entry.Harness) != want {
			continue
		}
		lane := strings.TrimSpace(entry.Lane)
		if lane == "" {
			lane = modelID
		}
		entries = append(entries, LaneProjectionEntry{
			Lane:              lane,
			ModelID:           modelID,
			Harness:           entry.Harness,
			Route:             entry.ProviderRoute,
			ProviderRouteKind: strings.TrimSpace(entry.ProviderRouteKind),
			NativeEffort:      entry.EffortDefault,
			Aliases:           append([]string(nil), entry.Aliases...),
			Tier:              entry.Tier,
			CostClass:         entry.CostClass,
			PacketCeiling:     entry.PacketCeiling,
			ContextWindow:     entry.ContextWindow,
			Notes:             entry.Notes,
			ExecutorOwned:     append([]string(nil), executorOwnedFields...),
		})
	}

	// Deterministic order: the projection is diffed and committed by consumers, so map
	// iteration order must never reach the output.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Harness != entries[j].Harness {
			return entries[i].Harness < entries[j].Harness
		}
		return entries[i].ModelID < entries[j].ModelID
	})

	return LaneProjection{
		SchemaVersion:  LaneProjectionSchema,
		AuthorityRef:   ModelCapabilitiesAuthorityRef,
		SourceRevision: sourceRevision,
		HarnessFilter:  strings.TrimSpace(harnessFilter),
		LaneCount:      len(entries),
		ExecutorOwned:  append([]string(nil), executorOwnedFields...),
		Lanes:          entries,
	}
}

// LaneProjectionFromHome loads the installed authority and projects it.
//
// Uses the snapshot loader rather than LoadModelCapabilitiesFromHome: that wrapper returns the
// authority PATH as its second value, not the revision, and stamping a path into
// `source_revision` would make every projection look identical no matter what the authority
// said — the exact class of un-verifiable claim this projection exists to avoid.
func LaneProjectionFromHome(homePath, harnessFilter string) (LaneProjection, error) {
	manifest, _, revision, err := loadModelCapabilitiesSnapshot(homePath)
	if err != nil {
		return LaneProjection{}, fmt.Errorf("load model capabilities authority: %w", err)
	}
	return BuildLaneProjection(manifest, revision, harnessFilter), nil
}

// normalizeHarnessKey folds the spellings that mean the same harness.
func normalizeHarnessKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "_")
	return strings.ReplaceAll(s, " ", "_")
}
