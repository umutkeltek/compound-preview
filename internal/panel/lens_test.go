package panel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/capgraph"
	"compound/internal/reviewartifact"
)

func TestSelectLensesIncludesAndExcludesReasons(t *testing.T) {
	homePath := t.TempDir()
	store, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	nodes := []capgraph.Node{
		{ID: "lens:included", Kind: "lens", Name: "Included", Triggers: []string{"memory_candidate"}, Weight: 1.5},
		{ID: "lens:token", Kind: "lens", Name: "Token", Triggers: []string{"memory"}, Weight: 0.8},
		{ID: "lens:retired", Kind: "lens", Name: "Retired", Triggers: []string{"memory_candidate"}, Retired: true},
		{ID: "lens:evidence-only", Kind: "lens", Name: "Evidence Only", Triggers: []string{"memory_candidate"}, EvidenceOnly: true},
		{ID: "lens:missing-prompt", Kind: "lens", Name: "Missing Prompt", Triggers: []string{"memory_candidate"}},
		{ID: "lens:mismatch", Kind: "lens", Name: "Mismatch", Triggers: []string{"external_repo"}},
		{ID: "tool:not-a-lens", Kind: "tool", Name: "Ignored Tool", Triggers: []string{"memory_candidate"}},
	}
	for _, node := range nodes {
		if err := store.Add(node); err != nil {
			t.Fatalf("add node %s: %v", node.ID, err)
		}
	}
	prompts := map[string]string{
		"lens:included":      "included prompt\n",
		"lens:token":         "token prompt\n",
		"lens:retired":       "retired prompt\n",
		"lens:evidence-only": "evidence prompt\n",
		"lens:mismatch":      "mismatch prompt\n",
	}
	for id, body := range prompts {
		writeLensPromptFixture(t, homePath, id, body)
	}

	selection, err := SelectLenses(homePath, "memory_candidate")
	if err != nil {
		t.Fatalf("select lenses: %v", err)
	}

	included := map[string]Lens{}
	for _, lens := range selection.Included {
		included[lens.NodeID] = lens
	}
	if len(included) != 2 {
		t.Fatalf("included = %+v, want 2 lenses", included)
	}
	for id, body := range map[string]string{"lens:included": prompts["lens:included"], "lens:token": prompts["lens:token"]} {
		lens := included[id]
		if lens.NodeID == "" {
			t.Fatalf("missing included lens %s in %+v", id, included)
		}
		if got, want := lens.PromptSHA256, sha256Hex([]byte(body)); got != want {
			t.Fatalf("%s prompt hash = %s, want %s", id, got, want)
		}
		if lens.PromptPath != filepath.Join(homePath, "registries", "lens-prompts", id+".md") {
			t.Fatalf("%s prompt path = %s", id, lens.PromptPath)
		}
	}
	if len(selection.IncludedRefs) != 2 {
		t.Fatalf("included refs = %+v, want 2", selection.IncludedRefs)
	}
	refByID := map[string]IncludedLens{}
	for _, ref := range selection.IncludedRefs {
		refByID[ref.Ref] = ref
	}
	if refByID["lens:included"].Reason != "trigger_match" || refByID["lens:included"].Weight != 1.5 {
		t.Fatalf("included ref not populated: %+v", refByID["lens:included"])
	}

	reasons := map[string]string{}
	for _, excluded := range selection.Excluded {
		reasons[excluded.Ref] = excluded.Reason
	}
	wantReasons := map[string]string{
		"lens:retired":        "retired",
		"lens:evidence-only":  "evidence_only",
		"lens:missing-prompt": "no_prompt_in_registry",
		"lens:mismatch":       "trigger_mismatch",
	}
	if !reflect.DeepEqual(reasons, wantReasons) {
		t.Fatalf("excluded reasons = %+v, want %+v", reasons, wantReasons)
	}
}

