package actuator

import (
	"testing"
	"time"
)

var testNow = time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

func TestBudgetValidateRejectsBadEnvelope(t *testing.T) {
	cases := map[string]BudgetEnvelope{
		"wrong schema":  {SchemaVersion: "x", ID: "b", Channels: []ChannelLimits{{Channel: "*", WindowHours: 24}}, CreatedAt: testNow.Format(time.RFC3339)},
		"no id":         {SchemaVersion: BudgetSchemaVersion, Channels: []ChannelLimits{{Channel: "*", WindowHours: 24}}, CreatedAt: testNow.Format(time.RFC3339)},
		"no channels":   {SchemaVersion: BudgetSchemaVersion, ID: "b", CreatedAt: testNow.Format(time.RFC3339)},
		"zero window":   {SchemaVersion: BudgetSchemaVersion, ID: "b", Channels: []ChannelLimits{{Channel: "*", WindowHours: 0}}, CreatedAt: testNow.Format(time.RFC3339)},
		"neg limit":     {SchemaVersion: BudgetSchemaVersion, ID: "b", Channels: []ChannelLimits{{Channel: "*", WindowHours: 24, MaxActions: -1}}, CreatedAt: testNow.Format(time.RFC3339)},
		"dup channel":   {SchemaVersion: BudgetSchemaVersion, ID: "b", Channels: []ChannelLimits{{Channel: "*", WindowHours: 24}, {Channel: "*", WindowHours: 24}}, CreatedAt: testNow.Format(time.RFC3339)},
		"bad timestamp": {SchemaVersion: BudgetSchemaVersion, ID: "b", Channels: []ChannelLimits{{Channel: "*", WindowHours: 24}}, CreatedAt: "not-a-time"},
	}
	for name, b := range cases {
		if err := b.Validate(); err == nil {
			t.Errorf("%s: expected validation error, got nil", name)
		}
	}
}

func TestLimitsForExactThenWildcard(t *testing.T) {
	b := BudgetEnvelope{
		SchemaVersion: BudgetSchemaVersion, ID: "b", CreatedAt: testNow.Format(time.RFC3339),
		Channels: []ChannelLimits{
			{Channel: WildcardChannel, WindowHours: 24, MaxActions: 10},
			{Channel: "paid_marketing", WindowHours: 24, MaxActions: 2},
		},
	}
	if lim, ok := b.LimitsFor("paid_marketing"); !ok || lim.MaxActions != 2 {
		t.Fatalf("exact match failed: %+v ok=%v", lim, ok)
	}
	if lim, ok := b.LimitsFor("distribution"); !ok || lim.MaxActions != 10 {
		t.Fatalf("wildcard fallback failed: %+v ok=%v", lim, ok)
	}
}

func TestLimitsForNoMatchNotOK(t *testing.T) {
	b := BudgetEnvelope{
		SchemaVersion: BudgetSchemaVersion, ID: "b", CreatedAt: testNow.Format(time.RFC3339),
		Channels: []ChannelLimits{{Channel: "distribution", WindowHours: 24, MaxActions: 5}},
	}
	if _, ok := b.LimitsFor("paid_marketing"); ok {
		t.Fatal("expected not-ok for undeclared channel with no wildcard")
	}
}

