package panel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/envelope"
)

func TestLoadComparisonTargetsFiltersAwaitingStrictDecodeAndSummary(t *testing.T) {
	home := t.TempDir()
	writeComparisonCandidateForPanelTest(t, home, "cmp_candidate_better", "awaiting_verdict", "lane/base", "lane/candidate", 120, 80, 340, 220)
	writeComparisonCandidateForPanelTest(t, home, "cmp_judged", "judged", "lane/base", "lane/candidate", 120, 80, 340, 220)

	got, err := LoadComparisonTargets(home, 1)
	if err != nil {
		t.Fatalf("load comparison targets: %v", err)
	}
	if len(got) != 1 || got[0].Kind != "comparison" || got[0].Ref != "cmp_candidate_better" || got[0].Visibility != "internal" {
		t.Fatalf("comparison target = %+v", got)
	}
	for _, want := range []string{"judge-critique", "same input", "baseline=lane/base", "candidate=lane/candidate", "tokens_delta=-40", "latency_delta_ms=-120"} {
		if !strings.Contains(got[0].Summary, want) {
			t.Fatalf("summary missing %q: %s", want, got[0].Summary)
		}
	}
	var raw map[string]any
	if err := json.Unmarshal(got[0].Payload, &raw); err != nil {
		t.Fatalf("payload is not raw json: %v", err)
	}
	if raw["id"] != "cmp_candidate_better" {
		t.Fatalf("payload id = %v", raw["id"])
	}

	all, err := LoadComparisonTargets(home, 0)
	if err != nil {
		t.Fatalf("load all comparison targets: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("comparison target count = %d, want 1", len(all))
	}

	writeJSONFileForPanelTest(t, filepath.Join(home, "inbox", "comparisons", "cmp_unknown.json"), map[string]any{
		"schema_version":   "compound.comparison_candidate.v0.1",
		"id":               "cmp_unknown",
		"capability_class": "judge-critique",
		"input_summary":    "unknown field must fail",
		"input_digest":     "sha256:unknown",
		"baseline":         map[string]any{"lane_id": "lane/base", "output": "ok", "total_tokens": 1, "latency_ms": 1},
		"candidate":        map[string]any{"lane_id": "lane/candidate", "output": "ok", "total_tokens": 1, "latency_ms": 1},
		"state":            "awaiting_verdict",
		"created_at":       "2026-07-01T00:00:00Z",
		"extra":            "strict decode should reject this",
	})
	if _, err := LoadComparisonTargets(home, 0); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("strict decode error = %v", err)
	}
}

func TestBuildPlanComparisonTargetsLensesAndPrivacyDefault(t *testing.T) {
	home := setupPanelPlanHome(t)
	writeComparisonFixtureForPanelTest(t, home, "cmp_candidate_better")
	writeComparisonFixtureForPanelTest(t, home, "cmp_inconclusive")

	run, err := BuildPlan(home, PlanInput{TargetKind: "comparison", Limit: 2}, fixedPanelNow)
	if err != nil {
		t.Fatalf("build comparison plan: %v", err)
	}
	if run.TargetKind != "comparison" || len(run.Targets) != 2 || run.CountsBefore["awaiting_verdict"] != 2 {
		t.Fatalf("comparison plan = %+v", run)
	}
	if !containsString(run.EvidenceRefs, "privacy_default:not_sensitive:comparison") {
		t.Fatalf("comparison privacy evidence = %v", run.EvidenceRefs)
	}
	for _, want := range []string{"lens:comparison-correctness", "lens:comparison-contract-fidelity", "lens:comparison-cost-latency"} {
		if !includedLensRefForPanelTest(run.LensSet.Included, want) {
			t.Fatalf("comparison lens %s missing from %+v", want, run.LensSet.Included)
		}
	}
	if includedLensRefForPanelTest(run.LensSet.Included, "lens:inversion-falsification") {
		t.Fatalf("generic inversion lens included for comparison plan: %+v", run.LensSet.Included)
	}
}

