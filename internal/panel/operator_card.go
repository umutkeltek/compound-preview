package panel

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"compound/internal/reviewartifact"
)

func (p verdictArtifactPayload) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForVerdict(p)
}

func OperatorCardForVerdict(p verdictArtifactPayload) (reviewartifact.OperatorCard, error) {
	decision, err := reviewartifact.RequireText("synthesis.decision", p.Synthesis.Decision)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	context, err := reviewartifact.RequireText("target context", panelVerdictContextLabel(p.Target))
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	group := "panel-verdicts|" + reviewartifact.FirstNonEmpty(p.RunID, "run") + "|" + decision
	lensCount := len(p.LensFindings)
	if lensCount == 0 {
		lensCount = 1
	}

	var summary, unlock string
	switch strings.TrimSpace(p.Target.Kind) {
	case "memory_candidate":
		switch decision {
		case "promote":
			summary = fmt.Sprintf("The judge panel passed this claim — all %d lenses clean. Add it to memory?", lensCount)
			if len(p.Synthesis.Dissent) > 0 {
				summary = fmt.Sprintf("The judge panel passed this claim with dissent noted — %d lenses ran. Add it to memory?", lensCount)
			} else if p.Synthesis.Confidence > 0 && p.Synthesis.Confidence < 0.7 {
				summary = fmt.Sprintf("The judge panel passed this claim at low confidence %.2f. Add it to memory?", p.Synthesis.Confidence)
			}
			unlock = "memory gains 1 promoted belief it can act on"
		case "reject":
			summary = "The judge panel rejected this claim — " + topFailedLensPlain(p.LensFindings) + " flagged it. Record the rejection?"
			unlock = "the inbox stops re-asking about this candidate"
		default:
			summary = "The judge panel needs your call on this claim. Record the decision?"
			unlock = "memory gets a reviewed answer for this candidate"
		}
	case "comparison":
		candidate, baseline, surface, private := comparisonPlainParts(p.Target)
		result := "lost to"
		record := "loss"
		if decision == "promote" || decision == "accept" || decision == "win" {
			result = "beat"
			record = "win"
		}
		if private {
			summary = fmt.Sprintf("The judge panel says %s %s %s at %s on a private transcript. Record the %s?", candidate, result, baseline, surface, record)
		} else {
			summary = fmt.Sprintf("The judge panel says %s %s %s on %s. Record the %s?", candidate, result, baseline, surface, record)
		}
		unlock = "the comparison result becomes reviewed route evidence"
	default:
		thing := reviewartifact.ShortLabel(reviewartifact.FirstNonEmpty(p.Target.Kind, "target"), 36)
		summary = fmt.Sprintf("The judge panel reached a %s verdict for this %s. Record it?", reviewartifact.PlainLabel(decision), thing)
		unlock = "the verdict stops waiting as raw panel output"
	}
	return reviewartifact.NewOperatorCard(summary, context, group, unlock)
}

func OperatorCardForVerdictJSON(data []byte) (reviewartifact.OperatorCard, error) {
	var payload verdictArtifactPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	return OperatorCardForVerdict(payload)
}

func (p PanelCapabilityFeedbackPlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForCapabilityFeedback(p)
}

func OperatorCardForCapabilityFeedback(p PanelCapabilityFeedbackPlan) (reviewartifact.OperatorCard, error) {
	context, err := reviewartifact.RequireText("verdict context", panelFeedbackContextLabel(p))
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	count := len(p.FeedbackCandidates)
	if count == 0 {
		return reviewartifact.OperatorCard{}, fmt.Errorf("feedback_candidates required for operator card")
	}
	summary := fmt.Sprintf("The judge panel found %d capability feedback items. Review the graph feedback?", count)
	if p.Status == "applied_feedback" || p.Status == "rejected" || p.Status == "recorded" {
		summary = fmt.Sprintf("The judge panel feedback review is already recorded for %s.", context)
	}
	group := "capability-panel-feedback|" + reviewartifact.FirstNonEmpty(p.RunID, "run") + "|" + reviewartifact.FirstNonEmpty(p.PanelDecision, "decision")
	return reviewartifact.NewOperatorCard(summary, context, group, "capability graph gains reviewed feedback edges")
}

func panelVerdictContextLabel(target TargetInput) string {
	var payload map[string]any
	if len(target.Payload) > 0 && json.Unmarshal(target.Payload, &payload) == nil {
		if sp, ok := payload["source_provenance"].(map[string]any); ok {
			if title, ok := sp["title"].(string); ok && strings.TrimSpace(title) != "" {
				return cleanPanelContextLabel(title)
			}
		}
		if object, ok := payload["object"].(string); ok && strings.TrimSpace(object) != "" {
			return cleanPanelContextLabel(object)
		}
	}
	return cleanPanelContextLabel(reviewartifact.HumanizeEntity(target.Ref), target.Kind)
}