func TestCheckAllowsWithHeadroom(t *testing.T) {
	home := t.TempDir()
	l := OpenLedger(home)
	b := BudgetEnvelope{
		SchemaVersion: BudgetSchemaVersion, ID: "b", CreatedAt: testNow.Format(time.RFC3339),
		Channels: []ChannelLimits{{Channel: "distribution", WindowHours: 24, MaxActions: 5, MaxSpendUSD: 10}},
	}
	h, err := l.Check(b, "product:x", "distribution", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Allowed || h.ActionsRemaining != 5 {
		t.Fatalf("expected allowed with 5 actions remaining, got %+v", h)
	}
}

func TestCheckDeniesUndeclaredChannel(t *testing.T) {
	l := OpenLedger(t.TempDir())
	b := BudgetEnvelope{
		SchemaVersion: BudgetSchemaVersion, ID: "b", CreatedAt: testNow.Format(time.RFC3339),
		Channels: []ChannelLimits{{Channel: "distribution", WindowHours: 24, MaxActions: 5}},
	}
	h, err := l.Check(b, "", "paid_marketing", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if h.Allowed {
		t.Fatal("undeclared channel must be fail-closed (not allowed)")
	}
}

func TestRecordAndCheckEnforcesActionCap(t *testing.T) {
	home := t.TempDir()
	l := OpenLedger(home)
	b := BudgetEnvelope{
		SchemaVersion: BudgetSchemaVersion, ID: "b", CreatedAt: testNow.Format(time.RFC3339),
		Channels: []ChannelLimits{{Channel: "distribution", WindowHours: 24, MaxActions: 2}},
	}
	for i := 0; i < 2; i++ {
		if err := l.Record(Consumption{At: testNow.Format(time.RFC3339), Channel: "distribution", Actions: 1}); err != nil {
			t.Fatal(err)
		}
	}
	h, err := l.Check(b, "", "distribution", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if h.Allowed {
		t.Fatalf("expected action cap to block; headroom %+v", h)
	}
}

func TestRecordAndCheckEnforcesSpendCap(t *testing.T) {
	home := t.TempDir()
	l := OpenLedger(home)
	b := BudgetEnvelope{
		SchemaVersion: BudgetSchemaVersion, ID: "b", CreatedAt: testNow.Format(time.RFC3339),
		Channels: []ChannelLimits{{Channel: "paid_marketing", WindowHours: 24, MaxSpendUSD: 50}},
	}
	if err := l.Record(Consumption{At: testNow.Format(time.RFC3339), Channel: "paid_marketing", Actions: 1, SpendUSD: 60}); err != nil {
		t.Fatal(err)
	}
	h, err := l.Check(b, "", "paid_marketing", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if h.Allowed {
		t.Fatalf("expected spend cap to block; headroom %+v", h)
	}
}

func TestRollingWindowIgnoresOldConsumption(t *testing.T) {
	home := t.TempDir()
	l := OpenLedger(home)
	b := BudgetEnvelope{
		SchemaVersion: BudgetSchemaVersion, ID: "b", CreatedAt: testNow.Format(time.RFC3339),
		Channels: []ChannelLimits{{Channel: "distribution", WindowHours: 24, MaxActions: 2}},
	}
	// Two actions 2 days ago — outside the 24h window.
	old := testNow.Add(-48 * time.Hour).Format(time.RFC3339)
	for i := 0; i < 2; i++ {
		if err := l.Record(Consumption{At: old, Channel: "distribution", Actions: 1}); err != nil {
			t.Fatal(err)
		}
	}
	h, err := l.Check(b, "", "distribution", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Allowed {
		t.Fatalf("old consumption should not count; headroom %+v", h)
	}
}

func TestProductScopingIsolatesUsage(t *testing.T) {
	home := t.TempDir()
	l := OpenLedger(home)
	b := BudgetEnvelope{
		SchemaVersion: BudgetSchemaVersion, ID: "b", CreatedAt: testNow.Format(time.RFC3339),
		Channels: []ChannelLimits{{Channel: "distribution", WindowHours: 24, MaxActions: 1}},
	}
	if err := l.Record(Consumption{At: testNow.Format(time.RFC3339), ProductID: "product:a", Channel: "distribution", Actions: 1}); err != nil {
		t.Fatal(err)
	}
	// product:b has its own budget headroom — product:a's usage doesn't bleed over.
	h, err := l.Check(b, "product:b", "distribution", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Allowed {
		t.Fatalf("product:b should have fresh headroom; headroom %+v", h)
	}
}

func TestSaveAndLoadBudgetRoundTrip(t *testing.T) {
	home := t.TempDir()
	l := OpenLedger(home)
	b := BudgetEnvelope{
		SchemaVersion: BudgetSchemaVersion, ID: "b", ProductID: "product:x", CreatedAt: testNow.Format(time.RFC3339),
		Channels: []ChannelLimits{{Channel: "distribution", WindowHours: 24, MaxActions: 7}},
	}
	if err := l.SaveBudget(b); err != nil {
		t.Fatal(err)
	}
	got, err := l.LoadBudget("product:x", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" || got.Channels[0].MaxActions != 7 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestLoadBudgetDefaultsWhenAbsent(t *testing.T) {
	l := OpenLedger(t.TempDir())
	got, err := l.LoadBudget("product:none", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != DefaultBudgetID {
		t.Fatalf("expected default budget, got %q", got.ID)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("default budget must be valid: %v", err)
	}
}

func TestProfileValidateAndSwitchbackPlan(t *testing.T) {
	p := ProjectProfile{
		SchemaVersion: ProfileSchemaVersion,
		ID:            "profile_x",
		ProductID:     "product:x",
		Slug:          "x",
		Workspace:     "/tmp/ws-x",
		Routing:       RoutingSpec{Harness: "codex", ConfigPath: "/tmp/ws-x/router.yaml", ConfigKey: "egress.lane", ConfigValue: "zai"},
		Instructions: []InstructionSurface{
			{Name: "AGENTS.md", Path: "AGENTS.md", Body: "# project x instructions"},
		},
		CreatedAt: testNow.Format(time.RFC3339),
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	steps, err := SwitchbackPlan(p, "work_1", testNow)
	if err != nil {
		t.Fatal(err)
	}
	// instructions, routing, budget (no settings declared)
	kinds := map[string]int{}
	for _, s := range steps {
		kinds[s.Kind]++
	}
	if kinds[StepInstructions] != 1 || kinds[StepRouting] != 1 || kinds[StepBudget] != 1 {
		t.Fatalf("unexpected step mix: %+v", kinds)
	}
	// routing step carries a work-scoped, local_reversible intent
	for _, s := range steps {
		if s.Kind == StepRouting {
			if s.Intent == nil || s.Intent.IDs.WorkID != "work_1" {
				t.Fatal("routing intent must be scoped to the work")
			}
			if s.Intent.MutationClass != "local_reversible" || s.Intent.Reversibility != "easy" {
				t.Fatal("routing intent must be local_reversible/easy to flow")
			}
		}
	}
}

func TestProfileRejectsEscapingInstructionPath(t *testing.T) {
	p := ProjectProfile{
		SchemaVersion: ProfileSchemaVersion, ID: "p", Slug: "x", Workspace: "/tmp/ws",
		Routing:      RoutingSpec{Harness: "codex"},
		Instructions: []InstructionSurface{{Name: "AGENTS.md", Path: "../escape.md", Body: "x"}},
		CreatedAt:    testNow.Format(time.RFC3339),
	}
	if err := p.Validate(); err == nil {
		t.Fatal("expected escaping instruction path to be rejected")
	}
}
