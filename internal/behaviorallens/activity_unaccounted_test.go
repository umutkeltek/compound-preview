package behaviorallens

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func activityObs(t *testing.T) Observation {
	t.Helper()
	payload := ActivityUnaccountedPayload{
		Date: "2026-07-15",
		Blocks: []ActivityUnaccountedBlock{
			{Start: "2026-07-15T09:00:00Z", End: "2026-07-15T09:47:00Z", ActiveMin: 45, DurationMin: 47, CategoryMix: "browsing/design"},
			{Start: "2026-07-15T14:00:00Z", End: "2026-07-15T14:25:00Z", ActiveMin: 24, DurationMin: 25, CategoryMix: "coding"},
		},
		AccountedRefs: []string{"rec_a", "rec_b"},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return Observation{Subject: "2026-07-15", Payload: raw}
}

// TestActivityUnaccounted_Infer proves the deterministic signal math: value = total unaccounted
// minutes (47+25), operator_private visibility.
func TestActivityUnaccounted_Infer(t *testing.T) {
	now := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	sig, err := ActivityUnaccountedLens{}.Infer(context.Background(), activityObs(t), now)
	if err != nil {
		t.Fatalf("infer: %v", err)
	}
	if sig.Value != 72 {
		t.Fatalf("value = %.1f, want 72 (47+25 min total)", sig.Value)
	}
	if sig.Visibility != VisibilityOperatorPrivate {
		t.Fatalf("visibility = %q, want operator_private", sig.Visibility)
	}
	if sig.Method != MethodLedgerReconciliation {
		t.Fatalf("method = %q, want %q", sig.Method, MethodLedgerReconciliation)
	}
}

// TestActivityUnaccounted_EmptyBlocks_NotApplicable proves an empty-blocks payload is a skip.
func TestActivityUnaccounted_EmptyBlocks_NotApplicable(t *testing.T) {
	raw, _ := json.Marshal(ActivityUnaccountedPayload{Date: "2026-07-15"})
	_, err := ActivityUnaccountedLens{}.Infer(context.Background(), Observation{Subject: "2026-07-15", Payload: raw}, time.Now())
	if err != ErrNotApplicable {
		t.Fatalf("empty blocks err = %v, want ErrNotApplicable", err)
	}
}

// TestFalsifier10_ActivityUnaccountedVisibilityBoundary is the visibility boundary end to end:
//   - stranger clone (empty Config): the lens is registered but SKIPPED by the Compose gate (no
//     signal), even though the observation IS applicable — absence is only the gate.
//   - opted-in home: the lens RUNS, produces an operator_private signal, and PublicProjection drops
//     it — the value never leaves COMPOUND_HOME.
func TestFalsifier10_ActivityUnaccountedVisibilityBoundary(t *testing.T) {
	now := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	obs := activityObs(t)

	strangerCfg := Config{}
	strangerRes, err := RegistryForHome(strangerCfg).Compose(context.Background(), obs, strangerCfg, now)
	if err != nil {
		t.Fatalf("stranger compose: %v", err)
	}
	if findSignal(strangerRes.Signals, ActivityUnaccountedLensID) != nil {
		t.Fatalf("stranger clone leaked a private activity signal: %+v", strangerRes.Signals)
	}
	skip := findSkip(strangerRes.Skipped, ActivityUnaccountedLensID)
	if skip == nil {
		t.Fatalf("stranger clone should SKIP the private lens; skipped=%+v", strangerRes.Skipped)
	}
	if skip.Visibility != VisibilityOperatorPrivate {
		t.Fatalf("skip visibility = %q, want operator_private", skip.Visibility)
	}

	enabledCfg := Config{EnabledPrivateLenses: []string{ActivityUnaccountedLensID}}
	enabledRes, err := RegistryForHome(enabledCfg).Compose(context.Background(), obs, enabledCfg, now)
	if err != nil {
		t.Fatalf("enabled compose: %v", err)
	}
	sig := findSignal(enabledRes.Signals, ActivityUnaccountedLensID)
	if sig == nil {
		t.Fatalf("opted-in home should RUN the private lens; signals=%+v skipped=%+v", enabledRes.Signals, enabledRes.Skipped)
	}
	if sig.Visibility != VisibilityOperatorPrivate {
		t.Fatalf("signal visibility = %q, want operator_private", sig.Visibility)
	}

	// THE OUT-GATE: the private signal's value never leaves the home.
	if got := PublicProjection([]InferredSignal{*sig}); len(got) != 0 {
		t.Fatalf("PublicProjection([private signal]) = %+v, want empty", got)
	}
}
