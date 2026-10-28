package routeevidence

import (
	"fmt"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/reviewartifact"
)

const RoutePolicyPlanSchemaVersion = "compound.route_policy_plan.v0.1"

// RoutePolicyPlan is a reviewed routing-policy proposal from a
// route_observation candidate. It never edits gateways, provider registries,
// or model-router config.
type RoutePolicyPlan struct {
	SchemaVersion    string           `json:"schema_version"`
	ID               string           `json:"id"`
	CreatedAt        string           `json:"created_at"`
	Status           string           `json:"status"`
	CandidateID      string           `json:"candidate_id"`
	CandidateVerdict string           `json:"candidate_verdict,omitempty"`
	RouteKey         string           `json:"route_key"`
	Subject          string           `json:"subject"`
	Statement        string           `json:"statement"`
	Detail           string           `json:"detail,omitempty"`
	Confidence       float64          `json:"confidence,omitempty"`
	EvidenceRefs     []string         `json:"evidence_refs"`
	TargetPath       string           `json:"target_path,omitempty"`
	TargetExists     bool             `json:"target_exists"`
	TargetHash       string           `json:"target_hash,omitempty"`
	PolicyDraft      RoutePolicyDraft `json:"policy_draft"`
	Warnings         []string         `json:"warnings,omitempty"`
}

type RoutePolicyDraft struct {
	PolicyID          string   `json:"policy_id"`
	Title             string   `json:"title"`
	AppliesWhen       string   `json:"applies_when"`
	RoutePreference   string   `json:"route_preference"`
	CostGuard         string   `json:"cost_guard"`
	EscalationTrigger string   `json:"escalation_trigger"`
	Verification      string   `json:"verification"`
	RequiredEvidence  []string `json:"required_evidence"`
	ReviewNote        string   `json:"review_note"`
}

type RoutePolicyPlanPaths struct {
	PlanPath   string
	ReviewPath string
}

