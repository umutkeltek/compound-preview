package panel

import (
	"strings"
	"testing"

	"compound/internal/reviewartifact"
)

func TestRecordVerdictHumanReviewRecordsNeedsHuman(t *testing.T) {
	home := t.TempDir()
	path := writeVerdictReviewRecordFixture(t, home, "panel_verdict_human_guard", "needs_human")

	report, err := RecordVerdictHumanReview(home, path, VerdictHumanReviewInput{
		Rationale:  "owner reviewed panel request; keep parked manual evidence",
		ReviewedBy: "codex",
	}, fixedPanelNow())
	if err != nil {
		t.Fatalf("record human review: %v", err)
	}
	if report.Status != "recorded" || report.Decision != "needs_human" || report.ReviewPath == "" {
		t.Fatalf("report = %+v, want recorded needs_human review path", report)
	}
	if report.ReviewedBy != "codex" {
		t.Fatalf("reviewed_by = %q, want codex", report.ReviewedBy)
	}
	if !strings.HasSuffix(report.ReviewPath, "panel_verdict_human_guard.reviewed.json") {
		t.Fatalf("review path = %q, want verdict id reviewed sidecar", report.ReviewPath)
	}
	if len(report.Boundaries) == 0 || !strings.Contains(strings.Join(report.Boundaries, " "), "no panel apply") {
		t.Fatalf("report should preserve no-mutation boundaries: %+v", report)
	}
	loaded, err := LoadVerdictHumanReviewReport(report.ReviewPath)
	if err != nil {
		t.Fatalf("load human review report: %v", err)
	}
	if loaded.ID != report.ID || loaded.ReviewPath != report.ReviewPath || loaded.ReviewedBy != "codex" {
		t.Fatalf("loaded report = %+v, want %+v", loaded, report)
	}
}

func TestRecordVerdictHumanReviewRejectsMutatingDecision(t *testing.T) {
	home := t.TempDir()
	path := writeVerdictReviewRecordFixture(t, home, "panel_verdict_promote_guard", "promote")

	_, err := RecordVerdictHumanReview(home, path, VerdictHumanReviewInput{
		Rationale:  "recording promote through human review must fail",
		ReviewedBy: "codex",
	}, fixedPanelNow())
	if err == nil || !strings.Contains(err.Error(), "separate owner mutation path") {
		t.Fatalf("err = %v, want separate owner mutation path rejection", err)
	}
}

func TestRecordVerdictHumanReviewRequiresReviewedBy(t *testing.T) {
	home := t.TempDir()
	path := writeVerdictReviewRecordFixture(t, home, "panel_verdict_reviewer_guard", "needs_human")

	_, err := RecordVerdictHumanReview(home, path, VerdictHumanReviewInput{
		Rationale: "owner reviewed panel request",
	}, fixedPanelNow())
	if err == nil || !strings.Contains(err.Error(), "reviewed_by required") {
		t.Fatalf("expected reviewed_by required error, got %v", err)
	}
}

func writeVerdictReviewRecordFixture(t *testing.T, home, id, decision string) string {
	t.Helper()
	paths, err := reviewartifact.Write(home, verdictReviewArea, id, map[string]any{
		"schema_version": "compound.panel_verdict.v0.1",
		"id":             id,
		"run_id":         "prun_review_record",
		"target": map[string]any{
			"kind": "memory_candidate",
			"ref":  "cand_review_record",
		},
		"synthesis": map[string]any{
			"schema_version": "compound.panel_synthesis.v0.1",
			"decision":       decision,
			"confidence":     0.5,
			"rationale":      "fixture",
		},
		"evidence_refs": []string{"route_fixture"},
	}, " ", "# Panel verdict\n")
	if err != nil {
		t.Fatal(err)
	}
	return paths.PlanPath
}
