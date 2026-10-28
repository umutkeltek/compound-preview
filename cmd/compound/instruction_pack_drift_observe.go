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

// instructionPackDivergenceEmission is the cmd-layer's small, JSON-friendly
// summary of an emitInstructionPackDivergence call. It stays a local type
// (not observation.Report) so main.go's instruction-pack verb branch can
// report on emission without importing internal/observation itself.
type instructionPackDivergenceEmission struct {
	Applied       bool   `json:"observation_applied"`
	ObservationID string `json:"observation_id,omitempty"`
	SourcePath    string `json:"source_path,omitempty"`
}

// emitInstructionPackDivergence bridges a drifted
// harness.InstructionPackDriftReport into the generic observation intake
// (packet work_e9a494d5cf58): it builds the neutral
// harness.InstructionPackObservation (internal/harness stays unaware of
// internal/observation — see internal/harness/instruction_pack_observation.go),
// assembles ONE compound.observation_batch.v0.1 batch with ONE tool_health
// observation, and applies it through observation.ObserveBatch so the
// existing source-intake edge can declare a divergence_declared card. It
// writes nothing when the report is not drifted (silence-on-match, F3).
//
// The batch is persisted at a path KEYED ON THE OBSERVATION ID under
// $COMPOUND_HOME/state/instruction-pack-drift-observations/, not a scratch
// file: the divergence card is rebuilt later by reloading row.SourcePath
// (internal/observation/intake.go sourceIntakeDivergenceDimensions), so
// the batch file must remain readable after this call returns. Keying the
// filename on the observation id (not just the harness) keeps re-emission
// of the SAME drift state idempotent — identical path, identical bytes —
// while a CHANGED drift state writes a new file alongside the old one
// instead of overwriting it out from under an already-recorded intake row.
func emitInstructionPackDivergence(homePath string, report harness.InstructionPackDriftReport, now time.Time) (instructionPackDivergenceEmission, error) {
	obs, ok := harness.BuildInstructionPackObservation(report)
	if !ok {
		return instructionPackDivergenceEmission{}, nil
	}
	nowStr := now.UTC().Format(time.RFC3339)
	batch := observation.Batch{
		SchemaVersion: observation.BatchSchemaVersion,
		Source: observation.Source{
			Kind:       "harness_probe",
			Ref:        "compound://context/instruction-packs/" + report.Harness + "/latest.json",
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
			ID:         obs.ObservationID,
			ObservedAt: nowStr,
			Kind:       "tool_health",
			Title:      obs.Title,
			Confidence: 1,
			Dimensions: map[string]string{
				"expected_state": obs.ExpectedState,
				"observed_state": obs.ObservedState,
			},
			EvidenceRefs: obs.EvidenceRefs,
		}},
	}
	sourcePath, err := writeInstructionPackDriftObservationBatch(homePath, batch)
	if err != nil {
		return instructionPackDivergenceEmission{}, err
	}
	if _, err := observation.ObserveBatch(homePath, nil, batch, sourcePath, observation.Options{ApplySourceObservations: true}, now); err != nil {
		return instructionPackDivergenceEmission{}, err
	}
	return instructionPackDivergenceEmission{Applied: true, ObservationID: obs.ObservationID, SourcePath: sourcePath}, nil
}

// writeInstructionPackDriftObservationBatch persists the batch under
// $COMPOUND_HOME/state/instruction-pack-drift-observations/ so it stays
// readable when the source-intake projection later reloads it by
// SourcePath. See emitInstructionPackDivergence doc comment for why the
// filename is keyed on the observation id (not just the harness).
func writeInstructionPackDriftObservationBatch(homePath string, batch observation.Batch) (string, error) {
	dir := filepath.Join(homePath, "state", "instruction-pack-drift-observations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir instruction-pack-drift-observations: %w", err)
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
