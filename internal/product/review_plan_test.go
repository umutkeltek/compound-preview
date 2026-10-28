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

func TestReviewPlanRecordsGatedReviewArtifactsWithoutResolvingHypothesis(t *testing.T) {
	home := t.TempDir()
	candidateID := "hyp_focus_conversion_review"
	dir := filepath.Join(home, candidates.InboxDir(candidates.ProductHypothesis))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	candidate := HypothesisCandidate{
		SchemaVersion:     ProductHypothesisCandidate,
		ID:                candidateID,
		Kind:              string(candidates.ProductHypothesis),
		ProductID:         "product:focus-notes",
		ChangeEvent:       "Changed first screenshot to show faster capture.",
		ExpectedEffect:    "conversion improves because screenshot communicates capture speed",
		Metrics:           []string{"app_store_product_page_conversion"},
		ObservationWindow: "14d after change",
		Result:            "unknown",
		NextAction:        "review explicit conversion signal",
		Verdict:           candidates.Park,
		EvidenceRefs:      []string{"product_source_issue_review:review_focus"},
	}
	data, err := json.MarshalIndent(candidate, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, candidateID+".json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	signalDoc := SignalDocument{
		SchemaVersion: SignalSchemaVersion,
		Source:        SignalSource{Kind: "app_store_connect", Ref: "fixture:signal-window"},
		Signal: Signal{
			ID:           "sig_focus_conversion_window",
			ProductID:    "product:focus-notes",
			Surface:      "app-store-page",
			ChangeEvent:  "Changed first screenshot to show faster capture.",
			MetricName:   "app_store_product_page_conversion",
			Before:       0.041,
			After:        0.049,
			Window:       "before 2026-06-01/2026-06-07; after 2026-06-15/2026-06-21",
			Confidence:   0.66,
			Confounders:  []string{"paid acquisition changed during window"},
			EvidenceRefs: []string{"asc:sales:focus:window"},
		},
	}
	signalReport, err := ApplySignal(home, store, signalDoc, "fixture/product-signal.yaml", tProduct)
	if err != nil {
		t.Fatal(err)
	}
	if signalReport.Status != "ok" || len(signalReport.SignalIDs) != 1 || len(signalReport.TransitionIDs) != 1 {
		t.Fatalf("signal report = %+v", signalReport)
	}

	targetPath := filepath.Join(t.TempDir(), "product-review.md")
	original := "# Product Review\n\nExisting notes.\n"
	if err := os.WriteFile(targetPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildReviewPlan(home, candidateID, signalDoc.Signal.ID, targetPath, tProduct.AddDate(0, 0, 15))
	if err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != ReviewPlanSchemaVersion ||
		plan.Status != "review_required" ||
		plan.ProductID != "product:focus-notes" ||
		plan.HypothesisID != candidateID ||
		plan.SignalID != signalDoc.Signal.ID ||
		plan.ReviewDraft.ReviewID == "" {
		t.Fatalf("bad review plan: %+v", plan)
	}
	if !hasString(plan.EvidenceRefs, "product_signal:"+signalDoc.Signal.ID) ||
		!hasString(plan.EvidenceRefs, "product_hypothesis:"+candidateID) ||
		!hasString(plan.EvidenceRefs, "asc:sales:focus:window") {
		t.Fatalf("plan evidence refs = %+v", plan.EvidenceRefs)
	}

	paths, err := RecordReviewPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.PlanPath, paths.ReviewPath, paths.ReviewYAMLPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing review artifact %s: %v", path, err)
		}
	}
	reviewText := readProductTestFile(t, paths.ReviewPath)
	if !strings.Contains(reviewText, "does not resolve the hypothesis") ||
		!strings.Contains(reviewText, "Required Evidence Before Review") {
		t.Fatalf("review plan missing guardrails:\n%s", reviewText)
	}

	unedited, err := LoadReview(paths.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyReview(home, store, unedited, paths.ReviewYAMLPath, tProduct.AddDate(0, 0, 16)); err == nil {
		t.Fatal("unedited review YAML should be rejected")
	}
	yamlDraft := readProductTestFile(t, paths.ReviewYAMLPath)
	yamlDraft = strings.ReplaceAll(yamlDraft, "TODO_result_supported_refuted_or_inconclusive", "supported")
	yamlDraft = strings.ReplaceAll(yamlDraft, "rationale: \"\"", "rationale: \"signal moved in expected direction after confounder review\"")
	yamlDraft = strings.ReplaceAll(yamlDraft, "confidence: -1", "confidence: 0.61")
	yamlDraft = strings.ReplaceAll(yamlDraft, "TODO_reviewed_by_actor", "operator")
	reviewPath := filepath.Join(t.TempDir(), "product-review.yaml")
	if err := os.WriteFile(reviewPath, []byte(yamlDraft), 0o644); err != nil {
		t.Fatal(err)
	}
	review, err := LoadReview(reviewPath)
	if err != nil {
		t.Fatal(err)
	}
	reviewReport, err := ApplyReview(home, store, review, reviewPath, tProduct.AddDate(0, 0, 17))
	if err != nil {
		t.Fatal(err)
	}
	if reviewReport.Status != "updated" || reviewReport.CandidateOutcome != "committed" || reviewReport.HypothesisID != candidateID || reviewReport.TransitionID == "" || reviewReport.EvidenceID == "" {
		t.Fatalf("review report = %+v", reviewReport)
	}
	doctor, err := DoctorWithNow(home, "product:focus-notes", tProduct.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "ok" || doctor.PendingReviewCount != 0 {
		t.Fatalf("review should clear pending hypothesis debt: %+v", doctor)
	}
	if got := readProductTestFile(t, targetPath); got != original {
		t.Fatalf("target mutated:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("review plan must not promote claims, stat err=%v", err)
	}
}

func TestAnnotateReviewQueueAddsProductReviewAction(t *testing.T) {
	reviewDir := filepath.Join(t.TempDir(), "product review")
	if err := os.MkdirAll(reviewDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reviewYAML := filepath.Join(reviewDir, "prp_review.review.yaml")
	if err := os.WriteFile(reviewYAML, []byte(`schema_version: compound.product_review.v0.1
id: review_focus
product_id: product:focus-notes
hypothesis_id: hyp_focus
result: TODO_result_supported_refuted_or_inconclusive
rationale: ""
confidence: -1
evidence_refs:
 - product_signal:sig_focus
`), 0o644); err != nil {
		t.Fatal(err)
	}
	report := reviewartifact.QueueReport{
		Items: []reviewartifact.QueueItem{
			{
				Area:       productReviewArea,
				ID:         "prp_review",
				Status:     "review_required",
				JSONPath:   filepath.Join(reviewDir, "prp_review.json"),
				ExtraPaths: []string{reviewYAML},
			},
			{
				Area:     productReviewArea,
				ID:       "prp_ok",
				Status:   "ok",
				JSONPath: "/tmp/prp_ok.json",
			},
			{
				Area:     "decision-lifecycle-repairs",
				ID:       "dlrp_other",
				Status:   "review_required",
				JSONPath: "/tmp/decision.json",
			},
		},
	}
	AnnotateReviewQueue(&report)
	review := report.Items[0]
	if review.Readiness != "needs_review" || review.NextAction == "" || len(review.Actions) != 1 {
		t.Fatalf("product review item not actionable: %+v", review)
	}
	if !strings.Contains(review.BlockedReason, "review result must be supported|refuted|inconclusive") {
		t.Fatalf("product review item should expose validation blocker: %+v", review)
	}
	action := review.Actions[0]
	if !action.RequiresReview || action.ID != "record_product_review" || !strings.Contains(action.Command, "'"+reviewYAML+"'") {
		t.Fatalf("review action should stay review-gated shell-safe: %+v", action)
	}
	if len(action.Argv) != 4 || action.Argv[0] != "compound" || action.Argv[1] != "product" || action.Argv[2] != "review" {
		t.Fatalf("review action argv = %+v", action.Argv)
	}
	if report.Items[1].Readiness != "no_action" || len(report.Items[1].Actions) != 0 {
		t.Fatalf("terminal product review should not be actionable: %+v", report.Items[1])
	}
	if report.Items[2].Readiness != "" || len(report.Items[2].Actions) != 0 {
		t.Fatalf("non-product area should stay generic: %+v", report.Items[2])
	}
}

func readProductTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
