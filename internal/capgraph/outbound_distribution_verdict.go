package capgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"compound/internal/reviewartifact"
	"compound/internal/verdictflow"

	"gopkg.in/yaml.v3"
)

const (
	OutboundDistributionVerdictPlanSchemaVersion   = "compound.capability.outbound_distribution_verdict_plan.v0.1"
	OutboundDistributionVerdictReviewSchemaVersion = "compound.capability.outbound_distribution_verdict_review.v0.1"
	OutboundDistributionVerdictReportSchemaVersion = "compound.capability.outbound_distribution_verdict_review_report.v0.1"
	OutboundDistributionVerdictReviewArea          = "outbound-distribution-verdicts"
)

type OutboundDistributionVerdictPlanOptions struct {
	Task              string
	ProductID         string
	ChangeEvent       string
	Hypothesis        string
	ObservationWindow string
	DryRunReceiptRef  string
	TargetPath        string
}

type OutboundDistributionVerdictPlan struct {
	SchemaVersion     string                          `json:"schema_version"`
	ID                string                          `json:"id"`
	CreatedAt         string                          `json:"created_at"`
	Status            string                          `json:"status"`
	AuthorityRef      string                          `json:"authority_ref"`
	Task              string                          `json:"task"`
	ProductID         string                          `json:"product_id"`
	ChangeEvent       string                          `json:"change_event"`
	Hypothesis        string                          `json:"hypothesis"`
	ObservationWindow string                          `json:"observation_window"`
	DryRunReceiptRef  string                          `json:"dry_run_receipt_ref"`
	AdoptionLevel     string                          `json:"adoption_level"`
	RequiredGates     []OutboundDistributionGate      `json:"required_gates"`
	Candidates        []OutboundDistributionCandidate `json:"candidates"`
	EvidenceRefs      []string                        `json:"evidence_refs"`
	Boundaries        []string                        `json:"boundaries"`
	TargetPath        string                          `json:"target_path,omitempty"`
	TargetExists      bool                            `json:"target_exists"`
	TargetHash        string                          `json:"target_hash,omitempty"`
	ReviewDraft       OutboundDistributionReviewDraft `json:"review_draft"`
	Warnings          []string                        `json:"warnings,omitempty"`
}

type OutboundDistributionReviewDraft struct {
	ReviewID         string   `json:"review_id"`
	Title            string   `json:"title"`
	ReviewQuestion   string   `json:"review_question"`
	AllowedDecisions []string `json:"allowed_decisions"`
	RequiredEvidence []string `json:"required_evidence"`
	ReviewCommand    string   `json:"review_command"`
	ReviewNote       string   `json:"review_note"`
}

type OutboundDistributionVerdictPlanPaths struct {
	PlanPath       string
	ReviewPath     string
	ReviewYAMLPath string
}

type OutboundDistributionVerdictReview struct {
	SchemaVersion    string   `json:"schema_version" yaml:"schema_version"`
	ID               string   `json:"id" yaml:"id"`
	PlanID           string   `json:"plan_id" yaml:"plan_id"`
	ProductID        string   `json:"product_id" yaml:"product_id"`
	Decision         string   `json:"decision" yaml:"decision"`
	Rationale        string   `json:"rationale" yaml:"rationale"`
	Confidence       float64  `json:"confidence" yaml:"confidence"`
	DryRunReceiptRef string   `json:"dry_run_receipt_ref" yaml:"dry_run_receipt_ref"`
	EvidenceRefs     []string `json:"evidence_refs" yaml:"evidence_refs"`
	ReviewedAt       string   `json:"reviewed_at,omitempty" yaml:"reviewed_at,omitempty"`
}