// comparisonRefPattern parses the "{candidate}_vs_{baseline}_{trailing id}"
// shape a comparison target ref encodes, e.g.
// "cmp_r3_PRIV_ornith_vs_hauhaucs_codex-rollout-2026-06-16T23-49-ba67d2e9" or
// "cmp_r3_deepseek_vs_autoextract_fixture-07". The trailing segment (session,
// transcript, or fixture id) is deliberately not captured; it must never
// reach the sentence.
var comparisonRefPattern = regexp.MustCompile(`^cmp_r\d+_(?:PRIV_)?(.+?)_vs_(.+)_[^_]+$`)

// comparisonPlainParts resolves the candidate/baseline/surface names a
// comparison sentence needs, and whether the comparison ran over a private
// transcript (never named in the sentence). It prefers explicit flat payload
// fields when a schema supplies them, then falls back to parsing the
// target's own ref. The ref is the only place today's comparison_candidate
// payload actually encodes human-readable candidate/baseline names (its
// candidate/baseline objects carry lane_id, not a short plain name).
func comparisonPlainParts(target TargetInput) (candidate, baseline, surface string, private bool) {
	var payload map[string]any
	if len(target.Payload) > 0 {
		_ = json.Unmarshal(target.Payload, &payload)
	}
	private = strings.Contains(target.Ref, "_PRIV_")
	candidate = mapString(payload, "candidate_plain", "candidate", "candidate_name")
	baseline = mapString(payload, "baseline_plain", "baseline", "baseline_name")
	if candidate == "" || baseline == "" {
		if m := comparisonRefPattern.FindStringSubmatch(target.Ref); m != nil {
			first := strings.ReplaceAll(m[1], "_", " ")
			second := strings.ReplaceAll(m[2], "_", " ")
			// Refs are conventionally {candidate}_vs_{baseline}, but the name is
			// human-chosen and live runs violate it (cmp_r3_deepseek_vs_autoextract
			// is baseline-first — it inverted 28 operator cards on 2026-07-09).
			// When the payload carries lane objects they decide orientation; the
			// ref still supplies the plain display names so lane ids never leak
			// into the sentence. Swap only when both tokens match unambiguously.
			candLane := compactToken(laneString(payload, "candidate"))
			baseLane := compactToken(laneString(payload, "baseline"))
			if candLane != "" && baseLane != "" {
				n1 := compactToken(first)
				n2 := compactToken(second)
				firstIsBaseline := strings.Contains(baseLane, n1) && !strings.Contains(candLane, n1)
				secondIsCandidate := strings.Contains(candLane, n2) && !strings.Contains(baseLane, n2)
				if firstIsBaseline && secondIsCandidate {
					first, second = second, first
				}
			}
			if candidate == "" {
				candidate = first
			}
			if baseline == "" {
				baseline = second
			}
		}
	}
	surface = mapString(payload, "surface_plain", "surface", "subject", "capability_class")
	if surface == "" {
		surface = reviewartifact.PlainLabel(reviewartifact.FirstClause(target.Summary))
	}
	candidate = reviewartifact.ShortLabel(reviewartifact.PlainLabel(reviewartifact.FirstNonEmpty(candidate, "candidate")), 34)
	baseline = reviewartifact.ShortLabel(reviewartifact.PlainLabel(reviewartifact.FirstNonEmpty(baseline, "baseline")), 34)
	surface = reviewartifact.ShortLabel(reviewartifact.FirstNonEmpty(surface, "test surface"), 34)
	return candidate, baseline, surface, private
}

// laneString pulls a lane identifier out of a comparison payload's candidate/
// baseline OBJECT (mapString above only reads string values, which is why ref
// order was ever consulted at all). Display text never uses this directly —
// orientation checks only.
func laneString(payload map[string]any, key string) string {
	obj, ok := payload[key].(map[string]any)
	if !ok {
		return ""
	}
	return mapString(obj, "lane_id", "name", "id")
}

// compactToken lowercases and strips non-alphanumerics so ref tokens
// ("mac qwen") can be matched against lane ids ("mac/qwen3.6-27b").
func compactToken(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func mapString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func topFailedLensPlain(findings []LensFinding) string {
	for _, finding := range findings {
		status := strings.TrimSpace(finding.Status)
		if status != "" && status != "clean" && status != "pass" && status != "passed" {
			return reviewartifact.ShortLabel(reviewartifact.PlainLabel(strings.TrimPrefix(finding.LensRef, "lens:")), 40)
		}
		if finding.Error != "" || len(finding.Findings) > 0 {
			return reviewartifact.ShortLabel(reviewartifact.PlainLabel(strings.TrimPrefix(finding.LensRef, "lens:")), 40)
		}
	}
	return "the review lens"
}

func panelFeedbackContextLabel(p PanelCapabilityFeedbackPlan) string {
	return cleanPanelContextLabel(p.TargetKind, reviewartifact.HumanizeEntity(p.TargetRef), p.VerdictID)
}

func cleanPanelContextLabel(values ...string) string {
	label := reviewartifact.PlainLabel(reviewartifact.FirstNonEmpty(values...))
	if label == "" {
		return ""
	}
	return reviewartifact.ShortLabel(label, 60)
}
