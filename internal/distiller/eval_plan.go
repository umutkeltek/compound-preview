package distiller

import (
	"fmt"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/reviewartifact"
)

const EvalPlanSchemaVersion = "compound.distiller.eval_plan.v0.1"

// EvalPlan is a reviewed distiller regression proposal. It turns an
// operationalized eval_case candidate into an explicit fixture draft without
// mutating the eval corpus.
type EvalPlan struct {
	SchemaVersion    string           `json:"schema_version"`
	ID               string           `json:"id"`
	CreatedAt        string           `json:"created_at"`
	Status           string           `json:"status"`
	CandidateID      string           `json:"candidate_id"`
	CandidateVerdict string           `json:"candidate_verdict,omitempty"`
	Subject          string           `json:"subject"`
	Statement        string           `json:"statement"`
	Detail           string           `json:"detail,omitempty"`
	Confidence       float64          `json:"confidence,omitempty"`
	EvidenceRefs     []string         `json:"evidence_refs"`
	TargetCorpusDir  string           `json:"target_corpus_dir,omitempty"`
	TargetExists     bool             `json:"target_exists"`
	TargetHash       string           `json:"target_hash,omitempty"`
	FixtureDraft     EvalFixtureDraft `json:"fixture_draft"`
	Warnings         []string         `json:"warnings,omitempty"`
}

type EvalFixtureDraft struct {
	SchemaVersion     string             `json:"schema_version"`
	ProposedCaseID    string             `json:"proposed_case_id"`
	SeedTraces        []string           `json:"seed_traces,omitempty"`
	RequestTraceRef   string             `json:"request_trace_ref"`
	ExpectedDecisions []ExpectedDecision `json:"expected_decisions"`
	RequiredArtifacts []string           `json:"required_artifacts"`
	ReviewNote        string             `json:"review_note"`
}

type EvalPlanPaths struct {
	PlanPath   string
	ReviewPath string
}

// BuildEvalPlan reads an eval_case candidate from COMPOUND_HOME and proposes a
// reviewed fixture draft. The draft is intentionally not executable until a
// reviewer attaches exact request/response artifacts.
func BuildEvalPlan(homePath, candidateID, targetCorpusDir string, now time.Time) (EvalPlan, error) {
	homePath = strings.TrimSpace(homePath)
	candidateID = strings.TrimSpace(candidateID)
	if homePath == "" {
		return EvalPlan{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if candidateID == "" {
		return EvalPlan{}, fmt.Errorf("candidate id required")
	}
	c, err := readEvalCandidate(homePath, candidateID)
	if err != nil {
		return EvalPlan{}, err
	}
	if c.Kind != KindEvalCase {
		return EvalPlan{}, fmt.Errorf("candidate %s kind=%q, want %q", candidateID, c.Kind, KindEvalCase)
	}
	evidenceRefs := reviewartifact.UniqueSorted(c.EvidenceRefs)
	targetExists, targetHash, err := reviewartifact.HashTreeTarget(targetCorpusDir)
	if err != nil {
		return EvalPlan{}, err
	}
	traceRef := firstEvalTraceRef(c, evidenceRefs)
	proposedID := "eval_" + reviewartifact.Slugify(c.Subject, 0)
	if proposedID == "eval_" {
		proposedID = "eval_" + reviewartifact.Slugify(candidateID, 0)
	}
	proposedID += "_" + reviewartifact.Digest12(candidateID+c.Statement)
	plan := EvalPlan{
		SchemaVersion:    EvalPlanSchemaVersion,
		ID:               idFor("evp_", candidateID, targetCorpusDir),
		CreatedAt:        rfc3339(now),
		Status:           "review_required",
		CandidateID:      candidateID,
		CandidateVerdict: c.Verdict,
		Subject:          c.Subject,
		Statement:        c.Statement,
		Detail:           c.Detail,
		Confidence:       c.Confidence,
		EvidenceRefs:     evidenceRefs,
		TargetCorpusDir:  strings.TrimSpace(targetCorpusDir),
		TargetExists:     targetExists,
		TargetHash:       targetHash,
		FixtureDraft: EvalFixtureDraft{
			SchemaVersion:   EvalCaseSchemaVersion,
			ProposedCaseID:  proposedID,
			SeedTraces:      evidenceRefs,
			RequestTraceRef: traceRef,
			ExpectedDecisions: []ExpectedDecision{{
				Kind:          KindEvalCase,
				Verdict:       Operationalize,
				Relation:      "new",
				BasisContains: "eval corpus",
			}},
			RequiredArtifacts: []string{
				"exact LLMExtractionRequest source fixture",
				"exact LLMExtractionResponse fixture",
				"reviewed expected adjudication decisions",
			},
			ReviewNote: "This is a draft regression target from an eval_case candidate. Reviewers must attach exact fixtures before adding a runnable *-eval.json case.",
		},
		Warnings: []string{
			"does not mutate eval corpus",
			"does not call a model",
			"does not promote memory truth",
		},
	}
	return plan, nil
}

func readEvalCandidate(homePath, candidateID string) (Candidate, error) {
	var c Candidate
	if err := candidates.ReadCandidate(homePath, candidates.EvalCase, candidateID, &c); err != nil {
		return c, err
	}
	if c.ID == "" {
		c.ID = candidateID
	}
	return c, nil
}

func RecordEvalPlan(homePath string, plan EvalPlan) (EvalPlanPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return EvalPlanPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if plan.ID == "" {
		return EvalPlanPaths{}, fmt.Errorf("eval plan id required")
	}
	paths, err := reviewartifact.Write(homePath, "distiller-eval", plan.ID, plan, " ", RenderEvalPlanReview(plan))
	return EvalPlanPaths{PlanPath: paths.PlanPath, ReviewPath: paths.ReviewPath}, err
}

func RenderEvalPlanReview(plan EvalPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Distiller Eval Plan: %s\n\n", plan.CandidateID)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- candidate: %s\n", plan.CandidateID)
	if plan.CandidateVerdict != "" {
		fmt.Fprintf(&b, "- candidate_verdict: %s\n", plan.CandidateVerdict)
	}
	fmt.Fprintf(&b, "- proposed_case: %s\n", plan.FixtureDraft.ProposedCaseID)
	fmt.Fprintf(&b, "- request_trace_ref: %s\n", plan.FixtureDraft.RequestTraceRef)
	if plan.TargetCorpusDir != "" {
		fmt.Fprintf(&b, "- target_corpus: %s\n", plan.TargetCorpusDir)
		fmt.Fprintf(&b, "- target_exists: %t\n", plan.TargetExists)
	}
	if plan.TargetHash != "" {
		fmt.Fprintf(&b, "- target_hash: %s\n", plan.TargetHash)
	}
	fmt.Fprintf(&b, "- evidence_refs: %s\n\n", strings.Join(plan.EvidenceRefs, ", "))
	fmt.Fprintln(&b, "This owner-review artifact does not mutate the eval corpus, call a model, or promote memory truth.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Candidate")
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
	fmt.Fprintln(&b, "## Required Before Corpus Apply")
	fmt.Fprintln(&b)
	for _, artifact := range plan.FixtureDraft.RequiredArtifacts {
		fmt.Fprintf(&b, "- %s\n", artifact)
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "%s\n", plan.FixtureDraft.ReviewNote)
	return b.String()
}

func firstEvalTraceRef(c Candidate, refs []string) string {
	for _, ref := range refs {
		if strings.HasPrefix(ref, "trace:") || strings.HasPrefix(ref, "trace_envelope:") || strings.HasPrefix(ref, "raw_trace:") {
			return ref
		}
	}
	return "candidate:" + c.ID
}
