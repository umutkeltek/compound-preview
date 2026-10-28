package reviewartifact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DecisionBriefSchemaVersion is the schema for the read-only, propose-only
// decision-brief sidecar (<artifact-id>.brief.json). A brief is a prepared
// TRANSLATION of an underlying review artifact for the operator console — it
// NEVER mutates the artifact's status/band, never applies an option, and is
// never itself counted as a review or adjudication (see queue.go's
// coreReviewJSON suffix list and the propose-only falsifier tests in
// brief_test.go).
const DecisionBriefSchemaVersion = "compound.decision_brief.v0.1"

// DecisionBrief is the generic, area-agnostic sidecar payload a preparing
// agent session (claude code / codex / claude-zai / opencode / switchback
// lanes) writes next to a review artifact. Every field here is display
// material for the console; none of it can move the underlying artifact's
// status or band — the queue annotator that attaches a Brief to a QueueItem
// only ever reads these fields, never "status"/"band"/etc, and rejects any
// payload that carries a field outside this schema (see ParseDecisionBrief).
type DecisionBrief struct {
	SchemaVersion  string                      `json:"schema_version"`
	Headline       string                      `json:"headline"`
	WhatThisIs     string                      `json:"what_this_is,omitempty"`
	Findings       []DecisionBriefFinding      `json:"findings"`
	Options        []DecisionBriefOption       `json:"options,omitempty"`
	Recommendation DecisionBriefRecommendation `json:"recommendation"`
	PreparedBy     string                      `json:"prepared_by"`
	PreparedAt     string                      `json:"prepared_at,omitempty"`
	ProposeOnly    bool                        `json:"propose_only"`
}

// DecisionBriefFinding is one cited observation. EvidenceRef is REQUIRED — a
// finding without one makes the whole brief invalid (evidence-ref-required,
// see ParseDecisionBrief).
type DecisionBriefFinding struct {
	Finding     string `json:"finding"`
	EvidenceRef string `json:"evidence_ref"`
}

// DecisionBriefOption is one operator choice. CommandArgv is copy/display
// material ONLY in this slice — the console never wires it into
// /api/decision/execute; execution stays with the existing decision-registry
// allowlist (see console/src/lib/decide-model.ts).
type DecisionBriefOption struct {
	Value              string   `json:"value"`
	Label              string   `json:"label"`
	ConsequenceApprove string   `json:"consequence_approve,omitempty"`
	ConsequenceHold    string   `json:"consequence_hold,omitempty"`
	CommandArgv        []string `json:"command_argv,omitempty"`
}

