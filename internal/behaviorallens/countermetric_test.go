package behaviorallens

import (
	"context"
	"encoding/json"
	"testing"
)

func counterMetricObs(t *testing.T, subject string, payload CounterMetricPayload) Observation {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal counter-metric payload: %v", err)
	}
	return Observation{SchemaVersion: ObservationSchemaVersion, Subject: subject, Payload: b}
}

func inferCounterMetric(t *testing.T, subject, summary string) InferredSignal {
	t.Helper()
	sig, err := CounterMetricLens{}.Infer(context.Background(), counterMetricObs(t, subject, CounterMetricPayload{Summary: summary}), fixedNow)
	if err != nil {
		t.Fatalf("infer %s: %v", subject, err)
	}
	return sig
}

// Synthetic close-summary text only (no personal/product names): the repo is destined public.
const (
	// an improvement claim (Nx multiplier + arrow numeric transition), with NO counter-metric named.
	unpairedClaimSummary = "Landed the change: attention 13-26s → 3-5s, 2.9x per-engineer throughput."
	// the SAME improvement claim, WITH counter-metric pairing text.
	pairedClaimSummary = "Landed the change: attention 13-26s → 3-5s, 2.9x per-engineer throughput; " +
		"reversion rate flat, review latency held."
	// no improvement claim at all: ordinary work, nothing to pair-check.
	noClaimSummary = "Recorded the decision and parked the intake for later adjudication."
)

// FALSIFIER 1 — the load-bearing assertion. An improvement claim with NO counter-metric named scores
// the NEGATIVE (unpaired) pole. This is the observed failure-pattern shape the lens exists to catch.
func TestCounterMetricLensUnpairedClaimScoresNegativePole(t *testing.T) {
	sig := inferCounterMetric(t, "receipt:unpaired", unpairedClaimSummary)
	if sig.Value >= 0 {
		t.Fatalf("an improvement claim with no counter-metric must score the NEGATIVE pole, got %.3f", sig.Value)
	}
	if sig.Visibility != VisibilityPublicSafe || sig.Method != MethodCounterMetricPairing {
		t.Fatalf("counter-metric lens is public_safe/counter_metric_pairing, got vis=%s method=%s", sig.Visibility, sig.Method)
	}
	if sig.LensID != CounterMetricLensID {
		t.Fatalf("lens id must be %q, got %q", CounterMetricLensID, sig.LensID)
	}
	if sig.Confidence <= 0 {
		t.Fatalf("a marker-rich unpaired claim must carry positive confidence, got %.3f", sig.Confidence)
	}
}

// FALSIFIER 2 — the SAME improvement claim, paired with counter-metric text, scores the POSITIVE
// pole: the honest claim-structure shape the source steal named as the model to reward.
func TestCounterMetricLensPairedClaimScoresPositivePole(t *testing.T) {
	sig := inferCounterMetric(t, "receipt:paired", pairedClaimSummary)
	if sig.Value <= 0 {
		t.Fatalf("an improvement claim paired with a counter-metric must score the POSITIVE pole, got %.3f", sig.Value)
	}
}

// pole separation: paired scores strictly higher than unpaired for the identical improvement claim —
// pairing text is what moves the score, not the improvement claim itself.
func TestCounterMetricLensPoleSeparation(t *testing.T) {
	unpaired := inferCounterMetric(t, "receipt:a", unpairedClaimSummary)
	paired := inferCounterMetric(t, "receipt:b", pairedClaimSummary)
	if !(unpaired.Value < 0 && paired.Value > 0 && unpaired.Value < paired.Value) {
		t.Fatalf("poles not separated: unpaired=%.3f paired=%.3f", unpaired.Value, paired.Value)
	}
}

