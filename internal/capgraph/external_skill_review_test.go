package capgraph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/candidates"
	"compound/internal/reviewartifact"
)

func TestExternalSkillCandidateReviewWritesReviewOnlyWithOverlapScan(t *testing.T) {
	home, store := newStore(t)
	if err := store.Add(Node{
		ID:       "skill:react-best-practices",
		Kind:     "skill",
		Name:     "React Best Practices",
		Triggers: []string{"react", "performance", "rendering"},
		Weight:   1,
	}); err != nil {
		t.Fatal(err)
	}
	nodesPath := filepath.Join(home, "registries", "capability-nodes.jsonl")
	beforeNodes, err := os.ReadFile(nodesPath)
	if err != nil {
		t.Fatal(err)
	}
	candidatePath := writeExternalSkillCandidate(t, ExternalSkillCandidate{
		ID:          "vercel-labs/agent-skills/react-best-practices",
		Name:        "React Best Practices",
		Slug:        "react-best-practices",
		Description: "React rendering performance and architecture review checklist",
		Source:      "vercel-labs/agent-skills",
		SourceType:  "github",
		InstallURL:  "https://github.com/vercel-labs/agent-skills",
		URL:         "https://skills.sh/vercel-labs/agent-skills/react-best-practices",
		Hash:        "skills-sh-hash",
		Installs:    515600,
		Official:    true,
		Topics:      []string{"React", "Testing"},
		Files: []ExternalSkillFile{
			{Path: "SKILL.md", Contents: "---\nname: React Best Practices\n---\n"},
		},
		Audits: []ExternalSkillAudit{
			{Provider: "Socket", Status: "pass", Summary: "No alerts", RiskLevel: "LOW"},
		},
	})
	targetDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(targetDir, "SKILL.md"), []byte("# staged only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	review, err := BuildExternalSkillCandidateReview(home, candidatePath, targetDir, t0)
	if err != nil {
		t.Fatal(err)
	}
	if review.SchemaVersion != SkillCandidateReviewSchemaVersion || review.Status != "review_required" {
		t.Fatalf("review metadata = %+v", review)
	}
	if review.CandidateKind != string(candidates.Skill) || review.CandidateID == "" {
		t.Fatalf("candidate identity = %+v", review)
	}
	if review.ProposedClassification != "shared_skill" || review.LifecycleStage != "staged_candidate" {
		t.Fatalf("classification = %s/%s", review.ProposedClassification, review.LifecycleStage)
	}
	if review.RecommendedDisposition != "merge" {
		t.Fatalf("disposition = %s reason=%s overlaps=%+v", review.RecommendedDisposition, review.DispositionReason, review.OverlapRefs)
	}
	if review.GraphCoherence.DecisionHint != "merge_existing" || review.GraphCoherence.DuplicateRisk < 0.60 || review.GraphCoherence.EvidenceStrength <= 0 {
		t.Fatalf("graph coherence = %+v", review.GraphCoherence)
	}
	if len(review.OverlapRefs) == 0 || review.OverlapRefs[0].NodeID != "skill:react-best-practices" {
		t.Fatalf("overlaps = %+v", review.OverlapRefs)
	}
	if !review.SourceExists || review.SourceHash == "" || !review.TargetExists || review.TargetHash == "" {
		t.Fatalf("hashes not captured: source=%t/%q target=%t/%q", review.SourceExists, review.SourceHash, review.TargetExists, review.TargetHash)
	}
	paths, err := RecordExternalSkillCandidateReview(home, review)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(paths.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	var stored SkillCandidateReview
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.ID != review.ID || stored.RecommendedDisposition != "merge" {
		t.Fatalf("stored review = %+v", stored)
	}
	markdown, err := os.ReadFile(paths.ReviewPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Skill Candidate Intake: React Best Practices",
		"graph_decision_hint: `merge_existing`",
		"Graph Coherence",
		"Overlap Scan",
		"`skill:react-best-practices`",
		"does not mutate CapabilityGraph",
		"does not install or serve external skill files",
	} {
		if !strings.Contains(string(markdown), want) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}
	afterNodes, err := os.ReadFile(nodesPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterNodes) != string(beforeNodes) {
		t.Fatalf("review mutated graph nodes:\nbefore=%s\nafter=%s", beforeNodes, afterNodes)
	}
	if _, err := os.Stat(filepath.Join(home, candidates.InboxDir(candidates.Skill))); !os.IsNotExist(err) {
		t.Fatalf("review must not write skill inbox candidate, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("review must not promote claims, stat err=%v", err)
	}
}

func TestExternalSkillCandidateReviewCoherenceRejectsHighRiskAudit(t *testing.T) {
	home, store := newStore(t)
	if err := store.Add(Node{
		ID:       "skill:image-edit",
		Kind:     "skill",
		Name:     "Image Edit",
		Triggers: []string{"image", "edit"},
		Weight:   1,
	}); err != nil {
		t.Fatal(err)
	}
	candidatePath := writeExternalSkillCandidate(t, ExternalSkillCandidate{
		ID:          "example/high-risk-image-edit",
		Name:        "Image Edit",
		Description: "Image editing workflow",
		Source:      "example/skills",
		URL:         "https://skills.sh/example/skills/image-edit",
		Installs:    200000,
		Files:       []ExternalSkillFile{{Path: "SKILL.md", Contents: "# Image Edit\n"}},
		Audits:      []ExternalSkillAudit{{Provider: "Socket", Status: "pass", RiskLevel: "High"}},
	})
	review, err := BuildExternalSkillCandidateReview(home, candidatePath, "", t0)
	if err != nil {
		t.Fatal(err)
	}
	if review.RecommendedDisposition != "reject" {
		t.Fatalf("disposition = %s reason=%s", review.RecommendedDisposition, review.DispositionReason)
	}
	if review.GraphCoherence.DecisionHint != "reject" || review.GraphCoherence.AuditRisk != "high" || review.GraphCoherence.DecisionConfidence < 0.85 {
		t.Fatalf("graph coherence = %+v", review.GraphCoherence)
	}
}

func TestSkillCandidateDecisionCreatesInboxCandidateOnlyAfterReview(t *testing.T) {
	home, store := newStore(t)
	if err := store.Add(Node{ID: "skill:pricing", Kind: "skill", Name: "Pricing", Triggers: []string{"pricing"}}); err != nil {
		t.Fatal(err)
	}
	nodesPath := filepath.Join(home, "registries", "capability-nodes.jsonl")
	beforeNodes, err := os.ReadFile(nodesPath)
	if err != nil {
		t.Fatal(err)
	}
	candidatePath := writeExternalSkillCandidate(t, ExternalSkillCandidate{
		ID:          "example/skills/pricing",
		Name:        "Pricing",
		Slug:        "pricing",
		Description: "Review pricing and paywall packaging decisions.",
		Source:      "example/skills",
		URL:         "https://skills.sh/example/skills/pricing",
		Files:       []ExternalSkillFile{{Path: "SKILL.md", Contents: "---\nname: pricing\n---\n"}},
	})
	review, err := BuildExternalSkillCandidateReview(home, candidatePath, "", t0)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := RecordExternalSkillCandidateReview(home, review)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := reviewartifactQueueForSkillCandidates(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue.Items) != 1 || queue.Items[0].Readiness != "blocked" || !strings.Contains(queue.Items[0].BlockedReason, "skill candidate decision") {
		t.Fatalf("queue before review = %+v", queue.Items)
	}
	if queue.Items[0].GraphDecisionHint != "merge_existing" || queue.Items[0].GraphDuplicateRisk <= 0 || queue.Items[0].GraphEvidenceStrength <= 0 {
		t.Fatalf("queue should surface graph coherence before decision: %+v", queue.Items[0])
	}
	validReview := strings.ReplaceAll(RenderExternalSkillCandidateDecisionYAMLDraft(review), "TODO_decision_create_candidate_merge_existing_reject_reference_only_keep_needs_review", "create_candidate")
	validReview = strings.ReplaceAll(validReview, "rationale: \"\"", "rationale: \"source reviewed and should enter normal skill-plan intake\"")
	validReview = strings.ReplaceAll(validReview, "confidence: -1", "confidence: 0.72")
	if err := os.WriteFile(paths.ReviewYAMLPath, []byte(validReview), 0o644); err != nil {
		t.Fatal(err)
	}
	decision, err := LoadSkillCandidateDecision(paths.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	report, err := RecordSkillCandidateDecision(home, decision, paths.ReviewYAMLPath, t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "candidate_recorded" || report.CandidateID != review.CandidateID {
		t.Fatalf("decision report = %+v", report)
	}
	var candidate skillCandidate
	if err := candidates.ReadCandidate(home, candidates.Skill, review.CandidateID, &candidate); err != nil {
		t.Fatal(err)
	}
	if candidate.Subject != "Pricing" || candidate.Verdict != candidates.Park || len(candidate.EvidenceRefs) == 0 {
		t.Fatalf("candidate = %+v", candidate)
	}
	queue, err = reviewartifactQueueForSkillCandidates(home)
	if err != nil {
		t.Fatal(err)
	}
	var reviewItem *reviewartifact.QueueItem
	for i := range queue.Items {
		if queue.Items[i].ID == review.ID {
			reviewItem = &queue.Items[i]
		}
	}
	if reviewItem == nil || reviewItem.Readiness != "no_action" || !strings.Contains(reviewItem.NextAction, "skill-plan") {
		t.Fatalf("queue after decision = %+v", queue.Items)
	}
	if reviewItem.GraphDecisionHint != "merge_existing" || reviewItem.GraphTopOverlapScore == 0 {
		t.Fatalf("queue should keep graph coherence after decision: %+v", reviewItem)
	}
	reviewedYAML := readReviewTestFile(t, paths.ReviewYAMLPath)
	refreshed, err := BuildExternalSkillCandidateReview(home, candidatePath, "", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordExternalSkillCandidateReview(home, refreshed); err != nil {
		t.Fatal(err)
	}
	if got := readReviewTestFile(t, paths.ReviewYAMLPath); got != reviewedYAML || strings.Contains(got, "TODO_decision") {
		t.Fatalf("review refresh must preserve reviewed YAML:\nbefore=%s\nafter=%s", reviewedYAML, got)
	}
	refreshedQueue, err := reviewartifactQueueForSkillCandidates(home)
	if err != nil {
		t.Fatal(err)
	}
	reviewItem = nil
	for i := range refreshedQueue.Items {
		if refreshedQueue.Items[i].ID == review.ID {
			reviewItem = &refreshedQueue.Items[i]
		}
	}
	if reviewItem == nil || reviewItem.Readiness != "no_action" || reviewItem.Status != "candidate_recorded" {
		t.Fatalf("queue after refresh should stay terminal: %+v", refreshedQueue.Items)
	}
	afterNodes, err := os.ReadFile(nodesPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterNodes) != string(beforeNodes) {
		t.Fatalf("decision must not mutate graph nodes")
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("decision must not promote claims, stat err=%v", err)
	}
}

func TestExternalSkillCandidateReviewRejectsUnsafeFilePaths(t *testing.T) {
	path := writeExternalSkillCandidateRaw(t, `{
 "id": "bad",
 "name": "Bad",
 "source": "owner/repo",
 "files": [{"path": "../SKILL.md", "contents": "x"}]
}`)
	_, err := LoadExternalSkillCandidate(path)
	if err == nil || !strings.Contains(err.Error(), "skill file path must be relative") {
		t.Fatalf("expected relative path validation error, got %v", err)
	}
}

func writeExternalSkillCandidate(t *testing.T, candidate ExternalSkillCandidate) string {
	t.Helper()
	data, err := json.MarshalIndent(candidate, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	return writeExternalSkillCandidateRaw(t, string(data))
}

func writeExternalSkillCandidateRaw(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "external-skill.json")
	if err := os.WriteFile(path, append([]byte(body), '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readReviewTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func reviewartifactQueueForSkillCandidates(home string) (reviewartifact.QueueReport, error) {
	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: skillCandidateReviewArea}, t0)
	if err != nil {
		return queue, err
	}
	AnnotateSkillCandidateQueue(&queue)
	return queue, nil
}
