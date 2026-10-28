package capgraph

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/reviewartifact"
)

const SkillPlanSchemaVersion = "compound.capability.skill_plan.v0.1"

// SkillPlan is a reviewed CapabilityGraph node proposal from a skill candidate.
// It never installs a skill, writes AGENTS/CLAUDE/Codex files, or mutates the graph.
type SkillPlan struct {
	SchemaVersion    string         `json:"schema_version"`
	ID               string         `json:"id"`
	CreatedAt        string         `json:"created_at"`
	Status           string         `json:"status"`
	CandidateID      string         `json:"candidate_id"`
	CandidateVerdict string         `json:"candidate_verdict,omitempty"`
	SkillKey         string         `json:"skill_key"`
	Subject          string         `json:"subject"`
	Statement        string         `json:"statement"`
	Detail           string         `json:"detail,omitempty"`
	Confidence       float64        `json:"confidence,omitempty"`
	EvidenceRefs     []string       `json:"evidence_refs"`
	TargetPath       string         `json:"target_path,omitempty"`
	TargetExists     bool           `json:"target_exists"`
	TargetHash       string         `json:"target_hash,omitempty"`
	NodeDraft        SkillNodeDraft `json:"node_draft"`
	Warnings         []string       `json:"warnings,omitempty"`
}

type SkillNodeDraft struct {
	NodeID           string   `json:"node_id"`
	Kind             string   `json:"kind"`
	Name             string   `json:"name"`
	Triggers         []string `json:"triggers"`
	Weight           float64  `json:"weight"`
	CompatibleWith   []string `json:"compatible_with,omitempty"`
	Implementation   string   `json:"implementation"`
	RequiredEvidence []string `json:"required_evidence"`
	ReviewNote       string   `json:"review_note"`
}

type SkillPlanPaths struct {
	PlanPath   string
	ReviewPath string
}

