package mem

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMeaningWritePlanRecordsReviewArtifactsWithoutMutatingTarget(t *testing.T) {
	homePath := t.TempDir()
	s, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Capture([]Candidate{{
		Entity:         "project:healthapp",
		Predicate:      "release_gate",
		Object:         "ship only after simulator screenshot pass",
		Confidence:     0.82,
		Derivation:     "extracted",
		ClaimType:      "constraint",
		SourceTraceRef: "trace_release_gate",
		Visibility:     "operational",
	}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Adjudicate(got[0].ID, "park", "work_review", t0); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(t.TempDir(), "brain", "project.md")
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		t.Fatal(err)
	}
	original := "# HealthApp\n\nExisting curated memory.\n"
	if err := os.WriteFile(targetPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := s.BuildMeaningWritePlan(got[0].ID, targetPath, t0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != MeaningWritePlanSchemaVersion ||
		plan.Status != "review_required" ||
		plan.CandidateState != "parked" ||
		plan.TargetHash == "" ||
		!plan.TargetExists ||
		!containsString(plan.EvidenceRefs, "trace_release_gate") ||
		!strings.Contains(plan.ProposedMarkdown, "ship only after simulator screenshot pass") {
		t.Fatalf("bad plan: %+v", plan)
	}
	paths, err := RecordMeaningWritePlan(homePath, plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.PlanPath); err != nil {
		t.Fatalf("plan missing: %v", err)
	}
	reviewData, err := os.ReadFile(paths.ReviewPath)
	if err != nil {
		t.Fatal(err)
	}
	review := string(reviewData)
	if !strings.Contains(review, "does not mutate curated memory source files") ||
		!strings.Contains(review, "Proposed Markdown") {
		t.Fatalf("review missing source-write warning:\n%s", review)
	}
	after, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Fatalf("target mutated:\n%s", after)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