func TestPanelFixtureE2EComparisonDryRun(t *testing.T) {
	home := setupPanelPlanHome(t)
	writeComparisonFixtureForPanelTest(t, home, "cmp_candidate_better")
	writeComparisonFixtureForPanelTest(t, home, "cmp_inconclusive")

	run, err := BuildPlan(home, PlanInput{TargetKind: "comparison", TargetRefs: []string{"cmp_candidate_better", "cmp_inconclusive"}}, fixedPanelNow)
	if err != nil {
		t.Fatalf("build comparison plan: %v", err)
	}
	completed, err := ExecuteDryRun(context.Background(), home, run, FixtureCaller{Dir: filepath.Join("..", "..", "fixtures", "panel")}, fixedPanelNow)
	if err != nil {
		t.Fatalf("dry-run comparison: %v", err)
	}
	if completed.Status != "verdicts_recorded" || len(completed.Findings) != 2 {
		t.Fatalf("completed comparison run = %+v", completed)
	}
	decisions := map[string]string{}
	for _, finding := range completed.Findings {
		decisions[finding.TargetRef] = finding.Synthesis.Decision
		if len(finding.LensFindings) != 3 || finding.VerdictArtifact == "" {
			t.Fatalf("finding did not record all comparison lenses/artifact: %+v", finding)
		}
	}
	if decisions["cmp_candidate_better"] != "promote" || decisions["cmp_inconclusive"] != "park" {
		t.Fatalf("comparison decisions = %+v", decisions)
	}
}

func TestApplyComparisonStampsCandidateEvidenceAndReapplyIdempotent(t *testing.T) {
	home := setupPanelPlanHome(t)
	writeComparisonFixtureForPanelTest(t, home, "cmp_candidate_better")
	artifact := writeApplyVerdictArtifact(t, home, "cmp_candidate_better", "promote")
	run := newPanelApplyRunForTest("comparison")
	run.Targets = []Target{{Ref: "cmp_candidate_better", Privacy: "internal", StateBefore: "awaiting_verdict"}}
	run.Findings = []TargetFinding{{
		TargetRef:       "cmp_candidate_better",
		VerdictArtifact: artifact,
		Synthesis: Synthesis{
			SchemaVersion: SynthesisSchemaVersion,
			Decision:      "promote",
			Confidence:    0.91,
			Rationale:     "candidate is more complete",
			Dissent:       []Dissent{},
		},
	}}
	run.CountsBefore = map[string]int{"awaiting_verdict": 1}
	run.CountsAfter = map[string]int{"awaiting_verdict": 1}

	got, err := Apply(home, run, incrementingPanelNowForTest())
	if err != nil {
		t.Fatalf("apply comparison: %v", err)
	}
	if got.Status != "applied" || got.WorkID == "" || got.ReceiptID == "" || got.TransitionID == "" {
		t.Fatalf("comparison apply proof refs not populated: %+v", got)
	}
	if got.Findings[0].Application.MappedVerdict != "prefer_candidate" || !got.Findings[0].Application.Applied {
		t.Fatalf("comparison application = %+v", got.Findings[0].Application)
	}
	var updated struct {
		State           string `json:"state"`
		Verdict         string `json:"verdict"`
		VerdictArtifact string `json:"verdict_artifact"`
	}
	readJSONForTest(t, filepath.Join(home, "inbox", "comparisons", "cmp_candidate_better.json"), &updated)
	if updated.State != "judged" || updated.Verdict != "prefer_candidate" || updated.VerdictArtifact != artifact {
		t.Fatalf("updated comparison candidate = %+v", updated)
	}
	var receipt envelope.Receipt
	readJSONForTest(t, filepath.Join(home, "events", got.ReceiptID+".json"), &receipt)
	if receipt.MoveType != "audit_only" || receipt.StateDelta.Changed {
		t.Fatalf("comparison receipt = %+v", receipt)
	}
	ledgerPath := filepath.Join(home, "ledger", "comparison-evidence.jsonl")
	if lines := countLines(t, ledgerPath); lines != 1 {
		t.Fatalf("comparison evidence lines = %d, want 1", lines)
	}
	rows := readComparisonEvidenceRowsForPanelTest(t, ledgerPath)
	if rows[0].ComparisonID != "cmp_candidate_better" || rows[0].CapabilityClass != "judge-critique" || rows[0].BaselineLane != "mac/baseline" || rows[0].CandidateLane != "mac/candidate" || rows[0].Decision != "prefer_candidate" || rows[0].RunID != run.ID || rows[0].VerdictArtifact != artifact {
		t.Fatalf("comparison evidence row = %+v", rows[0])
	}

	again, err := Apply(home, got, incrementingPanelNowForTest())
	if err != nil {
		t.Fatalf("reapply comparison: %v", err)
	}
	if again.Findings[0].Application.Note != "already applied; skipped" {
		t.Fatalf("reapply note = %q", again.Findings[0].Application.Note)
	}
	if lines := countLines(t, ledgerPath); lines != 1 {
		t.Fatalf("comparison evidence reapply lines = %d, want 1", lines)
	}
}

