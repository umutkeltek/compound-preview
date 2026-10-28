package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"compound/internal/authority"
	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/decisions"
	"compound/internal/home"
	"compound/internal/mem"
	"compound/internal/observation"
	panelpkg "compound/internal/panel"
	"compound/internal/product"
	"compound/internal/reviewartifact"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func TestOrientTensionCardsFalsifiers(t *testing.T) {
	live := map[string]bool{"claim_a": true, "claim_b": true, "claim_c": true, "claim_law": true}
	reviews := reviewartifact.QueueReport{Items: []reviewartifact.QueueItem{
		{Area: mem.ReconcileContestedArea, ID: "card_1", Status: "review_required", Readiness: "needs_review", OperatorSummary: "memory says A but evidence says B — representation correct?", EvidenceRefs: []string{"claim_a", "claim_b"}},
		{Area: mem.ReconcileContestedArea, ID: "card_2", Status: "review_required", Readiness: "needs_review", OperatorSummary: "memory says B but evidence says C — representation correct?", EvidenceRefs: []string{"claim_b", "claim_c"}},
	}}
	view := decisions.StandingLawView{
		Records:   []decisions.StandingLawRecord{{ClaimID: "claim_law", DecisionID: "decision_law", Title: "Keep the gate"}},
		Conflicts: []decisions.StandingLawConflict{{CandidateID: "cand_reverse", Restatement: "remove the gate", ConflictsWithClaim: "claim_law", ConflictsWithDecision: "decision_law", ConflictingTitle: "Keep the gate"}},
	}

	t.Run("f1 bounded detected cards carry sentences ids and commands", func(t *testing.T) {
		got := selectOrientTensionCards(reviews, live, nil, view)
		if got == nil || len(got.Items) != 3 {
			t.Fatalf("tensions = %+v, want three", got)
		}
		for _, card := range got.Items {
			if !strings.HasSuffix(card.Summary, "?") || len(card.Refs) < 2 || len(card.Command) == 0 {
				t.Fatalf("card lacks question, object ids, or command: %+v", card)
			}
		}
		report := orientOperatorReport{GeneratedAt: "now", NeedsMe: orientNeedsMe{TensionCards: got}}
		out := captureStdout(t, func() { printOrientOperatorReport(report) })
		if strings.Count(out, "headline=") != 1 || strings.Count(out, " - tension_card:") != 3 {
			t.Fatalf("headline/drill-down bounds failed:\n%s", out)
		}
	})

	t.Run("f2 superseded sides are stale", func(t *testing.T) {
		got := selectOrientTensionCards(reviews, live, map[string]bool{"claim_a": true}, decisions.StandingLawView{Conflicts: view.Conflicts})
		if got == nil || len(got.Items) != 1 || got.Items[0].ID != "card_2" {
			t.Fatalf("stale cards served: %+v", got)
		}
	})

	t.Run("f3 silence and f4 disposition", func(t *testing.T) {
		cleared := reviews
		for i := range cleared.Items {
			cleared.Items[i].Status = "dismissed"
			cleared.Items[i].Readiness = "no_action"
		}
		if got := selectOrientTensionCards(cleared, live, nil, decisions.StandingLawView{}); got != nil {
			t.Fatalf("cleared cards re-served: %+v", got)
		}
		out := captureStdout(t, func() { printOrientOperatorReport(orientOperatorReport{GeneratedAt: "now"}) })
		if strings.Contains(out, "tension") {
			t.Fatalf("empty tension scaffolding rendered:\n%s", out)
		}
	})

	t.Run("f5 selection is read only", func(t *testing.T) {
		homePath := t.TempDir()
		path := filepath.Join(homePath, "sentinel")
		if err := os.WriteFile(path, []byte("unchanged"), 0o600); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(path)
		_ = selectOrientTensionCards(reviews, live, nil, view)
		after, _ := os.ReadFile(path)
		if string(before) != string(after) {
			t.Fatal("tension selection mutated test home")
		}
	})
}

func TestOrientOperatorReportMatchesRawHomeState(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 2, 20, 30, 0, 0, time.UTC)
	seedOrientOperatorHome(t, homePath, now)

	report, err := buildOrientOperatorReport(homePath, 5, now, "")
	if err != nil {
		t.Fatalf("orient operator report: %v", err)
	}

	rawCandidates := countJSONFiles(t, filepath.Join(homePath, candidates.InboxDir(candidates.Memory))) +
		countJSONFiles(t, filepath.Join(homePath, candidates.InboxDir(candidates.Skill))) +
		countJSONFiles(t, filepath.Join(homePath, candidates.InboxDir(candidates.CapabilityObs))) +
		countJSONFiles(t, filepath.Join(homePath, candidates.InboxDir(candidates.MethodUpgrade)))
	if report.NeedsMe.CandidateTotal != rawCandidates {
		t.Fatalf("candidate total = %d, want raw %d", report.NeedsMe.CandidateTotal, rawCandidates)
	}
	if report.NeedsMe.CandidateActionable != 1 {
		t.Fatalf("candidate actionable = %d, want only the unstated skill candidate actionable: %+v", report.NeedsMe.CandidateActionable, report.NeedsMe)
	}
	memoryInbox := findOrientCandidateInbox(t, report, string(candidates.Memory))
	if memoryInbox.Count != 1 || memoryInbox.ActionableCount != 0 || memoryInbox.StateCounts["promoted"] != 1 {
		t.Fatalf("memory inbox state counts = %+v", memoryInbox)
	}
	skillInbox := findOrientCandidateInbox(t, report, string(candidates.Skill))
	if skillInbox.Count != 1 || skillInbox.ActionableCount != 1 || skillInbox.StateCounts["unspecified"] != 1 {
		t.Fatalf("skill inbox state counts = %+v", skillInbox)
	}
	capabilityInbox := findOrientCandidateInbox(t, report, string(candidates.CapabilityObs))
	if capabilityInbox.Count != 1 || capabilityInbox.ActionableCount != 0 || capabilityInbox.StateCounts["no_promote"] != 1 {
		t.Fatalf("capability inbox state counts = %+v", capabilityInbox)
	}
	methodInbox := findOrientCandidateInbox(t, report, string(candidates.MethodUpgrade))
	if methodInbox.Count != 1 || methodInbox.ActionableCount != 0 || methodInbox.StateCounts["park"] != 1 {
		t.Fatalf("method inbox state counts = %+v", methodInbox)
	}
	if report.NeedsMe.ReviewRequired != 1 || report.NeedsMe.PendingPanelNeedsHuman != 1 {
		t.Fatalf("needs-me review counts = %+v, want one review-required needs-human panel verdict", report.NeedsMe)
	}
	if report.NeedsMe.OldestItem == nil || report.NeedsMe.OldestItem.AgeSeconds <= 0 {
		t.Fatalf("oldest item missing age: %+v", report.NeedsMe.OldestItem)
	}
	if report.NeedsMe.OldestActionableItem == nil || report.NeedsMe.OldestActionableItem.ID != "skill_orient_fixture" {
		t.Fatalf("oldest actionable candidate = %+v", report.NeedsMe.OldestActionableItem)
	}
	if report.Running.OpenCount != 1 || report.Running.InProgress != 1 || report.Running.NonStaleLeaseCount != 1 {
		t.Fatalf("running counts = %+v, want one open in-progress active lease", report.Running)
	}
	if len(report.Running.OpenWork) != 1 || report.Running.OpenWork[0].Actor != "codex" || report.Running.OpenWork[0].ClaimedAgeSeconds <= 0 {
		t.Fatalf("open work = %+v, want codex claimed running item with age", report.Running.OpenWork)
	}
	if report.Running.LastDispatchStepRun == nil || report.Running.LastDispatchStepRun.AgeSeconds <= 0 {
		t.Fatalf("last dispatch-step run age missing: %+v", report.Running.LastDispatchStepRun)
	}
	if report.Changed.CurrentClaimSeq != 1 || report.Changed.ClaimsSeqDelta != 1 || !report.Changed.FirstRun {
		t.Fatalf("first changed cursor = %+v, want first-run delta of current seq", report.Changed)
	}
	if len(report.Changed.Recent) == 0 || report.Changed.Recent[0].Summary != "operator orientation proof closed" {
		t.Fatalf("recent changes = %+v, want receipt summary", report.Changed.Recent)
	}
	if !stringSliceContains(report.Changed.Recent[0].EvidenceRefs, "ver_orient_done") {
		t.Fatalf("recent evidence refs = %+v, want receipt proof ref", report.Changed.Recent[0].EvidenceRefs)
	}

	second, err := buildOrientOperatorReport(homePath, 5, now.Add(time.Minute), "")
	if err != nil {
		t.Fatalf("second orient operator report: %v", err)
	}
	if second.Changed.FirstRun || second.Changed.ClaimsSeqDelta != 0 || second.Changed.LastSeenSeq != 1 {
		t.Fatalf("second changed cursor = %+v, want no seq delta after cursor write", second.Changed)
	}
}

func TestOrientOperatorUsesMemoryCandidateAuthorityForNeedsMe(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 4, 8, 45, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
		t.Fatalf("init home: %v", err)
	}
	// Write a raw proposed inbox row that LOOKS operator-actionable; list/orient
	// re-admits via knowledge_import → parked source-index (authority, not raw state).
	candID := "cand_orient_knowledge_import_fixture"
	raw := mem.Candidate{
		ID:              candID,
		Entity:          "knowledge:chatgpt:fixture",
		Predicate:       "reference_title",
		Object:          "Source Metadata Fixture",
		ObservationDate: now.Add(-30 * time.Minute).Format(time.RFC3339),
		Confidence:      0.8,
		Derivation:      "deterministic",
		Visibility:      "private",
		PromotionBasis:  candidates.BasisTrustedDeterministic,
		Qualifiers: map[string]string{
			"source":      "knowledge_import",
			"source_kind": "chatgpt",
		},
		SourceTraceRef: "ev_orient_fixture",
		SnippetHash:    "sha256:orient_fixture",
		State:          "proposed",
		CreatedAt:      now.Add(-30 * time.Minute).Format(time.RFC3339),
	}
	if err := candidates.WriteCandidate(homePath, candidates.Memory, candID, &raw); err != nil {
		t.Fatalf("write raw candidate: %v", err)
	}
	rawPath := filepath.Join(homePath, candidates.InboxDir(candidates.Memory), candID+".json")
	rawData, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	var rawPayload candidateReadinessPayload
	if err := json.Unmarshal(rawData, &rawPayload); err != nil {
		t.Fatal(err)
	}
	if got := rawPayload.State; got != "proposed" {
		t.Fatalf("raw candidate state = %q, want proposed fixture proving raw file alone is misleading", got)
	}

	report := buildOrientNeedsMe(homePath, reviewartifact.QueueReport{}, work.WorkbenchReport{}, now, "")
	memoryInbox := findOrientCandidateInbox(t, orientOperatorReport{NeedsMe: report}, string(candidates.Memory))
	if memoryInbox.Count != 1 || memoryInbox.ActionableCount != 0 {
		t.Fatalf("memory inbox = %+v, want authority-parked non-actionable source metadata", memoryInbox)
	}
	if got := memoryInbox.StateCounts["parked"]; got != 1 {
		t.Fatalf("memory state_counts[parked] = %d, want 1 from memory authority; counts=%+v", got, memoryInbox.StateCounts)
	}
	if report.CandidateActionable != 0 || report.OldestActionableItem != nil {
		t.Fatalf("needs-me = %+v, want no actionable memory debt from authority-parked metadata", report)
	}
}

