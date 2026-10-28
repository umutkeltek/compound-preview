package main

import (
	"testing"
	"time"

	"compound/internal/mem"
	"compound/internal/reviewartifact"
)

// TestAnnotateSynthesisContradictionQueueFillsFallback covers gap #1's
// synthesis-contradictions area (36 live fallback items): a legacy artifact
// with no status field on disk (writer debt, pre-dates the recorded default)
// must render the §4 sentence and keep its needs_you band.
func TestAnnotateSynthesisContradictionQueueFillsFallback(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	artifact := synthesisContradictionArtifact{
		SchemaVersion: "compound.synthesis_contradiction_review.v0.1",
		ID:            "synthesis_contradiction_test",
		WorkID:        "work_test",
		Proposal: mem.SynthesisContradictionProposal{
			ID:         "synth_contradiction_test",
			ClusterKey: "decisionlifecycle\x1fdecision-lifecycle\x1fstatus",
			ClaimIDs:   []string{"claim_a", "claim_b"},
			Objects: []string{
				"compound owns decisionlifecycle source authority",
				"decisionlifecycle repair queue is closed",
			},
		},
	}
	if _, err := reviewartifact.WriteJSON(homePath, synthesisContradictionArea, artifact.ID+".json", artifact, "  "); err != nil {
		t.Fatal(err)
	}
	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: synthesisContradictionArea}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Items[0].Status != "unknown" || report.Items[0].SummarySource != reviewartifact.SummarySourceFallback {
		t.Fatalf("precondition: legacy no-status artifact should start as unknown/fallback, got %+v", report.Items[0])
	}

	annotateSynthesisReviewQueue(&report)
	reviewartifact.Recount(&report)

	item := report.Items[0]
	want := "Memory holds two claims about compound owns decisionlifecycle source authority that disagree. Pick which is true?"
	if item.OperatorSummary != want {
		t.Fatalf("OperatorSummary = %q, want %q", item.OperatorSummary, want)
	}
	// annotateSynthesisQueueItem forces Status to "recorded" for writer-debt
	// artifacts; the band computed from the ORIGINAL "unknown" status must
	// survive that status rewrite (needs_you, not silently filed away).
	if item.Status != "recorded" {
		t.Fatalf("Status = %q, want recorded (writer-debt normalization)", item.Status)
	}
	if item.Band != reviewartifact.BandNeedsYou {
		t.Fatalf("Band = %q, want needs_you (writer-debt items must stay visible)", item.Band)
	}
}

// TestAnnotateSynthesisSweepQueueFillsFallback covers gap #1's
// synthesis-sweeps area (4 live fallback items).
func TestAnnotateSynthesisSweepQueueFillsFallback(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	artifact := synthesisSweepArtifact{
		SchemaVersion: "compound.synthesis_sweep_artifact.v0.1",
		ID:            "synthesis_sweep_test",
		Status:        "recorded",
		Readiness:     "review_recorded",
		WorkID:        "work_sweep_test",
		Report:        mem.SynthesisSweepReport{ContradictionProposalCount: 2},
	}
	if _, err := reviewartifact.WriteJSON(homePath, synthesisSweepArea, artifact.ID+".json", artifact, "  "); err != nil {
		t.Fatal(err)
	}
	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: synthesisSweepArea}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Items[0].SummarySource != reviewartifact.SummarySourceFallback {
		t.Fatalf("precondition: legacy sweep should start as fallback, got %+v", report.Items[0])
	}
	annotateSynthesisReviewQueue(&report)
	reviewartifact.Recount(&report)

	item := report.Items[0]
	want := "Memory synthesis finished sweep — proposals recorded, nothing applied. Pick at most one repair to open."
	if item.OperatorSummary != want {
		t.Fatalf("OperatorSummary = %q, want %q", item.OperatorSummary, want)
	}
	if item.Band != reviewartifact.BandFiled {
		t.Fatalf("Band = %q, want filed (recorded status)", item.Band)
	}
}

// TestSynthesisContradictionGroupSummaryUsesDedicatedTemplate pins the §4
// group line and the honest "no safe batch verb" reason for
// synthesis-contradictions.
func TestSynthesisContradictionGroupSummaryUsesDedicatedTemplate(t *testing.T) {
	t.Parallel()
	// The registry is populated by this package's init(); exercise it
	// through the same seam Recount/buildQueueGroups uses by round-tripping
	// a minimal report.
	report := reviewartifact.QueueReport{}
	for i := 0; i < 2; i++ {
		report.Items = append(report.Items, reviewartifact.QueueItem{
			Area:            synthesisContradictionArea,
			ID:              "synthesis_contradiction_group_" + string(rune('a'+i)),
			Status:          "unknown",
			GroupKey:        "synthesis-contradictions|work_group_test",
			OperatorSummary: "placeholder",
			ContextLabel:    "placeholder",
			Band:            reviewartifact.BandNeedsYou,
		})
	}
	reviewartifact.Recount(&report)
	if len(report.Groups) != 1 {
		t.Fatalf("want 1 group, got %d: %+v", len(report.Groups), report.Groups)
	}
	got := report.Groups[0]
	want := "Memory found 2 pairs of claims that disagree with each other. Settle them one by one?"
	if got.OperatorSummary != want {
		t.Fatalf("group OperatorSummary = %q, want %q", got.OperatorSummary, want)
	}
	if got.TerminalReason != "each contradiction needs its own read" {
		t.Fatalf("group TerminalReason = %q, want the honest per-item-read reason", got.TerminalReason)
	}
	if got.SafeBatchAction != nil {
		t.Fatalf("group SafeBatchAction = %+v, want nil (no safe batch verb)", got.SafeBatchAction)
	}
}
