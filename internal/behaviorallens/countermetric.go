package behaviorallens

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// CounterMetricLensID is the id of the counter-metric behavioral lens — the third public_safe lens.
const CounterMetricLensID = "counter-metric"

// MethodCounterMetricPairing names the measurement behind a counter-metric signal: a deterministic
// count of improvement-claim markers vs counter-metric (held-flat) markers in a receipt's close
// summary, reduced to a gated polarity. No provider/network call — the score is a pure function of
// the text.
const MethodCounterMetricPairing = "counter_metric_pairing"

// CounterMetricPayload is the Observation payload the counter-metric lens reads: a receipt's
// close-shape fields, at minimum the StateDelta.Summary text the lens scores. Outcome/MoveType/
// ProofRefCount/HasSurprise ride along on the payload (a collector may correlate them later) but the
// lens itself scores Summary only.
type CounterMetricPayload struct {
	Summary       string `json:"summary"`
	Outcome       string `json:"outcome,omitempty"`
	MoveType      string `json:"move_type,omitempty"`
	ProofRefCount int    `json:"proof_ref_count,omitempty"`
	HasSurprise   bool   `json:"has_surprise,omitempty"`
}

// improvementWordMarkers are the word-shaped improvement-claim cues (the operator-named list: a
// close receipt that says its outcome got faster/smaller/better). Deliberately narrow — conservative
// prefers a false negative (missing a real improvement claim) over a false positive (flagging a
// summary that made no throughput claim at all).
var improvementWordMarkers = []string{
	"faster",
	"reduced",
	"improved",
	"speedup",
	"dropped",
}

// counterMetricWordMarkers are the word-shaped counter-metric (held-flat) cues: the pairing text an
// honest improvement claim names alongside itself (the metric that did NOT move, and a cohort/control
// framing).
var counterMetricWordMarkers = []string{
	"flat",
	"unchanged",
	"no regression",
	"counter-metric",
	"within band",
	"did not move",
}

var (
	improvementWordPatterns   = compileMarkers(improvementWordMarkers)
	counterMetricWordPatterns = compileMarkers(counterMetricWordMarkers)

	// These structured improvement-claim cues keep ambiguous words comparative: a signed or
	// comparative percent delta ("-38%", "38% smaller"), an
	// Nx multiplier ("2.9x per-engineer", "5.8x LOC"), or a numeric before/after transition joined
	// by "→" ("attention 13-26s → 3-5s"). "Cut" counts only with an explicit delta such as
	// "in half" or "by 20%"; ordinary uses such as "cut the ribbon" are not improvement claims.
	percentDeltaPattern    = regexp.MustCompile(`(?:[+-]\s*\d+(?:\.\d+)?\s*%|\b\d+(?:\.\d+)?\s*%\s+(?:faster|smaller|lower|less|higher|more)\b)`)
	multiplierPattern      = regexp.MustCompile(`\b\d+(?:\.\d+)?x\b`)
	arrowTransitionPattern = regexp.MustCompile(`\d[^\s→]*\s*→\s*\d`)
	comparativeCutPattern  = regexp.MustCompile(`\bcut\b[^,.;:!?\n]{0,80}(?:\bin\s+half\b|\bby\s+[+-]?\d+(?:\.\d+)?\s*%)(?:\s*[,.;:!?]|\s*$)`)

	// "Held" and "stayed" are ordinary verbs unless a metric noun directly owns them and the
	// phrase closes or names a stable state. Keeping them out of the global word list prevents
	// prose such as "held a review and stayed to answer" from manufacturing a pairing.
	contextualHoldPattern = regexp.MustCompile(`\b(?:latency|rate|reversions?|regressions?|errors?|failures?|costs?|quality|coverage|throughput|duration|time|count|volume|memory|load)\s+(?:held|stayed)(?:\s+(?:flat|unchanged|steady|constant|within\s+band|the\s+same))?(?:[.;,:!?]|$)`)
)

