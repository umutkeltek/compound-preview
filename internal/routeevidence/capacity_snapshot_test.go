package routeevidence

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/work"
)

var capacityNow = time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)

func capacityFixtureConfig(t *testing.T) LaneCapacityConfig {
	t.Helper()
	dir := t.TempDir()
	return LaneCapacityConfig{
		Paths: LaneCapacityPaths{
			UsageHistoryPath:      filepath.Join(dir, "usage-history.jsonl"),
			QuotaCachePath:        filepath.Join(dir, "quota-cache.json"),
			AccountProjectionPath: filepath.Join(dir, "account-projection.json"),
			AliasesPath:           filepath.Join(dir, "lane-aliases.json"),
		},
		ExpectedMinutes: 60,
	}
}

// writeLaneAliasesFile writes the operator-owned lane-alias override projection
// (`{"schema_version":"compound.lane_aliases.v0.1","aliases":{...}}`) to the fixture config's
// AliasesPath.
func writeLaneAliasesFile(t *testing.T, cfg LaneCapacityConfig, aliases map[string]string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"schema_version": "compound.lane_aliases.v0.1",
		"aliases":        aliases,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Paths.AliasesPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// appendCapacityUsageRow appends a second (or later) CodexBar usage row to an existing fixture
// file, used when a test needs rows for two different provider families in one file.
func appendCapacityUsageRow(t *testing.T, cfg LaneCapacityConfig, provider string, used float64, resetsAt, sampledAt time.Time) {
	t.Helper()
	row := map[string]any{
		"provider":      provider,
		"windowKind":    "5h",
		"windowMinutes": 300,
		"usedPercent":   used,
		"resetsAt":      resetsAt.UTC().Format(time.RFC3339),
		"sampledAt":     sampledAt.UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(cfg.Paths.UsageHistoryPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(body, '\n')); err != nil {
		t.Fatal(err)
	}
}

func writeCapacityUsage(t *testing.T, cfg LaneCapacityConfig, provider string, used float64, resetsAt, sampledAt time.Time) {
	t.Helper()
	row := map[string]any{
		"provider":      provider,
		"windowKind":    "5h",
		"windowMinutes": 300,
		"usedPercent":   used,
		"resetsAt":      resetsAt.UTC().Format(time.RFC3339),
		"sampledAt":     sampledAt.UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Paths.UsageHistoryPath, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeQuotaCache(t *testing.T, cfg LaneCapacityConfig, status int, modifiedAt time.Time) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"byAccountId": map[string]any{
			"fixture-org": map[string]any{"status": status},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Paths.QuotaCachePath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(cfg.Paths.QuotaCachePath, modifiedAt, modifiedAt); err != nil {
		t.Fatal(err)
	}
}

func writeDeadAccountProjection(t *testing.T, cfg LaneCapacityConfig, lane string, observedAt time.Time) {
	t.Helper()
	projection := AccountProjection{
		SchemaVersion:  AccountProjectionSchema,
		ImportedAt:     observedAt.UTC().Format(time.RFC3339),
		SourceRevision: "fixture-dead",
		Providers: map[string]AccountProjectionEntry{
			lane: {
				Provider:            lane,
				HealthyAccountCount: 0,
				TotalAccountCount:   1,
				FreshestSampledAt:   observedAt.UTC().Format(time.RFC3339),
				SourceRevision:      "fixture-dead",
				ImportedAt:          observedAt.UTC().Format(time.RFC3339),
			},
		},
	}
	body, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Paths.AccountProjectionPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func installCapacityGuard(t *testing.T, cfg LaneCapacityConfig) {
	t.Helper()
	previous := work.LaneCapacityGuard
	work.LaneCapacityGuard = WorkLaneCapacityGuard(cfg)
	t.Cleanup(func() { work.LaneCapacityGuard = previous })
}

func readyDispatchStore(t *testing.T, home string) *work.Store {
	t.Helper()
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(work.CreateInput{
		Owner:            "fixture",
		SuccessPredicate: "capacity permits dispatch",
		DesiredOutcome:   "exercise capacity veto",
		State:            "ready",
	}, capacityNow.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	return store
}

// f1: a fresh, exhausted five-hour CodexBar row must veto the lane at the dispatch seam and
// cite both the exact row timestamp and the routing-default alternative ladder.
func TestCapacityF1DispatchPlanRefusesFreshExhaustedCodex(t *testing.T) {
	home := t.TempDir()
	cfg := capacityFixtureConfig(t)
	sampledAt := capacityNow.Add(-5 * time.Minute)
	writeCapacityUsage(t, cfg, "codex", 100, capacityNow.Add(2*time.Hour), sampledAt)
	accountAdmissionRouteChain(t, home, []string{"codex/gpt", "zai/lane", "switchback/fallback"})
	installCapacityGuard(t, cfg)

	plan, err := readyDispatchStore(t, home).DispatchPlan(capacityNow, work.DispatchPlanOptions{
		Actor:           "codex",
		CapabilityClass: "build-implement",
	})
	if err != nil {
		t.Fatalf("dispatch plan: %v", err)
	}
	if plan.Status != "capacity_refused" || plan.Selected != nil {
		t.Fatalf("fresh exhausted codex lane was offered: %+v", plan)
	}
	reason := plan.NoAction
	for _, want := range []string{"lane codex", "state=walled", sampledAt.Format(time.RFC3339), "alternative_ladder", "zai/lane"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("refusal %q does not cite %q", reason, want)
		}
	}
}

// f2: lack of source rows while a capacity source IS wired is an explicit UNKNOWN, and dispatch
// cannot offer that lane until the caller supplies --confirm. Confirmation admits the same lane
// without inventing health. (The fully-unwired home fails OPEN — see
// TestCapacityUnknownWithoutAnySourceFailsOpen.)
func TestCapacityF2UnknownZaiRequiresConfirm(t *testing.T) {
	home := t.TempDir()
	cfg := capacityFixtureConfig(t)
	writeCapacityUsage(t, cfg, "codex", 10, capacityNow.Add(2*time.Hour), capacityNow.Add(-5*time.Minute))
	installCapacityGuard(t, cfg)
	store := readyDispatchStore(t, home)

	plan, err := store.DispatchPlan(capacityNow, work.DispatchPlanOptions{Actor: "zai"})
	if err != nil {
		t.Fatalf("dispatch plan: %v", err)
	}
	if plan.Status != "capacity_confirmation_required" || plan.Selected != nil || !strings.Contains(plan.NoAction, "--confirm") {
		t.Fatalf("unknown zai lane did not require confirmation: %+v", plan)
	}
	if plan.LaneCapacity == nil || plan.LaneCapacity.State != string(CapacityUnknown) {
		t.Fatalf("unknown capacity was not annotated: %+v", plan.LaneCapacity)
	}

	confirmed, err := store.DispatchPlan(capacityNow, work.DispatchPlanOptions{Actor: "zai", Confirm: true})
	if err != nil {
		t.Fatalf("confirmed dispatch plan: %v", err)
	}
	if confirmed.Status != "ready_to_claim" || confirmed.Selected == nil || confirmed.LaneCapacity == nil || confirmed.LaneCapacity.State != string(CapacityUnknown) {
		t.Fatalf("confirmed unknown lane was not admitted with its annotation: %+v", confirmed)
	}
}

// Fail-open when UNWIRED: a home with no capacity source installed at all (fresh adopter — no
// CodexBar, no quota cache, no account projection) dispatches with a warning annotation instead
// of demanding --confirm forever. This is the account-projection fail-open precedent applied to
// the veto. Wired-but-blind (a source file exists, the lane has no rows) still requires
// confirmation — the empty-file mutation below proves the distinction has teeth.
func TestCapacityUnknownWithoutAnySourceFailsOpen(t *testing.T) {
	home := t.TempDir()
	cfg := capacityFixtureConfig(t)
	installCapacityGuard(t, cfg)

	plan, err := readyDispatchStore(t, home).DispatchPlan(capacityNow, work.DispatchPlanOptions{Actor: "zai"})
	if err != nil {
		t.Fatalf("dispatch plan: %v", err)
	}
	if plan.Status != "ready_to_claim" || plan.Selected == nil {
		t.Fatalf("unwired home did not fail open: %+v", plan)
	}
	if plan.LaneCapacity == nil || plan.LaneCapacity.State != string(CapacityUnknown) {
		t.Fatalf("unwired capacity was not annotated unknown: %+v", plan.LaneCapacity)
	}
	warned := false
	for _, w := range plan.Warnings {
		if strings.Contains(w, "no capacity source installed") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("unwired dispatch missing warning: %+v", plan.Warnings)
	}

	if err := os.WriteFile(cfg.Paths.UsageHistoryPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	blind, err := readyDispatchStore(t, t.TempDir()).DispatchPlan(capacityNow, work.DispatchPlanOptions{Actor: "zai"})
	if err != nil {
		t.Fatalf("wired-blind dispatch plan: %v", err)
	}
	if blind.Status != "capacity_confirmation_required" || blind.Selected != nil {
		t.Fatalf("wired-but-blind lane did not require confirmation: %+v", blind)
	}
}

// f3: a quota-cache 429 older than six hours is historical evidence, never a current wall.
func TestCapacityF3Stale429DoesNotRefuse(t *testing.T) {
	cfg := capacityFixtureConfig(t)
	writeQuotaCache(t, cfg, 429, capacityNow.Add(-6*time.Hour-time.Second))

	capacity, err := ReadLaneCapacityWithConfig("codex", capacityNow, cfg)
	if err != nil {
		t.Fatalf("read lane capacity: %v", err)
	}
	if capacity.State == CapacityWalled || capacity.State == CapacityDead {
		t.Fatalf("stale 429 became a live refusal: %+v", capacity)
	}
	if capacity.State != CapacityUnknown {
		t.Fatalf("stale 429 with no live usage row should leave capacity unknown: %+v", capacity)
	}
}

// f4: a hard refusal goes through the canonical execution-route-evidence ledger exactly once;
// an allowed lane writes no route-evidence row.
func TestCapacityF4RefusalRecordsExactlyOneRouteEvidenceAndAllowRecordsNone(t *testing.T) {
	t.Run("refusal", func(t *testing.T) {
		home := t.TempDir()
		cfg := capacityFixtureConfig(t)
		writeCapacityUsage(t, cfg, "codex", 100, capacityNow.Add(2*time.Hour), capacityNow.Add(-5*time.Minute))
		installCapacityGuard(t, cfg)
		store := readyDispatchStore(t, home)

		for i := 0; i < 2; i++ {
			plan, err := store.DispatchPlan(capacityNow, work.DispatchPlanOptions{Actor: "codex"})
			if err != nil {
				t.Fatalf("dispatch refusal %d: %v", i+1, err)
			}
			if plan.Status != "capacity_refused" {
				t.Fatalf("dispatch %d was not refused: %+v", i+1, plan)
			}
		}

		body, err := os.ReadFile(ExecutionRouteEvidenceLedgerPath(home))
		if err != nil {
			t.Fatalf("read route evidence: %v", err)
		}
		lines := strings.FieldsFunc(strings.TrimSpace(string(body)), func(r rune) bool { return r == '\n' })
		if len(lines) != 1 {
			t.Fatalf("refusal wrote %d rows, want exactly one:\n%s", len(lines), body)
		}
		evidence, err := Decode([]byte(lines[0]))
		if err != nil {
			t.Fatalf("refusal row is not schema-valid route evidence: %v", err)
		}
		if evidence.Status != "capacity_refused" || evidence.SelectedTarget.Provider != "codex" {
			t.Fatalf("wrong refusal evidence: %+v", evidence)
		}
	})

	t.Run("allow", func(t *testing.T) {
		home := t.TempDir()
		cfg := capacityFixtureConfig(t)
		writeCapacityUsage(t, cfg, "codex", 25, capacityNow.Add(2*time.Hour), capacityNow.Add(-5*time.Minute))
		installCapacityGuard(t, cfg)

		plan, err := readyDispatchStore(t, home).DispatchPlan(capacityNow, work.DispatchPlanOptions{Actor: "codex"})
		if err != nil {
			t.Fatalf("dispatch allow: %v", err)
		}
		if plan.Status != "ready_to_claim" || plan.Selected == nil {
			t.Fatalf("alive lane was not allowed: %+v", plan)
		}
		if _, err := os.Stat(ExecutionRouteEvidenceLedgerPath(home)); !os.IsNotExist(err) {
			t.Fatalf("allowed lane wrote route evidence: %v", err)
		}
	})
}

func TestReadLaneCapacityFresh429IsWalled(t *testing.T) {
	cfg := capacityFixtureConfig(t)
	writeQuotaCache(t, cfg, 429, capacityNow.Add(-time.Hour))

	capacity, err := ReadLaneCapacityWithConfig("codex", capacityNow, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if capacity.State != CapacityWalled || !capacity.LastProven.Equal(capacityNow.Add(-time.Hour)) {
		t.Fatalf("fresh 429 did not produce a timestamped wall: %+v", capacity)
	}
}

func TestReadLaneCapacityDeadAccountProjectionFailsClosed(t *testing.T) {
	cfg := capacityFixtureConfig(t)
	observedAt := capacityNow.Add(-10 * time.Minute)
	writeDeadAccountProjection(t, cfg, "zai", observedAt)

	capacity, err := ReadLaneCapacityWithConfig("zai", capacityNow, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if capacity.State != CapacityDead || !capacity.LastProven.Equal(observedAt) {
		t.Fatalf("dead projected account did not fail closed: %+v", capacity)
	}
}

func TestReadLaneCapacityResetInsideExpectedWindowIsExpiring(t *testing.T) {
	cfg := capacityFixtureConfig(t)
	reset := capacityNow.Add(30 * time.Minute)
	writeCapacityUsage(t, cfg, "codex", 20, reset, capacityNow.Add(-5*time.Minute))

	capacity, err := ReadLaneCapacityWithConfig("codex", capacityNow, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if capacity.State != CapacityExpiring || !capacity.ResetsAt.Equal(reset) {
		t.Fatalf("reset inside expected window was not expiring: %+v", capacity)
	}
}

func TestPlanCapabilityRouteFiltersAndAnnotatesCapacityWithoutSelecting(t *testing.T) {
	home := t.TempDir()
	cfg := capacityFixtureConfig(t)
	writeCapacityUsage(t, cfg, "codex", 100, capacityNow.Add(2*time.Hour), capacityNow.Add(-5*time.Minute))
	accountAdmissionRouteChain(t, home, []string{"codex/gpt", "zai/lane", "switchback/fallback"})

	plan, err := PlanCapabilityRoute(home, "build-implement", CapabilityRoutePlanOptions{
		CapacityConfig: &cfg,
	}, capacityNow)
	if err != nil {
		t.Fatalf("plan route: %v", err)
	}
	if _, ok := laneByID(plan.Chain, "codex/gpt"); ok {
		t.Fatalf("walled codex lane remained offerable: %+v", plan.Chain)
	}
	codex, ok := laneByID(plan.Omitted, "codex/gpt")
	if !ok || codex.Capacity == nil || codex.Capacity.State != CapacityWalled {
		t.Fatalf("walled codex lane was not retained with capacity annotation: %+v", plan.Omitted)
	}
	if !strings.Contains(codex.OmitReason, "alternative_ladder") || !strings.Contains(codex.OmitReason, "zai/lane") {
		t.Fatalf("capacity omit reason lacks routing-default ladder: %q", codex.OmitReason)
	}
	if !plan.CapacityConfirmationRequired {
		t.Fatalf("unknown zai lane must make route confirmation-required: %+v", plan)
	}
	if fallback, ok := laneByID(plan.Chain, "switchback/fallback"); !ok || fallback.Capacity != nil {
		t.Fatalf("ungated fallback should remain offerable without invented capacity: %+v", plan.Chain)
	}

	body, err := os.ReadFile(ExecutionRouteEvidenceLedgerPath(home))
	if err != nil {
		t.Fatalf("read refusal evidence: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 1 {
		t.Fatalf("one capacity-filtered lane wrote %d refusal rows: %s", len(lines), body)
	}
	if _, err := Decode([]byte(lines[0])); err != nil {
		t.Fatalf("planner refusal row invalid: %v", err)
	}
}

func TestPlanCapabilityRouteUnknownLaneRequiresAndHonorsConfirm(t *testing.T) {
	home := t.TempDir()
	cfg := capacityFixtureConfig(t)
	writeCapacityUsage(t, cfg, "codex", 10, capacityNow.Add(2*time.Hour), capacityNow.Add(-5*time.Minute))
	accountAdmissionRouteChain(t, home, []string{"zai/lane", "switchback/fallback"})

	held, err := PlanCapabilityRoute(home, "build-implement", CapabilityRoutePlanOptions{
		CapacityConfig: &cfg,
	}, capacityNow)
	if err != nil {
		t.Fatalf("plan unconfirmed: %v", err)
	}
	if !held.CapacityConfirmationRequired {
		t.Fatalf("unknown lane did not require confirmation: %+v", held)
	}
	zaiHeld, ok := laneByID(held.Omitted, "zai/lane")
	if !ok || zaiHeld.Capacity == nil || zaiHeld.Capacity.State != CapacityUnknown || !strings.Contains(zaiHeld.OmitReason, "--confirm required") {
		t.Fatalf("unknown lane was not held with evidence: %+v", held.Omitted)
	}

	confirmed, err := PlanCapabilityRoute(home, "build-implement", CapabilityRoutePlanOptions{
		CapacityConfig: &cfg,
		Confirm:        true,
	}, capacityNow)
	if err != nil {
		t.Fatalf("plan confirmed: %v", err)
	}
	zaiAllowed, ok := laneByID(confirmed.Chain, "zai/lane")
	if !ok || zaiAllowed.Capacity == nil || zaiAllowed.Capacity.State != CapacityUnknown {
		t.Fatalf("confirmed unknown lane was not admitted with annotation: %+v", confirmed)
	}
	if _, err := os.Stat(ExecutionRouteEvidenceLedgerPath(home)); !os.IsNotExist(err) {
		t.Fatalf("unknown confirmation wrote hard-refusal route evidence: %v", err)
	}
}

// --- lane-capacity-alias resolution (packet-s1-lane-capacity-alias, work_416bb6b78bb6) ---

// Structural: real dispatch lane names (sub-lane suffixes, operator-chosen prefixes) must resolve
// to their capacity family, with zai/glm taking precedence over a claude prefix, and truly unknown
// names staying unknown. Existing exact names are a regression guard.
func TestNormalizeLaneCapacityKeyStructuralInferenceAndPrecedence(t *testing.T) {
	cases := []struct {
		name string
		lane string
		want string
	}{
		{"prefix-claude", "claude-main", "claude"},
		{"prefix-claude-sub-lane", "claude-main:sub-lane", "claude"},
		{"prefix-claude-opus", "claude-opus-x", "claude"},
		{"sub-lane-codex", "codex:root-1:candidate-x", "codex"},
		{"zai-beats-claude-prefix", "claude-zai", "zai"},
		{"zai-beats-claude-prefix-sub-lane", "claude-zai:worker-2", "zai"},
		{"unknown-stays-unknown", "synthesis", ""},
		{"exact-codex", "codex", "codex"},
		{"exact-claude", "claude", "claude"},
		{"exact-claude-code", "claude-code", "claude"},
		{"exact-anthropic", "anthropic", "claude"},
		{"exact-zai", "zai", "zai"},
		{"exact-z-dot-ai", "z.ai", "zai"},
		{"exact-glm", "glm", "zai"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeLaneCapacityKey(tc.lane); got != tc.want {
				t.Fatalf("normalizeLaneCapacityKey(%q) = %q, want %q", tc.lane, got, tc.want)
			}
		})
	}
}

// Alias override: an unresolvable lane name pointed at a family via the operator-owned alias
// file resolves to that family's live capacity row.
func TestReadLaneCapacityAliasOverrideResolvesUnknownLane(t *testing.T) {
	cfg := capacityFixtureConfig(t)
	writeLaneAliasesFile(t, cfg, map[string]string{"special-lane": "zai"})
	writeCapacityUsage(t, cfg, "zai", 20, capacityNow.Add(2*time.Hour), capacityNow.Add(-5*time.Minute))

	capacity, err := ReadLaneCapacityWithConfig("special-lane", capacityNow, cfg)
	if err != nil {
		t.Fatalf("read lane capacity: %v", err)
	}
	if capacity.State == CapacityUnknown {
		t.Fatalf("alias override did not resolve special-lane to the zai row: %+v", capacity)
	}
	if !strings.Contains(capacity.DataRef, "provider=zai") {
		t.Fatalf("resolved capacity is not the zai fixture row: %+v", capacity)
	}
}

// Alias override beats prefix inference: an alias pinning "claude-main" to codex must win over
// the structural claude-prefix rule, given fresh rows for both families.
func TestReadLaneCapacityAliasOverrideBeatsPrefixInference(t *testing.T) {
	cfg := capacityFixtureConfig(t)
	writeLaneAliasesFile(t, cfg, map[string]string{"claude-main": "codex"})
	writeCapacityUsage(t, cfg, "claude", 5, capacityNow.Add(2*time.Hour), capacityNow.Add(-5*time.Minute))
	appendCapacityUsageRow(t, cfg, "codex", 40, capacityNow.Add(2*time.Hour), capacityNow.Add(-5*time.Minute))

	capacity, err := ReadLaneCapacityWithConfig("claude-main", capacityNow, cfg)
	if err != nil {
		t.Fatalf("read lane capacity: %v", err)
	}
	if !strings.Contains(capacity.DataRef, "provider=codex") {
		t.Fatalf("alias override did not beat prefix inference (want the codex row): %+v", capacity)
	}
}

// A missing alias file is a no-op: structural prefix inference still resolves the lane.
func TestReadLaneCapacityMissingAliasFileLeavesStructuralBehaviorUnchanged(t *testing.T) {
	cfg := capacityFixtureConfig(t)
	// cfg.Paths.AliasesPath is a path capacityFixtureConfig built but never wrote to.
	writeCapacityUsage(t, cfg, "claude", 5, capacityNow.Add(2*time.Hour), capacityNow.Add(-5*time.Minute))

	capacity, err := ReadLaneCapacityWithConfig("claude-main", capacityNow, cfg)
	if err != nil {
		t.Fatalf("read lane capacity: %v", err)
	}
	if !strings.Contains(capacity.DataRef, "provider=claude") {
		t.Fatalf("structural inference broke with no alias file present: %+v", capacity)
	}
}

// A malformed alias file must not be silently swallowed, but the note only surfaces on a lane
// that would otherwise be unknown — a lane the structural rules already resolve stays clean.
func TestReadLaneCapacityMalformedAliasFileSurfacesNoteOnlyWhenUnknown(t *testing.T) {
	cfg := capacityFixtureConfig(t)
	if err := os.WriteFile(cfg.Paths.AliasesPath, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	unknown, err := ReadLaneCapacityWithConfig("synthesis", capacityNow, cfg)
	if err != nil {
		t.Fatalf("read lane capacity: %v", err)
	}
	if unknown.State != CapacityUnknown {
		t.Fatalf("unknown lane with a malformed alias file should stay unknown: %+v", unknown)
	}
	if !strings.Contains(unknown.DataRef, "lane-aliases.json unreadable") {
		t.Fatalf("malformed alias file was silently swallowed: %+v", unknown)
	}

	writeCapacityUsage(t, cfg, "claude", 5, capacityNow.Add(2*time.Hour), capacityNow.Add(-5*time.Minute))
	known, err := ReadLaneCapacityWithConfig("claude-main", capacityNow, cfg)
	if err != nil {
		t.Fatalf("read lane capacity: %v", err)
	}
	if strings.Contains(known.DataRef, "lane-aliases.json unreadable") {
		t.Fatalf("alias-unreadable note leaked onto a structurally-resolved lane: %+v", known)
	}
}

// Integration: a real dispatch sub-lane name resolves to its family's live CodexBar row, and the
// family's freshness window is still enforced after resolution (untouched by this change).
func TestReadLaneCapacitySubLaneIntegrationFreshnessEnforced(t *testing.T) {
	cfg := capacityFixtureConfig(t)
	writeCapacityUsage(t, cfg, "claude", 10, capacityNow.Add(2*time.Hour), capacityNow.Add(-5*time.Minute))

	capacity, err := ReadLaneCapacityWithConfig("claude-main:sub-lane", capacityNow, cfg)
	if err != nil {
		t.Fatalf("read lane capacity: %v", err)
	}
	if capacity.State != CapacityAlive {
		t.Fatalf("sub-lane name did not resolve to the live claude row: %+v", capacity)
	}

	stale := capacityFixtureConfig(t)
	writeCapacityUsage(t, stale, "claude", 10, capacityNow.Add(2*time.Hour), capacityNow.Add(-16*time.Minute))
	staleCapacity, err := ReadLaneCapacityWithConfig("claude-main:sub-lane", capacityNow, stale)
	if err != nil {
		t.Fatalf("read stale lane capacity: %v", err)
	}
	if staleCapacity.State != CapacityUnknown {
		t.Fatalf("claude's 15m freshness window was not enforced after sub-lane resolution: %+v", staleCapacity)
	}
}

// --- S4 capacity DataRef truth split (work_54c4de85a3c7) ---
//
// Three-way split: unresolvable lane, resolved family + empty usage file, and resolved family
// + only-stale rows must surface three DISTINCT DataRefs so callers can tell WHY the lane is
// unknown. Fresh-row behavior is byte-identical to today.

func TestReadLaneCapacityDataRefTruthThreeWaySplit(t *testing.T) {
	t.Run("unresolvable lane", func(t *testing.T) {
		cfg := capacityFixtureConfig(t)
		// Create at least one source file so installed=true (we want the unresolvable-lane
		// branch of ReadLaneCapacityWithConfig, not the !installed fail-open path).
		if err := os.WriteFile(cfg.Paths.UsageHistoryPath, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		capacity, err := ReadLaneCapacityWithConfig("synthesis", capacityNow, cfg)
		if err != nil {
			t.Fatalf("read lane capacity: %v", err)
		}
		if capacity.State != CapacityUnknown {
			t.Fatalf("unresolvable lane should stay unknown: %+v", capacity)
		}
		if capacity.DataRef != "no matching capacity row" {
			t.Fatalf("unresolvable lane DataRef = %q, want %q", capacity.DataRef, "no matching capacity row")
		}
	})

	t.Run("resolved family with empty usage file", func(t *testing.T) {
		cfg := capacityFixtureConfig(t)
		// File exists but contains zero rows for the resolved family.
		if err := os.WriteFile(cfg.Paths.UsageHistoryPath, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		capacity, err := ReadLaneCapacityWithConfig("codex", capacityNow, cfg)
		if err != nil {
			t.Fatalf("read lane capacity: %v", err)
		}
		if capacity.State != CapacityUnknown {
			t.Fatalf("empty-file lane should stay unknown: %+v", capacity)
		}
		if capacity.DataRef != "no usage rows for family: codex" {
			t.Fatalf("empty-file lane DataRef = %q, want %q", capacity.DataRef, "no usage rows for family: codex")
		}
		if !capacity.LastProven.IsZero() {
			t.Fatalf("empty-file lane must not invent LastProven: %+v", capacity)
		}
		if capacity.Source != "" {
			t.Fatalf("empty-file lane must not carry source: %q", capacity.Source)
		}
	})

	t.Run("resolved family with only stale rows", func(t *testing.T) {
		cfg := capacityFixtureConfig(t)
		// Resolves structurally, file contains rows but all are stale (claude 15m window).
		writeCapacityUsage(t, cfg, "claude", 10, capacityNow.Add(2*time.Hour), capacityNow.Add(-16*time.Minute))
		capacity, err := ReadLaneCapacityWithConfig("claude-main", capacityNow, cfg)
		if err != nil {
			t.Fatalf("read lane capacity: %v", err)
		}
		if capacity.State != CapacityUnknown {
			t.Fatalf("stale-only lane should stay unknown: %+v", capacity)
		}
		if !strings.HasPrefix(capacity.DataRef, "codexbar/usage-history:") {
			t.Fatalf("stale-only DataRef must carry codexbar/usage-history prefix: %q", capacity.DataRef)
		}
		if !strings.Contains(capacity.DataRef, "newest=") {
			t.Fatalf("stale-only DataRef must carry newest= timestamp: %q", capacity.DataRef)
		}
		expected := capacityNow.Add(-16 * time.Minute).UTC().Format(time.RFC3339)
		if !strings.Contains(capacity.DataRef, expected) {
			t.Fatalf("stale-only DataRef must cite newest sample RFC3339 %q: %q", expected, capacity.DataRef)
		}
		for _, part := range []string{"family=claude", "age=16m", "freshness=15m"} {
			if !strings.Contains(capacity.DataRef, part) {
				t.Fatalf("stale-only DataRef must carry %q (predicate: family, age, freshness window named): %q", part, capacity.DataRef)
			}
		}
		if capacity.Source != "codexbar/usage-history" {
			t.Fatalf("stale-only lane Source = %q, want %q", capacity.Source, "codexbar/usage-history")
		}
		if !capacity.LastProven.Equal(capacityNow.Add(-16 * time.Minute)) {
			t.Fatalf("stale-only lane LastProven = %v, want %v", capacity.LastProven, capacityNow.Add(-16*time.Minute))
		}
	})
}

// Fresh-row behavior is byte-identical to today: a fresh row produces exactly today's DataRef
// (already covered implicitly by the live-allow tests, but made explicit here so the contract
// is self-evident at the change seam).
func TestReadLaneCapacityFreshRowDataRefUnchanged(t *testing.T) {
	cfg := capacityFixtureConfig(t)
	writeCapacityUsage(t, cfg, "codex", 25, capacityNow.Add(2*time.Hour), capacityNow.Add(-5*time.Minute))
	capacity, err := ReadLaneCapacityWithConfig("codex", capacityNow, cfg)
	if err != nil {
		t.Fatalf("read lane capacity: %v", err)
	}
	if capacity.State != CapacityAlive {
		t.Fatalf("fresh row should be alive: %+v", capacity)
	}
	want := fmt.Sprintf("codexbar:provider=codex,window=5h/300m,sampled_at=%s", capacityNow.Add(-5*time.Minute).UTC().Format(time.RFC3339))
	if capacity.DataRef != want {
		t.Fatalf("fresh row DataRef = %q, want %q", capacity.DataRef, want)
	}
}

// Malformed alias file + resolved-but-stale lane: DataRef must carry BOTH the stale annotation
// AND the lane-aliases.json unreadable note (extends S1's TestReadLaneCapacityMalformedAliasFileSurfacesNoteOnlyWhenUnknown
// pattern to the resolved-but-stale branch).
func TestReadLaneCapacityMalformedAliasPlusStaleRowsAnnotatesBoth(t *testing.T) {
	cfg := capacityFixtureConfig(t)
	if err := os.WriteFile(cfg.Paths.AliasesPath, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCapacityUsage(t, cfg, "claude", 10, capacityNow.Add(2*time.Hour), capacityNow.Add(-16*time.Minute))
	capacity, err := ReadLaneCapacityWithConfig("claude-main", capacityNow, cfg)
	if err != nil {
		t.Fatalf("read lane capacity: %v", err)
	}
	if capacity.State != CapacityUnknown {
		t.Fatalf("stale-only lane with malformed alias should stay unknown: %+v", capacity)
	}
	if !strings.Contains(capacity.DataRef, "codexbar/usage-history:") {
		t.Fatalf("stale annotation missing from DataRef: %q", capacity.DataRef)
	}
	if !strings.Contains(capacity.DataRef, "lane-aliases.json unreadable") {
		t.Fatalf("malformed-alias note did not compose with stale annotation: %q", capacity.DataRef)
	}
}

// --- exact delegated-lane capacity identities (work_819d076cb3b0) ---
//
// claude-qwen and claude-minimax are provider-backed Claude-shaped harness lanes,
// not Anthropic subscription aliases. Capacity admission must resolve them to
// qwen and minimax provider rows. Missing provider evidence stays explicit unknown
// — it must never borrow the generic claude row.

// Exact names and underscore/hyphen/suffix variants of qwen and minimax must
// resolve BEFORE the generic claude prefix. Existing zai/glm precedence (zai
// beats generic claude) and the existing sub-lane-strip retry are unchanged.
func TestNormalizeLaneCapacityKeyQwenMinimaxExactAndStructural(t *testing.T) {
	cases := []struct {
		name string
		lane string
		want string
	}{
		{"exact-qwen", "qwen", "qwen"},
		{"exact-minimax", "minimax", "minimax"},
		{"claude-qwen-hyphen", "claude-qwen", "qwen"},
		{"claude_qwen-underscore", "claude_qwen", "qwen"},
		{"claude-qwen-suffix", "claude-qwen:sub-lane", "qwen"},
		{"claude_qwen-suffix", "claude_qwen:worker-2", "qwen"},
		{"claude-minimax-hyphen", "claude-minimax", "minimax"},
		{"claude_minimax-underscore", "claude_minimax", "minimax"},
		{"claude-minimax-suffix", "claude-minimax:sub-lane", "minimax"},
		// Regression guards — these MUST NOT regress to qwen/minimax.
		{"claude-main-stays-claude", "claude-main", "claude"},
		{"claude-zai-stays-zai", "claude-zai", "zai"},
		{"claude-zai-suffix-stays-zai", "claude-zai:worker-2", "zai"},
		{"unknown-stays-unknown", "synthesis", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeLaneCapacityKey(tc.lane); got != tc.want {
				t.Fatalf("normalizeLaneCapacityKey(%q) = %q, want %q", tc.lane, got, tc.want)
			}
		})
	}
}

// Provider-row consumption: claude-qwen resolves to qwen, so a fresh qwen
// provider row must be consumed (DataRef cites provider=qwen, state=alive).
func TestReadLaneCapacityQwenRowConsumedByClaudeQwenLane(t *testing.T) {
	cfg := capacityFixtureConfig(t)
	writeCapacityUsage(t, cfg, "qwen", 25, capacityNow.Add(2*time.Hour), capacityNow.Add(-5*time.Minute))

	capacity, err := ReadLaneCapacityWithConfig("claude-qwen", capacityNow, cfg)
	if err != nil {
		t.Fatalf("read lane capacity: %v", err)
	}
	if capacity.State != CapacityAlive {
		t.Fatalf("claude-qwen did not consume fresh qwen row: %+v", capacity)
	}
	if !strings.Contains(capacity.DataRef, "provider=qwen") {
		t.Fatalf("claude-qwen DataRef did not cite qwen row: %q", capacity.DataRef)
	}
}

// claude-minimax mirrors the qwen case — a fresh minimax row is consumed.
func TestReadLaneCapacityMinimaxRowConsumedByClaudeMinimaxLane(t *testing.T) {
	cfg := capacityFixtureConfig(t)
	writeCapacityUsage(t, cfg, "minimax", 30, capacityNow.Add(2*time.Hour), capacityNow.Add(-5*time.Minute))

	capacity, err := ReadLaneCapacityWithConfig("claude-minimax", capacityNow, cfg)
	if err != nil {
		t.Fatalf("read lane capacity: %v", err)
	}
	if capacity.State != CapacityAlive {
		t.Fatalf("claude-minimax did not consume fresh minimax row: %+v", capacity)
	}
	if !strings.Contains(capacity.DataRef, "provider=minimax") {
		t.Fatalf("claude-minimax DataRef did not cite minimax row: %q", capacity.DataRef)
	}
}

func TestGuardDeclaredCapacityUsesRefProviderForLiveFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	called := ""
	source := LaneCapacitySourceRef{
		Kind: "codexbar", FreshnessMinutes: 10, Ref: "codexbar://provider/qwencloud",
	}
	decision, err := guardDeclaredLaneCapacityWithProbe(
		t.TempDir(), "qwen", "build-implement", source, capacityNow, 60, false, nil, false,
		func(provider string, now time.Time, freshness time.Duration) (CodexBarCapacityEvidence, error) {
			called = provider
			if !now.Equal(capacityNow) || freshness != 10*time.Minute {
				t.Fatalf("probe now=%s freshness=%s", now, freshness)
			}
			return CodexBarCapacityEvidence{
				Provider: provider, UpdatedAt: now,
				All: []CodexBarWindowEvidence{{Present: true, RemainingPercent: 46, WindowMinutes: 10080, ResetsAt: now.Add(4 * 24 * time.Hour)}},
			}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if called != "qwencloud" {
		t.Fatalf("capacity probe provider=%q, want qwencloud from ref (execution lane was qwen)", called)
	}
	if decision.Capacity.State != CapacityAlive || decision.Capacity.Remaining != 46 || !strings.Contains(decision.Capacity.DataRef, source.Ref) {
		t.Fatalf("declared capacity decision = %+v", decision)
	}
}

func TestGuardDeclaredCapacityFreshHistorySuppressesLiveProbe(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	cfg := DefaultLaneCapacityConfig(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(cfg.Paths.UsageHistoryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCapacityUsage(t, cfg, "minimax", 15, capacityNow.Add(4*24*time.Hour), capacityNow.Add(-9*time.Minute))
	probeCalls := 0
	decision, err := guardDeclaredLaneCapacityWithProbe(
		t.TempDir(), "minimax", "build-implement",
		LaneCapacitySourceRef{Kind: "codexbar", FreshnessMinutes: 10, Ref: "codexbar://provider/minimax"},
		capacityNow, 60, false, nil, false,
		func(string, time.Time, time.Duration) (CodexBarCapacityEvidence, error) {
			probeCalls++
			return CodexBarCapacityEvidence{}, errors.New("must not probe")
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if probeCalls != 0 || decision.Capacity.State != CapacityAlive || decision.Capacity.Remaining != 85 {
		t.Fatalf("fresh history decision=%+v probe_calls=%d", decision, probeCalls)
	}
}

func TestGuardDeclaredCapacityTupleFreshnessControlsFallback(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	cfg := DefaultLaneCapacityConfig(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(cfg.Paths.UsageHistoryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCapacityUsage(t, cfg, "minimax", 15, capacityNow.Add(4*24*time.Hour), capacityNow.Add(-11*time.Minute))
	for _, tc := range []struct {
		freshness int
		wantProbe bool
	}{
		{freshness: 10, wantProbe: true},
		{freshness: 20, wantProbe: false},
	} {
		calls := 0
		_, err := guardDeclaredLaneCapacityWithProbe(
			t.TempDir(), "minimax", "build-implement",
			LaneCapacitySourceRef{Kind: "codexbar", FreshnessMinutes: tc.freshness, Ref: "codexbar://provider/minimax"},
			capacityNow, 60, false, nil, false,
			func(provider string, now time.Time, freshness time.Duration) (CodexBarCapacityEvidence, error) {
				calls++
				return CodexBarCapacityEvidence{Provider: provider, UpdatedAt: now, All: []CodexBarWindowEvidence{{Present: true, RemainingPercent: 80, WindowMinutes: 300, ResetsAt: now.Add(2 * time.Hour)}}}, nil
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		if got := calls > 0; got != tc.wantProbe {
			t.Fatalf("freshness=%d probe=%v, want %v", tc.freshness, got, tc.wantProbe)
		}
	}
}

// Generic-claude non-borrowing: lane "claude" must never satisfy a qwen or
// minimax provider row, even when those rows are the only fresh rows in the
// file. Row filtering is keyed by structural family, so the lane stays unknown
// and the DataRef reports zero rows for the claude family.
func TestReadLaneCapacityGenericClaudeDoesNotBorrowQwenOrMinimaxRows(t *testing.T) {
	cfg := capacityFixtureConfig(t)
	writeCapacityUsage(t, cfg, "qwen", 25, capacityNow.Add(2*time.Hour), capacityNow.Add(-5*time.Minute))
	appendCapacityUsageRow(t, cfg, "minimax", 25, capacityNow.Add(2*time.Hour), capacityNow.Add(-5*time.Minute))

	capacity, err := ReadLaneCapacityWithConfig("claude", capacityNow, cfg)
	if err != nil {
		t.Fatalf("read lane capacity: %v", err)
	}
	if capacity.State != CapacityUnknown {
		t.Fatalf("generic claude must not borrow qwen/minimax row, got state=%s: %+v", capacity.State, capacity)
	}
	if !strings.Contains(capacity.DataRef, "no usage rows for family: claude") {
		t.Fatalf("generic claude DataRef should report zero rows for claude family, got %q", capacity.DataRef)
	}
}

// Mixed file: claude and qwen rows both present. Each lane resolves to its own
// structural family and consumes only the matching row; claude-minimax (no
// minimax row) stays unknown and reports zero rows for the minimax family.
func TestReadLaneCapacityMixedQwenAndClaudeRowsRespectPerLaneFamily(t *testing.T) {
	cfg := capacityFixtureConfig(t)
	writeCapacityUsage(t, cfg, "claude", 80, capacityNow.Add(2*time.Hour), capacityNow.Add(-5*time.Minute))
	appendCapacityUsageRow(t, cfg, "qwen", 20, capacityNow.Add(2*time.Hour), capacityNow.Add(-5*time.Minute))

	qwenLane, err := ReadLaneCapacityWithConfig("claude-qwen", capacityNow, cfg)
	if err != nil {
		t.Fatalf("read claude-qwen: %v", err)
	}
	if qwenLane.State != CapacityAlive {
		t.Fatalf("claude-qwen should be alive from qwen row: %+v", qwenLane)
	}
	if !strings.Contains(qwenLane.DataRef, "provider=qwen") {
		t.Fatalf("claude-qwen should consume qwen row, got DataRef=%q", qwenLane.DataRef)
	}

	claudeLane, err := ReadLaneCapacityWithConfig("claude", capacityNow, cfg)
	if err != nil {
		t.Fatalf("read claude: %v", err)
	}
	if claudeLane.State != CapacityAlive {
		t.Fatalf("claude should be alive from claude row: %+v", claudeLane)
	}
	if !strings.Contains(claudeLane.DataRef, "provider=claude") {
		t.Fatalf("claude should consume claude row, got DataRef=%q", claudeLane.DataRef)
	}

	minimaxLane, err := ReadLaneCapacityWithConfig("claude-minimax", capacityNow, cfg)
	if err != nil {
		t.Fatalf("read claude-minimax: %v", err)
	}
	if minimaxLane.State != CapacityUnknown {
		t.Fatalf("claude-minimax without a minimax row should stay unknown: %+v", minimaxLane)
	}
	if !strings.Contains(minimaxLane.DataRef, "no usage rows for family: minimax") {
		t.Fatalf("claude-minimax DataRef should report zero minimax rows, got %q", minimaxLane.DataRef)
	}
}

// Stale 15m annotation: qwen and minimax fall under the default codex 15-minute
// freshness window. Stale-only rows must produce the codexbar stale annotation
// and cite the resolved family key (qwen / minimax), not the lane alias.
func TestReadLaneCapacityQwenAndMinimaxStale15mAnnotation(t *testing.T) {
	cfg := capacityFixtureConfig(t)
	// 16m old exceeds the 15m default freshness used for non-zai/non-claude families.
	writeCapacityUsage(t, cfg, "qwen", 10, capacityNow.Add(2*time.Hour), capacityNow.Add(-16*time.Minute))

	capacity, err := ReadLaneCapacityWithConfig("claude-qwen", capacityNow, cfg)
	if err != nil {
		t.Fatalf("read lane capacity: %v", err)
	}
	if capacity.State != CapacityUnknown {
		t.Fatalf("stale qwen row should keep lane unknown: %+v", capacity)
	}
	if !strings.HasPrefix(capacity.DataRef, "codexbar/usage-history:") {
		t.Fatalf("stale qwen DataRef must carry codexbar prefix: %q", capacity.DataRef)
	}
	if !strings.Contains(capacity.DataRef, "family=qwen") {
		t.Fatalf("stale qwen DataRef must cite family=qwen: %q", capacity.DataRef)
	}
	expected := capacityNow.Add(-16 * time.Minute).UTC().Format(time.RFC3339)
	if !strings.Contains(capacity.DataRef, expected) {
		t.Fatalf("stale qwen DataRef must cite newest RFC3339 %q: %q", expected, capacity.DataRef)
	}

	staleMinimax := capacityFixtureConfig(t)
	writeCapacityUsage(t, staleMinimax, "minimax", 10, capacityNow.Add(2*time.Hour), capacityNow.Add(-16*time.Minute))

	minimaxCapacity, err := ReadLaneCapacityWithConfig("claude-minimax", capacityNow, staleMinimax)
	if err != nil {
		t.Fatalf("read minimax lane capacity: %v", err)
	}
	if minimaxCapacity.State != CapacityUnknown {
		t.Fatalf("stale minimax row should keep lane unknown: %+v", minimaxCapacity)
	}
	if !strings.Contains(minimaxCapacity.DataRef, "family=minimax") {
		t.Fatalf("stale minimax DataRef must cite family=minimax: %q", minimaxCapacity.DataRef)
	}
}

// claude-qwen and claude-minimax must NOT borrow a generic claude row when
// only one exists in the file. Symmetric to the generic-claude non-borrowing
// case: every delegated-lane family reads only its own provider rows. The
// DataRef for each must cite ZERO rows for the lane's OWN family
// (qwen / minimax), not the generic claude family the file actually contains.
func TestReadLaneCapacityClaudeQwenAndMinimaxDoNotBorrowGenericClaudeRow(t *testing.T) {
	cases := []struct {
		name   string
		lane   string
		family string
	}{
		{"claude-qwen-borrows-only-qwen", "claude-qwen", "qwen"},
		{"claude-minimax-borrows-only-minimax", "claude-minimax", "minimax"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := capacityFixtureConfig(t)
			// ONLY a fresh generic claude row exists — neither delegated lane may consume it.
			writeCapacityUsage(t, cfg, "claude", 25, capacityNow.Add(2*time.Hour), capacityNow.Add(-5*time.Minute))

			capacity, err := ReadLaneCapacityWithConfig(tc.lane, capacityNow, cfg)
			if err != nil {
				t.Fatalf("read lane capacity: %v", err)
			}
			if capacity.State != CapacityUnknown {
				t.Fatalf("%s must not borrow generic claude row, got state=%s: %+v", tc.lane, capacity.State, capacity)
			}
			want := "no usage rows for family: " + tc.family
			if !strings.Contains(capacity.DataRef, want) {
				t.Fatalf("%s DataRef must cite zero rows for own family %q, got %q", tc.lane, tc.family, capacity.DataRef)
			}
		})
	}
}

// Stale-only DataRefs for qwen and minimax must LITERALLY cite freshness=15m.
// Both delegated lanes fall under the default codex 15-minute freshness window
// (neither has its own override), so the readCodexBarCapacity stale annotation
// must surface the same constant the byte-level format string is built from.
func TestReadLaneCapacityQwenAndMinimaxStaleFreshnessIs15m(t *testing.T) {
	cases := []struct {
		name     string
		lane     string
		provider string
	}{
		{"qwen-freshness-15m", "claude-qwen", "qwen"},
		{"minimax-freshness-15m", "claude-minimax", "minimax"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := capacityFixtureConfig(t)
			// 16m old exceeds the 15m default freshness window.
			writeCapacityUsage(t, cfg, tc.provider, 10, capacityNow.Add(2*time.Hour), capacityNow.Add(-16*time.Minute))

			capacity, err := ReadLaneCapacityWithConfig(tc.lane, capacityNow, cfg)
			if err != nil {
				t.Fatalf("read lane capacity: %v", err)
			}
			if capacity.State != CapacityUnknown {
				t.Fatalf("stale %s row should keep lane unknown: %+v", tc.provider, capacity)
			}
			if !strings.HasPrefix(capacity.DataRef, "codexbar/usage-history:") {
				t.Fatalf("stale %s DataRef must carry codexbar prefix: %q", tc.provider, capacity.DataRef)
			}
			if !strings.Contains(capacity.DataRef, "freshness=15m") {
				t.Fatalf("stale %s DataRef must literally contain freshness=15m: %q", tc.provider, capacity.DataRef)
			}
		})
	}
}

// --- expired usage windows never constrain (work_r6n_capacity_expired_window_20260911) ---
//
// Right after a provider's usage window resets, the 30-minute recent cluster holds BOTH the
// old window's last row (higher usage, older sample) and the new window's fresh rows. The old
// window's usage is dead — it must never win "highest UsedPercent" and pin LastProven to its
// aging sample while fresh rows keep arriving (observed live: the minimax primary window reset
// at 2026-09-11T05:00Z left the lane capacity_unknown for the whole cluster span).

// capacityWindowRow is one usage row with full window control, for tests that need several
// concurrent window groups (primary + secondary) or windows that already expired — the plain
// writeCapacityUsage helper only writes a single "5h"/300m row shape.
type capacityWindowRow struct {
	windowKind    string
	windowMinutes int
	used          float64
	resetsAt      time.Time
	sampledAt     time.Time
}

func writeCapacityWindowRows(t *testing.T, cfg LaneCapacityConfig, provider string, rows ...capacityWindowRow) {
	t.Helper()
	var body []byte
	for _, row := range rows {
		line, err := json.Marshal(map[string]any{
			"provider":      provider,
			"windowKind":    row.windowKind,
			"windowMinutes": row.windowMinutes,
			"usedPercent":   row.used,
			"resetsAt":      row.resetsAt.UTC().Format(time.RFC3339),
			"sampledAt":     row.sampledAt.UTC().Format(time.RFC3339),
		})
		if err != nil {
			t.Fatal(err)
		}
		body = append(body, line...)
		body = append(body, '\n')
	}
	if err := os.WriteFile(cfg.Paths.UsageHistoryPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The incident shape: old primary row (used 20, reset 10m in the past, sampled 13m ago)
// coexists in the recent cluster with a fresh primary row (used 0, new window) and a live
// weekly secondary row (used 11). The dead old window must not constrain: the secondary row
// does (remaining 89), LastProven is ITS sample (1m ago), and the lane is alive.
func TestExpiredWindowGroupIsNeverConstraining(t *testing.T) {
	cfg := capacityFixtureConfig(t)
	writeCapacityWindowRows(t, cfg, "minimax",
		capacityWindowRow{windowKind: "primary", windowMinutes: 300, used: 20, resetsAt: capacityNow.Add(-10 * time.Minute), sampledAt: capacityNow.Add(-13 * time.Minute)},
		capacityWindowRow{windowKind: "primary", windowMinutes: 300, used: 0, resetsAt: capacityNow.Add(5 * time.Hour), sampledAt: capacityNow.Add(-1 * time.Minute)},
		capacityWindowRow{windowKind: "secondary", windowMinutes: 10080, used: 11, resetsAt: capacityNow.Add(72 * time.Hour), sampledAt: capacityNow.Add(-1 * time.Minute)},
	)

	capacity, err := ReadLaneCapacityWithConfig("claude-minimax", capacityNow, cfg)
	if err != nil {
		t.Fatalf("read lane capacity: %v", err)
	}
	if capacity.State != CapacityAlive {
		t.Fatalf("lane must read alive from the new window's rows, got state=%s: %+v", capacity.State, capacity)
	}
	if capacity.Remaining != 89 {
		t.Fatalf("remaining = %v, want 89 (constrained by the live secondary row, used 11): %+v", capacity.Remaining, capacity)
	}
	if want := capacityNow.Add(-1 * time.Minute); !capacity.LastProven.Equal(want) {
		t.Fatalf("LastProven = %v, want the constraining row's sample %v", capacity.LastProven, want)
	}
	if !strings.Contains(capacity.DataRef, "window=secondary/10080m") {
		t.Fatalf("DataRef must cite the live secondary window as constraining: %q", capacity.DataRef)
	}
}

// Only expired groups: keep the pre-fix posture (unknown, no invented reset), but the DataRef
// must say that is WHY — every window group in the recent cluster is dead.
func TestAllWindowsExpiredStaysUnknown(t *testing.T) {
	cfg := capacityFixtureConfig(t)
	writeCapacityWindowRows(t, cfg, "minimax",
		capacityWindowRow{windowKind: "primary", windowMinutes: 300, used: 20, resetsAt: capacityNow.Add(-10 * time.Minute), sampledAt: capacityNow.Add(-5 * time.Minute)},
		capacityWindowRow{windowKind: "secondary", windowMinutes: 10080, used: 11, resetsAt: capacityNow.Add(-2 * time.Hour), sampledAt: capacityNow.Add(-5 * time.Minute)},
	)

	capacity, err := ReadLaneCapacityWithConfig("claude-minimax", capacityNow, cfg)
	if err != nil {
		t.Fatalf("read lane capacity: %v", err)
	}
	if capacity.State != CapacityUnknown {
		t.Fatalf("all-expired windows must stay unknown, got state=%s: %+v", capacity.State, capacity)
	}
	if !strings.Contains(capacity.DataRef, "all_windows_expired") {
		t.Fatalf("all-expired DataRef must say so: %q", capacity.DataRef)
	}
	if !capacity.ResetsAt.IsZero() {
		t.Fatalf("an expired window must not report a future reset: %+v", capacity)
	}
}

// Refusal evidence recorded for a claude-qwen lane must carry the structural
// provider identity (qwen) — not the lane alias (claude-qwen). Both Provider
// and Model on the refusal row follow the resolved family key.
func TestRecordCapacityRefusalQwenLaneProviderIdentity(t *testing.T) {
	home := t.TempDir()
	cfg := capacityFixtureConfig(t)
	// 100% used → CapacityWalled → refusal recorded with the resolved family key.
	writeCapacityUsage(t, cfg, "qwen", 100, capacityNow.Add(2*time.Hour), capacityNow.Add(-5*time.Minute))
	installCapacityGuard(t, cfg)

	plan, err := readyDispatchStore(t, home).DispatchPlan(capacityNow, work.DispatchPlanOptions{
		Actor: "claude-qwen",
	})
	if err != nil {
		t.Fatalf("dispatch plan: %v", err)
	}
	if plan.Status != "capacity_refused" {
		t.Fatalf("walled qwen lane did not refuse: %+v", plan)
	}

	body, err := os.ReadFile(ExecutionRouteEvidenceLedgerPath(home))
	if err != nil {
		t.Fatalf("read route evidence: %v", err)
	}
	lines := strings.FieldsFunc(strings.TrimSpace(string(body)), func(r rune) bool { return r == '\n' })
	if len(lines) != 1 {
		t.Fatalf("refusal wrote %d rows, want exactly one:\n%s", len(lines), body)
	}
	evidence, err := Decode([]byte(lines[0]))
	if err != nil {
		t.Fatalf("refusal row not schema-valid route evidence: %v", err)
	}
	if evidence.Status != "capacity_refused" {
		t.Fatalf("wrong refusal status: %+v", evidence)
	}
	if evidence.SelectedTarget.Provider != "qwen" {
		t.Fatalf("refusal evidence Provider = %q, want %q (claude-qwen resolves to qwen family)",
			evidence.SelectedTarget.Provider, "qwen")
	}
	if evidence.SelectedTarget.Model != "qwen" {
		t.Fatalf("refusal evidence Model = %q, want %q", evidence.SelectedTarget.Model, "qwen")
	}
}
