package product

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/mem"
	"compound/internal/panel"
	"compound/internal/reviewartifact"
	"compound/internal/verdictflow"
)

const (
	ObservationVerdictPlanSchemaVersion         = "compound.product_observation_verdict_plan.v0.1"
	ObservationVerdictReviewSchemaVersion       = "compound.product_observation_verdict_review.v0.1"
	ObservationVerdictReviewReportSchemaVersion = "compound.product_observation_verdict_review_report.v0.1"
	ObservationVerdictReviewArea                = "product-observation-verdicts"
)

type ObservationVerdictPlan struct {
	SchemaVersion     string                  `json:"schema_version"`
	ID                string                  `json:"id"`
	CreatedAt         string                  `json:"created_at"`
	Status            string                  `json:"status"`
	ProductID         string                  `json:"product_id"`
	VerdictID         string                  `json:"verdict_id"`
	RunID             string                  `json:"run_id"`
	TargetKind        string                  `json:"target_kind"`
	TargetRef         string                  `json:"target_ref"`
	PanelDecision     string                  `json:"panel_decision"`
	PanelRationale    string                  `json:"panel_rationale,omitempty"`
	ObservationID     string                  `json:"observation_id,omitempty"`
	ObservationKind   string                  `json:"observation_kind,omitempty"`
	SourceKind        string                  `json:"source_kind,omitempty"`
	SourceRef         string                  `json:"source_ref,omitempty"`
	Statement         string                  `json:"statement,omitempty"`
	Detail            string                  `json:"detail,omitempty"`
	ChangeEvent       string                  `json:"change_event"`
	ObservationWindow string                  `json:"observation_window"`
	NextAction        string                  `json:"next_action"`
	EvidenceRefs      []string                `json:"evidence_refs"`
	TargetPath        string                  `json:"target_path,omitempty"`
	TargetExists      bool                    `json:"target_exists"`
	TargetHash        string                  `json:"target_hash,omitempty"`
	ReviewDraft       ObservationVerdictDraft `json:"review_draft"`
	Warnings          []string                `json:"warnings,omitempty"`
	ReviewedAt        string                  `json:"reviewed_at,omitempty"`
	ReviewID          string                  `json:"review_id,omitempty"`
	ReviewDecision    string                  `json:"review_decision,omitempty"`
	HypothesisID      string                  `json:"hypothesis_id,omitempty"`
	ReviewEvidenceID  string                  `json:"review_evidence_id,omitempty"`
	ReviewTransition  string                  `json:"review_transition_id,omitempty"`
}

type ObservationVerdictDraft struct {
	ReviewID          string   `json:"review_id"`
	Title             string   `json:"title"`
	AllowedDecisions  []string `json:"allowed_decisions"`
	ReviewCommand     string   `json:"review_command"`
	RequiredEvidence  []string `json:"required_evidence"`
	PrimaryMetrics    []string `json:"primary_metrics"`
	ObservationWindow string   `json:"observation_window"`
	DecisionGate      string   `json:"decision_gate"`
	NextAction        string   `json:"next_action"`
}

type ObservationVerdictPlanPaths struct {
	PlanPath       string `json:"plan_path"`
	ReviewPath     string `json:"review_path"`
	ReviewYAMLPath string `json:"review_yaml_path"`
}

