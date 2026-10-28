package panel

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestOperatorCardForVerdictComparisonPrivateTranscript pins the exact
// sentence shape console-legibility gap #2 asked for: candidate/baseline
// names parsed from the target ref (not leaked lane_id/session text), a
// clean capability_class surface, and the "on a private transcript"
// qualifier when the ref carries the PRIV marker.
func TestOperatorCardForVerdictComparisonPrivateTranscript(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"capability_class": "classify-extract",
		"candidate":        map[string]any{"lane_id": "mac/ornith-1.0-35b"},
		"baseline":         map[string]any{"lane_id": "mac/hauhaucs-a3b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	verdict := verdictArtifactPayload{
		SchemaVersion: "compound.panel_verdict.v0.1",
		ID:            "panel_verdict_test",
		RunID:         "prun_test",
		Target: TargetInput{
			Kind:    "comparison",
			Ref:     "cmp_r3_PRIV_ornith_vs_hauhaucs_codex-rollout-2026-06-16T23-49-ba67d2e9",
			Payload: payload,
		},
		Synthesis: Synthesis{Decision: "promote"},
	}
	card, err := OperatorCardForVerdict(verdict)
	if err != nil {
		t.Fatal(err)
	}
	want := "The judge panel says ornith beat hauhaucs at classify-extract on a private transcript. Record the win?"
	if card.Summary != want {
		t.Fatalf("Summary = %q, want %q", card.Summary, want)
	}
	if strings.ContainsAny(card.Summary, "[]") {
		t.Fatalf("Summary leaked a bracket tag: %q", card.Summary)
	}
}

// TestOperatorCardForVerdictComparisonFixtureNotPrivate covers the
// synthetic/fixture comparison shape (no PRIV marker): the sentence must
// name the surface without the private-transcript qualifier. NOTE: this
// payload carries NO lane objects, so the card can only trust the ref's
// documented {candidate}_vs_{baseline} order — for this ref that order is
// wrong (baseline-first), and real verdicts DO carry lane objects; see
// TestOperatorCardForVerdictComparisonOrientsFromPayloadLanes for the
// oriented (correct) sentence on the same fixture.
func TestOperatorCardForVerdictComparisonFixtureNotPrivate(t *testing.T) {
	verdict := verdictArtifactPayload{
		SchemaVersion: "compound.panel_verdict.v0.1",
		ID:            "panel_verdict_fixture",
		RunID:         "prun_test",
		Target: TargetInput{
			Kind:    "comparison",
			Ref:     "cmp_r3_deepseek_vs_autoextract_fixture-07",
			Summary: "classify-extract fixture comparison",
			Payload: json.RawMessage(`{"capability_class":"classify-extract"}`),
		},
		Synthesis: Synthesis{Decision: "reject"},
	}
	card, err := OperatorCardForVerdict(verdict)
	if err != nil {
		t.Fatal(err)
	}
	want := "The judge panel says deepseek lost to autoextract on classify-extract. Record the loss?"
	if card.Summary != want {
		t.Fatalf("Summary = %q, want %q", card.Summary, want)
	}
	if strings.Contains(card.Summary, "private transcript") {
		t.Fatalf("non-private comparison must not claim a private transcript: %q", card.Summary)
	}
}

// TestOperatorCardForVerdictComparisonOrientsFromPayloadLanes pins the
// direction fix for the 2026-07-09 operator report: the live ref
// cmp_r3_deepseek_vs_autoextract is named {baseline}_vs_{candidate} —
// violating the convention comparisonRefPattern documents — so a card built
// from ref order alone inverted winner and loser ("deepseek lost to
// autoextract" while the verdicts rejected auto-extract). When the payload
// carries lane objects, they decide orientation; the ref still supplies the
// plain display names (lane ids must not leak into the sentence).
func TestOperatorCardForVerdictComparisonOrientsFromPayloadLanes(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"capability_class": "classify-extract",
		"baseline":         map[string]any{"lane_id": "switchback/deepseek-v4-pro"},
		"candidate":        map[string]any{"lane_id": "switchback/auto-extract"},
	})
	if err != nil {
		t.Fatal(err)
	}
	verdict := verdictArtifactPayload{
		SchemaVersion: "compound.panel_verdict.v0.1",
		ID:            "panel_verdict_orientation",
		RunID:         "prun_test",
		Target: TargetInput{
			Kind:    "comparison",
			Ref:     "cmp_r3_deepseek_vs_autoextract_fixture-01",
			Payload: payload,
		},
		Synthesis: Synthesis{Decision: "reject"},
	}
	card, err := OperatorCardForVerdict(verdict)
	if err != nil {
		t.Fatal(err)
	}
	want := "The judge panel says autoextract lost to deepseek on classify-extract. Record the loss?"
	if card.Summary != want {
		t.Fatalf("Summary = %q, want %q (candidate/baseline must orient from payload lanes, not ref order)", card.Summary, want)
	}
	if strings.Contains(card.Summary, "switchback/") {
		t.Fatalf("lane id leaked into card sentence: %q", card.Summary)
	}
}
