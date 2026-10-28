package hook

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// The CPU-time budget test needs a yardstick, because CPU time is a property of
// the work AND of the core that ran it. The same hook binary measured ~330ms
// p95 per prompt on the 16-core reference host and 711-882ms on a 10-core
// verifier with 4 performance + 6 efficiency cores while the full suite ran
// beside it (load ~4, under the NumCPU/2 contention signal): an efficiency
// core retires the same instructions in roughly twice the CPU time. Pinning the
// test to efficiency cores on the reference host (background QoS, breaker
// widened for the experiment) reproduces it: raw p95 857ms, calibration 2.66x
// slower, normalized p95 330ms. A mutant that burns ~1.6x more CPU per prompt
// still fails both ways: 548ms on performance cores, 561ms normalized (raw
// 1.36s) on efficiency cores.
//
// So the test runs a fixed calibration workload right before every prompt and
// once after the last, and converts each prompt's CPU time into
// reference-host-equivalent CPU time with the calibration samples that bracket
// it. The 500ms p95 budget then applies to that normalized cost: a hook that
// gets more expensive raises prompt time but not calibration time, so the
// budget still bites on any host; a slower core or a contended cache raises
// both, so it no longer reads as a regression.

// promptScaleCalibrationReference is the median calibration sample (workload
// plus its own GC) measured inside the CPU budget test on the 16-core reference
// host (12 performance + 4 efficiency cores) on 2026-10-25, under a load of
// 8-20 from sibling lanes. It is the host speed at which the historical 500ms
// budget was set. A quiet reference run measures a little lower (~40ms), which
// only makes the factor floor below bind sooner, never loosens the budget.
const promptScaleCalibrationReference = 45 * time.Millisecond

// promptScaleCalibrationRecord is the unit of the calibration workload. Its
// shape (string IDs, tag slices, small string maps, a float) mirrors the claim
// and capability rows the hook folds, so the workload exercises the same
// allocator, map, JSON and hashing paths rather than a cache-resident loop.
type promptScaleCalibrationRecord struct {
	ID    string            `json:"id"`
	Kind  string            `json:"kind"`
	Text  string            `json:"text"`
	Tags  []string          `json:"tags"`
	Attrs map[string]string `json:"attrs"`
	Score float64           `json:"score"`
}

const promptScaleCalibrationRecords = 9000

var promptScaleCalibrationSink [sha256.Size]byte