// FALSIFIER 3 — a receipt summary with no improvement claim at all is ErrNotApplicable; Compose
// skips it without failing (mirrors TestComposeSkipsNotApplicableWithoutFailing in behaviorallens_test.go).
func TestCounterMetricLensNoClaimNotApplicable(t *testing.T) {
	var lens Lens = CounterMetricLens{}
	obs := counterMetricObs(t, "receipt:none", CounterMetricPayload{Summary: noClaimSummary})
	if _, err := lens.Infer(context.Background(), obs, fixedNow); err != ErrNotApplicable {
		t.Fatalf("a summary with no improvement claim must be ErrNotApplicable, got %v", err)
	}
	// blank summary / absent payload -> also not applicable.
	if _, err := lens.Infer(context.Background(), counterMetricObs(t, "receipt:blank", CounterMetricPayload{Summary: "   "}), fixedNow); err != ErrNotApplicable {
		t.Fatalf("blank summary must be ErrNotApplicable, got %v", err)
	}
	if _, err := lens.Infer(context.Background(), Observation{Subject: "receipt:none"}, fixedNow); err != ErrNotApplicable {
		t.Fatalf("absent payload must be ErrNotApplicable, got %v", err)
	}
	// a missing subject is a real error, not a skip.
	if _, err := lens.Infer(context.Background(), counterMetricObs(t, "", CounterMetricPayload{Summary: unpairedClaimSummary}), fixedNow); err == nil {
		t.Fatal("missing subject must be rejected")
	}

	r := NewRegistry()
	mustRegister(t, r, CounterMetricLens{})
	res, err := r.Compose(context.Background(), obs, Config{}, fixedNow)
	if err != nil {
		t.Fatalf("compose must not fail on a not-applicable lens: %v", err)
	}
	if len(res.Signals) != 0 {
		t.Fatalf("no improvement claim should yield no signal, got %+v", res.Signals)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].LensID != CounterMetricLensID {
		t.Fatalf("the not-applicable lens should be a legible skip, got %+v", res.Skipped)
	}
}

// A percentage is not automatically an improvement delta. Completion summaries routinely report
// coverage/pass rates; treating those as improvement claims would manufacture an unpaired-claim
// failure where the receipt made no comparative claim at all.
func TestCounterMetricLensNonComparativePercentageNotApplicable(t *testing.T) {
	obs := counterMetricObs(t, "receipt:pass-rate", CounterMetricPayload{
		Summary: "Verification completed with 100% of tests passing.",
	})
	if _, err := (CounterMetricLens{}).Infer(context.Background(), obs, fixedNow); err != ErrNotApplicable {
		t.Fatalf("a non-comparative pass rate must be ErrNotApplicable, got %v", err)
	}
}

func TestCounterMetricLensComparativePercentagesRemainImprovementClaims(t *testing.T) {
	for _, summary := range []string{
		"Latency is 38% smaller.",
		"Throughput changed by +20%.",
	} {
		sig := inferCounterMetric(t, "receipt:percent-delta", summary)
		if sig.Value >= 0 {
			t.Fatalf("comparative percentage %q must remain an unpaired improvement claim, got %.3f", summary, sig.Value)
		}
	}
}

func TestCounterMetricLensUnrelatedHeldAndStayedDoNotPairClaim(t *testing.T) {
	sig := inferCounterMetric(t, "receipt:unrelated-counter-words",
		"Cut processing time in half; held a review and stayed to answer questions.")
	if sig.Value >= 0 {
		t.Fatalf("unrelated held/stayed prose must not pair an improvement claim, got %.3f", sig.Value)
	}
}

func TestCounterMetricLensNonComparativeCutNotApplicable(t *testing.T) {
	for _, summary := range []string{
		"Cut the ribbon and held a review before launch.",
		"Cut the ribbon. The report was filed in half an hour.",
		"Cut the ribbon and filed the report in half an hour.",
	} {
		obs := counterMetricObs(t, "receipt:ceremony", CounterMetricPayload{Summary: summary})
		if _, err := (CounterMetricLens{}).Infer(context.Background(), obs, fixedNow); err != ErrNotApplicable {
			t.Fatalf("a non-comparative use of cut must be ErrNotApplicable, got %v (summary=%q)", err, summary)
		}
	}
}

