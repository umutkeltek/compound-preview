package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"compound/internal/harness"
	"compound/internal/observation"
)

// permissionDivergenceEmission is the cmd-layer's small, JSON-friendly
// summary of an emitPermissionPostureDivergence call. It stays a local type
// (not observation.Report) so main.go's drift verb branch can report on
// emission without importing internal/observation itself.
type permissionDivergenceEmission struct {
	Applied       bool   `json:"observation_applied"`
	ObservationID string `json:"observation_id,omitempty"`
	SourcePath    string `json:"source_path,omitempty"`
}

// emitPermissionPostureDivergence bridges a drifted harness.PermissionDriftReport
// into the generic observation intake (packet work_0fc6126d8fa2): it renders
// the intended posture, reads the live target file, builds the neutral
// harness.PermissionPostureObservation (internal/harness stays unaware of
// internal/observation -- see internal/harness/permission_posture_observation.go),
// assembles ONE compound.observation_batch.v0.1 batch, and applies it through
// observation.ObserveBatch so the existing source-intake edge can declare a
// divergence_declared card. It writes nothing when the report is not
// drifted (silence-on-match, F3).
//
// The batch is persisted at a path KEYED ON THE OBSERVATION ID under
// $COMPOUND_HOME/state/harness-drift-observations/, not a scratch/temp file:
// the divergence card is rebuilt later by reloading row.SourcePath
// (internal/observation/intake.go sourceIntakeDivergenceDimensions), so the
// batch file must remain readable after this call returns. Keying the
// filename on the observation id (not just the harness) keeps re-emission of
// the SAME drift state idempotent (identical path, identical bytes) while a
// CHANGED drift state writes a new file alongside the old one instead of
// overwriting it out from under an already-recorded intake row.
func emitPermissionPostureDivergence(homePath string, report harness.PermissionDriftReport, now time.Time) (permissionDivergenceEmission, error) {
	if !report.Drifted {
		return permissionDivergenceEmission{}, nil
	}
	render, err := harness.RenderPermissions(homePath, report.Harness)
	if err != nil {
		return permissionDivergenceEmission{}, fmt.Errorf("render intended permission posture: %w", err)
	}
	live, err := os.ReadFile(report.TargetPath)
	if err != nil {
		return permissionDivergenceEmission{}, fmt.Errorf("read live permission target %s: %w", report.TargetPath, err)
	}
	posture, ok := harness.BuildPermissionPostureObservation(report, render.Content, live)
	if !ok {
		return permissionDivergenceEmission{}, nil
	}
	nowStr := now.UTC().Format(time.RFC3339)
	batch := observation.Batch{
		SchemaVersion: observation.BatchSchemaVersion,
		Source: observation.Source{
			Kind:       "harness_probe",
			Ref:        harness.PermissionAuthorityRef(report.Harness),
			CapturedAt: nowStr,
			Collector:  "compound harness drift",
			Privacy:    "private",
		},
		Subject: observation.Subject{
			Kind:    "harness",
			ID:      report.Harness,
			Surface: "control-plane",
			Channel: "probe",
		},
		Observations: []observation.Observation{{
			ID:         posture.ObservationID,
			ObservedAt: nowStr,
			Kind:       "tool_health",
			Title:      posture.Title,
			Confidence: 1,
			Dimensions: map[string]string{
				"expected_state": posture.ExpectedState,
				"observed_state": posture.ObservedState,
			},
			EvidenceRefs: posture.EvidenceRefs,
		}},
	}
	sourcePath, err := writePermissionDriftObservationBatch(homePath, batch)
	if err != nil {
		return permissionDivergenceEmission{}, err
	}
	if _, err := observation.ObserveBatch(homePath, nil, batch, sourcePath, observation.Options{ApplySourceObservations: true}, now); err != nil {
		return permissionDivergenceEmission{}, err
	}
	return permissionDivergenceEmission{Applied: true, ObservationID: posture.ObservationID, SourcePath: sourcePath}, nil
}

// writePermissionDriftObservationBatch persists the batch under
// $COMPOUND_HOME/state/harness-drift-observations/ so it stays readable when
// the source-intake projection later reloads it by SourcePath. See the
// emitPermissionPostureDivergence doc comment for why the filename is keyed
// on the observation id.
func writePermissionDriftObservationBatch(homePath string, batch observation.Batch) (string, error) {
	dir := filepath.Join(homePath, "state", "harness-drift-observations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	obsID := batch.Observations[0].ID
	path := filepath.Join(dir, batch.Subject.ID+"-"+obsID+".json")
	data, err := json.MarshalIndent(batch, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
