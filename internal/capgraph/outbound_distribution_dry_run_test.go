package capgraph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/reviewartifact"
)

func TestOutboundDistributionDryRunReceiptConsumesAdoptedVerdictWithoutProductMutation(t *testing.T) {
	home := t.TempDir()
	writeOutboundDistributionFixture(t, home)
	reviewed := recordAdoptedOutboundVerdict(t, home)
	adapterOutputPath := writeOutboundDryRunAdapterOutput(t, home, map[string]any{
		"schema_version":         OutboundDistributionDryRunReceiptSchemaVersion,
		"id":                     "oddr_fixture_postiz_synthetic",
		"plan_id":                reviewed.PlanID,
		"product_id":             reviewed.ProductID,
		"adapter_id":             "adapter:postiz-synthetic",
		"mode":                   "dry_run",
		"run_kind":               "synthetic",
		"public_side_effects":    false,
		"account_auth_used":      false,
		"external_requests_made": false,
		"posted":                 false,
		"replied":                false,
		"spent_money":            false,
		"product_loop": map[string]any{
			"change_event":       "prepare outbound distribution dry-run screenshot change",
			"hypothesis":         "drafts can be reviewed before any public posting",
			"observation_window": "14d after hypothetical publish",
			"next_bounded_move":  "review generated drafts and decide whether to request Tier-3 public distribution policy",
		},
		"artifacts": []map[string]any{{
			"kind":         "draft_post",
			"channel":      "synthetic-social",
			"ref":          "fixture://postiz/draft-1",
			"content_hash": "sha256:fixture-draft",
		}},
		"evidence_refs": []string{"fixture:synthetic-postiz-dry-run"},
	})

	report, err := BuildOutboundDistributionDryRunReceipt(home, OutboundDistributionDryRunReceiptOptions{
		VerdictReviewPath: reviewed.ReviewedPath,
		AdapterOutputPath: adapterOutputPath,
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != OutboundDistributionDryRunReceiptReportSchemaVersion ||
		report.Status != "review_required" ||
		report.AdoptionLevel != "dry_run_only" ||
		report.ProductID != reviewed.ProductID ||
		report.PlanID != reviewed.PlanID ||
		report.AdapterOutputPath != adapterOutputPath ||
		report.ReviewDraft.ReviewCommand == "" {
		t.Fatalf("bad dry-run report: %+v", report)
	}
	if report.Validation.PublicSideEffects || report.Validation.AccountAuthUsed || report.Validation.Posted || report.Validation.ExternalRequestsMade {
		t.Fatalf("dry-run validation must prove no public side effects: %+v", report.Validation)
	}
	if !strings.Contains(strings.Join(report.Boundaries, "\n"), "does not mutate product signals") {
		t.Fatalf("report should keep product ledger mutation out of this slice: %+v", report.Boundaries)
	}
	if !contains(report.EvidenceRefs, reviewed.ReviewedPath) || !contains(report.EvidenceRefs, "fixture:synthetic-postiz-dry-run") {
		t.Fatalf("report should carry verdict and adapter evidence refs: %+v", report.EvidenceRefs)
	}

	paths, err := RecordOutboundDistributionDryRunReceipt(home, report)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.ReportPath, paths.ReviewPath, paths.ReviewYAMLPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing dry-run artifact %s: %v", path, err)
		}
	}
	reviewText, err := os.ReadFile(paths.ReviewPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(reviewText), "No Public Side Effects") || !strings.Contains(string(reviewText), "Product Loop Evidence") {
		t.Fatalf("review markdown should expose side-effect and product-loop sections:\n%s", string(reviewText))
	}
	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: OutboundDistributionDryRunReviewArea}, t0)
	if err != nil {
		t.Fatal(err)
	}
	AnnotateOutboundDistributionDryRunQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.ReviewRequired != 1 || queue.Counts.DecisionCardCount != 1 || queue.Counts.ActionableCount != 1 {
		t.Fatalf("dry-run receipt should be reviewable: counts=%+v items=%+v", queue.Counts, queue.Items)
	}
}

