package distiller

import (
	"fmt"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/reviewartifact"
)

const TastePlanSchemaVersion = "compound.distiller.taste_plan.v0.1"

// TastePlan is a reviewed taste-standard proposal from a taste observation.
// It preserves taste as evidence-backed judgment without silently editing
// doctrine, product strategy, prompts, or skills.
type TastePlan struct {
	SchemaVersion    string             `json:"schema_version"`
	ID               string             `json:"id"`
	CreatedAt        string             `json:"created_at"`
	Status           string             `json:"status"`
	CandidateID      string             `json:"candidate_id"`
	CandidateVerdict string             `json:"candidate_verdict,omitempty"`
	TasteKey         string             `json:"taste_key"`
	Subject          string             `json:"subject"`
	Statement        string             `json:"statement"`
	Detail           string             `json:"detail,omitempty"`
	Confidence       float64            `json:"confidence,omitempty"`
	EvidenceRefs     []string           `json:"evidence_refs"`
	TargetPath       string             `json:"target_path,omitempty"`
	TargetExists     bool               `json:"target_exists"`
	TargetHash       string             `json:"target_hash,omitempty"`
	StandardDraft    TasteStandardDraft `json:"standard_draft"`
	Warnings         []string           `json:"warnings,omitempty"`
}

type TasteStandardDraft struct {
	StandardID       string   `json:"standard_id"`
	Title            string   `json:"title"`
	AppliesWhen      string   `json:"applies_when"`
	PositiveStandard string   `json:"positive_standard"`
	AntiPattern      string   `json:"anti_pattern"`
	ReviewLens       string   `json:"review_lens"`
	RequiredEvidence []string `json:"required_evidence"`
	ReviewNote       string   `json:"review_note"`
}

type TastePlanPaths struct {
	PlanPath   string
	ReviewPath string
}