// promptScaleCalibrationWork is a fixed, deterministic amount of hook-shaped
// work: build records, JSON round-trip them into generic maps, sort, and hash.
// It must never change with the hook: promptScaleCalibrationReference is a
// measurement of exactly this function.
func promptScaleCalibrationWork() {
	recs := make([]promptScaleCalibrationRecord, 0, promptScaleCalibrationRecords)
	for i := 0; i < promptScaleCalibrationRecords; i++ {
		recs = append(recs, promptScaleCalibrationRecord{
			ID:    fmt.Sprintf("clm_%06d", (i*7919)%promptScaleCalibrationRecords),
			Kind:  []string{"decision", "project-fact", "system-state"}[i%3],
			Text:  strings.Repeat(fmt.Sprintf("calibration claim %d accelerate worldwide shipping ", i), 3),
			Tags:  []string{"scale", fmt.Sprintf("t%d", i%17), fmt.Sprintf("u%d", i%29)},
			Attrs: map[string]string{"source": "calibration", "owner": fmt.Sprintf("o%d", i%11), "lane": "prompt"},
			Score: float64(i%97) / 97,
		})
	}
	raw, err := json.Marshal(recs)
	if err != nil {
		panic(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		panic(err)
	}
	sort.Slice(decoded, func(a, b int) bool {
		return decoded[a]["id"].(string) < decoded[b]["id"].(string)
	})
	h := sha256.New()
	for _, row := range decoded {
		h.Write([]byte(row["id"].(string)))
		h.Write([]byte(row["text"].(string)))
	}
	copy(promptScaleCalibrationSink[:], h.Sum(nil))
}

// promptScaleCalibrationSample measures one calibration unit in the same
// RUSAGE_SELF CPU time the prompt measurement uses. It collects its own
// garbage inside the measurement (~3ms) so neither the calibration nor the
// next prompt is billed for the other's GC debt.
func promptScaleCalibrationSample() time.Duration {
	return promptScalePerPromptCPUTime(func() {
		promptScaleCalibrationWork()
		runtime.GC()
	})
}

// promptScaleNormalizeCPU converts each prompt's CPU time into
// reference-host-equivalent CPU time. calibration[i] is the sample taken just
// before prompt i and calibration[i+1] the one just after, so the host factor
// for prompt i is the mean of the two over promptScaleCalibrationReference.
// The factor never drops below 1: a host faster than the reference keeps the
// absolute budget rather than a tighter one.
func promptScaleNormalizeCPU(prompts, calibration []time.Duration, reference time.Duration) ([]time.Duration, []float64, error) {
	if len(prompts) == 0 || len(calibration) != len(prompts)+1 {
		return nil, nil, fmt.Errorf("need one calibration sample around every prompt: %d prompts, %d samples", len(prompts), len(calibration))
	}
	if reference <= 0 {
		return nil, nil, fmt.Errorf("calibration reference %s must be positive", reference)
	}
	normalized := make([]time.Duration, len(prompts))
	factors := make([]float64, len(prompts))
	for i, prompt := range prompts {
		bracket := (calibration[i] + calibration[i+1]) / 2
		factor := float64(bracket) / float64(reference)
		if factor < 1 {
			factor = 1
		}
		factors[i] = factor
		normalized[i] = time.Duration(float64(prompt) / factor)
	}
	return normalized, factors, nil
}

// promptScaleP95AndMax returns the p95 and max of the samples with the index
// rule the CPU budget test has always used.
func promptScaleP95AndMax(samples []time.Duration) (time.Duration, time.Duration) {
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[int(float64(len(sorted))*0.95)-1], sorted[len(sorted)-1]
}

// Falsifier for the normalization: it must cancel host speed and nothing else.
// Each case is a whole run of prompts with the calibration samples around
// them, judged with the same p95/max rule and budget as the live test.
func TestPromptScaleCPUNormalizationKeepsTheBudgetBiting(t *testing.T) {
	const runs = 20
	ref := promptScaleCalibrationReference
	ms := func(v float64) time.Duration { return time.Duration(v * float64(time.Millisecond)) }
	uniform := func(prompt, cal time.Duration) ([]time.Duration, []time.Duration) {
		prompts := make([]time.Duration, runs)
		cals := make([]time.Duration, runs+1)
		for i := range prompts {
			prompts[i] = prompt
		}
		for i := range cals {
			cals[i] = cal
		}
		return prompts, cals
	}
	budget := promptScaleCPUTimeBudget()
	for _, tc := range []struct {
		name  string
		build func() ([]time.Duration, []time.Duration)
		pass  bool
	}{
		{"reference_host_historical_cost", func() ([]time.Duration, []time.Duration) { return uniform(ms(330), ref) }, true},
		{"reference_host_regressed_hook", func() ([]time.Duration, []time.Duration) { return uniform(ms(520), ref) }, false},
		// The verifier failure: 875ms raw p95 on efficiency cores. The same host slowness
		// shows in calibration, so the normalized cost is the historical one.
		{"efficiency_cores_same_hook", func() ([]time.Duration, []time.Duration) { return uniform(ms(875), ms(2.5*45)) }, true},
		// The same slow host with a hook 1.6x more expensive must still fail.
		{"efficiency_cores_regressed_hook", func() ([]time.Duration, []time.Duration) { return uniform(ms(1400), ms(2.5*45)) }, false},
		// A host faster than the reference keeps the absolute budget (factor floor 1):
		// it is neither tightened below 500ms nor loosened above it.
		{"faster_host_keeps_absolute_budget_pass", func() ([]time.Duration, []time.Duration) { return uniform(ms(480), ms(20)) }, true},
		{"faster_host_keeps_absolute_budget_fail", func() ([]time.Duration, []time.Duration) { return uniform(ms(520), ms(20)) }, false},
		// A transient slowdown seen by the bracketing samples is cancelled for the prompts
		// it covered; a slow prompt the calibration did NOT see stays slow.
		{"transient_slowdown_seen_by_calibration", func() ([]time.Duration, []time.Duration) {
			prompts, cals := uniform(ms(330), ref)
			for i := 3; i < 6; i++ {
				prompts[i] = ms(330 * 2.4)
			}
			for i := 3; i <= 6; i++ {
				cals[i] = ms(45 * 2.4)
			}
			return prompts, cals
		}, true},
		{"slow_prompts_calibration_did_not_see", func() ([]time.Duration, []time.Duration) {
			prompts, cals := uniform(ms(330), ref)
			for i := 3; i < 6; i++ {
				prompts[i] = ms(330 * 2.4)
			}
			return prompts, cals
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prompts, cals := tc.build()
			normalized, factors, err := promptScaleNormalizeCPU(prompts, cals, ref)
			if err != nil {
				t.Fatal(err)
			}
			p95, max := promptScaleP95AndMax(normalized)
			passed := p95 <= budget && max <= 2*budget
			if passed != tc.pass {
				t.Fatalf("passed=%t, want %t: p95=%s max=%s budget=%s factors=%.2f", passed, tc.pass, p95, max, budget, factors)
			}
		})
	}
	if _, _, err := promptScaleNormalizeCPU(make([]time.Duration, runs), make([]time.Duration, runs), ref); err == nil {
		t.Fatal("normalization accepted prompts without a calibration sample after the last one")
	}
}
