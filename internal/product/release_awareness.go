package product

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"compound/internal/sourceobs"
)

// ReleaseAwarenessSchemaVersion is the JSON schema for the additive
// release-awareness surface included in ProductLoop. It is
// evidence-only and never auto-claims current-live, never schedules
// release execution, and never promotes memory.
const ReleaseAwarenessSchemaVersion = "compound.product_release_awareness.v0.1"

// ReleaseAwarenessStatus is the closed enum for the per-product
// release-awareness summary. Anything outside this list is a programming
// error; callers should treat unknown statuses as "unknown".
var ReleaseAwarenessStatus = []string{
	"unobserved",  // no external release observation has been admitted for this product
	"invalid",     // incomplete inventory or observations that failed strict re-validation
	"stale",       // observations exist but every channel is older than StaleAfter
	"partial",     // some channels observed, others not, no same-channel+platform conflict
	"conflicting", // same channel+platform has multiple equally-current incompatible observations
	"current",     // at least one observation reports a live version with valid provenance
}

// ReleaseChannelPlatformView is the per (channel, platform) slice of
// release awareness for one product. Different channels/platforms are
// not contradictions: e.g. an App Store Connect candidate entry for iOS
// and a direct-mac live dmg are two independent views, never a conflict.
type ReleaseChannelPlatformView struct {
	Status string `json:"status"`

	Channel        string                       `json:"channel"`
	Platform       string                       `json:"platform"`
	Latest         sourceobs.ReleaseObservation `json:"latest"`
	StateSet       []string                     `json:"state_set,omitempty"`
	VersionSet     []string                     `json:"version_set,omitempty"`
	CurrentVersion string                       `json:"current_version,omitempty"`
	EvidenceRefs   []string                     `json:"evidence_refs,omitempty"`
	ObservationIDs []string                     `json:"observation_ids,omitempty"`
}

// ReleaseAwareness is the additive evidence-only per-product release
// reconciliation view. It surfaces candidate/submitted/live observations
// per channel+platform with provenance, never auto-claiming current-live
// or contradicting fresh metrics. ContractVersion is the recalled
// ReleaseIdentity from the product registry (adjudicated identity), not
// the deployed version; the gap between ContractVersion and the latest
// observed version is reported as OutdatedContract, never as a fix.
//
// NextAction is a bounded safe move: import more observations, resolve
// a conflict through operator review, or wait — never "ship" or
// "promote to live".
type ReleaseAwareness struct {
	SchemaVersion    string                       `json:"schema_version"`
	ProductID        string                       `json:"product_id"`
	Status           string                       `json:"status"`
	ContractVersion  string                       `json:"contract_version,omitempty"`
	ContractBuild    string                       `json:"contract_build,omitempty"`
	OutdatedContract bool                         `json:"outdated_contract,omitempty"`
	LatestObservedAt string                       `json:"latest_observed_at,omitempty"`
	LatestVersion    string                       `json:"latest_version,omitempty"`
	LatestChannel    string                       `json:"latest_channel,omitempty"`
	LatestPlatform   string                       `json:"latest_platform,omitempty"`
	LatestState      string                       `json:"latest_state,omitempty"`
	ChannelPlatforms []ReleaseChannelPlatformView `json:"channel_platforms,omitempty"`
	ObservationCount int                          `json:"observation_count"`
	Conflict         []string                     `json:"conflict,omitempty"`
	MissingEvidence  []string                     `json:"missing_evidence,omitempty"`
	Boundary         string                       `json:"boundary"`
	NextAction       string                       `json:"next_action"`
	EvidenceRefs     []string                     `json:"evidence_refs,omitempty"`
}

// ReleaseAwarenessOptions controls the per-product aggregation. Now is
// injected (Law 10 — tests must not compare wall-clock against fixed
// fixture dates). StaleAfter is the freshness window; defaults to 7
// days, matching the product-loop default.
type ReleaseAwarenessOptions struct {
	Now        time.Time
	StaleAfter time.Duration
}

