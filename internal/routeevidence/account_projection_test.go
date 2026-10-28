package routeevidence

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// accountAdmissionRouteChain writes a model-capabilities manifest whose build-implement
// routing_defaults is the given lane order, then imports it into home. The model set always
// defines the three gated harnesses (codex->codex, claude-code->claude, zai->zai) plus a
// switchback fallback (never gated). Tests vary only the routing_defaults chain.
func accountAdmissionRouteChain(t *testing.T, home string, chain []string) {
	t.Helper()
	manifest := ModelCapabilitiesManifest{
		Version:           1,
		Purpose:           "account admission gate test fixture",
		CapabilityClasses: []string{"build-implement"},
		Models: map[string]ModelCapabilityEntry{
			"codex/gpt": {
				Lane: "codex (subscription)", Harness: "codex", ProviderRoute: "codex",
				EffortDefault: "high", CostClass: "subscription-fixed", Tier: "premium", PacketCeiling: "multi-file",
				Capabilities: map[string]CapabilityClaim{"build-implement": {Level: "primary", Evidence: "fixture codex subscription lane"}},
			},
			"claude/code": {
				Lane: "claude-code (subscription)", Harness: "claude-code", ProviderRoute: "claude",
				EffortDefault: "high", CostClass: "subscription-fixed", Tier: "premium", PacketCeiling: "multi-file",
				Capabilities: map[string]CapabilityClaim{"build-implement": {Level: "primary", Evidence: "fixture claude-code subscription lane"}},
			},
			"zai/lane": {
				Lane: "zai (subscription)", Harness: "zai", ProviderRoute: "zai",
				EffortDefault: "medium", CostClass: "subscription-fixed", Tier: "premium", PacketCeiling: "multi-file",
				Capabilities: map[string]CapabilityClaim{"build-implement": {Level: "capable", Evidence: "fixture zai subscription lane"}},
			},
			"switchback/fallback": {
				Lane: "switchback fallback", Harness: "switchback", ProviderRoute: "auto/build",
				EffortDefault: "medium", CostClass: "api-marginal", Tier: "cheap", PacketCeiling: "multi-file",
				Capabilities: map[string]CapabilityClaim{"build-implement": {Level: "capable", Evidence: "fixture cheap fallback executor"}},
			},
		},
		RoutingDefaults: map[string][]string{"build-implement": chain},
	}
	data, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	src := filepath.Join(t.TempDir(), "model-capabilities.yaml")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if _, err := ImportModelCapabilitiesYAML(home, src, t0); err != nil {
		t.Fatalf("import manifest: %v", err)
	}
}

func laneByID(lanes []CapabilityRouteLane, id string) (CapabilityRouteLane, bool) {
	for _, lane := range lanes {
		if lane.ModelID == id {
			return lane, true
		}
	}
	return CapabilityRouteLane{}, false
}