type ObservationVerdictReview struct {
	SchemaVersion     string   `json:"schema_version" yaml:"schema_version"`
	ID                string   `json:"id" yaml:"id"`
	PlanID            string   `json:"plan_id" yaml:"plan_id"`
	ProductID         string   `json:"product_id" yaml:"product_id"`
	VerdictID         string   `json:"verdict_id" yaml:"verdict_id"`
	Decision          string   `json:"decision" yaml:"decision"`
	Rationale         string   `json:"rationale" yaml:"rationale"`
	Confidence        float64  `json:"confidence" yaml:"confidence"`
	ChangeEvent       string   `json:"change_event,omitempty" yaml:"change_event,omitempty"`
	ExpectedEffect    string   `json:"expected_effect,omitempty" yaml:"expected_effect,omitempty"`
	PrimaryMetrics    []string `json:"primary_metrics,omitempty" yaml:"primary_metrics,omitempty"`
	ObservationWindow string   `json:"observation_window,omitempty" yaml:"observation_window,omitempty"`
	NextAction        string   `json:"next_action,omitempty" yaml:"next_action,omitempty"`
	EvidenceRefs      []string `json:"evidence_refs" yaml:"evidence_refs"`
	ReviewedAt        string   `json:"reviewed_at,omitempty" yaml:"reviewed_at,omitempty"`
	AppliedAt         string   `json:"applied_at,omitempty" yaml:"applied_at,omitempty"`
	EvidenceID        string   `json:"evidence_id,omitempty" yaml:"evidence_id,omitempty"`
	SourceRef         string   `json:"source_ref,omitempty" yaml:"source_ref,omitempty"`
}

type ObservationVerdictReviewReport struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	CreatedAt     string   `json:"created_at"`
	Status        string   `json:"status"`
	ProductID     string   `json:"product_id"`
	VerdictID     string   `json:"verdict_id"`
	PlanID        string   `json:"plan_id"`
	ReviewID      string   `json:"review_id"`
	Decision      string   `json:"decision"`
	EvidenceID    string   `json:"evidence_id"`
	EvidenceRefs  []string `json:"evidence_refs"`
	TransitionID  string   `json:"transition_id,omitempty"`
	HypothesisID  string   `json:"hypothesis_id,omitempty"`
	ReviewPath    string   `json:"review_path,omitempty"`
}

type observationVerdictCandidatePayload struct {
	ID              string   `json:"id"`
	Kind            string   `json:"kind"`
	Subject         string   `json:"subject"`
	Statement       string   `json:"statement"`
	Detail          string   `json:"detail"`
	Confidence      float64  `json:"confidence"`
	State           string   `json:"state,omitempty"`
	Verdict         string   `json:"verdict,omitempty"`
	EvidenceRefs    []string `json:"evidence_refs,omitempty"`
	ObservationID   string   `json:"observation_id,omitempty"`
	ObservationKind string   `json:"observation_kind,omitempty"`
	SourceKind      string   `json:"source_kind,omitempty"`
	SourceRef       string   `json:"source_ref,omitempty"`
	SubjectKind     string   `json:"subject_kind,omitempty"`
	SubjectID       string   `json:"subject_id,omitempty"`
	Domain          string   `json:"domain,omitempty"`
	Task            string   `json:"task,omitempty"`
}

