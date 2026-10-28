package capgraph

import (
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	OutboundDistributionPlanSchemaVersion = "compound.capability.outbound_distribution_plan.v0.1"
	OutboundDistributionPlanAuthorityRef  = "compound://capability/outbound-distribution-plan"
)

type OutboundDistributionPlan struct {
	SchemaVersion string                          `json:"schema_version"`
	AuthorityRef  string                          `json:"authority_ref"`
	Status        string                          `json:"status"`
	CreatedAt     string                          `json:"created_at"`
	Task          string                          `json:"task"`
	AdoptionLevel string                          `json:"adoption_level"`
	RequiredGates []OutboundDistributionGate      `json:"required_gates,omitempty"`
	Candidates    []OutboundDistributionCandidate `json:"candidates,omitempty"`
	Boundaries    []string                        `json:"boundaries,omitempty"`
	NextAction    string                          `json:"next_action,omitempty"`
	Warnings      []string                        `json:"warnings,omitempty"`
}

type OutboundDistributionGate struct {
	Kind   string `json:"kind"`
	Ref    string `json:"ref"`
	Detail string `json:"detail,omitempty"`
}

type OutboundDistributionCandidate struct {
	Repo                  string   `json:"repo"`
	Role                  string   `json:"role"`
	EvidenceRef           string   `json:"evidence_ref"`
	Decision              string   `json:"decision"`
	Lane                  string   `json:"lane,omitempty"`
	Owner                 string   `json:"owner,omitempty"`
	Confidence            float64  `json:"confidence,omitempty"`
	CapabilityGraphStatus string   `json:"capability_graph_status,omitempty"`
	NodeStatus            string   `json:"node_status,omitempty"`
	NextReviewStep        string   `json:"next_review_step,omitempty"`
	Boundary              string   `json:"boundary,omitempty"`
	VerdictPath           []string `json:"verdict_path,omitempty"`
}

func BuildOutboundDistributionPlan(homePath, task string, now time.Time) (OutboundDistributionPlan, error) {
	task = strings.TrimSpace(task)
	report := OutboundDistributionPlan{
		SchemaVersion: OutboundDistributionPlanSchemaVersion,
		AuthorityRef:  OutboundDistributionPlanAuthorityRef,
		Status:        "no_distribution_evidence",
		CreatedAt:     now.UTC().Format(time.RFC3339),
		Task:          task,
		AdoptionLevel: "evidence_only",
		RequiredGates: outboundDistributionGates(),
		Boundaries: []string{
			"evidence-only evaluation; not adapter adoption",
			"no external repo clone/install/serve",
			"no social account auth, posting, replying, or public side effect",
			"no CapabilityGraph activation or memory promotion",
			"Tier-3 PolicyDecision required before any public distribution action",
		},
		NextAction: "collect or select outbound distribution evidence; do not auth or post",
	}
	intake, err := ExternalRepoIntakeReportForHome(homePath, task)
	if err != nil {
		return report, err
	}
	for _, match := range intake.Matches {
		if !outboundDistributionMatch(match) {
			continue
		}
		report.Candidates = append(report.Candidates, outboundDistributionCandidate(match))
	}
	sort.Slice(report.Candidates, func(i, j int) bool {
		if report.Candidates[i].Confidence != report.Candidates[j].Confidence {
			return report.Candidates[i].Confidence > report.Candidates[j].Confidence
		}
		return report.Candidates[i].Repo < report.Candidates[j].Repo
	})
	if len(report.Candidates) > 0 {
		report.Status = "review_ready"
		report.NextAction = "record an outbound distribution adapter verdict path; keep auth/posting behind Tier-3 policy and product-loop proof"
	}
	return report, nil
}

