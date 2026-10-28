package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/home"
	"compound/internal/intent"
	"compound/internal/mem"
)

func TestIntentCLIPlanReviewApplyUsesRecordedOrientationAndExistingAdmission(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	seedIntentCLIDirective(t, homePath, "LIVING-EDGE", "shape the living edge after review", time.Now().UTC().Add(-time.Minute))

	proposalPath := filepath.Join(t.TempDir(), "proposal.json")
	proposal := []byte(`{
  "desired_outcome": "the living edge becomes inspectable work",
  "success_predicate": "the living edge is shaped only after review",
  "non_goals": ["do not flatten the speaker's language"],
  "action_summary": "shape the living edge after review",
  "uncertainty": {"level": "inferred", "reason": "derived from expressive speech"}
}`)
	if err := os.WriteFile(proposalPath, proposal, 0o644); err != nil {
		t.Fatal(err)
	}

	planOut := captureStdout(t, func() {
		cmdIntent([]string{
			"plan",
			"--source", "console",
			"--text", "let the living edge breathe, then let it know where it is going",
			"--task", "shape the living edge after review",
			"--from-proposal", proposalPath,
			"--harness", "codex",
			"--json",
		})
	})
	var planned intent.PlanReport
	if err := json.Unmarshal([]byte(planOut), &planned); err != nil {
		t.Fatalf("decode plan output: %v\n%s", err, planOut)
	}
	if planned.Status != intent.StatusPlanProposed || planned.Plan == nil || planned.PlanPath == "" {
		t.Fatalf("planned = %+v", planned)
	}
	if planned.Plan.Work.IDs.ContextPackID == "" {
		t.Fatalf("plan omitted the recorded ContextPack identity: %+v", planned.Plan.Work.IDs)
	}
	if _, err := os.Stat(filepath.Join(homePath, "context", "packs", planned.Plan.Work.IDs.ContextPackID+".json")); err != nil {
		t.Fatalf("recorded ContextPack: %v", err)
	}
	if planned.Plan.ActionContext == nil || planned.Plan.ActionContext.SourceRef != "decision://LIVING-EDGE" {
		t.Fatalf("promoted meaning = %+v", planned.Plan.ActionContext)
	}

	reviewOut := captureStdout(t, func() {
		cmdIntent([]string{"review", planned.PlanPath, "--json"})
	})
	var reviewed intent.ReviewReport
	if err := json.Unmarshal([]byte(reviewOut), &reviewed); err != nil {
		t.Fatalf("decode review output: %v\n%s", err, reviewOut)
	}
	if reviewed.Kind != intent.ReviewKindPlan || reviewed.Plan == nil || reviewed.Plan.ID != planned.Plan.ID {
		t.Fatalf("reviewed = %+v", reviewed)
	}

	applyOut := captureStdout(t, func() {
		cmdIntent([]string{"apply", planned.PlanPath, "--by", "operator-fixture", "--json"})
	})
	if !strings.Contains(applyOut, `"status": "admitted"`) {
		t.Fatalf("first apply = %s", applyOut)
	}
	replayOut := captureStdout(t, func() {
		cmdIntent([]string{"apply", planned.PlanPath, "--by", "operator-fixture", "--json"})
	})
	if !strings.Contains(replayOut, `"status": "already_admitted"`) {
		t.Fatalf("replay apply = %s", replayOut)
	}
}

func seedIntentCLIDirective(t *testing.T, homePath, decisionID, directive string, now time.Time) {
	t.Helper()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Capture([]mem.Candidate{{
		Entity:          "loop:expressive-cli-fixture",
		Predicate:       "action_directive",
		Object:          directive,
		ObservationDate: now.Format(time.RFC3339),
		Confidence:      0.99,
		Derivation:      "deterministic",
		ClaimType:       "decision",
		Qualifiers: map[string]string{
			"decision_id":      decisionID,
			"lifecycle_status": "accepted",
		},
		PromotionBasis: candidates.BasisDecisionRecord,
		SourceTraceRef: "fixture://expressive-intent-cli/" + strings.ToLower(decisionID),
		SnippetHash:    "sha256:" + strings.ToLower(decisionID),
		Visibility:     "operational",
	}}, now)
	if err != nil || len(created) != 1 {
		t.Fatalf("capture count=%d err=%v", len(created), err)
	}
	if _, err := store.Adjudicate(created[0].ID, "promote", "intent_cli_fixture", now.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FoldPositions(mem.PositionFoldOptions{}, now.Add(2*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
}
