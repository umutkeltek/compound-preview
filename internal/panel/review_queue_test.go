package panel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/mem"
	"compound/internal/reviewartifact"
)

func TestAnnotateVerdictQueueAddsDecisionCardForNeedsHumanMemoryCandidate(t *testing.T) {
	home := t.TempDir()
	run := Run{ID: "prun_card", WorkID: "work_card"}
	target := TargetInput{
		Kind:       "memory_candidate",
		Ref:        "cand_card",
		Visibility: "private",
		Summary:    "fallback summary should not be headline",
		Payload:    json.RawMessage(`{"object":"Owned cognition runtime should decide next actions from goals, patterns, and evidence."}`),
	}
	lenses := []LensFinding{
		{
			LensRef:          "lens:evidence-strength",
			RouteEvidenceRef: "route_evidence",
			Status:           "completed",
			Findings: []Finding{{
				Claim:    "The source is thin and needs operator judgment before becoming ambient memory.",
				Severity: "high",
			}},
		},
		{
			LensRef: "lens:duplicate-risk",
			Status:  "completed",
			Findings: []Finding{{
				Claim:    "A similar strategy claim may already exist.",
				Severity: "medium",
			}},
		},
	}
	synthesis := Synthesis{
		SchemaVersion:         SynthesisSchemaVersion,
		Decision:              "needs_human",
		Confidence:            0.75,
		Rationale:             "The panel needs the operator to decide whether this is durable strategy or duplicate phrasing.",
		JudgeRouteEvidenceRef: "route_judge",
	}
	if _, err := writeVerdictArtifact(home, run, target, lenses, synthesis, time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("write verdict artifact: %v", err)
	}

	report, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: "panel-verdicts"}, time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	AnnotateVerdictQueue(&report)
	reviewartifact.Recount(&report)
	if len(report.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(report.Items))
	}
	item := report.Items[0]
	if item.Status != "review_required" || item.Readiness != "needs_review" {
		t.Fatalf("item status/readiness = %s/%s", item.Status, item.Readiness)
	}
	card := item.DecisionCard
	if card == nil {
		t.Fatal("decision card missing")
	}
	if strings.Contains(card.Claim, "cand_card") || !strings.Contains(card.Claim, "Owned cognition runtime") {
		t.Fatalf("claim = %q, want plain claim without id", card.Claim)
	}
	if card.PanelLean != "needs_human (confidence 0.75)" {
		t.Fatalf("panel lean = %q", card.PanelLean)
	}
	if !hasReasonQuestion(card.Why, "How well is this supported?") || !hasReasonQuestion(card.Why, "Do we already believe something like this?") {
		t.Fatalf("decision card reasons missing lens questions: %+v", card.Why)
	}
	if len(card.Choices) != 4 || len(item.Actions) != 5 {
		t.Fatalf("choices/actions = %d/%d, want 4 choices and 5 actions (4 shell choices + 1 clean member action)", len(card.Choices), len(item.Actions))
	}
	var clean []reviewartifact.Action
	for _, action := range item.Actions {
		if len(action.Argv) > 0 && action.Argv[0] == "compound" {
			clean = append(clean, action)
		}
	}
	if len(clean) != 1 {
		t.Fatalf("clean-argv actions = %d, want exactly 1: %+v", len(clean), item.Actions)
	}
	member := clean[0]
	if member.ID != "panel_verdict_record_member" || !member.RequiresReview {
		t.Fatalf("clean member action = %+v, want id=panel_verdict_record_member requires_review=true", member)
	}
	for _, token := range member.Argv {
		if token == "/bin/sh" || token == "-lc" || strings.Contains(token, "$RATIONALE") || strings.Contains(token, "&&") {
			t.Fatalf("clean member argv must not contain shell tokens or placeholders: %v", member.Argv)
		}
	}
	if containsReviewCardString(member.Argv, "--rationale") {
		t.Fatalf("clean member argv must omit --rationale (console appends it): %v", member.Argv)
	}
	promote := card.Choices[0]
	if promote.ID != "promote" || !strings.Contains(promote.Effect, "ambient canon") {
		t.Fatalf("promote choice = %+v", promote)
	}
	if !strings.Contains(promote.Command, "compound panel verdict-record") ||
		!strings.Contains(promote.Command, "compound memory adjudicate 'cand_card' --verdict 'promote'") {
		t.Fatalf("promote command should use canonical commands, got %q", promote.Command)
	}
	if !promote.RequiresReview {
		t.Fatal("promote choice should require operator review")
	}
	if !containsReviewCardString(card.Boundary, "no console-side state writes") {
		t.Fatalf("boundary missing console write ban: %+v", card.Boundary)
	}
}