type OutboundDistributionVerdictReviewReport struct {
	SchemaVersion    string   `json:"schema_version"`
	ID               string   `json:"id"`
	CreatedAt        string   `json:"created_at"`
	Status           string   `json:"status"`
	PlanID           string   `json:"plan_id"`
	ProductID        string   `json:"product_id"`
	Decision         string   `json:"decision"`
	DryRunReceiptRef string   `json:"dry_run_receipt_ref"`
	ReviewedPath     string   `json:"reviewed_path"`
	ReviewSourcePath string   `json:"review_source_path"`
	EvidenceRefs     []string `json:"evidence_refs"`
}

func BuildOutboundDistributionVerdictPlan(homePath string, opts OutboundDistributionVerdictPlanOptions, now time.Time) (OutboundDistributionVerdictPlan, error) {
	opts.Task = strings.TrimSpace(opts.Task)
	opts.ProductID = strings.TrimSpace(opts.ProductID)
	opts.ChangeEvent = strings.TrimSpace(opts.ChangeEvent)
	opts.Hypothesis = strings.TrimSpace(opts.Hypothesis)
	opts.ObservationWindow = strings.TrimSpace(opts.ObservationWindow)
	opts.DryRunReceiptRef = strings.TrimSpace(opts.DryRunReceiptRef)
	opts.TargetPath = strings.TrimSpace(opts.TargetPath)
	if opts.ProductID == "" {
		return OutboundDistributionVerdictPlan{}, fmt.Errorf("product_id required")
	}
	if opts.Task == "" {
		return OutboundDistributionVerdictPlan{}, fmt.Errorf("task required")
	}
	if opts.ChangeEvent == "" {
		return OutboundDistributionVerdictPlan{}, fmt.Errorf("change_event required")
	}
	if opts.Hypothesis == "" {
		return OutboundDistributionVerdictPlan{}, fmt.Errorf("hypothesis required")
	}
	if opts.ObservationWindow == "" {
		return OutboundDistributionVerdictPlan{}, fmt.Errorf("observation_window required")
	}
	if opts.DryRunReceiptRef == "" {
		return OutboundDistributionVerdictPlan{}, fmt.Errorf("dry_run_receipt_ref required")
	}

	sourcePlan, err := BuildOutboundDistributionPlan(homePath, opts.Task, now)
	if err != nil {
		return OutboundDistributionVerdictPlan{}, err
	}
	if sourcePlan.Status != "review_ready" || len(sourcePlan.Candidates) == 0 {
		return OutboundDistributionVerdictPlan{}, fmt.Errorf("no outbound distribution evidence for task %q", opts.Task)
	}
	targetExists, targetHash, err := reviewartifact.HashFileTarget(opts.TargetPath)
	if err != nil {
		return OutboundDistributionVerdictPlan{}, err
	}

	evidenceRefs := []string{opts.DryRunReceiptRef}
	for _, candidate := range sourcePlan.Candidates {
		evidenceRefs = append(evidenceRefs, candidate.EvidenceRef)
	}
	evidenceRefs = reviewartifact.UniqueSorted(evidenceRefs)
	id := outboundDistributionStableID("odvp_", opts.ProductID, opts.Task, opts.ChangeEvent, opts.Hypothesis, opts.DryRunReceiptRef)
	reviewID := "outbound_distribution_review_" + outboundDistributionSlug(opts.ProductID) + "_" + outboundDistributionStableID("", id)
	plan := OutboundDistributionVerdictPlan{
		SchemaVersion:     OutboundDistributionVerdictPlanSchemaVersion,
		ID:                id,
		CreatedAt:         now.UTC().Format(time.RFC3339),
		Status:            "review_required",
		AuthorityRef:      OutboundDistributionPlanAuthorityRef + "/verdict",
		Task:              opts.Task,
		ProductID:         opts.ProductID,
		ChangeEvent:       opts.ChangeEvent,
		Hypothesis:        opts.Hypothesis,
		ObservationWindow: opts.ObservationWindow,
		DryRunReceiptRef:  opts.DryRunReceiptRef,
		AdoptionLevel:     "verdict_only",
		RequiredGates:     outboundDistributionGates(),
		Candidates:        sourcePlan.Candidates,
		EvidenceRefs:      evidenceRefs,
		Boundaries:        sourcePlan.Boundaries,
		TargetPath:        opts.TargetPath,
		TargetExists:      targetExists,
		TargetHash:        targetHash,
		ReviewDraft: OutboundDistributionReviewDraft{
			ReviewID:         reviewID,
			Title:            "Review outbound distribution verdict for " + opts.ProductID,
			ReviewQuestion:   "Should this rented outbound distribution edge be adopted for dry-run only, parked, or rejected?",
			AllowedDecisions: []string{"adopt_for_dry_run", "park", "reject"},
			RequiredEvidence: []string{
				"product loop target: " + opts.ProductID,
				"change event: " + opts.ChangeEvent,
				"hypothesis: " + opts.Hypothesis,
				"observation window: " + opts.ObservationWindow,
				"dry-run receipt: " + opts.DryRunReceiptRef,
				"Tier-3 PolicyDecision before any public posting/replying",
			},
			ReviewCommand: "compound capability outbound-distribution-verdict-review <review.yaml>",
			ReviewNote:    "This verdict may only choose dry-run adoption, park, or reject. It does not authenticate accounts, post, reply, spend, mutate ProductSignal ledgers, activate graph nodes, or promote memory.",
		},
		Warnings: []string{
			"does not authenticate social accounts",
			"does not post, reply, spend, or change public state",
			"does not mutate ProductSignal ledgers or infer causality",
			"does not activate CapabilityGraph nodes or promote memory truth",
		},
	}
	return plan, nil
}

