package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/home"
	"compound/internal/panel"
	"compound/internal/reviewartifact"
)

func TestPanelLensesCLIJSON(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	manifest := filepath.Join("..", "..", "fixtures", "panel", "lens-manifest.yaml")
	captureStdout(t, func() { cmdPanel([]string{"import-lenses", manifest}) })

	out := captureStdout(t, func() { cmdPanel([]string{"lenses", "--target-kind", "memory_candidate", "--json"}) })
	var memorySelection panel.LensSelection
	if err := json.Unmarshal([]byte(out), &memorySelection); err != nil {
		t.Fatalf("parse memory lens JSON: %v\n%s", err, out)
	}
	if len(memorySelection.Included) != 6 || len(memorySelection.Excluded) != 10 {
		t.Fatalf("memory_candidate selection = %+v, want six included and ten non-memory excluded", memorySelection)
	}

	out = captureStdout(t, func() {
		cmdPanel([]string{"lenses", "--target-kind", "memory_candidate", "--domain", "paid-app", "--task", "pricing", "--json"})
	})
	var paidSelection panel.LensSelection
	if err := json.Unmarshal([]byte(out), &paidSelection); err != nil {
		t.Fatalf("parse paid lens JSON: %v\n%s", err, out)
	}
	if !panelSelectionIncludes(paidSelection, "lens:paid-app-commercial-fit") || len(paidSelection.MissingDomainLens) != 0 {
		t.Fatalf("paid-app selection = %+v, want paid commercial lens with no missing domain gap", paidSelection)
	}

	out = captureStdout(t, func() {
		cmdPanel([]string{"lenses", "--target-kind", "memory_candidate", "--domain", "memory-ingestion", "--task", "conversation memory recall proof", "--json"})
	})
	var memoryIngestionSelection panel.LensSelection
	if err := json.Unmarshal([]byte(out), &memoryIngestionSelection); err != nil {
		t.Fatalf("parse memory-ingestion lens JSON: %v\n%s", err, out)
	}
	if !panelSelectionIncludes(memoryIngestionSelection, "lens:memory-ingestion-admission") || len(memoryIngestionSelection.MissingDomainLens) != 0 {
		t.Fatalf("memory-ingestion selection = %+v, want admission lens with no missing domain gap", memoryIngestionSelection)
	}

	out = captureStdout(t, func() {
		cmdPanel([]string{"lenses", "--target-kind", "capability_observation", "--domain", "codebase-design", "--task", "adapt operating-system skill without copying; seam deletion test", "--json"})
	})
	var codebaseSelection panel.LensSelection
	if err := json.Unmarshal([]byte(out), &codebaseSelection); err != nil {
		t.Fatalf("parse codebase-design lens JSON: %v\n%s", err, out)
	}
	if !panelSelectionIncludes(codebaseSelection, "lens:codebase-seam-deletion-test") || len(codebaseSelection.MissingDomainLens) != 0 {
		t.Fatalf("codebase-design selection = %+v, want seam deletion lens and no missing domain gap", codebaseSelection)
	}

	out = captureStdout(t, func() {
		cmdPanel([]string{"lenses", "--target-kind", "skill_candidate", "--domain", "skill-creator", "--task", "judge whether the skill fires and changes outcome", "--json"})
	})
	var skillCreatorSelection panel.LensSelection
	if err := json.Unmarshal([]byte(out), &skillCreatorSelection); err != nil {
		t.Fatalf("parse skill-creator lens JSON: %v\n%s", err, out)
	}
	if !panelSelectionIncludes(skillCreatorSelection, "lens:skill-trigger-outcome-fit") || len(skillCreatorSelection.MissingDomainLens) != 0 {
		t.Fatalf("skill-creator selection = %+v, want trigger outcome lens and no missing domain gap", skillCreatorSelection)
	}

	out = captureStdout(t, func() {
		cmdPanel([]string{"lenses", "--target-kind", "decision_candidate", "--domain", "reasoning-lenses", "--task", "review falsifier and second-order decision effects", "--json"})
	})
	var decisionSelection panel.LensSelection
	if err := json.Unmarshal([]byte(out), &decisionSelection); err != nil {
		t.Fatalf("parse decision lens JSON: %v\n%s", err, out)
	}
	if !panelSelectionIncludes(decisionSelection, "lens:decision-falsifier-review") || len(decisionSelection.MissingDomainLens) != 0 {
		t.Fatalf("decision selection = %+v, want falsifier lens no missing domain gap", decisionSelection)
	}

	out = captureStdout(t, func() {
		cmdPanel([]string{"lenses", "--target-kind", "capability_observation", "--domain", "anti-pattern-detectors", "--task", "detect prompt-only anti-pattern prose and redirect to consumed proof", "--json"})
	})
	var antiPatternSelection panel.LensSelection
	if err := json.Unmarshal([]byte(out), &antiPatternSelection); err != nil {
		t.Fatalf("parse anti-pattern lens JSON: %v\n%s", err, out)
	}
	if !panelSelectionIncludes(antiPatternSelection, "lens:anti-pattern-operational-detector") || len(antiPatternSelection.MissingDomainLens) != 0 {
		t.Fatalf("anti-pattern selection = %+v, want operational detector lens no missing domain gap", antiPatternSelection)
	}

	out = captureStdout(t, func() { cmdPanel([]string{"lenses", "--target-kind", "external_repo", "--json"}) })
	var externalSelection panel.LensSelection
	if err := json.Unmarshal([]byte(out), &externalSelection); err != nil {
		t.Fatalf("parse external lens JSON: %v\n%s", err, out)
	}
	if len(externalSelection.Included) != 1 || externalSelection.Included[0].NodeID != "lens:privacy-risk" {
		t.Fatalf("external_repo included = %+v, want only privacy-risk", externalSelection.Included)
	}
	reasons := map[string]string{}
	for _, excluded := range externalSelection.Excluded {
		reasons[excluded.Ref] = excluded.Reason
	}
	for _, id := range []string{"lens:duplicate-risk", "lens:evidence-strength", "lens:contradiction-with-existing-memory", "lens:fossilization-risk", "lens:paid-app-commercial-fit", "lens:social-distribution-mechanics", "lens:comparison-correctness", "lens:comparison-contract-fidelity", "lens:comparison-cost-latency", "lens:inversion-falsification", "lens:decision-falsifier-review", "lens:codebase-seam-deletion-test", "lens:skill-trigger-outcome-fit", "lens:anti-pattern-operational-detector"} {
		if reasons[id] != "trigger_mismatch" {
			t.Fatalf("%s exclusion reason = %q, want trigger_mismatch (all reasons: %+v)", id, reasons[id], reasons)
		}
	}
}

