package envelope

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRouteDeviationAdmissionBinding is the S3 depth test at the envelope seam: a work envelope
// that declares a route deviation graded `judge` (or ungraded, which resolves to judge) MUST carry
// a staked Prediction or WorkEnvelope.Validate refuses it. The refusal binds on deviation-ness +
// risk grade ALONE -- it fires with NO card ref present, so omitting the advisory EV card can never
// become the bypass. `tidy` flows ungated (decisions-down), and a staked prediction admits.
func TestRouteDeviationAdmissionBinding(t *testing.T) {
	base := WorkEnvelope{
		Header:           hdr(SVWork, "work_route_deviation"),
		Owner:            "compound",
		State:            "ready",
		SuccessPredicate: "route deviation admission binds fail-closed",
	}

	// judge-grade deviation + NO prediction + NO card -> refused. Card absence is not a bypass.
	judgeNoPrediction := base
	judgeNoPrediction.RouteDeviation = &RouteDeviation{Lane: "scout/chat", RiskGrade: RouteDeviationJudge}
	if judgeNoPrediction.RouteDeviation.CardRef != "" {
		t.Fatal("test setup error: card ref must be absent to prove card absence is not a bypass")
	}
	if err := judgeNoPrediction.Validate(); err == nil || !strings.Contains(err.Error(), "route_deviation graded") {
		t.Fatalf("judge-grade deviation without a staked prediction must be refused naming the rule, got %v", err)
	}

	// Same refusal must still fire when a card ref IS present but no prediction is staked: the rule
	// keys on deviation-ness + grade, never on the card, so a present card cannot buy admission.
	judgeWithCardNoPrediction := base
	judgeWithCardNoPrediction.RouteDeviation = &RouteDeviation{Lane: "scout/chat", CardRef: "evcard_abc123", RiskGrade: RouteDeviationJudge}
	if err := judgeWithCardNoPrediction.Validate(); err == nil || !strings.Contains(err.Error(), "route_deviation graded") {
		t.Fatalf("judge-grade deviation with a card but no staked prediction must still be refused, got %v", err)
	}

	// judge-grade deviation + staked prediction -> admitted.
	judgeWithPrediction := base
	judgeWithPrediction.RouteDeviation = &RouteDeviation{Lane: "scout/chat", RiskGrade: RouteDeviationJudge}
	judgeWithPrediction.Prediction = &WorkPrediction{Outcome: "completed"}
	if err := judgeWithPrediction.Validate(); err != nil {
		t.Fatalf("judge-grade deviation WITH a staked prediction must be admitted, got %v", err)
	}

	// tidy-grade deviation + NO prediction -> admitted (decisions-down: no gate for reversible work).
	tidyNoPrediction := base
	tidyNoPrediction.RouteDeviation = &RouteDeviation{Lane: "scout/chat", RiskGrade: RouteDeviationTidy}
	if err := tidyNoPrediction.Validate(); err != nil {
		t.Fatalf("tidy-grade deviation without a prediction must be admitted (decisions-down), got %v", err)
	}

	// UNGRADED deviation defaults to judge -> refused without a prediction (fail-closed proof): an
	// omitted grade must never fall through to the ungated tidy path.
	ungradedNoPrediction := base
	ungradedNoPrediction.RouteDeviation = &RouteDeviation{Lane: "scout/chat"}
	if ungradedNoPrediction.RouteDeviation.RiskGrade != "" {
		t.Fatal("test setup error: risk grade must be empty to prove the fail-closed judge default")
	}
	if err := ungradedNoPrediction.Validate(); err == nil || !strings.Contains(err.Error(), "route_deviation graded") {
		t.Fatalf("an ungraded deviation must default to judge and be refused without a prediction, got %v", err)
	}

	// A deviation with a missing lane is malformed regardless of grade.
	missingLane := base
	missingLane.RouteDeviation = &RouteDeviation{RiskGrade: RouteDeviationTidy}
	if err := missingLane.Validate(); err == nil || !strings.Contains(err.Error(), "route_deviation.lane") {
		t.Fatalf("a deviation without a lane must be refused, got %v", err)
	}

	// An out-of-enum grade is reported as invalid (typo visibility) AND still binds at judge, so an
	// unrecognized grade with no prediction is refused rather than silently going ungated.
	badGrade := base
	badGrade.RouteDeviation = &RouteDeviation{Lane: "scout/chat", RiskGrade: "banana"}
	err := badGrade.Validate()
	if err == nil {
		t.Fatalf("an out-of-enum grade must fail validation, got nil")
	}
	if !strings.Contains(err.Error(), "route_deviation.risk_grade") {
		t.Fatalf("an out-of-enum grade must be named as invalid, got %v", err)
	}
	if !strings.Contains(err.Error(), "route_deviation graded") {
		t.Fatalf("an out-of-enum grade must still bind at judge and be refused without a prediction, got %v", err)
	}
}

// TestRouteDeviationRoundTripsAndLegacyMissingFieldLoads proves additivity: a work carrying a
// RouteDeviation round-trips byte-identical, and a legacy envelope JSON without the marker loads,
// re-marshals byte-identically, and still validates -- so introducing the field breaks no fixture.
func TestRouteDeviationRoundTripsAndLegacyMissingFieldLoads(t *testing.T) {
	w := WorkEnvelope{
		Header:           hdr(SVWork, "work_deviation_roundtrip"),
		Owner:            "compound",
		State:            "ready",
		SuccessPredicate: "route deviation round-trips",
		RouteDeviation: &RouteDeviation{
			Lane:            "scout/chat",
			CapabilityClass: "judge-critique",
			CardRef:         "evcard_abc123",
			RiskGrade:       RouteDeviationTidy,
		},
	}
	if err := w.Validate(); err != nil {
		t.Fatalf("tidy deviation work should validate: %v", err)
	}
	if err := RoundTrip(&w); err != nil {
		t.Fatalf("deviation work round-trip: %v", err)
	}

	legacyJSON := `{"schema_version":"compound.work.v0.1","id":"work_legacy_shape","created_at":"2026-06-28T00:00:00Z","producer":{"kind":"cli","name":"compound"},"authority":{"source_class":"runtime","status":"current"},"privacy":"private","risk":"tier1","evidence_refs":[],"uncertainty":{"level":"known"},"owner":"compound","state":"ready","success_predicate":"legacy remains valid","non_goals":[],"run_ids":[],"artifact_ids":[],"verification_ids":[],"memory_candidate_ids":[]}`
	var legacy WorkEnvelope
	if err := json.Unmarshal([]byte(legacyJSON), &legacy); err != nil {
		t.Fatalf("decode legacy work: %v", err)
	}
	if legacy.RouteDeviation != nil {
		t.Fatalf("legacy route_deviation = %+v, want nil", legacy.RouteDeviation)
	}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy work without a route_deviation must remain valid: %v", err)
	}
	// Additivity: re-marshaling a decoded legacy envelope must emit ZERO bytes for the new field,
	// so introducing route_deviation cannot perturb any envelope that predates it. (Full
	// byte-identity of a real envelope is already covered by the RoundTrip above; here we isolate
	// that the additive `omitempty` field is absent from marker-less output.)
	remarshaled, err := json.Marshal(&legacy)
	if err != nil {
		t.Fatalf("re-marshal legacy work: %v", err)
	}
	if strings.Contains(string(remarshaled), "route_deviation") {
		t.Fatalf("marker-less envelope must not emit a route_deviation key, got %s", remarshaled)
	}
	if strings.Contains(legacyJSON, "route_deviation") {
		t.Fatal("legacy fixture unexpectedly contains a route_deviation field; test setup error")
	}
}