func BuildObservationVerdictPlan(homePath, verdictPath, targetPath string, now time.Time) (ObservationVerdictPlan, error) {
	if strings.TrimSpace(homePath) == "" {
		return ObservationVerdictPlan{}, fmt.Errorf("COMPOUND_HOME required")
	}
	verdictPath = strings.TrimSpace(verdictPath)
	if verdictPath == "" {
		return ObservationVerdictPlan{}, fmt.Errorf("panel verdict path required")
	}
	verdict, err := panel.LoadVerdictArtifact(verdictPath)
	if err != nil {
		return ObservationVerdictPlan{}, err
	}
	if strings.TrimSpace(verdict.Target.Kind) != string(candidates.CapabilityObs) {
		return ObservationVerdictPlan{}, fmt.Errorf("product observation verdict plan requires target_kind %q, got %q", candidates.CapabilityObs, verdict.Target.Kind)
	}
	var payload observationVerdictCandidatePayload
	if len(verdict.Target.Payload) > 0 {
		if err := json.Unmarshal(verdict.Target.Payload, &payload); err != nil {
			return ObservationVerdictPlan{}, fmt.Errorf("parse observation candidate payload in verdict %s: %w", verdict.ID, err)
		}
	}
	if payload.ID == "" {
		payload.ID = strings.TrimSpace(verdict.Target.Ref)
	}
	productID := observationVerdictProductID(payload)
	if productID == "" {
		return ObservationVerdictPlan{}, fmt.Errorf("panel verdict %s target is not a product-subject observation candidate", verdict.ID)
	}
	if now.IsZero() {
		now = time.Now()
	}
	targetExists, targetHash, err := hashHypothesisPlanTarget(targetPath)
	if err != nil {
		return ObservationVerdictPlan{}, err
	}
	evidenceRefs := reviewartifact.UniqueSorted(compact(append(append([]string{}, verdict.EvidenceRefs...), payload.EvidenceRefs...)))
	evidenceRefs = reviewartifact.UniqueSorted(compact(append(evidenceRefs,
		"panel_verdict:"+verdict.ID,
		"observation_candidate:"+payload.ID,
		verdictPath,
	)))
	changeEvent := firstNonEmpty(payload.Statement, "reviewed observation feedback requires product interpretation")
	window := observationVerdictWindow(payload.ObservationKind)
	nextAction := "decide whether this reviewed observation evidence should become a parked product hypothesis candidate"
	id := stableID("povp_", productID, verdict.ID, payload.ID)
	reviewID := "review_" + strings.TrimPrefix(id, "povp_")
	metrics := observationVerdictDefaultMetrics(payload)
	return ObservationVerdictPlan{
		SchemaVersion:     ObservationVerdictPlanSchemaVersion,
		ID:                id,
		CreatedAt:         now.UTC().Format(time.RFC3339),
		Status:            "review_required",
		ProductID:         productID,
		VerdictID:         verdict.ID,
		RunID:             verdict.RunID,
		TargetKind:        verdict.Target.Kind,
		TargetRef:         verdict.Target.Ref,
		PanelDecision:     verdict.Synthesis.Decision,
		PanelRationale:    verdict.Synthesis.Rationale,
		ObservationID:     payload.ObservationID,
		ObservationKind:   payload.ObservationKind,
		SourceKind:        payload.SourceKind,
		SourceRef:         payload.SourceRef,
		Statement:         payload.Statement,
		Detail:            payload.Detail,
		ChangeEvent:       changeEvent,
		ObservationWindow: window,
		NextAction:        nextAction,
		EvidenceRefs:      evidenceRefs,
		TargetPath:        strings.TrimSpace(targetPath),
		TargetExists:      targetExists,
		TargetHash:        targetHash,
		ReviewDraft: ObservationVerdictDraft{
			ReviewID:         reviewID,
			Title:            "Review observation verdict for " + productID,
			AllowedDecisions: []string{"create_hypothesis_candidate", "park", "reject"},
			ReviewCommand:    "compound product observation-verdict-review <review.yaml>",
			RequiredEvidence: []string{
				"panel verdict is tied to a capability_observation candidate",
				"candidate subject is product-scoped",
				"expected effect and primary metric are product-owned, not inferred as truth",
				"review creates no ProductSignal and performs no external side effect",
			},
			PrimaryMetrics:    metrics,
			ObservationWindow: window,
			DecisionGate:      "create a hypothesis only when the feedback implies a testable product effect and a metric to observe",
			NextAction:        nextAction,
		},
		Warnings: []string{
			"does not apply panel verdicts",
			"does not mutate ProductSignal ledger",
			"does not call external accounts, post, reply, spend, or publish",
			"does not promote memory or create observation truth authority",
		},
	}, nil
}

func RecordObservationVerdictPlan(homePath string, plan ObservationVerdictPlan) (ObservationVerdictPlanPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return ObservationVerdictPlanPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(plan.ID) == "" {
		return ObservationVerdictPlanPaths{}, fmt.Errorf("product observation verdict plan id required")
	}
	arts, err := verdictflow.RecordPlan(homePath, ObservationVerdictReviewArea, plan.ID, " ", plan, renderObservationVerdictPlanMarkdown(plan), RenderObservationVerdictReviewYAML(plan))
	return ObservationVerdictPlanPaths{
		PlanPath:       arts.PlanPath,
		ReviewPath:     arts.ReviewPath,
		ReviewYAMLPath: arts.ReviewYAMLPath,
	}, err
}