func BuildTastePlan(homePath, candidateID, targetPath string, now time.Time) (TastePlan, error) {
	homePath = strings.TrimSpace(homePath)
	candidateID = strings.TrimSpace(candidateID)
	if homePath == "" {
		return TastePlan{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if candidateID == "" {
		return TastePlan{}, fmt.Errorf("candidate id required")
	}
	c, err := readTasteCandidate(homePath, candidateID)
	if err != nil {
		return TastePlan{}, err
	}
	if c.Kind != KindTaste {
		return TastePlan{}, fmt.Errorf("candidate %s kind=%q, want %q", candidateID, c.Kind, KindTaste)
	}
	evidenceRefs := reviewartifact.UniqueSorted(c.EvidenceRefs)
	targetExists, targetHash, err := reviewartifact.HashFileTarget(targetPath)
	if err != nil {
		return TastePlan{}, err
	}
	key := reviewartifact.Slugify(reviewartifact.FirstNonEmpty(c.Subject, c.Detail, c.Statement, c.ID), 0)
	if key == "" {
		key = reviewartifact.Digest12(candidateID)
	}
	statement := reviewartifact.FirstNonEmpty(c.Statement, c.Subject, "reviewed taste observation")
	plan := TastePlan{
		SchemaVersion:    TastePlanSchemaVersion,
		ID:               idFor("tsp_", candidateID, targetPath),
		CreatedAt:        rfc3339(now),
		Status:           "review_required",
		CandidateID:      candidateID,
		CandidateVerdict: c.Verdict,
		TasteKey:         key,
		Subject:          c.Subject,
		Statement:        c.Statement,
		Detail:           c.Detail,
		Confidence:       c.Confidence,
		EvidenceRefs:     evidenceRefs,
		TargetPath:       strings.TrimSpace(targetPath),
		TargetExists:     targetExists,
		TargetHash:       targetHash,
		StandardDraft: TasteStandardDraft{
			StandardID:       "taste_" + key + "_" + reviewartifact.Digest12(candidateID+c.Statement),
			Title:            reviewartifact.FirstNonEmpty(c.Subject, statement),
			AppliesWhen:      "When work touches a product/design/content surface matching this taste observation.",
			PositiveStandard: statement,
			AntiPattern:      "Do not generalize this into global style law without product/context evidence.",
			ReviewLens:       "Check whether the output shows this standard concretely, cites examples, and improves the target product rather than adding generic polish.",
			RequiredEvidence: []string{
				"source example or trace that produced the taste observation",
				"product/surface where the standard applies",
				"one positive example or expected output characteristic",
				"one anti-example or failure mode",
			},
			ReviewNote: "This is a taste-standard draft from a taste observation. Reviewers must decide scope and examples before applying it to product doctrine, prompts, skills, or design checks.",
		},
		Warnings: []string{
			"does not mutate doctrine or product files",
			"does not install a prompt or skill",
			"does not promote memory truth",
		},
	}
	return plan, nil
}

func readTasteCandidate(homePath, candidateID string) (Candidate, error) {
	var c Candidate
	if err := candidates.ReadCandidate(homePath, candidates.Taste, candidateID, &c); err != nil {
		return c, err
	}
	if c.ID == "" {
		c.ID = candidateID
	}
	return c, nil
}

func RecordTastePlan(homePath string, plan TastePlan) (TastePlanPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return TastePlanPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if plan.ID == "" {
		return TastePlanPaths{}, fmt.Errorf("taste plan id required")
	}
	paths, err := reviewartifact.Write(homePath, "taste-standards", plan.ID, plan, " ", RenderTasteReview(plan))
	return TastePlanPaths{PlanPath: paths.PlanPath, ReviewPath: paths.ReviewPath}, err
}

func RenderTasteReview(plan TastePlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Taste Standard Review: %s\n\n", plan.CandidateID)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- candidate: %s\n", plan.CandidateID)
	if plan.CandidateVerdict != "" {
		fmt.Fprintf(&b, "- candidate_verdict: %s\n", plan.CandidateVerdict)
	}
	fmt.Fprintf(&b, "- taste_key: %s\n", plan.TasteKey)
	fmt.Fprintf(&b, "- standard_id: %s\n", plan.StandardDraft.StandardID)
	if plan.TargetPath != "" {
		fmt.Fprintf(&b, "- target: %s\n", plan.TargetPath)
		fmt.Fprintf(&b, "- target_exists: %t\n", plan.TargetExists)
	}
	if plan.TargetHash != "" {
		fmt.Fprintf(&b, "- target_hash: %s\n", plan.TargetHash)
	}
	fmt.Fprintf(&b, "- evidence_refs: %s\n\n", strings.Join(plan.EvidenceRefs, ", "))
	fmt.Fprintln(&b, "This owner-review artifact does not mutate doctrine or product files, install prompts/skills, or promote memory truth.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Observation")
	fmt.Fprintln(&b)
	if plan.Subject != "" {
		fmt.Fprintf(&b, "- subject: %s\n", plan.Subject)
	}
	fmt.Fprintf(&b, "- statement: %s\n", plan.Statement)
	if plan.Detail != "" {
		fmt.Fprintf(&b, "- detail: %s\n", plan.Detail)
	}
	if plan.Confidence > 0 {
		fmt.Fprintf(&b, "- confidence: %.2f\n", plan.Confidence)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Draft Standard")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- applies_when: %s\n", plan.StandardDraft.AppliesWhen)
	fmt.Fprintf(&b, "- positive_standard: %s\n", plan.StandardDraft.PositiveStandard)
	fmt.Fprintf(&b, "- anti_pattern: %s\n", plan.StandardDraft.AntiPattern)
	fmt.Fprintf(&b, "- review_lens: %s\n", plan.StandardDraft.ReviewLens)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Required Evidence Before Apply")
	fmt.Fprintln(&b)
	for _, evidence := range plan.StandardDraft.RequiredEvidence {
		fmt.Fprintf(&b, "- %s\n", evidence)
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "%s\n", plan.StandardDraft.ReviewNote)
	return b.String()
}
