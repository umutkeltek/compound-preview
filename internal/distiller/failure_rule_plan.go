package distiller

import (
	"fmt"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/reviewartifact"
)

const FailureRulePlanSchemaVersion = "compound.distiller.failure_rule_plan.v0.1"

// FailureRulePlan is a reviewed rule proposal from a failure_pattern candidate.
// It makes Law 8 concrete without silently editing doctrine, prompts, or policy.
type FailureRulePlan struct {
	SchemaVersion    string           `json:"schema_version"`
	ID               string           `json:"id"`
	CreatedAt        string           `json:"created_at"`
	Status           string           `json:"status"`
	CandidateID      string           `json:"candidate_id"`
	CandidateVerdict string           `json:"candidate_verdict,omitempty"`
	FailureKey       string           `json:"failure_key"`
	Subject          string           `json:"subject"`
	Statement        string           `json:"statement"`
	Detail           string           `json:"detail,omitempty"`
	Confidence       float64          `json:"confidence,omitempty"`
	EvidenceRefs     []string         `json:"evidence_refs"`
	TargetPath       string           `json:"target_path,omitempty"`
	TargetExists     bool             `json:"target_exists"`
	TargetHash       string           `json:"target_hash,omitempty"`
	RuleDraft        FailureRuleDraft `json:"rule_draft"`
	Warnings         []string         `json:"warnings,omitempty"`
}

type FailureRuleDraft struct {
	RuleID            string   `json:"rule_id"`
	Title             string   `json:"title"`
	Trigger           string   `json:"trigger"`
	RequiredAction    string   `json:"required_action"`
	Verification      string   `json:"verification"`
	RetirementTrigger string   `json:"retirement_trigger"`
	RequiredEvidence  []string `json:"required_evidence"`
	ReviewNote        string   `json:"review_note"`
}

type FailureRulePlanPaths struct {
	PlanPath   string
	ReviewPath string
}