func panelSelectionIncludes(selection panel.LensSelection, ref string) bool {
	for _, included := range selection.IncludedRefs {
		if included.Ref == ref {
			return true
		}
	}
	return false
}

func TestPanelRunCallerRequiresLMStudioContextPreflightForLocalDefault(t *testing.T) {
	localBaseURL := "http://" + "127.0.0.1" + ":1234/v1"
	caller, _ := panelRunCaller(panelRunArgs{openAICompatibleURL: localBaseURL})
	gateway, ok := caller.(panel.GatewayCaller)
	if !ok {
		t.Fatalf("caller = %T, want GatewayCaller", caller)
	}
	if !gateway.RequireLoadedContextMax {
		t.Fatal("local LM Studio gateway should require loaded max context preflight")
	}

	caller, _ = panelRunCaller(panelRunArgs{openAICompatibleURL: localBaseURL, skipContextPreflight: true})
	gateway = caller.(panel.GatewayCaller)
	if gateway.RequireLoadedContextMax {
		t.Fatal("explicit skip should disable loaded context preflight")
	}

	caller, _ = panelRunCaller(panelRunArgs{openAICompatibleURL: "https://openrouter.ai/api/v1"})
	gateway = caller.(panel.GatewayCaller)
	if gateway.RequireLoadedContextMax {
		t.Fatal("non-LM-Studio gateway should not require LM Studio context preflight")
	}
}