func TestAnnotateVerdictQueueResolvesAlreadyParkedMemoryCandidate(t *testing.T) {
	home := t.TempDir()
	run := Run{ID: "prun_stale", WorkID: "work_stale"}
	target := TargetInput{
		Kind:       "memory_candidate",
		Ref:        "cand_stale_panel",
		Visibility: "private",
		Payload:    json.RawMessage(`{"object":"Owned cognition runtime should decide next actions from evidence."}`),
	}
	synthesis := Synthesis{
		SchemaVersion:         SynthesisSchemaVersion,
		Decision:              "needs_human",
		Confidence:            0.75,
		Rationale:             "Panel requested operator review before promotion.",
		JudgeRouteEvidenceRef: "route_judge",
	}
	if _, err := writeVerdictArtifact(home, run, target, nil, synthesis, time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("write verdict artifact: %v", err)
	}

	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordStableCandidate(mem.Candidate{
		ID:             "cand_stale_panel",
		Entity:         "knowledge:chatgpt:stale",
		Predicate:      "proposes_strategy",
		Object:         "Owned cognition runtime should decide next actions from evidence.",
		Confidence:     0.72,
		Derivation:     "extracted",
		ClaimType:      "architecture",
		Visibility:     "private",
		SourceTraceRef: "knowledge_extraction:kx_stale",
		Qualifiers: map[string]string{
			"source": "knowledge_extraction",
		},
	}); err != nil {
		t.Fatalf("record stable candidate: %v", err)
	}
	if _, err := store.AnnotateCandidate("cand_stale_panel", mem.CandidateAnnotation{
		Kind:             "panel_verdict",
		Decision:         "annotate",
		OriginalDecision: "needs_human",
		RunID:            run.ID,
		VerdictArtifact:  "panel_verdict_fixture.json",
		Rationale:        "panel evidence was applied as annotation, not ambient memory",
		Confidence:       0.75,
		EvidenceRefs:     []string{"panel_run:" + run.ID},
	}, time.Date(2026, 7, 4, 0, 1, 0, 0, time.UTC)); err != nil {
		t.Fatalf("annotate candidate: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(home, "ledger"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "ledger", "evidence.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: "panel-verdicts"}, time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	AnnotateVerdictQueue(&report)
	reviewartifact.Recount(&report)
	if len(report.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(report.Items))
	}
	item := report.Items[0]
	if item.Status != "resolved" || item.Readiness != "no_action" {
		t.Fatalf("item status/readiness = %s/%s, want resolved/no_action", item.Status, item.Readiness)
	}
	if !strings.Contains(item.NextAction, "already parked") || !strings.Contains(item.NextAction, "cand_stale_panel") {
		t.Fatalf("next action should explain stale candidate resolution, got %q", item.NextAction)
	}
	if item.DecisionCard != nil || len(item.Actions) != 0 {
		t.Fatalf("resolved stale verdict should not expose operator actions: card=%+v actions=%+v", item.DecisionCard, item.Actions)
	}
	if report.Counts.ReviewRequired != 0 || report.Counts.OK != 1 {
		t.Fatalf("review counts = %+v, want no review debt and one ok item", report.Counts)
	}
}

func TestAnnotateVerdictQueueIgnoresUnreferencedCorruptMemoryCandidates(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{"cand_point_lookup_alpha", "cand_point_lookup_beta"}
	for i, candidateID := range wantIDs {
		run := Run{ID: "prun_point_lookup_" + candidateID, WorkID: "work_point_lookup"}
		target := TargetInput{
			Kind:       "memory_candidate",
			Ref:        candidateID,
			Visibility: "private",
			Payload:    json.RawMessage(`{"object":"Point lookup should isolate unrelated candidate corruption."}`),
		}
		synthesis := Synthesis{
			SchemaVersion:         SynthesisSchemaVersion,
			Decision:              "needs_human",
			Confidence:            0.75,
			Rationale:             "Panel requested operator review before promotion.",
			JudgeRouteEvidenceRef: "route_point_lookup",
		}
		if _, err := writeVerdictArtifact(home, run, target, nil, synthesis, time.Date(2026, 7, 4, 0, i, 0, 0, time.UTC)); err != nil {
			t.Fatalf("write verdict artifact for %s: %v", candidateID, err)
		}
		if _, err := store.RecordStableCandidate(mem.Candidate{
			ID:             candidateID,
			Entity:         "knowledge:point-lookup:" + candidateID,
			Predicate:      "supports_isolated_lookup",
			Object:         "Point lookup should isolate unrelated candidate corruption.",
			Confidence:     0.8,
			Derivation:     "deterministic",
			Visibility:     "private",
			SourceTraceRef: "point_lookup_fixture",
		}); err != nil {
			t.Fatalf("record stable candidate %s: %v", candidateID, err)
		}
		if _, err := store.AnnotateCandidate(candidateID, mem.CandidateAnnotation{
			Kind:             "panel_verdict",
			Decision:         "annotate",
			OriginalDecision: "needs_human",
			RunID:            run.ID,
			VerdictArtifact:  "panel_verdict_fixture.json",
			Rationale:        "panel evidence was applied as annotation, not ambient memory",
			Confidence:       0.75,
			EvidenceRefs:     []string{"panel_run:" + run.ID},
		}, time.Date(2026, 7, 4, 0, i, 30, 0, time.UTC)); err != nil {
			t.Fatalf("annotate candidate %s: %v", candidateID, err)
		}
	}

	for _, name := range []string{"000-poison.json", "001-poison.json"} {
		path := filepath.Join(home, candidates.InboxDir(candidates.Memory), name)
		if err := os.WriteFile(path, []byte("{not-json"), 0o644); err != nil {
			t.Fatalf("write poison candidate %s: %v", name, err)
		}
	}

	report, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: "panel-verdicts"}, time.Date(2026, 7, 4, 0, 2, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	AnnotateVerdictQueue(&report)
	reviewartifact.Recount(&report)
	if len(report.Items) != len(wantIDs) {
		t.Fatalf("items = %d, want %d", len(report.Items), len(wantIDs))
	}
	for _, item := range report.Items {
		verdict, ok := verdictFromQueueItem(item)
		if !ok {
			t.Fatalf("verdict artifact unreadable for queue item %s", item.ID)
		}
		if item.Status != "resolved" || item.Readiness != "no_action" {
			t.Fatalf("target %s status/readiness = %s/%s, want resolved/no_action", verdict.Target.Ref, item.Status, item.Readiness)
		}
		wantNextAction := "memory candidate already parked with 1 annotation(s); panel verdict no longer open review debt (target=memory_candidate/" + verdict.Target.Ref + ")"
		if item.NextAction != wantNextAction {
			t.Fatalf("target %s next action = %q, want %q", verdict.Target.Ref, item.NextAction, wantNextAction)
		}
	}
	if report.Counts.ReviewRequired != 0 || report.Counts.OK != len(wantIDs) {
		t.Fatalf("review counts = %+v, want no review debt and %d ok items", report.Counts, len(wantIDs))
	}
}

