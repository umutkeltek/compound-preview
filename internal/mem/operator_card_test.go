package mem

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/reviewartifact"
)

// TestAnnotateBakeoffQueueFillsRootLevelStrays reproduces the exact debt
// found live: memory-bakeoff-*.json reports written directly under
// reviews/ (before the memory-bakeoffs/ subdirectory + operator-card
// injection existed), with no status and no operator_summary on disk. The
// annotator must complete the card at read time without rewriting the file.
func TestAnnotateBakeoffQueueFillsRootLevelStrays(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 6, 19, 35, 2, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatalf("init home: %v", err)
	}
	stray := map[string]any{
		"schema_version":   BakeoffReportSchemaVersion,
		"generated_at":     "2026-07-06T19:35:02Z",
		"query_count":      66,
		"k":                10,
		"candidate_engine": "gbrain",
	}
	data, err := json.MarshalIndent(stray, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	reviewsDir := filepath.Join(homePath, "reviews")
	if err := os.MkdirAll(reviewsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	strayPath := filepath.Join(reviewsDir, "memory-bakeoff-20260706T193502Z-29cfa9b977c5.json")
	if err := os.WriteFile(strayPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Items) != 1 {
		t.Fatalf("want 1 stray item, got %d: %+v", len(report.Items), report.Items)
	}
	if report.Items[0].SummarySource != reviewartifact.SummarySourceFallback {
		t.Fatalf("precondition: stray should start as fallback, got %+v", report.Items[0])
	}

	AnnotateBakeoffQueue(&report)
	reviewartifact.Recount(&report)

	item := report.Items[0]
	want := "A recall bake-off ran 66 golden queries against gbrain. Read the scores."
	if item.OperatorSummary != want {
		t.Fatalf("OperatorSummary = %q, want %q", item.OperatorSummary, want)
	}
	if item.Band != reviewartifact.BandFiled {
		t.Fatalf("Band = %q, want %q (reports, not decisions)", item.Band, reviewartifact.BandFiled)
	}
	if item.SummarySource != reviewartifact.SummarySourceAnnotator {
		t.Fatalf("SummarySource = %q, want annotator", item.SummarySource)
	}
	if item.GroupKey != "memory-bakeoffs|gbrain" {
		t.Fatalf("GroupKey = %q, want memory-bakeoffs|gbrain", item.GroupKey)
	}

	// Idempotent / non-mutating: the on-disk file must be untouched.
	after, err := os.ReadFile(strayPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(data) {
		t.Fatalf("annotator must not rewrite the artifact on disk")
	}
}
