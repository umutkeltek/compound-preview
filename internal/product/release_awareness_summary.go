package product

// ReleaseAwarenessSummary is the compact evidence-only JSON view consumed by
// operator orientation. The product owner keeps the wire shape and copying rules. Status is one of: unobserved,
// invalid, stale, partial, conflicting, current.
type ReleaseAwarenessSummary struct {
	SchemaVersion    string                          `json:"schema_version,omitempty"`
	ProductID        string                          `json:"product_id"`
	Status           string                          `json:"status"`
	ContractVersion  string                          `json:"contract_version,omitempty"`
	ContractBuild    string                          `json:"contract_build,omitempty"`
	OutdatedContract bool                            `json:"outdated_contract,omitempty"`
	LatestObservedAt string                          `json:"latest_observed_at,omitempty"`
	LatestVersion    string                          `json:"latest_version,omitempty"`
	LatestChannel    string                          `json:"latest_channel,omitempty"`
	LatestPlatform   string                          `json:"latest_platform,omitempty"`
	LatestState      string                          `json:"latest_state,omitempty"`
	ObservationCount int                             `json:"observation_count"`
	Conflict         []string                        `json:"conflict,omitempty"`
	MissingEvidence  []string                        `json:"missing_evidence,omitempty"`
	Boundary         string                          `json:"boundary,omitempty"`
	NextAction       string                          `json:"next_action"`
	ChannelPlatforms []ReleaseChannelPlatformSummary `json:"channel_platforms,omitempty"`
	EvidenceRefs     []string                        `json:"evidence_refs,omitempty"`
}

// ReleaseChannelPlatformSummary preserves channel-local release proof in the compact view.
type ReleaseChannelPlatformSummary struct {
	Status         string   `json:"status,omitempty"`
	CurrentVersion string   `json:"current_version,omitempty"`
	Channel        string   `json:"channel"`
	Platform       string   `json:"platform"`
	LatestVersion  string   `json:"latest_version,omitempty"`
	LatestState    string   `json:"latest_state,omitempty"`
	LatestObserved string   `json:"latest_observed_at,omitempty"`
	StateSet       []string `json:"state_set,omitempty"`
	VersionSet     []string `json:"version_set,omitempty"`
	EvidenceRefs   []string `json:"evidence_refs,omitempty"`
}

// Summary returns the compact release-evidence view for operator orientation.
// A nil receiver remains nil, so callers can omit absent awareness. Slices are
// copied: changing the returned view cannot mutate the source evidence model.
func (aw *ReleaseAwareness) Summary() *ReleaseAwarenessSummary {
	if aw == nil {
		return nil
	}
	channels := make([]ReleaseChannelPlatformSummary, 0, len(aw.ChannelPlatforms))
	for _, view := range aw.ChannelPlatforms {
		channels = append(channels, ReleaseChannelPlatformSummary{
			Status:         view.Status,
			CurrentVersion: view.CurrentVersion,
			Channel:        view.Channel,
			Platform:       view.Platform,
			LatestVersion:  view.Latest.ObservedVersion,
			LatestState:    view.Latest.ObservedState,
			LatestObserved: view.Latest.ObservedAt,
			StateSet:       append([]string{}, view.StateSet...),
			VersionSet:     append([]string{}, view.VersionSet...),
			EvidenceRefs:   append([]string{}, view.EvidenceRefs...),
		})
	}
	return &ReleaseAwarenessSummary{
		SchemaVersion:    aw.SchemaVersion,
		ProductID:        aw.ProductID,
		Status:           aw.Status,
		ContractVersion:  aw.ContractVersion,
		ContractBuild:    aw.ContractBuild,
		OutdatedContract: aw.OutdatedContract,
		LatestObservedAt: aw.LatestObservedAt,
		LatestVersion:    aw.LatestVersion,
		LatestChannel:    aw.LatestChannel,
		LatestPlatform:   aw.LatestPlatform,
		LatestState:      aw.LatestState,
		ObservationCount: aw.ObservationCount,
		Conflict:         append([]string{}, aw.Conflict...),
		MissingEvidence:  append([]string{}, aw.MissingEvidence...),
		Boundary:         aw.Boundary,
		NextAction:       aw.NextAction,
		ChannelPlatforms: channels,
		EvidenceRefs:     append([]string{}, aw.EvidenceRefs...),
	}
}
