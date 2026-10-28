package product

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/mem"
	"compound/internal/reviewartifact"
)

func TestObservationVerdictPlanCreatesReviewGatedProductHypothesisMaterial(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	verdictPath := writeProductObservationPanelVerdictFixture(t, home)
	targetPath := filepath.Join(t.TempDir(), "product-loop.md")
	if err := os.WriteFile(targetPath, []byte("# Product loop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildObservationVerdictPlan(home, verdictPath, targetPath, tProduct)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != ObservationVerdictPlanSchemaVersion || plan.Status != "review_required" || plan.ProductID != "product:focus-notes" || plan.VerdictID != "panel_verdict_fixture_social" || plan.PanelDecision != "park" {
		t.Fatalf("bad observation verdict plan: %+v", plan)
	}
	if !containsProductString(plan.EvidenceRefs, "panel_verdict:"+plan.VerdictID) || !containsProductString(plan.EvidenceRefs, "observation_candidate:cobs_fixture_social_reply") {
		t.Fatalf("plan missing verdict/candidate evidence refs: %+v", plan.EvidenceRefs)
	}
	if !containsProductString(plan.Warnings, "does not mutate ProductSignal ledger") || !containsProductString(plan.Warnings, "does not apply panel verdicts") {
		t.Fatalf("plan should carry hard boundaries: %+v", plan.Warnings)
	}
	paths, err := RecordObservationVerdictPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.PlanPath, paths.ReviewPath, paths.ReviewYAMLPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected artifact %s: %v", path, err)
		}
	}
	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: ObservationVerdictReviewArea}, tProduct)
	if err != nil {
		t.Fatal(err)
	}
	AnnotateReviewQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.ReviewRequired != 1 || queue.Counts.DecisionCardCount != 1 || queue.Counts.ActionableCount != 1 {
		t.Fatalf("product observation verdict queue should be actionable: %+v", queue.Counts)
	}
	item := queue.Items[0]
	if item.DecisionCard == nil || item.DecisionCard.Meta["verdict_id"] != plan.VerdictID || len(item.Actions) != 1 || item.Actions[0].ID != "record_product_observation_verdict_review" {
		t.Fatalf("bad queue annotation: %+v", item)
	}
	reviewYAML := readProductTestFile(t, paths.ReviewYAMLPath)
	reviewYAML = strings.ReplaceAll(reviewYAML, "decision: TODO_decision_create_hypothesis_candidate_park_reject", "decision: create_hypothesis_candidate")
	reviewYAML = strings.ReplaceAll(reviewYAML, "rationale: TODO_rationale", "rationale: reviewed social feedback is product-loop evidence, not a signal")
	reviewYAML = strings.ReplaceAll(reviewYAML, "confidence: TODO_confidence_0_to_1", "confidence: 0.72")
	reviewYAML = strings.ReplaceAll(reviewYAML, "expected_effect: TODO_expected_effect", "expected_effect: export-support feedback should improve distribution reply quality")
	if err := os.WriteFile(paths.ReviewYAMLPath, []byte(reviewYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	review, err := LoadObservationVerdictReview(paths.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	report, err := ApplyObservationVerdictReview(home, store, review, paths.ReviewYAMLPath, tProduct.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != ObservationVerdictReviewReportSchemaVersion || report.Status != "recorded" || report.Decision != "create_hypothesis_candidate" || report.HypothesisID == "" || report.TransitionID == "" || report.EvidenceID == "" {
		t.Fatalf("bad observation verdict review report: %+v", report)
	}
	candidate, err := loadHypothesisCandidate(home, report.HypothesisID)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Kind != string(candidates.ProductHypothesis) || candidate.Verdict != candidates.Park || candidate.ProductID != "product:focus-notes" || candidate.ExpectedEffect != "export-support feedback should improve distribution reply quality" {
		t.Fatalf("bad product hypothesis candidate: %+v", candidate)
	}
	if !containsProductString(candidate.EvidenceRefs, "product_observation_verdict_review:"+review.ID) || !containsProductString(candidate.EvidenceRefs, report.EvidenceID) {
		t.Fatalf("candidate missing reviewed observation evidence refs: %+v", candidate.EvidenceRefs)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "product-signals.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("observation verdict bridge must not product signals: %v", err)
	}
	queue, err = reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: ObservationVerdictReviewArea}, tProduct.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateReviewQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.ReviewRequired != 0 || queue.Items[0].Status != "recorded" || queue.Items[0].Readiness != "no_action" {
		t.Fatalf("reviewed queue should be terminal: counts=%+v item=%+v", queue.Counts, queue.Items[0])
	}
}

func writeProductObservationPanelVerdictFixture(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(home, "reviews", "panel-verdicts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := observationVerdictCandidatePayload{
		ID:              "cobs_fixture_social_reply",
		Kind:            string(candidates.CapabilityObs),
		Subject:         "observation:product:focus-notes",
		Statement:       "Synthetic commenter asked whether Focus Notes exports plain text.",
		Detail:          "social-distribution feedback should be reviewed before product-loop action",
		Confidence:      0.7,
		State:           "park",
		EvidenceRefs:    []string{"fixture:social:reply"},
		ObservationID:   "obs_fixture_social_reply",
		ObservationKind: "distribution_feedback",
		SourceKind:      "social_channel",
		SourceRef:       "channel:fixture-social",
		SubjectKind:     "product",
		SubjectID:       "product:focus-notes",
		Domain:          "social-distribution",
		Task:            "distribution feedback",
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	verdict := map[string]any{
		"schema_version": "compound.panel_verdict.v0.1",
		"id":             "panel_verdict_fixture_social",
		"created_at":     tProduct.Format("2006-01-02T15:04:05Z"),
		"run_id":         "prun_fixture_social",
		"target": map[string]any{
			"kind":       string(candidates.CapabilityObs),
			"ref":        payload.ID,
			"visibility": "internal",
			"summary":    payload.Statement,
			"payload":    json.RawMessage(raw),
		},
		"lens_findings": []map[string]any{{
			"lens_ref":           "lens:social-distribution-mechanics",
			"model_id":           "fixture",
			"lane_tier":          "local",
			"route_evidence_ref": "route_fixture_social",
			"confidence":         0.81,
			"findings": []map[string]any{{
				"claim":         "feedback is product-loop candidate material",
				"severity":      "low",
				"evidence_refs": []string{"fixture:social:reply"},
			}},
			"status": "completed",
		}},
		"synthesis": map[string]any{
			"schema_version":           "compound.panel_synthesis.v0.1",
			"decision":                 "park",
			"confidence":               0.81,
			"rationale":                "Feedback is useful as product evidence, but not a signal or truth.",
			"guardrails_applied":       []string{"no_truth_promotion"},
			"judge_route_evidence_ref": "route_fixture_judge",
			"judge_usage":              map[string]any{"total_tokens": 42},
		},
		"evidence_refs": []string{"fixture:social:reply", "route_fixture_social"},
	}
	path := filepath.Join(dir, "panel_verdict_fixture_social.json")
	data, err := json.MarshalIndent(verdict, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestProductObservationVerdictRenderRecordGolden pins the render + record + load
// scaffolding byte-for-byte so a later extraction into internal/verdictflow cannot
// silently change bytes. The two "recorded file == render func output" equalities are
// the shared plan-record seam's teeth: a mutation in verdictflow's record path reddens
// them without touching product's own render code.
func TestProductObservationVerdictRenderRecordGolden(t *testing.T) {
	home := t.TempDir()
	verdictPath := writeProductObservationPanelVerdictFixture(t, home)
	plan, err := BuildObservationVerdictPlan(home, verdictPath, "", tProduct)
	if err != nil {
		t.Fatal(err)
	}

	reviewYAML := RenderObservationVerdictReviewYAML(plan)
	for _, want := range []string{
		"schema_version: compound.product_observation_verdict_review.v0.1\n",
		"decision: TODO_decision_create_hypothesis_candidate_park_reject\n",
		" - \"distribution_feedback_quality\"\n",
		" - \"qualified_reply_rate\"\n",
	} {
		if !strings.Contains(reviewYAML, want) {
			t.Fatalf("review YAML missing %q:\n%s", want, reviewYAML)
		}
	}
	markdown := renderObservationVerdictPlanMarkdown(plan)
	if !strings.HasPrefix(markdown, "# Product Observation Verdict Plan: panel_verdict_fixture_social\n") {
		t.Fatalf("markdown header drift:\n%s", markdown)
	}

	paths, err := RecordObservationVerdictPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := readProductTestFile(t, paths.ReviewYAMLPath); got != reviewYAML {
		t.Fatalf("recorded review.yaml != render:\n got=%q\nwant=%q", got, reviewYAML)
	}
	if got := readProductTestFile(t, paths.ReviewPath); got != markdown {
		t.Fatalf("recorded .md != render:\n got=%q\nwant=%q", got, markdown)
	}

	var reloaded ObservationVerdictPlan
	data, err := os.ReadFile(paths.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &reloaded); err != nil {
		t.Fatal(err)
	}
	if reloaded.ID != plan.ID || reloaded.ReviewDraft.ReviewID != plan.ReviewDraft.ReviewID || reloaded.ProductID != plan.ProductID {
		t.Fatalf("plan round-trip drift: %+v", reloaded)
	}

	filled := strings.NewReplacer(
		"decision: TODO_decision_create_hypothesis_candidate_park_reject", "decision: park",
		"rationale: TODO_rationale", "rationale: keep reviewed observation as product evidence",
		"confidence: TODO_confidence_0_to_1", "confidence: 0.5",
	).Replace(reviewYAML)
	if err := os.WriteFile(paths.ReviewYAMLPath, []byte(filled), 0o644); err != nil {
		t.Fatal(err)
	}
	review, err := LoadObservationVerdictReview(paths.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	if review.Decision != "park" || review.PlanID != plan.ID || review.Confidence != 0.5 {
		t.Fatalf("load round-trip drift: %+v", review)
	}
}