func TestImportLensManifestIdempotent(t *testing.T) {
	homePath := t.TempDir()
	manifest := filepath.Join("..", "..", "fixtures", "panel", "lens-manifest.yaml")
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	first, err := ImportLensManifest(homePath, manifest, now)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if !first.Changed || first.NodesChanged != 16 || first.PromptsChanged != 17 || !first.AuthorityAppended {
		t.Fatalf("first import result = %+v, want lens and judge prompt content changed", first)
	}
	nodesBefore := readTestFile(t, filepath.Join(homePath, "registries", "capability-nodes.jsonl"))
	authorityBefore := readTestFile(t, filepath.Join(homePath, "registries", "lens-source-authority.jsonl"))

	second, err := ImportLensManifest(homePath, manifest, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if second.Changed || second.NodesChanged != 0 || second.PromptsChanged != 0 || second.AuthorityAppended {
		t.Fatalf("second import result = %+v, want no changes", second)
	}
	if got := readTestFile(t, filepath.Join(homePath, "registries", "capability-nodes.jsonl")); !bytes.Equal(got, nodesBefore) {
		t.Fatalf("node log changed on idempotent import")
	}
	if got := readTestFile(t, filepath.Join(homePath, "registries", "lens-source-authority.jsonl")); !bytes.Equal(got, authorityBefore) {
		t.Fatalf("authority log changed on idempotent import")
	}

	store, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	lensCount := 0
	for _, node := range nodes {
		if node.Kind == "lens" {
			lensCount++
		}
	}
	if lensCount != 16 {
		t.Fatalf("lens node count = %d, want 16", lensCount)
	}
	selection, err := SelectLenses(homePath, "memory_candidate")
	if err != nil {
		t.Fatalf("select imported lenses: %v", err)
	}
	if len(selection.Included) != 6 || len(selection.Excluded) != 10 {
		t.Fatalf("imported memory_candidate selection = %+v, want six included and ten non-memory excluded", selection)
	}
}

func TestSelectLensesWithContextSelectsDomainLenses(t *testing.T) {
	homePath := t.TempDir()
	manifest := filepath.Join("..", "..", "fixtures", "panel", "lens-manifest.yaml")
	if _, err := ImportLensManifest(homePath, manifest, panelTestNow); err != nil {
		t.Fatalf("import lenses: %v", err)
	}

	paid, err := SelectLensesWithContext(homePath, "memory_candidate", LensContext{Domain: "paid-app", Task: "pricing review"})
	if err != nil {
		t.Fatalf("select paid-app lenses: %v", err)
	}
	if !selectionIncludesLens(paid, "lens:paid-app-commercial-fit", lensReasonDomainTriggerMatch) {
		t.Fatalf("paid-app selection missing commercial lens: %+v", paid.IncludedRefs)
	}
	if selectionIncludesLens(paid, "lens:social-distribution-mechanics", "") {
		t.Fatalf("paid-app selection unexpectedly included social lens: %+v", paid.IncludedRefs)
	}
	if len(paid.MissingDomainLens) != 0 {
		t.Fatalf("paid-app missing domain lens = %+v, want none", paid.MissingDomainLens)
	}

	social, err := SelectLensesWithContext(homePath, "memory_candidate", LensContext{Domain: "social-distribution", Task: "audience reach"})
	if err != nil {
		t.Fatalf("select social-distribution lenses: %v", err)
	}
	if !selectionIncludesLens(social, "lens:social-distribution-mechanics", lensReasonDomainTriggerMatch) {
		t.Fatalf("social selection missing distribution lens: %+v", social.IncludedRefs)
	}
	if selectionIncludesLens(social, "lens:paid-app-commercial-fit", "") {
		t.Fatalf("social selection unexpectedly included paid lens: %+v", social.IncludedRefs)
	}
	if len(social.MissingDomainLens) != 0 {
		t.Fatalf("social missing domain lens = %+v, want none", social.MissingDomainLens)
	}

	memoryIngestion, err := SelectLensesWithContext(homePath, "memory_candidate", LensContext{Domain: "memory-ingestion", Task: "conversation memory recall proof"})
	if err != nil {
		t.Fatalf("select memory-ingestion lenses: %v", err)
	}
	if !selectionIncludesLens(memoryIngestion, "lens:memory-ingestion-admission", lensReasonDomainTriggerMatch) {
		t.Fatalf("memory-ingestion selection missing admission lens: %+v", memoryIngestion.IncludedRefs)
	}
	if selectionIncludesLens(memoryIngestion, "lens:paid-app-commercial-fit", "") || selectionIncludesLens(memoryIngestion, "lens:social-distribution-mechanics", "") {
		t.Fatalf("memory-ingestion unexpectedly included unrelated domain lens: %+v", memoryIngestion.IncludedRefs)
	}
	if len(memoryIngestion.MissingDomainLens) != 0 {
		t.Fatalf("memory-ingestion missing domain lens = %+v, want none", memoryIngestion.MissingDomainLens)
	}

	codebase, err := SelectLensesWithContext(homePath, "capability_observation", LensContext{Domain: "codebase-design", Task: "adapt operating-system skill without copying; run seam deletion test"})
	if err != nil {
		t.Fatalf("select codebase-design lenses: %v", err)
	}
	if !selectionIncludesLens(codebase, "lens:codebase-seam-deletion-test", lensReasonDomainTriggerMatch) {
		t.Fatalf("codebase-design selection missing seam deletion lens: %+v", codebase.IncludedRefs)
	}
	if selectionIncludesLens(codebase, "lens:paid-app-commercial-fit", "") || selectionIncludesLens(codebase, "lens:social-distribution-mechanics", "") {
		t.Fatalf("codebase-design unexpectedly included unrelated domain lens: %+v", codebase.IncludedRefs)
	}
	if len(codebase.MissingDomainLens) != 0 {
		t.Fatalf("codebase-design missing domain lens = %+v, want none", codebase.MissingDomainLens)
	}

	antiPattern, err := SelectLensesWithContext(homePath, "capability_observation", LensContext{Domain: "anti-pattern-detectors", Task: "detect map-as-work and skill-folder-copying redirects into consumed proof"})
	if err != nil {
		t.Fatalf("select anti-pattern-detectors lenses: %v", err)
	}
	if !selectionIncludesLens(antiPattern, "lens:anti-pattern-operational-detector", lensReasonDomainTriggerMatch) {
		t.Fatalf("anti-pattern-detectors selection missing operational detector lens: %+v", antiPattern.IncludedRefs)
	}
	if len(antiPattern.MissingDomainLens) != 0 {
		t.Fatalf("anti-pattern-detectors missing domain lens = %+v, want none", antiPattern.MissingDomainLens)
	}

	skillCreator, err := SelectLensesWithContext(homePath, "skill_candidate", LensContext{Domain: "skill-creator", Task: "judge whether the skill fires and changes outcome"})
	if err != nil {
		t.Fatalf("select skill-creator lenses: %v", err)
	}
	if !selectionIncludesLens(skillCreator, "lens:skill-trigger-outcome-fit", lensReasonTriggerMatch) {
		t.Fatalf("skill-creator selection missing trigger outcome lens: %+v", skillCreator.IncludedRefs)
	}
	if selectionIncludesLens(skillCreator, "lens:codebase-seam-deletion-test", "") {
		t.Fatalf("skill-creator unexpectedly included seam deletion lens: %+v", skillCreator.IncludedRefs)
	}
	if len(skillCreator.MissingDomainLens) != 0 {
		t.Fatalf("skill-creator missing domain lens = %+v, want none", skillCreator.MissingDomainLens)
	}

	decisionReview, err := SelectLensesWithContext(homePath, "decision_candidate", LensContext{Domain: "reasoning-lenses", Task: "review falsifier and second-order decision effects"})
	if err != nil {
		t.Fatalf("select decision review lenses: %v", err)
	}
	if !selectionIncludesLens(decisionReview, "lens:decision-falsifier-review", lensReasonTriggerMatch) {
		t.Fatalf("decision review selection missing falsifier lens: %+v", decisionReview.IncludedRefs)
	}
	if !selectionIncludesLens(decisionReview, "lens:inversion-falsification", lensReasonTriggerMatch) {
		t.Fatalf("decision review selection missing generic inversion lens: %+v", decisionReview.IncludedRefs)
	}
	if selectionIncludesLens(decisionReview, "lens:codebase-seam-deletion-test", "") || selectionIncludesLens(decisionReview, "lens:skill-trigger-outcome-fit", "") {
		t.Fatalf("decision review unexpectedly included adaptation lenses: %+v", decisionReview.IncludedRefs)
	}
	if len(decisionReview.MissingDomainLens) != 0 {
		t.Fatalf("decision review missing domain lens = %+v, want none", decisionReview.MissingDomainLens)
	}
}

func TestSelectLensesWithContextRecordsUnknownDomainGap(t *testing.T) {
	homePath := t.TempDir()
	manifest := filepath.Join("..", "..", "fixtures", "panel", "lens-manifest.yaml")
	if _, err := ImportLensManifest(homePath, manifest, panelTestNow); err != nil {
		t.Fatalf("import lenses: %v", err)
	}

	selection, err := SelectLensesWithContext(homePath, "memory_candidate", LensContext{Domain: "hardware-lab"})
	if err != nil {
		t.Fatalf("select unknown-domain lenses: %v", err)
	}
	if len(selection.Included) != 6 {
		t.Fatalf("unknown-domain included lenses = %d, want generic six: %+v", len(selection.Included), selection.IncludedRefs)
	}
	if selectionIncludesLens(selection, "lens:paid-app-commercial-fit", "") || selectionIncludesLens(selection, "lens:social-distribution-mechanics", "") {
		t.Fatalf("unknown-domain selection should not include known domain lenses: %+v", selection.IncludedRefs)
	}
	wantMissing := []string{"hardware", "hardware-lab", "lab"}
	if !reflect.DeepEqual(selection.MissingDomainLens, wantMissing) {
		t.Fatalf("missing domain lens = %+v, want %+v", selection.MissingDomainLens, wantMissing)
	}
}

func writeLensPromptFixture(t *testing.T, homePath, id, body string) {
	t.Helper()
	dir := filepath.Join(homePath, "registries", "lens-prompts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLensManifestFixtureHasRequiredPrompts(t *testing.T) {
	data := readTestFile(t, filepath.Join("..", "..", "fixtures", "panel", "lens-manifest.yaml"))
	for _, id := range []string{
		"duplicate-risk",
		"evidence-strength",
		"contradiction-with-existing-memory",
		"fossilization-risk",
		"privacy-risk",
		"paid-app-commercial-fit",
		"social-distribution-mechanics",
		"comparison-correctness",
		"comparison-contract-fidelity",
		"comparison-cost-latency",
		"inversion-falsification",
		"codebase-seam-deletion-test",
		"skill-trigger-outcome-fit",
		"anti-pattern-operational-detector",
	} {
		if !strings.Contains(string(data), "id: "+id) {
			t.Fatalf("manifest missing lens %s", id)
		}
	}
	if got := strings.Count(string(data), `{"confidence": <0..1>, "findings": [{"claim": "...", "severity": "low|medium|high", "evidence_refs": ["..."]}]}`); got != 16 {
		t.Fatalf("strict JSON contract count = %d, want 16", got)
	}
}

func TestPanelCapabilityFeedbackReviewAppliesGraphEdgesOnlyAfterReview(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 5, 0, 30, 0, 0, time.UTC)

	graph, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Add(capgraph.Node{
		ID:       "lens:decision-falsifier-review",
		Kind:     "lens",
		Name:     "Decision Falsifier Review",
		Triggers: []string{"decision_candidate"},
		Weight:   1,
	}); err != nil {
		t.Fatal(err)
	}

	run := Run{
		SchemaVersion: RunSchemaVersion,
		ID:            "prun_feedback_review",
		CreatedAt:     now.Format(time.RFC3339),
		Status:        "verdicts_recorded",
		TargetKind:    "decision_candidate",
		Targets:       []Target{{Ref: "dc_feedback", Privacy: "internal", StateBefore: "parked"}},
		LensSet: LensSet{Included: []IncludedLens{{
			Ref:    "lens:decision-falsifier-review",
			Reason: "domain_trigger_match",
			Weight: 1.12,
		}}},
	}
	target := TargetInput{
		Kind:    "decision_candidate",
		Ref:     "dc_feedback",
		Summary: "Decision candidate missing falsifier review trigger",
		Payload: []byte(`{"id":"dc_feedback","kind":"decision","statement":"adopt capability"}`),
	}
	lenses := []LensFinding{{
		LensRef:          "lens:decision-falsifier-review",
		RouteEvidenceRef: "route:panel-feedback",
		Status:           "completed",
		Findings: []Finding{{
			Claim:        "decision lacks falsifier",
			Severity:     "high",
			EvidenceRefs: []string{"candidate:dc_feedback"},
		}},
	}}
	synthesis := Synthesis{
		SchemaVersion: SynthesisSchemaVersion,
		Decision:      "park",
		Confidence:    0.82,
		Rationale:     "Park until falsifier review trigger exists.",
	}
	verdictPath, err := writeVerdictArtifact(home, run, target, lenses, synthesis, now)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPanelCapabilityFeedbackPlan(home, verdictPath, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.FeedbackCandidates) != 1 {
		t.Fatalf("feedback candidates = %+v, want one", plan.FeedbackCandidates)
	}
	candidate := plan.FeedbackCandidates[0]
	if candidate.NodeID != "lens:decision-falsifier-review" || candidate.EdgeKind != capgraph.EdValidatedBy || !strings.Contains(candidate.Context, "decision_candidate") {
		t.Fatalf("feedback candidate = %+v", candidate)
	}
	paths, err := RecordPanelCapabilityFeedbackPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	statsBefore, err := graph.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if statsBefore.EdgeCount != 0 {
		t.Fatalf("plan must not mutate graph edges: %+v", statsBefore)
	}

	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: PanelCapabilityFeedbackReviewArea}, now)
	if err != nil {
		t.Fatal(err)
	}
	AnnotateCapabilityFeedbackQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.ReviewRequired != 1 || queue.Counts.DecisionCardCount != 1 || queue.Counts.ActionableCount != 1 {
		t.Fatalf("queue counts = %+v items=%+v", queue.Counts, queue.Items)
	}
	if len(queue.Items) != 1 || queue.Items[0].DecisionCard == nil || !strings.Contains(queue.Items[0].DecisionCard.CanonicalActionHint, "panel feedback-review") {
		t.Fatalf("queue item missing feedback review action: %+v", queue.Items)
	}

	if _, err := LoadPanelCapabilityFeedbackReview(paths.ReviewYAMLPath); err == nil {
		t.Fatal("unedited review YAML should be rejected")
	}
	reviewDraft := string(mustReadPanelFeedbackFile(t, paths.ReviewYAMLPath))
	reviewDraft = strings.ReplaceAll(reviewDraft, "TODO_apply_feedback_reject_keep_needs_review", "apply_feedback")
	reviewDraft = strings.ReplaceAll(reviewDraft, "TODO_apply_or_reject", "apply")
	reviewDraft = strings.ReplaceAll(reviewDraft, `rationale: ""`, `rationale: "reviewed lens useful evidence decision candidate review"`)
	reviewDraft = strings.ReplaceAll(reviewDraft, `reviewed_by: ""`, `reviewed_by: "codex"`)
	editedReview := paths.ReviewYAMLPath + ".edited.yaml"
	if err := os.WriteFile(editedReview, []byte(reviewDraft), 0o644); err != nil {
		t.Fatal(err)
	}
	review, err := LoadPanelCapabilityFeedbackReview(editedReview)
	if err != nil {
		t.Fatal(err)
	}
	report, err := RecordPanelCapabilityFeedbackReview(home, review, editedReview, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "applied_feedback" || report.AppliedEdges != 1 {
		t.Fatalf("review report = %+v", report)
	}
	statsAfter, err := graph.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if statsAfter.EdgeKindCounts[capgraph.EdValidatedBy] != 1 {
		t.Fatalf("validated edge not applied: %+v", statsAfter)
	}

	queue, err = reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: PanelCapabilityFeedbackReviewArea}, now)
	if err != nil {
		t.Fatal(err)
	}
	AnnotateCapabilityFeedbackQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.ReviewRequired != 0 || queue.Counts.OK != 1 || queue.Items[0].Readiness != "no_action" {
		t.Fatalf("reviewed queue should be terminal: counts=%+v item=%+v", queue.Counts, queue.Items[0])
	}
}