func TestAnnotateVerdictQueueMissingMemoryCandidateDoesNotBlockSiblings(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	const (
		presentID = "cand_point_lookup_present"
		missingID = "cand_point_lookup_missing"
	)
	for i, candidateID := range []string{presentID, missingID} {
		run := Run{ID: "prun_missing_" + candidateID, WorkID: "work_missing_candidate"}
		target := TargetInput{
			Kind:       "memory_candidate",
			Ref:        candidateID,
			Visibility: "private",
			Payload:    json.RawMessage(`{"object":"A missing candidate should not block a readable sibling."}`),
		}
		synthesis := Synthesis{
			SchemaVersion:         SynthesisSchemaVersion,
			Decision:              "needs_human",
			Confidence:            0.75,
			Rationale:             "Panel requested operator review before promotion.",
			JudgeRouteEvidenceRef: "route_missing_candidate",
		}
		if _, err := writeVerdictArtifact(home, run, target, nil, synthesis, time.Date(2026, 7, 4, 1, i, 0, 0, time.UTC)); err != nil {
			t.Fatalf("write verdict artifact for %s: %v", candidateID, err)
		}
	}
	if _, err := store.RecordStableCandidate(mem.Candidate{
		ID:             presentID,
		Entity:         "knowledge:point-lookup:present",
		Predicate:      "supports_missing_sibling",
		Object:         "A missing candidate should not block a readable sibling.",
		Confidence:     0.8,
		Derivation:     "deterministic",
		Visibility:     "private",
		SourceTraceRef: "missing_candidate_fixture",
	}); err != nil {
		t.Fatalf("record stable candidate %s: %v", presentID, err)
	}
	if _, err := store.AnnotateCandidate(presentID, mem.CandidateAnnotation{
		Kind:             "panel_verdict",
		Decision:         "annotate",
		OriginalDecision: "needs_human",
		RunID:            "prun_missing_" + presentID,
		VerdictArtifact:  "panel_verdict_fixture.json",
		Rationale:        "panel evidence was applied as annotation, not ambient memory",
		Confidence:       0.75,
		EvidenceRefs:     []string{"panel_run:prun_missing_" + presentID},
	}, time.Date(2026, 7, 4, 1, 2, 0, 0, time.UTC)); err != nil {
		t.Fatalf("annotate candidate %s: %v", presentID, err)
	}

	report, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: "panel-verdicts"}, time.Date(2026, 7, 4, 1, 3, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	AnnotateVerdictQueue(&report)
	if len(report.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(report.Items))
	}
	for _, item := range report.Items {
		verdict, ok := verdictFromQueueItem(item)
		if !ok {
			t.Fatalf("verdict artifact unreadable for queue item %s", item.ID)
		}
		switch verdict.Target.Ref {
		case presentID:
			if item.Status != "resolved" || item.Readiness != "no_action" {
				t.Fatalf("present sibling status/readiness = %s/%s, want resolved/no_action", item.Status, item.Readiness)
			}
		case missingID:
			if item.Status != "review_required" || item.Readiness != "needs_review" {
				t.Fatalf("missing target status/readiness = %s/%s, want review_required/needs_review", item.Status, item.Readiness)
			}
		default:
			t.Fatalf("unexpected target %s", verdict.Target.Ref)
		}
	}
}

func hasReasonQuestion(reasons []reviewartifact.DecisionCardReason, question string) bool {
	for _, reason := range reasons {
		if reason.Question == question {
			return true
		}
	}
	return false
}

func containsReviewCardString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
