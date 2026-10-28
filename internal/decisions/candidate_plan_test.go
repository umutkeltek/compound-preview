package decisions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/reviewartifact"
)

func TestDecisionCandidatePlanRecordsReviewArtifactsWithoutMutatingTarget(t *testing.T) {
	home := t.TempDir()
	candidateID := "cand_decision_review"
	dir := filepath.Join(home, candidates.InboxDir(candidates.Decision))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	candidate := decisionCandidateRecord{
		ID:         candidateID,
		Kind:       string(candidates.Decision),
		Subject:    "decision:route-free-model-judging",
		Statement:  "Use paid frontier lanes for final judgment only after cheap lanes produce evidence.",
		Detail:     "Keeps routing cost controlled while preserving quality gates.",
		Confidence: 0.86,
		Qualifiers: map[string]string{
			"kind":           KindOperational,
			"owner":          "compound",
			"scope":          "routing",
			"falsifier":      "cheap lanes repeatedly miss material defects",
			"review_trigger": "after three route-evidence regressions",
		},
		EvidenceRefs: []string{"trace:decision", "receipt:routing"},
		Verdict:      candidates.Park,
	}
	data, err := json.MarshalIndent(candidate, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, candidateID+".json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(t.TempDir(), "DEC-routing.md")
	original := []byte("# Existing Decision\n\n")
	if err := os.WriteFile(targetPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildDecisionCandidatePlan(home, candidateID, targetPath, time.Date(2026, 6, 29, 23, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != DecisionCandidatePlanSchemaVersion ||
		plan.Status != "review_required" ||
		plan.CandidateID != candidateID ||
		plan.CandidateVerdict != candidates.Park ||
		plan.DecisionKey != "route-free-model-judging" ||
		plan.LifecycleDraft.DecisionID != "route-free-model-judging" ||
		plan.LifecycleDraft.EntityID != "decision:route-free-model-judging" ||
		plan.LifecycleDraft.Status != StatusProposed ||
		plan.LifecycleDraft.Falsifier != "cheap lanes repeatedly miss material defects" ||
		plan.LifecycleDraft.ReviewTrigger != "after three route-evidence regressions" ||
		plan.TargetHash == "" {
		t.Fatalf("bad decision candidate plan: %+v", plan)
	}
	paths, err := RecordDecisionCandidatePlan(home, plan)
	if err != nil {
		t.Fatal(err)
	}
	planData, err := os.ReadFile(paths.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	var recorded DecisionCandidatePlan
	if err := json.Unmarshal(planData, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.ID != plan.ID || recorded.LifecycleDraft.EntityID != "decision:route-free-model-judging" {
		t.Fatalf("recorded plan = %+v", recorded)
	}
	review, err := os.ReadFile(paths.ReviewPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(review), "does not mutate decision source files") ||
		!strings.Contains(string(review), "## Draft DecisionLifecycle") {
		t.Fatalf("review missing guardrails:\n%s", review)
	}
	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("target mutated:\n%s", got)
	}
}

func TestDecisionCandidatePlanRequiresParkedDecisionCandidate(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, candidates.InboxDir(candidates.Decision))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	candidate := decisionCandidateRecord{
		ID:      "cand_rejected_decision",
		Kind:    string(candidates.Decision),
		Subject: "decision:rejected",
		Verdict: candidates.Reject,
	}
	data, err := json.MarshalIndent(candidate, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, candidate.ID+".json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildDecisionCandidatePlan(home, candidate.ID, "", time.Date(2026, 6, 29, 23, 31, 0, 0, time.UTC)); err == nil ||
		!strings.Contains(err.Error(), "want park") {
		t.Fatalf("expected parked-verdict error, got %v", err)
	}
}

func TestAnnotateDecisionCandidateQueueAddsBoundedPanelPlanAction(t *testing.T) {
	home := t.TempDir()
	candidateID := "cand_decision_panel_review"
	dir := filepath.Join(home, candidates.InboxDir(candidates.Decision))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	candidate := decisionCandidateRecord{
		ID:        candidateID,
		Kind:      string(candidates.Decision),
		Subject:   "decision:destructive-action-hold",
		Statement: "Hold destructive actions until the owner approves them.",
		Verdict:   candidates.Park,
	}
	data, err := json.MarshalIndent(candidate, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, candidateID+".json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildDecisionCandidatePlan(home, candidateID, "", time.Date(2026, 7, 15, 6, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordDecisionCandidatePlan(home, plan); err != nil {
		t.Fatal(err)
	}

	report, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: DecisionCandidateReviewArea}, time.Date(2026, 7, 15, 6, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateDecisionCandidateQueue(&report)
	reviewartifact.Recount(&report)
	if len(report.Items) != 1 || len(report.Items[0].Actions) != 1 {
		t.Fatalf("decision candidate queue action missing: %+v", report)
	}
	item := report.Items[0]
	action := item.Actions[0]
	wantArgv := []string{
		"compound", "panel", "plan",
		"--target-kind", "decision_candidate",
		"--target", candidateID,
		"--domain", "reasoning-lenses",
		"--task", decisionCandidateReviewTask,
		"--supervised",
		"--max-items", "1",
		"--json",
	}
	if action.ID != DecisionCandidateReviewActionID ||
		action.Label != "Plan decision review" ||
		!action.RequiresReview ||
		strings.Join(action.Argv, "\x00") != strings.Join(wantArgv, "\x00") ||
		!strings.Contains(action.Command, "'"+decisionCandidateReviewTask+"'") ||
		!strings.Contains(action.PolicyNote, "decision source") {
		t.Fatalf("bad decision candidate action: %+v", action)
	}
	if item.CandidateID != candidateID || item.Owner != "internal/decisions" ||
		len(report.Groups) != 1 || report.Groups[0].SafeBatchAction == nil || report.Groups[0].TerminalReason != "" {
		t.Fatalf("decision candidate projection not actionable: item=%+v groups=%+v", item, report.Groups)
	}
}

func TestAnnotateDecisionCandidateQueueKeepsSameKindCandidatesIndividuallyActionable(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, candidates.InboxDir(candidates.Decision))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	candidateIDs := []string{"cand_decision_panel_alpha", "cand_decision_panel_bravo"}
	for index, candidateID := range candidateIDs {
		candidate := decisionCandidateRecord{
			ID:        candidateID,
			Kind:      string(candidates.Decision),
			Subject:   "decision:bounded-panel-review",
			Statement: fmt.Sprintf("Review bounded decision candidate %d.", index+1),
			Verdict:   candidates.Park,
		}
		data, err := json.MarshalIndent(candidate, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, candidateID+".json"), append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		plan, err := BuildDecisionCandidatePlan(home, candidateID, "", time.Date(2026, 7, 15, 6, index, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		if plan.LifecycleDraft.Kind != KindOperational {
			t.Fatalf("candidate %s kind=%q, want shared %q group shape", candidateID, plan.LifecycleDraft.Kind, KindOperational)
		}
		if _, err := RecordDecisionCandidatePlan(home, plan); err != nil {
			t.Fatal(err)
		}
	}

	report, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: DecisionCandidateReviewArea}, time.Date(2026, 7, 15, 6, 3, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Items) != 2 || len(report.Groups) != 2 {
		t.Fatalf("new decision candidate artifacts must serialize as singleton groups: items=%d groups=%+v", len(report.Items), report.Groups)
	}
	// Old artifacts used one kind-wide group. The live annotator must repair
	// that projection shape so an existing backlog gains the same action path.
	for index := range report.Items {
		report.Items[index].GroupKey = DecisionCandidateReviewArea + "|" + KindOperational
	}
	reviewartifact.Recount(&report)
	if len(report.Groups) != 1 {
		t.Fatalf("legacy grouping fixture did not collapse: %+v", report.Groups)
	}
	AnnotateDecisionCandidateQueue(&report)
	reviewartifact.Recount(&report)
	if len(report.Items) != 2 || len(report.Groups) != 2 {
		t.Fatalf("same-kind decision candidates must remain singleton action groups: items=%d groups=%+v", len(report.Items), report.Groups)
	}
	for _, item := range report.Items {
		if len(item.Actions) != 1 {
			t.Fatalf("candidate %s lost its owner action: %+v", item.CandidateID, item)
		}
		wantTarget := item.CandidateID
		action := item.Actions[0]
		if len(action.Argv) < 7 || action.Argv[5] != "--target" || action.Argv[6] != wantTarget {
			t.Fatalf("candidate %s action targets the wrong candidate: %+v", wantTarget, action.Argv)
		}
		var group *reviewartifact.QueueGroup
		for index := range report.Groups {
			if len(report.Groups[index].ItemIDs) == 1 && report.Groups[index].ItemIDs[0] == item.ID {
				group = &report.Groups[index]
				break
			}
		}
		if group == nil || group.Count != 1 || group.SafeBatchAction == nil || group.TerminalReason != "" {
			t.Fatalf("candidate %s is not individually actionable: group=%+v", wantTarget, group)
		}
		if got := group.SafeBatchAction.Argv[6]; got != wantTarget {
			t.Fatalf("candidate %s group action targets %s", wantTarget, got)
		}
	}
}
