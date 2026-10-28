package work

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewCardWorkPlanRecordsASCOperatorGateProposalOnly(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	source, err := s.Create(CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "held app-store-connect ASC daily signals need private config before scheduler",
		State:            "held",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	cardID := "operator-gate-" + source.ID + "-app-store-connect-private-config"
	report, err := s.BuildReviewCardWorkPlan(cardID, fixedNow, ReviewCardWorkPlanOptions{Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != ReviewCardWorkPlanReportSchemaVersion ||
		report.Status != "recorded" ||
		report.ReviewCard.CardID != cardID ||
		report.ReviewCard.CardKind != "source_private_config_gate" ||
		report.ReviewCard.GateID != "asc-private-config" ||
		report.ReviewCard.SourceKind != "app_store_connect" ||
		!contains(report.ReviewCard.ActionCommand, "tools/asc_preflight.ts") ||
		!contains(report.ReviewCard.StopConditions, "requested action would fetch live ASC data, apply ProductSignal, load scheduler, or clear held work") {
		t.Fatalf("review-card work plan report did not preserve ASC gate: %+v", report)
	}
	if report.WorkPlan.PlanPath == "" {
		t.Fatalf("review-card work plan not recorded: %+v", report.WorkPlan)
	}
	if _, err := os.Stat(report.WorkPlan.PlanPath); err != nil {
		t.Fatalf("recorded create-plan missing: %v", err)
	}
	if _, err := s.Get(report.WorkPlan.WorkID); err == nil {
		t.Fatalf("review-card work-plan should not admit work %s", report.WorkPlan.WorkID)
	}
	stillHeld, err := s.Get(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillHeld.State != "held" {
		t.Fatalf("source held work should remain held, got %s", stillHeld.State)
	}
	ctx := report.WorkPlan.ActionContext
	if ctx == nil ||
		ctx.Kind != "workbench_review_card" ||
		ctx.SourceID != cardID ||
		ctx.SourceRef != "compound://workbench/review-cards/"+cardID ||
		ctx.ActionSummary == "" ||
		!contains(ctx.Command, "tools/asc_preflight.ts") ||
		!contains(ctx.RequiredInputs, "private ASC product mapping config") ||
		!contains(ctx.ExpectedValidation, "no product signal apply and no scheduler load") ||
		!contains(ctx.StopConditions, "requested action would fetch live ASC data, apply ProductSignal, load scheduler, or clear held work") ||
		!strings.Contains(ctx.Boundary, "read-only ASC preflight") ||
		!strings.Contains(ctx.Proof, "ASC private-config gate") {
		t.Fatalf("create-plan action context did not preserve review-card gates: %+v", ctx)
	}
	wb, err := s.Workbench(fixedNow, WorkbenchOptions{RecentLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if wb.Counts.AdmissionPreviews != 1 || len(wb.AdmissionPreviews) != 1 {
		t.Fatalf("workbench should expose review-card admission preview: counts=%+v previews=%+v", wb.Counts, wb.AdmissionPreviews)
	}
	preview := wb.AdmissionPreviews[0]
	if preview.WorkID != report.WorkPlan.WorkID ||
		preview.ActionContext == nil ||
		preview.ActionContext.Kind != "workbench_review_card" ||
		!preview.ActionContext.RequiresOperator ||
		!sameStringSlice(preview.ActionContext.Command, report.ReviewCard.ActionCommand) ||
		preview.ActionSummary != report.ReviewCard.CardTitle ||
		!preview.RequiresOperator ||
		preview.AdmissionBlockedReason == "" ||
		len(preview.AdmitCommand) != 0 {
		t.Fatalf("admission preview should preserve review-card action context: %+v", preview)
	}
	admitted, err := s.AdmitCreatePlanPath(report.WorkPlan.PlanPath, fixedNow, AdmitCreatePlanOptions{Actor: "operator-test"})
	if err != nil {
		t.Fatal(err)
	}
	if admitted.Status != "admitted" ||
		admitted.ActionContext == nil ||
		admitted.ActionContext.SourceID != cardID ||
		!sameStringSlice(admitted.ActionContext.Command, report.ReviewCard.ActionCommand) {
		t.Fatalf("admit-plan did not preserve review-card action context: %+v", admitted)
	}
	afterSource, err := s.Get(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterSource.State != "held" {
		t.Fatalf("admitting review-card proposal must not clear source held work: %s", afterSource.State)
	}
}

func TestReviewCardWorkPlanRecordsMemoryCandidateCardWithoutPromotion(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	source, err := s.Create(CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "held conversation memory review has existing run artifacts",
		State:            "held",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(s.home, "runs", source.ID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	candidateDir := filepath.Join(s.home, "inbox", "memory-candidates")
	if err := os.MkdirAll(candidateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(candidateDir, "cand_reviewed_conversation.json"), map[string]any{
		"id":     "cand_reviewed_conversation",
		"state":  "parked",
		"entity": "knowledge:chatgpt:fixture",
	})
	writeJSONFile(t, filepath.Join(runDir, "panel-run-single-prun_fixture.json"), map[string]any{
		"schema_version": "compound.panel_run.v0.1",
		"id":             "prun_fixture",
		"status":         "applied",
		"target_kind":    "memory_candidate",
		"targets": []map[string]any{{
			"ref":          "cand_reviewed_conversation",
			"privacy":      "private",
			"state_before": "proposed",
		}},
	})
	writeJSONFile(t, filepath.Join(runDir, "promoted-chatgpt-claims.json"), map[string]any{
		"count": 0,
		"refs":  []string{},
	})

	cardID := "memory-review-" + source.ID + "-cand_reviewed_conversation"
	report, err := s.BuildReviewCardWorkPlan(cardID, fixedNow, ReviewCardWorkPlanOptions{Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if report.ReviewCard.CardKind != "memory_candidate_adjudication" ||
		report.ReviewCard.CandidateID != "cand_reviewed_conversation" ||
		report.ReviewCard.PanelRunID != "prun_fixture" ||
		report.ReviewCard.PromotionSummaryRef == "" ||
		!contains(report.ReviewCard.ExpectedValidation, "promotion summary remains zero before explicit promotion") ||
		!contains(report.ReviewCard.StopConditions, "requested action would run import, extraction, panel run, mass import, or automatic promotion") {
		t.Fatalf("review-card work plan report did not preserve memory card: %+v", report)
	}
	ctx := report.WorkPlan.ActionContext
	if ctx == nil ||
		ctx.Kind != "workbench_review_card" ||
		ctx.SourceID != cardID ||
		!contains(ctx.Command, "prun_fixture") ||
		!contains(ctx.EvidenceRefs, "memory_candidate:cand_reviewed_conversation") ||
		!contains(ctx.EvidenceRefs, "panel_run:prun_fixture") ||
		!strings.Contains(ctx.Boundary, "no automatic memory promotion") {
		t.Fatalf("create-plan action context did not preserve memory card evidence/boundary: %+v", ctx)
	}
	if _, err := s.Get(report.WorkPlan.WorkID); err == nil {
		t.Fatalf("review-card work-plan should not admit work %s", report.WorkPlan.WorkID)
	}
	stillHeld, err := s.Get(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillHeld.State != "held" {
		t.Fatalf("source held work should remain held, got %s", stillHeld.State)
	}
}
