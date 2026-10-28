package behaviorallens

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"
)

func mustObs(t *testing.T, subject string, payload AgreementAsymmetryPayload) Observation {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return Observation{Subject: subject, Payload: raw}
}

// trackerPayload: a lane that always sides with the operator ("op") while its two peers converge on a
// different verdict ("truth"). operator-agreement rate 1.0, peer-agreement rate 0.0 -> asymmetry 1.0.
func trackerPayload() AgreementAsymmetryPayload {
	var p AgreementAsymmetryPayload
	for _, a := range []string{"c1", "c2", "c3", "c4"} {
		p.Records = append(p.Records, AgreementRecord{
			ArtifactID:         a,
			AgreesWithOperator: true,
			Verdict:            "op",
			PeerVerdicts:       []string{"truth", "truth"},
		})
	}
	return p
}

// TestAgreementAsymmetry_TrackerScoresHigh_BalancedScoresZero is falsifier (i): a lane with high
// operator-agreement and low peer-agreement scores a large positive value; a lane that agrees with
// the operator no more than it agrees with peers scores ~0.
func TestAgreementAsymmetry_TrackerScoresHigh_BalancedScoresZero(t *testing.T) {
	lens := AgreementAsymmetryLens{}
	now := time.Unix(1_800_000_000, 0).UTC()

	trackerSig, err := lens.Infer(context.Background(), mustObs(t, "agent-lane:tracker", trackerPayload()), now)
	if err != nil {
		t.Fatalf("tracker infer: %v", err)
	}
	if math.Abs(trackerSig.Value-1.0) > 1e-9 {
		t.Fatalf("tracker asymmetry = %.6f, want 1.0 (op-agreement 1.0 minus peer-agreement 0.0)", trackerSig.Value)
	}
	if trackerSig.Visibility != VisibilityOperatorPrivate {
		t.Fatalf("tracker visibility = %q, want operator_private", trackerSig.Visibility)
	}
	if trackerSig.Method != MethodAgreementAsymmetry {
		t.Fatalf("tracker method = %q, want %q", trackerSig.Method, MethodAgreementAsymmetry)
	}
	if trackerSig.Confidence <= 0 {
		t.Fatalf("tracker confidence = %.3f, want > 0 (has peer comparisons)", trackerSig.Confidence)
	}

	// Balanced: agrees with the operator on exactly half its artifacts, and matches a peer on exactly
	// half its peer comparisons (verdict "x" matches peer "x", not peer "y"). op-rate == peer-rate == 0.5.
	var balanced AgreementAsymmetryPayload
	for i, a := range []string{"b1", "b2", "b3", "b4"} {
		balanced.Records = append(balanced.Records, AgreementRecord{
			ArtifactID:         a,
			AgreesWithOperator: i < 2, // true on 2 of 4
			Verdict:            "x",
			PeerVerdicts:       []string{"x", "y"},
		})
	}
	balancedSig, err := lens.Infer(context.Background(), mustObs(t, "agent-lane:balanced", balanced), now)
	if err != nil {
		t.Fatalf("balanced infer: %v", err)
	}
	if math.Abs(balancedSig.Value-0.0) > 1e-9 {
		t.Fatalf("balanced asymmetry = %.6f, want 0.0 (agrees with operator no more than with peers)", balancedSig.Value)
	}
	if balancedSig.Value >= trackerSig.Value {
		t.Fatalf("balanced asymmetry %.6f must sit below tracker %.6f", balancedSig.Value, trackerSig.Value)
	}
}

// TestAgreementAsymmetry_NoPeerData_ZeroConfidence is a depth falsifier: with operator agreement but
// no peer comparisons at all, the value equals the operator-agreement rate (peer rate defaults to 0),
// but confidence MUST be 0 — the signal cannot be trusted as "tracks the operator" without peer data.
// A naive confidence that ignored the peer sample would fail here.
func TestAgreementAsymmetry_NoPeerData_ZeroConfidence(t *testing.T) {
	var p AgreementAsymmetryPayload
	for _, a := range []string{"n1", "n2", "n3"} {
		p.Records = append(p.Records, AgreementRecord{ArtifactID: a, AgreesWithOperator: true, Verdict: "op"})
	}
	sig, err := AgreementAsymmetryLens{}.Infer(context.Background(), mustObs(t, "agent-lane:lonely", p), time.Now())
	if err != nil {
		t.Fatalf("infer: %v", err)
	}
	if math.Abs(sig.Value-1.0) > 1e-9 {
		t.Fatalf("value = %.6f, want 1.0 (op-rate with peer-rate defaulting to 0)", sig.Value)
	}
	if sig.Confidence != 0 {
		t.Fatalf("confidence = %.6f, want 0 (no peer comparisons -> asymmetry is unverifiable)", sig.Confidence)
	}
}