func warningsContain(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func accountAdmissionCapacityConfig(t *testing.T, home string) LaneCapacityConfig {
	t.Helper()
	cfg := capacityFixtureConfig(t)
	cfg.Paths.AccountProjectionPath = AccountProjectionPath(home)
	return cfg
}

// (F1) valid admin-accounts@1 JSON -> projection written under temp home; identical re-import is
// idempotent (same bytes, no duplicate report side-effects); absent projection is (_, false, nil);
// bad schema is rejected.
func TestImportAccountProjectionRoundTrip(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join("..", "..", "fixtures", "route", "account-projection-source.json")

	report1, err := ImportAccountProjectionFile(home, src, t0)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if report1.SchemaVersion != AccountProjectionSchema || report1.ProviderCount != 3 || report1.SourceRevision != "42" {
		t.Fatalf("bad report: %+v", report1)
	}
	body1, err := os.ReadFile(AccountProjectionPath(home))
	if err != nil {
		t.Fatalf("read projection: %v", err)
	}

	report2, err := ImportAccountProjectionFile(home, src, t0)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	body2, err := os.ReadFile(AccountProjectionPath(home))
	if err != nil {
		t.Fatalf("read projection second time: %v", err)
	}
	if !bytes.Equal(body1, body2) {
		t.Fatalf("re-import not byte-identical:\n%s\n---\n%s", body1, body2)
	}
	if report1.ProviderCount != report2.ProviderCount || report1.SourceRevision != report2.SourceRevision {
		t.Fatalf("duplicate report side effects: %+v vs %+v", report1, report2)
	}

	projection, ok, err := LoadAccountProjection(home)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	codex := projection.Providers["codex"]
	if codex.TotalAccountCount != 2 || codex.HealthyAccountCount != 0 {
		t.Fatalf("bad codex entry (want 0/2 healthy, both walled): %+v", codex)
	}
	claude := projection.Providers["claude"]
	if claude.TotalAccountCount != 1 || claude.HealthyAccountCount != 1 {
		t.Fatalf("bad claude entry (want 1/1 healthy): %+v", claude)
	}
	zai := projection.Providers["zai"]
	if zai.TotalAccountCount != 1 || zai.HealthyAccountCount != 1 {
		t.Fatalf("bad zai entry (no capacity data -> healthy by state): %+v", zai)
	}
	if codex.SourceRevision == "" || codex.ImportedAt == "" || projection.SourceRevision == "" {
		t.Fatalf("source_revision/imported_at must be stamped: projection=%+v codex=%+v", projection, codex)
	}

	// Absent projection => (_, false, nil): no gate, never an error.
	empty := t.TempDir()
	if _, ok, err := LoadAccountProjection(empty); ok || err != nil {
		t.Fatalf("absent projection must be (_, false, nil), got ok=%v err=%v", ok, err)
	}

	// Bad schema is rejected at import.
	badSrc := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(badSrc, []byte(`{"schema":"nope","accounts":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportAccountProjectionFile(home, badSrc, t0); err == nil {
		t.Fatal("bad schema must be rejected")
	}
}

func TestAccountProjectionExcludesParkedAccountHistoryFromTotals(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(t.TempDir(), "admin-accounts.json")
	body := `{
	  "schema":"switchback/admin-accounts@1",
	  "authority_revision":97,
	  "accounts":[
	    {"id":"openai-current-a","provider":"openai","state":"enrolled"},
	    {"id":"openai-current-b","provider":"openai","state":"enrolled"},
	    {"id":"openai-old-a","provider":"openai","state":"parked"},
	    {"id":"openai-old-b","provider":"openai","state":"parked"}
	  ],
	  "capacity":[
	    {"account_id":"openai-current-a","sampled_at":"2026-07-27T18:00:00Z","windows":[]},
	    {"account_id":"openai-old-a","sampled_at":"2026-07-27T18:00:00Z","windows":[{"window_kind":"week","used":{"kind":"percent","used_percent":100}}]}
	  ]
	}`
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportAccountProjectionFile(home, src, t0); err != nil {
		t.Fatalf("import: %v", err)
	}
	projection, ok, err := LoadAccountProjection(home)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	openai := projection.Providers["openai"]
	if openai.TotalAccountCount != 2 || openai.HealthyAccountCount != 2 {
		t.Fatalf("parked account history entered current totals: %+v", openai)
	}
}

// (F2) projection with provider codex zero-healthy -> codex-harness lanes land in plan.Omitted with
// reason; other lanes (claude healthy, switchback ungated) unaffected.
func TestAccountAdmissionGateOmitsZeroHealthyProvider(t *testing.T) {
	home := t.TempDir()
	cfg := accountAdmissionCapacityConfig(t, home)
	accountAdmissionRouteChain(t, home, []string{"codex/gpt", "claude/code", "switchback/fallback"})
	src := filepath.Join("..", "..", "fixtures", "route", "account-projection-source.json")
	if _, err := ImportAccountProjectionFile(home, src, t0); err != nil {
		t.Fatalf("import: %v", err)
	}

	plan, err := PlanCapabilityRoute(home, "build-implement", CapabilityRoutePlanOptions{CapacityConfig: &cfg, Confirm: true}, t0)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, inChain := laneByID(plan.Chain, "codex/gpt"); inChain {
		t.Fatalf("zero-healthy codex must not remain in chain: %+v", plan.Chain)
	}
	omitted, ok := laneByID(plan.Omitted, "codex/gpt")
	if !ok || omitted.Capacity == nil || omitted.Capacity.State != CapacityDead || !strings.Contains(omitted.OmitReason, "state=dead") {
		t.Fatalf("bad codex omit reason: %+v", omitted)
	}
	if omitted.AccountAdmission != AccountAdmissionOmitted {
		t.Fatalf("codex lane must record omitted admission, got %q", omitted.AccountAdmission)
	}
	claudeLane, ok := laneByID(plan.Chain, "claude/code")
	if !ok || claudeLane.AccountAdmission != AccountAdmissionAdmitted {
		t.Fatalf("healthy claude must stay admitted in chain: %+v", plan.Chain)
	}
	fallback, ok := laneByID(plan.Chain, "switchback/fallback")
	if !ok || fallback.AccountAdmission != AccountAdmissionUngated {
		t.Fatalf("switchback must be ungated, got %+v", fallback)
	}
}

// (F3, superseded by the capacity veto) ALL providers zero-healthy now fail closed. The old
// account-only never-empty resurrection would dispatch a known-dead lane.
func TestCapacityGateDeadAccountsCanEmptyChain(t *testing.T) {
	home := t.TempDir()
	cfg := accountAdmissionCapacityConfig(t, home)
	accountAdmissionRouteChain(t, home, []string{"codex/gpt", "claude/code"})
	src := filepath.Join("..", "..", "fixtures", "route", "account-projection-source-all-walled.json")
	if _, err := ImportAccountProjectionFile(home, src, t0); err != nil {
		t.Fatalf("import: %v", err)
	}

	plan, err := PlanCapabilityRoute(home, "build-implement", CapabilityRoutePlanOptions{CapacityConfig: &cfg, Confirm: true}, t0)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Chain) != 0 || len(plan.Omitted) != 2 {
		t.Fatalf("dead account lanes must fail closed; chain=%+v omitted=%+v", plan.Chain, plan.Omitted)
	}
	if plan.Status != "blocked_no_lane" {
		t.Fatalf("all-dead route must block, got %q", plan.Status)
	}
	for _, modelID := range []string{"codex/gpt", "claude/code"} {
		lane, ok := laneByID(plan.Omitted, modelID)
		if !ok || lane.Capacity == nil || lane.Capacity.State != CapacityDead {
			t.Fatalf("%s was not retained as a dead omitted lane: %+v", modelID, plan.Omitted)
		}
	}
	if warningsContain(plan.Warnings, "resurrected") {
		t.Fatalf("dead lanes must not be resurrected: %+v", plan.Warnings)
	}
}

// (F4) no projection file -> plan deep-equal (and deterministic) to a run with the feature absent:
// full unfiltered chain, no Account* fields stamped, no account-gate warning. The gate is purely
// additive and only activates when a fresh projection file exists, so "no projection file" IS "the
// feature absent" for this implementation.
func TestAccountAdmissionGateFailOpenNoProjection(t *testing.T) {
	home := t.TempDir()
	cfg := accountAdmissionCapacityConfig(t, home)
	accountAdmissionRouteChain(t, home, []string{"codex/gpt", "claude/code", "switchback/fallback"})

	plan1, err := PlanCapabilityRoute(home, "build-implement", CapabilityRoutePlanOptions{CapacityConfig: &cfg, Confirm: true}, t0)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	plan2, err := PlanCapabilityRoute(home, "build-implement", CapabilityRoutePlanOptions{CapacityConfig: &cfg, Confirm: true}, t0)
	if err != nil {
		t.Fatalf("plan again: %v", err)
	}
	if !reflect.DeepEqual(plan1, plan2) {
		t.Fatalf("plan not deterministic across calls with no projection:\n%+v\nvs\n%+v", plan1, plan2)
	}
	if len(plan1.Chain) != 3 || len(plan1.Omitted) != 0 {
		t.Fatalf("no-projection plan must carry the full unfiltered chain, got chain=%+v omitted=%+v", plan1.Chain, plan1.Omitted)
	}
	for _, lane := range plan1.Chain {
		if lane.AccountAdmission != "" || lane.AccountProjectionRevision != "" {
			t.Fatalf("no-projection plan must leave Account* fields zero, got %+v", lane)
		}
	}
	for _, warning := range plan1.Warnings {
		if strings.Contains(warning, "account") {
			t.Fatalf("no-projection plan must carry no account-gate warning, got %+v", plan1.Warnings)
		}
	}
}

// (F5) any omitted/admitted (mapped-harness) lane carries a non-empty AccountProjectionRevision
// matching the import report; an ungated lane does NOT carry one.
func TestAccountAdmissionGateStampsRevisionOnGatedLanes(t *testing.T) {
	home := t.TempDir()
	cfg := accountAdmissionCapacityConfig(t, home)
	accountAdmissionRouteChain(t, home, []string{"codex/gpt", "claude/code", "zai/lane", "switchback/fallback"})
	src := filepath.Join("..", "..", "fixtures", "route", "account-projection-source.json")
	report, err := ImportAccountProjectionFile(home, src, t0)
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	plan, err := PlanCapabilityRoute(home, "build-implement", CapabilityRoutePlanOptions{CapacityConfig: &cfg, Confirm: true}, t0)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, modelID := range []string{"claude/code", "zai/lane"} {
		lane, ok := laneByID(plan.Chain, modelID)
		if !ok {
			t.Fatalf("%s must be in chain: %+v", modelID, plan.Chain)
		}
		if lane.AccountProjectionRevision == "" {
			t.Fatalf("%s must carry non-empty AccountProjectionRevision, got %+v", modelID, lane)
		}
		if lane.AccountProjectionRevision != report.SourceRevision {
			t.Fatalf("%s revision mismatch: lane=%q report=%q", modelID, lane.AccountProjectionRevision, report.SourceRevision)
		}
	}
	codexOmitted, ok := laneByID(plan.Omitted, "codex/gpt")
	if !ok || codexOmitted.AccountProjectionRevision == "" {
		t.Fatalf("omitted codex must also carry non-empty AccountProjectionRevision, got %+v", codexOmitted)
	}
	fallback, ok := laneByID(plan.Chain, "switchback/fallback")
	if !ok {
		t.Fatalf("fallback must be in chain: %+v", plan.Chain)
	}
	if fallback.AccountProjectionRevision != "" {
		t.Fatalf("ungated fallback must NOT carry a revision stamp, got %+v", fallback)
	}
}

var accountLeakEmailPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

func assertNoAccountLeak(t *testing.T, label string, body []byte) {
	t.Helper()
	text := string(body)
	if strings.Contains(text, "pa_") {
		t.Fatalf("%s leaks an account id prefix:\n%s", label, text)
	}
	if strings.Contains(text, "account_id") {
		t.Fatalf("%s leaks an account_id field:\n%s", label, text)
	}
	if accountLeakEmailPattern.MatchString(text) {
		t.Fatalf("%s leaks an email-shaped string:\n%s", label, text)
	}
}

// (F6) the fixture input CONTAINS account ids (pa_test...) and an email-shaped string
// (ray.test@example.invalid); neither the persisted projection file nor the full marshaled plan
// JSON may contain a pa_ prefix, an account_id field, or an email-shaped string.
func TestAccountAdmissionGateNoAccountLeak(t *testing.T) {
	home := t.TempDir()
	cfg := accountAdmissionCapacityConfig(t, home)
	accountAdmissionRouteChain(t, home, []string{"codex/gpt", "claude/code", "switchback/fallback"})
	src := filepath.Join("..", "..", "fixtures", "route", "account-projection-source.json")

	rawSource, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rawSource), "pa_test") || !accountLeakEmailPattern.MatchString(string(rawSource)) {
		t.Fatalf("fixture must contain a synthetic account id AND an email-shaped string to make this a real falsifier")
	}

	if _, err := ImportAccountProjectionFile(home, src, t0); err != nil {
		t.Fatalf("import: %v", err)
	}
	projectionBytes, err := os.ReadFile(AccountProjectionPath(home))
	if err != nil {
		t.Fatalf("read projection: %v", err)
	}
	assertNoAccountLeak(t, "projection file", projectionBytes)

	plan, err := PlanCapabilityRoute(home, "build-implement", CapabilityRoutePlanOptions{CapacityConfig: &cfg, Confirm: true}, t0)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	planBytes, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	assertNoAccountLeak(t, "plan JSON", planBytes)
}

// (F7) projection imported_at >6h old -> gate skipped, warning tag present, plan otherwise matches
// the fail-open shape (no Account* fields stamped on any lane).
func TestAccountAdmissionGateStaleProjectionNotGated(t *testing.T) {
	home := t.TempDir()
	cfg := accountAdmissionCapacityConfig(t, home)
	accountAdmissionRouteChain(t, home, []string{"codex/gpt", "claude/code", "switchback/fallback"})
	src := filepath.Join("..", "..", "fixtures", "route", "account-projection-source.json")
	staleImportTime := t0.Add(-7 * time.Hour)
	if _, err := ImportAccountProjectionFile(home, src, staleImportTime); err != nil {
		t.Fatalf("import: %v", err)
	}

	plan, err := PlanCapabilityRoute(home, "build-implement", CapabilityRoutePlanOptions{CapacityConfig: &cfg, Confirm: true}, t0)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, ok := laneByID(plan.Chain, "codex/gpt"); !ok {
		t.Fatalf("stale projection must not gate codex, got chain=%+v", plan.Chain)
	}
	if _, ok := laneByID(plan.Omitted, "codex/gpt"); ok {
		t.Fatalf("stale projection must not omit codex, got omitted=%+v", plan.Omitted)
	}
	if !warningsContain(plan.Warnings, "stale") {
		t.Fatalf("expected a stale-projection warning, got %+v", plan.Warnings)
	}
	for _, lane := range plan.Chain {
		if lane.AccountAdmission != "" || lane.AccountProjectionRevision != "" {
			t.Fatalf("stale-gate plan must otherwise match fail-open (no Account* fields), got %+v", lane)
		}
	}
}