func TestPanelRunCallerUsesHostContextLimit(t *testing.T) {
	localBaseURL := "http://" + "127.0.0.1" + ":1234/v1"
	caller, refs := panelRunCaller(panelRunArgs{openAICompatibleURL: localBaseURL, hostContextLimit: 65536})
	gateway, ok := caller.(panel.GatewayCaller)
	if !ok {
		t.Fatalf("caller = %T, want GatewayCaller", caller)
	}
	if !gateway.RequireLoadedContextMax {
		t.Fatal("local LM Studio gateway should still preflight loaded context")
	}
	if gateway.ContextPolicy.HostContextLimitTokens != 65536 {
		t.Fatalf("host context limit = %d, want 65536", gateway.ContextPolicy.HostContextLimitTokens)
	}
	if !containsString(refs, "context:host_context_limit=65536") {
		t.Fatalf("evidence refs = %+v, want host context limit ref", refs)
	}
}

func TestPanelRunArgsUsesHostContextLimitEnv(t *testing.T) {
	t.Setenv("COMPOUND_PANEL_HOST_CONTEXT_LIMIT", "32768")
	localBaseURL := "http://" + "127.0.0.1" + ":1234/v1"
	parsed := parsePanelRunArgs([]string{"--plan", "prun_env", "--openai-compatible-url", localBaseURL})
	if parsed.hostContextLimit != 32768 {
		t.Fatalf("host context limit = %d, want 32768", parsed.hostContextLimit)
	}
}

func TestPanelPlanRunShowCLIHappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	captureStdout(t, func() {
		cmdRoute([]string{"import-model-capabilities", filepath.Join("..", "..", "fixtures", "panel", "model-capabilities.yaml")})
	})
	captureStdout(t, func() {
		cmdPanel([]string{"import-lenses", filepath.Join("..", "..", "fixtures", "panel", "lens-manifest.yaml")})
	})
	seedPanelCLITarget(t, homePath, "mcand_example_api_clean")
	seedPanelCLITarget(t, homePath, "mcand_example_api_dissent")

	planOut := captureStdout(t, func() {
		cmdPanel([]string{"plan", "--target-kind", "memory_candidate", "--target", "mcand_example_api_clean", "--target", "mcand_example_api_dissent", "--state", "parked", "--not-privacy-sensitive", "synthetic fixtures", "--limit", "2", "--max-items", "2", "--json"})
	})
	var planned panel.Run
	if err := json.Unmarshal([]byte(planOut), &planned); err != nil {
		t.Fatalf("parse plan JSON: %v\n%s", err, planOut)
	}
	if planned.Status != "planned" || planned.Route.JudgeLane.ModelID != "panel/cheap-judge" {
		t.Fatalf("planned run = %+v", planned)
	}

	runOut := captureStdout(t, func() {
		cmdPanel([]string{"run", "--plan", planned.ID, "--fixtures", filepath.Join("..", "..", "fixtures", "panel"), "--json"})
	})
	var completed panel.Run
	if err := json.Unmarshal([]byte(runOut), &completed); err != nil {
		t.Fatalf("parse run JSON: %v\n%s", err, runOut)
	}
	if completed.Status != "verdicts_recorded" || len(completed.Findings) != 2 {
		t.Fatalf("completed run = %+v", completed)
	}

	showOut := captureStdout(t, func() { cmdPanel([]string{"show", planned.ID}) })
	for _, want := range []string{"- status: verdicts_recorded", "decision=promote", "verdict:", "omitted: panel/free-judge-tripwire reason=free lane may not judge"} {
		if !strings.Contains(showOut, want) {
			t.Fatalf("show output missing %q:\n%s", want, showOut)
		}
	}

	applyOut := captureStdout(t, func() {
		cmdPanel([]string{"run", "--plan", planned.ID, "--apply", "--json"})
	})
	var applied panel.Run
	if err := json.Unmarshal([]byte(applyOut), &applied); err != nil {
		t.Fatalf("parse apply JSON: %v\n%s", err, applyOut)
	}
	if applied.Status != "applied" || applied.WorkID == "" || applied.ReceiptID == "" || applied.TransitionID == "" {
		t.Fatalf("applied run = %+v", applied)
	}
	reapplyOut := captureStdout(t, func() {
		cmdPanel([]string{"run", "--plan", planned.ID, "--apply", "--json"})
	})
	var reapplied panel.Run
	if err := json.Unmarshal([]byte(reapplyOut), &reapplied); err != nil {
		t.Fatalf("parse reapply JSON: %v\n%s", err, reapplyOut)
	}
	if reapplied.Status != "applied" || reapplied.ReceiptID == "" || reapplied.TransitionID == "" {
		t.Fatalf("reapplied run = %+v", reapplied)
	}
	applyShow := captureStdout(t, func() {
		cmdPanel([]string{"show", planned.ID})
	})
	for _, want := range []string{"- status: applied", "- work:", "- receipt:", "application: mapped=promote status=applied", "claim:"} {
		if !strings.Contains(applyShow, want) {
			t.Fatalf("apply show output missing %q:\n%s", want, applyShow)
		}
	}
}

