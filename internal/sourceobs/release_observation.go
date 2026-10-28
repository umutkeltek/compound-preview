package sourceobs

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"compound/internal/candidates"
)

// ReleaseObservationSchemaVersion is the schema for the typed external
// release-state payload carried by a SourceObservation. The payload is
// additive and evidence-only; it never promotes to release planning, never
// mutates product memory, and never authorizes release execution.
const ReleaseObservationSchemaVersion = "compound.source_release_observation.v0.1"

// ReleaseObservationStates is the closed enum for the observed release
// state. Anything outside this list fails Validate and is rejected by
// intake, so projection cannot promote ambiguity as a live claim.
var ReleaseObservationStates = []string{
	"candidate",
	"submitted",
	"preparing",
	"live",
	"unknown",
}

// ReleaseObservation is the minimal typed payload for one external
// release-state observation. It is additive: a SourceObservation without
// this field preserves its prior behavior. Recalled contract identity
// (from Product.ReleaseIdentity) and these observations are distinct
// sources; the carrier keeps both with their own provenance.
//
// Different channels/platforms are not contradictions: e.g. an App Store
// Connect candidate entry for iOS and a direct-mac live dmg are two
// independent views. Same channel+platform with equally-current
// incompatible observations remains unresolved (Status="conflicting"),
// never silently overwritten.
type ReleaseObservation struct {
	ProductID       string `json:"product_id" yaml:"product_id"`
	Channel         string `json:"channel" yaml:"channel"`
	Platform        string `json:"platform" yaml:"platform"`
	ObservedVersion string `json:"observed_version" yaml:"observed_version"`
	ObservedBuild   string `json:"observed_build,omitempty" yaml:"observed_build,omitempty"`
	ObservedState   string `json:"observed_state" yaml:"observed_state"`
	ObservedAt      string `json:"observed_at" yaml:"observed_at"`
	SourceRevision  string `json:"source_revision" yaml:"source_revision"`
	EvidenceRef     string `json:"evidence_ref" yaml:"evidence_ref"`
}

// releaseObservationObjectFamily is the synthetic ObjectFamily marker the
// aggregation uses to filter a single source-observation list scan. It is
// not a candidate kind; it is the carrier's namespace inside MapsTo.
const releaseObservationObjectFamily = "release_observation"

// releaseObservationCandidateKind is the candidate kind the carrier
// writes through the existing intake door. route_observation fits
// because release-state observation is evidence about an external
// route to a live surface (vs. promoted memory).
const releaseObservationCandidateKind = candidates.RouteObservation

// ValidateReleaseObservation is the strict-contract check for the typed
// payload. It rejects missing required fields, malformed timestamps,
// future timestamps (clock injection is the falsifier), and states
// outside the closed enum. It does NOT decide whether the observation is
// current-live; that is the aggregator's job.
func ValidateReleaseObservation(obs ReleaseObservation, now time.Time) error {
	if strings.TrimSpace(obs.ProductID) == "" {
		return fmt.Errorf("release observation product_id required")
	}
	if strings.TrimSpace(obs.Channel) == "" {
		return fmt.Errorf("release observation %s channel required", obs.ProductID)
	}
	if strings.TrimSpace(obs.Platform) == "" {
		return fmt.Errorf("release observation %s/%s platform required", obs.ProductID, obs.Channel)
	}
	if strings.TrimSpace(obs.ObservedVersion) == "" {
		return fmt.Errorf("release observation %s/%s/%s observed_version required", obs.ProductID, obs.Channel, obs.Platform)
	}
	if !oneOfReleaseState(obs.ObservedState) {
		return fmt.Errorf("release observation %s/%s/%s observed_state %q not in %v", obs.ProductID, obs.Channel, obs.Platform, obs.ObservedState, ReleaseObservationStates)
	}
	if strings.TrimSpace(obs.ObservedAt) == "" {
		return fmt.Errorf("release observation %s/%s/%s observed_at required", obs.ProductID, obs.Channel, obs.Platform)
	}
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(obs.ObservedAt))
	if err != nil {
		return fmt.Errorf("release observation %s/%s/%s observed_at %q invalid RFC3339: %w", obs.ProductID, obs.Channel, obs.Platform, obs.ObservedAt, err)
	}
	if now.IsZero() {
		now = time.Now()
	}
	if parsed.After(now.UTC().Add(time.Minute)) {
		// tolerate a 1-minute clock-skew margin; anything beyond is rejected.
		return fmt.Errorf("release observation %s/%s/%s observed_at %q is in the future (now=%s)", obs.ProductID, obs.Channel, obs.Platform, obs.ObservedAt, now.UTC().Format(time.RFC3339))
	}
	if strings.TrimSpace(obs.SourceRevision) == "" {
		return fmt.Errorf("release observation %s/%s/%s source_revision required", obs.ProductID, obs.Channel, obs.Platform)
	}
	if strings.TrimSpace(obs.EvidenceRef) == "" {
		return fmt.Errorf("release observation %s/%s/%s evidence_ref required", obs.ProductID, obs.Channel, obs.Platform)
	}
	return nil
}