func TestAntiPatternDetectorVerdictFeedsCapabilityFeedbackPlan(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 5, 1, 10, 0, 0, time.UTC)

	graph, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Add(capgraph.Node{
		ID:       "lens:anti-pattern-operational-detector",
		Kind:     "lens",
		Name:     "Anti-Pattern Operational Detector",
		Triggers: []string{"anti-pattern-detectors"},
		Weight:   1.14,
	}); err != nil {
		t.Fatal(err)
	}

	run := Run{
		SchemaVersion: RunSchemaVersion,
		ID:            "prun_anti_pattern_feedback",
		CreatedAt:     now.Format(time.RFC3339),
		Status:        "verdicts_recorded",
		TargetKind:    "capability_observation",
		Targets:       []Target{{Ref: "capobs_anti_pattern_prompt_only", Privacy: "internal", StateBefore: "park"}},
		LensSet: LensSet{Included: []IncludedLens{{
			Ref:    "lens:anti-pattern-operational-detector",
			Reason: "domain_trigger_match",
			Weight: 1.14,
		}}},
	}
	target := TargetInput{
		Kind:       "capability_observation",
		Ref:        "capobs_anti_pattern_prompt_only",
		Visibility: "internal",
		Summary:    "Anti-pattern rules listed as prose without consumer proof",
		Payload:    []byte(`{"id":"capobs_anti_pattern_prompt_only","kind":"capability_observation","state":"park","summary":"anti-pattern prose lacks redirect proof"}`),
	}
	lenses := []LensFinding{{
		LensRef:          "lens:anti-pattern-operational-detector",
		RouteEvidenceRef: "route:anti-pattern-detector",
		Status:           "completed",
		Findings: []Finding{{
			Claim:        "anti-pattern list lacks trigger redirect consumer proof silence retirement guard",
			Severity:     "high",
			EvidenceRefs: []string{"candidate:capobs_anti_pattern_prompt_only", "anti_pattern:trigger", "anti_pattern:redirect", "anti_pattern:consumer", "anti_pattern:proof", "anti_pattern:silence", "anti_pattern:retirement"},
		}},
	}}
	synthesis := Synthesis{
		SchemaVersion: SynthesisSchemaVersion,
		Decision:      "needs_human",
		Confidence:    0.84,
		Rationale:     "review whether detector should validate, miss, or false-trigger capability surfacing",
	}
	verdictPath, err := writeVerdictArtifact(home, run, target, lenses, synthesis, now)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPanelCapabilityFeedbackPlan(home, verdictPath, now)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "review_required" || len(plan.FeedbackCandidates) != 1 {
		t.Fatalf("feedback plan = %+v", plan)
	}
	candidate := plan.FeedbackCandidates[0]
	if candidate.NodeID != "lens:anti-pattern-operational-detector" || candidate.EdgeKind != capgraph.EdMissedWhen {
		t.Fatalf("feedback candidate = %+v, want detector missed_when redirect candidate", candidate)
	}
	if !strings.Contains(candidate.Context, "capability_observation") ||
		!strings.Contains(candidate.Context, "anti-pattern list lacks") ||
		!strings.Contains(candidate.Context, "trigger redirect consumer proof silence retirement") {
		t.Fatalf("feedback candidate context = %q, want target and finding details", candidate.Context)
	}
	paths, err := RecordPanelCapabilityFeedbackPlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: PanelCapabilityFeedbackReviewArea}, now)
	if err != nil {
		t.Fatal(err)
	}
	AnnotateCapabilityFeedbackQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.ReviewRequired != 1 || queue.Counts.ActionableCount != 1 {
		t.Fatalf("feedback queue counts = %+v item=%+v paths=%+v", queue.Counts, queue.Items, paths)
	}
	stats, err := graph.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.EdgeCount != 0 {
		t.Fatalf("feedback plan must not mutate capability graph edges: %+v", stats)
	}
}

