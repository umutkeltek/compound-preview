package actuator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
)

func testPlan(class, rev string, mode DecisionMode, outcome string, headroom bool, channel string) Plan {
	return Plan{
		Decision:       Decision{Mode: mode, PolicyOutcome: outcome, Channel: channel, Reasons: []string{"reason"}},
		PolicyDecision: envelope.PolicyDecision{Outcome: outcome},
		Headroom:       Headroom{Allowed: headroom},
		Intent: envelope.ActionIntent{
			Header:        envelope.Header{IDs: envelope.IDs{WorkID: "work_1"}},
			Capability:    "cap_x",
			MutationClass: class,
			Reversibility: rev,
		},
		Channel:   channel,
		ProductID: "product:demo",
	}
}

func TestReleaseGateFinancialIsHumanGatedSpend(t *testing.T) {
	card := ReleaseGateFor(testPlan("financial", "hard", ModeHold, "hold", true, "paid_marketing"), testNow)
	g := strings.ToLower(card.ReleaseGate)
	if !strings.Contains(g, "never spend") || !strings.Contains(g, "human") {
		t.Fatalf("financial gate must say spend is human-gated and actuator never spends, got %q", card.ReleaseGate)
	}
	if !strings.Contains(g, "dry-run") {
		t.Fatalf("financial gate must require a dry-run first, got %q", card.ReleaseGate)
	}
	if len(card.RequiredInputs) == 0 || len(card.StopConditions) == 0 {
		t.Fatalf("financial card must carry required inputs + stop conditions, got %+v", card)
	}
	if card.Decision != "hold" || card.MutationClass != "financial" {
		t.Fatalf("card must reflect the held financial move, got %+v", card)
	}
}

func TestReleaseGateExternalIsStagedHumanRelease(t *testing.T) {
	card := ReleaseGateFor(testPlan("external_side_effect", "hard", ModeHold, "hold", true, "aso"), testNow)
	g := strings.ToLower(card.ReleaseGate)
	if !strings.Contains(g, "stages locally") || !strings.Contains(g, "human releases") {
		t.Fatalf("external gate must say actuator stages only and a human releases, got %q", card.ReleaseGate)
	}
	if !strings.Contains(g, "tier-3") {
		t.Fatalf("external gate must require Tier-3 policy (outbound-distribution doctrine), got %q", card.ReleaseGate)
	}
}

func TestReleaseGateBudgetForAutoEligibleClass(t *testing.T) {
	card := ReleaseGateFor(testPlan("local_reversible", "easy", ModeHold, "allow", false, "distribution"), testNow)
	g := strings.ToLower(card.ReleaseGate)
	if !strings.Contains(g, "budget exhausted") {
		t.Fatalf("auto-eligible class held for headroom must yield a budget gate, got %q", card.ReleaseGate)
	}
}

func TestReleaseGateDenyIsTerminal(t *testing.T) {
	card := ReleaseGateFor(testPlan("financial", "hard", ModeDeny, "deny", true, "paid_marketing"), testNow)
	if !strings.Contains(strings.ToLower(card.ReleaseGate), "denied") {
		t.Fatalf("deny must produce a terminal DENIED gate, got %q", card.ReleaseGate)
	}
	if card.Decision != "deny" {
		t.Fatalf("expected deny decision, got %s", card.Decision)
	}
}

func TestReleaseGateBudgetAccountingAndUnknownExecutionDoNotSuggestReset(t *testing.T) {
	for _, scenario := range []string{"provider-accounting", "unresolved-reservation"} {
		t.Run(scenario, func(t *testing.T) {
			plan := testPlan("local_reversible", "easy", ModeHold, "allow", false, "execution")
			if scenario == "provider-accounting" {
				plan.Headroom.RequiresProviderAccounting = true
			} else {
				plan.Headroom.PendingReservationIDs = []string{"bres_test"}
			}
			card := ReleaseGateFor(plan, testNow)
			if strings.Contains(card.ReleaseGate, "Wait for the rolling window to reset") || strings.Contains(card.ReleaseGate, "budget-set --channel") {
				t.Fatalf("unsatisfiable budget repair advice: %s", card.ReleaseGate)
			}
			if len(card.RequiredInputs) == 0 || len(card.StopConditions) == 0 {
				t.Fatalf("budget hold is not actionable: %+v", card)
			}
		})
	}
}