func TestComparisonDecisionMapping(t *testing.T) {
	tests := []struct {
		decision string
		want     string
	}{
		{decision: "promote", want: "prefer_candidate"},
		{decision: "reject", want: "prefer_baseline"},
		{decision: "park", want: "inconclusive"},
		{decision: "needs_human", want: "needs_human"},
		{decision: "needs_trial", want: "needs_trial"},
	}
	for _, tt := range tests {
		t.Run(tt.decision, func(t *testing.T) {
			if got := mapComparisonDecision(tt.decision); got != tt.want {
				t.Fatalf("mapComparisonDecision(%q) = %q, want %q", tt.decision, got, tt.want)
			}
		})
	}
}

func writeComparisonFixtureForPanelTest(t *testing.T, home, id string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "panel", "targets", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "inbox", "comparisons", id+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeComparisonCandidateForPanelTest(t *testing.T, home, id, state, baselineLane, candidateLane string, baselineTokens, candidateTokens int, baselineLatency, candidateLatency int64) {
	t.Helper()
	writeJSONFileForPanelTest(t, filepath.Join(home, "inbox", "comparisons", id+".json"), map[string]any{
		"schema_version":   "compound.comparison_candidate.v0.1",
		"id":               id,
		"capability_class": "judge-critique",
		"input_summary":    "same input",
		"input_digest":     "sha256:" + id,
		"baseline": map[string]any{
			"lane_id":      baselineLane,
			"output":       "baseline answer",
			"total_tokens": baselineTokens,
			"latency_ms":   baselineLatency,
		},
		"candidate": map[string]any{
			"lane_id":      candidateLane,
			"output":       "candidate answer",
			"total_tokens": candidateTokens,
			"latency_ms":   candidateLatency,
		},
		"state":      state,
		"created_at": "2026-07-01T00:00:00Z",
	})
}

type comparisonEvidenceRowForPanelTest struct {
	ComparisonID    string  `json:"comparison_id"`
	CapabilityClass string  `json:"capability_class"`
	BaselineLane    string  `json:"baseline_lane"`
	CandidateLane   string  `json:"candidate_lane"`
	Decision        string  `json:"decision"`
	Confidence      float64 `json:"confidence"`
	RunID           string  `json:"run_id"`
	VerdictArtifact string  `json:"verdict_artifact"`
	CreatedAt       string  `json:"created_at"`
}

func readComparisonEvidenceRowsForPanelTest(t *testing.T, path string) []comparisonEvidenceRowForPanelTest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	rows := make([]comparisonEvidenceRowForPanelTest, 0, len(lines))
	for _, line := range lines {
		var row comparisonEvidenceRowForPanelTest
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("parse comparison evidence row: %v", err)
		}
		rows = append(rows, row)
	}
	return rows
}