// TestAgreementAsymmetry_EmptyRecords_NotApplicable proves an empty payload is a skip, not a signal.
func TestAgreementAsymmetry_EmptyRecords_NotApplicable(t *testing.T) {
	_, err := AgreementAsymmetryLens{}.Infer(context.Background(), Observation{Subject: "agent-lane:x"}, time.Now())
	if err != ErrNotApplicable {
		t.Fatalf("empty records err = %v, want ErrNotApplicable", err)
	}
}

// TestAgreementAsymmetry_VisibilityBoundary is falsifier (ii): the visibility boundary end to end.
//   - stranger clone (empty Config): RegistryForHome(cfg).Compose SKIPS the private lens (SkipNote,
//     no signal) even though the observation IS applicable — the only reason for absence is the gate.
//   - opted-in home (Config enabling the lens): it RUNS, produces a signal whose Visibility is
//     operator_private, and PublicProjection of that signal (and of the whole compose result) is
//     empty — the value never leaves COMPOUND_HOME.
func TestAgreementAsymmetry_VisibilityBoundary(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	obs := mustObs(t, "agent-lane:tracker", trackerPayload()) // applicable: non-empty records

	// --- stranger clone: no config -> private lens registered but SKIPPED by the gate ---
	strangerCfg := Config{}
	strangerRes, err := RegistryForHome(strangerCfg).Compose(context.Background(), obs, strangerCfg, now)
	if err != nil {
		t.Fatalf("stranger compose: %v", err)
	}
	if findSignal(strangerRes.Signals, AgreementAsymmetryLensID) != nil {
		t.Fatalf("stranger clone leaked a private signal: %+v", strangerRes.Signals)
	}
	skip := findSkip(strangerRes.Skipped, AgreementAsymmetryLensID)
	if skip == nil {
		t.Fatalf("stranger clone should SKIP the private lens with a legible note; skipped=%+v", strangerRes.Skipped)
	}
	if skip.Visibility != VisibilityOperatorPrivate {
		t.Fatalf("skip visibility = %q, want operator_private", skip.Visibility)
	}

	// --- opted-in home: config enables the lens -> it runs and produces an operator_private signal ---
	enabledCfg := Config{EnabledPrivateLenses: []string{AgreementAsymmetryLensID}}
	enabledRes, err := RegistryForHome(enabledCfg).Compose(context.Background(), obs, enabledCfg, now)
	if err != nil {
		t.Fatalf("enabled compose: %v", err)
	}
	sig := findSignal(enabledRes.Signals, AgreementAsymmetryLensID)
	if sig == nil {
		t.Fatalf("opted-in home should RUN the private lens; signals=%+v skipped=%+v", enabledRes.Signals, enabledRes.Skipped)
	}
	if sig.Visibility != VisibilityOperatorPrivate {
		t.Fatalf("signal visibility = %q, want operator_private", sig.Visibility)
	}
	if math.Abs(sig.Value-1.0) > 1e-9 {
		t.Fatalf("signal value = %.6f, want 1.0 (ties the boundary run to the math)", sig.Value)
	}

	// THE OUT-GATE: the private signal's value never leaves the home. PublicProjection drops it both
	// on its own and within the whole compose result.
	if got := PublicProjection([]InferredSignal{*sig}); len(got) != 0 {
		t.Fatalf("PublicProjection([private signal]) = %+v, want empty (value must never leave the home)", got)
	}
	for _, s := range PublicProjection(enabledRes.Signals) {
		if s.LensID == AgreementAsymmetryLensID {
			t.Fatalf("PublicProjection of the compose result leaked the private signal: %+v", s)
		}
	}
}

func findSignal(signals []InferredSignal, lensID string) *InferredSignal {
	for i := range signals {
		if signals[i].LensID == lensID {
			return &signals[i]
		}
	}
	return nil
}

func findSkip(skips []SkipNote, lensID string) *SkipNote {
	for i := range skips {
		if skips[i].LensID == lensID {
			return &skips[i]
		}
	}
	return nil
}
