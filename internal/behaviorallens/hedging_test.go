package behaviorallens

import (
	"context"
	"encoding/json"
	"testing"
)

func hedgingObs(t *testing.T, subject, text string) Observation {
	t.Helper()
	payload, err := json.Marshal(HedgingPayload{Text: text})
	if err != nil {
		t.Fatalf("marshal hedging payload: %v", err)
	}
	return Observation{SchemaVersion: ObservationSchemaVersion, Subject: subject, Payload: payload}
}

func inferHedging(t *testing.T, subject, text string) InferredSignal {
	t.Helper()
	sig, err := HedgingLens{}.Infer(context.Background(), hedgingObs(t, subject, text), fixedNow)
	if err != nil {
		t.Fatalf("infer %s: %v", subject, err)
	}
	return sig
}

// Synthetic outputs only (no personal/product names): the repo is destined public.
const (
	// uncertainty markers dominate: i think, might, not sure, could, possibly, seems, perhaps,
	// arguably, probably.
	hedgyOutput = "I think this might work, but I'm not sure. It could possibly help, and it " +
		"seems like perhaps things improve. Arguably this is probably fine."
	// certainty markers dominate: definitely, guaranteed, always, never, obviously, clearly,
	// undeniably, certainly, absolutely, without doubt.
	overclaimOutput = "This will definitely work. It is guaranteed to always succeed and never " +
		"fail. Obviously it is clearly the best; undeniably and certainly the winner, " +
		"absolutely, without doubt."
	// no polarity markers at all: a flat description.
	neutralOutput = "The function reads the file, parses each line, and returns a list of " +
		"records. It runs in linear time and writes output under the home directory."
	// one hedge (might) balanced by one overclaim (definitely) -> net-zero polarity.
	balancedOutput = "This might work, but it will definitely need more testing."
)

// FALSIFIER (pole separation) — the load-bearing assertion. A hedgy output scores the NEGATIVE pole,
// an overclaiming output the POSITIVE pole, and a marker-free output sits at ~0. This is the assertion
// that goes RED against a stub scorer (value always 0) and GREEN only once real marker polarity is
// computed: a no-op scorer cannot separate the poles.
func TestHedgingLensPoleSeparation(t *testing.T) {
	hedgy := inferHedging(t, "output:hedgy", hedgyOutput)
	overclaim := inferHedging(t, "output:overclaim", overclaimOutput)
	neutral := inferHedging(t, "output:neutral", neutralOutput)

	if hedgy.Value >= 0 {
		t.Fatalf("hedgy output must score the negative (hedging) pole, got %.3f", hedgy.Value)
	}
	if overclaim.Value <= 0 {
		t.Fatalf("overclaiming output must score the positive (overclaim) pole, got %.3f", overclaim.Value)
	}
	if abs(neutral.Value) > 0.01 {
		t.Fatalf("marker-free output must score ~0 (calibrated), got %.3f", neutral.Value)
	}
	// ordered separation: hedging < calibrated < overclaim, with real margin.
	if !(hedgy.Value < neutral.Value && neutral.Value < overclaim.Value) {
		t.Fatalf("poles not ordered: hedgy=%.3f neutral=%.3f overclaim=%.3f", hedgy.Value, neutral.Value, overclaim.Value)
	}
	if hedgy.Value > -0.5 || overclaim.Value < 0.5 {
		t.Fatalf("poles must be well-separated, got hedgy=%.3f overclaim=%.3f", hedgy.Value, overclaim.Value)
	}

	// it is the second PUBLIC_SAFE lens and it names its technique.
	if hedgy.Visibility != VisibilityPublicSafe || hedgy.Method != MethodMarkerPolarity {
		t.Fatalf("hedging is a public_safe marker_polarity lens, got vis=%s method=%s", hedgy.Visibility, hedgy.Method)
	}
	if hedgy.LensID != HedgingLensID {
		t.Fatalf("lens id must be %q, got %q", HedgingLensID, hedgy.LensID)
	}
	// a marker-free output has no calibration evidence -> confidence 0; a marker-rich one is confident.
	if neutral.Confidence != 0 {
		t.Fatalf("marker-free output must carry confidence 0 (no evidence), got %.3f", neutral.Confidence)
	}
	if overclaim.Confidence <= 0 {
		t.Fatalf("marker-rich output must carry positive confidence, got %.3f", overclaim.Confidence)
	}
}

// A balanced output (one hedge, one overclaim) nets to ~0: domination, not mere presence, is what
// moves the score off calibrated.
func TestHedgingLensBalancedIsNearZero(t *testing.T) {
	sig := inferHedging(t, "output:balanced", balancedOutput)
	if abs(sig.Value) > 0.01 {
		t.Fatalf("balanced markers must net to ~0, got %.3f", sig.Value)
	}
	// but there IS evidence, unlike the marker-free case: confidence is positive.
	if sig.Confidence <= 0 {
		t.Fatalf("balanced output has markers, so confidence must be positive, got %.3f", sig.Confidence)
	}
}

// A blank / absent output text is nothing to infer from -> ErrNotApplicable (composition skips it
// rather than failing).
func TestHedgingLensEmptyTextNotApplicable(t *testing.T) {
	var lens Lens = HedgingLens{}
	if _, err := lens.Infer(context.Background(), hedgingObs(t, "output:x", "   "), fixedNow); err != ErrNotApplicable {
		t.Fatalf("blank text must be ErrNotApplicable, got %v", err)
	}
	// absent payload entirely -> also not applicable.
	if _, err := lens.Infer(context.Background(), Observation{Subject: "output:x"}, fixedNow); err != ErrNotApplicable {
		t.Fatalf("absent payload must be ErrNotApplicable, got %v", err)
	}
	// a missing subject is a real error, not a skip.
	if _, err := lens.Infer(context.Background(), hedgingObs(t, "", hedgyOutput), fixedNow); err == nil {
		t.Fatal("missing subject must be rejected")
	}
}

// A produced signal validates, records to the append-only ledger under a temp COMPOUND_HOME, reads
// back, and survives the public projection (it is public_safe — it infers OUTPUT behavior).
func TestHedgingLensSignalValidatesRecordsAndReadsBack(t *testing.T) {
	hedgy := inferHedging(t, "output:hedgy", hedgyOutput)
	overclaim := inferHedging(t, "output:overclaim", overclaimOutput)

	if err := hedgy.Validate(); err != nil {
		t.Fatalf("hedgy signal must validate: %v", err)
	}
	if err := overclaim.Validate(); err != nil {
		t.Fatalf("overclaim signal must validate: %v", err)
	}

	home := t.TempDir()
	if err := RecordSignal(home, hedgy); err != nil {
		t.Fatalf("record hedgy: %v", err)
	}
	if err := RecordSignal(home, overclaim); err != nil {
		t.Fatalf("record overclaim: %v", err)
	}
	rows, err := LoadSignals(home)
	if err != nil {
		t.Fatalf("load signals: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 recorded signal rows, got %d", len(rows))
	}
	// public_safe -> both survive the public projection (the only set a public-emitting surface reads).
	if pub := PublicProjection(rows); len(pub) != 2 {
		t.Fatalf("public_safe hedging signals must survive the public projection, got %d", len(pub))
	}
}
