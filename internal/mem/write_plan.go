package mem

import (
	"fmt"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/reviewartifact"
)

const MeaningWritePlanSchemaVersion = "compound.meaning_write_plan.v0.1"

// MeaningWritePlan is a reviewed curated-memory write artifact. It turns a
// parked/proposed memory candidate into an explicit markdown proposal without
// mutating the target knowledge source.
type MeaningWritePlan struct {
	SchemaVersion    string   `json:"schema_version"`
	ID               string   `json:"id"`
	CreatedAt        string   `json:"created_at"`
	Status           string   `json:"status"`
	CandidateID      string   `json:"candidate_id"`
	CandidateState   string   `json:"candidate_state,omitempty"`
	Entity           string   `json:"entity"`
	Predicate        string   `json:"predicate"`
	Object           string   `json:"object"`
	ClaimType        string   `json:"claim_type,omitempty"`
	Confidence       float64  `json:"confidence,omitempty"`
	TargetPath       string   `json:"target_path"`
	TargetExists     bool     `json:"target_exists"`
	TargetHash       string   `json:"target_hash,omitempty"`
	EvidenceRefs     []string `json:"evidence_refs"`
	ProposedHeading  string   `json:"proposed_heading"`
	ProposedMarkdown string   `json:"proposed_markdown"`
	Warnings         []string `json:"warnings,omitempty"`
}

type MeaningWritePlanPaths struct {
	PlanPath   string
	ReviewPath string
}

type meaningCandidateRecord struct {
	ID             string   `json:"id"`
	Kind           string   `json:"kind,omitempty"`
	Entity         string   `json:"entity"`
	Predicate      string   `json:"predicate"`
	Object         string   `json:"object"`
	ClaimType      string   `json:"claim_type,omitempty"`
	Confidence     float64  `json:"confidence,omitempty"`
	SourceTraceRef string   `json:"source_trace_ref,omitempty"`
	EvidenceRefs   []string `json:"evidence_refs,omitempty"`
	State          string   `json:"state,omitempty"`
	Verdict        string   `json:"verdict,omitempty"`
}

func (s *Store) BuildMeaningWritePlan(candidateID, targetPath string, now time.Time) (MeaningWritePlan, error) {
	candidateID = strings.TrimSpace(candidateID)
	targetPath = strings.TrimSpace(targetPath)
	if candidateID == "" {
		return MeaningWritePlan{}, fmt.Errorf("candidate id required")
	}
	if targetPath == "" {
		return MeaningWritePlan{}, fmt.Errorf("target path required")
	}
	c, err := s.readMeaningCandidate(candidateID)
	if err != nil {
		return MeaningWritePlan{}, err
	}
	if strings.TrimSpace(c.Entity) == "" || strings.TrimSpace(c.Predicate) == "" || strings.TrimSpace(c.Object) == "" {
		return MeaningWritePlan{}, fmt.Errorf("candidate %s missing entity/predicate/object", candidateID)
	}
	targetExists, targetHash, err := reviewartifact.HashFileTarget(targetPath)
	if err != nil {
		return MeaningWritePlan{}, err
	}
	evidenceRefs := reviewartifact.UniqueSorted(append(append([]string{}, c.EvidenceRefs...), c.SourceTraceRef))
	if len(evidenceRefs) == 0 {
		evidenceRefs = []string{"candidate:" + candidateID}
	}
	state := firstNonEmpty(c.Verdict, c.State, "unknown")
	heading := "Proposed memory update: " + c.Entity + " / " + c.Predicate
	plan := MeaningWritePlan{
		SchemaVersion:   MeaningWritePlanSchemaVersion,
		CreatedAt:       now.UTC().Format(time.RFC3339),
		Status:          "review_required",
		CandidateID:     candidateID,
		CandidateState:  state,
		Entity:          c.Entity,
		Predicate:       c.Predicate,
		Object:          c.Object,
		ClaimType:       c.ClaimType,
		Confidence:      c.Confidence,
		TargetPath:      targetPath,
		TargetExists:    targetExists,
		TargetHash:      targetHash,
		EvidenceRefs:    evidenceRefs,
		ProposedHeading: heading,
		Warnings:        []string{"review artifact only; does not mutate curated memory source", "promotion remains separate from source write review"},
	}
	plan.ProposedMarkdown = renderMeaningWriteMarkdown(plan)
	plan.ID = "mwp_" + reviewartifact.Digest12("meaning-write\n"+candidateID+"\n"+targetPath+"\n"+targetHash+"\n"+plan.ProposedMarkdown)
	return plan, nil
}

func (s *Store) readMeaningCandidate(candidateID string) (meaningCandidateRecord, error) {
	var c meaningCandidateRecord
	if err := candidates.ReadCandidate(s.home, candidates.Memory, candidateID, &c); err != nil {
		return c, err
	}
	if c.ID == "" {
		c.ID = candidateID
	}
	if c.Kind != "" && c.Kind != "memory" {
		return c, fmt.Errorf("candidate %s kind=%q, want memory", candidateID, c.Kind)
	}
	return c, nil
}

func RecordMeaningWritePlan(homePath string, plan MeaningWritePlan) (MeaningWritePlanPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return MeaningWritePlanPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if plan.ID == "" {
		return MeaningWritePlanPaths{}, fmt.Errorf("meaning write plan id required")
	}
	paths, err := reviewartifact.Write(homePath, "meaning-write", plan.ID, plan, " ", RenderMeaningWriteReview(plan))
	return MeaningWritePlanPaths{PlanPath: paths.PlanPath, ReviewPath: paths.ReviewPath}, err
}

func RenderMeaningWriteReview(plan MeaningWritePlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Meaning Write Review: %s\n\n", plan.CandidateID)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- target: %s\n", plan.TargetPath)
	fmt.Fprintf(&b, "- target_exists: %t\n", plan.TargetExists)
	if plan.TargetHash != "" {
		fmt.Fprintf(&b, "- target_hash: %s\n", plan.TargetHash)
	}
	fmt.Fprintf(&b, "- candidate_state: %s\n", plan.CandidateState)
	fmt.Fprintf(&b, "- entity: %s\n", plan.Entity)
	fmt.Fprintf(&b, "- predicate: %s\n", plan.Predicate)
	fmt.Fprintf(&b, "- evidence_refs: %s\n", strings.Join(plan.EvidenceRefs, ", "))
	b.WriteString("\nThis review artifact does not mutate curated memory source files or promote memory truth. Apply source edits only after review, and keep promotion/adjudication separate.\n\n")
	b.WriteString("## Proposed Markdown\n\n")
	b.WriteString(plan.ProposedMarkdown)
	if !strings.HasSuffix(plan.ProposedMarkdown, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

func renderMeaningWriteMarkdown(plan MeaningWritePlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n\n", plan.ProposedHeading)
	fmt.Fprintf(&b, "- candidate: `%s`\n", plan.CandidateID)
	fmt.Fprintf(&b, "- entity: `%s`\n", plan.Entity)
	fmt.Fprintf(&b, "- predicate: `%s`\n", plan.Predicate)
	fmt.Fprintf(&b, "- value: %s\n", plan.Object)
	if plan.ClaimType != "" {
		fmt.Fprintf(&b, "- claim_type: `%s`\n", plan.ClaimType)
	}
	if plan.Confidence > 0 {
		fmt.Fprintf(&b, "- confidence: %.2f\n", plan.Confidence)
	}
	fmt.Fprintf(&b, "- evidence_refs: %s\n", strings.Join(plan.EvidenceRefs, ", "))
	b.WriteString("- review_status: pending\n")
	return b.String()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