func TestOutboundDistributionDryRunReceiptRejectsPublicSideEffects(t *testing.T) {
	home := t.TempDir()
	writeOutboundDistributionFixture(t, home)
	reviewed := recordAdoptedOutboundVerdict(t, home)
	adapterOutputPath := writeOutboundDryRunAdapterOutput(t, home, map[string]any{
		"schema_version":         OutboundDistributionDryRunReceiptSchemaVersion,
		"id":                     "oddr_bad_public_post",
		"plan_id":                reviewed.PlanID,
		"product_id":             reviewed.ProductID,
		"adapter_id":             "adapter:postiz-synthetic",
		"mode":                   "dry_run",
		"run_kind":               "synthetic",
		"public_side_effects":    true,
		"account_auth_used":      true,
		"external_requests_made": true,
		"posted":                 true,
		"replied":                false,
		"spent_money":            false,
		"product_loop": map[string]any{
			"change_event":       "prepare outbound distribution dry-run screenshot change",
			"hypothesis":         "drafts can be reviewed before any public posting",
			"observation_window": "14d",
			"next_bounded_move":  "stop because adapter posted publicly",
		},
		"artifacts": []map[string]any{{
			"kind":         "published_post",
			"channel":      "social",
			"ref":          "https://example.invalid/post/1",
			"content_hash": "sha256:public",
		}},
		"evidence_refs": []string{"fixture:bad-public-side-effect"},
	})

	_, err := BuildOutboundDistributionDryRunReceipt(home, OutboundDistributionDryRunReceiptOptions{
		VerdictReviewPath: reviewed.ReviewedPath,
		AdapterOutputPath: adapterOutputPath,
	}, t0)
	if err == nil || !strings.Contains(err.Error(), "public side effects") {
		t.Fatalf("expected public side-effect rejection, got %v", err)
	}
}

func recordAdoptedOutboundVerdict(t *testing.T, home string) OutboundDistributionVerdictReviewReport {
	t.Helper()
	targetPath := filepath.Join(t.TempDir(), "target.md")
	if err := os.WriteFile(targetPath, []byte("# outbound target\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildOutboundDistributionVerdictPlan(home, OutboundDistributionVerdictPlanOptions{
		Task:              "evaluate social media distribution outbound posting adapter Postiz Agent-Reach product feedback loops",
		ProductID:         "product:focus-notes",
		ChangeEvent:       "prepare outbound distribution dry-run screenshot change",
		Hypothesis:        "rented posting edge can produce reviewable drafts without public posting",
		ObservationWindow: "14d",
		DryRunReceiptRef:  "receipt:outbound-distribution-dry-run-synthetic",
		TargetPath:        targetPath,
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := RecordOutboundDistributionVerdictPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	reviewYAML, err := os.ReadFile(paths.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.ReplaceAll(string(reviewYAML), "TODO_decision_adopt_for_dry_run_park_reject", "adopt_for_dry_run")
	body = strings.ReplaceAll(body, "TODO_rationale", "synthetic dry-run receipt should be validated before any public distribution")
	body = strings.ReplaceAll(body, "TODO_confidence_0_to_1", "0.72")
	body = strings.ReplaceAll(body, "TODO_dry_run_receipt_ref", "receipt:outbound-distribution-dry-run-synthetic")
	reviewPath := filepath.Join(t.TempDir(), "outbound-verdict-review.yaml")
	if err := os.WriteFile(reviewPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	review, err := LoadOutboundDistributionVerdictReview(reviewPath)
	if err != nil {
		t.Fatal(err)
	}
	report, err := RecordOutboundDistributionVerdictReview(home, review, reviewPath, t0)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func writeOutboundDryRunAdapterOutput(t *testing.T, home string, payload map[string]any) string {
	t.Helper()
	path := filepath.Join(home, "tmp", payload["id"].(string)+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(payload, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