func RenderObservationVerdictReviewYAML(plan ObservationVerdictPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version: %s\n", ObservationVerdictReviewSchemaVersion)
	fmt.Fprintf(&b, "id: %s\n", plan.ReviewDraft.ReviewID)
	fmt.Fprintf(&b, "plan_id: %s\n", plan.ID)
	fmt.Fprintf(&b, "product_id: %s\n", plan.ProductID)
	fmt.Fprintf(&b, "verdict_id: %s\n", plan.VerdictID)
	fmt.Fprintln(&b, "decision: TODO_decision_create_hypothesis_candidate_park_reject")
	fmt.Fprintln(&b, "rationale: TODO_rationale")
	fmt.Fprintln(&b, "confidence: TODO_confidence_0_to_1")
	fmt.Fprintf(&b, "change_event: %q\n", plan.ChangeEvent)
	fmt.Fprintln(&b, "expected_effect: TODO_expected_effect")
	fmt.Fprintln(&b, "primary_metrics:")
	for _, metric := range plan.ReviewDraft.PrimaryMetrics {
		fmt.Fprintf(&b, " - %q\n", metric)
	}
	if len(plan.ReviewDraft.PrimaryMetrics) == 0 {
		fmt.Fprintln(&b, " - TODO_primary_metric")
	}
	fmt.Fprintf(&b, "observation_window: %q\n", plan.ObservationWindow)
	fmt.Fprintf(&b, "next_action: %q\n", plan.NextAction)
	fmt.Fprintln(&b, "evidence_refs:")
	for _, ref := range plan.EvidenceRefs {
		fmt.Fprintf(&b, " - %q\n", ref)
	}
	return b.String()
}

func LoadObservationVerdictReview(path string) (ObservationVerdictReview, error) {
	return verdictflow.LoadReview[ObservationVerdictReview](path)
}