// FromReleaseObservation wraps a typed release observation into the
// generic SourceObservation contract so it can enter through the
// existing sourceobs evidence door. The SourceRef path is a deterministic
// synthetic key. PromoteAllowed is false: this is no-promote evidence,
// never memory authority. The typed payload survives candidate persistence
// and list projection directly; evidence refs remain provenance, not a
// second encoding of the release fields.
func FromReleaseObservation(obs ReleaseObservation, now time.Time) (SourceObservation, error) {
	if now.IsZero() {
		now = time.Now()
	}
	if err := ValidateReleaseObservation(obs, now); err != nil {
		return SourceObservation{}, err
	}
	identityBytes, err := json.Marshal(obs)
	if err != nil {
		return SourceObservation{}, fmt.Errorf("release identity: %w", err)
	}
	path := releaseObservationPath(obs)
	return SourceObservation{
		SchemaVersion: SchemaVersion,
		ID:            idFor("srcobs_", "release_observation", string(identityBytes)),
		AdapterID:     "compound.release-observation",
		SourceSystem:  "external",
		ObservedAt:    obs.ObservedAt,
		SourceRef: SourceRef{
			Path:        path,
			Kind:        "release_observation",
			Owner:       firstNonEmpty(obs.ProductID, "release_observation"),
			SourceClass: "release_observation",
			Revision:    strings.TrimSpace(obs.SourceRevision),
		},
		GeneratedView: GeneratedView{
			ViewKind:    "release_observation",
			Consumer:    "compound product release-awareness",
			Generator:   "compound sourceobs release-observation adapter",
			Marker:      ReleaseObservationSchemaVersion,
			Freshness:   "observed",
			DriftReason: fmt.Sprintf("release_observation:version=%s state=%s observed_at=%s", obs.ObservedVersion, obs.ObservedState, obs.ObservedAt),
		},
		MapsTo: []MapTarget{
			{
				ObjectFamily:   releaseObservationObjectFamily,
				CandidateKind:  string(releaseObservationCandidateKind),
				EntityHint:     obs.ProductID,
				PromoteAllowed: false,
			},
		},
		EvidenceRefs: []string{obs.EvidenceRef},
		Release:      &obs,
	}, nil
}

// ReleaseObservationRecord preserves the native intake identities alongside
// the typed payload. These are evidence references, never promoted authority.
type ReleaseObservationRecord struct {
	Observation         ReleaseObservation
	CandidateID         string
	SourceObservationID string
	SourceRef           string
	EvidenceRefs        []string
}

type ReleaseObservationIssue struct {
	ProductHints []string

	ProductID           string
	Channel             string
	Platform            string
	CandidateID         string
	SourceObservationID string
	Reason              string
}

// AppliesTo scopes uncertainty without resolving an ambiguous identity.
// Truly unattributable evidence remains relevant to every requested product.
func (i ReleaseObservationIssue) AppliesTo(productID string) bool {
	if i.ProductID != "" {
		return i.ProductID == productID
	}
	if len(i.ProductHints) == 0 {
		return true
	}
	for _, hint := range i.ProductHints {
		if hint == productID {
			return true
		}
	}
	return false
}

// ReleaseObservationsReport is one read snapshot. Invalid records remain
// attributable; an unreadable inventory is an error, not an empty snapshot.
type ReleaseObservationsReport struct {
	Records []ReleaseObservationRecord
	Issues  []ReleaseObservationIssue
}

