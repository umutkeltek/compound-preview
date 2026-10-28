package harness

import (
	"fmt"
	"strings"
)

// InstructionPackObservation is the neutral, batch-ready payload for one
// instruction-pack drift declaration (packet work_e9a494d5cf58: mirror the
// permission-posture observation loop). This file NEVER imports
// internal/observation: internal/harness stays upstream of the generic
// intake seam; the cmd layer (cmd/compound/instruction_pack_drift_observe.go)
// assembles observation.Batch and calls observation.ObserveBatch — exactly
// as permission_posture_observation.go is upstream of
// cmd/compound/harness_drift_observe.go.
type InstructionPackObservation struct {
	// ObservationID is a stable hash of (harness, expected_state,
	// observed_state). Re-emitting on the SAME drift state reproduces this
	// id so the source-intake divergence card dedupes to one; a changed
	// drift state mints a different id (so a distinct card is added).
	ObservationID string
	// ExpectedState / ObservedState are compact, deterministic digests of
	// the managed surface — ContentSHA256 + per-layer refs/revisions, never
	// raw file dumps, never GeneratedAt. They stay stable across re-runs
	// over identical inputs and ignore unrelated home churn (the managed-
	// surface discipline from TestPermissionDriftRedGreenAndIgnoresUnrelatedKeys,
	// mirrored here).
	ExpectedState string
	ObservedState string
	Title         string
	EvidenceRefs  []string
}

// BuildInstructionPackObservation derives the observation payload for a
// drifted InstructionPackDriftReport. The cold-start case (ObservedDigest
// empty) gets a stable "no_on_disk_pack" sentinel so the intake seam sees
// a card (F3) instead of staying silent — closing the loop on the S5
// deviation. A drifted report whose computed states still collide hands
// back silence (F4) so the intake seam never drops a card it thinks it
// just minted.
func BuildInstructionPackObservation(report InstructionPackDriftReport) (InstructionPackObservation, bool) {
	if !report.Drifted {
		return InstructionPackObservation{}, false
	}
	expected := report.ExpectedDigest
	observed := report.ObservedDigest
	if observed == "" {
		observed = "no_on_disk_pack"
	}
	if expected == observed {
		return InstructionPackObservation{}, false
	}
	// The preset joins the id hash ONLY when non-empty, so presetless
	// observation ids stay byte-identical to the ones already recorded in
	// existing intake stores. Two presets over one harness that drift the
	// same way must NOT dedupe onto one card — the preset term is what
	// keeps them distinct.
	hashParts := []string{report.Harness}
	title := fmt.Sprintf("%s instruction-pack drift", report.Harness)
	if report.Preset != "" {
		hashParts = append(hashParts, report.Preset)
		title = fmt.Sprintf("%s[%s] instruction-pack drift", report.Harness, report.Preset)
	}
	hashParts = append(hashParts, expected, observed)

	return InstructionPackObservation{
		ObservationID: instructionPackObservationHash("packdrift_", hashParts...),
		ExpectedState: expected,
		ObservedState: observed,
		Title:         title,
		EvidenceRefs: []string{
			report.Path,
			"context_expected_sha256:" + expected,
		},
	}, true
}

func instructionPackObservationHash(prefix string, parts ...string) string {
	return prefix + digest([]byte(strings.Join(parts, "\x00")))[:12]
}