func TestOrientOperatorResolvesPanelLensGapAfterReviewedLensImport(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 4, 9, 30, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
		t.Fatalf("init home: %v", err)
	}
	if _, err := panelpkg.ImportLensManifest(homePath, filepath.Join("..", "..", "fixtures", "panel", "lens-manifest.yaml"), now.Add(-45*time.Minute)); err != nil {
		t.Fatalf("import lenses: %v", err)
	}
	id := "cobs_missing_domain_lens_fixture"
	if err := candidates.WriteCandidate(homePath, candidates.CapabilityObs, id, map[string]any{
		"id":         id,
		"kind":       string(candidates.CapabilityObs),
		"subject":    "panel_lens_selection",
		"statement":  "No reviewed panel lens matched requested domain context.",
		"detail":     "target_kind=memory_candidate domain=memory-ingestion task=conversation memory recall proof",
		"confidence": 1.0,
		"state":      "needs_review",
		"evidence_refs": []string{
			"panel:lens_selection",
			"target_kind:memory_candidate",
			"domain:memory-ingestion",
			"missing_domain_lens:ingestion,memory,memory-ingestion",
			"task:conversation memory recall proof",
		},
	}); err != nil {
		t.Fatalf("write capability observation: %v", err)
	}

	report := buildOrientNeedsMe(homePath, reviewartifact.QueueReport{}, work.WorkbenchReport{}, now, "")
	capabilityInbox := findOrientCandidateInbox(t, orientOperatorReport{NeedsMe: report}, string(candidates.CapabilityObs))
	if capabilityInbox.Count != 1 || capabilityInbox.ActionableCount != 0 {
		t.Fatalf("capability observation inbox = %+v, want resolved non-actionable lens gap", capabilityInbox)
	}
	if got := capabilityInbox.StateCounts["resolved"]; got != 1 {
		t.Fatalf("capability state_counts[resolved] = %d, want 1; counts=%+v", got, capabilityInbox.StateCounts)
	}
	if report.CandidateActionable != 0 || report.OldestActionableItem != nil {
		t.Fatalf("needs-me = %+v, want no actionable candidate after reviewed lens import", report)
	}
}

func TestOrientOperatorTreatsTerminalCandidateVerdictAsNonActionable(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 5, 9, 15, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
		t.Fatalf("init home: %v", err)
	}

	if err := candidates.WriteCandidate(homePath, candidates.CapabilityObs, "ocand_review_consumed", map[string]any{
		"id":      "ocand_review_consumed",
		"kind":    string(candidates.CapabilityObs),
		"subject": "observation:capability:skill-intake-adaptation-gate",
		"state":   "needs_review",
		"verdict": "park",
		"detail":  "source_kind=operating_system_recipe subject_kind=capability",
		"evidence_refs": []string{
			"panel_verdict:panel_verdict_reviewed",
			"source_observation_verdict:review_recorded",
		},
	}); err != nil {
		t.Fatalf("write consumed capability observation: %v", err)
	}

	report := buildOrientNeedsMe(homePath, reviewartifact.QueueReport{}, work.WorkbenchReport{}, now, "")
	capabilityInbox := findOrientCandidateInbox(t, orientOperatorReport{NeedsMe: report}, string(candidates.CapabilityObs))
	if capabilityInbox.Count != 1 || capabilityInbox.ActionableCount != 0 {
		t.Fatalf("capability inbox = %+v, want parked verdict non-actionable despite needs_review state", capabilityInbox)
	}
	if got := capabilityInbox.StateCounts["park"]; got != 1 {
		t.Fatalf("capability state_counts[park] = %d, want 1 from terminal verdict; counts=%+v", got, capabilityInbox.StateCounts)
	}
	if report.CandidateActionable != 0 || report.OldestActionableItem != nil {
		t.Fatalf("needs-me = %+v, want no actionable candidate after terminal verdict", report)
	}
}

func TestOrientOperatorCarriesObservationReviewDebtProjection(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 4, 16, 10, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
		t.Fatalf("init home: %v", err)
	}
	captureStdout(t, func() {
		cmdPanel([]string{"import-lenses", filepath.Join("..", "..", "fixtures", "panel", "lens-manifest.yaml")})
	})
	out := captureStdout(t, func() {
		cmdSource([]string{"observe-batch", "../../fixtures/observation/social-distribution-feedback.json", "--review-candidates", "--json"})
	})
	var observed observation.Report
	if err := json.Unmarshal([]byte(out), &observed); err != nil {
		t.Fatalf("parse observation report: %v\n%s", err, out)
	}
	if len(observed.CandidateReviewResults) != 1 {
		t.Fatalf("candidate review results = %+v", observed.CandidateReviewResults)
	}
	result := observed.CandidateReviewResults[0]
	if _, err := reviewartifact.Write(homePath, "panel-verdicts", "panel_verdict_observation_orient", map[string]any{
		"schema_version": "compound.panel_verdict.v0.1",
		"id":             "panel_verdict_observation_orient",
		"created_at":     now.Add(-20 * time.Minute).Format(time.RFC3339),
		"run_id":         "prun_observation_orient",
		"target": map[string]any{
			"kind":    string(candidates.CapabilityObs),
			"ref":     result.CandidateID,
			"summary": "social-distribution feedback observation needs synthesis review",
			"payload": map[string]any{"object": "Observation feedback should stay candidate material until a product/source owner consumes it."},
		},
		"lens_findings": []map[string]any{{
			"lens_ref": "lens:social-distribution-mechanics",
			"status":   "completed",
			"findings": []map[string]any{{"claim": "bounded distribution feedback", "severity": "low"}},
		}},
		"synthesis": map[string]any{
			"schema_version": "compound.panel_synthesis.v0.1",
			"decision":       "park",
			"confidence":     0.74,
			"rationale":      "review material only",
		},
		"evidence_refs": []string{"observation:" + result.ObservationID},
	}, " ", "# Panel Verdict\n"); err != nil {
		t.Fatalf("write observation panel verdict: %v", err)
	}

	report, err := buildOrientOperatorReport(homePath, 5, now, "")
	if err != nil {
		t.Fatalf("orient operator report: %v", err)
	}
	obs := report.NeedsMe.ObservationReviews
	if obs == nil || obs.ObservationCandidateReviewCount != 1 || obs.ObservationPanelVerdictCount != 1 || obs.TotalReviewDebt != 2 {
		t.Fatalf("observation review projection = %+v", obs)
	}
	if !strings.Contains(obs.NextAction, "plan observation lens review") || len(obs.Commands) != 2 {
		t.Fatalf("observation review next action/commands = %+v", obs)
	}
	if !orientHasDrillDown(report, "observation candidates") || !orientHasDrillDown(report, "observation panel verdicts") {
		t.Fatalf("observation drill-down commands missing: %+v", report.DrillDown)
	}

	text := captureStdout(t, func() {
		cmdOrientOperator(homePath, []string{"--operator"})
	})
	for _, want := range []string{
		"observation_reviews: candidate_reviews=1 panel_verdicts=1 total=2",
		"observation candidates: compound reviews --area observation-candidates",
		"observation panel verdicts: compound reviews --area panel-verdicts",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("orient output missing %q:\n%s", want, text)
		}
	}
}

func TestOrientOperatorCarriesReviewedCapabilityFeedbackImpact(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 5, 1, 10, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
		t.Fatalf("init home: %v", err)
	}
	graph, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatalf("open capability graph: %v", err)
	}
	if err := graph.Add(capgraph.Node{
		ID:       "lens:decision-falsifier-review",
		Kind:     "lens",
		Name:     "Decision Falsifier Review",
		Triggers: []string{"decision_candidate"},
		Weight:   1,
	}); err != nil {
		t.Fatalf("add graph node: %v", err)
	}
	verdictPaths, err := reviewartifact.Write(homePath, "panel-verdicts", "panel_verdict_capability_feedback_orient", map[string]any{
		"schema_version": "compound.panel_verdict.v0.1",
		"id":             "panel_verdict_capability_feedback_orient",
		"created_at":     now.Add(-35 * time.Minute).Format(time.RFC3339),
		"run_id":         "prun_capability_feedback_orient",
		"target": map[string]any{
			"kind":    "decision_candidate",
			"ref":     "dc_capability_feedback_orient",
			"summary": "decision candidate needs falsifier review",
			"payload": map[string]any{"id": "dc_capability_feedback_orient", "kind": "decision"},
		},
		"lens_findings": []map[string]any{{
			"lens_ref":           "lens:decision-falsifier-review",
			"route_evidence_ref": "route:capability-feedback-orient",
			"status":             "completed",
			"confidence":         0.9,
			"findings": []map[string]any{{
				"claim":         "decision lacks falsifier",
				"severity":      "high",
				"evidence_refs": []string{"candidate:dc_capability_feedback_orient"},
			}},
		}},
		"synthesis": map[string]any{
			"schema_version": "compound.panel_synthesis.v0.1",
			"decision":       "park",
			"confidence":     0.84,
			"rationale":      "park until falsifier is reviewed",
		},
		"evidence_refs": []string{"candidate:dc_capability_feedback_orient"},
	}, " ", "# Panel verdict\n")
	if err != nil {
		t.Fatalf("write panel verdict: %v", err)
	}
	plan, err := panelpkg.BuildPanelCapabilityFeedbackPlan(homePath, verdictPaths.PlanPath, now.Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("build capability feedback plan: %v", err)
	}
	feedbackPaths, err := panelpkg.RecordPanelCapabilityFeedbackPlan(homePath, plan)
	if err != nil {
		t.Fatalf("record capability feedback plan: %v", err)
	}
	pending, err := buildOrientOperatorReport(homePath, 5, now.Add(-20*time.Minute), "")
	if err != nil {
		t.Fatalf("pending orient report: %v", err)
	}
	if pending.NeedsMe.CapabilityFeedback == nil {
		t.Fatalf("capability feedback projection missing pending review: %+v", pending.NeedsMe)
	}
	if pending.NeedsMe.CapabilityFeedback.PendingReviewCount != 1 || pending.NeedsMe.CapabilityFeedback.AppliedReviewCount != 0 {
		t.Fatalf("pending capability feedback = %+v, want one pending and no applied review", pending.NeedsMe.CapabilityFeedback)
	}
	reviewBytes, err := os.ReadFile(feedbackPaths.ReviewYAMLPath)
	if err != nil {
		t.Fatalf("read review yaml: %v", err)
	}
	reviewDraft := string(reviewBytes)
	reviewDraft = strings.ReplaceAll(reviewDraft, "TODO_apply_feedback_reject_keep_needs_review", "apply_feedback")
	reviewDraft = strings.ReplaceAll(reviewDraft, "TODO_apply_or_reject", "apply")
	reviewDraft = strings.ReplaceAll(reviewDraft, "rationale: \"\"", "rationale: \"reviewed feedback should change future lens surfacing\"")
	reviewDraft = strings.ReplaceAll(reviewDraft, "reviewed_by: \"\"", "reviewed_by: \"codex\"")
	editedReview := feedbackPaths.ReviewYAMLPath + ".edited.yaml"
	if err := os.WriteFile(editedReview, []byte(reviewDraft), 0o644); err != nil {
		t.Fatalf("write edited review: %v", err)
	}
	review, err := panelpkg.LoadPanelCapabilityFeedbackReview(editedReview)
	if err != nil {
		t.Fatalf("load edited review: %v", err)
	}
	if _, err := panelpkg.RecordPanelCapabilityFeedbackReview(homePath, review, editedReview, now.Add(-10*time.Minute)); err != nil {
		t.Fatalf("record edited feedback review: %v", err)
	}
	applied, err := buildOrientOperatorReport(homePath, 5, now, "")
	if err != nil {
		t.Fatalf("applied orient report: %v", err)
	}
	feedback := applied.NeedsMe.CapabilityFeedback
	if feedback == nil {
		t.Fatalf("capability feedback projection missing after reviewed graph feedback: %+v", applied.NeedsMe)
	}
	if feedback.PendingReviewCount != 0 || feedback.AppliedReviewCount != 1 || feedback.AppliedEdges != 1 {
		t.Fatalf("applied capability feedback = %+v, want reviewed graph impact", feedback)
	}
	if got := feedback.EdgeKindCounts[capgraph.EdValidatedBy]; got != 1 {
		t.Fatalf("edge kind counts = %+v, want validated_by=1", feedback.EdgeKindCounts)
	}
	if len(feedback.Recent) != 1 || feedback.Recent[0].VerdictID != "panel_verdict_capability_feedback_orient" {
		t.Fatalf("recent capability feedback = %+v, want reviewed verdict", feedback.Recent)
	}
	if !orientHasDrillDown(applied, "capability feedback") || !orientHasDrillDown(applied, "capability authority") {
		t.Fatalf("capability feedback drill-down commands missing: %+v", applied.DrillDown)
	}
	text := captureStdout(t, func() { cmdOrientOperator(homePath, []string{"--operator"}) })
	for _, want := range []string{
		"capability_feedback: pending=0 applied_reviews=1 applied_edges=1 edge_kinds=validated_by=1",
		"capability_feedback_review:",
		"capability feedback: compound reviews --area capability-panel-feedback",
		"capability authority: compound capability authority",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("orient output missing %q:\n%s", want, text)
		}
	}
}