type routeCandidate struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	Subject      string   `json:"subject,omitempty"`
	Statement    string   `json:"statement,omitempty"`
	Detail       string   `json:"detail,omitempty"`
	Confidence   float64  `json:"confidence,omitempty"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
	Verdict      string   `json:"verdict,omitempty"`
}

func BuildRoutePolicyPlan(homePath, candidateID, targetPath string, now time.Time) (RoutePolicyPlan, error) {
	homePath = strings.TrimSpace(homePath)
	candidateID = strings.TrimSpace(candidateID)
	if homePath == "" {
		return RoutePolicyPlan{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if candidateID == "" {
		return RoutePolicyPlan{}, fmt.Errorf("candidate id required")
	}
	c, err := readRouteCandidate(homePath, candidateID)
	if err != nil {
		return RoutePolicyPlan{}, err
	}
	if c.Kind != string(candidates.RouteObservation) {
		return RoutePolicyPlan{}, fmt.Errorf("candidate %s kind=%q, want %q", candidateID, c.Kind, candidates.RouteObservation)
	}
	evidenceRefs := reviewartifact.UniqueSorted(c.EvidenceRefs)
	targetExists, targetHash, err := reviewartifact.HashFileTarget(targetPath)
	if err != nil {
		return RoutePolicyPlan{}, err
	}
	key := reviewartifact.Slugify(reviewartifact.FirstNonEmpty(c.Subject, c.Detail, c.Statement, c.ID), 0)
	if key == "" {
		key = reviewartifact.Digest12(candidateID)
	}
	statement := reviewartifact.FirstNonEmpty(c.Statement, c.Subject, "reviewed routing observation")
	plan := RoutePolicyPlan{
		SchemaVersion:    RoutePolicyPlanSchemaVersion,
		ID:               idFor("rpp_", candidateID, targetPath),
		CreatedAt:        rfc3339(now),
		Status:           "review_required",
		CandidateID:      candidateID,
		CandidateVerdict: c.Verdict,
		RouteKey:         key,
		Subject:          c.Subject,
		Statement:        c.Statement,
		Detail:           c.Detail,
		Confidence:       c.Confidence,
		EvidenceRefs:     evidenceRefs,
		TargetPath:       strings.TrimSpace(targetPath),
		TargetExists:     targetExists,
		TargetHash:       targetHash,
		PolicyDraft: RoutePolicyDraft{
			PolicyID:          "route_" + key + "_" + reviewartifact.Digest12(candidateID+c.Statement),
			Title:             reviewartifact.FirstNonEmpty(c.Subject, statement),
			AppliesWhen:       "When a future work item matches this routing observation and its privacy/risk envelope.",
			RoutePreference:   statement,
			CostGuard:         "Use the cheapest sufficient lane only when capability, privacy, latency, and eval gates are satisfied.",
			EscalationTrigger: "Escalate to a stronger or human-approved lane when the cheap lane lacks required capability, evidence quality, or permission.",
			Verification:      "A future ExecutionRouteEvidence row cites this policy draft or its accepted successor and the resulting Transition records routing/cost projections without claiming task success.",
			RequiredEvidence: []string{
				"one or more ExecutionRouteEvidence refs or trace refs",
				"task class or capability class the route applies to",
				"selected target and rejected/fallback target rationale",
				"cost/latency/privacy/capability tradeoff evidence",
			},
			ReviewNote: "This is a routing-policy draft from a route_observation candidate. Reviewers must decide whether evidence is strong enough before applying it to model-router, gateway, or provider config.",
		},
		Warnings: []string{
			"does not mutate gateway or provider config",
			"does not change routing policy",
			"does not promote task success",
		},
	}
	return plan, nil
}

func readRouteCandidate(homePath, candidateID string) (routeCandidate, error) {
	var c routeCandidate
	if err := candidates.ReadCandidate(homePath, candidates.RouteObservation, candidateID, &c); err != nil {
		return c, err
	}
	if c.ID == "" {
		c.ID = candidateID
	}
	return c, nil
}

func RecordRoutePolicyPlan(homePath string, plan RoutePolicyPlan) (RoutePolicyPlanPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return RoutePolicyPlanPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if plan.ID == "" {
		return RoutePolicyPlanPaths{}, fmt.Errorf("route policy plan id required")
	}
	paths, err := reviewartifact.Write(homePath, "route-policy", plan.ID, plan, " ", RenderRoutePolicyReview(plan))
	return RoutePolicyPlanPaths{PlanPath: paths.PlanPath, ReviewPath: paths.ReviewPath}, err
}

func RenderRoutePolicyReview(plan RoutePolicyPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Route Policy Review: %s\n\n", plan.CandidateID)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- candidate: %s\n", plan.CandidateID)
	if plan.CandidateVerdict != "" {
		fmt.Fprintf(&b, "- candidate_verdict: %s\n", plan.CandidateVerdict)
	}
	fmt.Fprintf(&b, "- route_key: %s\n", plan.RouteKey)
	fmt.Fprintf(&b, "- policy_id: %s\n", plan.PolicyDraft.PolicyID)
	if plan.TargetPath != "" {
		fmt.Fprintf(&b, "- target: %s\n", plan.TargetPath)
		fmt.Fprintf(&b, "- target_exists: %t\n", plan.TargetExists)
	}
	if plan.TargetHash != "" {
		fmt.Fprintf(&b, "- target_hash: %s\n", plan.TargetHash)
	}
	fmt.Fprintf(&b, "- evidence_refs: %s\n\n", strings.Join(plan.EvidenceRefs, ", "))
	fmt.Fprintln(&b, "This owner-review artifact does not mutate gateway/provider config, change routing policy, or claim task success.")
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
	fmt.Fprintln(&b, "## Draft Policy")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- applies_when: %s\n", plan.PolicyDraft.AppliesWhen)
	fmt.Fprintf(&b, "- route_preference: %s\n", plan.PolicyDraft.RoutePreference)
	fmt.Fprintf(&b, "- cost_guard: %s\n", plan.PolicyDraft.CostGuard)
	fmt.Fprintf(&b, "- escalation_trigger: %s\n", plan.PolicyDraft.EscalationTrigger)
	fmt.Fprintf(&b, "- verification: %s\n", plan.PolicyDraft.Verification)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Required Evidence Before Apply")
	fmt.Fprintln(&b)
	for _, evidence := range plan.PolicyDraft.RequiredEvidence {
		fmt.Fprintf(&b, "- %s\n", evidence)
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "%s\n", plan.PolicyDraft.ReviewNote)
	return b.String()
}
