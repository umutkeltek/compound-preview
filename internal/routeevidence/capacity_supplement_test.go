package routeevidence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- work_9928ccb3621b: compound-owned usage-supplement capacity source ---
//
// An external launchd sampler appends claude/zai capacity rows into CodexBar.app's own
// usage-history.jsonl. Since CodexBar started rewriting/pruning that file (2026-07-23), a
// claude-family row landing there can be silently dropped, freezing every claude lane on
// capacity_confirmation_required. These falsifiers prove the reader now merges Compound's OWN
// supplement feed (<COMPOUND_HOME>/projections/usage-supplement.jsonl) at row level BEFORE any
// freshness/cluster logic runs, so a healthy supplement row survives a stale or pruned CodexBar
// file, and prove the non-fatal capacity.blackout:<family> doctor watchdog.

// supplementFixtureConfig mirrors capacity_snapshot_test.go's capacityFixtureConfig, plus an
// explicit SupplementUsagePath fixture file, so these tests never depend on $HOME/CodexBar's real
// on-disk location.
func supplementFixtureConfig(t *testing.T) LaneCapacityConfig {
	t.Helper()
	dir := t.TempDir()
	return LaneCapacityConfig{
		Paths: LaneCapacityPaths{
			UsageHistoryPath:      filepath.Join(dir, "usage-history.jsonl"),
			QuotaCachePath:        filepath.Join(dir, "quota-cache.json"),
			AccountProjectionPath: filepath.Join(dir, "account-projection.json"),
			AliasesPath:           filepath.Join(dir, "lane-aliases.json"),
			SupplementUsagePath:   filepath.Join(dir, "usage-supplement.jsonl"),
		},
		ExpectedMinutes: 60,
	}
}