// LoadReleaseObservationsReport scans the source projection once per call.
// It does not memoize across calls or hide invalid evidence. A product filter
// bounds the returned scope, not the number of candidate files scanned.
func LoadReleaseObservationsReport(homePath string, productIDs []string, now time.Time) (ReleaseObservationsReport, error) {
	report := ReleaseObservationsReport{}
	if now.IsZero() {
		now = time.Now()
	}
	list, err := ListSourceObservations(homePath, now)
	if err != nil {
		return report, err
	}
	if len(list.Warnings) > 0 {
		return report, fmt.Errorf("source-observation inventory incomplete: %s", strings.Join(list.Warnings, "; "))
	}
	filter := stringSet(productIDs)
	for _, item := range list.Items {
		if !item.IsReleaseObservation() {
			continue
		}
		obs, decoded := decodeReleaseObservationFromItem(item)
		// Subject is used only to attribute a malformed record, never to
		// repair its typed payload or turn it into valid release evidence.
		productID := firstNonEmpty(obs.ProductID, item.Subject)
		issue := ReleaseObservationIssue{
			ProductID: productID, Channel: obs.Channel, Platform: obs.Platform,
			CandidateID: item.CandidateID, SourceObservationID: item.SourceObservationID,
		}
		var invalid error
		switch {
		case strings.TrimSpace(obs.ProductID) != "" && strings.TrimSpace(item.Subject) != "" && strings.TrimSpace(obs.ProductID) != strings.TrimSpace(item.Subject):
			// Neither identity wins. Hints bound which product views need
			// review; they cannot repair or admit the ambiguous observation.
			issue.ProductID = ""
			issue.ProductHints = []string{strings.TrimSpace(obs.ProductID), strings.TrimSpace(item.Subject)}
			sort.Strings(issue.ProductHints)
			invalid = fmt.Errorf("release product identity %q disagrees with source target %q", obs.ProductID, item.Subject)
		case !decoded:
			invalid = fmt.Errorf("incomplete release payload")
		default:
			invalid = ValidateReleaseObservation(obs, now)
		}
		if filter != nil {
			relevant := false
			for id := range filter {
				relevant = relevant || issue.AppliesTo(id)
			}
			if !relevant {
				continue
			}
		}
		if invalid != nil {
			issue.Reason = invalid.Error()
			report.Issues = append(report.Issues, issue)
			continue
		}
		report.Records = append(report.Records, ReleaseObservationRecord{
			Observation: obs, CandidateID: item.CandidateID,
			SourceObservationID: item.SourceObservationID, SourceRef: item.SourceRef,
			EvidenceRefs: append([]string(nil), item.EvidenceRefs...),
		})
	}
	sort.Slice(report.Records, func(i, j int) bool {
		a, b := report.Records[i], report.Records[j]
		if a.Observation.ProductID != b.Observation.ProductID {
			return a.Observation.ProductID < b.Observation.ProductID
		}
		if a.Observation.Channel != b.Observation.Channel {
			return a.Observation.Channel < b.Observation.Channel
		}
		if a.Observation.Platform != b.Observation.Platform {
			return a.Observation.Platform < b.Observation.Platform
		}
		at, _ := time.Parse(time.RFC3339, strings.TrimSpace(a.Observation.ObservedAt))
		bt, _ := time.Parse(time.RFC3339, strings.TrimSpace(b.Observation.ObservedAt))
		if !at.Equal(bt) {
			return at.Before(bt)
		}
		return a.CandidateID < b.CandidateID
	})
	sort.Slice(report.Issues, func(i, j int) bool { return report.Issues[i].CandidateID < report.Issues[j].CandidateID })
	return report, nil
}

// LoadReleaseObservations retains the legacy valid-payload-only interface.
// Consumers making readiness claims must use the report so invalid evidence
// and native provenance remain visible. Only fully identical payloads dedupe.
func LoadReleaseObservations(homePath string, productIDs []string, now time.Time) ([]ReleaseObservation, error) {
	report, err := LoadReleaseObservationsReport(homePath, productIDs, now)
	if err != nil {
		return nil, err
	}
	var out []ReleaseObservation
	seen := map[ReleaseObservation]bool{}
	for _, record := range report.Records {
		if !seen[record.Observation] {
			seen[record.Observation] = true
			out = append(out, record.Observation)
		}
	}
	return out, nil
}

// releaseObservationPath is a deterministic source label. Candidate identity
// hashes the full typed payload, including build and evidence, so observations
// sharing this label cannot overwrite distinct evidence.
func releaseObservationPath(obs ReleaseObservation) string {
	return "compound://release_observation/" + strings.TrimSpace(obs.ProductID) +
		"/" + strings.TrimSpace(obs.Channel) +
		"/" + strings.TrimSpace(obs.Platform) +
		"/" + strings.TrimSpace(obs.ObservedVersion) +
		"/" + strings.TrimSpace(obs.ObservedState) +
		"/" + strings.TrimSpace(obs.ObservedAt) +
		"#" + strings.TrimSpace(obs.SourceRevision)
}

