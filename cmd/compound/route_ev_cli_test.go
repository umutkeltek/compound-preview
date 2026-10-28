package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/home"
	"compound/internal/routeev"
	"compound/internal/routeevidence"
)

type routeEvCLIOutput struct {
	Card routeev.Card `json:"card"`
	Ref  string       `json:"ref"`
	Path string       `json:"path"`
}

func setupRouteEvHome(t *testing.T) string {
	t.Helper()
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	src := filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml")
	captureStdout(t, func() { cmdRoute([]string{"import-model-capabilities", src}) })
	return homePath
}

func runRouteEvJSON(t *testing.T, args ...string) routeEvCLIOutput {
	t.Helper()
	out := captureStdout(t, func() { cmdRoute(append([]string{"ev"}, args...)) })
	var parsed routeEvCLIOutput
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("parse route ev --json: %v\n%s", err, out)
	}
	return parsed
}

// TestRouteEvSharesPolicyLaneWithRoutePlan is the S2 depth test named in the spec (§7): `route ev`
// MUST resolve its policy line through the SAME code path as `route plan --capability-class`, not
// a second routing opinion. It calls both subcommands against one fixture home and asserts their
// resolved lane is byte-identical. This is pinned on the OBSERVABLE (the resolved lane_id), not on
// an implementation detail, so it fails if a future edit forks the two resolutions apart.
func TestRouteEvSharesPolicyLaneWithRoutePlan(t *testing.T) {
	setupRouteEvHome(t)

	planOut := captureStdout(t, func() {
		cmdRoute([]string{"plan", "--capability-class", "classify-extract", "--json"})
	})
	var plan routeevidence.CapabilityRoutePlan
	if err := json.Unmarshal([]byte(planOut), &plan); err != nil {
		t.Fatalf("parse route plan --json: %v\n%s", err, planOut)
	}
	if len(plan.Chain) == 0 {
		t.Fatalf("route plan resolved an empty chain: %s", planOut)
	}

	evOut := runRouteEvJSON(t, "--capability-class", "classify-extract", "--deviation-lane", "scout/chat", "--json")

	if evOut.Card.PolicyLine.LaneID != plan.Chain[0].ModelID {
		t.Fatalf("route ev policy_line.lane_id = %q, want %q (route plan's resolved lane, byte-identical)",
			evOut.Card.PolicyLine.LaneID, plan.Chain[0].ModelID)
	}
	if evOut.Card.PolicyLine.LaneID != "auto/extract" {
		t.Fatalf("sanity: fixture's classify-extract chain should lead with auto/extract, got %q", evOut.Card.PolicyLine.LaneID)
	}
}

// TestRouteEvUnpriceableNamesMissingDeviationCost is the S2 depth test: when the deviation lane
// has no cost_per_attempt evidence, the card must come out `unpriceable` naming the gap — and this
// must be a SUCCESSFUL computation (the CLI call returns normally; a refusing card is not a
// command failure). The policy line's cost IS primed here so the test isolates the deviation gap
// specifically, rather than both lanes failing for the same reason.
func TestRouteEvUnpriceableNamesMissingDeviationCost(t *testing.T) {
	setupRouteEvHome(t)

	ledger := filepath.Join(t.TempDir(), "provider-calls.ndjson")
	row := `{"request_id":"req_policy_1","ts_start":"2026-07-01T00:00:00Z","caller":"test","project":"fixture","purpose":"fixture","route_alias":"auto/extract","provider":"fixture-provider","model":"auto/extract","transport":"chat_completions","input_tokens":10,"output_tokens":2,"estimated_cost_usd":0.02,"latency_ms":40,"status":"ok"}` + "\n"
	if err := os.WriteFile(ledger, []byte(row), 0o644); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() { cmdRoute([]string{"import-provider-ledger", ledger}) })

	evOut := runRouteEvJSON(t, "--capability-class", "classify-extract", "--deviation-lane", "scout/chat", "--json")

	if evOut.Card.Verdict != routeev.VerdictUnpriceable {
		t.Fatalf("verdict = %q, want %q: %+v", evOut.Card.Verdict, routeev.VerdictUnpriceable, evOut.Card)
	}
	foundDeviationCost := false
	for _, m := range evOut.Card.MissingInputs {
		if m == "deviation.cost_per_attempt" {
			foundDeviationCost = true
		}
	}
	if !foundDeviationCost {
		t.Fatalf("missing_inputs = %v, want it to name deviation.cost_per_attempt", evOut.Card.MissingInputs)
	}
	// Policy line's cost WAS primed (unlike the deviation's) — the card only refuses on the gaps
	// that actually exist.
	if evOut.Card.PolicyLine.CostPerAttempt == nil {
		t.Fatalf("policy_line.cost_per_attempt should have resolved from the imported ledger: %+v", evOut.Card.PolicyLine)
	}
	if evOut.Ref == "" || evOut.Path == "" {
		t.Fatalf("unpriceable card should still be stored (a refusing card is a successful computation): ref=%q path=%q", evOut.Ref, evOut.Path)
	}
}

