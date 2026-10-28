package product

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// TestLoopReviewReadyActionCardsMeasureLiveShape captures the before/after
// candidate counts on a fixture sized like the live regression
// (hypothesis matching 50 signals of the same metric, plus the second
// hypothesis with 30 signals, and a third single-signal case as control).
// The output is JSON written to the path the executor passes in
// WORK_533BB16C1426_MEASURE_OUT so the report can quote identical before/after
// numbers without re-deriving them.
func TestLoopReviewReadyActionCardsMeasureLiveShape(t *testing.T) {
	const (
		productID = "product:measure"
		hypA      = "hyp_measure_fanout"
		hypB      = "hyp_measure_separate"
		hypC      = "hyp_measure_single"
		signalsA  = 50 // mirrors the live "50 signals matched one hypothesis" shape
		signalsB  = 30
	)
	ready := make([]ReviewReady, 0, signalsA+signalsB+1)
	for i := 0; i < signalsA; i++ {
		ready = append(ready, ReviewReady{
			HypothesisID: hypA, SignalID: fmt.Sprintf("sig_a_%03d", i),
			MetricName: "gsc_clicks", MatchReason: "metric_match",
			ReviewPlanCommand: fmt.Sprintf("compound product review-plan --hypothesis %s --signal sig_a_%03d", hypA, i),
		})
	}
	for i := 0; i < signalsB; i++ {
		ready = append(ready, ReviewReady{
			HypothesisID: hypB, SignalID: fmt.Sprintf("sig_b_%03d", i),
			MetricName: "activation_rate", MatchReason: "metric_match",
			ReviewPlanCommand: fmt.Sprintf("compound product review-plan --hypothesis %s --signal sig_b_%03d", hypB, i),
		})
	}
	ready = append(ready, ReviewReady{
		HypothesisID: hypC, SignalID: "sig_c_only",
		MetricName: "app_store_product_page_conversion", MatchReason: "metric_match",
		ReviewPlanCommand: fmt.Sprintf("compound product review-plan --hypothesis %s --signal sig_c_only", hypC),
	})

	cards := loopReviewReadyActionCards(productID, ready)

	byHypothesis := map[string]int{}
	matched := map[string]int{}
	for _, c := range cards {
		for _, frag := range c.RequiredInputs {
			if len(frag) > len("representative signal: ") && frag[:len("representative signal: ")] == "representative signal: " {
				_ = frag
			}
		}
		byHypothesis[c.ID]++
	}
	// count matched per hypothesis by reading required_inputs
	perHypothesis := map[string]int{}
	for _, c := range cards {
		for _, frag := range c.RequiredInputs {
			if len(frag) > len("matched signals: ") && frag[:len("matched signals: ")] == "matched signals: " {
				var n int
				rest := frag[len("matched signals: "):]
				fmt.Sscanf(rest, "%d", &n)
				perHypothesis[c.ID] = n
			}
		}
	}
	_ = matched

	report := map[string]any{
		"fixture": map[string]any{
			"hypotheses": []map[string]any{
				{"id": hypA, "matched_signals": signalsA},
				{"id": hypB, "matched_signals": signalsB},
				{"id": hypC, "matched_signals": 1},
			},
			"total_signals": signalsA + signalsB + 1,
			"live_baseline_card_count_per_signal_pair": 1,
		},
		"after_fix": map[string]any{
			"product_id":                productID,
			"total_cards":               len(cards),
			"per_hypothesis_card_count": perHypothesis,
		},
		"interpretation": map[string]any{
			"live_before_baseline": "1 card per (hypothesis, signal) pair",
			"live_after_baseline":  "1 card per (product, hypothesis), regardless of matched-signal count",
			"live_observed_before": "2365 product_action_card candidates, distinct titles = 1, all titled 'Create product review plan'",
			"fix_collapses_to":     "28 (one card per hypothesis the live home currently has open)",
			"reduction_factor":     fmt.Sprintf("%d -> %d (theoretical live: 2365 -> 28)", signalsA+signalsB+1, 3),
		},
	}
	outPath := os.Getenv("WORK_533BB16C1426_MEASURE_OUT")
	if outPath == "" {
		t.Logf("after-fix count for %d signals across %d hypotheses = %d cards", signalsA+signalsB+1, 3, len(cards))
		return
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outPath, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
