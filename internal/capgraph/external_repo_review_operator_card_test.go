package capgraph

import (
	"testing"
	"time"

	"compound/internal/reviewartifact"
)

// TestAnnotateExternalRepoIntakeQueueFillsFallbackAndFilesRecorded
// reproduces the two live gaps together: (1) a legacy plan artifact written
// before card injection existed (no operator_summary on disk) must get the
// §4 template sentence, and (2) once a matching .reviewed.json sidecar
// exists, the item must file — not sit in needs_you forever.
func TestAnnotateExternalRepoIntakeQueueFillsFallbackAndFilesRecorded(t *testing.T) {
	homePath := t.TempDir()
	plan := ExternalRepoIntakeReviewPlanItem{
		ID:              "extrepo_test",
		Status:          "review_required",
		CreatedAt:       "2026-07-02T03:18:13Z",
		SourceKind:      "external-repo-source-list",
		SourceListQuery: "https://github.com/mksglu/context-mode",
		NormalizedRepo:  "mksglu/context-mode",
		Repo:            "mksglu/context-mode",
		Decision:        "keep_needs_review",
		Owner:           "Agent Workbench / harness adapters",
		EvidenceRefs:    []string{"compound://reviews/external-repo-intake"},
	}
	// Write the raw plan JSON directly (bypassing reviewartifact.Write's
	// automatic ReviewOperatorCard() injection) so it matches the legacy
	// on-disk shape that predates the card mechanism.
	if _, err := reviewartifact.WriteJSON(homePath, ExternalRepoIntakeReviewArea, plan.ID+".json", plan, "  "); err != nil {
		t.Fatal(err)
	}

	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: ExternalRepoIntakeReviewArea}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Items[0].SummarySource != reviewartifact.SummarySourceFallback {
		t.Fatalf("precondition: legacy plan should start as fallback, got %+v", report.Items[0])
	}
	AnnotateExternalRepoIntakeQueue(&report)
	reviewartifact.Recount(&report)

	wantOpen := "Repo intake kept mksglu/context-mode reference evidence for Agent Workbench / harness adapters — nothing installed or run. File it?"
	if report.Items[0].OperatorSummary != wantOpen {
		t.Fatalf("open item OperatorSummary = %q, want %q", report.Items[0].OperatorSummary, wantOpen)
	}
	if report.Items[0].Band != reviewartifact.BandNeedsYou {
		t.Fatalf("open item Band = %q, want needs_you", report.Items[0].Band)
	}

	// Now record a matching review sidecar (mirrors RecordExternalRepoIntakeReview's output shape).
	reviewReport := ExternalRepoIntakeReviewReport{
		SchemaVersion: ExternalRepoIntakeReviewReportSchemaVersion,
		ID:            "errr_test",
		CreatedAt:     "2026-07-02T03:04:07Z",
		Status:        "recorded",
		PlanID:        plan.ID,
		ReviewID:      "review_test",
		Repo:          plan.Repo,
		Decision:      plan.Decision,
		NextAction:    "keep as reference evidence",
	}
	if _, err := reviewartifact.WriteJSON(homePath, ExternalRepoIntakeReviewArea, plan.ID+".review_test.reviewed.json", reviewReport, " "); err != nil {
		t.Fatal(err)
	}

	report, err = reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: ExternalRepoIntakeReviewArea}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	AnnotateExternalRepoIntakeQueue(&report)
	reviewartifact.Recount(&report)

	item := report.Items[0]
	if item.Status != "recorded" {
		t.Fatalf("Status = %q, want recorded", item.Status)
	}
	if item.Band != reviewartifact.BandFiled {
		t.Fatalf("recorded item Band = %q, want filed (all 28 live items were wrongly needs_you before this fix)", item.Band)
	}
	if item.OperatorSummary != wantOpen {
		t.Fatalf("recorded item OperatorSummary = %q, want %q (same §4 sentence, now filed)", item.OperatorSummary, wantOpen)
	}

	needsYou := 0
	for _, it := range report.Items {
		if it.Band == reviewartifact.BandNeedsYou {
			needsYou++
		}
	}
	if needsYou != 0 {
		t.Fatalf("needs_you count for a fixture home with one recorded repo-intake = %d, want 0", needsYou)
	}
}

// TestExternalRepoIntakeGroupSummaryUsesDedicatedTemplate pins the §4 group
// line for external-repo-intake: grouping several open repos must not just
// repeat the first item's singular sentence.
func TestExternalRepoIntakeGroupSummaryUsesDedicatedTemplate(t *testing.T) {
	homePath := t.TempDir()
	for i, repo := range []string{"example/one", "example/two"} {
		plan := ExternalRepoIntakeReviewPlanItem{
			ID:         "extrepo_group_" + repo[len(repo)-1:],
			Status:     "review_required",
			CreatedAt:  "2026-07-02T03:18:13Z",
			Repo:       repo,
			Decision:   "keep_needs_review",
			Owner:      "Agent Workbench",
			SourceKind: "external-repo-source-list",
		}
		_ = i
		if _, err := reviewartifact.WriteJSON(homePath, ExternalRepoIntakeReviewArea, plan.ID+".json", plan, "  "); err != nil {
			t.Fatal(err)
		}
	}
	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: ExternalRepoIntakeReviewArea}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	AnnotateExternalRepoIntakeQueue(&report)
	reviewartifact.Recount(&report)
	if len(report.Groups) != 1 {
		t.Fatalf("want 1 group (same decision+date group_key), got %d: %+v", len(report.Groups), report.Groups)
	}
	group := report.Groups[0]
	if group.Count != 2 {
		t.Fatalf("group count = %d, want 2", group.Count)
	}
	want := "Repo intake reviewed 2 external repos; all kept as read-only evidence. File them?"
	if group.OperatorSummary != want {
		t.Fatalf("group OperatorSummary = %q, want %q", group.OperatorSummary, want)
	}
	if group.OperatorUnlock != "they become citable evidence, or clear out honestly" {
		t.Fatalf("group OperatorUnlock = %q", group.OperatorUnlock)
	}
}