// TestRouteEvCardRoundTripsThroughStore is the S2 depth test: the stored blob round-trips byte-
// identically. It composes the same card twice (idempotent write -> same ref), reads the ref back
// through routeev.ReadBytes, and checks those bytes decode to the exact same Card whose own
// canonical re-marshal is byte-identical to what was read — no corruption, no drift on write.
func TestRouteEvCardRoundTripsThroughStore(t *testing.T) {
	homePath := setupRouteEvHome(t)

	first := runRouteEvJSON(t, "--capability-class", "classify-extract", "--deviation-lane", "scout/chat", "--json")
	second := runRouteEvJSON(t, "--capability-class", "classify-extract", "--deviation-lane", "scout/chat", "--json")

	if first.Ref == "" {
		t.Fatalf("route ev did not return a stored ref: %+v", first)
	}
	if first.Ref != second.Ref {
		t.Fatalf("composing the identical card twice produced different refs: %q vs %q (Compose is not deterministic, or the store is not content-addressed)", first.Ref, second.Ref)
	}

	stored, err := routeev.ReadBytes(homePath, first.Ref)
	if err != nil {
		t.Fatalf("read stored card back: %v", err)
	}
	remarshaled, err := json.Marshal(first.Card)
	if err != nil {
		t.Fatalf("remarshal parsed card: %v", err)
	}
	if !bytes.Equal(stored, remarshaled) {
		t.Fatalf("stored card bytes do not round-trip byte-identically:\nstored=%s\nremarshaled=%s", stored, remarshaled)
	}

	// The path printed by the CLI is exactly what routeev.Store wrote.
	fromDisk, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatalf("read printed store path: %v", err)
	}
	if !bytes.Equal(fromDisk, stored) {
		t.Fatalf("printed path contents differ from routeev.ReadBytes contents:\ndisk=%s\nstore=%s", fromDisk, stored)
	}
}

