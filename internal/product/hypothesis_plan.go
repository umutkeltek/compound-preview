package product

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/reviewartifact"
)

const HypothesisPlanSchemaVersion = "compound.product_hypothesis_plan.v0.1"

// HypothesisPlan is a reviewed product experiment/review proposal from a
// product_hypothesis candidate. It keeps product learning evidence-shaped
// without silently editing product strategy, analytics sources, store assets,
// or promoted memory truth.
type HypothesisPlan struct {
	SchemaVersion     string              `json:"schema_version"`
	ID                string              `json:"id"`
	CreatedAt         string              `json:"created_at"`
	Status            string              `json:"status"`
	CandidateID       string              `json:"candidate_id"`
	CandidateVerdict  string              `json:"candidate_verdict,omitempty"`
	HypothesisKey     string              `json:"hypothesis_key"`
	ProductID         string              `json:"product_id"`
	ChangeEvent       string              `json:"change_event"`
	ExpectedEffect    string              `json:"expected_effect"`
	Metrics           []string            `json:"metrics,omitempty"`
	ObservationWindow string              `json:"observation_window,omitempty"`
	Result            string              `json:"result,omitempty"`
	NextAction        string              `json:"next_action,omitempty"`
	Confidence        float64             `json:"confidence,omitempty"`
	EvidenceRefs      []string            `json:"evidence_refs"`
	TargetPath        string              `json:"target_path,omitempty"`
	TargetExists      bool                `json:"target_exists"`
	TargetHash        string              `json:"target_hash,omitempty"`
	ReviewDraft       HypothesisPlanDraft `json:"review_draft"`
	Warnings          []string            `json:"warnings,omitempty"`
}

type HypothesisPlanDraft struct {
	ReviewID           string   `json:"review_id"`
	Title              string   `json:"title"`
	PrimaryMetrics     []string `json:"primary_metrics"`
	ObservationWindow  string   `json:"observation_window"`
	EvaluationMethod   string   `json:"evaluation_method"`
	ConfoundersToCheck []string `json:"confounders_to_check"`
	DecisionGate       string   `json:"decision_gate"`
	NextAction         string   `json:"next_action"`
	RequiredEvidence   []string `json:"required_evidence"`
	ReviewNote         string   `json:"review_note"`
}

type HypothesisPlanPaths struct {
	PlanPath        string
	ReviewPath      string
	SignalDraftPath string
}

type hypothesisPlanCandidate struct {
	SchemaVersion     string   `json:"schema_version,omitempty"`
	ID                string   `json:"id"`
	Kind              string   `json:"kind"`
	ProductID         string   `json:"product_id,omitempty"`
	Subject           string   `json:"subject,omitempty"`
	Statement         string   `json:"statement,omitempty"`
	Detail            string   `json:"detail,omitempty"`
	ChangeEvent       string   `json:"change_event,omitempty"`
	ExpectedEffect    string   `json:"expected_effect,omitempty"`
	Metrics           []string `json:"metrics,omitempty"`
	ObservationWindow string   `json:"observation_window,omitempty"`
	Result            string   `json:"result,omitempty"`
	NextAction        string   `json:"next_action,omitempty"`
	Confidence        float64  `json:"confidence,omitempty"`
	Verdict           string   `json:"verdict,omitempty"`
	EvidenceRefs      []string `json:"evidence_refs,omitempty"`
	CreatedAt         string   `json:"created_at,omitempty"`
}

