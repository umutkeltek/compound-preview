package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type LensOutput struct {
	Lens       Lens
	Confidence float64
	Findings   []Finding
	Err        error
}

type JudgeOutput struct {
	Decision   string    `json:"decision"`
	Confidence float64   `json:"confidence"`
	Rationale  string    `json:"rationale"`
	Dissent    []Dissent `json:"dissent"`
}

type Guardrails struct {
	MinConfidence      float64
	MinLensFindings    int
	DisagreementSpread float64
}

func DecodeJudge(data []byte) (JudgeOutput, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var out JudgeOutput
	if err := dec.Decode(&out); err != nil {
		return JudgeOutput{}, fmt.Errorf("decode judge output: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return JudgeOutput{}, fmt.Errorf("decode judge output: trailing JSON")
		}
		return JudgeOutput{}, fmt.Errorf("decode judge output trailing JSON: %w", err)
	}
	if strings.TrimSpace(out.Decision) == "" {
		return JudgeOutput{}, fmt.Errorf("judge decision required")
	}
	if out.Confidence < 0 || out.Confidence > 1 {
		return JudgeOutput{}, fmt.Errorf("judge confidence must be 0..1")
	}
	return out, nil
}

func Synthesize(outputs []LensOutput, judge JudgeOutput, g Guardrails) Synthesis {
	successful := successfulLensOutputsForSynthesis(outputs)
	g = g.withDefaults(len(successful))
	var guardrails []string
	forcedPark := false
	if len(successful) < g.MinLensFindings {
		guardrails = append(guardrails, "lens_quorum_not_met")
		forcedPark = true
	}
	if hasHighPrivacyRisk(successful) {
		guardrails = append(guardrails, "privacy_risk_high")
		forcedPark = true
	}
	if judge.Confidence < g.MinConfidence {
		guardrails = append(guardrails, "judge_confidence_below_floor")
		forcedPark = true
	}

	decision := strings.TrimSpace(judge.Decision)
	if decision == "" {
		decision = "park"
	}
	if forcedPark {
		decision = "park"
	} else if decision == "promote" && confidenceSpread(successful) > g.DisagreementSpread {
		decision = "needs_human"
		guardrails = append(guardrails, "lens_disagreement")
	}

	return Synthesis{
		SchemaVersion:     SynthesisSchemaVersion,
		Decision:          decision,
		Confidence:        judge.Confidence,
		Rationale:         judge.Rationale,
		Dissent:           dissentContradicting(decision, judge.Dissent),
		GuardrailsApplied: guardrails,
	}
}

func (g Guardrails) withDefaults(usableLensCount int) Guardrails {
	if g.MinConfidence == 0 {
		g.MinConfidence = 0.7
	}
	if g.MinLensFindings == 0 {
		g.MinLensFindings = minInt(3, usableLensCount)
	}
	if g.DisagreementSpread == 0 {
		g.DisagreementSpread = 0.5
	}
	return g
}

func successfulLensOutputsForSynthesis(outputs []LensOutput) []LensOutput {
	out := []LensOutput{}
	for _, output := range outputs {
		if output.Err == nil {
			out = append(out, output)
		}
	}
	return out
}

func hasHighPrivacyRisk(outputs []LensOutput) bool {
	for _, output := range outputs {
		id := strings.ToLower(output.Lens.NodeID)
		name := strings.ToLower(output.Lens.Name)
		if !strings.Contains(id, "privacy-risk") && !strings.Contains(name, "privacy") {
			continue
		}
		for _, finding := range output.Findings {
			if strings.EqualFold(finding.Severity, "high") {
				return true
			}
		}
	}
	return false
}

func confidenceSpread(outputs []LensOutput) float64 {
	if len(outputs) < 2 {
		return 0
	}
	min, max := outputs[0].Confidence, outputs[0].Confidence
	for _, output := range outputs[1:] {
		if output.Confidence < min {
			min = output.Confidence
		}
		if output.Confidence > max {
			max = output.Confidence
		}
	}
	return max - min
}

func dissentContradicting(finalDecision string, dissent []Dissent) []Dissent {
	finalDecision = normalizePanelPosition(finalDecision)
	var out []Dissent
	for _, d := range dissent {
		if normalizePanelPosition(d.Position) != finalDecision {
			out = append(out, d)
		}
	}
	return out
}

func normalizePanelPosition(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