// countStructuredImprovementMarkers counts comparative improvement cues in already-lowercased text.
func countStructuredImprovementMarkers(lowerText string) int {
	return len(percentDeltaPattern.FindAllString(lowerText, -1)) +
		len(multiplierPattern.FindAllString(lowerText, -1)) +
		len(arrowTransitionPattern.FindAllString(lowerText, -1)) +
		len(comparativeCutPattern.FindAllString(lowerText, -1))
}

// clampSigned bounds v to [-1, 1] — the counter-metric lens's valid signed-value range.
func clampSigned(v float64) float64 {
	if v < -1 {
		return -1
	}
	if v > 1 {
		return 1
	}
	return v
}

// CounterMetricLens is a PUBLIC_SAFE lens that scores whether a close receipt's improvement claim
// (a throughput/latency/LOC delta) is paired with the counter-metric that stayed flat (a cohort
// control, a reversion rate, a review-latency figure that did not move). An honest throughput claim
// names both; a claim that reports only the improvement is the observed failure-pattern shape this
// lens exists to catch. It ships in the generic product because it infers OUTPUT (receipt summary)
// behavior, not the operator's real interactions. It is EVIDENCE — no threshold, no gate; turning an
// unpaired-claim signal into a minted failure_pattern candidate is a DEFERRED collector-level act
// (executorlens), never this lens.
type CounterMetricLens struct{}

func (CounterMetricLens) ID() string             { return CounterMetricLensID }
func (CounterMetricLens) Visibility() Visibility { return VisibilityPublicSafe }

// Infer scores the receipt summary's claim-pairing polarity:
//   - no improvement-claim marker at all -> ErrNotApplicable (nothing to pair-check; composition
//     skips it rather than failing — most receipts report ordinary work, not a throughput claim).
//   - an improvement claim WITH at least one counter-metric marker -> POSITIVE pole (paired,
//     honest claim structure).
//   - an improvement claim with NO counter-metric marker -> NEGATIVE pole (the unpaired-claim
//     failure shape).
//
// Confidence scales with total marker strength (more markers -> more confidence, asymptotic to 1),
// mirroring the hedging lens's markerConfidence. Value is clamped to [-1, 1].
func (l CounterMetricLens) Infer(ctx context.Context, obs Observation, now time.Time) (InferredSignal, error) {
	if err := ctx.Err(); err != nil {
		return InferredSignal{}, err
	}
	subject := strings.TrimSpace(obs.Subject)
	if subject == "" {
		return InferredSignal{}, fmt.Errorf("counter-metric lens: observation subject (receipt/lane id) required")
	}
	var payload CounterMetricPayload
	if len(obs.Payload) > 0 {
		if err := json.Unmarshal(obs.Payload, &payload); err != nil {
			return InferredSignal{}, fmt.Errorf("counter-metric lens: decode payload: %w", err)
		}
	}
	summary := strings.TrimSpace(payload.Summary)
	if summary == "" {
		return InferredSignal{}, ErrNotApplicable
	}

	lower := strings.ToLower(summary)
	improvementMarkers := countMarkers(lower, improvementWordPatterns) + countStructuredImprovementMarkers(lower)
	if improvementMarkers == 0 {
		// no improvement claim at all: nothing for this lens to pair-check.
		return InferredSignal{}, ErrNotApplicable
	}
	counterMarkers := countMarkers(lower, counterMetricWordPatterns) + len(contextualHoldPattern.FindAllString(lower, -1))

	paired := counterMarkers > 0
	polarity := -1.0
	if paired {
		polarity = 1.0
	}
	total := improvementMarkers + counterMarkers
	value := clampSigned(polarity * (float64(total) / float64(total+2)))

	var detail string
	if paired {
		detail = fmt.Sprintf(
			"improvement claim (%d marker(s)) paired with a counter-metric (%d marker(s)): positive pole (evidence, not a gate)",
			improvementMarkers, counterMarkers)
	} else {
		detail = fmt.Sprintf(
			"improvement claim (%d marker(s)) with NO counter-metric named: negative unpaired-claim pole (evidence, not a gate)",
			improvementMarkers)
	}
	return newSignal(CounterMetricLensID, subject, value, markerConfidence(total), VisibilityPublicSafe, MethodCounterMetricPairing, detail, obs.EvidenceRefs, now)
}
