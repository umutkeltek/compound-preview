package capgraph

import (
	homepkg "compound/internal/home"
	"compound/internal/reviewartifact"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExternalRepoMatchesPreserveReportReviewSnapshot(t *testing.T) {
	home := t.TempDir()
	decision := ExternalRepoIntakeDecision{
		Repo: "example/review-tool", OwnerReviewStatus: "recorded",
		OwnerReviewDecision: "reference_only", OwnerReviewPath: "snapshot.reviewed.json",
	}
	dir := filepath.Join(home, "reviews", ExternalRepoIntakeReviewArea)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A review written after the report was projected belongs to the next
	// report, not a second, contradictory read while selecting its matches.
	data, err := json.Marshal(ExternalRepoIntakeReviewReport{
		Repo: decision.Repo, Status: "recorded", Decision: "reject",
		ReviewPath: "new.reviewed.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.reviewed.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	matches := externalRepoMatchesForQuery(home, []ExternalRepoIntakeDecision{decision}, decision.Repo)
	if len(matches) != 1 || matches[0].OwnerReviewDecision != "reference_only" || matches[0].OwnerReviewPath != decision.OwnerReviewPath {
		t.Fatalf("match selection changed the report review snapshot: %+v", matches)
	}
	// No process-global cache: a new report must see the changed decision.
	fresh := annotateExternalRepoDecisionState(home, []ExternalRepoIntakeDecision{decision})
	if fresh[0].OwnerReviewDecision != "reject" || fresh[0].OwnerReviewPath != "new.reviewed.json" {
		t.Fatalf("fresh report missed the changed review: %+v", fresh)
	}
}

func TestExternalRepoIntakeReportForHome(t *testing.T) {
	home := t.TempDir()

	missing, err := ExternalRepoIntakeReportForHome(home)
	if err != nil {
		t.Fatalf("missing report should not fail: %v", err)
	}
	if missing.Status != "missing" || missing.AuthorityRef != ExternalRepoIntakeAuthorityRef {
		t.Fatalf("missing report = %+v", missing)
	}

	batchDir := filepath.Join(home, "inbox", "external-repo-intake", "2026-07-01-test")
	if err := os.MkdirAll(batchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mapPath := filepath.Join(batchDir, "external-repo-intake-map.json")
	writeJSON(t, mapPath, map[string]any{
		"boundaries": []string{"review-only", "no install"},
		"items": []map[string]any{
			{
				"repo":             "example/high-value",
				"owner":            "deep-research",
				"next_review_step": "run freshness eval",
				"not_to_do":        "do not auto-run social research",
			},
			{
				"repo":             "example/merge",
				"owner":            "existing-owner",
				"next_review_step": "merge deltas only",
				"not_to_do":        "do not duplicate skill",
			},
		},
	})
	summaryPath := filepath.Join(batchDir, "run-summary.json")
	writeJSON(t, summaryPath, map[string]any{
		"schema_version":          "compound.external_repo_intake_run.v0.1",
		"id":                      "2026-07-01-test",
		"created_at":              "2026-07-01T12:00:00Z",
		"map_path":                mapPath,
		"source_observation_path": filepath.Join(batchDir, "source-observation.json"),
		"candidate_count":         2,
		"source_observe_output": []string{
			"# source observation srcobs_external_test",
			"- source_candidate: src_cand_external_test",
			"- source_no_promote: nop_external_test",
		},
		"counts_by_decision": map[string]any{
			"keep_needs_review": 1,
			"merge_existing":    1,
		},
		"counts_by_lane": map[string]any{
			"freshness_research_skill_candidate": 1,
			"owned_skill_upstream":               1,
		},
		"decisions": []map[string]any{
			{
				"repo":              "example/high-value",
				"decision":          "keep_needs_review",
				"lane":              "freshness_research_skill_candidate",
				"status":            "recorded",
				"evidence_strength": 0.82,
				"duplicate_risk":    0.2,
				"review_id":         "scr_high",
			},
			{
				"repo":              "example/merge",
				"decision":          "merge_existing",
				"lane":              "owned_skill_upstream",
				"status":            "recorded",
				"evidence_strength": 0.7,
				"duplicate_risk":    0.95,
				"review_id":         "scr_merge",
			},
		},
	})

	report, err := ExternalRepoIntakeReportForHome(home)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || report.BatchCount != 1 || report.CandidateCount != 2 {
		t.Fatalf("report counts = %+v", report)
	}
	if report.SourceObservationID != "srcobs_external_test" {
		t.Fatalf("source observation = %q", report.SourceObservationID)
	}
	if len(report.SourceCandidateIDs) != 1 || report.SourceCandidateIDs[0] != "src_cand_external_test" {
		t.Fatalf("source candidates = %+v", report.SourceCandidateIDs)
	}
	if len(report.NoPromoteIDs) != 1 || report.NoPromoteIDs[0] != "nop_external_test" {
		t.Fatalf("no-promote ids = %+v", report.NoPromoteIDs)
	}
	if report.OpenReviewCount != 1 || report.MergeExistingCount != 1 || report.ReferenceOnlyCount != 0 {
		t.Fatalf("decision counts = %+v", report)
	}
	if len(report.HighValue) != 1 || report.HighValue[0].Repo != "example/high-value" {
		t.Fatalf("high value = %+v", report.HighValue)
	}
	if report.HighValue[0].Owner != "deep-research" || report.HighValue[0].NextReviewStep != "run freshness eval" {
		t.Fatalf("high value metadata = %+v", report.HighValue[0])
	}

	reviewDir := filepath.Join(home, "reviews", ExternalRepoIntakeReviewArea)
	if err := os.MkdirAll(reviewDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reviewedPath := filepath.Join(reviewDir, "example-high-value.reviewed.json")
	writeJSON(t, reviewedPath, ExternalRepoIntakeReviewReport{
		SchemaVersion: ExternalRepoIntakeReviewReportSchemaVersion,
		ID:            "errr_high_value",
		CreatedAt:     "2026-07-02T03:04:07Z",
		Status:        "recorded",
		Repo:          "example/high-value",
		Decision:      "keep_needs_review",
		Owner:         "deep-research",
		NextAction:    "run freshness eval",
		ReviewPath:    reviewedPath,
	})
	afterReview, err := ExternalRepoIntakeReportForHome(home)
	if err != nil {
		t.Fatalf("after-review report: %v", err)
	}
	if afterReview.CountsByDecision["keep_needs_review"] != 1 || afterReview.OpenReviewCount != 0 {
		t.Fatalf("default report should keep raw decision count but clear owner-review debt: counts=%+v open=%d", afterReview.CountsByDecision, afterReview.OpenReviewCount)
	}
	if len(afterReview.HighValue) != 1 || afterReview.HighValue[0].OwnerReviewStatus != "recorded" || afterReview.HighValue[0].CapabilityEvaluationStatus != "pending" || afterReview.HighValue[0].CapabilityGraphStatus != "blocked_on_capability_evaluation" {
		t.Fatalf("default high-value row should expose review/evaluation/graph status: %+v", afterReview.HighValue)
	}
	filtered, err := ExternalRepoIntakeReportForHome(home, "https://github.com/example/high-value.git")
	if err != nil {
		t.Fatalf("filtered report should not fail: %v", err)
	}
	if filtered.RepoQuery == "" || len(filtered.Matches) != 1 || filtered.Matches[0].Repo != "example/high-value" {
		t.Fatalf("filtered matches = %+v", filtered)
	}
	if filtered.Matches[0].NotToDo != "do not auto-run social research" {
		t.Fatalf("filtered metadata missing: %+v", filtered.Matches[0])
	}
	partial, err := ExternalRepoIntakeReportForHome(home, "merge")
	if err != nil {
		t.Fatalf("partial report should not fail: %v", err)
	}
	if len(partial.Matches) != 1 || partial.Matches[0].Repo != "example/merge" {
		t.Fatalf("partial matches = %+v", partial.Matches)
	}
	if len(report.Boundaries) != 2 || report.Boundaries[1] != "no install" {
		t.Fatalf("boundaries = %+v", report.Boundaries)
	}
}

func TestExternalRepoIntakeReportMatchesTaskContextEvidence(t *testing.T) {
	home := t.TempDir()
	batchDir := filepath.Join(home, "inbox", "external-repo-intake", "2026-07-04-context")
	if err := os.MkdirAll(batchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mapPath := filepath.Join(batchDir, "external-repo-intake-map.json")
	writeJSON(t, mapPath, map[string]any{
		"boundaries": []string{"review-only", "no install"},
		"items": []map[string]any{
			{
				"repo":             "gitroomhq/postiz-app",
				"owner":            "product outbound distribute pipe",
				"next_review_step": "evaluate rented posting edge behind PolicyDecision; posting receipts feed product loops",
				"not_to_do":        "do not auth social accounts or auto-post",
			},
			{
				"repo":             "example/memory-importer",
				"owner":            "memory ingestion",
				"next_review_step": "evaluate transcript import edge",
			},
		},
	})
	writeJSON(t, filepath.Join(batchDir, "run-summary.json"), map[string]any{
		"schema_version":          "compound.external_repo_intake_run.v0.1",
		"id":                      "2026-07-04-context",
		"created_at":              "2026-07-04T07:30:00Z",
		"map_path":                mapPath,
		"source_observation_path": filepath.Join(batchDir, "source-observation.json"),
		"candidate_count":         2,
		"counts_by_decision": map[string]any{
			"keep_needs_review": 2,
		},
		"counts_by_lane": map[string]any{
			"distribution_adapter_candidate": 1,
			"memory_source_candidate":        1,
		},
		"decisions": []map[string]any{
			{
				"repo":              "gitroomhq/postiz-app",
				"decision":          "keep_needs_review",
				"lane":              "distribution_adapter_candidate",
				"status":            "recorded",
				"evidence_strength": 0.86,
			},
			{
				"repo":              "example/memory-importer",
				"decision":          "keep_needs_review",
				"lane":              "memory_source_candidate",
				"status":            "recorded",
				"evidence_strength": 0.72,
			},
		},
	})

	report, err := ExternalRepoIntakeReportForHome(home, "evaluate social media distribution outbound posting adapter with Postiz for product feedback loops")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Matches) != 1 {
		t.Fatalf("context query should match one reviewed repo evidence row, got %+v", report.Matches)
	}
	match := report.Matches[0]
	if match.Repo != "gitroomhq/postiz-app" ||
		match.Owner != "product outbound distribute pipe" ||
		match.Lane != "distribution_adapter_candidate" ||
		match.NotToDo != "do not auth social accounts or auto-post" {
		t.Fatalf("context match did not preserve external repo evidence metadata: %+v", match)
	}
}

func TestExternalRepoIntakeReportSourceListCoverage(t *testing.T) {
	home := t.TempDir()
	batchDir := filepath.Join(home, "inbox", "external-repo-intake", "2026-07-01-test")
	if err := os.MkdirAll(batchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mapPath := filepath.Join(batchDir, "external-repo-intake-map.json")
	writeJSON(t, mapPath, map[string]any{
		"items": []map[string]any{
			{
				"repo":             "example/known",
				"owner":            "procedure:skill-ops",
				"next_review_step": "merge deltas only",
				"not_to_do":        "do not install wholesale",
			},
			{
				"repo":             "example/review",
				"owner":            "Agent Workbench / harness adapters",
				"next_review_step": "review harness pattern",
				"not_to_do":        "do not clone automatically",
			},
		},
	})
	writeJSON(t, filepath.Join(batchDir, "run-summary.json"), map[string]any{
		"schema_version":  "compound.external_repo_intake_run.v0.1",
		"id":              "2026-07-01-test",
		"created_at":      "2026-07-01T12:00:00Z",
		"map_path":        mapPath,
		"candidate_count": 1,
		"decisions": []map[string]any{
			{
				"repo":              "example/known",
				"decision":          "merge_existing",
				"lane":              "external_skill_candidate",
				"evidence_strength": 0.8,
			},
			{
				"repo":              "example/review",
				"decision":          "keep_needs_review",
				"lane":              "harness_reference",
				"evidence_strength": 0.9,
			},
		},
		"counts_by_decision": map[string]any{"merge_existing": 1, "keep_needs_review": 1},
		"counts_by_lane":     map[string]any{"external_skill_candidate": 1, "harness_reference": 1},
	})
	sourceListPath := filepath.Join(home, "repos.md")
	if err := os.WriteFile(sourceListPath, []byte("https://github.com/example/known\nhttps://github.com/example/review\nhttps://github.com/example/known\nhttps://github.com/example/unknown\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	evalDir := filepath.Join(home, "inbox", "capability-evaluations")
	if err := os.MkdirAll(evalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(evalDir, "receipt-only-mention.json"), map[string]any{
		"schema_version":    1,
		"id":                "capability-evaluation.external.other.20260701",
		"source_receipt":    "file:///tmp/repos.md#github.com/example/unknown",
		"capability":        "procedure:skill-ops",
		"verdict":           "downgrade_to_index",
		"rubric_normalized": 0.4,
		"signals": map[string]any{
			"external_repo":  "example/other",
			"external_url":   "https://github.com/example/other",
			"existing_owner": "procedure:skill-ops",
		},
	})

	report, err := ExternalRepoIntakeReportForHomeWithSourceList(home, sourceListPath)
	if err != nil {
		t.Fatalf("source list report should not fail: %v", err)
	}
	if report.SourceListPath != sourceListPath || report.SourceListTotal != 3 || report.SourceListKnown != 2 || report.SourceListUnknown != 1 {
		t.Fatalf("source list counts = %+v", report)
	}
	if got := report.SourceListItems[0]; got.Status != "known" || got.Normalized != "example/known" || got.MatchCount != 1 {
		t.Fatalf("known item = %+v", got)
	}
	if report.SourceListByDecision["merge_existing"] != 1 || report.SourceListByDecision["keep_needs_review"] != 1 || report.SourceListByLane["external_skill_candidate"] != 1 || report.SourceListByLane["harness_reference"] != 1 || report.SourceListByOwner["procedure:skill-ops"] != 1 || report.SourceListByOwner["Agent Workbench / harness adapters"] != 1 {
		t.Fatalf("source-list aggregate counts missing: decision=%+v lane=%+v owner=%+v", report.SourceListByDecision, report.SourceListByLane, report.SourceListByOwner)
	}
	if report.SourceListByOwnerReviewStatus["not_required"] != 1 || report.SourceListByOwnerReviewStatus["pending"] != 1 {
		t.Fatalf("source-list owner review status counts missing: %+v", report.SourceListByOwnerReviewStatus)
	}
	if report.SourceListByCapabilityEvaluationStatus["not_required"] != 1 || report.SourceListByCapabilityEvaluationStatus["blocked_on_owner_review"] != 1 {
		t.Fatalf("source-list capability evaluation status counts missing: %+v", report.SourceListByCapabilityEvaluationStatus)
	}
	if len(report.SourceListReviewNext) != 1 || report.SourceListReviewNext[0].Matches[0].Repo != "example/review" {
		t.Fatalf("source-list review_next = %+v", report.SourceListReviewNext)
	}
	if report.SourceListReviewNextCount == nil || *report.SourceListReviewNextCount != 1 || report.SourceListEvalNextCount == nil || *report.SourceListEvalNextCount != 0 {
		t.Fatalf("source-list follow-up counts missing: review=%v eval=%v", report.SourceListReviewNextCount, report.SourceListEvalNextCount)
	}
	if report.SourceListReviewNextMeaning != "owner_review_suggestions_not_open_review_debt" || report.SourceListEvalNextMeaning != "focused_capability_evaluation_follow_up_queue" {
		t.Fatalf("source-list follow-up meanings missing: review=%q eval=%q", report.SourceListReviewNextMeaning, report.SourceListEvalNextMeaning)
	}
	if report.SourceListItems[0].Matches[0].Owner != "procedure:skill-ops" {
		t.Fatalf("known item metadata missing: %+v", report.SourceListItems[0].Matches[0])
	}
	if got := report.SourceListItems[2]; got.Status != "unknown" || got.Normalized != "example/unknown" || got.MatchCount != 0 {
		t.Fatalf("unknown item = %+v", got)
	}
}

func TestExternalRepoIntakeReviewPlanWritesQueueArtifacts(t *testing.T) {
	home := t.TempDir()
	batchDir := filepath.Join(home, "inbox", "external-repo-intake", "2026-07-01-test")
	if err := os.MkdirAll(batchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mapPath := filepath.Join(batchDir, "external-repo-intake-map.json")
	writeJSON(t, mapPath, map[string]any{
		"boundaries": []string{"review-only", "no install"},
		"items": []map[string]any{
			{
				"repo":             "example/review",
				"owner":            "Agent Workbench / harness adapters",
				"next_review_step": "review harness pattern",
				"not_to_do":        "do not clone automatically",
			},
			{
				"repo":             "example/merge",
				"owner":            "procedure:skill-ops",
				"next_review_step": "merge deltas only",
				"not_to_do":        "do not install wholesale",
			},
		},
	})
	writeJSON(t, filepath.Join(batchDir, "run-summary.json"), map[string]any{
		"schema_version":  "compound.external_repo_intake_run.v0.1",
		"id":              "2026-07-01-test",
		"created_at":      "2026-07-01T12:00:00Z",
		"map_path":        mapPath,
		"candidate_count": 2,
		"decisions": []map[string]any{
			{
				"repo":              "example/review",
				"decision":          "keep_needs_review",
				"lane":              "harness_reference",
				"evidence_strength": 0.9,
			},
			{
				"repo":              "example/merge",
				"decision":          "merge_existing",
				"lane":              "external_skill_candidate",
				"evidence_strength": 0.7,
			},
		},
		"counts_by_decision": map[string]any{"keep_needs_review": 1, "merge_existing": 1},
		"counts_by_lane":     map[string]any{"harness_reference": 1, "external_skill_candidate": 1},
	})
	sourceListPath := filepath.Join(home, "repos.md")
	if err := os.WriteFile(sourceListPath, []byte("https://github.com/example/review\nhttps://github.com/example/merge\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := WriteExternalRepoIntakeReviewPlan(home, sourceListPath, time.Date(2026, 7, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatalf("write review plan: %v", err)
	}
	if report.Status != "ok" || report.WroteCount != 1 || len(report.Items) != 1 {
		t.Fatalf("review plan report = %+v", report)
	}
	item := report.Items[0]
	if item.Status != "review_required" || item.Repo != "example/review" || item.Decision != "keep_needs_review" || item.NextAction != "review harness pattern" {
		t.Fatalf("review plan item = %+v", item)
	}
	if len(item.EvaluationPlan) < 4 || item.EvaluationPlan[0].ID != "existing-owner-fit" || item.EvaluationPlan[0].Silence == "" || item.EvaluationPlan[0].Retirement == "" {
		t.Fatalf("review plan should include context-aware evaluation steps: %+v", item.EvaluationPlan)
	}
	if !hasExternalRepoEvaluationStep(item.EvaluationPlan, "harness-fit") {
		t.Fatalf("harness repo should add harness-fit evaluation lens: %+v", item.EvaluationPlan)
	}
	if item.JSONPath == "" || item.MarkdownPath == "" || item.ReviewYAMLPath == "" {
		t.Fatalf("review plan paths missing: %+v", item)
	}
	md, err := os.ReadFile(item.MarkdownPath)
	if err != nil {
		t.Fatalf("read markdown: %v", err)
	}
	if !strings.Contains(string(md), "do not clone automatically") {
		t.Fatalf("markdown missing boundary:\n%s", md)
	}
	for _, want := range []string{
		"## Context-Aware Evaluation Plan",
		"existing-owner-fit (owner-fit)",
		"harness-fit (harness-fit)",
		"silence:",
		"proof:",
		"retirement:",
	} {
		if !strings.Contains(string(md), want) {
			t.Fatalf("markdown missing evaluation plan %q:\n%s", want, md)
		}
	}
	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: ExternalRepoIntakeReviewArea}, time.Date(2026, 7, 2, 3, 4, 6, 0, time.UTC))
	if err != nil {
		t.Fatalf("queue review artifacts: %v", err)
	}
	if queue.Counts.Total != 1 || queue.Counts.ReviewRequired != 1 || queue.Items[0].TargetPath != "github.com/example/review" || queue.Items[0].NextAction != "review harness pattern" || queue.Items[0].GraphEvidenceStrength != 0.9 {
		t.Fatalf("queue did not surface review plan: %+v", queue)
	}
	AnnotateExternalRepoIntakeQueue(&queue)
	if queue.Items[0].Readiness != "needs_review" || len(queue.Items[0].Actions) != 1 || queue.Items[0].Actions[0].ID != "record_external_repo_intake_review" {
		t.Fatalf("queue should expose external repo review action before valid review: %+v", queue.Items[0])
	}
	if queue.Items[0].DecisionCard == nil {
		t.Fatalf("queue should expose context-aware external repo decision card: %+v", queue.Items[0])
	}
	if queue.Items[0].DecisionCard.Meta["evaluation_step_count"] != "4" {
		t.Fatalf("decision card should expose evaluation step count: %+v", queue.Items[0].DecisionCard)
	}
	if !decisionCardHasReasonLabel(queue.Items[0].DecisionCard, "evaluation lens: harness-fit") {
		t.Fatalf("decision card missing harness evaluation lens: %+v", queue.Items[0].DecisionCard)
	}

	reviewDraft, err := os.ReadFile(item.ReviewYAMLPath)
	if err != nil {
		t.Fatalf("read review yaml: %v", err)
	}
	editedReview := strings.ReplaceAll(string(reviewDraft), `rationale: ""`, `rationale: "owner reviewed against current Compound capability boundaries"`)
	if err := os.WriteFile(item.ReviewYAMLPath, []byte(editedReview), 0o644); err != nil {
		t.Fatalf("write review yaml: %v", err)
	}
	review, err := LoadExternalRepoIntakeReview(item.ReviewYAMLPath)
	if err != nil {
		t.Fatalf("load review yaml: %v", err)
	}
	reported, err := RecordExternalRepoIntakeReview(home, review, item.ReviewYAMLPath, time.Date(2026, 7, 2, 3, 4, 7, 0, time.UTC))
	if err != nil {
		t.Fatalf("record external repo review: %v", err)
	}
	if reported.Status != "recorded" || reported.Repo != "example/review" || reported.Decision != "keep_needs_review" || reported.ReviewPath == "" {
		t.Fatalf("review report = %+v", reported)
	}
	if reported.Rationale != "owner reviewed against current Compound capability boundaries" || reported.SourceHash == "" || reported.SourceRef != "external-repo-intake-review:"+filepath.Base(item.ReviewYAMLPath) || reported.ReviewedAt != "2026-07-02T03:04:07Z" || len(reported.Boundaries) == 0 {
		t.Fatalf("review report should preserve reviewed evidence: %+v", reported)
	}
	persisted, err := LoadExternalRepoIntakeReviewReport(reported.ReviewPath)
	if err != nil {
		t.Fatalf("load reviewed report: %v", err)
	}
	if persisted.Rationale != reported.Rationale || persisted.SourceHash != reported.SourceHash || len(persisted.Boundaries) != len(reported.Boundaries) {
		t.Fatalf("persisted reviewed report lost evidence: got=%+v want=%+v", persisted, reported)
	}
	queue, err = reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: ExternalRepoIntakeReviewArea}, time.Date(2026, 7, 2, 3, 4, 8, 0, time.UTC))
	if err != nil {
		t.Fatalf("queue after review: %v", err)
	}
	AnnotateExternalRepoIntakeQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.ReviewRequired != 0 || queue.Counts.OK != 1 || queue.Items[0].Status != "recorded" || queue.Items[0].Readiness != "review_recorded" || len(queue.Items[0].Actions) != 0 {
		t.Fatalf("reviewed external repo queue item should be terminal: counts=%+v item=%+v", queue.Counts, queue.Items[0])
	}
}

func TestExternalRepoCapabilityEvaluationPlanConsumesRecordedReview(t *testing.T) {
	home := t.TempDir()
	batchDir := filepath.Join(home, "inbox", "external-repo-intake", "2026-07-01-test")
	if err := os.MkdirAll(batchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mapPath := filepath.Join(batchDir, "external-repo-intake-map.json")
	writeJSON(t, mapPath, map[string]any{
		"boundaries": []string{"review-only", "no install"},
		"items": []map[string]any{
			{
				"repo":             "example/review",
				"owner":            "Agent Workbench / harness adapters",
				"next_review_step": "evaluate browser QA workflow against Agent Workbench",
				"not_to_do":        "do not clone automatically",
			},
		},
	})
	writeJSON(t, filepath.Join(batchDir, "run-summary.json"), map[string]any{
		"schema_version":  "compound.external_repo_intake_run.v0.1",
		"id":              "2026-07-01-test",
		"created_at":      "2026-07-01T12:00:00Z",
		"map_path":        mapPath,
		"candidate_count": 1,
		"decisions": []map[string]any{
			{
				"repo":              "example/review",
				"decision":          "keep_needs_review",
				"lane":              "harness_reference",
				"evidence_strength": 0.9,
				"duplicate_risk":    0.2,
			},
		},
		"counts_by_decision": map[string]any{"keep_needs_review": 1},
		"counts_by_lane":     map[string]any{"harness_reference": 1},
	})
	sourceListPath := filepath.Join(home, "repos.md")
	if err := os.WriteFile(sourceListPath, []byte("https://github.com/example/review\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reviewDir := filepath.Join(home, "reviews", ExternalRepoIntakeReviewArea)
	if err := os.MkdirAll(reviewDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reviewedPath := filepath.Join(reviewDir, "example-review.reviewed.json")
	writeJSON(t, reviewedPath, ExternalRepoIntakeReviewReport{
		SchemaVersion: ExternalRepoIntakeReviewReportSchemaVersion,
		ID:            "errr_test",
		CreatedAt:     "2026-07-02T03:04:07Z",
		Status:        "recorded",
		PlanID:        "extrepo_test",
		ReviewID:      "err_test",
		Repo:          "example/review",
		Decision:      "keep_needs_review",
		Owner:         "Agent Workbench / harness adapters",
		NextAction:    "evaluate browser QA workflow against Agent Workbench",
		SourcePath:    "external-repo-intake-review.yaml",
		ReviewPath:    reviewedPath,
		EvidenceRefs:  []string{ExternalRepoIntakeAuthorityRef + "/2026-07-01-test#example/review"},
		Boundaries:    []string{"review-only", "no install"},
	})

	report, err := RecordExternalRepoCapabilityEvaluationPlan(home, ExternalRepoCapabilityEvaluationPlanOptions{
		SourceListPath: sourceListPath,
		RepoQuery:      "github.com/example/review",
	}, time.Date(2026, 7, 2, 4, 5, 6, 0, time.UTC))
	if err != nil {
		t.Fatalf("record external repo evaluation plan: %v", err)
	}
	if report.Status != "review_required" || report.Repo != "example/review" || report.OwnerReviewStatus != "recorded" {
		t.Fatalf("report = %+v", report)
	}
	if report.EvaluationPath == "" || report.PlanPath == "" || report.ReviewPath == "" || report.ReviewYAMLPath == "" || report.ManifestDraftPath == "" {
		t.Fatalf("paths missing from report: %+v", report)
	}
	eval, _, err := loadCapabilityEvaluationFile(report.EvaluationPath)
	if err != nil {
		t.Fatalf("load written evaluation: %v", err)
	}
	if eval.Verdict != "evaluate_further" || eval.Capability != "external-repo-example-review-workflow" {
		t.Fatalf("evaluation = %+v", eval)
	}
	if signalString(eval.Signals, "external_repo") != "example/review" || signalString(eval.Signals, "existing_owner") != "Agent Workbench / harness adapters" {
		t.Fatalf("evaluation signals = %+v", eval.Signals)
	}
	lenses := signalStringSlice(eval.Signals, "context_evaluation_lenses")
	if !contains(lenses, "owner-fit") || !contains(lenses, "harness-fit") {
		t.Fatalf("evaluation should carry context-aware lenses, got signals=%+v lenses=%+v", eval.Signals, lenses)
	}
	if _, err := os.Stat(filepath.Join(home, "registries", "capability-nodes.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("evaluation plan must not mutate capability graph, stat err=%v", err)
	}
	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: capabilityEvaluationReviewArea}, time.Date(2026, 7, 2, 4, 5, 7, 0, time.UTC))
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	AnnotateCapabilityEvaluationQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.ReviewRequired != 1 || queue.Items[0].Status != "review_required" {
		t.Fatalf("capability evaluation queue = counts=%+v item=%+v", queue.Counts, queue.Items[0])
	}
	if queue.Items[0].DecisionCard == nil || !decisionCardHasReasonLabel(queue.Items[0].DecisionCard, "evaluation lens: harness-fit") {
		t.Fatalf("capability evaluation card should carry context lens details: %+v", queue.Items[0].DecisionCard)
	}
	pending, err := ExternalRepoIntakeReportForHomeWithSourceList(home, sourceListPath)
	if err != nil {
		t.Fatalf("refresh source-list report with pending evaluation review: %v", err)
	}
	if len(pending.SourceListEvalNext) != 0 {
		t.Fatalf("pending capability evaluation review should leave evaluation-next: %+v", pending.SourceListEvalNext)
	}
	if pending.SourceListByCapabilityEvaluationStatus["review_required"] != 1 || pending.SourceListByCapabilityGraphStatus["blocked_on_evaluation_review"] != 1 {
		t.Fatalf("pending evaluation review status missing: eval=%+v graph=%+v", pending.SourceListByCapabilityEvaluationStatus, pending.SourceListByCapabilityGraphStatus)
	}
	pendingMatch := pending.SourceListItems[0].Matches[0]
	if pendingMatch.CapabilityEvaluationStatus != "review_required" || pendingMatch.CapabilityEvaluationPath != report.ReviewYAMLPath || pendingMatch.CapabilityEvaluationNodeStatus != "review_required" {
		t.Fatalf("pending evaluation context missing: %+v", pendingMatch)
	}
	var plan CapabilityEvaluationPlan
	planData, err := os.ReadFile(report.PlanPath)
	if err != nil {
		t.Fatalf("read evaluation plan: %v", err)
	}
	if err := json.Unmarshal(planData, &plan); err != nil {
		t.Fatalf("parse evaluation plan: %v", err)
	}
	if !contains(plan.RequiredReview, "context lens harness-fit proof and silence condition reviewed") {
		t.Fatalf("capability evaluation plan should require context-lens review before graph import: %+v", plan.RequiredReview)
	}
	reviewReport, err := RecordCapabilityEvaluationReview(home, CapabilityEvaluationReview{
		SchemaVersion: CapabilityEvaluationReviewSchemaVersion,
		ReviewID:      "review_" + report.PlanID,
		PlanID:        report.PlanID,
		EvaluationID:  plan.EvaluationID,
		Capability:    plan.Capability,
		Decision:      "keep_needs_review",
		Rationale:     "reviewed against existing owner; keep as evidence only",
		Confidence:    0.82,
		NodeDraft:     plan.NodeDraft,
		EvidenceRefs:  plan.EvidenceRefs,
	}, report.ReviewYAMLPath, time.Date(2026, 7, 2, 4, 5, 8, 0, time.UTC))
	if err != nil {
		t.Fatalf("record evaluation review: %v", err)
	}
	refreshed, err := ExternalRepoIntakeReportForHomeWithSourceList(home, sourceListPath)
	if err != nil {
		t.Fatalf("refresh source-list report: %v", err)
	}
	if len(refreshed.SourceListEvalNext) != 0 {
		t.Fatalf("reviewed capability evaluation should leave evaluation-next: %+v", refreshed.SourceListEvalNext)
	}
	if len(refreshed.SourceListItems) != 1 || len(refreshed.SourceListItems[0].Matches) != 1 {
		t.Fatalf("refreshed source-list item missing match: %+v", refreshed.SourceListItems)
	}
	match := refreshed.SourceListItems[0].Matches[0]
	if match.CapabilityEvaluationStatus != "recorded" || match.CapabilityEvaluationPath != reviewReport.ReviewedPath || match.CapabilityEvaluationDecision != "keep_needs_review" || match.CapabilityEvaluationNodeStatus != "needs_review" {
		t.Fatalf("capability evaluation review context missing: match=%+v review=%+v", match, reviewReport)
	}
	if refreshed.SourceListByOwnerReviewStatus["recorded"] != 1 || refreshed.SourceListByCapabilityEvaluationStatus["recorded"] != 1 {
		t.Fatalf("reviewed source-list status counts missing: owner=%+v eval=%+v", refreshed.SourceListByOwnerReviewStatus, refreshed.SourceListByCapabilityEvaluationStatus)
	}
	if refreshed.SourceListByCapabilityGraphStatus["reviewed_manifest_pending_import"] != 1 {
		t.Fatalf("reviewed manifest graph-import status missing: %+v", refreshed.SourceListByCapabilityGraphStatus)
	}
	if len(refreshed.SourceListGraphImportNext) != 1 || refreshed.SourceListGraphImportNext[0].Matches[0].CapabilityGraphStatus != "reviewed_manifest_pending_import" {
		t.Fatalf("graph import follow-up missing: %+v", refreshed.SourceListGraphImportNext)
	}
	if refreshed.SourceListGraphImportNextCount == nil || *refreshed.SourceListGraphImportNextCount != 1 || refreshed.SourceListGraphImportNextOmitted != 0 || refreshed.SourceListGraphImportNextMeaning != "reviewed_manifest_import_queue_not_graph_mutation" {
		t.Fatalf("graph import follow-up count/meaning missing: count=%v omitted=%d meaning=%q", refreshed.SourceListGraphImportNextCount, refreshed.SourceListGraphImportNextOmitted, refreshed.SourceListGraphImportNextMeaning)
	}

	importReport, err := ImportExternalRepoReviewedManifests(home, sourceListPath, 0, time.Date(2026, 7, 2, 4, 5, 9, 0, time.UTC))
	if err != nil {
		t.Fatalf("import reviewed manifests: %v", err)
	}
	if importReport.Status != "ok" || importReport.PendingImportCount != 1 || importReport.ImportedCount != 1 || len(importReport.Items) != 1 {
		t.Fatalf("import report = %+v", importReport)
	}
	if importReport.Items[0].Repo != "example/review" || importReport.Items[0].NodeID != "procedure:external-repo-example-review-workflow" || importReport.Items[0].NodeStatus != "needs_review" {
		t.Fatalf("import item missing graph context: %+v", importReport.Items[0])
	}
	importedRefresh, err := ExternalRepoIntakeReportForHomeWithSourceList(home, sourceListPath)
	if err != nil {
		t.Fatalf("refresh source-list report after manifest import: %v", err)
	}
	if importedRefresh.SourceListByCapabilityGraphStatus["imported"] != 1 || len(importedRefresh.SourceListGraphImportNext) != 0 {
		t.Fatalf("imported manifest should leave no graph-import follow-up: status=%+v next=%+v", importedRefresh.SourceListByCapabilityGraphStatus, importedRefresh.SourceListGraphImportNext)
	}
	store, err := Open(home)
	if err != nil {
		t.Fatalf("open graph: %v", err)
	}
	surfaced, err := store.Surface("example review workflow")
	if err != nil {
		t.Fatalf("surface imported review workflow: %v", err)
	}
	if hasSurfaced(surfaced, "procedure:external-repo-example-review-workflow") {
		t.Fatalf("needs_review imported manifest must not surface: %+v", surfaced)
	}

	archiveReviewAreaFilesForTest(t, home, capabilityEvaluationReviewArea, time.Date(2026, 7, 2, 4, 6, 0, 0, time.UTC))
	archivedRefresh, err := ExternalRepoIntakeReportForHomeWithSourceList(home, sourceListPath)
	if err != nil {
		t.Fatalf("refresh source-list report after archive: %v", err)
	}
	if len(archivedRefresh.SourceListEvalNext) != 0 {
		t.Fatalf("archived capability evaluation review should leave evaluation-next: %+v", archivedRefresh.SourceListEvalNext)
	}
	archivedMatch := archivedRefresh.SourceListItems[0].Matches[0]
	if archivedMatch.CapabilityEvaluationStatus != "recorded" || archivedMatch.CapabilityEvaluationPath == "" || !strings.Contains(archivedMatch.CapabilityEvaluationPath, "archives/operational/files/reviews/capability-evaluation-intake/") {
		t.Fatalf("archived capability evaluation review context missing: %+v", archivedMatch)
	}
}

func TestExternalRepoIntakeReportIncludesMatchingEvaluationReferences(t *testing.T) {
	home := t.TempDir()
	batchDir := filepath.Join(home, "inbox", "external-repo-intake", "2026-07-01-test")
	if err := os.MkdirAll(batchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mapPath := filepath.Join(batchDir, "external-repo-intake-map.json")
	writeJSON(t, mapPath, map[string]any{
		"items": []map[string]any{
			{"repo": "example/repo"},
		},
	})
	writeJSON(t, filepath.Join(batchDir, "run-summary.json"), map[string]any{
		"schema_version":  "compound.external_repo_intake_run.v0.1",
		"id":              "2026-07-01-test",
		"created_at":      "2026-07-01T12:00:00Z",
		"map_path":        mapPath,
		"candidate_count": 1,
		"decisions": []map[string]any{
			{
				"repo":              "example/repo",
				"decision":          "merge_existing",
				"lane":              "external_skill_candidate",
				"evidence_strength": 0.6,
			},
		},
		"counts_by_decision": map[string]any{"merge_existing": 1},
		"counts_by_lane":     map[string]any{"external_skill_candidate": 1},
	})

	evalDir := filepath.Join(home, "inbox", "capability-evaluations")
	if err := os.MkdirAll(evalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(evalDir, "karpathy-gist.json"), map[string]any{
		"schema_version":    1,
		"id":                "capability-evaluation.external.karpathy.gist.20260701",
		"created_at":        "2026-07-01T15:14:30Z",
		"source_receipt":    "file:///tmp/repos.md#gist.github.com/karpathy/442a6bf555914893e9891c11519de94f",
		"capability":        "procedure:skill-ops",
		"verdict":           "downgrade_to_index",
		"local_state_only":  true,
		"rubric_normalized": 0.38,
		"signals": map[string]any{
			"kind":                "external_reference_only_source",
			"cluster":             "skill-source-intake",
			"external_repo":       "gist:karpathy/442a6bf555914893e9891c11519de94f",
			"external_url":        "https://gist.github.com/karpathy/442a6bf555914893e9891c11519de94f",
			"existing_owner":      "procedure:skill-ops",
			"required_next_proof": "revisit only if a concrete task cites the gist",
			"main_risks":          "do not treat gist notes as installable repos",
			"distinctive_claim":   "reference-only source",
			"owned_gap_observed":  "no runtime adoption gap",
			"comparison_scope":    "Desktop source list coverage",
		},
	})

	writeJSON(t, filepath.Join(evalDir, "skills-sh-marketplace.json"), map[string]any{
		"schema_version":    1,
		"id":                "capability-evaluation.external.skills-sh-marketplace.20260701",
		"created_at":        "2026-07-01T15:41:40Z",
		"source_receipt":    "compound://reviews/external-repo-intake/2026-07-01-external-skill-repo-map#skills-sh-marketplace",
		"capability":        "procedure:skill-ops",
		"verdict":           "downgrade_to_index",
		"local_state_only":  true,
		"rubric_normalized": 0.66,
		"signals": map[string]any{
			"kind":                "external_catalog_reference",
			"cluster":             "skill-catalog-source-discovery",
			"external_repo":       "skills.sh marketplace",
			"external_url":        "https://www.skills.sh",
			"existing_owner":      "procedure:skill-ops",
			"required_next_proof": "review child skills individually through capability intake",
			"main_risks":          "do not bulk install marketplace skills",
			"distinctive_claim":   "marketplace catalog source, not runtime authority",
			"owned_gap_observed":  "query coverage for marketplace source itself",
			"comparison_scope":    "Marketplace catalog lookup and child-skill candidate triage",
		},
	})

	report, err := ExternalRepoIntakeReportForHome(home, "gist.github.com/karpathy/442a6bf555914893e9891c11519de94f")
	if err != nil {
		t.Fatalf("report should include evaluation references: %v", err)
	}
	if len(report.Matches) != 1 {
		t.Fatalf("matches = %+v", report.Matches)
	}
	match := report.Matches[0]
	if match.Repo != "gist:karpathy/442a6bf555914893e9891c11519de94f" {
		t.Fatalf("repo = %q", match.Repo)
	}
	if match.Decision != "reference_only" || match.Lane != "skill-source-intake" {
		t.Fatalf("decision/lane = %+v", match)
	}
	if match.Owner != "procedure:skill-ops" || match.GraphHint != "downgrade_to_index" {
		t.Fatalf("owner/hint = %+v", match)
	}
	if match.EvidenceStrength != 0.38 {
		t.Fatalf("evidence strength = %.2f", match.EvidenceStrength)
	}

	repoReport, err := ExternalRepoIntakeReportForHome(home, "example/repo")
	if err != nil {
		t.Fatalf("repo report should still work: %v", err)
	}
	if len(repoReport.Matches) != 1 || repoReport.Matches[0].Repo != "example/repo" {
		t.Fatalf("repo matches = %+v", repoReport.Matches)
	}
	marketplaceReport, err := ExternalRepoIntakeReportForHome(home, "skills.sh")
	if err != nil {
		t.Fatalf("marketplace report should not fail: %v", err)
	}
	if len(marketplaceReport.Matches) != 1 || marketplaceReport.Matches[0].Repo != "skills.sh marketplace" {
		t.Fatalf("marketplace matches = %+v", marketplaceReport.Matches)
	}
	if marketplaceReport.Matches[0].Decision != "reference_only" || marketplaceReport.Matches[0].GraphHint != "downgrade_to_index" {
		t.Fatalf("marketplace decision should stay index-only: %+v", marketplaceReport.Matches[0])
	}
}

func hasExternalRepoEvaluationStep(steps []ExternalRepoContextEvaluationStep, id string) bool {
	for _, step := range steps {
		if step.ID == id {
			return true
		}
	}
	return false
}

func decisionCardHasReasonLabel(card *reviewartifact.DecisionCard, label string) bool {
	if card == nil {
		return false
	}
	for _, reason := range card.Why {
		if reason.Label == label {
			return true
		}
	}
	return false
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
func archiveReviewAreaFilesForTest(t *testing.T, homePath, area string, now time.Time) {
	t.Helper()
	dir := filepath.Join(homePath, "reviews", area)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var indexLines []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		rel := filepath.ToSlash(filepath.Join("reviews", area, entry.Name()))
		archiveRel := filepath.ToSlash(filepath.Join(homepkg.OperationalArchiveFilesRel, rel))
		src := filepath.Join(homePath, filepath.FromSlash(rel))
		dst := filepath.Join(homePath, filepath.FromSlash(archiveRel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(src, dst); err != nil {
			t.Fatal(err)
		}
		record := homepkg.OperationalArchiveEntry{
			SchemaVersion: homepkg.OperationalArchiveSchemaVersion,
			ID:            reviewartifact.ID("arch_", rel),
			Operation:     "archived",
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Kind:          "review",
			Status:        "recorded",
			OriginalPath:  rel,
			ArchivePath:   archiveRel,
		}
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		indexLines = append(indexLines, string(data))
	}
	if len(indexLines) == 0 {
		t.Fatalf("no files archived for area %s", area)
	}
	indexPath := homepkg.OperationalArchiveIndexPath(homePath)
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, []byte(strings.Join(indexLines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