func BuildHypothesisPlan(homePath, candidateID, targetPath string, now time.Time) (HypothesisPlan, error) {
	homePath = strings.TrimSpace(homePath)
	candidateID = strings.TrimSpace(candidateID)
	if homePath == "" {
		return HypothesisPlan{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if candidateID == "" {
		return HypothesisPlan{}, fmt.Errorf("candidate id required")
	}
	c, err := readHypothesisPlanCandidate(homePath, candidateID)
	if err != nil {
		return HypothesisPlan{}, err
	}
	if c.Kind != "" && c.Kind != string(candidates.ProductHypothesis) {
		return HypothesisPlan{}, fmt.Errorf("candidate %q kind %q, want %q", candidateID, c.Kind, candidates.ProductHypothesis)
	}
	productID := firstNonEmpty(c.ProductID, productIDFromHypothesisSubject(c.Subject))
	if productID == "" {
		return HypothesisPlan{}, fmt.Errorf("product_id required: candidate must set product_id or subject product:<id>")
	}
	expectedEffect := firstNonEmpty(c.ExpectedEffect, c.Statement)
	if expectedEffect == "" {
		return HypothesisPlan{}, fmt.Errorf("expected_effect required")
	}
	// Fail-closed twin gate (work_6ede073fe356 falsifier 1): a director-drafted
	// candidate parks with the literal operator placeholder as its
	// expected_effect. Planning it anyway mints a review_required plan whose
	// hypothesis literally IS "OPERATOR MUST WRITE: ...", making an unfilled
	// draft indistinguishable from an approved one. The governed door around
	// this gate is `compound product hypothesis-approve`, the only verb that
	// writes the operator's real sentence.
	if strings.HasPrefix(strings.TrimSpace(expectedEffect), HypothesisDraftExpectedEffectPlaceholderPrefix) {
		return HypothesisPlan{}, fmt.Errorf("candidate %s expected_effect is still the unfilled operator placeholder (%q): run `compound product hypothesis-approve %s --expected-effect \"<what should the metric do after the change?>\"` first", candidateID, HypothesisDraftExpectedEffectPlaceholderPrefix, candidateID)
	}
	changeEvent := firstNonEmpty(c.ChangeEvent, c.Detail)
	if changeEvent == "" {
		changeEvent = "candidate product change requires review"
	}
	metrics := compact(c.Metrics)
	if len(metrics) == 0 {
		metrics = []string{"metric selection required"}
	}
	observationWindow := firstNonEmpty(c.ObservationWindow, "review window required")
	nextAction := firstNonEmpty(c.NextAction, "review hypothesis evidence before product action")
	targetExists, targetHash, err := hashHypothesisPlanTarget(targetPath)
	if err != nil {
		return HypothesisPlan{}, err
	}
	key := normalizeIDPart(strings.Join([]string{productID, changeEvent, expectedEffect}, " "))
	if key == "" {
		key = normalizeIDPart(candidateID)
	}
	evidenceRefs := compact(c.EvidenceRefs)
	warnings := []string{
		"does not mutate product strategy or store assets",
		"does not call analytics providers",
		"does not promote memory truth or declare causality",
	}
	if len(evidenceRefs) == 0 {
		warnings = append(warnings, "candidate has no evidence refs; reviewer must attach source evidence before action")
	}
	plan := HypothesisPlan{
		SchemaVersion:     HypothesisPlanSchemaVersion,
		ID:                stableID("php_", candidateID, productID, expectedEffect),
		CreatedAt:         now.UTC().Format(time.RFC3339),
		Status:            "review_required",
		CandidateID:       candidateID,
		CandidateVerdict:  c.Verdict,
		HypothesisKey:     key,
		ProductID:         productID,
		ChangeEvent:       changeEvent,
		ExpectedEffect:    expectedEffect,
		Metrics:           metrics,
		ObservationWindow: observationWindow,
		Result:            firstNonEmpty(c.Result, "unknown"),
		NextAction:        nextAction,
		Confidence:        c.Confidence,
		EvidenceRefs:      evidenceRefs,
		TargetPath:        strings.TrimSpace(targetPath),
		TargetExists:      targetExists,
		TargetHash:        targetHash,
		ReviewDraft: HypothesisPlanDraft{
			ReviewID:          "review_" + key + "_" + stableID("", candidateID, expectedEffect),
			Title:             firstNonEmpty(expectedEffect, "product hypothesis review"),
			PrimaryMetrics:    metrics,
			ObservationWindow: observationWindow,
			EvaluationMethod:  "Compare stated before/after evidence, confounders, and product context; record supported/refuted/inconclusive through product review.",
			ConfoundersToCheck: []string{
				"sample size and seasonality",
				"other product, pricing, release, or distribution changes in window",
				"provider lag or attribution mismatch",
			},
			DecisionGate: "No product surface or strategy change until a reviewed product review records result and next action.",
			NextAction:   nextAction,
			RequiredEvidence: []string{
				"source trace or product signal that produced the hypothesis",
				"before metric window and after metric window",
				"named confounders considered",
				"review result: supported, refuted, or inconclusive",
			},
			ReviewNote: "This product hypothesis draft is owner-review material. Use it to prepare a product review or experiment, not to declare causality or mutate product assets directly.",
		},
		Warnings: warnings,
	}
	return plan, nil
}

func readHypothesisPlanCandidate(homePath, candidateID string) (hypothesisPlanCandidate, error) {
	var c hypothesisPlanCandidate
	if err := candidates.ReadCandidate(homePath, candidates.ProductHypothesis, candidateID, &c); err != nil {
		return c, err
	}
	if c.ID == "" {
		c.ID = candidateID
	}
	if c.Kind == "" {
		c.Kind = string(candidates.ProductHypothesis)
	}
	return c, nil
}

func RecordHypothesisPlan(homePath string, plan HypothesisPlan) (HypothesisPlanPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return HypothesisPlanPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if plan.ID == "" {
		return HypothesisPlanPaths{}, fmt.Errorf("product hypothesis plan id required")
	}
	dir, err := reviewartifact.Dir(homePath, "product-hypotheses")
	if err != nil {
		return HypothesisPlanPaths{}, err
	}
	paths := HypothesisPlanPaths{
		PlanPath:        filepath.Join(dir, plan.ID+".json"),
		ReviewPath:      filepath.Join(dir, plan.ID+".md"),
		SignalDraftPath: filepath.Join(dir, plan.ID+".signal.yaml"),
	}
	if _, err := reviewartifact.Write(homePath, "product-hypotheses", plan.ID, plan, " ", RenderHypothesisPlanReview(plan)); err != nil {
		return paths, err
	}
	if _, err := reviewartifact.WriteRaw(homePath, "product-hypotheses", plan.ID+".signal.yaml", []byte(RenderHypothesisSignalDraft(plan))); err != nil {
		return paths, err
	}
	return paths, nil
}

func RenderHypothesisPlanReview(plan HypothesisPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Product Hypothesis Review Plan: %s\n\n", plan.CandidateID)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- candidate: %s\n", plan.CandidateID)
	if plan.CandidateVerdict != "" {
		fmt.Fprintf(&b, "- candidate_verdict: %s\n", plan.CandidateVerdict)
	}
	fmt.Fprintf(&b, "- product: %s\n", plan.ProductID)
	fmt.Fprintf(&b, "- hypothesis_key: %s\n", plan.HypothesisKey)
	fmt.Fprintf(&b, "- review_id: %s\n", plan.ReviewDraft.ReviewID)
	if plan.TargetPath != "" {
		fmt.Fprintf(&b, "- target: %s\n", plan.TargetPath)
		fmt.Fprintf(&b, "- target_exists: %t\n", plan.TargetExists)
	}
	if plan.TargetHash != "" {
		fmt.Fprintf(&b, "- target_hash: %s\n", plan.TargetHash)
	}
	fmt.Fprintf(&b, "- evidence_refs: %s\n\n", strings.Join(plan.EvidenceRefs, ", "))
	fmt.Fprintln(&b, "This owner-review artifact does not mutate product strategy, store assets, analytics sources, or promoted memory truth.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Hypothesis")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- change_event: %s\n", plan.ChangeEvent)
	fmt.Fprintf(&b, "- expected_effect: %s\n", plan.ExpectedEffect)
	fmt.Fprintf(&b, "- metrics: %s\n", strings.Join(plan.Metrics, ", "))
	fmt.Fprintf(&b, "- observation_window: %s\n", plan.ObservationWindow)
	if plan.Confidence > 0 {
		fmt.Fprintf(&b, "- confidence: %.2f\n", plan.Confidence)
	}
	if plan.NextAction != "" {
		fmt.Fprintf(&b, "- next_action: %s\n", plan.NextAction)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Review Draft")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- evaluation_method: %s\n", plan.ReviewDraft.EvaluationMethod)
	fmt.Fprintf(&b, "- decision_gate: %s\n", plan.ReviewDraft.DecisionGate)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Confounders To Check")
	fmt.Fprintln(&b)
	for _, confounder := range plan.ReviewDraft.ConfoundersToCheck {
		fmt.Fprintf(&b, "- %s\n", confounder)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Required Evidence Before Action")
	fmt.Fprintln(&b)
	for _, evidence := range plan.ReviewDraft.RequiredEvidence {
		fmt.Fprintf(&b, "- %s\n", evidence)
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "%s\n", plan.ReviewDraft.ReviewNote)
	return b.String()
}

func RenderHypothesisSignalDraft(plan HypothesisPlan) string {
	metric := "TODO_metric_name"
	if len(plan.Metrics) > 0 && !strings.Contains(plan.Metrics[0], "required") {
		metric = plan.Metrics[0]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version: %s\n", SignalSchemaVersion)
	fmt.Fprintln(&b, "source:")
	fmt.Fprintln(&b, "  kind: TODO_provider")
	fmt.Fprintf(&b, "  ref: product_hypothesis:%s\n", plan.CandidateID)
	fmt.Fprintln(&b, "signal:")
	fmt.Fprintf(&b, "  id: %s\n", stableID("sig_", plan.ProductID, plan.CandidateID, metric, plan.ObservationWindow))
	fmt.Fprintf(&b, "  product_id: %s\n", plan.ProductID)
	fmt.Fprintln(&b, "  surface: TODO_surface")
	fmt.Fprintf(&b, "  change_event: %q\n", plan.ChangeEvent)
	fmt.Fprintf(&b, "  metric_name: %s\n", metric)
	fmt.Fprintln(&b, "  before: TODO_before_value")
	fmt.Fprintln(&b, "  after: TODO_after_value")
	fmt.Fprintf(&b, "  window: %q\n", plan.ObservationWindow)
	fmt.Fprintln(&b, "  confidence: 0")
	fmt.Fprintln(&b, "  confounders:")
	for _, confounder := range plan.ReviewDraft.ConfoundersToCheck {
		fmt.Fprintf(&b, "    - %q\n", confounder)
	}
	fmt.Fprintln(&b, "  interpretations:")
	fmt.Fprintln(&b, "    - lens: hypothesis")
	fmt.Fprintf(&b, "      claim: %q\n", "fill after metric evidence is imported; do not declare causality from this draft")
	fmt.Fprintf(&b, "  next_action: %q\n", plan.NextAction)
	fmt.Fprintln(&b, "  evidence_refs:")
	for _, ref := range plan.EvidenceRefs {
		fmt.Fprintf(&b, "    - %q\n", ref)
	}
	fmt.Fprintf(&b, "    - %q\n", "product_hypothesis:"+plan.CandidateID)
	fmt.Fprintln(&b, "evidence_refs:")
	for _, ref := range plan.EvidenceRefs {
		fmt.Fprintf(&b, "  - %q\n", ref)
	}
	fmt.Fprintf(&b, "  - %q\n", "product_hypothesis:"+plan.CandidateID)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "# This draft is intentionally invalid until TODO_provider, TODO_surface, before, and after are replaced with real evidence.")
	fmt.Fprintln(&b, "# Importing it with product signal should fail before those fields are reviewed.")
	return b.String()
}

func productIDFromHypothesisSubject(subject string) string {
	subject = strings.TrimSpace(subject)
	if !strings.HasPrefix(subject, "product:") {
		return ""
	}
	parts := strings.Split(subject, ":")
	if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
		return ""
	}
	return "product:" + strings.TrimSpace(parts[1])
}

func hashHypothesisPlanTarget(path string) (bool, string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return false, "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, "", nil
		}
		return false, "", err
	}
	return true, "sha256:" + hashBytes(data), nil
}