type skillCandidate struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	Subject      string   `json:"subject,omitempty"`
	Statement    string   `json:"statement,omitempty"`
	Detail       string   `json:"detail,omitempty"`
	Confidence   float64  `json:"confidence,omitempty"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
	Verdict      string   `json:"verdict,omitempty"`
}

func BuildSkillPlan(homePath, candidateID, targetPath string, now time.Time) (SkillPlan, error) {
	homePath = strings.TrimSpace(homePath)
	candidateID = strings.TrimSpace(candidateID)
	targetPath = strings.TrimSpace(targetPath)
	if homePath == "" {
		return SkillPlan{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if candidateID == "" {
		return SkillPlan{}, fmt.Errorf("candidate id required")
	}
	c, err := readSkillCandidate(homePath, candidateID)
	if err != nil {
		return SkillPlan{}, err
	}
	if c.Kind != string(candidates.Skill) {
		return SkillPlan{}, fmt.Errorf("candidate %s kind=%q, want %q", candidateID, c.Kind, candidates.Skill)
	}
	if c.Verdict != candidates.Park && c.Verdict != candidates.Operationalize {
		return SkillPlan{}, fmt.Errorf("candidate %s verdict=%q, want park or operationalize", candidateID, c.Verdict)
	}
	targetExists, targetHash, err := reviewartifact.HashFileTarget(targetPath)
	if err != nil {
		return SkillPlan{}, err
	}
	evidenceRefs := append([]string(nil), c.EvidenceRefs...)
	sort.Strings(evidenceRefs)
	key := reviewartifact.Slugify(reviewartifact.FirstNonEmpty(c.Subject, c.Detail, c.Statement, c.ID), 48)
	if key == "" {
		key = reviewartifact.Digest12(candidateID)
	}
	name := reviewartifact.FirstNonEmpty(c.Subject, c.Statement, key)
	statement := reviewartifact.FirstNonEmpty(c.Statement, c.Subject, "reviewed skill candidate")
	triggers := planSkillTriggers(c.Subject, c.Statement, c.Detail)
	plan := SkillPlan{
		SchemaVersion:    SkillPlanSchemaVersion,
		ID:               reviewartifact.ID("skp_", candidateID, targetPath),
		CreatedAt:        reviewartifact.Timestamp(now),
		Status:           "review_required",
		CandidateID:      candidateID,
		CandidateVerdict: c.Verdict,
		SkillKey:         key,
		Subject:          c.Subject,
		Statement:        c.Statement,
		Detail:           c.Detail,
		Confidence:       c.Confidence,
		EvidenceRefs:     evidenceRefs,
		TargetPath:       targetPath,
		TargetExists:     targetExists,
		TargetHash:       targetHash,
		NodeDraft: SkillNodeDraft{
			NodeID:           "skill:" + key,
			Kind:             "skill",
			Name:             name,
			Triggers:         triggers,
			Weight:           1,
			Implementation:   statement,
			RequiredEvidence: []string{"source trace that produced the skill candidate", "clear trigger conditions", "expected input and output contract", "one validation receipt or eval showing useful behavior"},
			ReviewNote:       "This skill draft is a CapabilityGraph node proposal. Reviewers must decide whether to import it as a skill/procedure/lens/tool wrapper and whether a deterministic script should replace LLM execution.",
		},
		Warnings: []string{
			"does not mutate CapabilityGraph",
			"does not install skill files",
			"does not execute tools or MCPs",
			"does not promote memory truth",
		},
	}
	return plan, nil
}

func readSkillCandidate(homePath, candidateID string) (skillCandidate, error) {
	var c skillCandidate
	if err := candidates.ReadCandidate(homePath, candidates.Skill, candidateID, &c); err != nil {
		return c, err
	}
	if c.ID == "" {
		c.ID = candidateID
	}
	return c, nil
}

func RecordSkillPlan(homePath string, plan SkillPlan) (SkillPlanPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return SkillPlanPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if plan.ID == "" {
		return SkillPlanPaths{}, fmt.Errorf("skill plan id required")
	}
	paths, err := reviewartifact.Write(homePath, "capability-skills", plan.ID, plan, "  ", RenderSkillPlanReview(plan))
	return SkillPlanPaths{PlanPath: paths.PlanPath, ReviewPath: paths.ReviewPath}, err
}

func RenderSkillPlanReview(plan SkillPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Capability Skill Review: %s\n\n", plan.CandidateID)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- candidate: %s\n", plan.CandidateID)
	if plan.CandidateVerdict != "" {
		fmt.Fprintf(&b, "- candidate_verdict: %s\n", plan.CandidateVerdict)
	}
	fmt.Fprintf(&b, "- skill_key: %s\n", plan.SkillKey)
	fmt.Fprintf(&b, "- node_id: %s\n", plan.NodeDraft.NodeID)
	if plan.TargetPath != "" {
		fmt.Fprintf(&b, "- target: %s\n", plan.TargetPath)
		fmt.Fprintf(&b, "- target_exists: %t\n", plan.TargetExists)
	}
	if plan.TargetHash != "" {
		fmt.Fprintf(&b, "- target_hash: %s\n", plan.TargetHash)
	}
	fmt.Fprintf(&b, "- evidence_refs: %s\n\n", strings.Join(plan.EvidenceRefs, ", "))
	fmt.Fprintln(&b, "This owner-review artifact does not mutate CapabilityGraph, install skill files, execute tools/MCPs, or promote memory truth.")
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
	fmt.Fprintln(&b, "## Draft CapabilityGraph Node")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- id: %s\n", plan.NodeDraft.NodeID)
	fmt.Fprintf(&b, "- kind: %s\n", plan.NodeDraft.Kind)
	fmt.Fprintf(&b, "- name: %s\n", plan.NodeDraft.Name)
	fmt.Fprintf(&b, "- triggers: %s\n", strings.Join(plan.NodeDraft.Triggers, ", "))
	fmt.Fprintf(&b, "- weight: %.1f\n", plan.NodeDraft.Weight)
	fmt.Fprintf(&b, "- implementation: %s\n", plan.NodeDraft.Implementation)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Required Evidence Before Import")
	fmt.Fprintln(&b)
	for _, evidence := range plan.NodeDraft.RequiredEvidence {
		fmt.Fprintf(&b, "- %s\n", evidence)
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "%s\n", plan.NodeDraft.ReviewNote)
	return b.String()
}

func planSkillTriggers(values ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		for _, tok := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
		}) {
			if len(tok) < 3 || seen[tok] {
				continue
			}
			seen[tok] = true
			out = append(out, tok)
			if len(out) >= 8 {
				return out
			}
		}
	}
	if len(out) == 0 {
		out = []string{"review"}
	}
	return out
}
