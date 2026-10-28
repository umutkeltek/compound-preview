package harness

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PermissionPostureObservation is the neutral, batch-ready payload for one
// permission-posture drift declaration (packet work_0fc6126d8fa2: wire the
// existing PermissionDrift probe into the observation intake). This file
// never imports internal/observation: internal/harness stays upstream of the
// generic intake seam; the cmd layer (cmd/compound/harness_drift_observe.go)
// assembles the observation.Batch and calls observation.ObserveBatch.
type PermissionPostureObservation struct {
	// ObservationID is a stable hash of (harness, expected_state,
	// observed_state). Re-emitting on the SAME drift state reproduces this
	// id so the source-intake divergence card dedupes to one; a changed
	// drift state mints a different id (and so a distinct card).
	ObservationID string
	// ExpectedState / ObservedState are compact deterministic digests of the
	// intended vs live permission posture -- never full file dumps -- so
	// they stay stable across runs for the same drift state and unrelated
	// hand-owned keys (theme, a custom hook, ...) never perturb the
	// identity.
	ExpectedState string
	ObservedState string
	Title         string
	EvidenceRefs  []string
}

// BuildPermissionPostureObservation derives the observation payload for a
// drifted PermissionDriftReport. intendedContent is the authority's rendered
// settings (RenderPermissions(...).Content); liveContent is the live target
// file bytes PermissionDrift already read at report.TargetPath. ok is false
// when the report is not drifted (silence-on-match) or, defensively, when
// the computed states still collide -- this never hands the intake seam an
// observation shaped like a divergence that it would silently drop.
func BuildPermissionPostureObservation(report PermissionDriftReport, intendedContent, liveContent []byte) (PermissionPostureObservation, bool) {
	if !report.Drifted {
		return PermissionPostureObservation{}, false
	}
	expected := permissionPostureState(intendedContent, nil)
	observed := permissionPostureState(liveContent, report.Details)
	if expected == observed {
		return PermissionPostureObservation{}, false
	}
	return PermissionPostureObservation{
		ObservationID: permissionPostureHash("permdrift_", report.Harness, expected, observed),
		ExpectedState: expected,
		ObservedState: observed,
		Title:         fmt.Sprintf("%s permission posture drift", report.Harness),
		EvidenceRefs: []string{
			PermissionAuthorityRef(report.Harness),
			"artifact_sha256:" + digest(liveContent),
		},
	}, true
}

// permissionPostureState renders a compact, deterministic digest of a
// settings JSON body's MANAGED permission surface (defaultMode + each
// managed rule-set's sorted-rule digest), optionally folding in the
// human-readable drift Details. It deliberately never hashes the raw file
// bytes: an unrelated key (theme, a hand-owned hook, ...) must never change
// the posture identity, matching PermissionDrift's own "ignores unrelated
// keys" contract (TestPermissionDriftRedGreenAndIgnoresUnrelatedKeys).
func permissionPostureState(content []byte, details []string) string {
	var settings map[string]any
	_ = json.Unmarshal(content, &settings) // best-effort: malformed/empty content still yields a stable zero state
	permissions := objectAt(settings, "permissions")
	autoMode := objectAt(settings, "autoMode")
	parts := []string{
		"mode=" + fmt.Sprintf("%v", permissions["defaultMode"]),
		"allow=" + permissionRuleDigest(stringSlice(permissions["allow"])),
		"deny=" + permissionRuleDigest(stringSlice(permissions["deny"])),
		"ask=" + permissionRuleDigest(stringSlice(permissions["ask"])),
		"auto=" + permissionRuleDigest(stringSlice(autoMode["allow"])),
	}
	if len(details) > 0 {
		parts = append(parts, "details="+permissionRuleDigest(uniqueSorted(details)))
	}
	return strings.Join(parts, ";")
}

func permissionRuleDigest(rules []string) string {
	return digest([]byte(strings.Join(sortedRules(rules), ",")))[:16]
}

func permissionPostureHash(prefix string, parts ...string) string {
	return prefix + digest([]byte(strings.Join(parts, "\x00")))[:12]
}