func writeUsageRowFile(t *testing.T, path, provider string, used float64, resetsAt, sampledAt time.Time) {
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
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeCodexbarRow(t *testing.T, cfg LaneCapacityConfig, provider string, used float64, resetsAt, sampledAt time.Time) {
	t.Helper()
	writeUsageRowFile(t, cfg.Paths.UsageHistoryPath, provider, used, resetsAt, sampledAt)
}

func writeSupplementRow(t *testing.T, cfg LaneCapacityConfig, provider string, used float64, resetsAt, sampledAt time.Time) {
	t.Helper()
	writeUsageRowFile(t, cfg.Paths.SupplementUsagePath, provider, used, resetsAt, sampledAt)
}

// F1: a fresh supplement row must win over a stale (26h old) CodexBar row for the same family —
// alive, and Source names the supplement feed, not CodexBar's file.
func TestLaneCapacitySupplementFreshBeatsCodexbarStale(t *testing.T) {
	cfg := supplementFixtureConfig(t)
	writeCodexbarRow(t, cfg, "claude", 10, capacityNow.Add(5*time.Hour), capacityNow.Add(-26*time.Hour))
	writeSupplementRow(t, cfg, "claude", 20, capacityNow.Add(5*time.Hour), capacityNow.Add(-2*time.Minute))

	capacity, err := ReadLaneCapacityWithConfig("claude", capacityNow, cfg)
	if err != nil {
		t.Fatalf("read lane capacity: %v", err)
	}
	if capacity.State != CapacityAlive {
		t.Fatalf("state = %s, want alive: %+v", capacity.State, capacity)
	}
	if capacity.Source != "compound/usage-supplement" {
		t.Fatalf("source = %q, want compound/usage-supplement: %+v", capacity.Source, capacity)
	}
	if capacity.Remaining != 80 {
		t.Fatalf("remaining = %v, want 80 (100-20 from the supplement row)", capacity.Remaining)
	}
}

// F2 (+F5 folded in): CodexBar's path points at a file that was never created; the supplement
// feed alone must still resolve the lane to alive, and SourceInstalled must be true because the
// supplement file itself exists on disk.
func TestLaneCapacitySupplementOnly(t *testing.T) {
	cfg := supplementFixtureConfig(t)
	cfg.Paths.UsageHistoryPath = filepath.Join(t.TempDir(), "does-not-exist.jsonl")
	writeSupplementRow(t, cfg, "claude", 10, capacityNow.Add(5*time.Hour), capacityNow.Add(-2*time.Minute))

	capacity, err := ReadLaneCapacityWithConfig("claude", capacityNow, cfg)
	if err != nil {
		t.Fatalf("read lane capacity: %v", err)
	}
	if capacity.State != CapacityAlive {
		t.Fatalf("state = %s, want alive: %+v", capacity.State, capacity)
	}
	if !capacity.SourceInstalled {
		t.Fatalf("source_installed = false, want true (supplement file exists): %+v", capacity)
	}
	if capacity.Source != "compound/usage-supplement" {
		t.Fatalf("source = %q, want compound/usage-supplement: %+v", capacity.Source, capacity)
	}
}

// F3: fresh rows in both sources, sharing one reset/window group so the "recent cluster" merge
// picks the more recently SAMPLED row within the group — CodexBar's own row (1m old) must beat
// the supplement row (10m old) — proving the merge honors recency across sources, not a fixed
// source preference.
func TestLaneCapacityNewestWinsAcrossSources(t *testing.T) {
	cfg := supplementFixtureConfig(t)
	resetsAt := capacityNow.Add(5 * time.Hour)
	writeCodexbarRow(t, cfg, "claude", 30, resetsAt, capacityNow.Add(-1*time.Minute))
	writeSupplementRow(t, cfg, "claude", 50, resetsAt, capacityNow.Add(-10*time.Minute))

	capacity, err := ReadLaneCapacityWithConfig("claude", capacityNow, cfg)
	if err != nil {
		t.Fatalf("read lane capacity: %v", err)
	}
	if capacity.State != CapacityAlive {
		t.Fatalf("state = %s, want alive: %+v", capacity.State, capacity)
	}
	if capacity.Source != "codexbar/usage-history" {
		t.Fatalf("source = %q, want codexbar/usage-history (newer sample): %+v", capacity.Source, capacity)
	}
	if capacity.Remaining != 70 {
		t.Fatalf("remaining = %v, want 70 (100-30 from the newer codexbar row)", capacity.Remaining)
	}
}

// F4: both sources stale (merged newest is the supplement's -18m row, still older than the 15m
// claude freshness window) must stay fail-closed: CapacityUnknown, and GuardLaneCapacity without
// --confirm must require confirmation. DataRef must carry the MERGED newest, not just CodexBar's.
func TestLaneCapacityBothStaleStaysFailClosed(t *testing.T) {
	cfg := supplementFixtureConfig(t)
	writeCodexbarRow(t, cfg, "claude", 10, capacityNow.Add(5*time.Hour), capacityNow.Add(-20*time.Minute))
	writeSupplementRow(t, cfg, "claude", 15, capacityNow.Add(5*time.Hour), capacityNow.Add(-18*time.Minute))

	capacity, err := ReadLaneCapacityWithConfig("claude", capacityNow, cfg)
	if err != nil {
		t.Fatalf("read lane capacity: %v", err)
	}
	if capacity.State != CapacityUnknown {
		t.Fatalf("state = %s, want unknown (both stale): %+v", capacity.State, capacity)
	}
	wantNewest := capacityNow.Add(-18 * time.Minute).UTC().Format(time.RFC3339)
	if !strings.Contains(capacity.DataRef, "newest="+wantNewest) {
		t.Fatalf("data_ref must cite the MERGED newest (-18m, the supplement row), got %q", capacity.DataRef)
	}
	if capacity.Source != "compound/usage-supplement" {
		t.Fatalf("source = %q, want compound/usage-supplement (merged-newest row): %+v", capacity.Source, capacity)
	}

	decision, err := GuardLaneCapacity("", "claude", "", capacityNow, false, nil, cfg)
	if err != nil {
		t.Fatalf("guard lane capacity: %v", err)
	}
	if !decision.ConfirmationRequired {
		t.Fatalf("decision = %+v, want confirmation_required=true", decision)
	}
	if !strings.Contains(decision.Capacity.DataRef, "newest="+wantNewest) {
		t.Fatalf("guard decision data_ref must cite merged newest, got %q", decision.Capacity.DataRef)
	}
}

// F6: a declared codexbar source ref whose CodexBar file is stale, but whose supplement feed is
// fresh, must resolve WITHOUT a live probe — the probe function fails the test if it is ever
// invoked, proving the merged file read is exhausted before falling back to a live CodexBar CLI
// probe.
func TestGuardDeclaredLaneCapacityReadsSupplement(t *testing.T) {
	cfg := supplementFixtureConfig(t)
	writeCodexbarRow(t, cfg, "claude", 40, capacityNow.Add(5*time.Hour), capacityNow.Add(-20*time.Minute))
	writeSupplementRow(t, cfg, "claude", 5, capacityNow.Add(5*time.Hour), capacityNow.Add(-2*time.Minute))

	source := LaneCapacitySourceRef{Kind: "codexbar", FreshnessMinutes: 15, Ref: "codexbar://provider/claude"}
	probeCalled := false
	decision, err := guardDeclaredLaneCapacityWithConfigAndProbe(
		"", "claude", "", source, capacityNow, 60, false, nil, false, cfg,
		func(provider string, now time.Time, freshness time.Duration) (CodexBarCapacityEvidence, error) {
			probeCalled = true
			t.Errorf("live probe must not be called when the merged files have a fresh row")
			return CodexBarCapacityEvidence{}, nil
		},
	)
	if err != nil {
		t.Fatalf("guard declared lane capacity: %v", err)
	}
	if probeCalled {
		t.Fatal("live probe was invoked")
	}
	if decision.Capacity.State != CapacityAlive {
		t.Fatalf("state = %s, want alive: %+v", decision.Capacity.State, decision)
	}
	if decision.Capacity.Remaining != 95 {
		t.Fatalf("remaining = %v, want 95 (100-5 from the fresh supplement row)", decision.Capacity.Remaining)
	}
}

// F7: the non-fatal capacity.blackout:<family> doctor watchdog — a family with rows of ANY age
// warns once its merged-newest sample crosses 4x its own freshness window; a family with a
// fresh-enough row, or with zero rows ever, produces no item; the boundary sits at EXACTLY 4x
// (inclusive of the boundary itself staying silent).
func TestCapacityFeedWarnings(t *testing.T) {
	hasWarning := func(warnings []CapacityFeedWarning, name string) (CapacityFeedWarning, bool) {
		for _, w := range warnings {
			if w.Name == name {
				return w, true
			}
		}
		return CapacityFeedWarning{}, false
	}

	t.Run("stale-enrolled family warns", func(t *testing.T) {
		cfg := supplementFixtureConfig(t)
		// claude freshness window is 15m; 75m = 5x, comfortably past the 4x=60m threshold.
		writeCodexbarRow(t, cfg, "claude", 10, capacityNow.Add(5*time.Hour), capacityNow.Add(-75*time.Minute))

		warnings := CapacityFeedWarnings(cfg, capacityNow)
		w, ok := hasWarning(warnings, "capacity.blackout:claude")
		if !ok {
			t.Fatalf("want capacity.blackout:claude, got %+v", warnings)
		}
		if w.OK {
			t.Fatalf("blackout item must report ok=false: %+v", w)
		}
		if !strings.Contains(w.Detail, "age=75m") || !strings.Contains(w.Detail, "threshold=60m") {
			t.Fatalf("blackout detail must cite age and threshold: %q", w.Detail)
		}
	})

	t.Run("fresh family does not warn", func(t *testing.T) {
		cfg := supplementFixtureConfig(t)
		writeCodexbarRow(t, cfg, "codex", 10, capacityNow.Add(5*time.Hour), capacityNow.Add(-5*time.Minute))

		warnings := CapacityFeedWarnings(cfg, capacityNow)
		if _, ok := hasWarning(warnings, "capacity.blackout:codex"); ok {
			t.Fatalf("fresh family must not warn, got %+v", warnings)
		}
	})

	t.Run("never-enrolled family does not warn", func(t *testing.T) {
		cfg := supplementFixtureConfig(t)
		writeCodexbarRow(t, cfg, "codex", 10, capacityNow.Add(5*time.Hour), capacityNow.Add(-5*time.Minute))
		// zai never appears in any source file.

		warnings := CapacityFeedWarnings(cfg, capacityNow)
		if _, ok := hasWarning(warnings, "capacity.blackout:zai"); ok {
			t.Fatalf("never-enrolled family must not warn, got %+v", warnings)
		}
	})

	t.Run("boundary at exactly 4x window stays silent, one tick past fires", func(t *testing.T) {
		atBoundary := supplementFixtureConfig(t)
		writeCodexbarRow(t, atBoundary, "claude", 10, capacityNow.Add(5*time.Hour), capacityNow.Add(-60*time.Minute))
		warnings := CapacityFeedWarnings(atBoundary, capacityNow)
		if _, ok := hasWarning(warnings, "capacity.blackout:claude"); ok {
			t.Fatalf("exactly-4x age must stay silent (boundary is inclusive-safe), got %+v", warnings)
		}

		pastBoundary := supplementFixtureConfig(t)
		writeCodexbarRow(t, pastBoundary, "claude", 10, capacityNow.Add(5*time.Hour), capacityNow.Add(-61*time.Minute))
		warnings = CapacityFeedWarnings(pastBoundary, capacityNow)
		if _, ok := hasWarning(warnings, "capacity.blackout:claude"); !ok {
			t.Fatalf("one tick past 4x must fire, got %+v", warnings)
		}
	})
}