func TestOrientOperatorTreatsConsumedSurfacingFeedbackReportTerminal(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 3, 8, 30, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
		t.Fatalf("init home: %v", err)
	}
	reportID := "skill-surfacing-feedback-instruction-design"
	reportPayload := map[string]any{
		"id":             reportID,
		"generated":      now.Add(-45 * time.Minute).Format(time.RFC3339),
		"scanned":        1,
		"skipped":        0,
		"laneFails":      0,
		"remaining":      0,
		"proposals":      []any{},
		"all_misses":     []any{},
		"false_triggers": []any{map[string]any{"skill": "skill:swift-actor-persistence", "count": 1}},
		"perReceipt": []any{map[string]any{
			"dir": "2026-07-01-instruction-design-surfacing",
			"false_triggered": []any{map[string]any{
				"skill":      "skill:swift-actor-persistence",
				"confidence": 1,
				"reason":     "instruction graph hook coherence",
			}},
		}},
	}
	if err := candidates.WriteCandidate(homePath, candidates.CapabilityObs, reportID, reportPayload); err != nil {
		t.Fatalf("write surfacing feedback report: %v", err)
	}
	summary := candidateInboxSummary(homePath, candidates.InboxDir(candidates.CapabilityObs), string(candidates.CapabilityObs), now)
	if summary.ActionableCount != 1 || summary.StateCounts["unspecified"] != 1 {
		t.Fatalf("unconsumed feedback summary = %+v, want one unspecified actionable report", summary)
	}

	reportPath := filepath.Join(homePath, candidates.InboxDir(candidates.CapabilityObs), reportID+".json")
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	sourceCandidateID := "src_cand_surfacing_feedback"
	sourceCandidate := map[string]any{
		"schema_version":        "compound.source_candidate.v0.1",
		"id":                    sourceCandidateID,
		"kind":                  string(candidates.CapabilityObs),
		"subject":               "skill_surfacing_misses:" + hash[:12],
		"statement":             "capability:" + reportPath,
		"detail":                "observed",
		"confidence":            1,
		"source_observation_id": "srcobs_surfacing_feedback",
		"source_ref":            reportPath,
		"promote_allowed":       false,
		"verdict":               candidates.NoPromote,
		"created_at":            now.Format(time.RFC3339),
		"evidence_refs":         []string{"skill_surfacing_misses:" + hash, "source:" + reportPath + "#sha256:" + hash},
	}
	if err := candidates.WriteCandidate(homePath, candidates.CapabilityObs, sourceCandidateID, sourceCandidate); err != nil {
		t.Fatalf("write source no-promote candidate: %v", err)
	}

	summary = candidateInboxSummary(homePath, candidates.InboxDir(candidates.CapabilityObs), string(candidates.CapabilityObs), now)
	if summary.Count != 2 || summary.ActionableCount != 0 || summary.StateCounts[candidates.NoPromote] != 2 {
		t.Fatalf("consumed feedback summary = %+v, want both report and source candidate terminal no_promote", summary)
	}
}

func TestCandidateInboxSummaryNeverCountsCustodyCorruptionAsActionable(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC)
	valid := map[string]any{"id": "zz-valid", "state": "proposed", "created_at": now.Add(-time.Minute).Format(time.RFC3339)}
	if err := candidates.WriteCandidate(homePath, candidates.Skill, "zz-valid", valid); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(homePath, candidates.InboxDir(candidates.Skill))
	if err := os.WriteFile(filepath.Join(dir, "mismatch.json"), []byte(`{"id":"other","state":"proposed"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "linked.json")
	if err := os.WriteFile(outside, []byte(`{"id":"linked","state":"proposed"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scratch.tmp"), []byte("not a canonical candidate"), 0o644); err != nil {
		t.Fatal(err)
	}

	summary := candidateInboxSummary(homePath, candidates.InboxDir(candidates.Skill), string(candidates.Skill), now)
	if summary.Count != 1 || summary.CorruptCount != 3 || summary.ActionableCount != 1 || summary.OldestActionable == nil || summary.OldestActionable.ID != "zz-valid" {
		t.Fatalf("corrupt custody became operator action: %+v", summary)
	}
	warnings := strings.Join(summary.Warnings, "\n")
	for _, name := range []string{"linked.json", "mismatch.json", "scratch.tmp"} {
		if !strings.Contains(warnings, name) {
			t.Fatalf("summary warnings %q do not name corrupt entry %s", warnings, name)
		}
	}
}

func TestOrientOperatorCLIPrintsTriptychAndJSON(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 2, 20, 45, 0, 0, time.UTC)
	seedOrientOperatorHome(t, homePath, now)

	text := captureStdout(t, func() {
		cmdOrient([]string{"--operator", "--recent", "3"})
	})
	for _, want := range []string{"NEEDS-ME", "SESSION-CONTEXT", "RUNNING", "CHANGED", "pending_panel_needs_human=1", "compound orient --harness codex --repo-kind control-plane --task", "DRILL-DOWN", "compound workbench"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text orient output missing %q:\n%s", want, text)
		}
	}

	out := captureStdout(t, func() {
		cmdOrient([]string{"--operator", "--json", "--recent", "3"})
	})
	var report orientOperatorReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse orient operator json: %v\n%s", err, out)
	}
	if report.SchemaVersion != orientOperatorReportSchemaVersion || report.NeedsMe.PendingPanelNeedsHuman != 1 || report.Running.OpenCount != 1 {
		t.Fatalf("json report = %+v", report)
	}
	if report.SessionContext == nil || report.SessionContext.Source != "workbench.open_work" || !stringSliceContains(report.SessionContext.Command, "--task") {
		t.Fatalf("session context = %+v, want open-work context command", report.SessionContext)
	}
	if report.NeedsMe.CandidateActionable != 1 {
		t.Fatalf("json actionable candidate count = %+v", report.NeedsMe)
	}
}

func TestOrientOperatorCarriesAdaptationIntakeProjection(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 5, 9, 30, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
		t.Fatalf("init home: %v", err)
	}

	externalID := "extrepo_orient_adaptation"
	if _, err := reviewartifact.Write(homePath, capgraph.ExternalRepoIntakeReviewArea, externalID, map[string]any{
		"schema_version":          capgraph.ExternalRepoIntakeReviewPlanSchemaVersion,
		"id":                      externalID,
		"status":                  "recorded",
		"created_at":              now.Add(-40 * time.Minute).Format(time.RFC3339),
		"source_kind":             "external-repo-source-list",
		"target_path":             "github.com/Panniantong/Agent-Reach",
		"owner":                   "Distribution / outbound edge",
		"graph_decision_hint":     "reference_only",
		"graph_evidence_strength": 0.81,
		"evidence_refs":           []string{"compound://reviews/external-repo-intake#Agent-Reach"},
		"boundaries":              []string{"no clone", "no install", "no graph mutation"},
		"next_action":             "external repo review recorded; keep as rented-edge evidence",
	}, "  ", "# External Repo\n"); err != nil {
		t.Fatalf("write external repo intake artifact: %v", err)
	}
	if _, err := reviewartifact.WriteJSON(homePath, capgraph.ExternalRepoIntakeReviewArea, externalID+".reviewed.json", map[string]any{
		"schema_version": capgraph.ExternalRepoIntakeReviewReportSchemaVersion,
		"id":             "errr_orient_adaptation",
		"created_at":     now.Add(-35 * time.Minute).Format(time.RFC3339),
		"status":         "recorded",
		"plan_id":        externalID,
		"review_id":      "review_orient_adaptation",
		"repo":           "Panniantong/Agent-Reach",
		"decision":       "reference_only",
		"owner":          "Distribution / outbound edge",
		"next_action":    "keep as reviewed evidence; no graph mutation performed",
		"evidence_refs":  []string{"compound://reviews/external-repo-intake#Agent-Reach"},
	}, "  "); err != nil {
		t.Fatalf("write external repo reviewed sidecar: %v", err)
	}
	if _, err := reviewartifact.Write(homePath, "capability-evaluation-intake", "cep_orient_adaptation", map[string]any{
		"schema_version": "compound.capability.evaluation_review_report.v0.1",
		"id":             "cep_orient_adaptation",
		"status":         "recorded",
		"created_at":     now.Add(-25 * time.Minute).Format(time.RFC3339),
		"source_path":    "reviews/external-repo-intake/extrepo_orient_adaptation.reviewed.json",
		"owner":          "Distribution / outbound edge",
		"evidence_refs":  []string{"capability_evaluation:agent-reach-context-fit"},
		"next_action":    "capability evaluation review recorded; graph mutation requires separate import",
	}, "  ", "# Capability Evaluation\n"); err != nil {
		t.Fatalf("write capability evaluation artifact: %v", err)
	}

	report, err := buildOrientOperatorReport(homePath, 5, now, "")
	if err != nil {
		t.Fatalf("orient operator report: %v", err)
	}
	intake := report.NeedsMe.AdaptationIntake
	if intake == nil {
		t.Fatalf("adaptation intake projection missing: %+v", report.NeedsMe)
	}
	if intake.ExternalRepoCount != 1 || intake.ExternalRepoReviewedCount != 1 || intake.ExternalRepoOpenCount != 0 {
		t.Fatalf("external repo adaptation counts = %+v, want reviewed without open debt", intake)
	}
	if intake.CapabilityEvaluationCount != 1 || intake.CapabilityEvaluationReviewedCount != 1 || intake.CapabilityEvaluationOpenCount != 0 {
		t.Fatalf("capability evaluation adaptation counts = %+v, want reviewed without open debt", intake)
	}
	if intake.ActionableCount != 0 || !strings.Contains(intake.NextAction, "already reviewed/recorded") {
		t.Fatalf("adaptation next action = %+v, want reviewed receipts not fresh action", intake)
	}
	if !orientHasDrillDown(report, "external repo intake") || !orientHasDrillDown(report, "capability evaluation intake") {
		t.Fatalf("adaptation drill-down commands missing: %+v", report.DrillDown)
	}
	if len(intake.Recent) == 0 || intake.Recent[0].TargetPath == "" {
		t.Fatalf("adaptation recent rows should carry target evidence: %+v", intake.Recent)
	}
	text := captureStdout(t, func() {
		cmdOrientOperator(homePath, []string{"--operator"})
	})
	for _, want := range []string{
		"adaptation_intake: external_repos=1 open=0 reviewed=1 capability_evaluations=1 open=0 reviewed=1 actionable=0",
		"adaptation_item: external-repo-intake/extrepo_orient_adaptation",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("orient output missing %q:\n%s", want, text)
		}
	}
}

