package product

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/candidates"
	"compound/internal/mem"
	"compound/internal/reviewartifact"
)

func TestOutboundDryRunEvidencePlanCreatesReviewableProductLoopBridge(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	receiptPath, reviewPath := writeAcceptedOutboundDryRunFixture(t, home)
	targetPath := filepath.Join(t.TempDir(), "product-loop.md")
	if err := os.WriteFile(targetPath, []byte("# Product loop\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := BuildOutboundDryRunEvidencePlan(home, reviewPath, targetPath, tProduct)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != OutboundDryRunEvidencePlanSchemaVersion ||
		plan.Status != "review_required" ||
		plan.ProductID != "product:focus-notes" ||
		plan.ReceiptID != "oddr_fixture_postiz_synthetic" ||
		plan.ReceiptPath != receiptPath ||
		plan.ReviewSourcePath != reviewPath ||
		plan.ReviewDraft.ReviewCommand != "compound product outbound-dry-run-review <review.yaml>" ||
		len(plan.ReviewDraft.RequiredEvidence) == 0 ||
		len(plan.EvidenceRefs) < 3 {
		t.Fatalf("bad outbound dry-run evidence plan: %+v", plan)
	}
	if !containsProductString(plan.Warnings, "does not mutate ProductSignal ledger") ||
		!containsProductString(plan.Warnings, "does not publish or authenticate social accounts") {
		t.Fatalf("plan must carry product/public-action boundaries: %+v", plan.Warnings)
	}

	paths, err := RecordOutboundDryRunEvidencePlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.PlanPath, paths.ReviewPath, paths.ReviewYAMLPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing outbound dry-run product artifact %s: %v", path, err)
		}
	}
	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: OutboundDryRunEvidenceReviewArea}, tProduct)
	if err != nil {
		t.Fatal(err)
	}
	AnnotateOutboundDryRunEvidenceQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.ReviewRequired != 1 || queue.Counts.DecisionCardCount != 1 || queue.Counts.ActionableCount != 1 {
		t.Fatalf("product outbound dry-run bridge should be reviewable: counts=%+v items=%+v", queue.Counts, queue.Items)
	}

	reviewYAML, err := os.ReadFile(paths.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	reviewBody := strings.ReplaceAll(string(reviewYAML), "TODO_decision_create_hypothesis_candidate_park_reject", "create_hypothesis_candidate")
	reviewBody = strings.ReplaceAll(reviewBody, "TODO_rationale", "synthetic drafts are useful enough to create a product-loop hypothesis")
	reviewBody = strings.ReplaceAll(reviewBody, "TODO_confidence_0_to_1", "0.66")
	reviewBody = strings.ReplaceAll(reviewBody, "TODO_expected_effect", "reviewed outbound drafts improve product-message clarity before public distribution")
	reviewBody = strings.ReplaceAll(reviewBody, "TODO_primary_metric", "distribution_draft_quality")
	reviewYAMLPath := filepath.Join(t.TempDir(), "product-outbound-review.yaml")
	if err := os.WriteFile(reviewYAMLPath, []byte(reviewBody), 0o644); err != nil {
		t.Fatal(err)
	}
	review, err := LoadOutboundDryRunEvidenceReview(reviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	report, err := ApplyOutboundDryRunEvidenceReview(home, store, review, reviewYAMLPath, tProduct)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "recorded" || report.Decision != "create_hypothesis_candidate" || report.HypothesisID == "" || report.TransitionID == "" || report.EvidenceID == "" {
		t.Fatalf("bad outbound dry-run evidence review report: %+v", report)
	}
	candidate, err := loadHypothesisCandidate(home, report.HypothesisID)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.ProductID != "product:focus-notes" ||
		candidate.ChangeEvent != "prepare outbound distribution dry-run screenshot change" ||
		candidate.ExpectedEffect != "reviewed outbound drafts improve product-message clarity before public distribution" ||
		!containsProductString(candidate.EvidenceRefs, "outbound_dry_run_review:"+review.ID) ||
		!containsProductString(candidate.EvidenceRefs, report.EvidenceID) {
		t.Fatalf("candidate should carry reviewed dry-run evidence: %+v", candidate)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "product-signals.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("outbound dry-run bridge must not create product signals: %v", err)
	}
}

func writeAcceptedOutboundDryRunFixture(t *testing.T, home string) (string, string) {
	t.Helper()
	areaDir := filepath.Join(home, "reviews", "outbound-distribution-dry-runs")
	if err := os.MkdirAll(areaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	receipt := map[string]any{
		"schema_version":      "compound.capability.outbound_distribution_dry_run_receipt_report.v0.1",
		"id":                  "oddr_fixture_postiz_synthetic",
		"created_at":          tProduct.Format("2006-01-02T15:04:05Z"),
		"status":              "review_required",
		"authority_ref":       "compound://capability/outbound-distribution-dry-run",
		"plan_id":             "odvp_focus_postiz",
		"product_id":          "product:focus-notes",
		"verdict_review_path": filepath.Join(areaDir, "odvp_focus_postiz.reviewed.json"),
		"adapter_output_path": "fixture://adapter-output",
		"adoption_level":      "dry_run_only",
		"adapter_id":          "adapter:postiz-synthetic",
		"evidence_refs":       []string{"fixture:dry-run", "compound://reviews/external-repo-intake/gitroomhq/postiz-app"},
		"boundaries":          []string{"does not mutate product signals", "public distribution remains Tier-3 gated"},
		"artifacts":           []map[string]any{{"kind": "draft_post", "channel": "synthetic-social", "ref": "fixture://postiz/draft-1", "content_hash": "sha256:fixture"}},
		"product_loop":        map[string]any{"change_event": "prepare outbound distribution dry-run screenshot change", "hypothesis": "drafts can be reviewed before public posting", "observation_window": "14d", "next_bounded_move": "review drafts and decide whether Tier-3 publish policy is warranted"},
		"validation":          map[string]any{"mode": "dry_run", "run_kind": "synthetic", "public_side_effects": false, "account_auth_used": false, "external_requests_made": false, "posted": false, "replied": false, "spent_money": false, "artifact_count": 1},
		"target_exists":       false,
	}
	reviewed := map[string]any{
		"schema_version":     "compound.capability.outbound_distribution_dry_run_review_report.v0.1",
		"id":                 "oddr_review_fixture",
		"created_at":         tProduct.Format("2006-01-02T15:04:05Z"),
		"status":             "recorded",
		"receipt_id":         "oddr_fixture_postiz_synthetic",
		"plan_id":            "odvp_focus_postiz",
		"product_id":         "product:focus-notes",
		"decision":           "accept_as_product_loop_evidence",
		"reviewed_path":      filepath.Join(areaDir, "oddr_fixture_postiz_synthetic.reviewed.json"),
		"review_source_path": "fixture://dry-run-review.yaml",
		"evidence_refs":      []string{"fixture:dry-run-review"},
	}
	receiptPath := filepath.Join(areaDir, "oddr_fixture_postiz_synthetic.json")
	reviewPath := filepath.Join(areaDir, "oddr_fixture_postiz_synthetic.reviewed.json")
	writeProductJSON(t, receiptPath, receipt)
	writeProductJSON(t, reviewPath, reviewed)
	return receiptPath, reviewPath
}

func writeProductJSON(t *testing.T, path string, payload any) {
	t.Helper()
	data, err := json.MarshalIndent(payload, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func containsProductString(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}

var _ = candidates.ProductHypothesis