func outboundDistributionCandidate(match ExternalRepoIntakeDecision) OutboundDistributionCandidate {
	return OutboundDistributionCandidate{
		Repo:                  match.Repo,
		Role:                  outboundDistributionRole(match),
		EvidenceRef:           ExternalRepoIntakeAuthorityRef + "/" + normalizeExternalRepoQuery(match.Repo),
		Decision:              match.Decision,
		Lane:                  match.Lane,
		Owner:                 match.Owner,
		Confidence:            externalRepoMatchedEvidenceConfidence(match),
		CapabilityGraphStatus: match.CapabilityGraphStatus,
		NodeStatus:            match.CapabilityEvaluationNodeStatus,
		NextReviewStep:        firstNonEmpty(match.NextReviewStep, match.OwnerReviewNextAction, match.CapabilityEvaluationNextAction),
		Boundary:              match.NotToDo,
		VerdictPath: []string{
			"choose product loop target and distribution hypothesis",
			"record adapter verdict with evidence refs",
			"prove dry-run adapter receipt before any account auth",
			"require Tier-3 PolicyDecision before public posting/replying",
		},
	}
}

func outboundDistributionGates() []OutboundDistributionGate {
	return []OutboundDistributionGate{
		{
			Kind:   "tier3_public_distribution_policy",
			Ref:    "compound://policy/tier3-public-distribution",
			Detail: "public posting/replying, social account auth, spend, and destructive channel actions require explicit Tier-3 policy",
		},
		{
			Kind:   "product_loop_target",
			Ref:    "compound://product-loop/change-signal-hypothesis-review",
			Detail: "adapter evaluation must name product, change, signal source, hypothesis, observation window, and next bounded move",
		},
		{
			Kind:   "dry_run_receipt",
			Ref:    "compound://receipts/outbound-distribution-dry-run",
			Detail: "first proof is local/synthetic or read-only dry run, not account auth or live posting",
		},
		{
			Kind:   "no_social_auth_or_posting",
			Ref:    "compound://boundary/no-social-auth-or-posting",
			Detail: "this projection never clones repos, authenticates accounts, posts, replies, spends, or changes public state",
		},
	}
}

func outboundDistributionMatch(match ExternalRepoIntakeDecision) bool {
	text := strings.ToLower(strings.Join([]string{
		match.Repo,
		match.Lane,
		match.Owner,
		match.NextReviewStep,
		match.OwnerReviewNextAction,
		match.CapabilityEvaluationNextAction,
		match.NotToDo,
	}, " "))

	repo := strings.ToLower(match.Repo)
	if strings.Contains(repo, "postiz") || strings.Contains(repo, "agent-reach") {
		return true
	}
	if outboundDistributionNegated(text) {
		return false
	}
	for _, token := range []string{"outbound", "distribut", "posting", "social", "channel", "audience", "engagement"} {
		if strings.Contains(text, token) {
			return true
		}
	}
	return outboundDistributionHasWord(text, "reach")
}

func outboundDistributionRole(match ExternalRepoIntakeDecision) string {
	text := strings.ToLower(match.Repo + " " + match.Lane + " " + match.Owner + " " + match.NextReviewStep)
	if strings.Contains(text, "postiz") || strings.Contains(text, "posting") || strings.Contains(text, "adapter") {
		return "posting_adapter_candidate"
	}
	if strings.Contains(text, "agent-reach") || outboundDistributionHasWord(text, "reach") || strings.Contains(text, "channel") || strings.Contains(text, "audience") {
		return "reach_research_reference"
	}
	return "distribution_evidence"
}

func outboundDistributionNegated(text string) bool {
	for _, phrase := range []string{
		"not a distribution/reach item",
		"not distribution/reach",
		"not a distribution item",
		"not distribution item",
		"not a distribution",
		"not distribution",
		"not outbound",
	} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func outboundDistributionHasWord(text, word string) bool {
	word = strings.ToLower(strings.TrimSpace(word))
	if word == "" {
		return false
	}
	for _, field := range strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if strings.ToLower(field) == word {
			return true
		}
	}
	return false
}