func ApplyObservationVerdictReview(homePath string, store *mem.Store, review ObservationVerdictReview, sourcePath string, now time.Time) (ObservationVerdictReviewReport, error) {
	if store == nil {
		return ObservationVerdictReviewReport{}, fmt.Errorf("memory store required")
	}
	if err := ValidateObservationVerdictReview(review); err != nil {
		return ObservationVerdictReviewReport{SchemaVersion: ObservationVerdictReviewReportSchemaVersion, ProductID: review.ProductID, VerdictID: review.VerdictID, ReviewID: review.ID, Status: "fail"}, err
	}
	plan, err := findObservationVerdictPlanForReview(homePath, review, sourcePath)
	if err != nil {
		return ObservationVerdictReviewReport{}, err
	}
	if plan.ProductID != review.ProductID || plan.VerdictID != review.VerdictID {
		return ObservationVerdictReviewReport{}, fmt.Errorf("observation verdict review does not match product plan")
	}
	if now.IsZero() {
		now = time.Now()
	}
	if review.ReviewedAt == "" {
		review.ReviewedAt = now.UTC().Format(time.RFC3339)
	}
	review.AppliedAt = now.UTC().Format(time.RFC3339)
	review.SourceRef = "product-observation-verdict-review:" + filepath.Base(sourcePath)
	evidence, err := store.RecordEvidence(mem.EvidenceItem{
		SourceKind: "panel_verdict",
		SourceID:   review.SourceRef,
		QuoteText:  review.Rationale,
		Actor:      "tool",
	}, now)
	if err != nil {
		return ObservationVerdictReviewReport{}, err
	}
	review.EvidenceID = evidence.ID
	evidenceRefs := reviewartifact.UniqueSorted(compact(append(append([]string{}, plan.EvidenceRefs...), review.EvidenceRefs...)))
	evidenceRefs = reviewartifact.UniqueSorted(compact(append(evidenceRefs,
		evidence.ID,
		"panel_verdict:"+plan.VerdictID,
		"product_observation_verdict_review:"+review.ID,
	)))
	var hypothesisID string
	if review.Decision == "create_hypothesis_candidate" {
		hypothesisID = stableID("hyp_", review.ProductID, review.VerdictID, review.ID, review.ExpectedEffect)
		candidate := hypothesisCandidate(Hypothesis{
			ID:                hypothesisID,
			ProductID:         review.ProductID,
			ChangeEvent:       firstNonEmpty(review.ChangeEvent, plan.ChangeEvent),
			ExpectedEffect:    review.ExpectedEffect,
			Metrics:           compact(review.PrimaryMetrics),
			ObservationWindow: firstNonEmpty(review.ObservationWindow, plan.ObservationWindow),
			Result:            "unknown",
			NextAction:        firstNonEmpty(review.NextAction, plan.NextAction),
			EvidenceRefs:      evidenceRefs,
		}, now)
		candidate.Verdict = candidates.Park
		if err := writeHypothesisCandidate(homePath, candidate); err != nil {
			return ObservationVerdictReviewReport{}, err
		}
		evidenceRefs = reviewartifact.UniqueSorted(compact(append(evidenceRefs, "product_hypothesis:"+hypothesisID)))
	}
	tr, err := BuildLoopTransition(LoopTransitionInput{
		Kind:         "product_observation_verdict_review",
		ProductID:    review.ProductID,
		ObjectID:     review.ID,
		SourceTrace:  "product_observation_verdict_review:" + review.ID,
		BeforeRefs:   plan.EvidenceRefs,
		AfterRefs:    evidenceRefs,
		EvidenceRefs: evidenceRefs,
		Summary:      fmt.Sprintf("product observation verdict review %s recorded %s; no ProductSignal or external surface changed", review.ID, review.Decision),
	}, now)
	if err != nil {
		return ObservationVerdictReviewReport{}, err
	}
	if err := RecordTransition(homePath, tr); err != nil {
		return ObservationVerdictReviewReport{}, err
	}
	report := ObservationVerdictReviewReport{
		SchemaVersion: ObservationVerdictReviewReportSchemaVersion,
		ID:            reviewartifact.ID("povr_", review.ID, evidence.ID, tr.ID),
		CreatedAt:     now.UTC().Format(time.RFC3339),
		Status:        "recorded",
		ProductID:     review.ProductID,
		VerdictID:     review.VerdictID,
		PlanID:        plan.ID,
		ReviewID:      review.ID,
		Decision:      review.Decision,
		EvidenceID:    evidence.ID,
		EvidenceRefs:  evidenceRefs,
		TransitionID:  tr.ID,
		HypothesisID:  hypothesisID,
	}
	if err := recordObservationVerdictReviewResult(homePath, sourcePath, plan, report); err != nil {
		return ObservationVerdictReviewReport{}, err
	}
	return report, nil
}

