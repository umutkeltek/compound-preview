package panel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/capgraph"
)

func secondReadRun(t *testing.T, home, id, model string, findings []TargetFinding) Run {
	t.Helper()
	run := Run{
		SchemaVersion: RunSchemaVersion,
		ID:            id,
		CreatedAt:     "2026-07-18T10:00:00Z",
		Status:        "verdicts_recorded",
		TargetKind:    "decision_candidate",
		Supervised:    true,
		Supervision:   "operator",
		Targets:       []Target{},
		Route: RoutePlan{
			LensLane:  LaneRef{ModelID: model, Tier: "local-batch", GateRole: "eligible"},
			JudgeLane: LaneRef{ModelID: model, Tier: "local-batch", GateRole: "eligible"},
		},
		Findings:     findings,
		CountsBefore: map[string]int{},
		CountsAfter:  map[string]int{},
	}
	for _, f := range findings {
		run.Targets = append(run.Targets, Target{Ref: f.TargetRef, Privacy: "internal", StateBefore: "needs_review"})
	}
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	return run
}

func secondReadFinding(target, model, decision string, lensSeverities map[string]string, lensConfidence map[string]float64) TargetFinding {
	f := TargetFinding{
		TargetRef: target,
		Synthesis: Synthesis{
			SchemaVersion: SynthesisSchemaVersion,
			Decision:      decision,
			Confidence:    0.9,
			Rationale:     "test",
		},
	}
	lensRefs := []string{"lens:evidence-strength", "lens:duplicate-risk"}
	for _, lensRef := range lensRefs {
		lf := LensFinding{LensRef: lensRef, ModelID: model, Status: "completed", Confidence: 0.8}
		if conf, ok := lensConfidence[lensRef]; ok {
			lf.Confidence = conf
		}
		if sev, ok := lensSeverities[lensRef]; ok && sev != "" {
			lf.Findings = []Finding{{Claim: "claim about " + target, Severity: sev}}
		}
		f.LensFindings = append(f.LensFindings, lf)
	}
	return f
}

func countInboxObservations(t *testing.T, home string) int {
	t.Helper()
	dir := filepath.Join(home, candidates.InboxDir(candidates.CapabilityObs))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	count := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			count++
		}
	}
	return count
}

