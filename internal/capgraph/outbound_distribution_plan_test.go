package capgraph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/reviewartifact"
)

func TestOutboundDistributionPlanConsumesMatchedExternalRepoEvidence(t *testing.T) {
	home := t.TempDir()
	writeOutboundDistributionFixture(t, home)

	report, err := BuildOutboundDistributionPlan(home, "evaluate social media distribution outbound posting adapter with Postiz and Agent-Reach for product feedback loops", t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != OutboundDistributionPlanSchemaVersion ||
		report.AuthorityRef != OutboundDistributionPlanAuthorityRef ||
		report.Status != "review_ready" {
		t.Fatalf("report header/status = %+v", report)
	}
	if report.AdoptionLevel != "evidence_only" || len(report.RequiredGates) < 4 {
		t.Fatalf("plan must stay evidence-only with explicit gates: %+v", report)
	}
	for _, want := range []string{
		"tier3_public_distribution_policy",
		"product_loop_target",
		"dry_run_receipt",
		"no_social_auth_or_posting",
	} {
		if !planHasGate(report.RequiredGates, want) {
			t.Fatalf("plan missing gate %q: %+v", want, report.RequiredGates)
		}
	}
	postiz := outboundCandidateByRepo(report.Candidates, "gitroomhq/postiz-app")
	if postiz == nil {
		t.Fatalf("Postiz candidate missing: %+v", report.Candidates)
	}
	if postiz.Role != "posting_adapter_candidate" ||
		postiz.CapabilityGraphStatus != "imported" ||
		postiz.NodeStatus != "needs_review" ||
		!strings.Contains(postiz.Boundary, "no social-account auth") ||
		!strings.Contains(postiz.NextReviewStep, "PolicyDecision") {
		t.Fatalf("Postiz candidate should preserve gated adapter evidence: %+v", postiz)
	}
	reach := outboundCandidateByRepo(report.Candidates, "Panniantong/Agent-Reach")
	if reach == nil || reach.Role != "reach_research_reference" {
		t.Fatalf("Agent-Reach should remain reach reference evidence: %+v", report.Candidates)
	}
	if outboundCandidateByRepo(report.Candidates, "cobusgreyling/loop-engineering") != nil {
		t.Fatalf("loop-engineering doctrine evidence should not be promoted into outbound adapter candidates: %+v", report.Candidates)
	}
}

func TestOutboundDistributionPlanNoEvidenceForUnrelatedTask(t *testing.T) {
	home := t.TempDir()
	writeOutboundDistributionFixture(t, home)

	report, err := BuildOutboundDistributionPlan(home, "evaluate memory transcript ingestion adapters", t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "no_distribution_evidence" || len(report.Candidates) != 0 {
		t.Fatalf("unrelated task should not surface distribution candidates: %+v", report)
	}
}

func TestOutboundDistributionVerdictPlanRecordsReviewArtifactsWithoutMutation(t *testing.T) {
	home := t.TempDir()
	writeOutboundDistributionFixture(t, home)
	targetPath := filepath.Join(t.TempDir(), "product-loop.md")
	original := "# Product Loop\n\nExisting outbound notes.\n"
	if err := os.WriteFile(targetPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := BuildOutboundDistributionVerdictPlan(home, OutboundDistributionVerdictPlanOptions{
		Task:              "evaluate social media distribution outbound posting adapter with Postiz and Agent-Reach for product feedback loops",
		ProductID:         "product:focus-notes",
		ChangeEvent:       "prepare outbound distribution dry-run for app-store screenshot change",
		Hypothesis:        "a rented posting edge can produce reviewable channel drafts without public posting",
		ObservationWindow: "14d after dry-run",
		DryRunReceiptRef:  "receipt:outbound-distribution-dry-run-synthetic",
		TargetPath:        targetPath,
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != OutboundDistributionVerdictPlanSchemaVersion ||
		plan.Status != "review_required" ||
		plan.ProductID != "product:focus-notes" ||
		plan.AdoptionLevel != "verdict_only" ||
		!plan.TargetExists ||
		plan.TargetHash == "" ||
		len(plan.Candidates) != 2 {
		t.Fatalf("bad verdict plan: %+v", plan)
	}
	if !planHasGate(plan.RequiredGates, "tier3_public_distribution_policy") ||
		!planHasGate(plan.RequiredGates, "product_loop_target") ||
		!planHasGate(plan.RequiredGates, "dry_run_receipt") ||
		!planHasGate(plan.RequiredGates, "no_social_auth_or_posting") {
		t.Fatalf("verdict plan missing gates: %+v", plan.RequiredGates)
	}
	if !contains(plan.EvidenceRefs, "compound://reviews/external-repo-intake/gitroomhq/postiz-app") ||
		!contains(plan.EvidenceRefs, "compound://reviews/external-repo-intake/panniantong/agent-reach") {
		t.Fatalf("verdict plan should carry external repo evidence refs: %+v", plan.EvidenceRefs)
	}
	if !strings.Contains(strings.Join(plan.ReviewDraft.RequiredEvidence, "\n"), "receipt:outbound-distribution-dry-run-synthetic") ||
		!strings.Contains(plan.ReviewDraft.ReviewCommand, "compound capability outbound-distribution-verdict-review") {
		t.Fatalf("review draft should require dry-run receipt and canonical review command: %+v", plan.ReviewDraft)
	}

	paths, err := RecordOutboundDistributionVerdictPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.PlanPath, paths.ReviewPath, paths.ReviewYAMLPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing artifact %s: %v", path, err)
		}
	}
	reviewText, err := os.ReadFile(paths.ReviewPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(reviewText), "Required Evidence Before Verdict") ||
		!strings.Contains(string(reviewText), "no social account auth, posting, replying, or public side effect") {
		t.Fatalf("review text missing gates:\n%s", reviewText)
	}

	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: OutboundDistributionVerdictReviewArea}, t0)
	if err != nil {
		t.Fatal(err)
	}
	AnnotateOutboundDistributionVerdictQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.ReviewRequired != 1 ||
		queue.Counts.DecisionCardCount != 1 ||
		queue.Counts.ActionableCount != 1 ||
		queue.Counts.ReviewRequiredMissingDecisionCard != 0 ||
		queue.Counts.ReviewRequiredMissingAction != 0 {
		t.Fatalf("queue should expose actionable verdict review card: %+v", queue.Counts)
	}
	item := queue.Items[0]
	if item.DecisionCard == nil ||
		!strings.Contains(item.DecisionCard.Claim, "product:focus-notes") ||
		len(item.Actions) != 1 ||
		!strings.Contains(item.Actions[0].Command, "compound capability outbound-distribution-verdict-review") {
		t.Fatalf("queue item should expose decision card and review command: %+v", item)
	}
	if got, err := os.ReadFile(targetPath); err != nil || string(got) != original {
		t.Fatalf("target mutated err=%v:\n%s", err, got)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "product-signals.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("verdict plan must not create product signals: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("verdict plan must not promote claims: %v", err)
	}
}

func TestOutboundDistributionVerdictReviewRecordsTerminalArtifact(t *testing.T) {
	home := t.TempDir()
	writeOutboundDistributionFixture(t, home)
	plan, err := BuildOutboundDistributionVerdictPlan(home, OutboundDistributionVerdictPlanOptions{
		Task:              "evaluate outbound posting adapter with Postiz",
		ProductID:         "product:focus-notes",
		ChangeEvent:       "prepare outbound distribution dry-run",
		Hypothesis:        "drafted posts can be reviewed before public distribution",
		ObservationWindow: "7d",
		DryRunReceiptRef:  "receipt:dry-run",
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := RecordOutboundDistributionVerdictPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := os.ReadFile(paths.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	reviewYAML := strings.ReplaceAll(string(draft), "TODO_decision_adopt_for_dry_run_park_reject", "park")
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_rationale", "keep as evidence until product loop target is chosen")
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_confidence_0_to_1", "0.61")
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_dry_run_receipt_ref", "receipt:dry-run")
	reviewPath := filepath.Join(t.TempDir(), "outbound-review.yaml")
	if err := os.WriteFile(reviewPath, []byte(reviewYAML), 0o644); err != nil {
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
	if report.Status != "recorded" ||
		report.Decision != "park" ||
		report.ReviewedPath == "" ||
		report.PlanID != plan.ID ||
		report.ProductID != "product:focus-notes" {
		t.Fatalf("bad review report: %+v", report)
	}
	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: OutboundDistributionVerdictReviewArea}, t0)
	if err != nil {
		t.Fatal(err)
	}
	AnnotateOutboundDistributionVerdictQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.ReviewRequired != 0 ||
		queue.Counts.OK != 1 ||
		queue.Items[0].Readiness != "no_action" {
		t.Fatalf("reviewed verdict should terminalize queue item: counts=%+v item=%+v", queue.Counts, queue.Items[0])
	}
}

func writeOutboundDistributionFixture(t *testing.T, home string) {
	t.Helper()
	batchDir := filepath.Join(home, "inbox", "external-repo-intake", "2026-07-04-distribution")
	if err := os.MkdirAll(batchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mapPath := filepath.Join(batchDir, "external-repo-intake-map.json")
	writeJSON(t, mapPath, map[string]any{
		"boundaries": []string{"review-only", "no install"},
		"items": []map[string]any{
			{
				"repo":             "gitroomhq/postiz-app",
				"owner":            "product outbound distribute pipe (Tier-3 gated)",
				"next_review_step": "evaluate rented posting edge behind PolicyDecision; posting receipts feed product loops",
				"not_to_do":        "no install/serve, no social-account auth, no auto-posting",
			},
			{
				"repo":             "Panniantong/Agent-Reach",
				"owner":            "knowledge intake edges / distribution research",
				"next_review_step": "mine channel/reach patterns evidence for outbound-distribute packet; no execution",
				"not_to_do":        "no agent execution, no MCP auth, no channel account access",
			},
			{
				"repo":             "cobusgreyling/loop-engineering",
				"owner":            "product loop model + distiller loop doctrine",
				"next_review_step": "operator correction: not a distribution/reach item; audit loop-engineering doctrine quality",
				"not_to_do":        "no wholesale import external loop framework",
			},
		},
	})
	writeJSON(t, filepath.Join(batchDir, "run-summary.json"), map[string]any{
		"schema_version":          "compound.external_repo_intake_run.v0.1",
		"id":                      "2026-07-04-distribution",
		"created_at":              "2026-07-04T07:50:00Z",
		"map_path":                mapPath,
		"source_observation_path": filepath.Join(batchDir, "source-observation.json"),
		"candidate_count":         3,
		"counts_by_decision": map[string]any{
			"keep_needs_review": 3,
		},
		"counts_by_lane": map[string]any{
			"distribution_adapter_candidate": 1,
			"capture_edge_reach_reference":   1,
			"feedback_loop_doctrine_audit":   1,
		},
		"decisions": []map[string]any{
			{
				"repo":              "gitroomhq/postiz-app",
				"decision":          "keep_needs_review",
				"lane":              "distribution_adapter_candidate",
				"status":            "recorded",
				"evidence_strength": 0.9,
			},
			{
				"repo":              "Panniantong/Agent-Reach",
				"decision":          "keep_needs_review",
				"lane":              "capture_edge_reach_reference",
				"status":            "recorded",
				"evidence_strength": 0.76,
			},
			{
				"repo":              "cobusgreyling/loop-engineering",
				"decision":          "keep_needs_review",
				"lane":              "feedback_loop_doctrine_audit",
				"status":            "recorded",
				"evidence_strength": 0.74,
			},
		},
	})
	sourceListPath := filepath.Join(batchDir, "source-list.md")
	if err := os.WriteFile(sourceListPath, []byte(strings.Join([]string{
		"https://github.com/gitroomhq/postiz-app",
		"https://github.com/Panniantong/Agent-Reach",
		"https://github.com/cobusgreyling/loop-engineering",
	}, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reviewPlan, err := WriteExternalRepoIntakeReviewPlan(home, sourceListPath, t0)
	if err != nil {
		t.Fatalf("write external repo review plan: %v", err)
	}
	recorded := map[string]bool{
		"gitroomhq/postiz-app":           true,
		"Panniantong/Agent-Reach":        true,
		"cobusgreyling/loop-engineering": true,
	}
	for _, item := range reviewPlan.Items {
		if !recorded[item.Repo] {
			continue
		}
		if _, err := RecordExternalRepoIntakeReview(home, ExternalRepoIntakeReview{
			SchemaVersion: ExternalRepoIntakeReviewSchemaVersion,
			ID:            "review_" + item.ID,
			PlanID:        item.ID,
			Repo:          item.Repo,
			Decision:      "keep_needs_review",
			Owner:         item.Owner,
			Rationale:     "reviewed fixture evidence; keep gated",
			Confidence:    item.GraphEvidenceStrength,
			NextAction:    item.NextAction,
			EvidenceRefs:  item.EvidenceRefs,
			Boundaries:    item.Boundaries,
		}, item.ReviewYAMLPath, t0); err != nil {
			t.Fatalf("record owner review for %s: %v", item.Repo, err)
		}
	}
	for _, repo := range []string{"gitroomhq/postiz-app", "Panniantong/Agent-Reach"} {
		evalReport, err := RecordExternalRepoCapabilityEvaluationPlan(home, ExternalRepoCapabilityEvaluationPlanOptions{
			SourceListPath: sourceListPath,
			RepoQuery:      repo,
		}, t0)
		if err != nil {
			t.Fatalf("record capability evaluation for %s: %v", repo, err)
		}
		data, err := os.ReadFile(evalReport.PlanPath)
		if err != nil {
			t.Fatalf("read capability plan for %s: %v", repo, err)
		}
		var plan CapabilityEvaluationPlan
		if err := json.Unmarshal(data, &plan); err != nil {
			t.Fatalf("parse capability plan for %s: %v", repo, err)
		}
		reviewReport, err := RecordCapabilityEvaluationReview(home, CapabilityEvaluationReview{
			SchemaVersion: CapabilityEvaluationReviewSchemaVersion,
			ReviewID:      "review_" + plan.ID,
			PlanID:        plan.ID,
			EvaluationID:  plan.EvaluationID,
			Capability:    plan.Capability,
			Decision:      "keep_needs_review",
			Rationale:     "keep as evidence for outbound distribution; do not activate",
			Confidence:    0.82,
			NodeDraft:     plan.NodeDraft,
			EvidenceRefs:  plan.EvidenceRefs,
		}, evalReport.ReviewYAMLPath, t0)
		if err != nil {
			t.Fatalf("record capability review for %s: %v", repo, err)
		}
		manifest, err := LoadSkillManifest(reviewReport.ManifestPath)
		if err != nil {
			t.Fatalf("load reviewed manifest for %s: %v", repo, err)
		}
		if _, err := ApplySkillManifest(home, manifest, t0); err != nil {
			t.Fatalf("import reviewed manifest for %s: %v", repo, err)
		}
	}
}

func outboundCandidateByRepo(candidates []OutboundDistributionCandidate, repo string) *OutboundDistributionCandidate {
	for i := range candidates {
		if candidates[i].Repo == repo {
			return &candidates[i]
		}
	}
	return nil
}

func planHasGate(gates []OutboundDistributionGate, kind string) bool {
	for _, gate := range gates {
		if gate.Kind == kind {
			return true
		}
	}
	return false
}

// TestOutboundDistributionVerdictRenderRecordGolden pins the render + record + load
// scaffolding byte-for-byte across the extraction. The two "recorded file == render
// func output" equalities exercise the shared plan-record seam (verdictflow), so a
// mutation there reddens this golden without touching capgraph's own render code.
func TestOutboundDistributionVerdictRenderRecordGolden(t *testing.T) {
	home := t.TempDir()
	writeOutboundDistributionFixture(t, home)
	plan, err := BuildOutboundDistributionVerdictPlan(home, OutboundDistributionVerdictPlanOptions{
		Task:              "evaluate outbound posting adapter with Postiz",
		ProductID:         "product:focus-notes",
		ChangeEvent:       "prepare outbound distribution dry-run",
		Hypothesis:        "drafted posts can be reviewed before public distribution",
		ObservationWindow: "7d",
		DryRunReceiptRef:  "receipt:dry-run",
	}, t0)
	if err != nil {
		t.Fatal(err)
	}

	reviewYAML := RenderOutboundDistributionVerdictReviewYAML(plan)
	for _, want := range []string{
		"schema_version: compound.capability.outbound_distribution_verdict_review.v0.1\n",
		"decision: TODO_decision_adopt_for_dry_run_park_reject\n",
		"product_id: product:focus-notes\n",
		"dry_run_receipt_ref: TODO_dry_run_receipt_ref\n",
	} {
		if !strings.Contains(reviewYAML, want) {
			t.Fatalf("review YAML missing %q:\n%s", want, reviewYAML)
		}
	}
	markdown := RenderOutboundDistributionVerdictPlanReview(plan)
	if !strings.HasPrefix(markdown, "# Outbound Distribution Verdict Plan: product:focus-notes\n") {
		t.Fatalf("markdown header drift:\n%s", markdown)
	}

	paths, err := RecordOutboundDistributionVerdictPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := readOutboundVerdictGoldenFile(t, paths.ReviewYAMLPath); got != reviewYAML {
		t.Fatalf("recorded review.yaml != render:\n got=%q\nwant=%q", got, reviewYAML)
	}
	if got := readOutboundVerdictGoldenFile(t, paths.ReviewPath); got != markdown {
		t.Fatalf("recorded .md != render:\n got=%q\nwant=%q", got, markdown)
	}

	filled := strings.NewReplacer(
		"TODO_decision_adopt_for_dry_run_park_reject", "park",
		"TODO_rationale", "keep as evidence until product loop target is chosen",
		"TODO_confidence_0_to_1", "0.4",
		"TODO_dry_run_receipt_ref", "receipt:dry-run",
	).Replace(reviewYAML)
	reviewPath := filepath.Join(t.TempDir(), "golden-review.yaml")
	if err := os.WriteFile(reviewPath, []byte(filled), 0o644); err != nil {
		t.Fatal(err)
	}
	review, err := LoadOutboundDistributionVerdictReview(reviewPath)
	if err != nil {
		t.Fatal(err)
	}
	if review.Decision != "park" || review.PlanID != plan.ID || review.DryRunReceiptRef != "receipt:dry-run" || review.Confidence != 0.4 {
		t.Fatalf("load round-trip drift: %+v", review)
	}
}

func readOutboundVerdictGoldenFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