func RecordOutboundDistributionVerdictPlan(homePath string, plan OutboundDistributionVerdictPlan) (OutboundDistributionVerdictPlanPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return OutboundDistributionVerdictPlanPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(plan.ID) == "" {
		return OutboundDistributionVerdictPlanPaths{}, fmt.Errorf("outbound distribution verdict plan id required")
	}
	arts, err := verdictflow.RecordPlan(homePath, OutboundDistributionVerdictReviewArea, plan.ID, "  ", plan, RenderOutboundDistributionVerdictPlanReview(plan), RenderOutboundDistributionVerdictReviewYAML(plan))
	return OutboundDistributionVerdictPlanPaths{
		PlanPath:       arts.PlanPath,
		ReviewPath:     arts.ReviewPath,
		ReviewYAMLPath: arts.ReviewYAMLPath,
	}, err
}

func RenderOutboundDistributionVerdictPlanReview(plan OutboundDistributionVerdictPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Outbound Distribution Verdict Plan: %s\n\n", plan.ProductID)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- product: %s\n", plan.ProductID)
	fmt.Fprintf(&b, "- change_event: %s\n", plan.ChangeEvent)
	fmt.Fprintf(&b, "- hypothesis: %s\n", plan.Hypothesis)
	fmt.Fprintf(&b, "- observation_window: %s\n", plan.ObservationWindow)
	fmt.Fprintf(&b, "- dry_run_receipt: %s\n", plan.DryRunReceiptRef)
	if plan.TargetPath != "" {
		fmt.Fprintf(&b, "- target: %s\n", plan.TargetPath)
		fmt.Fprintf(&b, "- target_exists: %t\n", plan.TargetExists)
		if plan.TargetHash != "" {
			fmt.Fprintf(&b, "- target_hash: %s\n", plan.TargetHash)
		}
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Candidates")
	fmt.Fprintln(&b)
	for _, candidate := range plan.Candidates {
		fmt.Fprintf(&b, "- %s role=%s decision=%s evidence=%s\n", candidate.Repo, candidate.Role, candidate.Decision, candidate.EvidenceRef)
		if candidate.Boundary != "" {
			fmt.Fprintf(&b, "  boundary: %s\n", candidate.Boundary)
		}
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Required Gates")
	fmt.Fprintln(&b)
	for _, gate := range plan.RequiredGates {
		fmt.Fprintf(&b, "- %s: %s\n", gate.Kind, gate.Detail)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Required Evidence Before Verdict")
	fmt.Fprintln(&b)
	for _, evidence := range plan.ReviewDraft.RequiredEvidence {
		fmt.Fprintf(&b, "- %s\n", evidence)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Boundaries")
	fmt.Fprintln(&b)
	for _, boundary := range plan.Boundaries {
		fmt.Fprintf(&b, "- %s\n", boundary)
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "Review command: `%s`\n", plan.ReviewDraft.ReviewCommand)
	return b.String()
}

func RenderOutboundDistributionVerdictReviewYAML(plan OutboundDistributionVerdictPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version: %s\n", OutboundDistributionVerdictReviewSchemaVersion)
	fmt.Fprintf(&b, "id: %s\n", plan.ReviewDraft.ReviewID)
	fmt.Fprintf(&b, "plan_id: %s\n", plan.ID)
	fmt.Fprintf(&b, "product_id: %s\n", plan.ProductID)
	fmt.Fprintln(&b, "decision: TODO_decision_adopt_for_dry_run_park_reject")
	fmt.Fprintln(&b, "rationale: TODO_rationale")
	fmt.Fprintln(&b, "confidence: TODO_confidence_0_to_1")
	fmt.Fprintln(&b, "dry_run_receipt_ref: TODO_dry_run_receipt_ref")
	fmt.Fprintln(&b, "evidence_refs:")
	for _, ref := range plan.EvidenceRefs {
		fmt.Fprintf(&b, "  - %s\n", ref)
	}
	return b.String()
}

func LoadOutboundDistributionVerdictReview(path string) (OutboundDistributionVerdictReview, error) {
	var review OutboundDistributionVerdictReview
	data, err := os.ReadFile(path)
	if err != nil {
		return review, err
	}
	if err := yaml.Unmarshal(data, &review); err != nil {
		return review, err
	}
	return review, nil
}

func RecordOutboundDistributionVerdictReview(homePath string, review OutboundDistributionVerdictReview, reviewPath string, now time.Time) (OutboundDistributionVerdictReviewReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return OutboundDistributionVerdictReviewReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(review.PlanID) == "" {
		return OutboundDistributionVerdictReviewReport{}, fmt.Errorf("plan_id required")
	}
	if strings.TrimSpace(review.ID) == "" {
		review.ID = "review_" + review.PlanID
	}
	if !outboundDistributionAllowedReviewDecision(review.Decision) {
		return OutboundDistributionVerdictReviewReport{}, fmt.Errorf("decision %q must be adopt_for_dry_run, park, or reject", review.Decision)
	}
	if strings.TrimSpace(review.Rationale) == "" || strings.HasPrefix(strings.TrimSpace(review.Rationale), "TODO") {
		return OutboundDistributionVerdictReviewReport{}, fmt.Errorf("rationale required")
	}
	if review.Confidence < 0 || review.Confidence > 1 {
		return OutboundDistributionVerdictReviewReport{}, fmt.Errorf("confidence must be between 0 and 1")
	}
	if strings.TrimSpace(review.DryRunReceiptRef) == "" || strings.HasPrefix(strings.TrimSpace(review.DryRunReceiptRef), "TODO") {
		return OutboundDistributionVerdictReviewReport{}, fmt.Errorf("dry_run_receipt_ref required")
	}
	review.ReviewedAt = firstNonEmpty(review.ReviewedAt, now.UTC().Format(time.RFC3339))
	report := OutboundDistributionVerdictReviewReport{
		SchemaVersion:    OutboundDistributionVerdictReportSchemaVersion,
		ID:               "odvr_" + outboundDistributionStableID("", review.PlanID, review.Decision, review.Rationale),
		CreatedAt:        now.UTC().Format(time.RFC3339),
		Status:           "recorded",
		PlanID:           review.PlanID,
		ProductID:        review.ProductID,
		Decision:         review.Decision,
		DryRunReceiptRef: review.DryRunReceiptRef,
		ReviewSourcePath: strings.TrimSpace(reviewPath),
		EvidenceRefs:     reviewartifact.UniqueSorted(append(append([]string{}, review.EvidenceRefs...), review.DryRunReceiptRef)),
	}
	path, err := reviewartifact.WriteJSON(homePath, OutboundDistributionVerdictReviewArea, review.PlanID+".reviewed.json", report, "  ")
	if err != nil {
		return OutboundDistributionVerdictReviewReport{}, err
	}
	report.ReviewedPath = path
	if _, err := reviewartifact.WriteJSON(homePath, OutboundDistributionVerdictReviewArea, review.PlanID+".reviewed.json", report, "  "); err != nil {
		return OutboundDistributionVerdictReviewReport{}, err
	}
	return report, nil
}

func AnnotateOutboundDistributionVerdictQueue(report *reviewartifact.QueueReport) {
	verdictflow.AnnotateQueue(report, OutboundDistributionVerdictReviewArea, annotateOutboundDistributionVerdictItem)
}

func annotateOutboundDistributionVerdictItem(item *reviewartifact.QueueItem) {
	if reviewed, ok := outboundDistributionVerdictReviewedFromExtras(item.ExtraPaths); ok {
		item.Status = "ok"
		item.Readiness = "no_action"
		item.BlockedReason = ""
		item.NextAction = "outbound distribution verdict recorded: " + reviewed.Decision
		item.Actions = nil
		item.DecisionCard = nil
		return
	}
	plan, ok := outboundDistributionVerdictPlanFromQueueItem(*item)
	if !ok {
		item.Status = "blocked"
		item.Readiness = "blocked"
		item.BlockedReason = "outbound distribution verdict plan artifact unreadable"
		item.NextAction = "regenerate outbound distribution verdict plan"
		item.Actions = nil
		return
	}
	if strings.TrimSpace(item.Status) == "" || strings.TrimSpace(item.Status) == "unknown" {
		item.Status = "review_required"
	}
	reviewPath := outboundDistributionVerdictReviewYAMLPath(*item)
	if reviewPath == "" {
		item.Readiness = "blocked"
		item.BlockedReason = "review YAML draft missing"
		item.NextAction = "regenerate outbound distribution verdict plan"
		item.Actions = nil
		return
	}
	item.Readiness = "needs_review"
	item.BlockedReason = "review decision must be adopt_for_dry_run|park|reject; public posting remains Tier-3 gated"
	item.NextAction = "edit review YAML then record outbound distribution verdict"
	card := outboundDistributionVerdictDecisionCard(plan, reviewPath)
	item.DecisionCard = card
	item.Actions = outboundDistributionVerdictDecisionCardActions(card)
}

func outboundDistributionVerdictDecisionCard(plan OutboundDistributionVerdictPlan, reviewPath string) *reviewartifact.DecisionCard {
	command := outboundDistributionVerdictReviewCommand(reviewPath)
	card := &reviewartifact.DecisionCard{
		Claim:               "Review outbound distribution verdict for " + plan.ProductID,
		OptionalRationale:   false,
		PanelLean:           "park unless dry-run receipt and product loop target are clear",
		CanonicalActionHint: "Edit review YAML decision/rationale/confidence, then run `compound capability outbound-distribution-verdict-review <review.yaml>`. This records a verdict only; it does not auth or post.",
		Boundary: []string{
			"read-only review artifact",
			"no social auth/post/reply/spend/public side effect",
			"no ProductSignal ledger mutation",
			"no CapabilityGraph activation or memory promotion",
		},
		Meta: map[string]string{
			"product_id":          plan.ProductID,
			"change_event":        plan.ChangeEvent,
			"dry_run_receipt_ref": plan.DryRunReceiptRef,
			"review_yaml":         reviewPath,
		},
	}
	card.Why = []reviewartifact.DecisionCardReason{
		{Label: "product loop", Question: "Which product loop would consume this?", Detail: plan.ProductID + " / " + plan.ChangeEvent},
		{Label: "dry-run proof", Severity: "warning", Question: "What proof exists before any account auth?", Detail: plan.DryRunReceiptRef},
		{Label: "external evidence", Question: "Which rented edges are being evaluated?", Detail: outboundDistributionCandidateSummary(plan.Candidates)},
		{Label: "boundary", Severity: "warning", Question: "What must not happen from this verdict?", Detail: strings.Join(plan.Boundaries, "; ")},
	}
	card.Choices = []reviewartifact.DecisionCardChoice{
		{ID: "adopt_for_dry_run", Label: "Adopt for dry-run", Effect: "Allows future dry-run adapter work only; public action still needs Tier-3 PolicyDecision.", Command: command, Argv: []string{"compound", "capability", "outbound-distribution-verdict-review", reviewPath}, RequiresReview: true},
		{ID: "park", Label: "Park", Effect: "Keeps evidence available without adapter work.", Command: command, Argv: []string{"compound", "capability", "outbound-distribution-verdict-review", reviewPath}, RequiresReview: true},
		{ID: "reject", Label: "Reject", Effect: "Records that these external edges should not be adapted for this product loop.", Command: command, Argv: []string{"compound", "capability", "outbound-distribution-verdict-review", reviewPath}, RequiresReview: true},
	}
	return card
}

func outboundDistributionVerdictDecisionCardActions(card *reviewartifact.DecisionCard) []reviewartifact.Action {
	if card == nil {
		return nil
	}
	return []reviewartifact.Action{{
		ID:             "record_outbound_distribution_verdict",
		Label:          "Record outbound distribution verdict",
		Command:        outboundDistributionVerdictReviewCommand(card.Meta["review_yaml"]),
		Argv:           []string{"compound", "capability", "outbound-distribution-verdict-review", card.Meta["review_yaml"]},
		RequiresReview: true,
	}}
}

func outboundDistributionVerdictPlanFromQueueItem(item reviewartifact.QueueItem) (OutboundDistributionVerdictPlan, bool) {
	var plan OutboundDistributionVerdictPlan
	data, err := os.ReadFile(item.JSONPath)
	if err != nil {
		return plan, false
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		return plan, false
	}
	return plan, plan.ID != ""
}

func outboundDistributionVerdictReviewedFromExtras(paths []string) (OutboundDistributionVerdictReviewReport, bool) {
	return verdictflow.LatestReviewedFromExtras(paths,
		func(r OutboundDistributionVerdictReviewReport) bool { return r.Status == "recorded" },
		func(r OutboundDistributionVerdictReviewReport) string { return r.ID },
		func(r OutboundDistributionVerdictReviewReport) string { return r.CreatedAt },
	)
}

func outboundDistributionVerdictReviewYAMLPath(item reviewartifact.QueueItem) string {
	for _, path := range item.ExtraPaths {
		if strings.HasSuffix(path, ".review.yaml") {
			return path
		}
	}
	return ""
}

func outboundDistributionVerdictReviewCommand(reviewPath string) string {
	if strings.TrimSpace(reviewPath) == "" {
		return "compound capability outbound-distribution-verdict-review <review.yaml>"
	}
	quoted := shellQuote(reviewPath)
	return "$EDITOR " + quoted + " && compound capability outbound-distribution-verdict-review " + quoted
}

func outboundDistributionAllowedReviewDecision(decision string) bool {
	switch strings.TrimSpace(decision) {
	case "adopt_for_dry_run", "park", "reject":
		return true
	default:
		return false
	}
}

func outboundDistributionCandidateSummary(candidates []OutboundDistributionCandidate) string {
	var parts []string
	for _, candidate := range candidates {
		parts = append(parts, candidate.Repo+" ("+candidate.Role+")")
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func outboundDistributionSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	prevUnderscore := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevUnderscore = false
			continue
		}
		if !prevUnderscore {
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}

func outboundDistributionStableID(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return prefix + hex.EncodeToString(sum[:])[:12]
}