func TestSecondReadDecisionDivergenceEmitsObservation(t *testing.T) {
	home := t.TempDir()
	secondReadRun(t, home, "prun_20260718T100000Z_aaaaaaaaaaaa", "mac/model-a", []TargetFinding{
		secondReadFinding("dcand_1", "mac/model-a", "promote", nil, nil),
	})
	secondReadRun(t, home, "prun_20260718T100100Z_bbbbbbbbbbbb", "mac/model-b", []TargetFinding{
		secondReadFinding("dcand_1", "mac/model-b", "reject", nil, nil),
	})
	report, err := SecondRead(home, SecondReadInput{BaseRunID: "prun_20260718T100000Z_aaaaaaaaaaaa", ReaderRunID: "prun_20260718T100100Z_bbbbbbbbbbbb"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Agreement {
		t.Fatal("expected divergence, got agreement")
	}
	if len(report.Divergences) != 1 || report.Divergences[0].Class != DivergenceDecision {
		t.Fatalf("expected one decision_divergence, got %+v", report.Divergences)
	}
	if len(report.Emitted) != 1 || report.Emitted[0].Skipped {
		t.Fatalf("expected one fresh emission, got %+v", report.Emitted)
	}
	var obs capabilityObservationTarget
	if err := candidates.ReadCandidate(home, candidates.CapabilityObs, report.Emitted[0].ID, &obs); err != nil {
		t.Fatalf("emitted observation unreadable: %v", err)
	}
	if obs.Detail != "reader_divergence" || obs.State != "needs_review" || obs.Verdict != "" {
		t.Fatalf("decision divergence observation misshaped: %+v", obs)
	}
	if !strings.Contains(obs.Statement, "promote") || !strings.Contains(obs.Statement, "reject") {
		t.Fatalf("statement should carry both decisions: %q", obs.Statement)
	}
}

func TestSecondReadRejectsSameReaderModel(t *testing.T) {
	home := t.TempDir()
	secondReadRun(t, home, "prun_20260718T100000Z_aaaaaaaaaaaa", "mac/model-a", []TargetFinding{
		secondReadFinding("dcand_1", "mac/model-a", "promote", nil, nil),
	})
	secondReadRun(t, home, "prun_20260718T100100Z_bbbbbbbbbbbb", "mac/model-a", []TargetFinding{
		secondReadFinding("dcand_1", "mac/model-a", "reject", nil, nil),
	})
	_, err := SecondRead(home, SecondReadInput{BaseRunID: "prun_20260718T100000Z_aaaaaaaaaaaa", ReaderRunID: "prun_20260718T100100Z_bbbbbbbbbbbb"})
	if err == nil || !strings.Contains(err.Error(), "different reader model") {
		t.Fatalf("expected same-model rejection, got %v", err)
	}
	if got := countInboxObservations(t, home); got != 0 {
		t.Fatalf("same-model rejection must not emit, found %d files", got)
	}
}

// Two runs through the SAME gateway lane (auto/extract) but with different
// --model-override readers are DIFFERENT perspectives — the effective model
// lives in override evidence, not the lane id (observed live 2026-07-18).
func TestSecondReadResolvesOverriddenReaderModels(t *testing.T) {
	home := t.TempDir()
	saveWithOverride := func(id, overrideModel, decision string) {
		run := secondReadRun(t, home, id, "auto/extract", []TargetFinding{
			secondReadFinding("dcand_1", "auto/extract", decision, nil, nil),
		})
		run.EvidenceRefs = []string{"override:model:auto/extract=" + overrideModel}
		store, err := Open(home)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SaveRun(run); err != nil {
			t.Fatal(err)
		}
	}
	saveWithOverride("prun_20260718T100000Z_aaaaaaaaaaaa", "qwen.model-a", "promote")
	saveWithOverride("prun_20260718T100100Z_bbbbbbbbbbbb", "gemma.model-b", "reject")
	report, err := SecondRead(home, SecondReadInput{BaseRunID: "prun_20260718T100000Z_aaaaaaaaaaaa", ReaderRunID: "prun_20260718T100100Z_bbbbbbbbbbbb"})
	if err != nil {
		t.Fatalf("override runs through one lane must be accepted as distinct readers: %v", err)
	}
	if report.BaseModel != "qwen.model-a" || report.ReaderModel != "gemma.model-b" {
		t.Fatalf("report must carry effective models, got base=%q reader=%q", report.BaseModel, report.ReaderModel)
	}
	// Same lane AND same overridden model stays redundancy, not perspective.
	saveWithOverride("prun_20260718T100200Z_cccccccccccc", "qwen.model-a", "reject")
	_, err = SecondRead(home, SecondReadInput{BaseRunID: "prun_20260718T100000Z_aaaaaaaaaaaa", ReaderRunID: "prun_20260718T100200Z_cccccccccccc"})
	if err == nil || !strings.Contains(err.Error(), "different reader model") {
		t.Fatalf("same overridden model must be rejected, got %v", err)
	}
}

func TestSecondReadIsIdempotent(t *testing.T) {
	home := t.TempDir()
	secondReadRun(t, home, "prun_20260718T100000Z_aaaaaaaaaaaa", "mac/model-a", []TargetFinding{
		secondReadFinding("dcand_1", "mac/model-a", "promote", nil, nil),
	})
	secondReadRun(t, home, "prun_20260718T100100Z_bbbbbbbbbbbb", "mac/model-b", []TargetFinding{
		secondReadFinding("dcand_1", "mac/model-b", "reject", nil, nil),
	})
	in := SecondReadInput{BaseRunID: "prun_20260718T100000Z_aaaaaaaaaaaa", ReaderRunID: "prun_20260718T100100Z_bbbbbbbbbbbb"}
	if _, err := SecondRead(home, in); err != nil {
		t.Fatal(err)
	}
	before := countInboxObservations(t, home)
	report, err := SecondRead(home, in)
	if err != nil {
		t.Fatal(err)
	}
	if got := countInboxObservations(t, home); got != before {
		t.Fatalf("second run changed inbox: %d -> %d", before, got)
	}
	if len(report.Emitted) != 1 || !report.Emitted[0].Skipped {
		t.Fatalf("re-run should report skipped emission, got %+v", report.Emitted)
	}
}

func TestSecondReadAgreementEmitsNothing(t *testing.T) {
	home := t.TempDir()
	secondReadRun(t, home, "prun_20260718T100000Z_aaaaaaaaaaaa", "mac/model-a", []TargetFinding{
		secondReadFinding("dcand_1", "mac/model-a", "promote", nil, nil),
	})
	secondReadRun(t, home, "prun_20260718T100100Z_bbbbbbbbbbbb", "mac/model-b", []TargetFinding{
		secondReadFinding("dcand_1", "mac/model-b", "promote", nil, nil),
	})
	report, err := SecondRead(home, SecondReadInput{BaseRunID: "prun_20260718T100000Z_aaaaaaaaaaaa", ReaderRunID: "prun_20260718T100100Z_bbbbbbbbbbbb"})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Agreement || len(report.Emitted) != 0 {
		t.Fatalf("expected clean agreement, got %+v", report)
	}
	if got := countInboxObservations(t, home); got != 0 {
		t.Fatalf("agreement must not emit, found %d files", got)
	}
}

func TestSecondReadSeverityBlindspotShapesMissedWhen(t *testing.T) {
	home := t.TempDir()
	secondReadRun(t, home, "prun_20260718T100000Z_aaaaaaaaaaaa", "mac/model-a", []TargetFinding{
		secondReadFinding("dcand_1", "mac/model-a", "promote", map[string]string{"lens:evidence-strength": "low"}, nil),
	})
	secondReadRun(t, home, "prun_20260718T100100Z_bbbbbbbbbbbb", "mac/model-b", []TargetFinding{
		secondReadFinding("dcand_1", "mac/model-b", "promote", map[string]string{"lens:evidence-strength": "high"}, nil),
	})
	report, err := SecondRead(home, SecondReadInput{BaseRunID: "prun_20260718T100000Z_aaaaaaaaaaaa", ReaderRunID: "prun_20260718T100100Z_bbbbbbbbbbbb"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Divergences) != 1 || report.Divergences[0].Class != DivergenceSeverityBlindspot {
		t.Fatalf("expected one severity_blindspot, got %+v", report.Divergences)
	}
	var obs capabilityObservationTarget
	if err := candidates.ReadCandidate(home, candidates.CapabilityObs, report.Emitted[0].ID, &obs); err != nil {
		t.Fatal(err)
	}
	if obs.Detail != capgraph.EdMissedWhen {
		t.Fatalf("blindspot must shape missed_when, got %q", obs.Detail)
	}
	if obs.Statement != "decision_candidate" {
		t.Fatalf("statement must be the target kind context token, got %q", obs.Statement)
	}
	// The BLIND lane (base, which scored low while the reader saw high) is the subject.
	if !strings.Contains(obs.Subject, "model-a") {
		t.Fatalf("subject must name the blind lane, got %q", obs.Subject)
	}
	if obs.Verdict != "" {
		t.Fatalf("emission must not self-adjudicate, got verdict %q", obs.Verdict)
	}
}

// The full compounding wire: an emitted blindspot observation, once adjudicated
// operationalize, must be APPLIED by capgraph.Consume as a missed_when belief
// edge against the blind lane. This test fails if any link in the chain —
// emission shape, candidates inbox path, Consume contract — drifts.
func TestSecondReadBlindspotConsumedByCapgraphAfterOperationalize(t *testing.T) {
	home := t.TempDir()
	secondReadRun(t, home, "prun_20260718T100000Z_aaaaaaaaaaaa", "mac/model-a", []TargetFinding{
		secondReadFinding("dcand_1", "mac/model-a", "promote", map[string]string{"lens:evidence-strength": "low"}, nil),
	})
	secondReadRun(t, home, "prun_20260718T100100Z_bbbbbbbbbbbb", "mac/model-b", []TargetFinding{
		secondReadFinding("dcand_1", "mac/model-b", "promote", map[string]string{"lens:evidence-strength": "high"}, nil),
	})
	report, err := SecondRead(home, SecondReadInput{BaseRunID: "prun_20260718T100000Z_aaaaaaaaaaaa", ReaderRunID: "prun_20260718T100100Z_bbbbbbbbbbbb"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Emitted) != 1 {
		t.Fatalf("expected one emission, got %+v", report.Emitted)
	}
	obsID := report.Emitted[0].ID

	graph, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	// Before adjudication: needs_review with no verdict must NOT apply.
	applied, err := graph.Consume(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if applied != 0 {
		t.Fatalf("unadjudicated observation must not apply, applied=%d", applied)
	}

	var obs capabilityObservationTarget
	if err := candidates.ReadCandidate(home, candidates.CapabilityObs, obsID, &obs); err != nil {
		t.Fatal(err)
	}
	obs.Verdict = candidates.Operationalize
	if err := candidates.WriteCandidate(home, candidates.CapabilityObs, obsID, obs); err != nil {
		t.Fatal(err)
	}
	applied, err = graph.Consume(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Fatalf("operationalized blindspot must apply exactly one belief signal, applied=%d", applied)
	}
}

func TestSecondReadConfidenceGap(t *testing.T) {
	home := t.TempDir()
	secondReadRun(t, home, "prun_20260718T100000Z_aaaaaaaaaaaa", "mac/model-a", []TargetFinding{
		secondReadFinding("dcand_1", "mac/model-a", "promote", nil, map[string]float64{"lens:evidence-strength": 0.95}),
	})
	secondReadRun(t, home, "prun_20260718T100100Z_bbbbbbbbbbbb", "mac/model-b", []TargetFinding{
		secondReadFinding("dcand_1", "mac/model-b", "promote", nil, map[string]float64{"lens:evidence-strength": 0.30}),
	})
	report, err := SecondRead(home, SecondReadInput{BaseRunID: "prun_20260718T100000Z_aaaaaaaaaaaa", ReaderRunID: "prun_20260718T100100Z_bbbbbbbbbbbb"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Divergences) != 1 || report.Divergences[0].Class != DivergenceConfidenceGap {
		t.Fatalf("expected one confidence_gap, got %+v", report.Divergences)
	}
}