// decodeReleaseObservationFromItem prefers the canonical typed payload.
// Legacy metadata refs remain readable only when no typed payload exists.
// The loader validates the selected payload; malformed typed evidence must
// never fall back to a contradictory legacy encoding.
func decodeReleaseObservationFromItem(item SourceObservationListItem) (ReleaseObservation, bool) {
	if item.Release != nil {
		return *item.Release, true
	}
	obs := ReleaseObservation{}
	for _, ref := range item.EvidenceRefs {
		ref = strings.TrimSpace(ref)
		switch {
		case strings.HasPrefix(ref, "release_observation:evidence:"):
			obs.EvidenceRef = strings.TrimPrefix(ref, "release_observation:evidence:")
		case strings.HasPrefix(ref, "release_observation:revision:"):
			obs.SourceRevision = strings.TrimPrefix(ref, "release_observation:revision:")
		case strings.HasPrefix(ref, "release_observation:state:"):
			obs.ObservedState = strings.TrimPrefix(ref, "release_observation:state:")
		case strings.HasPrefix(ref, "release_observation:observed_at:"):
			obs.ObservedAt = strings.TrimPrefix(ref, "release_observation:observed_at:")
		case strings.HasPrefix(ref, "release_observation:version:"):
			obs.ObservedVersion = strings.TrimPrefix(ref, "release_observation:version:")
		case strings.HasPrefix(ref, "release_observation:build:"):
			obs.ObservedBuild = strings.TrimPrefix(ref, "release_observation:build:")
		case strings.HasPrefix(ref, "release_observation:"):
			// anchor: "release_observation:<productID>:<channel>:<platform>"
			// productID itself may contain ":" (e.g. "product:alpha"),
			// so the LAST two colon-separated segments are channel+platform.
			rest := strings.TrimPrefix(ref, "release_observation:")
			parts := strings.Split(rest, ":")
			if len(parts) >= 3 {
				obs.Platform = strings.TrimSpace(parts[len(parts)-1])
				obs.Channel = strings.TrimSpace(parts[len(parts)-2])
				obs.ProductID = strings.TrimSpace(strings.Join(parts[:len(parts)-2], ":"))
			}
		}
	}
	if obs.ProductID == "" || obs.Channel == "" || obs.Platform == "" {
		return obs, false
	}
	if obs.ObservedVersion == "" || obs.ObservedState == "" || obs.ObservedAt == "" {
		return obs, false
	}
	if obs.EvidenceRef == "" || obs.SourceRevision == "" {
		return obs, false
	}
	return obs, true
}

// IsReleaseObservation reports whether a SourceObservationListItem came
// from typed intake or the legacy release-observation carrier. Typed
// observations may use an ordinary source URL instead of a synthetic key.
func (i SourceObservationListItem) IsReleaseObservation() bool {
	return i.Release != nil || strings.HasPrefix(strings.TrimSpace(i.SourceRef), "compound://release_observation/")
}

func stringSet(values []string) map[string]bool {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]bool, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			out[v] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func oneOfReleaseState(value string) bool {
	for _, a := range ReleaseObservationStates {
		if a == value {
			return true
		}
	}
	return false
}

// validateReleaseObservationShape is the structural-only validation used
// inside SourceObservation.Validate(). It mirrors ValidateReleaseObservation
// minus the time-based checks (which require a `now` argument the public
// Validate() does not receive). Full validation with time + state runs
// in the loader's strict re-validation pass.
func validateReleaseObservationShape(obs ReleaseObservation) bool {
	if strings.TrimSpace(obs.ProductID) == "" {
		return false
	}
	if strings.TrimSpace(obs.Channel) == "" {
		return false
	}
	if strings.TrimSpace(obs.Platform) == "" {
		return false
	}
	if strings.TrimSpace(obs.ObservedVersion) == "" {
		return false
	}
	if !oneOfReleaseState(obs.ObservedState) {
		return false
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimSpace(obs.ObservedAt)); err != nil {
		return false
	}
	if strings.TrimSpace(obs.SourceRevision) == "" {
		return false
	}
	if strings.TrimSpace(obs.EvidenceRef) == "" {
		return false
	}
	return true
}