func TestPanelRunCLIExecutesAndAppliesInOneCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	captureStdout(t, func() {
		cmdRoute([]string{"import-model-capabilities", filepath.Join("..", "..", "fixtures", "panel", "model-capabilities.yaml")})
	})
	captureStdout(t, func() {
		cmdPanel([]string{"import-lenses", filepath.Join("..", "..", "fixtures", "panel", "lens-manifest.yaml")})
	})
	seedPanelCLITarget(t, homePath, "mcand_example_api_clean")

	planOut := captureStdout(t, func() {
		cmdPanel([]string{"plan", "--target-kind", "memory_candidate", "--target", "mcand_example_api_clean", "--state", "parked", "--not-privacy-sensitive", "synthetic fixtures", "--limit", "1", "--max-items", "1", "--json"})
	})
	var planned panel.Run
	if err := json.Unmarshal([]byte(planOut), &planned); err != nil {
		t.Fatalf("parse plan JSON: %v\n%s", err, planOut)
	}

	runOut := captureStdout(t, func() {
		cmdPanel([]string{"run", "--plan", planned.ID, "--fixtures", filepath.Join("..", "..", "fixtures", "panel"), "--apply", "--json"})
	})
	var applied panel.Run
	if err := json.Unmarshal([]byte(runOut), &applied); err != nil {
		t.Fatalf("parse applied run JSON: %v\n%s", err, runOut)
	}
	if applied.Status != "applied" || applied.ReceiptID == "" || applied.TransitionID == "" {
		t.Fatalf("applied run = %+v, want applied with receipt and transition", applied)
	}
	if len(applied.Findings) != 1 || !applied.Findings[0].Application.Applied || applied.Findings[0].Application.ClaimRef == "" {
		t.Fatalf("finding application = %+v, want promoted claim", applied.Findings)
	}
}