// DecisionBriefRecommendation is the preparing session's own lean — never
// auto-applied (decisions-down law: the system prepares, the operator decides).
type DecisionBriefRecommendation struct {
	Option     string  `json:"option,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	Rationale  string  `json:"rationale,omitempty"`
}

// ParseDecisionBrief strictly decodes a `.brief.json` sidecar payload and
// validates it. It rejects any field outside the schema (a brief that injects
// "status"/"band"/anything else is invalid, never silently ignored — the
// first line of defense for the propose-only falsifier) and returns
// human-readable warnings for a well-formed-but-incomplete brief (missing
// evidence refs, missing provenance). The returned *DecisionBrief is non-nil
// ONLY when the brief is fully valid; callers must attach the warnings either
// way so the operator can see why a brief was dropped.
func ParseDecisionBrief(raw []byte) (*DecisionBrief, []string) {
	var brief DecisionBrief
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&brief); err != nil {
		return nil, []string{fmt.Sprintf("decision brief invalid (unknown field or malformed schema): %s", err.Error())}
	}
	var warnings []string
	if strings.TrimSpace(brief.SchemaVersion) != DecisionBriefSchemaVersion {
		warnings = append(warnings, fmt.Sprintf("decision brief schema_version must be %q", DecisionBriefSchemaVersion))
	}
	if strings.TrimSpace(brief.Headline) == "" {
		warnings = append(warnings, "decision brief headline is required")
	}
	if len(brief.Findings) == 0 {
		warnings = append(warnings, "decision brief findings must include at least one finding")
	}
	for i, finding := range brief.Findings {
		if strings.TrimSpace(finding.EvidenceRef) == "" {
			warnings = append(warnings, fmt.Sprintf("decision brief finding %d missing required evidence_ref", i))
		}
	}
	if strings.TrimSpace(brief.PreparedBy) == "" {
		warnings = append(warnings, "decision brief prepared_by is required")
	}
	if len(warnings) > 0 {
		return nil, warnings
	}
	return &brief, nil
}

// DecisionBriefSidecarName returns the `<id>.brief.json` sidecar filename for
// a review artifact id — same pattern as `<id>.reviewed.json`.
func DecisionBriefSidecarName(id string) string {
	return id + ".brief.json"
}

// DecisionBriefWritePath returns the exact sidecar path a preparing session
// must write to, next to the review artifact's own JSON path.
func DecisionBriefWritePath(itemJSONPath, itemID string) string {
	return filepath.Join(filepath.Dir(itemJSONPath), DecisionBriefSidecarName(itemID))
}

// loadDecisionBriefSidecar reads and validates the `<stem>.brief.json`
// sidecar next to a review artifact, if any. A missing sidecar is not an
// error — it just means the item has no brief yet.
func loadDecisionBriefSidecar(dir, stem string) (*DecisionBrief, []string, error) {
	path := filepath.Join(dir, DecisionBriefSidecarName(stem))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	brief, warnings := ParseDecisionBrief(data)
	return brief, warnings, nil
}

// DecisionBriefJSONSchema returns the versioned, inline JSON-Schema-shaped
// description of compound.decision_brief.v0.1 for `compound reviews
// brief-plan` to hand a preparing agent session verbatim. It is the ONE place
// the shape is described so the plan packet and ParseDecisionBrief can never
// drift silently apart.
func DecisionBriefJSONSchema() map[string]any {
	return map[string]any{
		"$id":                  DecisionBriefSchemaVersion,
		"type":                 "object",
		"required":             []string{"schema_version", "headline", "findings", "recommendation", "prepared_by", "propose_only"},
		"additionalProperties": false,
		"properties": map[string]any{
			"schema_version": map[string]any{"type": "string", "const": DecisionBriefSchemaVersion},
			"headline":       map[string]any{"type": "string", "description": "one-line plain-language headline for the operator card"},
			"what_this_is":   map[string]any{"type": "string", "description": "1-2 sentences translating the underlying artifact"},
			"findings": map[string]any{
				"type":     "array",
				"minItems": 1,
				"items": map[string]any{
					"type":                 "object",
					"required":             []string{"finding", "evidence_ref"},
					"additionalProperties": false,
					"properties": map[string]any{
						"finding":      map[string]any{"type": "string"},
						"evidence_ref": map[string]any{"type": "string", "description": "REQUIRED on every finding"},
					},
				},
			},
			"options": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"required":             []string{"value", "label"},
					"additionalProperties": false,
					"properties": map[string]any{
						"value":               map[string]any{"type": "string"},
						"label":               map[string]any{"type": "string"},
						"consequence_approve": map[string]any{"type": "string"},
						"consequence_hold":    map[string]any{"type": "string"},
						"command_argv": map[string]any{
							"type":        []string{"array", "null"},
							"items":       map[string]any{"type": "string"},
							"description": "copy/display material only — never wired into /api/decision/execute in this slice",
						},
					},
				},
			},
			"recommendation": map[string]any{
				"type":                 "object",
				"required":             []string{"option", "confidence", "rationale"},
				"additionalProperties": false,
				"properties": map[string]any{
					"option":     map[string]any{"type": "string"},
					"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
					"rationale":  map[string]any{"type": "string"},
				},
			},
			"prepared_by":  map[string]any{"type": "string", "description": "REQUIRED — lane/actor provenance, e.g. codex, claude_code, switchback_api"},
			"prepared_at":  map[string]any{"type": "string", "format": "date-time"},
			"propose_only": map[string]any{"type": "boolean", "const": true, "description": "always true — a brief never mutates status/band/adjudication"},
		},
	}
}

// DecisionBriefProposeOnlyWarnings are the propose-only constraints repeated
// verbatim on every brief-plan packet so a preparing agent session cannot
// miss them regardless of harness (claude/codex/opencode/switchback all
// consume the same plain JSON plan).
var DecisionBriefProposeOnlyWarnings = []string{
	"propose-only: writing this file never changes the artifact's status or band, and this brief is never counted as a review or adjudication.",
	"strict schema: any field outside this schema (e.g. status, band) makes the whole brief invalid; it is dropped with a warning, not merged.",
	"options[].command_argv is copy/display material only in this slice — it is never wired into /api/decision/execute; execution stays with the existing decision-registry allowlist.",
}
