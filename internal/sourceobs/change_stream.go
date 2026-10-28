package sourceobs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"compound/internal/candidates"
)

// ChangeStreamSchemaVersion is the door's input contract version. The Hermes change-stream
// scorer (change_stream_candidates.py) emits this shape; the door translates the Hermes-native
// candidate types into Compound candidate kinds and parks every one no-promote.
const ChangeStreamSchemaVersion = "compound.change_stream_intake.v0.1"

// ChangeStreamIntake is the door input: a reference to the raw Hermes change-stream capture
// (the original artifact the door content-addresses and preserves — keep-all-data source layer)
// plus the candidates a scorer derived from it (the derived layer parked above the source).
type ChangeStreamIntake struct {
	SchemaVersion string                  `json:"schema_version"`
	SourceCapture string                  `json:"source_capture"` // absolute path to the raw capture JSON
	GeneratedAt   string                  `json:"generated_at"`
	Candidates    []ChangeStreamCandidate `json:"candidates"`
}

// ChangeStreamCandidate is one derived signal carried in the intake. Type is the Hermes-native
// candidate vocabulary; the door maps it to a Compound candidate kind. Nothing here promotes:
// the door is the meaning owner's ingress, not a truth-promotion authority.
type ChangeStreamCandidate struct {
	Type         string   `json:"type"` // repo-signal | agent-history-row | canon-file-signal
	EntityHint   string   `json:"entity_hint"`
	Surface      string   `json:"surface"`
	Summary      string   `json:"summary"`
	Score        int      `json:"score"`
	EvidenceRefs []string `json:"evidence_refs"`
}

// changeStreamKindMap translates the Hermes-native candidate type into a Compound candidate kind
// and its object family. The door OWNS this translation (single source of truth) so the Python
// scorer never has to know Compound's inbox taxonomy. Product/portfolio drift (a repo or a
// session touched an app surface) is a product hypothesis awaiting operator adjudication
// (park/operationalize/reject — its promotion path is logging a durable, measurable app change);
// harness-canon drift is a capability observation, the same kind canon-observe parks. An unknown
// type is a hard error (fail-closed) — a candidate whose kind we cannot name must never reach disk.
func changeStreamKind(hermesType string) (candidates.Kind, string, error) {
	switch strings.TrimSpace(hermesType) {
	case "repo-signal", "agent-history-row":
		return candidates.ProductHypothesis, "product", nil
	case "canon-file-signal":
		return candidates.CapabilityObs, "capability", nil
	default:
		return "", "", fmt.Errorf("unknown change-stream candidate type %q", hermesType)
	}
}

// changeStreamEntityHint produces a per-candidate hint that is (a) non-empty (Validate requires it),
// (b) stable for a given candidate so an idempotent re-run overwrites the same candidate record,
// and (c) unique across distinct candidates in one capture so two signals sharing a slug/surface do
// not collide onto one candidate id. The trailing content hash provides the uniqueness+stability.
func changeStreamEntityHint(c ChangeStreamCandidate) string {
	base := strings.TrimSpace(c.EntityHint)
	if base == "" {
		base = strings.TrimSpace(c.Surface)
	}
	if base == "" {
		base = strings.TrimSpace(c.Type)
	}
	if base == "" {
		base = "change-stream"
	}
	disc := shortSHA(c.Type, c.EntityHint, c.Surface, c.Summary)
	return base + "#" + disc
}

func shortSHA(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])[:8]
}

// FromChangeStream builds the SourceObservation for one change-stream capture. captureSHA is the
// content hash of the raw capture body (deterministic => the observation id and every candidate id
// are content-addressed, so re-observing the identical capture overwrites the same records rather
// than spraying duplicates). bodyRef is the content-addressed artifact-store ref of the preserved
// raw capture (the keep-all-data original layer). The observation maps to no-promote candidates
// only; the change-stream is evidence feeding review, never promoted truth.
func FromChangeStream(intake ChangeStreamIntake, captureSHA, bodyRef string, now time.Time) (SourceObservation, error) {
	capturePath := strings.TrimSpace(intake.SourceCapture)
	if capturePath == "" {
		return SourceObservation{}, fmt.Errorf("change-stream intake: source_capture required")
	}
	if strings.TrimSpace(captureSHA) == "" {
		return SourceObservation{}, fmt.Errorf("change-stream intake: capture sha required")
	}
	if len(intake.Candidates) == 0 {
		return SourceObservation{}, fmt.Errorf("change-stream intake: no candidates to map (preserve the body without an observation)")
	}
	maps := make([]MapTarget, 0, len(intake.Candidates))
	for i, c := range intake.Candidates {
		kind, family, err := changeStreamKind(c.Type)
		if err != nil {
			return SourceObservation{}, fmt.Errorf("change-stream candidate %d: %w", i, err)
		}
		maps = append(maps, MapTarget{
			ObjectFamily:   family,
			CandidateKind:  string(kind),
			EntityHint:     changeStreamEntityHint(c),
			PromoteAllowed: false,
		})
	}
	evidence := []string{"change_stream_capture:" + captureSHA}
	if bodyRef != "" {
		evidence = append(evidence, bodyRef)
	}
	if gen := strings.TrimSpace(intake.GeneratedAt); gen != "" {
		evidence = append(evidence, "generated_at:"+gen)
	}
	return SourceObservation{
		SchemaVersion: SchemaVersion,
		ID:            idFor("srcobs_", "change_stream", capturePath, captureSHA),
		AdapterID:     "compound.change-stream",
		SourceSystem:  "hermes-change-stream",
		ObservedAt:    rfc3339(now),
		SourceRef: SourceRef{
			Path:        capturePath,
			Kind:        "change_stream_capture",
			Owner:       "hermes-change-stream",
			SourceClass: "observed",
			Revision:    captureSHA,
		},
		MapsTo:       maps,
		EvidenceRefs: evidence,
	}, nil
}