func TestPanelVerdictRecordCLIAnnotatesQueue(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	paths, err := reviewartifact.Write(homePath, "panel-verdicts", "panel_verdict_record_cli", map[string]any{
		"schema_version": "compound.panel_verdict.v0.1",
		"id":             "panel_verdict_record_cli",
		"created_at":     cliTestNow.Format("2006-01-02T15:04:05Z"),
		"run_id":         "prun_record_cli",
		"target": map[string]any{
			"kind":    "memory_candidate",
			"ref":     "cand_panel_record_cli",
			"summary": "candidate needs human adjudication",
			"payload": map[string]any{
				"object": "Operator should decide whether this memory becomes durable strategy.",
			},
		},
		"lens_findings": []map[string]any{{
			"lens_ref":           "lens:privacy",
			"route_evidence_ref": "route_panel_record",
			"status":             "failed",
		}},
		"synthesis": map[string]any{
			"schema_version": "compound.panel_synthesis.v0.1",
			"decision":       "needs_human",
			"confidence":     0.62,
			"rationale":      "privacy lens failed",
		},
		"evidence_refs": []string{"route_panel_record"},
	}, " ", "# Panel verdict\n")
	if err != nil {
		t.Fatal(err)
	}

	pendingOut := captureStdout(t, func() {
		cmdReviews([]string{"--area", "panel-verdicts", "--json"})
	})
	var pending reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(pendingOut), &pending); err != nil {
		t.Fatalf("parse pending reviews report: %v\n%s", err, pendingOut)
	}
	if len(pending.Items) != 1 || pending.Items[0].DecisionCard == nil {
		t.Fatalf("pending panel verdict should expose decision card: %+v", pending.Items)
	}
	card := pending.Items[0].DecisionCard
	if !strings.Contains(card.Claim, "durable strategy") || strings.Contains(card.Claim, "cand_panel_record_cli") {
		t.Fatalf("decision card claim = %q", card.Claim)
	}
	if len(card.Choices) != 4 || len(pending.Items[0].Actions) != 5 {
		t.Fatalf("decision card choices/actions = %d/%d, want 4 choices and 5 actions (4 shell choices + 1 clean member action)", len(card.Choices), len(pending.Items[0].Actions))
	}
	if !strings.Contains(card.Choices[0].Command, "compound panel verdict-record") ||
		!strings.Contains(card.Choices[0].Command, "compound memory adjudicate 'cand_panel_record_cli' --verdict 'promote'") {
		t.Fatalf("decision card command should use canonical CLI path: %q", card.Choices[0].Command)
	}

	out := captureStdout(t, func() {
		cmdPanel([]string{"verdict-record", "--review", paths.PlanPath, "--rationale", "owner reviewed panel request; keep target parked until human evidence exists", "--reviewed-by", "codex", "--json"})
	})
	var record panel.VerdictHumanReviewReport
	if err := json.Unmarshal([]byte(out), &record); err != nil {
		t.Fatalf("parse verdict-record JSON: %v\n%s", err, out)
	}
	if record.Status != "recorded" || record.Decision != "needs_human" || record.ReviewPath == "" {
		t.Fatalf("record = %+v, want recorded needs_human", record)
	}
	if record.ReviewedBy != "codex" {
		t.Fatalf("reviewed_by = %q, want codex", record.ReviewedBy)
	}

	queueOut := captureStdout(t, func() {
		cmdReviews([]string{"--area", "panel-verdicts", "--json"})
	})
	var report reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(queueOut), &report); err != nil {
		t.Fatalf("parse reviews report: %v\n%s", err, queueOut)
	}
	if report.Counts.Total != 1 || report.Counts.ReviewRequired != 0 || report.Counts.OK != 1 {
		t.Fatalf("recorded panel verdict should be ok: %+v", report.Counts)
	}
	if report.Counts.ByStatus["recorded"] != 1 {
		t.Fatalf("recorded panel verdict status missing: %+v", report.Counts.ByStatus)
	}
	if len(report.Items) != 1 || report.Items[0].Status != "recorded" || report.Items[0].Readiness != "review_recorded" {
		t.Fatalf("panel verdict recorded hold should be review_recorded: %+v", report.Items)
	}
	item := report.Items[0]
	if len(item.Actions) != 0 || !strings.Contains(item.NextAction, "keep target parked") {
		t.Fatalf("recorded panel verdict should expose no-action next step: %+v", item)
	}
}