func TestOrientOperatorCarriesSourceControlReviewProjection(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 5, 10, 5, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
		t.Fatalf("init home: %v", err)
	}
	repo := initCompoundCommandGitRepo(t)
	t.Chdir(repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scratch.txt"), []byte("scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := authority.ReadGitSnapshot(repo)
	if err != nil {
		t.Fatalf("read source-control snapshot: %v", err)
	}
	plan, err := authority.BuildSourceControlReviewPlan(snap, "work_source_control", now.Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("build source-control review plan: %v", err)
	}
	paths, err := authority.RecordSourceControlReviewPlan(homePath, plan)
	if err != nil {
		t.Fatalf("record source-control review plan: %v", err)
	}
	review := authority.SourceControlReview{
		SchemaVersion: authority.SourceControlReviewSchemaVersion,
		ID:            plan.ID,
		PlanID:        plan.ID,
		RepoID:        plan.RepoID,
		Decision:      "parked",
		Rationale:     "dirty source-control state reviewed; leave scratch parked before any git mutation",
		Confidence:    0.88,
		NextAction:    "keep scratch parked and continue scoped tracked work only",
		EvidenceRefs:  append([]string{}, plan.EvidenceRefs...),
	}
	if _, err := authority.RecordSourceControlReview(homePath, review, paths.ReviewYAMLPath, now.Add(-20*time.Minute)); err != nil {
		t.Fatalf("record source-control review: %v", err)
	}

	report, err := buildOrientOperatorReport(homePath, 5, now, "")
	if err != nil {
		t.Fatalf("orient operator report: %v", err)
	}
	source := report.NeedsMe.SourceControl
	if source == nil {
		t.Fatalf("source-control projection missing: %+v", report.NeedsMe)
	}
	if source.LiveStatus != "dirty" || source.ChangedPaths != 2 || source.Unstaged != 1 || source.Untracked != 1 {
		t.Fatalf("source-control live snapshot = %+v, want dirty 1 unstaged 1 untracked", source)
	}
	if source.ReviewCount != 1 || source.ReviewRecordedCount != 1 || source.ReviewOpenCount != 0 || !source.HeadReviewRecorded {
		t.Fatalf("source-control review counts = %+v, want one recorded current-head review", source)
	}
	if !strings.Contains(source.NextAction, "inspect cleanup receipts") {
		t.Fatalf("source-control next action = %q, want receipt-first guidance", source.NextAction)
	}
	if !orientHasDrillDown(report, "source-control cleanup reviews") || !orientHasDrillDown(report, "source-control snapshot") {
		t.Fatalf("source-control drill-down commands missing: %+v", report.DrillDown)
	}
	text := captureStdout(t, func() {
		cmdOrientOperator(homePath, []string{"--operator"})
	})
	for _, want := range []string{
		"source_control: live=dirty changed=2 staged=0 unstaged=1 untracked=1 reviews=1 open=0 recorded=1 head_review_recorded=true",
		"source_control_review: " + plan.ID,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("orient output missing %q:\n%s", want, text)
		}
	}
}

func TestOrientOperatorTreatsTamperedMutationAuthorizationAsOpen(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 13, 14, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
		t.Fatalf("init home: %v", err)
	}
	repo := initCompoundCommandGitRepo(t)
	t.Chdir(repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := authority.ReadGitSnapshot(repo)
	if err != nil {
		t.Fatalf("read source-control snapshot: %v", err)
	}
	binding := authority.SourceControlMutationBinding{
		ActionWorkID:      "work_orient_tampered_authorization",
		MutationInputHash: "mutation-input:sha256:" + strings.Repeat("a", 64),
		Capability:        "filesystem.write",
		Action:            "write_file",
		MutationClass:     "local_reversible",
		TargetPaths:       []string{"README.md"},
		ActionIntentIDs:   []string{"act_orient_tampered_authorization"},
	}
	plan, err := authority.BuildSourceControlMutationReviewPlan(snapshot, binding.ActionWorkID, binding, now.Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("build source-control mutation review plan: %v", err)
	}
	paths, err := authority.RecordSourceControlReviewPlan(homePath, plan)
	if err != nil {
		t.Fatalf("record source-control mutation review plan: %v", err)
	}
	report, err := authority.RecordSourceControlReview(homePath, authority.SourceControlReview{
		SchemaVersion: authority.SourceControlReviewSchemaVersion,
		ID:            plan.ReviewDraft.ReviewID,
		PlanID:        plan.ID,
		RepoID:        plan.RepoID,
		Decision:      "authorize_mutation",
		Rationale:     "operator reviewed the exact dirty mutation",
		Confidence:    1,
		EvidenceRefs:  append([]string{}, plan.EvidenceRefs...),
		ReviewedBy:    "operator:test",
	}, paths.ReviewYAMLPath, now.Add(-20*time.Minute))
	if err != nil {
		t.Fatalf("record source-control mutation authorization: %v", err)
	}
	report.NextAction = "tampered after authorization was sealed"
	writeOrientJSONFixture(t, report.ReviewPath, report)

	orient, err := buildOrientOperatorReport(homePath, 5, now, "")
	if err != nil {
		t.Fatalf("orient operator report: %v", err)
	}
	source := orient.NeedsMe.SourceControl
	if source == nil {
		t.Fatalf("source-control projection missing: %+v", orient.NeedsMe)
	}
	if source.ReviewCount != 1 || source.ReviewRecordedCount != 0 || source.ReviewOpenCount != 1 || source.HeadReviewRecorded {
		t.Fatalf("tampered authorization projection = %+v, want one open and zero recorded", source)
	}
	if !strings.Contains(source.NextAction, "review is open") {
		t.Fatalf("tampered authorization next action = %q, want open review guidance", source.NextAction)
	}
}

func TestOrientOperatorCarriesProductLoopEvidenceDebtProjection(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 4, 11, 30, 0, 0, time.UTC)
	seedOrientProductLoopProjectionHome(t, homePath, now)

	report, err := buildOrientOperatorReport(homePath, 5, now, "")
	if err != nil {
		t.Fatalf("orient operator report: %v", err)
	}
	if report.ProductLoops == nil {
		t.Fatalf("product loop projection missing from orient report: %+v", report)
	}
	open := findOrientProductLoop(t, report.ProductLoops.Products, "product:orient-open")
	if open.OpenHypothesisDebtCount != 1 {
		t.Fatalf("open product loop debt = %+v, want one open debt", open)
	}
	if group := findOrientMissingSignalSource(open.OpenHypothesesByMissingSignalSource, "posthog"); len(group.HypothesisIDs) != 1 || group.HypothesisIDs[0] != "hyp_orient_open_activation" {
		t.Fatalf("missing signal group = %+v, want posthog hypothesis debt", group)
	}
	if card := findOrientLoopActionCard(open.ActionCards, "product_signal_import"); card.ID == "" || !containsString(card.Command, "signal") {
		t.Fatalf("open product loop should carry product-signal import action card: %+v", open.ActionCards)
	}
	closed := findOrientProductLoop(t, report.ProductLoops.Products, "product:orient-closed")
	if group := findOrientReviewNextAction(closed.ClosedReviewsByNextAction, "keep screenshot one more window"); len(group.ReviewIDs) != 1 || group.ReviewIDs[0] != "review_orient_conversion" {
		t.Fatalf("closed review next action group = %+v, want review follow-up", group)
	}
	if card := findOrientLoopActionCard(closed.ActionCards, "product_next_bounded_move"); card.ID == "" || !containsString(card.Command, "action-plan") || !containsString(card.Command, "--card") || !containsString(card.Command, card.ID) {
		t.Fatalf("closed product loop should carry bounded next-move action card: %+v", closed.ActionCards)
	}
	if gap := findOrientSignalHypothesisGap(closed.SignalsWithoutHypothesis, "Observed store search traffic."); len(gap.SignalIDs) != 1 || gap.SignalIDs[0] != "sig_orient_unmatched_store" || len(gap.Metrics) != 1 || gap.Metrics[0] != "app_store_search_impressions" {
		t.Fatalf("orient should carry signal-without-hypothesis group: %+v", closed.SignalsWithoutHypothesis)
	}
	if card := findOrientLoopActionCard(closed.ActionCards, "product_signal_hypothesis_review"); card.ID == "" || !containsString(card.Command, "action-plan") || !containsString(card.Command, "--card") || !containsString(card.Command, card.ID) {
		t.Fatalf("closed product loop should carry signal-hypothesis review card: %+v", closed.ActionCards)
	}

	text := captureStdout(t, func() {
		printOrientOperatorReport(report)
	})
	for _, want := range []string{
		"product_loops:",
		"action_cards=",
		"product:orient-open",
		"missing_signal_source=posthog",
		"product:orient-closed",
		"signals_without_hypothesis=Observed store search traffic.",
		"closed_review_next=keep screenshot one more window",
		"compound product action-plan",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text orient output missing %q:\n%s", want, text)
		}
	}
}

func TestOrientOperatorSurfacesHeldValveAdjacentProgress(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 4, 9, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
		t.Fatalf("init home: %v", err)
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	asc, err := workStore.Create(work.CreateInput{
		Owner:            "codex",
		SuccessPredicate: "held ASC private config credential scheduler valve",
		State:            "held",
	}, now.Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("create asc held work: %v", err)
	}
	conversation, err := workStore.Create(work.CreateInput{
		Owner:            "codex",
		SuccessPredicate: "held ChatGPT conversation import promotion valve",
		State:            "held",
	}, now.Add(-25*time.Minute))
	if err != nil {
		t.Fatalf("create conversation held work: %v", err)
	}
	report, err := buildOrientOperatorReport(homePath, 5, now, "")
	if err != nil {
		t.Fatalf("orient operator report: %v", err)
	}
	adjacent := report.Running.AdjacentProgress
	if adjacent == nil || !adjacent.Available || adjacent.NonPacketActionCount == 0 {
		t.Fatalf("orient operator missing adjacent progress for held valves: %+v", adjacent)
	}
	if !stringSliceContains(adjacent.HeldWorkIDs, asc.ID) || !stringSliceContains(adjacent.HeldWorkIDs, conversation.ID) {
		t.Fatalf("held work ids not carried into orient adjacent progress: %+v", adjacent)
	}
	for _, id := range []string{"operator-gate-" + asc.ID + "-app-store-connect-private-config", "asc-sales-series-bridge-proof", "conversation-preflight-private-export", "conversation-memory-review-gate", "second-opinion-evidence-recovery", "codex-pretooluse-native-gate-proof"} {
		if !stringSliceContains(adjacent.NonPacketActionIDs, id) {
			t.Fatalf("adjacent action %s missing from orient operator: %+v", id, adjacent)
		}
	}
	if report.SessionContext == nil || report.SessionContext.Source != "workbench.adjacent_progress" {
		t.Fatalf("session context = %+v, want held-valve adjacent-progress source", report.SessionContext)
	}
	if !strings.Contains(report.SessionContext.Task, "held-valve-adjacent") || !strings.Contains(strings.Join(report.SessionContext.Command, " "), "--task continue held-valve-adjacent") {
		t.Fatalf("session context command = %+v, want adjacent context task", report.SessionContext)
	}
	if !strings.Contains(report.SessionContext.Boundary, "without claiming") || !strings.Contains(report.SessionContext.Proof, "ContextPack") {
		t.Fatalf("session context boundary/proof = %+v", report.SessionContext)
	}
	nextFor := func(workID string) string {
		t.Helper()
		for _, item := range report.Running.OpenWork {
			if item.WorkID == workID {
				return item.NextAction
			}
		}
		t.Fatalf("open work %s missing from orient report: %+v", workID, report.Running.OpenWork)
		return ""
	}
	ascNext := nextFor(asc.ID)
	if !strings.Contains(ascNext, "needs_me.operator_gate_cards") || !strings.Contains(ascNext, "operator-gate-"+asc.ID+"-app-store-connect-private-config") || strings.Contains(ascNext, "resolve policy/source-control hold") {
		t.Fatalf("ASC held next action should point at adjacent preflight, got %q", ascNext)
	}
	conversationNext := nextFor(conversation.ID)
	if !strings.Contains(conversationNext, "running.adjacent_progress") ||
		!strings.Contains(conversationNext, "conversation-preflight-private-export") ||
		!strings.Contains(conversationNext, "conversation-memory-review-gate") ||
		strings.Contains(conversationNext, "resolve policy/source-control hold") {
		t.Fatalf("conversation held next action should point at adjacent preflight/review gate, got %q", conversationNext)
	}
	text := captureStdout(t, func() {
		cmdOrient([]string{"--operator", "--recent", "3"})
	})
	if !strings.Contains(text, "SESSION-CONTEXT") || !strings.Contains(text, "source=workbench.adjacent_progress") || !strings.Contains(text, "compound orient --harness codex --repo-kind control-plane --task continue held-valve-adjacent") {
		t.Fatalf("text orient output missing session context:\n%s", text)
	}
	if !strings.Contains(text, "adjacent_progress: available=true") ||
		!strings.Contains(text, "non_packet_actions=") ||
		!strings.Contains(text, "operator_gate_cards: 1") ||
		!strings.Contains(text, "source=app_store_connect") ||
		!strings.Contains(text, "needs_me.operator_gate_cards") {
		t.Fatalf("text orient output missing adjacent progress:\n%s", text)
	}
}