func TestWriteAndListHeldMoves(t *testing.T) {
	home := t.TempDir()
	card := ReleaseGateFor(testPlan("financial", "hard", ModeHold, "hold", true, "paid_marketing"), testNow)
	path, err := WriteHeldMoveCard(home, card)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("held-move card not written: %v", err)
	}
	// A second product's card must be filterable.
	other := ReleaseGateFor(testPlan("external_side_effect", "hard", ModeHold, "hold", true, "aso"), testNow)
	other.ProductID = "product:other"
	if _, err := WriteHeldMoveCard(home, other); err != nil {
		t.Fatal(err)
	}
	all, err := ListHeldMoves(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 held moves, got %d", len(all))
	}
	onlyDemo, err := ListHeldMoves(home, "product:demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyDemo) != 1 || onlyDemo[0].ProductID != "product:demo" {
		t.Fatalf("expected product filter to return 1 demo card, got %+v", onlyDemo)
	}
}

func TestPostHoldPersistsActionCard(t *testing.T) {
	home := t.TempDir()
	createKnownActuatorWork(t, home, "work_1")
	cfg := policy.DefaultConfig()
	ledger := OpenLedger(home)
	budget := BudgetEnvelope{
		SchemaVersion: BudgetSchemaVersion, ID: "budget_product-demo", ProductID: "product:demo",
		CreatedAt: testNow.Format(time.RFC3339),
		Channels:  []ChannelLimits{{Channel: "distribution", WindowHours: 24, MaxActions: 1}},
	}
	if err := ledger.SaveBudget(budget); err != nil {
		t.Fatal(err)
	}
	in := PostInput{WorkID: "work_1", ProductID: "product:demo", Channel: "distribution", Surface: "x", Content: "one", Metric: "posthog_signups"}
	if _, err := Post(home, in, cfg, testNow); err != nil {
		t.Fatal(err)
	}
	// Second post exhausts the budget -> held AND a persisted, actionable card.
	in.Content = "two"
	res, err := Post(home, in, cfg, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Held || res.HeldMoveID == "" {
		t.Fatalf("expected a held post with a card, got %+v", res)
	}
	data, err := os.ReadFile(res.HeldMovePath)
	if err != nil {
		t.Fatalf("held-move card missing: %v", err)
	}
	if !strings.Contains(string(data), "Budget exhausted") {
		t.Fatalf("budget-held card must carry the budget release gate, got %s", string(data))
	}
}

func TestPostFailsClosedWhenHeldMoveCardCannotBePersisted(t *testing.T) {
	home := t.TempDir()
	createKnownActuatorWork(t, home, "work_post")

	ledger := OpenLedger(home)
	budget := BudgetEnvelope{
		SchemaVersion: BudgetSchemaVersion,
		ID:            "budget_product-demo",
		ProductID:     "product:demo",
		CreatedAt:     testNow.Format(time.RFC3339),
		Channels:      []ChannelLimits{{Channel: "distribution", WindowHours: 24, MaxActions: 1}},
	}
	if err := ledger.SaveBudget(budget); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Record(Consumption{
		At:        testNow.Format(time.RFC3339),
		ProductID: "product:demo",
		Channel:   "distribution",
		Actions:   1,
		WorkID:    "work_post",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "actuator", "held-moves"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := Post(home, PostInput{
		WorkID:    "work_post",
		ProductID: "product:demo",
		Channel:   "distribution",
		Surface:   "x",
		Content:   "must remain held",
	}, policy.DefaultConfig(), testNow)

	if err == nil || !strings.Contains(err.Error(), "persist held-move card") {
		t.Fatalf("held-card persistence failure must surface, result=%+v err=%v", result, err)
	}
	if result.Held || result.HeldMoveID != "" || result.HeldMovePath != "" {
		t.Fatalf("an unpersisted card must not be reported as a successful hold: %+v", result)
	}
}