// TestRouteEvClassBucketEscapeHatchPricesFromUnknownBucket exercises the full pricing path end to
// end and the --class-bucket unknown escape hatch (spec §7 S2, S0 finding): today's completion
// evidence sits under capability class "unknown", so by DEFAULT a card for a real capability class
// like "classify-extract" is unpriceable on posterior (a missing cell is a missing input, never a
// silent fallback). With --class-bucket unknown, the same evidence prices the card, and the
// fallback is visibly marked in belief_refs — never silent.
func TestRouteEvClassBucketEscapeHatchPricesFromUnknownBucket(t *testing.T) {
	homePath := setupRouteEvHome(t)

	// Seed completion evidence for both lanes through the real fold path (public CLI surface):
	// 6/6 completed for the policy line (auto/extract), 5/5 completed for the deviation
	// (scout/chat) -- both at/above MIN_LANE_RECEIPTS, both land in the "unknown" bucket because
	// completionCapabilityClass always returns "unknown" today (S0's documented scope).
	seedCLILaneNode(t, homePath, "auto/extract")
	seedCLILaneNode(t, homePath, "scout/chat")
	for i := 0; i < 6; i++ {
		writeCLILaneBeliefReceiptEvent(t, homePath, fmt.Sprintf("rec_auto_%02d", i), fmt.Sprintf("work_auto_%02d", i), "auto/extract")
	}
	for i := 0; i < 5; i++ {
		writeCLILaneBeliefReceiptEvent(t, homePath, fmt.Sprintf("rec_scout_%02d", i), fmt.Sprintf("work_scout_%02d", i), "scout/chat")
	}
	captureStdout(t, func() { cmdLaneBeliefs([]string{"fold", "--experimental"}) })

	// Cost per attempt: policy line $0.02/attempt, deviation $0.01/attempt (cheaper).
	ledger := filepath.Join(t.TempDir(), "provider-calls.ndjson")
	rows := strings.Join([]string{
		`{"request_id":"req_policy_1","ts_start":"2026-07-01T00:00:00Z","caller":"test","project":"fixture","purpose":"fixture","route_alias":"auto/extract","provider":"fixture-provider","model":"auto/extract","transport":"chat_completions","input_tokens":10,"output_tokens":2,"estimated_cost_usd":0.02,"latency_ms":40,"status":"ok"}`,
		`{"request_id":"req_dev_1","ts_start":"2026-07-01T00:05:00Z","caller":"test","project":"fixture","purpose":"fixture","route_alias":"scout/chat","provider":"fixture-provider","model":"scout/chat","transport":"chat_completions","input_tokens":10,"output_tokens":2,"estimated_cost_usd":0.01,"latency_ms":40,"status":"ok"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(ledger, []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() { cmdRoute([]string{"import-provider-ledger", ledger}) })

	// Default (no escape hatch): classify-extract has no completion cell yet -> unpriceable,
	// naming BOTH lanes' posterior as missing (never a silent "unknown" read).
	byDefault := runRouteEvJSON(t, "--capability-class", "classify-extract", "--deviation-lane", "scout/chat", "--json")
	if byDefault.Card.Verdict != routeev.VerdictUnpriceable {
		t.Fatalf("default (no --class-bucket) verdict = %q, want unpriceable: %+v", byDefault.Card.Verdict, byDefault.Card)
	}
	wantMissing := map[string]bool{"policy_line.posterior": false, "deviation.posterior": false}
	for _, m := range byDefault.Card.MissingInputs {
		if _, ok := wantMissing[m]; ok {
			wantMissing[m] = true
		}
	}
	for name, found := range wantMissing {
		if !found {
			t.Fatalf("default missing_inputs = %v, want it to include %q", byDefault.Card.MissingInputs, name)
		}
	}

	// With the escape hatch: both posteriors resolve from the "unknown" bucket, and the card
	// prices. policy EV = 0.02/0.8 = 0.025; required_posterior = 0.01/0.025 = 0.4;
	// deviation posterior ~0.778 >= 0.4 -> priced_right.
	withBucket := runRouteEvJSON(t, "--capability-class", "classify-extract", "--deviation-lane", "scout/chat", "--risk-grade", "judge", "--class-bucket", "unknown", "--json")
	if withBucket.Card.Verdict != routeev.VerdictPricedRight {
		t.Fatalf("--class-bucket unknown verdict = %q, want priced_right: %+v", withBucket.Card.Verdict, withBucket.Card)
	}
	if withBucket.Card.RequiredPosterior == nil {
		t.Fatalf("priced card must carry required_posterior: %+v", withBucket.Card)
	}
	if got, want := *withBucket.Card.RequiredPosterior, 0.4; got < want-1e-6 || got > want+1e-6 {
		t.Fatalf("required_posterior = %v, want ~%v", got, want)
	}
	if !withBucket.Card.PredictionRequired {
		t.Fatalf("risk_grade=judge must set prediction_required: %+v", withBucket.Card)
	}
	for _, lane := range []routeev.LaneCard{withBucket.Card.PolicyLine, withBucket.Card.Deviation} {
		if lane.Posterior == nil {
			t.Fatalf("lane %s posterior should have resolved via the unknown bucket: %+v", lane.LaneID, lane)
		}
		marked := false
		for _, ref := range lane.Inputs.BeliefRefs {
			if ref == "posterior_class_bucket:unknown" {
				marked = true
			}
		}
		if !marked {
			t.Fatalf("lane %s belief_refs = %v, want the visible posterior_class_bucket:unknown marker (the fallback must never be silent)", lane.LaneID, lane.Inputs.BeliefRefs)
		}
	}
}
