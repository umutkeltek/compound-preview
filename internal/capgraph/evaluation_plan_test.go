package capgraph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/reviewartifact"
)

func TestCapabilityEvaluationPlanRecordsReviewArtifactsWithoutMutatingGraph(t *testing.T) {
	home := t.TempDir()
	set, err := LoadCapabilityEvaluations(filepath.Join("..", "..", "fixtures", "capability-evaluations", "web-design-guidelines.json"))
	if err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(t.TempDir(), "capability-manifest.yaml")
	original := []byte("existing: manifest\n")
	if err := os.WriteFile(targetPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := BuildCapabilityEvaluationPlan(home, set, "web-design-guidelines", targetPath, t0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != CapabilityEvaluationPlanSchemaVersion || plan.Status != "review_required" || plan.Capability != "web-design-guidelines" {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.NodeDraft.NodeID != "skill:web-design-guidelines" || plan.NodeDraft.Kind != "skill" || len(plan.NodeDraft.RequiredEvidence) == 0 {
		t.Fatalf("node draft = %+v", plan.NodeDraft)
	}
	if !plan.TargetExists || plan.TargetHash == "" || len(plan.EvidenceRefs) < 2 {
		t.Fatalf("target/evidence not preserved: %+v", plan)
	}
	paths, err := RecordCapabilityEvaluationPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: capabilityEvaluationReviewArea}, t0)
	if err != nil {
		t.Fatal(err)
	}
	AnnotateCapabilityEvaluationQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.ReviewRequired != 1 || len(queue.Items) != 1 {
		t.Fatalf("capability evaluation review queue counts = %+v items=%d", queue.Counts, len(queue.Items))
	}
	card := queue.Items[0].DecisionCard
	if card == nil || card.Meta["review_yaml"] != paths.ReviewYAMLPath || card.Meta["manifest_draft"] != paths.ManifestDraftPath {
		t.Fatalf("decision card missing review paths: item=%+v card=%+v", queue.Items[0], card)
	}
	if queue.Items[0].Readiness != "needs_review" || !strings.Contains(queue.Items[0].NextAction, "edit YAML") || len(queue.Items[0].Actions) != 3 {
		t.Fatalf("pending review should expose guarded actions: %+v", queue.Items[0])
	}
	if !strings.Contains(card.CanonicalActionHint, "evaluation-review") || len(card.Choices) != 3 || !card.Choices[0].RequiresReview {
		t.Fatalf("decision card should point to gated evaluation-review choices: %+v", card)
	}
	for _, action := range queue.Items[0].Actions {
		if !action.RequiresReview || !strings.Contains(action.Command, "compound capability evaluation-review") {
			t.Fatalf("action should require reviewed evaluation command: %+v", action)
		}
	}
	var stored CapabilityEvaluationPlan
	data, err := os.ReadFile(paths.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.ID != plan.ID || stored.NodeDraft.NodeID != plan.NodeDraft.NodeID {
		t.Fatalf("stored plan = %+v", stored)
	}
	if _, err := os.Stat(filepath.Join(home, "registries", "capability-nodes.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("evaluation-plan must not mutate capability nodes, stat err=%v", err)
	}
	manifest, err := LoadSkillManifest(paths.ManifestDraftPath)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != SkillManifestSchemaVersion || manifest.ID != "skill_manifest:"+plan.ID || len(manifest.Skills) != 1 {
		t.Fatalf("manifest = %+v", manifest)
	}
	if manifest.Skills[0].ID != "skill:web-design-guidelines" || manifest.Skills[0].Status != "needs_review" {
		t.Fatalf("manifest skill = %+v", manifest.Skills[0])
	}
	if _, err := LoadCapabilityEvaluationReview(paths.ReviewYAMLPath); err == nil {
		t.Fatal("unedited review YAML should be rejected")
	}
	reviewDraft := string(mustReadFile(t, paths.ReviewYAMLPath))
	reviewDraft = strings.ReplaceAll(reviewDraft, "TODO_activate_reject_or_keep_needs_review", "activate")
	reviewDraft = strings.ReplaceAll(reviewDraft, "rationale: \"\"", "rationale: \"reviewed overlap and validation evidence\"")
	reviewDraft = strings.ReplaceAll(reviewDraft, "confidence: -1", "confidence: 0.82")
	reviewPath := filepath.Join(t.TempDir(), "capability-review.yaml")
	if err := os.WriteFile(reviewPath, []byte(reviewDraft), 0o644); err != nil {
		t.Fatal(err)
	}
	reviewDoc, err := LoadCapabilityEvaluationReview(reviewPath)
	if err != nil {
		t.Fatal(err)
	}
	reviewReport, err := RecordCapabilityEvaluationReview(home, reviewDoc, reviewPath, t0)
	if err != nil {
		t.Fatal(err)
	}
	if reviewReport.Status != "manifest_ready" || reviewReport.NodeStatus != "active" || reviewReport.ManifestPath == "" {
		t.Fatalf("review report = %+v", reviewReport)
	}
	if reviewReport.PlanPath == "" || reviewReport.ReviewedPath == "" {
		t.Fatalf("review report did not close plan artifacts: %+v", reviewReport)
	}
	var reviewedPlan CapabilityEvaluationPlan
	data, err = os.ReadFile(paths.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &reviewedPlan); err != nil {
		t.Fatal(err)
	}
	if reviewedPlan.Status != "recorded" {
		t.Fatalf("reviewed plan status = %q, want recorded", reviewedPlan.Status)
	}
	if _, err := os.Stat(reviewReport.ReviewedPath); err != nil {
		t.Fatalf("reviewed sidecar missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "registries", "capability-nodes.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("evaluation-review must not mutate capability nodes, stat err=%v", err)
	}
	activeManifest, err := LoadSkillManifest(reviewReport.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if activeManifest.Skills[0].Status != "active" {
		t.Fatalf("active manifest = %+v", activeManifest)
	}
	report, err := ApplySkillManifest(home, manifest, t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || len(report.NodeIDs) != 1 {
		t.Fatalf("manifest report = %+v", report)
	}
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := store.Surface("web design guidelines")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("needs_review draft import must not surface: %+v", hits)
	}
	activeReport, err := ApplySkillManifest(home, activeManifest, t0)
	if err == nil || !strings.Contains(err.Error(), "resurrect") {
		t.Fatalf("active manifest must not silently resurrect a retired needs_review node: %v", err)
	}
	if activeReport.Status != "ok" || len(activeReport.NodeIDs) != 0 {
		t.Fatalf("blocked active manifest should report no nodes applied: %+v", activeReport)
	}
	hits, err = store.Surface("web design guidelines")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("retired node must not surface after blocked activation attempt: %+v", hits)
	}
	review, err := os.ReadFile(paths.ReviewPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Capability Evaluation Intake: web-design-guidelines",
		"does not mutate CapabilityGraph",
		"status `needs_review`",
		"Required Evidence Before Import",
		"one validation receipt or eval showing useful behavior",
	} {
		if !strings.Contains(string(review), want) {
			t.Fatalf("review missing %q:\n%s", want, string(review))
		}
	}
	if got := string(mustReadFile(t, targetPath)); got != string(original) {
		t.Fatalf("target mutated: %q", got)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("evaluation-plan must not promote claims, stat err=%v", err)
	}
}

func TestCapabilityEvaluationPlanRejectsAlreadyKnownCapability(t *testing.T) {
	home, store := newStore(t)
	if err := store.Add(Node{ID: "skill:web-design-guidelines", Kind: "skill", Name: "Web Design Guidelines", Triggers: []string{"web", "design"}}); err != nil {
		t.Fatal(err)
	}
	set, err := LoadCapabilityEvaluations(filepath.Join("..", "..", "fixtures", "capability-evaluations", "web-design-guidelines.json"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = BuildCapabilityEvaluationPlan(home, set, "web-design-guidelines", "", t0)
	if err == nil || !strings.Contains(err.Error(), "already known") {
		t.Fatalf("err = %v, want already known", err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