func includedLensRefForPanelTest(lenses []IncludedLens, want string) bool {
	for _, lens := range lenses {
		if lens.Ref == want {
			return true
		}
	}
	return false
}

func TestComparisonEvidenceLedgerSurvivesMultipleRows(t *testing.T) {
	home := t.TempDir()
	rows := []comparisonEvidenceRow{
		{ComparisonID: "cmp_a", CapabilityClass: "judge-critique", BaselineLane: "lane/base", CandidateLane: "lane/cand-a", Decision: "prefer_baseline", Confidence: 0.9, RunID: "prun_1", CreatedAt: "2026-07-02T00:00:00Z"},
		{ComparisonID: "cmp_b", CapabilityClass: "judge-critique", BaselineLane: "lane/base", CandidateLane: "lane/cand-b", Decision: "inconclusive", Confidence: 0.6, RunID: "prun_1", CreatedAt: "2026-07-02T00:01:00Z"},
		{ComparisonID: "cmp_c", CapabilityClass: "judge-critique", BaselineLane: "lane/base", CandidateLane: "lane/cand-c", Decision: "prefer_candidate", Confidence: 0.95, RunID: "prun_2", CreatedAt: "2026-07-02T00:02:00Z"},
	}
	for _, row := range rows {
		if err := appendComparisonEvidenceRow(home, row); err != nil {
			t.Fatalf("append %s to ledger with existing rows: %v", row.ComparisonID, err)
		}
	}
	// Idempotency must still hold once the ledger holds multiple newline-delimited rows.
	if err := appendComparisonEvidenceRow(home, rows[0]); err != nil {
		t.Fatalf("idempotent re-append over multi-row ledger: %v", err)
	}
	data, err := os.ReadFile(comparisonEvidenceLedgerPath(home))
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("ledger rows = %d, want 3 (no duplicates, all rows kept)", len(lines))
	}
	exists, err := comparisonEvidenceRowExists(comparisonEvidenceLedgerPath(home), "cmp_c", "prun_2")
	if err != nil {
		t.Fatalf("row-exists over multi-row ledger: %v", err)
	}
	if !exists {
		t.Fatalf("row cmp_c/prun_2 not found in multi-row ledger")
	}
}

func TestApplyComparisonAppendsLedgerRowBeforeJudgingCandidate(t *testing.T) {
	home := setupPanelPlanHome(t)
	writeComparisonCandidateForPanelTest(t, home, "cmp_order_proof", "awaiting_verdict", "lane/base", "lane/cand", 120, 80, 340, 220)
	run := Run{ID: "prun_order_proof", TargetKind: "comparison"}
	finding := TargetFinding{
		TargetRef: "cmp_order_proof",
		Synthesis: Synthesis{SchemaVersion: "compound.panel_synthesis.v0.1", Decision: "promote", Confidence: 0.9, Rationale: "better"},
	}
	mapped, applied, note, err := applyComparisonVerdict(home, run, &finding, fixedPanelNow)
	if err != nil || !applied || mapped != "prefer_candidate" {
		t.Fatalf("apply: mapped=%q applied=%v note=%q err=%v", mapped, applied, note, err)
	}
	exists, err := comparisonEvidenceRowExists(comparisonEvidenceLedgerPath(home), "cmp_order_proof", "prun_order_proof")
	if err != nil || !exists {
		t.Fatalf("ledger row missing after apply: exists=%v err=%v", exists, err)
	}
	// The judged candidate and the ledger row must move together: a judged
	// candidate without its evidence row is the stranded state this guards.
	raw, err := os.ReadFile(filepath.Join(home, "inbox", "comparisons", "cmp_order_proof.json"))
	if err != nil {
		t.Fatalf("read candidate: %v", err)
	}
	if !strings.Contains(string(raw), "\"judged\"") || !strings.Contains(string(raw), "prefer_candidate") {
		t.Fatalf("candidate not judged with verdict: %s", raw)
	}
}