// BuildReleaseAwareness reads one evidence snapshot for a direct caller.
// Portfolio projections load once and call the same pure reducer below.
// Observation time orders a channel's history; only disagreeing observations
// at the same newest instant conflict. Freshness is checked per channel,
// never borrowed from a newer observation on another channel.
func BuildReleaseAwareness(homePath, productID string, identity ReleaseIdentity, opts ReleaseAwarenessOptions) (ReleaseAwareness, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	report, err := sourceobs.LoadReleaseObservationsReport(homePath, []string{productID}, opts.Now)
	if err != nil {
		return ReleaseAwareness{}, err
	}
	return buildReleaseAwareness(productID, identity, opts, report), nil
}

type releaseChannelKey struct{ channel, platform string }

func buildReleaseAwareness(productID string, identity ReleaseIdentity, opts ReleaseAwarenessOptions, report sourceobs.ReleaseObservationsReport) ReleaseAwareness {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	staleAfter := opts.StaleAfter
	if staleAfter <= 0 {
		staleAfter = 7 * 24 * time.Hour
	}
	out := ReleaseAwareness{
		SchemaVersion: ReleaseAwarenessSchemaVersion, ProductID: productID, Status: "unobserved",
		ContractVersion: strings.TrimSpace(identity.Version), ContractBuild: strings.TrimSpace(identity.Build),
		Boundary:         "evidence-only; never auto-claims current-live, never schedules release execution, never promotes memory",
		NextAction:       "import typed release observations through compound source observe or wait for governed intake",
		ChannelPlatforms: []ReleaseChannelPlatformView{}, Conflict: []string{},
		MissingEvidence: []string{}, EvidenceRefs: []string{},
	}
	if strings.TrimSpace(productID) == "" {
		out.Status = "invalid"
		out.MissingEvidence = append(out.MissingEvidence, "product_id required")
		return out
	}
	invalidChannels := map[releaseChannelKey]bool{}
	unknownInvalid := false
	for _, issue := range report.Issues {
		if !issue.AppliesTo(productID) {
			continue
		}
		reason := issue.Reason
		if issue.CandidateID != "" {
			reason = issue.CandidateID + ": " + reason
			out.EvidenceRefs = append(out.EvidenceRefs, "candidate:"+issue.CandidateID)
		}
		if issue.SourceObservationID != "" {
			out.EvidenceRefs = append(out.EvidenceRefs, "source-observation:"+issue.SourceObservationID)
		}
		out.MissingEvidence = append(out.MissingEvidence, reason)
		if issue.Channel == "" || issue.Platform == "" {
			unknownInvalid = true
		} else {
			invalidChannels[releaseChannelKey{issue.Channel, issue.Platform}] = true
		}
	}
	groups := map[releaseChannelKey][]sourceobs.ReleaseObservationRecord{}
	var keys []releaseChannelKey
	for _, record := range report.Records {
		if record.Observation.ProductID != productID {
			continue
		}
		key := releaseChannelKey{record.Observation.Channel, record.Observation.Platform}
		if _, ok := groups[key]; !ok {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], record)
		out.ObservationCount++
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].channel != keys[j].channel {
			return keys[i].channel < keys[j].channel
		}
		return keys[i].platform < keys[j].platform
	})
	hasFresh, hasCurrent := false, false
	for _, key := range keys {
		records := groups[key]
		latest := records[0]
		for _, record := range records[1:] {
			at, bt := parseTimeOrZero(record.Observation.ObservedAt), parseTimeOrZero(latest.Observation.ObservedAt)
			if at.After(bt) || (at.Equal(bt) && record.CandidateID < latest.CandidateID) {
				latest = record
			}
		}
		latestTime := parseTimeOrZero(latest.Observation.ObservedAt)
		equalCurrent := map[[3]string]bool{}
		view := ReleaseChannelPlatformView{Channel: key.channel, Platform: key.platform, Latest: latest.Observation}
		for _, record := range records {
			obs := record.Observation
			if parseTimeOrZero(obs.ObservedAt).Equal(latestTime) {
				equalCurrent[[3]string{obs.ObservedVersion, obs.ObservedBuild, obs.ObservedState}] = true
			}
			view.StateSet = append(view.StateSet, obs.ObservedState)
			view.VersionSet = append(view.VersionSet, obs.ObservedVersion)
			view.ObservationIDs = append(view.ObservationIDs, record.SourceObservationID)
			view.EvidenceRefs = append(view.EvidenceRefs, record.EvidenceRefs...)
			view.EvidenceRefs = append(view.EvidenceRefs, obs.EvidenceRef, obs.SourceRevision, record.SourceRef, "candidate:"+record.CandidateID, "source-observation:"+record.SourceObservationID)
		}
		fresh := !isStale(latest.Observation.ObservedAt, now, staleAfter)
		hasFresh = hasFresh || fresh
		switch {
		case unknownInvalid || invalidChannels[key]:
			view.Status = "invalid"
		case len(equalCurrent) > 1:
			view.Status = "conflicting"
			for sig := range equalCurrent {
				out.Conflict = append(out.Conflict, fmt.Sprintf("%s/%s:version=%q build=%q state=%q", key.channel, key.platform, sig[0], sig[1], sig[2]))
			}
		case !fresh:
			view.Status = "stale"
		case latest.Observation.ObservedState == "live":
			view.Status = "current"
			view.CurrentVersion = latest.Observation.ObservedVersion
			hasCurrent = true
			versionDiffers := out.ContractVersion != "" && !sameReleaseVersion(out.ContractVersion, view.CurrentVersion)
			buildDiffers := out.ContractBuild != "" && strings.TrimSpace(latest.Observation.ObservedBuild) != "" && out.ContractBuild != strings.TrimSpace(latest.Observation.ObservedBuild)
			if versionDiffers || buildDiffers {
				out.OutdatedContract = true
			}
		default:
			view.Status = "partial"
		}
		view.StateSet = sortedUnique(view.StateSet)
		view.VersionSet = sortedUnique(view.VersionSet)
		view.ObservationIDs = sortedUnique(view.ObservationIDs)
		view.EvidenceRefs = sortedUnique(view.EvidenceRefs)
		out.EvidenceRefs = append(out.EvidenceRefs, view.EvidenceRefs...)
		out.ChannelPlatforms = append(out.ChannelPlatforms, view)
		if out.LatestObservedAt == "" || latestTime.After(parseTimeOrZero(out.LatestObservedAt)) {
			out.LatestObservedAt, out.LatestVersion = latest.Observation.ObservedAt, latest.Observation.ObservedVersion
			out.LatestChannel, out.LatestPlatform, out.LatestState = key.channel, key.platform, latest.Observation.ObservedState
		}
	}
	switch {
	case len(out.MissingEvidence) > 0:
		out.Status = "invalid"
		out.NextAction = "review incomplete inventory or invalid release evidence through governed repair or intake; do not infer current-live from missing data"
	case len(out.Conflict) > 0:
		out.Status = "conflicting"
		out.NextAction = "operator-reviewed reconcile required: same channel/platform and observation time disagree; do not auto-claim live"
	case out.ObservationCount == 0:
	case !hasFresh:
		out.Status = "stale"
		out.NextAction = "re-import current release observations before claiming release awareness"
	case hasCurrent:
		out.Status = "current"
		if out.OutdatedContract {
			out.NextAction = "fresh live evidence differs from recalled contract identity; review the mismatch without automatically changing memory"
		} else {
			out.NextAction = "evidence-only; retain provenance and do not infer causality or schedule release execution"
		}
	default:
		out.Status = "partial"
		out.NextAction = "no channel has fresh unambiguous live evidence; observe the live surface through governed intake"
	}
	out.EvidenceRefs = sortedUnique(out.EvidenceRefs)
	out.Conflict = sortedUnique(out.Conflict)
	out.MissingEvidence = sortedUnique(out.MissingEvidence)
	return out
}

func isStale(observedAt string, now time.Time, staleAfter time.Duration) bool {
	observedAt = strings.TrimSpace(observedAt)
	if observedAt == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, observedAt)
	if err != nil {
		return true
	}
	return now.UTC().Sub(t) > staleAfter
}

func parseTimeOrZero(s string) time.Time {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}
	}
	return t
}

// sameReleaseVersion is the contract-side equality check: normalize the
// version string (trim, lowercase) and compare. Build numbers are
// compared separately by callers when needed.
func sameReleaseVersion(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}
