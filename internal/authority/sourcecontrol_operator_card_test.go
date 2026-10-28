package authority

import (
	"path/filepath"
	"testing"
	"time"

	"compound/internal/reviewartifact"
)

// TestAnnotateSourceControlCleanupQueueFillsFallback covers gap #1's
// source-control-cleanup area: AnnotateSourceControlCleanupQueue tracked
// readiness/actions but never computed an operator card, so every unannotated
// plan rendered a generic fallback sentence.
func TestAnnotateSourceControlCleanupQueueFillsFallback(t *testing.T) {
	homePath := t.TempDir()
	repo := initGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "changed\n")
	snapshot, err := ReadGitSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildSourceControlReviewPlan(snapshot, "work_operator_card", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reviewartifact.WriteJSON(homePath, SourceControlCleanupReviewArea, plan.ID+".json", plan, "  "); err != nil {
		t.Fatal(err)
	}
	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: SourceControlCleanupReviewArea}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Items[0].SummarySource != reviewartifact.SummarySourceFallback {
		t.Fatalf("precondition: unannotated plan should start fallback, got %+v", report.Items[0])
	}

	AnnotateSourceControlCleanupQueue(&report)
	reviewartifact.Recount(&report)

	item := report.Items[0]
	card, err := OperatorCardForSourceControlReviewPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if item.OperatorSummary != card.Summary {
		t.Fatalf("OperatorSummary = %q, want %q", item.OperatorSummary, card.Summary)
	}
	if item.Band != reviewartifact.BandNeedsYou {
		t.Fatalf("Band = %q, want %q", item.Band, reviewartifact.BandNeedsYou)
	}
}

// TestOperatorCardForSourceControlReviewPlanStripsRepoPrefix pins the real
// live bug: repo_id carries a "repo:" machine prefix on disk (e.g.
// "repo:compound"), which must not leak into the sentence or context_label.
func TestOperatorCardForSourceControlReviewPlanStripsRepoPrefix(t *testing.T) {
	card, err := OperatorCardForSourceControlReviewPlan(SourceControlReviewPlan{
		RepoID:       "repo:compound",
		Upstream:     "origin/main",
		Ahead:        17,
		ChangedPaths: 95,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "Repo watch sees compound has 95 uncommitted files and 17 commits ahead of origin/main. Tidy it?"
	if card.Summary != want {
		t.Fatalf("Summary = %q, want %q", card.Summary, want)
	}
	if card.ContextLabel != "compound" {
		t.Fatalf("ContextLabel = %q, want %q", card.ContextLabel, "compound")
	}
}