func TestPanelVerdictRecordGroupCLI(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	runID := "prun_cli_group"
	store, err := panel.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRun(panel.Run{
		SchemaVersion: panel.RunSchemaVersion,
		ID:            runID,
		CreatedAt:     cliTestNow.Format(time.RFC3339),
		Status:        "running",
		TargetKind:    "memory_candidate",
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"panel_verdict_group_a", "panel_verdict_group_b"} {
		verdict := map[string]any{
			"schema_version": "compound.panel_verdict.v0.1",
			"id":             id,
			"run_id":         runID,
			"status":         "review_required",
			"target": map[string]any{
				"kind": "memory_candidate",
				"ref":  "cand_" + id,
				"payload": map[string]any{
					"object": "operator should inspect " + id,
				},
			},
			"synthesis": map[string]any{
				"decision":   "needs_human",
				"confidence": 0.4,
				"rationale":  "panel asked for operator review",
			},
			"evidence_refs": []string{"artifact:" + id},
		}
		if _, err := reviewartifact.Write(homePath, "panel-verdicts", id, verdict, " ", "# Panel verdict\n"); err != nil {
			t.Fatal(err)
		}
	}

	out := captureStdout(t, func() {
		cmdPanel([]string{"verdict-record", "--group", "panel-verdicts|" + runID + "|needs_human", "--rationale", "recorded as one grouped decision", "--reviewed-by", "codex", "--json"})
	})
	var report panel.VerdictHumanReviewGroupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse group verdict-record JSON: %v\n%s", err, out)
	}
	if report.Status != "recorded" || report.Count != 2 || report.GroupKey != "panel-verdicts|"+runID+"|needs_human" {
		t.Fatalf("group report = %+v, want recorded count=2", report)
	}
	if strings.Join(report.VerdictIDs, ",") != "panel_verdict_group_a,panel_verdict_group_b" {
		t.Fatalf("verdict ids = %+v", report.VerdictIDs)
	}
	if report.ReviewedBy != "codex" {
		t.Fatalf("reviewed_by = %q, want codex", report.ReviewedBy)
	}
}

