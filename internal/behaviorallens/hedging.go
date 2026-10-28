package behaviorallens

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// HedgingLensID is the id of the hedging/overclaim lens — the second behavioral lens.
const HedgingLensID = "hedging"

// MethodMarkerPolarity names the measurement behind a hedging signal: a deterministic count of
// uncertainty vs certainty markers in an output text, reduced to a signed polarity fraction. No
// provider/network call — the score is a pure function of the text.
const MethodMarkerPolarity = "marker_polarity"

// hedgeMarkers are uncertainty/waffle cues. A text dominated by these hedges (value < 0). The core
// set is the operator-named list (might, maybe, possibly, perhaps, I think, could, seems, arguably,
// not sure); the additions are same-class cues chosen for low false-positive risk (they read as
// hedging in almost any context) — deliberately NOT "may" (month/permission collisions) or bare
// "sure" (only "for sure" is a certainty cue).
var hedgeMarkers = []string{
	"might",
	"maybe",
	"possibly",
	"perhaps",
	"probably",
	"presumably",
	"i think",
	"i believe",
	"i guess",
	"i suppose",
	"could",
	"seems",
	"appears",
	"arguably",
	"somewhat",
	"not sure",
}

// overclaimMarkers are certainty/absolute cues. A text dominated by these overclaims (value > 0).
// The core set is the operator-named list (definitely, certainly, guaranteed, always, never,
// obviously, without doubt, undeniably, clearly); the additions are same-class absolutes.
var overclaimMarkers = []string{
	"definitely",
	"certainly",
	"absolutely",
	"undoubtedly",
	"unquestionably",
	"undeniably",
	"obviously",
	"clearly",
	"surely",
	"guaranteed",
	"guarantee",
	"always",
	"never",
	"without doubt",
	"no doubt",
	"for sure",
}

// Precompiled marker matchers, built once. Each marker becomes a word-boundary-anchored pattern so
// "could" does not match "shrouded" and multi-word phrases ("not sure", "without doubt") match as a
// whitespace-tolerant sequence. Input is lowercased before matching, so patterns are lowercase.
var (
	hedgeMarkerPatterns     = compileMarkers(hedgeMarkers)
	overclaimMarkerPatterns = compileMarkers(overclaimMarkers)
)

func compileMarkers(markers []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(markers))
	for _, m := range markers {
		words := strings.Fields(m)
		for i, w := range words {
			words[i] = regexp.QuoteMeta(w)
		}
		out = append(out, regexp.MustCompile(`\b`+strings.Join(words, `\s+`)+`\b`))
	}
	return out
}

func countMarkers(lowerText string, patterns []*regexp.Regexp) int {
	total := 0
	for _, re := range patterns {
		total += len(re.FindAllString(lowerText, -1))
	}
	return total
}

// HedgingLens is a PUBLIC_SAFE lens that measures the calibration polarity of an OUTPUT text: does it
// waffle (uncertainty markers dominate) or overclaim (certainty markers dominate). It ships in the
// generic product because it infers OUTPUT behavior over supplied text, not the operator's real
// interactions. It is EVIDENCE — no threshold, no gate. The score is deterministic and makes NO
// provider/network call.
type HedgingLens struct{}

func (HedgingLens) ID() string             { return HedgingLensID }
func (HedgingLens) Visibility() Visibility { return VisibilityPublicSafe }

// HedgingPayload is the Observation payload the hedging lens reads: the output text under judgment.
// The lens infers over exactly this text; a blank text is nothing to infer from (ErrNotApplicable).
type HedgingPayload struct {
	Text string `json:"text"`
}

// Infer scores the text's calibration polarity. Value is the signed polarity fraction
// (certainty_markers - uncertainty_markers) / (certainty_markers + uncertainty_markers), which is
// exactly bounded to [-1, 1]:
//   - value < 0: uncertainty markers dominate (hedging/waffle); -1 = every polarity marker is a hedge.
//   - value > 0: certainty markers dominate (overclaim/overconfidence); +1 = every marker is a claim.
//   - value ~ 0: calibrated — either balanced markers or none at all.
//
// Confidence is advisory marker mass (more polarity markers -> more confidence, asymptotic to 1); a
// text with no polarity markers scores value 0 at confidence 0 (calibrated but no evidence either
// way). It is EVIDENCE — no threshold, no gate. A blank text is ErrNotApplicable so composition skips
// it rather than failing.
func (l HedgingLens) Infer(ctx context.Context, obs Observation, now time.Time) (InferredSignal, error) {
	if err := ctx.Err(); err != nil {
		return InferredSignal{}, err
	}
	subject := strings.TrimSpace(obs.Subject)
	if subject == "" {
		return InferredSignal{}, fmt.Errorf("hedging lens: observation subject (output/run id) required")
	}
	var payload HedgingPayload
	if len(obs.Payload) > 0 {
		if err := json.Unmarshal(obs.Payload, &payload); err != nil {
			return InferredSignal{}, fmt.Errorf("hedging lens: decode payload: %w", err)
		}
	}
	text := strings.TrimSpace(payload.Text)
	if text == "" {
		return InferredSignal{}, ErrNotApplicable
	}

	lower := strings.ToLower(text)
	hedges := countMarkers(lower, hedgeMarkerPatterns)
	overclaims := countMarkers(lower, overclaimMarkerPatterns)
	total := hedges + overclaims
	// signed polarity fraction, exactly bounded to [-1, 1]; no polarity markers -> calibrated (0).
	value := 0.0
	if total > 0 {
		value = float64(overclaims-hedges) / float64(total)
	}

	detail := fmt.Sprintf("marker polarity over output text: %d uncertainty vs %d certainty markers; value<0 hedging, value>0 overclaim, ~0 calibrated (evidence, not a gate)", hedges, overclaims)
	return newSignal(HedgingLensID, subject, value, markerConfidence(total), VisibilityPublicSafe, MethodMarkerPolarity, detail, obs.EvidenceRefs, now)
}

// markerConfidence is advisory sample strength: more polarity markers -> more confidence, asymptotic
// to 1. A text with no polarity markers has no calibration evidence -> confidence 0.
func markerConfidence(n int) float64 {
	if n <= 0 {
		return 0
	}
	return float64(n) / float64(n+2)
}