func BuildFailureRulePlan(homePath, candidateID, targetPath string, now time.Time) (FailureRulePlan, error) {
	homePath = strings.TrimSpace(homePath)
	candidateID = strings.TrimSpace(candidateID)
	if homePath == "" {
		return FailureRulePlan{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if candidateID == "" {
		return FailureRulePlan{}, fmt.Errorf("candidate id required")
	}
	c, err := readFailureCandidate(homePath, candidateID)
	if err != nil {
		return FailureRulePlan{}, err
	}
	if c.Kind != KindFailure {
		return FailureRulePlan{}, fmt.Errorf("candidate %s kind=%q, want %q", candidateID, c.Kind, KindFailure)
	}
	evidenceRefs := reviewartifact.UniqueSorted(c.EvidenceRefs)
	targetExists, targetHash, err := reviewartifact.HashFileTarget(targetPath)
	if err != nil {
		return FailureRulePlan{}, err
	}
	failureKey := reviewartifact.FirstNonEmpty(c.Subject, c.Detail, c.Statement, c.ID)
	slug := reviewartifact.Slugify(failureKey, 0)
	if slug == "" {
		slug = reviewartifact.Digest12(candidateID)
	}
	title := reviewartifact.FirstNonEmpty(c.Subject, c.Statement, "Failure pattern "+candidateID)
	plan := FailureRulePlan{
		SchemaVersion:    FailureRulePlanSchemaVersion,
		ID:               idFor("frp_", candidateID, targetPath),
		CreatedAt:        rfc3339(now),
		Status:           "review_required",
		CandidateID:      candidateID,
		CandidateVerdict: c.Verdict,
		FailureKey:       slug,
		Subject:          c.Subject,
		Statement:        c.Statement,
		Detail:           c.Detail,
		Confidence:       c.Confidence,
		EvidenceRefs:     evidenceRefs,
		TargetPath:       strings.TrimSpace(targetPath),
		TargetExists:     targetExists,
		TargetHash:       targetHash,
		RuleDraft: FailureRuleDraft{
			RuleID:            "rule_" + slug + "_" + reviewartifact.Digest12(candidateID+c.Statement),
			Title:             title,
			Trigger:           "When this failure pattern is observed again: " + reviewartifact.FirstNonEmpty(c.Statement, c.Subject, c.ID),
			RequiredAction:    "Stop repeating the failed path; run the smallest governed repair or route to owner review.",
			Verification:      "A future run cites this rule, avoids the repeated failure, and leaves a Receipt/Transition state delta.",
			RetirementTrigger: "Retire or revise when evidence shows the trigger no longer predicts the failure or the rule creates more OS tax than it removes.",
			RequiredEvidence: []string{
				"at least one concrete failing run or trace evidence ref",
				"owner/surface affected by the failure",
				"smallest repeat-prevention action",
				"verification that proves the failure did not recur",
			},
			ReviewNote: "This is a rule draft from a failure_pattern candidate. Reviewers must decide whether recurrence evidence is strong enough before applying it to doctrine, policy, skills, or hooks.",
		},
		Warnings: []string{
			"does not mutate doctrine or policy",
			"does not install a hook",
			"does not promote memory truth",
		},
	}
	return plan, nil
}

func readFailureCandidate(homePath, candidateID string) (Candidate, error) {
	var c Candidate
	if err := candidates.ReadCandidate(homePath, candidates.FailurePattern, candidateID, &c); err != nil {
		return c, err
	}
	if c.ID == "" {
		c.ID = candidateID
	}
	return c, nil
}

func RecordFailureRulePlan(homePath string, plan FailureRulePlan) (FailureRulePlanPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return FailureRulePlanPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if plan.ID == "" {
		return FailureRulePlanPaths{}, fmt.Errorf("failure rule plan id required")
	}
	paths, err := reviewartifact.Write(homePath, "failure-rules", plan.ID, plan, " ", RenderFailureRuleReview(plan))
	return FailureRulePlanPaths{PlanPath: paths.PlanPath, ReviewPath: paths.ReviewPath}, err
}

func RenderFailureRuleReview(plan FailureRulePlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Failure Rule Review: %s\n\n", plan.CandidateID)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- candidate: %s\n", plan.CandidateID)
	if plan.CandidateVerdict != "" {
		fmt.Fprintf(&b, "- candidate_verdict: %s\n", plan.CandidateVerdict)
	}
	fmt.Fprintf(&b, "- failure_key: %s\n", plan.FailureKey)
	fmt.Fprintf(&b, "- rule_id: %s\n", plan.RuleDraft.RuleID)
	if plan.TargetPath != "" {
		fmt.Fprintf(&b, "- target: %s\n", plan.TargetPath)
		fmt.Fprintf(&b, "- target_exists: %t\n", plan.TargetExists)
	}
	if plan.TargetHash != "" {
		fmt.Fprintf(&b, "- target_hash: %s\n", plan.TargetHash)
	}
	fmt.Fprintf(&b, "- evidence_refs: %s\n\n", strings.Join(plan.EvidenceRefs, ", "))
	fmt.Fprintln(&b, "This owner-review artifact does not mutate doctrine or policy, install a hook, or promote memory truth.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Failure")
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
	fmt.Fprintln(&b, "## Draft Rule")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- trigger: %s\n", plan.RuleDraft.Trigger)
	fmt.Fprintf(&b, "- required_action: %s\n", plan.RuleDraft.RequiredAction)
	fmt.Fprintf(&b, "- verification: %s\n", plan.RuleDraft.Verification)
	fmt.Fprintf(&b, "- retirement_trigger: %s\n", plan.RuleDraft.RetirementTrigger)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Required Evidence Before Apply")
	fmt.Fprintln(&b)
	for _, evidence := range plan.RuleDraft.RequiredEvidence {
		fmt.Fprintf(&b, "- %s\n", evidence)
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "%s\n", plan.RuleDraft.ReviewNote)
	return b.String()
}