func TestPanelFeedbackPlanReviewCLIFeedsCapabilityGraph(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	graph, err := capgraph.Open(homePath)
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
	verdictPath, err := reviewartifact.WriteJSON(homePath, "panel-verdicts", "panel_feedback_cli.json", map[string]any{
		"schema_version": "compound.panel_verdict.v0.1",
		"id":             "panel_feedback_cli",
		"created_at":     cliTestNow.Format("2006-01-02T15:04:05Z"),
		"run_id":         "prun_panel_feedback_cli",
		"target": map[string]any{
			"kind":    "decision_candidate",
			"ref":     "dc_panel_feedback_cli",
			"summary": "decision candidate lacks falsifier",
			"payload": map[string]any{"statement": "adopt unreviewed capability"},
		},
		"lens_findings": []map[string]any{{
			"lens_ref":           "lens:decision-falsifier-review",
			"route_evidence_ref": "route_panel_feedback_cli",
			"confidence":         0.77,
			"status":             "completed",
			"findings": []map[string]any{{
				"claim":         "missing falsifier",
				"severity":      "high",
				"evidence_refs": []string{"candidate:dc_panel_feedback_cli"},
			}},
		}},
		"synthesis": map[string]any{
			"schema_version": "compound.panel_synthesis.v0.1",
			"decision":       "park",
			"confidence":     0.81,
			"rationale":      "review before capability authority changes",
		},
		"evidence_refs": []string{"candidate:dc_panel_feedback_cli"},
	}, " ")
	if err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdPanel([]string{"feedback-plan", "--verdict", verdictPath, "--json"})
	})
	var planned struct {
		Plan  panel.PanelCapabilityFeedbackPlan  `json:"plan"`
		Paths panel.PanelCapabilityFeedbackPaths `json:"paths"`
	}
	if err := json.Unmarshal([]byte(out), &planned); err != nil {
		t.Fatalf("parse feedback-plan JSON: %v\n%s", err, out)
	}
	if planned.Plan.Status != "review_required" || len(planned.Plan.FeedbackCandidates) != 1 || planned.Paths.ReviewYAMLPath == "" {
		t.Fatalf("planned feedback = %+v", planned)
	}

	queueOut := captureStdout(t, func() {
		cmdReviews([]string{"--area", panel.PanelCapabilityFeedbackReviewArea, "--json"})
	})
	var queue reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(queueOut), &queue); err != nil {
		t.Fatalf("parse feedback queue: %v\n%s", err, queueOut)
	}
	if queue.Counts.ReviewRequired != 1 || queue.Counts.DecisionCardCount != 1 || queue.Counts.ActionableCount != 1 {
		t.Fatalf("feedback queue counts = %+v items=%+v", queue.Counts, queue.Items)
	}

	reviewDraft := string(readFile(t, planned.Paths.ReviewYAMLPath))
	reviewDraft = strings.ReplaceAll(reviewDraft, "TODO_apply_feedback_reject_keep_needs_review", "apply_feedback")
	reviewDraft = strings.ReplaceAll(reviewDraft, "TODO_apply_or_reject", "apply")
	reviewDraft = strings.ReplaceAll(reviewDraft, "rationale: \"\"", "rationale: \"reviewed panel lens should validate capability feedback\"")
	reviewDraft = strings.ReplaceAll(reviewDraft, "reviewed_by: \"\"", "reviewed_by: \"codex\"")
	if err := os.WriteFile(planned.Paths.ReviewYAMLPath, []byte(reviewDraft), 0o644); err != nil {
		t.Fatal(err)
	}

	out = captureStdout(t, func() {
		cmdPanel([]string{"feedback-review", planned.Paths.ReviewYAMLPath, "--json"})
	})
	var report panel.PanelCapabilityFeedbackReviewReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse feedback-review JSON: %v\n%s", err, out)
	}
	if report.Status != "applied_feedback" || report.AppliedEdges != 1 {
		t.Fatalf("feedback review report = %+v", report)
	}
	stats, err := graph.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.EdgeKindCounts[capgraph.EdValidatedBy] != 1 {
		t.Fatalf("capability graph not updated by reviewed feedback: %+v", stats)
	}
}

func seedPanelCLITarget(t *testing.T, homePath, id string) {
	t.Helper()
	data := readFile(t, filepath.Join("..", "..", "fixtures", "panel", "targets", id+".json"))
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("decode target fixture %s: %v", id, err)
	}
	if err := candidates.WriteCandidate(homePath, candidates.Memory, id, payload); err != nil {
		t.Fatalf("write candidate %s: %v", id, err)
	}
}

