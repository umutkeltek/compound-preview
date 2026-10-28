package projection

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"compound/internal/reviewartifact"
)

const RegenerationPlanSchemaVersion = "compound.projection_regeneration_plan.v0.1"

// RegenerationPlan is a reviewable generated-view repair artifact. It records
// the evidence needed to regenerate or review an owner view, but never writes
// that owner view.
type RegenerationPlan struct {
	SchemaVersion       string              `json:"schema_version"`
	ID                  string              `json:"id"`
	CreatedAt           string              `json:"created_at"`
	Status              string              `json:"status"`
	ProjectionID        string              `json:"projection_id"`
	ViewRef             string              `json:"view_ref"`
	FreshnessStatus     string              `json:"freshness_status"`
	Staleness           string              `json:"staleness,omitempty"`
	CoverageVerdict     string              `json:"coverage_verdict,omitempty"`
	SourceLedgerSeq     int64               `json:"source_ledger_seq"`
	OutputHash          string              `json:"output_hash,omitempty"`
	Sources             []SourceObservation `json:"sources,omitempty"`
	FailedChecks        []Check             `json:"failed_checks,omitempty"`
	WritebackGateID     string              `json:"writeback_gate_id,omitempty"`
	WorkID              string              `json:"work_id,omitempty"`
	ReceiptID           string              `json:"receipt_id,omitempty"`
	TransitionID        string              `json:"transition_id,omitempty"`
	SourceObservationID string              `json:"source_observation_id,omitempty"`
	CandidateIDs        []string            `json:"candidate_ids,omitempty"`
	NoPromoteIDs        []string            `json:"no_promote_ids,omitempty"`
	ProposedActions     []string            `json:"proposed_actions"`
	EvidenceRefs        []string            `json:"evidence_refs"`
}

type RegenerationRefs struct {
	WritebackGateID     string
	WorkID              string
	ReceiptID           string
	TransitionID        string
	SourceObservationID string
	CandidateIDs        []string
	NoPromoteIDs        []string
}

type RegenerationArtifactPaths struct {
	PlanPath       string
	ReviewPath     string
	ReviewYAMLPath string
}

func BuildRegenerationPlan(f Freshness, refs RegenerationRefs, now time.Time) (RegenerationPlan, error) {
	if !NeedsWriteback(f) {
		return RegenerationPlan{}, fmt.Errorf("projection %s is fresh; regeneration plan not needed", f.ProjectionID)
	}
	createdAt := now.UTC().Format(time.RFC3339Nano)
	plan := RegenerationPlan{
		SchemaVersion:       RegenerationPlanSchemaVersion,
		ID:                  "pgrp_" + reviewartifact.Digest12(strings.Join([]string{f.ProjectionID, f.ViewRef, f.Status, f.OutputHash, createdAt}, "\n")),
		CreatedAt:           createdAt,
		Status:              "review_required",
		ProjectionID:        f.ProjectionID,
		ViewRef:             f.ViewRef,
		FreshnessStatus:     f.Status,
		Staleness:           f.Staleness,
		CoverageVerdict:     f.CoverageVerdict,
		SourceLedgerSeq:     f.SourceLedgerSeq,
		OutputHash:          f.OutputHash,
		Sources:             append([]SourceObservation(nil), f.Sources...),
		FailedChecks:        failedChecks(f.Checks),
		WritebackGateID:     refs.WritebackGateID,
		WorkID:              refs.WorkID,
		ReceiptID:           refs.ReceiptID,
		TransitionID:        refs.TransitionID,
		SourceObservationID: refs.SourceObservationID,
		CandidateIDs:        append([]string(nil), refs.CandidateIDs...),
		NoPromoteIDs:        append([]string(nil), refs.NoPromoteIDs...),
		ProposedActions: []string{
			"review failed freshness checks and source hashes",
			"regenerate the view through its owner generator after source-owner review",
			"rerun projection doctor and compare output/source hashes",
			"do not hand-edit generated view or promote generated-view truth",
		},
		EvidenceRefs: EvidenceRefs(f),
	}
	return plan, nil
}

func RecordRegenerationPlan(homePath string, plan RegenerationPlan) (RegenerationArtifactPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return RegenerationArtifactPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	paths, err := reviewartifact.Write(homePath, ProjectionRegenerationReviewArea, plan.ID, plan, " ", RenderRegenerationReview(plan))
	if err != nil {
		return RegenerationArtifactPaths{}, err
	}
	reviewYAMLPath, err := reviewartifact.WriteRaw(homePath, ProjectionRegenerationReviewArea, plan.ID+".review.yaml", []byte(RenderRegenerationReviewYAMLDraft(plan)))
	if err != nil {
		return RegenerationArtifactPaths{}, err
	}
	return RegenerationArtifactPaths{PlanPath: paths.PlanPath, ReviewPath: paths.ReviewPath, ReviewYAMLPath: reviewYAMLPath}, nil
}

func RenderRegenerationReview(plan RegenerationPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Projection Regeneration Plan\n\n")
	fmt.Fprintf(&b, "- plan: `%s`\n", plan.ID)
	fmt.Fprintf(&b, "- status: `%s`\n", plan.Status)
	fmt.Fprintf(&b, "- projection: `%s`\n", plan.ProjectionID)
	fmt.Fprintf(&b, "- view: `%s`\n", plan.ViewRef)
	fmt.Fprintf(&b, "- freshness: `%s`\n", plan.FreshnessStatus)
	if plan.Staleness != "" {
		fmt.Fprintf(&b, "- staleness: `%s`\n", plan.Staleness)
	}
	if plan.WritebackGateID != "" {
		fmt.Fprintf(&b, "- writeback_gate: `%s`\n", plan.WritebackGateID)
	}
	if plan.SourceObservationID != "" {
		fmt.Fprintf(&b, "- source_observation: `%s`\n", plan.SourceObservationID)
	}
	fmt.Fprintf(&b, "\nThis is an owner-review artifact. It does not regenerate or edit the generated view.\n\n")
	if len(plan.FailedChecks) > 0 {
		fmt.Fprintf(&b, "## Failed Checks\n\n")
		for _, check := range plan.FailedChecks {
			fmt.Fprintf(&b, "- `%s`: `%s`", check.Name, check.Status)
			if check.Detail != "" {
				fmt.Fprintf(&b, " (%s)", check.Detail)
			}
			fmt.Fprintln(&b)
		}
		fmt.Fprintln(&b)
	}
	if len(plan.Sources) > 0 {
		fmt.Fprintf(&b, "## Sources\n\n")
		sources := append([]SourceObservation(nil), plan.Sources...)
		sort.Slice(sources, func(i, j int) bool { return sources[i].SourceRef < sources[j].SourceRef })
		for _, source := range sources {
			fmt.Fprintf(&b, "- `%s`: `%s`", source.SourceRef, source.Status)
			if source.CurrentHash != "" {
				fmt.Fprintf(&b, " current `%s`", source.CurrentHash)
			}
			if source.ExpectedHash != "" {
				fmt.Fprintf(&b, " expected `%s`", source.ExpectedHash)
			}
			fmt.Fprintln(&b)
		}
		fmt.Fprintln(&b)
	}
	fmt.Fprintf(&b, "## Proposed Actions\n\n")
	for _, action := range plan.ProposedActions {
		fmt.Fprintf(&b, "- %s\n", action)
	}
	return b.String()
}

func failedChecks(checks []Check) []Check {
	var out []Check
	for _, check := range checks {
		if check.Status != "ok" {
			out = append(out, check)
		}
	}
	return out
}