func TestOrientOperatorSuppressesResolvedDispatchStepRun(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	// Hermetic capacity guard: the routeevidence init() adapter reads the OPERATOR's real
	// CodexBar/quota files, so this test's dispatch outcome would depend on the machine's live
	// lane state (observed 2026-07-13: a genuinely walled codex 5h window refused the dispatch
	// and emptied the wait reason). Nil restores pure work-package behavior for the test.
	previousGuard := work.LaneCapacityGuard
	work.LaneCapacityGuard = nil
	t.Cleanup(func() { work.LaneCapacityGuard = previousGuard })
	now := time.Date(2026, 7, 2, 21, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
		t.Fatalf("init home: %v", err)
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	ready, err := workStore.Create(work.CreateInput{Owner: "codex", SuccessPredicate: "resolved dispatch wait"}, now.Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	run, err := workStore.DispatchStepRun(now.Add(-25*time.Minute), work.DispatchStepRunOptions{
		DispatchStepOptions: work.DispatchStepOptions{Actor: "codex"},
		MaxSteps:            5,
	})
	if err != nil {
		t.Fatalf("dispatch step run: %v", err)
	}
	if run.WaitReason != "run_evidence" {
		t.Fatalf("dispatch run wait reason = %q, want run_evidence", run.WaitReason)
	}
	if _, err := workStore.Advance(ready.ID, work.AdvanceInput{To: "verifying", VerificationID: "ver_orient_resolved_dispatch"}, now.Add(-20*time.Minute)); err != nil {
		t.Fatalf("advance verifying: %v", err)
	}
	if _, err := workStore.Close(ready.ID, work.CloseInput{
		Outcome:  "completed",
		MoveType: "outward_delta",
		Summary:  "resolved dispatch wait",
		Changed:  true,
	}, now.Add(-19*time.Minute)); err != nil {
		t.Fatalf("close work: %v", err)
	}

	report, err := buildOrientOperatorReport(homePath, 5, now, "")
	if err != nil {
		t.Fatalf("orient operator report: %v", err)
	}
	if report.Running.OpenCount != 0 || report.Running.LastDispatchStepRun != nil {
		t.Fatalf("running should not surface resolved dispatch history: %+v", report.Running)
	}
}

func findOrientCandidateInbox(t *testing.T, report orientOperatorReport, kind string) orientCandidateInbox {
	t.Helper()
	for _, inbox := range report.NeedsMe.CandidateInboxes {
		if inbox.Kind == kind {
			return inbox
		}
	}
	t.Fatalf("candidate inbox %q missing: %+v", kind, report.NeedsMe.CandidateInboxes)
	return orientCandidateInbox{}
}

func seedOrientOperatorHome(t *testing.T, homePath string, now time.Time) {
	t.Helper()
	if err := home.Init(homePath, "test", now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("init home: %v", err)
	}
	memStore, err := mem.Open(homePath)
	if err != nil {
		t.Fatalf("open memory: %v", err)
	}
	candidatesOut, err := memStore.Capture([]mem.Candidate{{
		ID:              "cand_orient_seq",
		Entity:          "compound",
		Predicate:       "has_operator_orient_fixture",
		Object:          "true",
		ObservationDate: now.Add(-90 * time.Minute).Format(time.RFC3339),
		Confidence:      0.91,
		Derivation:      "deterministic",
		ClaimType:       "status",
		PromotionBasis:  candidates.BasisRuntimeProof,
		SourceTraceRef:  "trace://orient-operator-fixture",
		SnippetHash:     "sha256:orientoperator",
		Visibility:      "contextual",
		CreatedAt:       now.Add(-90 * time.Minute).Format(time.RFC3339),
	}}, now.Add(-90*time.Minute))
	if err != nil {
		t.Fatalf("capture memory candidate: %v", err)
	}
	if _, err := memStore.Adjudicate(candidatesOut[0].ID, candidates.Promote, "work_orient_fixture", now.Add(-80*time.Minute)); err != nil {
		t.Fatalf("promote memory candidate: %v", err)
	}
	if err := candidates.WriteCandidate(homePath, candidates.Skill, "skill_orient_fixture", map[string]any{
		"id":         "skill_orient_fixture",
		"kind":       "skill",
		"created_at": now.Add(-70 * time.Minute).Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("write skill candidate: %v", err)
	}
	oldCandidatePath := filepath.Join(homePath, candidates.InboxDir(candidates.Skill), "skill_orient_fixture.json")
	if err := os.Chtimes(oldCandidatePath, now.Add(-70*time.Minute), now.Add(-70*time.Minute)); err != nil {
		t.Fatalf("chtimes skill candidate: %v", err)
	}
	if err := candidates.WriteCandidate(homePath, candidates.CapabilityObs, "capability_orient_no_promote", map[string]any{
		"id":              "capability_orient_no_promote",
		"kind":            "capability_observation",
		"created_at":      now.Add(-68 * time.Minute).Format(time.RFC3339),
		"verdict":         candidates.NoPromote,
		"promote_allowed": false,
	}); err != nil {
		t.Fatalf("write capability candidate: %v", err)
	}
	if err := os.Chtimes(filepath.Join(homePath, candidates.InboxDir(candidates.CapabilityObs), "capability_orient_no_promote.json"), now.Add(-68*time.Minute), now.Add(-68*time.Minute)); err != nil {
		t.Fatalf("chtimes capability candidate: %v", err)
	}
	if err := candidates.WriteCandidate(homePath, candidates.MethodUpgrade, "method_orient_parked", map[string]any{
		"id":         "method_orient_parked",
		"kind":       "method_upgrade",
		"created_at": now.Add(-67 * time.Minute).Format(time.RFC3339),
		"verdict":    candidates.Park,
	}); err != nil {
		t.Fatalf("write method candidate: %v", err)
	}
	if err := os.Chtimes(filepath.Join(homePath, candidates.InboxDir(candidates.MethodUpgrade), "method_orient_parked.json"), now.Add(-67*time.Minute), now.Add(-67*time.Minute)); err != nil {
		t.Fatalf("chtimes method candidate: %v", err)
	}
	if _, err := reviewartifact.Write(homePath, "panel-verdicts", "panel_verdict_orient_needs_human", map[string]any{
		"schema_version": "compound.panel_verdict.v0.1",
		"id":             "panel_verdict_orient_needs_human",
		"created_at":     now.Add(-65 * time.Minute).Format(time.RFC3339),
		"run_id":         "prun_orient_needs_human",
		"target": map[string]any{
			"kind":    "memory_candidate",
			"ref":     "cand_orient_seq",
			"summary": "candidate needs operator decision",
		},
		"lens_findings": []map[string]any{{
			"lens_ref":           "lens:operator",
			"route_evidence_ref": "route_orient_operator",
			"status":             "failed",
		}},
		"synthesis": map[string]any{
			"schema_version": "compound.panel_synthesis.v0.1",
			"decision":       "needs_human",
			"confidence":     0.72,
			"rationale":      "operator must decide",
		},
		"evidence_refs": []string{"route_orient_operator"},
	}, " ", "# Panel verdict\n"); err != nil {
		t.Fatalf("write panel verdict: %v", err)
	}

	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	running, err := workStore.Create(work.CreateInput{Owner: "codex", SuccessPredicate: "operator orient running proof"}, now.Add(-50*time.Minute))
	if err != nil {
		t.Fatalf("create running work: %v", err)
	}
	if _, err := workStore.Claim(running.ID, "codex", now.Add(-45*time.Minute)); err != nil {
		t.Fatalf("claim running work: %v", err)
	}
	orientRunningRunAttempt, err := runattempt.Open(homePath).Mint(running.ID, "codex", "codex", now.Add(-40*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workStore.Advance(running.ID, work.AdvanceInput{To: "running", RunAttemptIDs: []string{orientRunningRunAttempt.ID}}, now.Add(-40*time.Minute)); err != nil {
		t.Fatalf("advance running work: %v", err)
	}
	done, err := workStore.Create(work.CreateInput{Owner: "codex", SuccessPredicate: "operator orient closed proof"}, now.Add(-35*time.Minute))
	if err != nil {
		t.Fatalf("create done work: %v", err)
	}
	if _, err := workStore.Claim(done.ID, "codex", now.Add(-34*time.Minute)); err != nil {
		t.Fatalf("claim done work: %v", err)
	}
	orientDoneRunAttempt, err := runattempt.Open(homePath).Mint(done.ID, "codex", "codex", now.Add(-33*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workStore.Advance(done.ID, work.AdvanceInput{To: "running", RunAttemptIDs: []string{orientDoneRunAttempt.ID}}, now.Add(-33*time.Minute)); err != nil {
		t.Fatalf("advance done running: %v", err)
	}
	if _, err := workStore.Advance(done.ID, work.AdvanceInput{To: "verifying", VerificationID: "ver_orient_done"}, now.Add(-32*time.Minute)); err != nil {
		t.Fatalf("advance done verifying: %v", err)
	}
	if _, err := workStore.Close(done.ID, work.CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Actor:     "codex",
		Summary:   "operator orientation proof closed",
		Changed:   true,
		ProofRefs: []string{"ver_orient_done"},
	}, now.Add(-31*time.Minute)); err != nil {
		t.Fatalf("close done work: %v", err)
	}
	if _, err := workStore.DispatchStepRun(now.Add(-30*time.Minute), work.DispatchStepRunOptions{MaxSteps: 1, DispatchStepOptions: work.DispatchStepOptions{Actor: "codex"}}); err != nil {
		t.Fatalf("dispatch step run: %v", err)
	}
}

func seedOrientProductLoopProjectionHome(t *testing.T, homePath string, now time.Time) {
	t.Helper()
	if err := home.Init(homePath, "test", now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatalf("open memory: %v", err)
	}
	openBundle := product.Bundle{
		SchemaVersion: product.SchemaVersion,
		Product: product.Product{
			ID:             "product:orient-open",
			Name:           "Orient Open",
			OwnerEntity:    "project:orient-open",
			Surfaces:       []product.Surface{{ID: "landing", Kind: "web", Ref: "https://example.invalid/orient-open"}},
			Channels:       []string{"landing_page"},
			Positioning:    "Operator orient open product-loop debt fixture.",
			PrivacyPosture: "fixture",
		},
		Hypotheses: []product.Hypothesis{{
			ID:                "hyp_orient_open_activation",
			ProductID:         "product:orient-open",
			ChangeEvent:       "Changed onboarding headline.",
			ExpectedEffect:    "Activation improves.",
			Metrics:           []string{"posthog_activation_rate"},
			ObservationWindow: "7d after change",
			Result:            "unknown",
			NextAction:        "wait for posthog activation export",
			EvidenceRefs:      []string{"fixture:orient-open-headline"},
		}},
		EvidenceRefs: []string{"fixture:orient-open-product"},
	}
	if _, err := product.Apply(homePath, store, openBundle, "fixture/orient-open-product.yaml", now.Add(-90*time.Minute)); err != nil {
		t.Fatalf("apply open product bundle: %v", err)
	}

	closedBundle := product.Bundle{
		SchemaVersion: product.SchemaVersion,
		Product: product.Product{
			ID:             "product:orient-closed",
			Name:           "Orient Closed",
			OwnerEntity:    "project:orient-closed",
			Surfaces:       []product.Surface{{ID: "app-store-page", Kind: "app_store", Ref: "appstore:987654321"}},
			Channels:       []string{"app_store"},
			Positioning:    "Operator orient closed product-loop review fixture.",
			PrivacyPosture: "fixture",
		},
		Signals: []product.Signal{{
			ID:           "sig_orient_conversion",
			ProductID:    "product:orient-closed",
			Surface:      "app-store-page",
			ChangeEvent:  "Changed first screenshot.",
			MetricName:   "app_store_product_page_conversion",
			Before:       0.041,
			After:        0.052,
			Window:       "7d after change",
			Confidence:   0.62,
			EvidenceRefs: []string{"fixture:orient-conversion-signal"},
		}, {
			ID:           "sig_orient_unmatched_store",
			ProductID:    "product:orient-closed",
			Surface:      "app-store-page",
			ChangeEvent:  "Observed store search traffic.",
			MetricName:   "app_store_search_impressions",
			Before:       80,
			After:        120,
			Window:       "7d after search change",
			Confidence:   0.58,
			EvidenceRefs: []string{"fixture:orient-unmatched-store-signal"},
		}},
		Hypotheses: []product.Hypothesis{{
			ID:                "hyp_orient_conversion",
			ProductID:         "product:orient-closed",
			ChangeEvent:       "Changed first screenshot.",
			ExpectedEffect:    "Conversion improves.",
			Metrics:           []string{"app_store_product_page_conversion"},
			ObservationWindow: "7d after change",
			Result:            "unknown",
			NextAction:        "review conversion evidence",
			EvidenceRefs:      []string{"fixture:orient-conversion-hypothesis"},
		}},
		EvidenceRefs: []string{"fixture:orient-closed-product"},
	}
	if _, err := product.Apply(homePath, store, closedBundle, "fixture/orient-closed-product.yaml", now.Add(-80*time.Minute)); err != nil {
		t.Fatalf("apply closed product bundle: %v", err)
	}
	review := product.Review{
		SchemaVersion: product.ReviewSchemaVersion,
		ID:            "review_orient_conversion",
		ProductID:     "product:orient-closed",
		HypothesisID:  "hyp_orient_conversion",
		Result:        "inconclusive",
		Rationale:     "Conversion moved but sample is still too small.",
		Confidence:    0.61,
		NextAction:    "keep screenshot one more window",
		ReviewedBy:    "operator",
		ReviewedAt:    now.Add(-30 * time.Minute).Format(time.RFC3339),
		EvidenceRefs:  []string{"product_signal:sig_orient_conversion", "fixture:orient-review"},
	}
	if _, err := product.ApplyReview(homePath, store, review, "fixture/orient-conversion-review.yaml", now.Add(-20*time.Minute)); err != nil {
		t.Fatalf("apply product review: %v", err)
	}
}

func TestOrientOperatorSurfacesMemoryReviewCardsFromHeldRunArtifacts(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 5, 2, 10, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	held, err := store.Create(work.CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "held chatgpt conversation memory has reviewed parked candidate",
		State:            "held",
	}, now.Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("create held work: %v", err)
	}
	if err := candidates.WriteCandidate(homePath, candidates.Memory, "cand_orient_conversation_review", map[string]any{
		"id":     "cand_orient_conversation_review",
		"state":  "parked",
		"entity": "knowledge:chatgpt:orient-fixture",
	}); err != nil {
		t.Fatalf("write memory candidate: %v", err)
	}
	runDir := filepath.Join(homePath, "runs", held.ID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeOrientJSONFixture(t, filepath.Join(runDir, "panel-run-single-prun_orient_fixture.json"), map[string]any{
		"schema_version": "compound.panel_run.v0.1",
		"id":             "prun_orient_fixture",
		"status":         "applied",
		"target_kind":    "memory_candidate",
		"targets": []map[string]any{{
			"ref": "cand_orient_conversation_review",
		}},
	})
	writeOrientJSONFixture(t, filepath.Join(runDir, "promoted-chatgpt-claims.json"), map[string]any{
		"count": 0,
		"refs":  []string{},
	})

	report, err := buildOrientOperatorReport(homePath, 5, now, "")
	if err != nil {
		t.Fatalf("orient report: %v", err)
	}
	if report.NeedsMe.MemoryReviewCardCount != 1 || len(report.NeedsMe.MemoryReviewCards) != 1 {
		t.Fatalf("memory review cards = %+v", report.NeedsMe)
	}
	card := report.NeedsMe.MemoryReviewCards[0]
	if card.WorkID != held.ID ||
		card.CandidateID != "cand_orient_conversation_review" ||
		card.CandidateState != "parked" ||
		card.PanelRunID != "prun_orient_fixture" ||
		card.PromotionCount == nil ||
		*card.PromotionCount != 0 ||
		!strings.Contains(card.NextAction, "cand_orient_conversation_review") {
		t.Fatalf("orient memory review card should name exact parked candidate and zero promotion: %+v", card)
	}
	var openWork orientOpenWorkItem
	for _, item := range report.Running.OpenWork {
		if item.WorkID == held.ID {
			openWork = item
			break
		}
	}
	if openWork.WorkID == "" ||
		!strings.Contains(openWork.NextAction, card.ID) ||
		!strings.Contains(openWork.NextAction, "needs_me.memory_review_cards") {
		t.Fatalf("open work next action should point exact memory review card, got %+v", openWork)
	}
	text := captureStdout(t, func() {
		printOrientOperatorReport(report)
	})
	if !strings.Contains(text, "memory_review_cards: 1") ||
		!strings.Contains(text, "candidate=cand_orient_conversation_review") ||
		!strings.Contains(text, "promoted=0") {
		t.Fatalf("orient text missing memory review card:\n%s", text)
	}
}

func TestOrientOperatorSurfacesWorkbenchCandidateIntegrity(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 5, 2, 10, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	held, err := store.Create(work.CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "operator orient names corrupt candidate evidence",
		State:            "held",
	}, now.Add(-30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(homePath, "runs", held.ID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeOrientJSONFixture(t, filepath.Join(runDir, "panel-run-corrupt.json"), map[string]any{
		"id":          "prun_corrupt",
		"status":      "applied",
		"target_kind": "memory_candidate",
		"targets":     []map[string]any{{"ref": "cand_bad/../escape"}},
	})

	report, err := buildOrientOperatorReport(homePath, 5, now, "")
	if err != nil {
		t.Fatal(err)
	}
	warnings := strings.Join(report.Warnings, "\n")
	if !strings.Contains(warnings, held.ID+": memory candidate artifact corrupt") {
		t.Fatalf("operator orient warnings dropped Workbench integrity: %v", report.Warnings)
	}
	text := captureStdout(t, func() { printOrientOperatorReport(report) })
	if !strings.Contains(text, "WARNINGS") || !strings.Contains(text, "memory candidate artifact corrupt") {
		t.Fatalf("operator orient text dropped candidate integrity:\n%s", text)
	}
}

func TestOrientOperatorSurfacesASCGateCardsForHeldWork(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 5, 2, 30, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	held, err := store.Create(work.CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "held app-store-connect ASC daily signals need private mapping config before scheduler",
		State:            "held",
	}, now.Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("create held work: %v", err)
	}

	report, err := buildOrientOperatorReport(homePath, 5, now, "")
	if err != nil {
		t.Fatalf("orient report: %v", err)
	}
	if report.NeedsMe.OperatorGateCardCount != 1 || len(report.NeedsMe.OperatorGateCards) != 1 {
		t.Fatalf("operator gate cards = %+v", report.NeedsMe)
	}
	card := report.NeedsMe.OperatorGateCards[0]
	if card.WorkID != held.ID ||
		card.Kind != "source_private_config_gate" ||
		card.SourceKind != "app_store_connect" ||
		card.GateID != "asc-private-config" ||
		!strings.Contains(strings.Join(card.Command, " "), "tools/asc_preflight.ts") ||
		!strings.Contains(strings.Join(card.StopConditions, "\n"), "requested action would fetch live ASC data, apply ProductSignal, load scheduler, or clear held work") {
		t.Fatalf("orient ASC gate card should expose exact read-only private-config valve: %+v", card)
	}
	var openWork orientOpenWorkItem
	for _, item := range report.Running.OpenWork {
		if item.WorkID == held.ID {
			openWork = item
			break
		}
	}
	if openWork.WorkID == "" ||
		!strings.Contains(openWork.NextAction, card.ID) ||
		!strings.Contains(openWork.NextAction, "needs_me.operator_gate_cards") {
		t.Fatalf("open work next action should point exact ASC gate card, got %+v", openWork)
	}
	text := captureStdout(t, func() {
		printOrientOperatorReport(report)
	})
	if !strings.Contains(text, "operator_gate_cards: 1") ||
		!strings.Contains(text, "source=app_store_connect") ||
		!strings.Contains(text, "gate=asc-private-config") {
		t.Fatalf("orient text missing ASC operator gate card:\n%s", text)
	}
}

func TestOrientOperatorSurfacesAdmissionPreviewReviewCardContext(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Date(2026, 7, 5, 2, 45, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	held, err := store.Create(work.CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "held app-store-connect ASC daily signals need private mapping config scheduler",
		State:            "held",
	}, now.Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("create held work: %v", err)
	}
	cardID := "operator-gate-" + held.ID + "-app-store-connect-private-config"
	plan, err := store.BuildReviewCardWorkPlan(cardID, now.Add(-5*time.Minute), work.ReviewCardWorkPlanOptions{Actor: "codex"})
	if err != nil {
		t.Fatalf("review-card work plan: %v", err)
	}

	report, err := buildOrientOperatorReport(homePath, 5, now, "")
	if err != nil {
		t.Fatalf("orient operator report: %v", err)
	}
	if report.Running.AdmissionPreviewCount != 1 || len(report.Running.AdmissionPreviews) != 1 {
		t.Fatalf("admission preview missing from orient running: %+v", report.Running)
	}
	preview := report.Running.AdmissionPreviews[0]
	if preview.WorkID != plan.WorkPlan.WorkID ||
		preview.SourceReviewCardID != cardID ||
		preview.Staleness != "fresh" ||
		preview.ActionContext == nil ||
		preview.ActionContext.Kind != "workbench_review_card" ||
		preview.ActionContext.SourceID != cardID ||
		!preview.ActionContext.RequiresOperator ||
		!stringSliceContains(preview.ActionContext.StopConditions, "requested action would fetch live ASC data, apply ProductSignal, load scheduler, or clear held work") ||
		!preview.RequiresOperator ||
		preview.AdmissionBlockedReason == "" ||
		len(preview.AdmitCommand) != 0 {
		t.Fatalf("orient admission preview did not preserve review-card context: %+v", preview)
	}
	source, err := store.Get(held.ID)
	if err != nil {
		t.Fatal(err)
	}
	if source.State != "held" {
		t.Fatalf("review-card preview should not clear source held work: %s", source.State)
	}
	text := captureStdout(t, func() {
		printOrientOperatorReport(report)
	})
	if !strings.Contains(text, "admission_previews: 1") ||
		!strings.Contains(text, "action_context=workbench_review_card") ||
		!strings.Contains(text, "source_review_card="+cardID) ||
		!strings.Contains(text, "operator_required=true") ||
		!strings.Contains(text, "blocked_reason: operator required inputs") ||
		strings.Contains(text, "admit=compound work admit-plan --plan") {
		t.Fatalf("orient text missing review-card admission preview:\n%s", text)
	}
}

func writeOrientJSONFixture(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func findOrientProductLoop(t *testing.T, products []orientProductLoopItem, id string) orientProductLoopItem {
	t.Helper()
	for _, product := range products {
		if product.ProductID == id {
			return product
		}
	}
	t.Fatalf("product loop %s missing: %+v", id, products)
	return orientProductLoopItem{}
}

func findOrientMissingSignalSource(groups []orientLoopMissingSignalSource, source string) orientLoopMissingSignalSource {
	for _, group := range groups {
		if group.Source == source {
			return group
		}
	}
	return orientLoopMissingSignalSource{}
}

func findOrientSignalHypothesisGap(groups []orientLoopSignalHypothesisGap, changeEvent string) orientLoopSignalHypothesisGap {
	for _, group := range groups {
		if group.ChangeEvent == changeEvent {
			return group
		}
	}
	return orientLoopSignalHypothesisGap{}
}

func findOrientReviewNextAction(groups []orientLoopReviewNextAction, nextAction string) orientLoopReviewNextAction {
	for _, group := range groups {
		if group.NextAction == nextAction {
			return group
		}
	}
	return orientLoopReviewNextAction{}
}

func findOrientLoopActionCard(cards []orientLoopActionCard, kind string) orientLoopActionCard {
	for _, card := range cards {
		if card.Kind == kind {
			return card
		}
	}
	return orientLoopActionCard{}
}

func orientHasDrillDown(report orientOperatorReport, label string) bool {
	for _, drill := range report.DrillDown {
		if drill.Label == label {
			return true
		}
	}
	return false
}

func countJSONFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			count++
		}
	}
	return count
}

// strangerFixtureCandidate builds a minimal memory candidate for the S3b dealer tests below,
// using PromotionBasis explicit_confirmation so raw Capture admits it into "proposed" (a human
// gate is required) rather than auto-filing it straight to a terminal state (policy.ResolveAdjudication
// auto-files runtime_proof/trusted_deterministic_import/curated_page/decision_record bases —
// see internal/policy/adjudication_policy.go — which would make every dealer test fixture
// terminal before the dealer ever saw it). Entity/object are unique per call, domain qualifier
// sets the candidate's domain-aware topic axis.
func strangerFixtureCandidate(entity, topic string) mem.Candidate {
	c := mem.Candidate{
		Entity:         entity,
		Predicate:      "status",
		Object:         entity + "-value",
		Confidence:     0.9,
		Derivation:     "extracted",
		PromotionBasis: candidates.BasisExplicitConfirmation,
		SourceTraceRef: "trace://" + entity,
		SnippetHash:    "sha256:" + entity,
	}
	if topic != "" {
		c.Qualifiers = map[string]string{"domain": topic}
	}
	return c
}

// strangerFixturePromote promotes a strangerFixtureCandidate (PromotionBasis
// explicit_confirmation) through its required two-step flow: park, then an operator
// confirmation, then AdjudicateExplicitConfirmation — the same sequence
// internal/mem's promoteForTest helper uses (unexported, so replicated here for
// cmd/compound tests).
func strangerFixturePromote(t *testing.T, s *mem.Store, candidateID string, now time.Time) {
	t.Helper()
	if _, err := s.Adjudicate(candidateID, "park", "", now); err != nil {
		t.Fatalf("park before confirmation: %v", err)
	}
	evidence, err := s.RecordOperatorConfirmation(candidateID, "test fixture confirms "+candidateID, "operator:test", now)
	if err != nil {
		t.Fatalf("record operator confirmation: %v", err)
	}
	if _, err := s.AdjudicateExplicitConfirmation(candidateID, evidence.ID, "", now); err != nil {
		t.Fatalf("adjudicate explicit confirmation: %v", err)
	}
}

func finalizeOrientStrangerForTest(t *testing.T, store *mem.Store, homePath, excludeTopic string, now time.Time) *orientTensionCard {
	t.Helper()
	pending := prepareOrientStrangerDeal(store, homePath, excludeTopic, now)
	if pending == nil {
		return nil
	}
	finalized, err := pending.store.FinalizeStrangerDeal(pending.preparation, now, strangerCardCooldown)
	if err != nil {
		t.Fatalf("finalize stranger deal: %v", err)
	}
	if !finalized {
		return nil
	}
	card := pending.card
	return &card
}

// TestOrientStrangerCardDealerFalsifiers covers the S3b dealer's F5-F7 falsifiers across its
// prepare + Store-owned finalize seam (packet D4/D5/D7).
func TestOrientStrangerCardDealerFalsifiers(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	t.Run("f5 topic-excluded-only candidate is not dealt", func(t *testing.T) {
		homePath := t.TempDir()
		now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
		if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
			t.Fatalf("init home: %v", err)
		}
		s, err := mem.Open(homePath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Capture([]mem.Candidate{strangerFixtureCandidate("entity:f5-excluded-only", "topic-x")}, now.Add(-30*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if card := finalizeOrientStrangerForTest(t, s, homePath, "topic-x", now); card != nil {
			t.Fatalf("excluded-topic-only candidate must not be dealt: %+v", card)
		}
	})

	t.Run("f5 different-topic candidate is dealt exactly once", func(t *testing.T) {
		homePath := t.TempDir()
		now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
		if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
			t.Fatalf("init home: %v", err)
		}
		s, err := mem.Open(homePath)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Capture([]mem.Candidate{
			strangerFixtureCandidate("entity:f5-excluded", "topic-x"),
			strangerFixtureCandidate("entity:f5-eligible", "topic-y"),
		}, now.Add(-30*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		card := finalizeOrientStrangerForTest(t, s, homePath, "topic-x", now)
		if card == nil {
			t.Fatal("expected a stranger card from the different-topic candidate")
		}
		if len(card.Refs) != 1 || card.Refs[0] != got[1].ID {
			t.Fatalf("dealt card = %+v, want exactly the eligible (topic-y) candidate %s", card, got[1].ID)
		}
		if !strings.HasPrefix(card.ID, "stranger:") || !strings.Contains(card.Summary, "stranger") {
			t.Fatalf("card must honestly label itself stranger: %+v", card)
		}
		wantCommand := []string{"compound", "memory", "adjudicate", got[1].ID, "--verdict", "park"}
		if !slices.Equal(card.Command, wantCommand) {
			t.Fatalf("card command = %q, want actionable argv %q", card.Command, wantCommand)
		}
	})

	t.Run("f5 topic-ref descendant of excluded subtree is not dealt", func(t *testing.T) {
		homePath := t.TempDir()
		now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
		if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
			t.Fatalf("init home: %v", err)
		}
		s, err := mem.Open(homePath)
		if err != nil {
			t.Fatal(err)
		}
		topics := []byte(`{"topics":[{"slug":"topic-root"},{"slug":"topic-child","parent":"topic-root"},{"slug":"topic-outside"}]}`)
		if _, err := s.ImportTopics(topics, now.Add(-45*time.Minute)); err != nil {
			t.Fatalf("import topic subtree: %v", err)
		}
		excluded := strangerFixtureCandidate("entity:f5-topic-ref-excluded", "fallback-unrelated")
		excluded.TopicRefs = []string{"topic-child"}
		outside := strangerFixtureCandidate("entity:f5-topic-ref-outside", "fallback-outside")
		outside.TopicRefs = []string{"topic-outside"}
		excludedRows, err := s.Capture([]mem.Candidate{excluded}, now.Add(-31*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		outsideRows, err := s.Capture([]mem.Candidate{outside}, now.Add(-30*time.Minute))
		if err != nil {
			t.Fatal(err)
		}

		card := finalizeOrientStrangerForTest(t, s, homePath, "topic-root", now)
		if card == nil {
			t.Fatal("expected the candidate outside the excluded topic subtree")
		}
		if len(card.Refs) != 1 || card.Refs[0] != outsideRows[0].ID {
			t.Fatalf("dealt card = %+v, want outside candidate %s; older descendant candidate %s must be excluded via topic_refs", card, outsideRows[0].ID, excludedRows[0].ID)
		}
	})

	t.Run("f5 no topic context does not claim cross-topic", func(t *testing.T) {
		homePath := t.TempDir()
		now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
		if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
			t.Fatalf("init home: %v", err)
		}
		s, err := mem.Open(homePath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Capture([]mem.Candidate{strangerFixtureCandidate("entity:f5-no-topic-context", "")}, now.Add(-30*time.Minute)); err != nil {
			t.Fatal(err)
		}

		card := finalizeOrientStrangerForTest(t, s, homePath, "", now)
		if card == nil {
			t.Fatal("expected a stranger card without an exclusion topic")
		}
		if strings.Contains(card.Summary, "cross-topic") {
			t.Fatalf("summary claims an unproven cross-topic relationship without exclusion or current-topic context: %q", card.Summary)
		}
	})

	t.Run("f6 terminal-state candidates are never dealt", func(t *testing.T) {
		homePath := t.TempDir()
		now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
		if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
			t.Fatalf("init home: %v", err)
		}
		s, err := mem.Open(homePath)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Capture([]mem.Candidate{
			strangerFixtureCandidate("entity:f6-rejected", ""),
			strangerFixtureCandidate("entity:f6-no-promote", ""),
			strangerFixtureCandidate("entity:f6-promoted", ""),
		}, now.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Adjudicate(got[0].ID, "reject", "", now.Add(-50*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Adjudicate(got[1].ID, "no_promote", "", now.Add(-50*time.Minute)); err != nil {
			t.Fatal(err)
		}
		strangerFixturePromote(t, s, got[2].ID, now.Add(-50*time.Minute))
		if card := finalizeOrientStrangerForTest(t, s, homePath, "", now); card != nil {
			t.Fatalf("rejected/no_promote/promoted candidates must never be dealt: %+v", card)
		}
	})

	t.Run("f7 dealing appends exactly one row and honors the 7-day cooldown", func(t *testing.T) {
		homePath := t.TempDir()
		now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
		if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
			t.Fatalf("init home: %v", err)
		}
		s, err := mem.Open(homePath)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Capture([]mem.Candidate{strangerFixtureCandidate("entity:f7-cooldown", "")}, now.Add(-30*time.Minute))
		if err != nil {
			t.Fatal(err)
		}

		card := finalizeOrientStrangerForTest(t, s, homePath, "", now)
		if card == nil {
			t.Fatal("expected a deal on the first orient")
		}
		rows, err := mem.ReadStrangerDealtLog(homePath)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].CandidateRef != got[0].ID {
			t.Fatalf("dealt-log = %+v, want exactly one row for %s", rows, got[0].ID)
		}

		// Still within the 7-day cooldown: the same (only) candidate must not be re-dealt,
		// and no second row is appended.
		if again := finalizeOrientStrangerForTest(t, s, homePath, "", now.Add(3*time.Hour)); again != nil {
			t.Fatalf("cooldown must suppress re-dealing within 7 days: %+v", again)
		}
		rowsAfter, err := mem.ReadStrangerDealtLog(homePath)
		if err != nil {
			t.Fatal(err)
		}
		if len(rowsAfter) != 1 {
			t.Fatalf("no card dealt -> no new dealt-log row, got %+v", rowsAfter)
		}

		// Past the cooldown window the candidate is eligible again.
		if later := finalizeOrientStrangerForTest(t, s, homePath, "", now.Add(8*24*time.Hour)); later == nil {
			t.Fatal("candidate should be eligible again once the 7-day cooldown has elapsed")
		}
	})

	t.Run("f7 unreadable dealt-log degrades to no card no crash", func(t *testing.T) {
		homePath := t.TempDir()
		now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
		if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
			t.Fatalf("init home: %v", err)
		}
		s, err := mem.Open(homePath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Capture([]mem.Candidate{strangerFixtureCandidate("entity:f7-corrupt-log", "")}, now.Add(-30*time.Minute)); err != nil {
			t.Fatal(err)
		}
		logPath := filepath.Join(homePath, filepath.FromSlash(mem.StrangerDealtLogRelPath))
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(logPath, []byte("not valid json\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Must degrade to no card, no panic — never deal blind against an unverifiable cooldown.
		if card := finalizeOrientStrangerForTest(t, s, homePath, "", now); card != nil {
			t.Fatalf("corrupt dealt-log must degrade to no card, got %+v", card)
		}
	})

	t.Run("expired oldest candidate does not starve a newer live candidate", func(t *testing.T) {
		homePath := t.TempDir()
		now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
		if err := home.Init(homePath, "test", now.Add(-3*time.Hour)); err != nil {
			t.Fatalf("init home: %v", err)
		}
		s, err := mem.Open(homePath)
		if err != nil {
			t.Fatal(err)
		}
		expired := strangerFixtureCandidate("entity:expired-oldest", "topic-old")
		expired.ExpiresAt = now.Add(-time.Minute).UTC().Format(time.RFC3339)
		if _, err := s.Capture([]mem.Candidate{expired}, now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		live, err := s.Capture([]mem.Candidate{strangerFixtureCandidate("entity:live-newer", "topic-live")}, now.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}

		card := finalizeOrientStrangerForTest(t, s, homePath, "", now)
		if card == nil || len(card.Refs) != 1 || card.Refs[0] != live[0].ID {
			t.Fatalf("expired oldest candidate starved newer live candidate %s: %+v", live[0].ID, card)
		}
	})

	t.Run("topic aliases are canonicalized before subtree exclusion", func(t *testing.T) {
		homePath := t.TempDir()
		now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
		if err := home.Init(homePath, "test", now.Add(-3*time.Hour)); err != nil {
			t.Fatalf("init home: %v", err)
		}
		s, err := mem.Open(homePath)
		if err != nil {
			t.Fatal(err)
		}
		topics := []byte(`{"topics":[{"slug":"topic-root"},{"slug":"topic-child","aliases":["Child Alias"],"parent":"topic-root"},{"slug":"topic-outside"}]}`)
		if _, err := s.ImportTopics(topics, now.Add(-3*time.Hour)); err != nil {
			t.Fatal(err)
		}
		excluded := strangerFixtureCandidate("entity:alias-excluded", "fallback-unrelated")
		excluded.TopicRefs = []string{"Child Alias"}
		if _, err := s.Capture([]mem.Candidate{excluded}, now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		outside, err := s.Capture([]mem.Candidate{strangerFixtureCandidate("entity:alias-outside", "topic-outside")}, now.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}

		card := finalizeOrientStrangerForTest(t, s, homePath, "topic-root", now)
		if card == nil || len(card.Refs) != 1 || card.Refs[0] != outside[0].ID {
			t.Fatalf("alias of excluded descendant bypassed subtree; want %s, got %+v", outside[0].ID, card)
		}
	})

	t.Run("ambiguous topic alias fails closed", func(t *testing.T) {
		homePath := t.TempDir()
		now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
		if err := home.Init(homePath, "test", now.Add(-3*time.Hour)); err != nil {
			t.Fatalf("init home: %v", err)
		}
		s, err := mem.Open(homePath)
		if err != nil {
			t.Fatal(err)
		}
		topics := []byte(`{"topics":[{"slug":"topic-root"},{"slug":"topic-child-a","aliases":["Shared Child"],"parent":"topic-root"},{"slug":"topic-child-b","aliases":["Shared Child"],"parent":"topic-root"}]}`)
		if _, err := s.ImportTopics(topics, now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		ambiguous := strangerFixtureCandidate("entity:alias-ambiguous", "fallback-unrelated")
		ambiguous.TopicRefs = []string{"Shared Child"}
		if _, err := s.Capture([]mem.Candidate{ambiguous}, now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}

		if card := finalizeOrientStrangerForTest(t, s, homePath, "topic-root", now); card != nil {
			t.Fatalf("ambiguous topic alias must fail closed, got %+v", card)
		}
	})
}

// TestOrientOperatorReportIncludesDealtStrangerCard proves the D7 wiring end to end: a single
// eligible candidate produces exactly one stranger card inside the SAME TensionCards.Items list
// contested-claim/standing-law cards use (no new report field).
func TestOrientOperatorReportIncludesDealtStrangerCard(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
		t.Fatalf("init home: %v", err)
	}
	s, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Capture([]mem.Candidate{strangerFixtureCandidate("entity:wiring-proof", "")}, now.Add(-30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	report, err := buildOrientOperatorReport(homePath, 5, now, "")
	if err != nil {
		t.Fatal(err)
	}
	if report.NeedsMe.TensionCards == nil || len(report.NeedsMe.TensionCards.Items) != 1 {
		t.Fatalf("expected exactly one stranger tension card, got %+v", report.NeedsMe.TensionCards)
	}
	card := report.NeedsMe.TensionCards.Items[0]
	if !strings.HasPrefix(card.ID, "stranger:") || len(card.Refs) != 1 || card.Refs[0] != got[0].ID {
		t.Fatalf("card = %+v, want a stranger card naming %s", card, got[0].ID)
	}
}

func TestOrientStrangerCardConcurrentReportsFinalizeOneDeal(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
		t.Fatalf("init home: %v", err)
	}
	s, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Capture([]mem.Candidate{strangerFixtureCandidate("entity:concurrent-deal", "")}, now.Add(-30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	const callers = 2
	arrived := make(chan struct{}, callers)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAll)
	previousObserver := orientStrangerDealPreparedObserver
	orientStrangerDealPreparedObserver = func() {
		arrived <- struct{}{}
		<-release
	}
	t.Cleanup(func() { orientStrangerDealPreparedObserver = previousObserver })

	type result struct {
		report orientOperatorReport
		err    error
	}
	results := make(chan result, callers)
	start := make(chan struct{})
	for range callers {
		go func() {
			<-start
			report, err := buildOrientOperatorReport(homePath, 5, now, "")
			results <- result{report: report, err: err}
		}()
	}
	close(start)
	for range callers {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent orient calls did not both prepare the same deal")
		}
	}
	releaseAll()

	visibleCards := 0
	for range callers {
		res := <-results
		if res.err != nil {
			t.Fatalf("concurrent orient: %v", res.err)
		}
		if cards := res.report.NeedsMe.TensionCards; cards != nil {
			for _, card := range cards.Items {
				if card.ID == "stranger:"+got[0].ID {
					visibleCards++
				}
			}
		}
	}
	rows, err := mem.ReadStrangerDealtLog(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].CandidateRef != got[0].ID || visibleCards != 1 {
		t.Fatalf("concurrent deal rows=%+v visible_cards=%d, want one atomic winner", rows, visibleCards)
	}
}

func TestOrientStrangerCardCursorFailureLeavesNoDealOrCard(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
		t.Fatalf("init home: %v", err)
	}
	s, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Capture([]mem.Candidate{strangerFixtureCandidate("entity:cursor-failure", "")}, now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}

	report, err := buildOrientOperatorReportWithCursorWriter(homePath, 5, now, "", func(string, int64, time.Time) error {
		return errors.New("fixture cursor failure")
	})
	if err == nil {
		t.Fatal("cursor failure must fail the operator report")
	}
	if cards := report.NeedsMe.TensionCards; cards != nil && len(cards.Items) != 0 {
		t.Fatalf("failed report must not expose a stranger card: %+v", cards)
	}
	rows, readErr := mem.ReadStrangerDealtLog(homePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(rows) != 0 {
		t.Fatalf("cursor failure persisted a deal before report success: %+v", rows)
	}
}

func TestOrientStrangerCardFinalizeFailureSuppressesCard(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now.Add(-time.Hour)); err != nil {
		t.Fatalf("init home: %v", err)
	}
	if err := writeOrientOperatorCursor(homePath, 0, now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("seed orient cursor: %v", err)
	}
	cursorPath := orientOperatorCursorPath(homePath)
	cursorBefore, err := os.ReadFile(cursorPath)
	if err != nil {
		t.Fatalf("read cursor before failed finalization: %v", err)
	}
	s, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Capture([]mem.Candidate{strangerFixtureCandidate("entity:append-failure", "")}, now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}

	previousObserver := orientStrangerDealPreparedObserver
	orientStrangerDealPreparedObserver = func() {
		logPath := filepath.Join(homePath, filepath.FromSlash(mem.StrangerDealtLogRelPath))
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			t.Errorf("prepare dealt-log parent: %v", err)
			return
		}
		if err := os.Symlink(filepath.Join(homePath, "missing", "deals.jsonl"), logPath); err != nil {
			t.Errorf("install failing dealt-log symlink: %v", err)
		}
	}
	t.Cleanup(func() { orientStrangerDealPreparedObserver = previousObserver })

	report, err := buildOrientOperatorReport(homePath, 5, now, "")
	if err != nil {
		t.Fatalf("deal append failure must degrade without failing orient: %v", err)
	}
	if cards := report.NeedsMe.TensionCards; cards != nil && len(cards.Items) != 0 {
		t.Fatalf("failed finalization must suppress the prepared card: %+v", cards)
	}
	if !strings.Contains(strings.Join(report.Warnings, "\n"), "stranger_card:") {
		t.Fatalf("finalization failure must remain observable as a warning: %+v", report.Warnings)
	}
	rows, err := mem.ReadStrangerDealtLog(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("failed finalization persisted rows: %+v", rows)
	}
	cursorAfter, err := os.ReadFile(cursorPath)
	if err != nil {
		t.Fatalf("read cursor after failed finalization: %v", err)
	}
	if string(cursorAfter) != string(cursorBefore) {
		t.Fatalf("failed deal finalization advanced orient cursor:\nbefore=%s\nafter=%s", cursorBefore, cursorAfter)
	}
}

func TestOrientStrangerCardFinalizationRejectsChangedPreparedSnapshot(t *testing.T) {
	tests := []struct {
		name         string
		excludeTopic string
		mutate       func(*mem.Candidate)
		prepareHome  func(*testing.T, *mem.Store, time.Time)
	}{
		{
			name:   "card object changed",
			mutate: func(candidate *mem.Candidate) { candidate.Object = "changed-after-prepare" },
		},
		{
			name:         "candidate joined excluded subtree",
			excludeTopic: "topic-root",
			prepareHome: func(t *testing.T, store *mem.Store, now time.Time) {
				t.Helper()
				topics := []byte(`{"topics":[{"slug":"topic-root"},{"slug":"topic-child","parent":"topic-root"}]}`)
				if _, err := store.ImportTopics(topics, now.Add(-time.Hour)); err != nil {
					t.Fatal(err)
				}
			},
			mutate: func(candidate *mem.Candidate) { candidate.TopicRefs = []string{"topic-child"} },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := t.TempDir()
			now := time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC)
			if err := home.Init(homePath, "test", now.Add(-2*time.Hour)); err != nil {
				t.Fatal(err)
			}
			store, err := mem.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			if tt.prepareHome != nil {
				tt.prepareHome(t, store, now)
			}
			captured, err := store.Capture([]mem.Candidate{strangerFixtureCandidate("entity:snapshot-"+strings.ReplaceAll(tt.name, " ", "-"), "topic-outside")}, now.Add(-30*time.Minute))
			if err != nil {
				t.Fatal(err)
			}

			previousObserver := orientStrangerDealPreparedObserver
			orientStrangerDealPreparedObserver = func() {
				var candidate mem.Candidate
				if err := candidates.ReadCandidate(homePath, candidates.Memory, captured[0].ID, &candidate); err != nil {
					t.Fatal(err)
				}
				tt.mutate(&candidate)
				if err := candidates.WriteCandidate(homePath, candidates.Memory, captured[0].ID, &candidate); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { orientStrangerDealPreparedObserver = previousObserver })

			report, err := buildOrientOperatorReport(homePath, 5, now, tt.excludeTopic)
			if err != nil {
				t.Fatal(err)
			}
			if cards := report.NeedsMe.TensionCards; cards != nil {
				for _, card := range cards.Items {
					if card.ID == "stranger:"+captured[0].ID {
						t.Fatalf("changed prepared snapshot was exposed: %+v", card)
					}
				}
			}
			rows, err := mem.ReadStrangerDealtLog(homePath)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 0 {
				t.Fatalf("changed prepared snapshot was recorded as dealt: %+v", rows)
			}
		})
	}
}