func TestPanelRunCLILiveGatewayEndToEnd(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	t.Setenv("FAKE_EMPTY", "")
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	captureStdout(t, func() {
		cmdRoute([]string{"import-model-capabilities", filepath.Join("..", "..", "fixtures", "panel", "model-capabilities.yaml")})
	})
	captureStdout(t, func() {
		cmdPanel([]string{"import-lenses", filepath.Join("..", "..", "fixtures", "panel", "lens-manifest.yaml")})
	})
	seedPanelCLITarget(t, homePath, "mcand_example_api_clean")

	modelCalls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %q, want /chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("authorization = %q, want empty for FAKE_EMPTY", got)
		}
		var body struct {
			Model    string              `json:"model"`
			Messages []map[string]string `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode gateway request: %v", err)
		}
		modelCalls[body.Model]++
		user := ""
		if len(body.Messages) > 1 {
			user = body.Messages[1]["content"]
		}
		content := `{"confidence":0.8,"findings":[]}`
		if strings.Contains(user, "lens_outputs") {
			content = `{"decision":"promote","confidence":0.82,"rationale":"httptest judge","dissent":[]}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": content}}},
			"usage":   map[string]int{"total_tokens": 5},
		})
	}))
	defer server.Close()

	planOut := captureStdout(t, func() {
		cmdPanel([]string{"plan", "--target-kind", "memory_candidate", "--target", "mcand_example_api_clean", "--state", "parked", "--not-privacy-sensitive", "synthetic fixtures", "--max-usd-micros", "1", "--max-items", "1", "--json"})
	})
	var planned panel.Run
	if err := json.Unmarshal([]byte(planOut), &planned); err != nil {
		t.Fatalf("parse plan JSON: %v; output=%s", err, planOut)
	}
	runOut := captureStdout(t, func() {
		cmdPanel([]string{"run", "--plan", planned.ID, "--openai-compatible-url", server.URL, "--api-key-env", "FAKE_EMPTY", "--model-override", "panel/free-extract=served-lens", "--model-override", "panel/cheap-judge=served-judge", "--json"})
	})
	var completed panel.Run
	if err := json.Unmarshal([]byte(runOut), &completed); err != nil {
		t.Fatalf("parse run JSON: %v; output=%s", err, runOut)
	}
	if completed.Status != "verdicts_recorded" || len(completed.Findings) != 1 {
		t.Fatalf("completed run = %+v", completed)
	}
	for _, want := range []string{"override:model:panel/free-extract=served-lens", "override:model:panel/cheap-judge=served-judge", "cost_unknown"} {
		if !containsString(completed.EvidenceRefs, want) {
			t.Fatalf("evidence refs missing %q: %v", want, completed.EvidenceRefs)
		}
	}
	if modelCalls["served-lens"] != len(completed.LensSet.Included) || modelCalls["served-judge"] != 1 {
		t.Fatalf("model calls = %+v", modelCalls)
	}
	rowsBefore := panelCLIRouteEvidenceRows(t, homePath)
	callsBefore := modelCalls["served-lens"] + modelCalls["served-judge"]
	rerunOut := captureStdout(t, func() {
		cmdPanel([]string{"run", "--plan", planned.ID, "--openai-compatible-url", server.URL, "--api-key-env", "FAKE_EMPTY", "--model-override", "panel/free-extract=served-lens", "--model-override", "panel/cheap-judge=served-judge", "--json"})
	})
	var rerun panel.Run
	if err := json.Unmarshal([]byte(rerunOut), &rerun); err != nil {
		t.Fatalf("parse rerun JSON: %v; output=%s", err, rerunOut)
	}
	if rerun.Status != "verdicts_recorded" || panelCLIRouteEvidenceRows(t, homePath) != rowsBefore {
		t.Fatalf("rerun status=%s before=%d after=%d", rerun.Status, rowsBefore, panelCLIRouteEvidenceRows(t, homePath))
	}
	if callsAfter := modelCalls["served-lens"] + modelCalls["served-judge"]; callsAfter != callsBefore {
		t.Fatalf("rerun called gateway: before=%d after=%d", callsBefore, callsAfter)
	}
}

func TestPanelRunHumanOutputShowsBlockedNextAction(t *testing.T) {
	out := captureStdout(t, func() {
		printPanelRun(panel.Run{
			ID:            "prun_blocked_budget",
			Status:        "blocked",
			TargetKind:    "memory_candidate",
			StoppedReason: "budget:metered_lane_requires_max_usd_micros",
			NextAction:    "create a new panel plan with explicit --max-usd-micros plus --max-items or --max-tokens, then run only that bounded plan",
		})
	})
	for _, want := range []string{
		"- status: blocked",
		"- stopped_reason: budget:metered_lane_requires_max_usd_micros",
		"- next_action: create a new panel plan with explicit --max-usd-micros plus --max-items or --max-tokens",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("panel run output missing %q:\n%s", want, out)
		}
	}
}

func panelCLIRouteEvidenceRows(t *testing.T, homePath string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(homePath, "ledger", "execution-route-evidence.jsonl"))
	if err != nil {
		t.Fatalf("read route evidence: %v", err)
	}
	rows := 0
	for _, raw := range strings.Split(string(data), string([]byte{10})) {
		if strings.TrimSpace(raw) != "" {
			rows++
		}
	}
	return rows
}