func ValidateObservationVerdictReview(review ObservationVerdictReview) error {
	if review.SchemaVersion != ObservationVerdictReviewSchemaVersion {
		return fmt.Errorf("schema_version must be %s, got %q", ObservationVerdictReviewSchemaVersion, review.SchemaVersion)
	}
	if strings.TrimSpace(review.ID) == "" || strings.TrimSpace(review.PlanID) == "" || strings.TrimSpace(review.ProductID) == "" || strings.TrimSpace(review.VerdictID) == "" {
		return fmt.Errorf("observation verdict review id plan_id product_id verdict_id required")
	}
	if strings.ContainsAny(review.PlanID, `/\`) || strings.ContainsAny(review.VerdictID, `/\`) {
		return fmt.Errorf("observation verdict review plan_id and verdict_id must not contain path separators")
	}
	if !oneOf(review.Decision, []string{"create_hypothesis_candidate", "park", "reject"}) {
		return fmt.Errorf("observation verdict review decision must be create_hypothesis_candidate|park|reject, got %q", review.Decision)
	}
	if strings.TrimSpace(review.Rationale) == "" || strings.HasPrefix(strings.TrimSpace(review.Rationale), "TODO") {
		return fmt.Errorf("observation verdict review rationale required")
	}
	if review.Confidence < 0 || review.Confidence > 1 {
		return fmt.Errorf("observation verdict review confidence must be between 0 and 1")
	}
	if len(compact(review.EvidenceRefs)) == 0 {
		return fmt.Errorf("observation verdict review evidence_refs required")
	}
	if review.Decision == "create_hypothesis_candidate" {
		if strings.TrimSpace(review.ExpectedEffect) == "" || strings.HasPrefix(strings.TrimSpace(review.ExpectedEffect), "TODO") {
			return fmt.Errorf("observation verdict review expected_effect required hypothesis candidate")
		}
		if len(compact(review.PrimaryMetrics)) == 0 {
			return fmt.Errorf("observation verdict review primary_metrics required hypothesis candidate")
		}
	}
	return nil
}

func annotateObservationVerdictItem(item *reviewartifact.QueueItem) {
	if record, ok := observationVerdictReviewedFromExtras(item.ExtraPaths); ok {
		item.Status = record.Status
		item.Readiness = "no_action"
		item.BlockedReason = ""
		item.NextAction = "product observation verdict review recorded"
		item.Actions = nil
		item.DecisionCard = nil
		return
	}
	plan, ok := observationVerdictPlanFromQueueItem(*item)
	if !ok {
		item.Readiness = "blocked"
		item.BlockedReason = "product observation verdict plan unreadable"
		item.NextAction = "regenerate product observation-verdict-plan"
		item.Actions = nil
		return
	}
	reviewPath := observationVerdictReviewYAMLPath(*item)
	if reviewPath == "" {
		item.Readiness = "blocked"
		item.BlockedReason = "review YAML draft missing"
		item.NextAction = "regenerate product observation-verdict-plan"
		item.Actions = nil
		return
	}
	item.Readiness = "needs_review"
	item.BlockedReason = "product owner must decide whether panel-observed feedback creates a hypothesis candidate"
	item.NextAction = "edit review YAML then record product observation verdict review"
	card := observationVerdictDecisionCard(plan, reviewPath)
	item.DecisionCard = card
	item.Actions = []reviewartifact.Action{{
		ID:             "record_product_observation_verdict_review",
		Label:          "Record product observation verdict review",
		Command:        shellJoin([]string{"compound", "product", "observation-verdict-review", reviewPath}),
		Argv:           []string{"compound", "product", "observation-verdict-review", reviewPath},
		RequiresReview: true,
	}}
}

func observationVerdictDecisionCard(plan ObservationVerdictPlan, reviewPath string) *reviewartifact.DecisionCard {
	argv := []string{"compound", "product", "observation-verdict-review", reviewPath}
	return &reviewartifact.DecisionCard{
		Claim:               "Review observation verdict for " + plan.ProductID,
		OptionalRationale:   false,
		PanelLean:           "create hypothesis only if feedback implies a testable product effect",
		CanonicalActionHint: "Edit review YAML decision/rationale/confidence, then run `compound product observation-verdict-review <review.yaml>`. This may create a parked product hypothesis candidate; it never mutates ProductSignal or applies the panel verdict.",
		Boundary:            plan.Warnings,
		Meta: map[string]string{
			"review_yaml": reviewPath,
			"verdict_id":  plan.VerdictID,
			"product_id":  plan.ProductID,
		},
		Why: []reviewartifact.DecisionCardReason{
			{Label: "panel verdict", Question: "What lens synthesis is being consumed?", Detail: plan.VerdictID + " / " + plan.PanelDecision},
			{Label: "product subject", Question: "Which product loop owns the next interpretation?", Detail: plan.ProductID},
			{Label: "candidate", Question: "What was observed?", Detail: firstNonEmpty(plan.Statement, plan.Detail, plan.TargetRef)},
			{Label: "boundary", Severity: "warning", Question: "What must stay outside this bridge?", Detail: strings.Join(plan.Warnings, "; ")},
		},
		Choices: []reviewartifact.DecisionCardChoice{
			{ID: "create_hypothesis_candidate", Label: "Create Candidate", Effect: "Creates a parked product hypothesis candidate from reviewed observation verdict evidence.", Command: shellJoin(argv), Argv: argv, RequiresReview: true},
			{ID: "park", Label: "Park", Effect: "Records product review evidence without creating a hypothesis candidate.", Command: shellJoin(argv), Argv: argv, RequiresReview: true},
			{ID: "reject", Label: "Reject", Effect: "Records that the panel verdict should not feed product-loop work.", Command: shellJoin(argv), Argv: argv, RequiresReview: true},
		},
	}
}

func renderObservationVerdictPlanMarkdown(plan ObservationVerdictPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Product Observation Verdict Plan: %s\n\n", plan.VerdictID)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- product: %s\n", plan.ProductID)
	fmt.Fprintf(&b, "- verdict: %s\n", plan.VerdictID)
	fmt.Fprintf(&b, "- target: %s/%s\n", plan.TargetKind, plan.TargetRef)
	fmt.Fprintf(&b, "- panel_decision: %s\n", plan.PanelDecision)
	fmt.Fprintf(&b, "- review_command: `%s`\n", plan.ReviewDraft.ReviewCommand)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Product Basis")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- observation: %s (%s)\n", firstNonEmpty(plan.ObservationID, "unknown"), firstNonEmpty(plan.ObservationKind, "unknown"))
	fmt.Fprintf(&b, "- source: %s %s\n", firstNonEmpty(plan.SourceKind, "unknown"), firstNonEmpty(plan.SourceRef, "unknown"))
	fmt.Fprintf(&b, "- statement: %s\n", firstNonEmpty(plan.Statement, "unknown"))
	fmt.Fprintf(&b, "- detail: %s\n", firstNonEmpty(plan.Detail, "unknown"))
	fmt.Fprintf(&b, "- next_action: %s\n", plan.NextAction)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Required Evidence Before Review")
	fmt.Fprintln(&b)
	for _, evidence := range plan.ReviewDraft.RequiredEvidence {
		fmt.Fprintf(&b, "- %s\n", evidence)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Boundaries")
	fmt.Fprintln(&b)
	for _, warning := range plan.Warnings {
		fmt.Fprintf(&b, "- %s\n", warning)
	}
	return b.String()
}

func observationVerdictProductID(payload observationVerdictCandidatePayload) string {
	if strings.TrimSpace(payload.SubjectKind) == "product" && strings.TrimSpace(payload.SubjectID) != "" {
		return strings.TrimSpace(payload.SubjectID)
	}
	subject := strings.TrimSpace(payload.Subject)
	if strings.HasPrefix(subject, "product:") {
		return subject
	}
	return ""
}

func observationVerdictWindow(kind string) string {
	switch strings.TrimSpace(kind) {
	case "distribution_feedback", "social_feedback", "support_message", "message", "comment":
		return "next comparable feedback window after bounded product/distribution change"
	default:
		return "review window required"
	}
}

func observationVerdictDefaultMetrics(payload observationVerdictCandidatePayload) []string {
	switch strings.TrimSpace(payload.ObservationKind) {
	case "distribution_feedback", "social_feedback", "comment":
		return []string{"distribution_feedback_quality", "qualified_reply_rate"}
	case "support_message", "message":
		return []string{"support_feedback_theme_count", "activation_or_retention_proxy"}
	default:
		return []string{"metric selection required"}
	}
}

func findObservationVerdictPlanForReview(homePath string, review ObservationVerdictReview, sourcePath string) (ObservationVerdictPlan, error) {
	if stem := reviewArtifactStem(sourcePath); stem != "" {
		plan, err := loadObservationVerdictPlanByPath(filepath.Join(homePath, "reviews", ObservationVerdictReviewArea, stem+".json"))
		if err == nil && plan.ID == review.PlanID && plan.ProductID == review.ProductID && plan.VerdictID == review.VerdictID {
			return plan, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return ObservationVerdictPlan{}, err
		}
	}
	var found ObservationVerdictPlan
	root := filepath.Join(homePath, "reviews", ObservationVerdictReviewArea)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".json" || strings.Contains(filepath.Base(path), ".reviewed.") {
			return nil
		}
		plan, err := loadObservationVerdictPlanByPath(path)
		if err != nil {
			return nil
		}
		if plan.ID == review.PlanID && plan.ReviewDraft.ReviewID == review.ID && plan.ProductID == review.ProductID && plan.VerdictID == review.VerdictID {
			found = plan
			return filepath.SkipAll
		}
		return nil
	})
	if os.IsNotExist(err) {
		return ObservationVerdictPlan{}, fmt.Errorf("product observation verdict plan for review %q not found", review.ID)
	}
	if err != nil {
		return ObservationVerdictPlan{}, err
	}
	if found.ID == "" {
		return ObservationVerdictPlan{}, fmt.Errorf("product observation verdict plan for review %q not found", review.ID)
	}
	return found, nil
}

func loadObservationVerdictPlanByPath(path string) (ObservationVerdictPlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ObservationVerdictPlan{}, err
	}
	var plan ObservationVerdictPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return ObservationVerdictPlan{}, err
	}
	return plan, nil
}

func recordObservationVerdictReviewResult(homePath, sourcePath string, plan ObservationVerdictPlan, report ObservationVerdictReviewReport) error {
	stem := reviewArtifactStem(sourcePath)
	if stem == "" {
		stem = plan.ID
	}
	dir, err := reviewartifact.Dir(homePath, ObservationVerdictReviewArea)
	if err != nil {
		return err
	}
	report.ReviewPath = filepath.Join(dir, stem+"."+report.ID+".reviewed.json")
	_, err = reviewartifact.WriteJSON(homePath, ObservationVerdictReviewArea, stem+"."+report.ID+".reviewed.json", report, " ")
	if err != nil {
		return err
	}
	plan.Status = "recorded"
	plan.ReviewedAt = report.CreatedAt
	plan.ReviewID = report.ReviewID
	plan.ReviewDecision = report.Decision
	plan.HypothesisID = report.HypothesisID
	plan.ReviewEvidenceID = report.EvidenceID
	plan.ReviewTransition = report.TransitionID
	path := filepath.Join(homePath, "reviews", ObservationVerdictReviewArea, stem+".json")
	data, err := json.MarshalIndent(plan, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func observationVerdictPlanFromQueueItem(item reviewartifact.QueueItem) (ObservationVerdictPlan, bool) {
	plan, err := loadObservationVerdictPlanByPath(item.JSONPath)
	if err != nil {
		return ObservationVerdictPlan{}, false
	}
	if plan.SchemaVersion != ObservationVerdictPlanSchemaVersion || plan.ID == "" {
		return ObservationVerdictPlan{}, false
	}
	return plan, true
}

func observationVerdictReviewYAMLPath(item reviewartifact.QueueItem) string {
	if item.JSONPath == "" {
		return ""
	}
	path := strings.TrimSuffix(item.JSONPath, ".json") + ".review.yaml"
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

func observationVerdictReviewedFromExtras(paths []string) (ObservationVerdictReviewReport, bool) {
	return verdictflow.LatestReviewedFromExtras(paths,
		func(r ObservationVerdictReviewReport) bool {
			return r.SchemaVersion == ObservationVerdictReviewReportSchemaVersion && r.ID != "" && r.Status != ""
		},
		func(r ObservationVerdictReviewReport) string { return r.ID },
		func(r ObservationVerdictReviewReport) string { return r.CreatedAt },
	)
}