func TestAntiPatternDetectorFindingPreservesOperationalRedirectEvidence(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 5, 2, 55, 0, 0, time.UTC)
	graph, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Add(capgraph.Node{
		ID:       "lens:anti-pattern-operational-detector",
		Kind:     "lens",
		Name:     "Anti-Pattern Operational Detector",
		Triggers: []string{"anti-pattern-detectors"},
		Weight:   1.14,
	}); err != nil {
		t.Fatal(err)
	}
	run := Run{
		SchemaVersion: RunSchemaVersion,
		ID:            "prun_anti_pattern_redirect_contract",
		CreatedAt:     now.Format(time.RFC3339),
		Status:        "verdicts_recorded",
		TargetKind:    "capability_observation",
		Targets:       []Target{{Ref: "capobs_prompt_only_detector", Privacy: "internal", StateBefore: "park"}},
		LensSet: LensSet{Included: []IncludedLens{{
			Ref:    "lens:anti-pattern-operational-detector",
			Reason: "domain_trigger_match",
			Weight: 1.14,
		}}},
	}
	target := TargetInput{
		Kind:       "capability_observation",
		Ref:        "capobs_prompt_only_detector",
		Visibility: "internal",
		Summary:    "anti-pattern prose has trigger words but no consumer proof",
		Payload:    []byte(`{"id":"capobs_prompt_only_detector","kind":"capability_observation","state":"park"}`),
	}
	lenses := []LensFinding{{
		LensRef:          "lens:anti-pattern-operational-detector",
		RouteEvidenceRef: "route:anti-pattern-detector",
		Status:           "completed",
		Findings: []Finding{{
			Claim:        "anti-pattern detector lacks trigger redirect consumer proof silence retirement contract",
			Severity:     "high",
			EvidenceRefs: []string{"anti_pattern:trigger", "anti_pattern:redirect", "anti_pattern:consumer", "anti_pattern:proof", "anti_pattern:silence", "anti_pattern:retirement"},
		}},
	}}
	verdictPath, err := writeVerdictArtifact(home, run, target, lenses, Synthesis{
		SchemaVersion: SynthesisSchemaVersion,
		Decision:      "needs_human",
		Confidence:    0.86,
		Rationale:     "prompt-only anti-pattern detector needs an operational redirect before promotion",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPanelCapabilityFeedbackPlan(home, verdictPath, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.FeedbackCandidates) != 1 {
		t.Fatalf("feedback candidates = %+v, want one", plan.FeedbackCandidates)
	}
	candidate := plan.FeedbackCandidates[0]
	if candidate.EdgeKind != capgraph.EdMissedWhen || candidate.NodeID != "lens:anti-pattern-operational-detector" {
		t.Fatalf("feedback candidate = %+v, want missed_when detector candidate", candidate)
	}
	for _, ref := range []string{"anti_pattern:trigger", "anti_pattern:redirect", "anti_pattern:consumer", "anti_pattern:proof", "anti_pattern:silence", "anti_pattern:retirement"} {
		if !containsString(candidate.EvidenceRefs, ref) {
			t.Fatalf("feedback candidate evidence refs = %v, missing %s", candidate.EvidenceRefs, ref)
		}
	}
	if !strings.Contains(candidate.Context, "trigger redirect consumer proof silence retirement") ||
		!strings.Contains(candidate.Rationale, "missed_when") ||
		!strings.Contains(candidate.Rationale, "redirect context") {
		t.Fatalf("feedback candidate lost operational redirect semantics: %+v", candidate)
	}
}

func mustReadPanelFeedbackFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func selectionIncludesLens(selection LensSelection, ref, reason string) bool {
	for _, included := range selection.IncludedRefs {
		if included.Ref != ref {
			continue
		}
		return reason == "" || included.Reason == reason
	}
	return false
}
