package capgraph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/candidates"
)

func TestSkillPlanRecordsReviewArtifactsWithoutMutatingGraphOrTarget(t *testing.T) {
	home := t.TempDir()
	candidateID := "cand_skill_review"
	dir := filepath.Join(home, candidates.InboxDir(candidates.Skill))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	candidate := skillCandidate{
		ID:           candidateID,
		Kind:         string(candidates.Skill),
		Subject:      "frontend performance lens",
		Statement:    "review React component rendering paths before adding memoization",
		Detail:       "react_performance_review",
		Confidence:   0.88,
		EvidenceRefs: []string{"trace:skill", "receipt:frontend"},
		Verdict:      candidates.Park,
	}
	data, err := json.MarshalIndent(candidate, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, candidateID+".json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(t.TempDir(), "SKILL.md")
	original := []byte("# Existing Skill\n\n")
	if err := os.WriteFile(targetPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := BuildSkillPlan(home, candidateID, targetPath, t0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != SkillPlanSchemaVersion ||
		plan.Status != "review_required" ||
		plan.CandidateID != candidateID ||
		plan.CandidateVerdict != candidates.Park ||
		plan.SkillKey != "frontend-performance-lens" ||
		plan.NodeDraft.NodeID != "skill:frontend-performance-lens" ||
		plan.NodeDraft.Kind != "skill" ||
		!contains(plan.NodeDraft.Triggers, "frontend") ||
		!strings.Contains(plan.NodeDraft.Implementation, "React component") ||
		!plan.TargetExists ||
		plan.TargetHash == "" {
		t.Fatalf("bad skill plan: %+v", plan)
	}
	paths, err := RecordSkillPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	reviewData, err := os.ReadFile(paths.ReviewPath)
	if err != nil {
		t.Fatal(err)
	}
	review := string(reviewData)
	if !strings.Contains(review, "does not mutate CapabilityGraph") ||
		!strings.Contains(review, "Required Evidence Before Import") {
		t.Fatalf("review missing safety/procedure text:\n%s", review)
	}
	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("target mutated:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(home, "registries", "capability-nodes.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("skill-plan must not mutate capability graph, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("skill-plan must not promote claims, stat err=%v", err)
	}
}

// TestReadSkillCandidateRejectsPathTraversalID is the S3 red falsifier:
// readSkillCandidate must reject a path-traversal-shaped candidate id before
// it ever touches disk, not just fail to find a file at the naive path. A
// candidate file with no "id" field is planted OUTSIDE
// inbox/skill-candidates/ so a pre-fix reader (which builds the path with a
// bare filepath.Join and only checks id equality AFTER reading) succeeds
// and defaults the empty id to the traversal string — proving the escape
// rather than a coincidental not-exist error.
func TestReadSkillCandidateRejectsPathTraversalID(t *testing.T) {
	home := t.TempDir()
	inboxDir := filepath.Join(home, candidates.InboxDir(candidates.Skill))
	if err := os.MkdirAll(inboxDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outsideDir := filepath.Join(home, "outside-skill-secret")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	planted := skillCandidate{Kind: string(candidates.Skill), Subject: "escaped read"}
	data, err := json.MarshalIndent(planted, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideDir, "planted.json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	traversalID := "../../outside-skill-secret/planted"
	if _, err := readSkillCandidate(home, traversalID); err == nil {
		t.Fatalf("path-traversal-shaped candidate id %q must be rejected before touching disk", traversalID)
	}
}