// FALSIFIER 5 — visibility: the signal survives PublicProjection (public_safe) and validates through
// newSignal / RecordSignal / LoadSignals round-trip.
func TestCounterMetricLensSignalValidatesRecordsAndReadsBack(t *testing.T) {
	unpaired := inferCounterMetric(t, "receipt:unpaired", unpairedClaimSummary)
	paired := inferCounterMetric(t, "receipt:paired", pairedClaimSummary)

	if err := unpaired.Validate(); err != nil {
		t.Fatalf("unpaired signal must validate: %v", err)
	}
	if err := paired.Validate(); err != nil {
		t.Fatalf("paired signal must validate: %v", err)
	}

	home := t.TempDir()
	if err := RecordSignal(home, unpaired); err != nil {
		t.Fatalf("record unpaired: %v", err)
	}
	if err := RecordSignal(home, paired); err != nil {
		t.Fatalf("record paired: %v", err)
	}
	rows, err := LoadSignals(home)
	if err != nil {
		t.Fatalf("load signals: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 recorded signal rows, got %d", len(rows))
	}
	if pub := PublicProjection(rows); len(pub) != 2 {
		t.Fatalf("public_safe counter-metric signals must survive the public projection, got %d", len(pub))
	}
}

// FALSIFIER 6 — registry: DefaultRegistry() registers the counter-metric lens exactly once, alongside
// sycophancy and hedging (TestRegistryRefusesDuplicateAndBlankLens in behaviorallens_test.go stays green).
func TestDefaultRegistryIncludesCounterMetricExactlyOnce(t *testing.T) {
	r := DefaultRegistry()
	ids := r.IDs()
	var count int
	for _, id := range ids {
		if id == CounterMetricLensID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected counter-metric registered exactly once in DefaultRegistry, got %d (ids=%v)", count, ids)
	}
	if len(ids) != 3 {
		t.Fatalf("expected 3 default lenses (sycophancy, hedging, counter-metric), got %d: %v", len(ids), ids)
	}
	lens, ok := r.Lens(CounterMetricLensID)
	if !ok || lens.Visibility() != VisibilityPublicSafe {
		t.Fatalf("counter-metric lens must be registered public_safe, ok=%v", ok)
	}
}

// every unambiguous counter-metric marker phrase, paired with a plain improvement marker, scores
// positive — proves each marker in counterMetricWordMarkers is actually wired.
func TestCounterMetricLensAllCounterMarkersWired(t *testing.T) {
	for _, marker := range counterMetricWordMarkers {
		summary := "This is faster now; " + marker + "."
		sig := inferCounterMetric(t, "receipt:marker", summary)
		if sig.Value <= 0 {
			t.Fatalf("counter-metric marker %q must pair to a positive pole, got %.3f (summary=%q)", marker, sig.Value, summary)
		}
	}
}

func TestCounterMetricLensContextualHeldAndStayedMarkersWired(t *testing.T) {
	for _, summary := range []string{
		"This is faster now; review latency held.",
		"This is faster now; reversion rate stayed.",
	} {
		sig := inferCounterMetric(t, "receipt:contextual-marker", summary)
		if sig.Value <= 0 {
			t.Fatalf("contextual counter-metric marker must pair to a positive pole, got %.3f (summary=%q)", sig.Value, summary)
		}
	}
}

// every operator-named improvement marker word, alone (no pairing), scores the negative pole —
// proves each marker in improvementWordMarkers is actually wired.
func TestCounterMetricLensAllImprovementMarkersWired(t *testing.T) {
	for _, marker := range improvementWordMarkers {
		summary := "This got " + marker + " overall."
		sig := inferCounterMetric(t, "receipt:marker", summary)
		if sig.Value >= 0 {
			t.Fatalf("improvement marker %q alone must score the negative (unpaired) pole, got %.3f (summary=%q)", marker, sig.Value, summary)
		}
	}
}
