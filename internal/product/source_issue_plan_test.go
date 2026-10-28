package product

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/mem"
)

func TestSourceIssuePlanRecordsReviewArtifactsWithoutClosingIssue(t *testing.T) {
	home := t.TempDir()
	doc, err := LoadChangeStreamCandidates("../../fixtures/product/change-stream-candidates.md")
	if err != nil {
		t.Fatal(err)
	}
	report, err := ApplyChangeStreamCandidates(home, doc, tProduct)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.IssueIDs) == 0 {
		t.Fatalf("change stream report has no issues: %+v", report)
	}
	targetPath := filepath.Join(t.TempDir(), "product-review.md")
	original := []byte("# Product Review\n\nExisting source.\n")
	if err := os.WriteFile(targetPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildSourceIssuePlan(home, "product:focus_notes", report.IssueIDs[0], targetPath, tProduct)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != SourceIssuePlanSchemaVersion || plan.Status != "review_required" || plan.ProductID != "product:focus_notes" || plan.IssueID != report.IssueIDs[0] {
		t.Fatalf("plan identity = %+v", plan)
	}
	if !strings.Contains(plan.Basis, "review before treating as product change") || len(plan.ReviewDraft.AllowedResults) != 3 {
		t.Fatalf("plan review boundary = %+v", plan)
	}
	if !plan.TargetExists || plan.TargetHash == "" || !hasString(plan.EvidenceRefs, "product_id:product:focus_notes") {
		t.Fatalf("plan target/evidence = %+v", plan)
	}
	paths, err := RecordSourceIssuePlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(paths.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	var recorded SourceIssuePlan
	if err := json.Unmarshal(data, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.ID != plan.ID || recorded.ReviewDraft.ClosureCommand != "compound product source-issue-review <review.yaml>" {
		t.Fatalf("recorded plan = %+v", recorded)
	}
	reviewData, err := os.ReadFile(paths.ReviewPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(reviewData), "Review YAML Template") || !strings.Contains(string(reviewData), "result: resolved|waived|false_positive") {
		t.Fatalf("review markdown missing closure template:\n%s", string(reviewData))
	}
	reviewYAMLData, err := os.ReadFile(paths.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(reviewYAMLData), "TODO_resolved_waived_or_false_positive") {
		t.Fatalf("review YAML draft should require explicit result:\n%s", string(reviewYAMLData))
	}
	draft, err := LoadSourceIssueReview(paths.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSourceIssueReview(draft); err == nil {
		t.Fatal("generated review YAML draft should not validate before reviewer selects a result")
	}
	afterTarget, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterTarget) != string(original) {
		t.Fatal("source issue plan mutated target file")
	}
	doctor, err := Doctor(home, "product:focus_notes")
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "needs_review" || doctor.SourceIssueCount != 2 {
		t.Fatalf("plan should not close issue, doctor = %+v", doctor)
	}
	reviewedYAML := strings.ReplaceAll(string(reviewYAMLData), "TODO_resolved_waived_or_false_positive", "false_positive")
	reviewedYAML = strings.ReplaceAll(reviewedYAML, "TODO: reviewed source owner evidence", "reviewed source issue was not durable product change")
	reviewedPath := filepath.Join(t.TempDir(), "source-issue-review.yaml")
	if err := os.WriteFile(reviewedPath, []byte(reviewedYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	reviewed, err := LoadSourceIssueReview(reviewedPath)
	if err != nil {
		t.Fatal(err)
	}
	closure, err := ApplySourceIssueReview(home, store, reviewed, reviewedPath, tProduct.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if closure.Status != "closed" || closure.IssueID != report.IssueIDs[0] || closure.TransitionID == "" {
		t.Fatalf("closure from generated review YAML = %+v", closure)
	}
	afterClosure, err := Doctor(home, "product:focus_notes")
	if err != nil {
		t.Fatal(err)
	}
	if afterClosure.Status != "needs_review" || afterClosure.SourceIssueCount != 1 {
		t.Fatalf("closing one generated source issue should leave one open issue, doctor = %+v", afterClosure)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("source issue plan should not create claims ledger: %v", err)
	}
}

func TestSourceIssuePlanRejectsClosedIssue(t *testing.T) {
	home := t.TempDir()
	doc, err := LoadChangeStreamCandidates("../../fixtures/product/change-stream-candidates.md")
	if err != nil {
		t.Fatal(err)
	}
	report, err := ApplyChangeStreamCandidates(home, doc, tProduct)
	if err != nil {
		t.Fatal(err)
	}
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	review := SourceIssueReview{
		SchemaVersion: SourceIssueReviewSchemaVersion,
		ID:            "source_issue_review_closed",
		ProductID:     "product:focus_notes",
		IssueID:       report.IssueIDs[0],
		Result:        "false_positive",
		Rationale:     "reviewed source issue and it was not a durable product change",
		EvidenceRefs:  []string{"fixture:source-issue-review"},
	}
	if _, err := ApplySourceIssueReview(home, store, review, "fixtures/product/source-issue-review.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildSourceIssuePlan(home, "product:focus_notes", report.IssueIDs[0], "", tProduct); err == nil {
		t.Fatal("closed source issue should not produce another review plan")
	}
}
