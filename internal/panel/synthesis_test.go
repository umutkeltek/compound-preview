package panel

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSynthesizeGuardrails(t *testing.T) {
	baseJudge := JudgeOutput{Decision: "promote", Confidence: 0.86, Rationale: "clean promote"}
	tests := []struct {
		name           string
		outputs        []LensOutput
		judge          JudgeOutput
		wantDecision   string
		wantGuardrails []string
		wantDissent    []Dissent
	}{
		{name: "lens_quorum_not_met", outputs: successfulLensOutputs(0.8, 0.9), judge: baseJudge, wantDecision: "park", wantGuardrails: []string{"lens_quorum_not_met"}},
		{
			name: "privacy_risk_high",
			outputs: []LensOutput{
				{Lens: Lens{NodeID: "lens:duplicate-risk"}, Confidence: 0.8},
				{Lens: Lens{NodeID: "lens:evidence-strength"}, Confidence: 0.82},
				{Lens: Lens{NodeID: "lens:privacy-risk"}, Confidence: 0.9, Findings: []Finding{{Claim: "credential material is present", Severity: "high", EvidenceRefs: []string{"trace://privacy"}}}},
			},
			judge: baseJudge, wantDecision: "park", wantGuardrails: []string{"privacy_risk_high"},
		},
		{
			name:         "judge_confidence_below_floor_preserves_dissent",
			outputs:      successfulLensOutputs(0.75, 0.78, 0.8),
			judge:        JudgeOutput{Decision: "promote", Confidence: 0.49, Rationale: "too uncertain", Dissent: []Dissent{{LensRef: "lens:evidence-strength", Position: "promote", Note: "The evidence lens would promote if the judge were confident."}}},
			wantDecision: "park", wantGuardrails: []string{"judge_confidence_below_floor"},
			wantDissent: []Dissent{{LensRef: "lens:evidence-strength", Position: "promote", Note: "The evidence lens would promote if the judge were confident."}},
		},
		{name: "lens_disagreement", outputs: successfulLensOutputs(0.95, 0.2, 0.8), judge: baseJudge, wantDecision: "needs_human", wantGuardrails: []string{"lens_disagreement"}},
		{name: "needs_trial_passthrough", outputs: successfulLensOutputs(0.75, 0.76, 0.77), judge: JudgeOutput{Decision: "needs_trial", Confidence: 0.84, Rationale: "trial before mutation"}, wantDecision: "needs_trial"},
		{name: "clean_promote", outputs: successfulLensOutputs(0.75, 0.8, 0.82), judge: baseJudge, wantDecision: "promote"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			guardrails := Guardrails{}
			if tt.name == "lens_quorum_not_met" {
				guardrails.MinLensFindings = 3
			}
			got := Synthesize(tt.outputs, tt.judge, guardrails)
			if got.SchemaVersion != SynthesisSchemaVersion {
				t.Fatalf("schema version = %q", got.SchemaVersion)
			}
			if got.Decision != tt.wantDecision {
				t.Fatalf("decision = %q, want %q: %+v", got.Decision, tt.wantDecision, got)
			}
			if !reflect.DeepEqual(got.GuardrailsApplied, tt.wantGuardrails) {
				t.Fatalf("guardrails = %+v, want %+v", got.GuardrailsApplied, tt.wantGuardrails)
			}
			if !reflect.DeepEqual(got.Dissent, tt.wantDissent) {
				t.Fatalf("dissent = %+v, want %+v", got.Dissent, tt.wantDissent)
			}
		})
	}
}

func TestDecodeJudgeRejectsMalformedAndUnknownFields(t *testing.T) {
	malformed, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "panel", "judge-responses", "c547245c50a7.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeJudge(malformed); err == nil {
		t.Fatalf("malformed judge JSON decoded successfully")
	}

	unknown := []byte(`{"decision":"promote","confidence":0.8,"rationale":"ok","dissent":[],"extra":true}`)
	if _, err := DecodeJudge(unknown); err == nil {
		t.Fatalf("judge JSON with unknown field decoded successfully")
	}
}

func successfulLensOutputs(confidences ...float64) []LensOutput {
	ids := []string{"lens:duplicate-risk", "lens:evidence-strength", "lens:inversion-falsification"}
	out := make([]LensOutput, 0, len(confidences))
	for i, confidence := range confidences {
		id := ids[i%len(ids)]
		out = append(out, LensOutput{Lens: Lens{NodeID: id}, Confidence: confidence, Findings: []Finding{{Claim: "synthetic finding", Severity: "low", EvidenceRefs: []string{"trace://synthetic"}}}})
	}
	return out
}
