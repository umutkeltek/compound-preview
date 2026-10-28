package routeevidence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMeasuredLatencyClassAgainstDefaultBudget(t *testing.T) {
	budget := DefaultLatencyBudget()
	for _, tc := range []struct {
		name string
		tps  float64
		ttft int64
		want LatencyClass
	}{
		{"fast-first-token", 40, 300, LatencyInteractive},
		{"slow-throughput-batch", 5, 40000, LatencyBatch},
		{"high-ttft-fails-interactive", 40, 9000, LatencyBatch},
		{"below-batch-floor-overnight", 1.5, 1000, LatencyOvernight},
		{"huge-ttft-overnight", 40, 120000, LatencyOvernight},
		{"dead-lane-overnight", 0, 200, LatencyOvernight},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MeasuredLatencyClass(tc.tps, tc.ttft, budget); got != tc.want {
				t.Fatalf("MeasuredLatencyClass(%v,%v)=%s want %s", tc.tps, tc.ttft, got, tc.want)
			}
		})
	}
}

func TestDeclaredLatencyClassFromTier(t *testing.T) {
	if got := DeclaredLatencyClass("local-batch"); got != LatencyBatch {
		t.Fatalf("local-batch declared %s, want batch", got)
	}
	for _, tier := range []string{"free", "cheap", "premium", "browser"} {
		if got := DeclaredLatencyClass(tier); got != LatencyInteractive {
			t.Fatalf("tier %q declared %s, want interactive", tier, got)
		}
	}
}

func TestLaneSpeedProbeIngestRoundTripAndDedup(t *testing.T) {
	home := t.TempDir()
	raw := []byte(`{
	  "harness": "switchback",
	  "route": "auto/extract",
	  "model": "auto/extract",
	  "effort": "low",
	  "budget": "single-file",
	  "lane_ref": "auto/extract",
	  "tokens_per_sec": 5.0,
	  "ttft_ms": 40000,
	  "measured_at": "2026-06-29T11:00:00Z",
	  "source": "probe-orchestrator"
	}`)
	probe, wrote, err := IngestLaneSpeedProbe(home, raw, t0)
	if err != nil || !wrote {
		t.Fatalf("ingest probe: wrote=%v err=%v", wrote, err)
	}
	if probe.SchemaVersion != LaneSpeedProbeSchemaVersion || probe.ID == "" || probe.LaneTupleID == "" {
		t.Fatalf("probe not normalized: %+v", probe)
	}
	if probe.LaneTupleID != LaneSpeedTupleID("switchback", "auto/extract", "auto/extract", "low") {
		t.Fatalf("tuple id = %q", probe.LaneTupleID)
	}

	// Re-ingesting the identical probe is an idempotent no-op.
	_, wrote2, err := IngestLaneSpeedProbe(home, raw, t0)
	if err != nil || wrote2 {
		t.Fatalf("re-ingest should be no-op: wrote=%v err=%v", wrote2, err)
	}
	all, err := LoadLaneSpeedProbes(home)
	if err != nil || len(all) != 1 {
		t.Fatalf("ledger rows = %d err=%v", len(all), err)
	}

	// A fresher probe for the same tuple wins "latest".
	newer := []byte(`{"harness":"switchback","route":"auto/extract","model":"auto/extract","effort":"low","tokens_per_sec":30,"ttft_ms":400,"measured_at":"2026-06-29T13:00:00Z","source":"probe-orchestrator"}`)
	if _, wrote, err := IngestLaneSpeedProbe(home, newer, t0); err != nil || !wrote {
		t.Fatalf("ingest newer: wrote=%v err=%v", wrote, err)
	}
	latest, err := LatestLaneSpeedByTuple(home)
	if err != nil {
		t.Fatal(err)
	}
	got := latest[LaneSpeedTupleID("switchback", "auto/extract", "auto/extract", "low")]
	if got.TokensPerSec != 30 || got.TTFTms != 400 {
		t.Fatalf("latest probe = %+v, want the 13:00 sample", got)
	}
}

func TestIngestLaneSpeedProbeRejectsUnknownAndInvalid(t *testing.T) {
	home := t.TempDir()
	if _, _, err := IngestLaneSpeedProbe(home, []byte(`{"harness":"x","tokens_per_sec":5,"ttft_ms":10,"provider_api_key":"leak"}`), t0); err == nil {
		t.Fatal("unknown field must be rejected (no provider blobs)")
	}
	if _, _, err := IngestLaneSpeedProbe(home, []byte(`{"harness":"x","tokens_per_sec":-1,"ttft_ms":10}`), t0); err == nil {
		t.Fatal("negative throughput must be rejected")
	}
	if _, _, err := IngestLaneSpeedProbe(home, []byte(`{"tokens_per_sec":5,"ttft_ms":10}`), t0); err == nil {
		t.Fatal("probe with no lane identity must be rejected")
	}
}

func TestLoadLatencyBudgetOverride(t *testing.T) {
	home := t.TempDir()
	budget, source, err := LoadLatencyBudget(home)
	if err != nil {
		t.Fatal(err)
	}
	if source != "default_operator_sla" || budget != DefaultLatencyBudget() {
		t.Fatalf("missing file should default: source=%s budget=%+v", source, budget)
	}

	override := LatencyBudget{
		Interactive: LatencyThreshold{MinTokensPerSec: 25, MaxTTFTms: 1000},
		Batch:       LatencyThreshold{MinTokensPerSec: 3, MaxTTFTms: 30000},
	}
	if err := os.MkdirAll(filepath.Dir(LatencyBudgetPath(home)), 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(override)
	if err := os.WriteFile(LatencyBudgetPath(home), data, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, source, err := LoadLatencyBudget(home)
	if err != nil {
		t.Fatal(err)
	}
	if loaded != override || source == "default_operator_sla" {
		t.Fatalf("override not loaded: source=%s budget=%+v", source, loaded)
	}
	// The override changes classification: 25 tok/s now fails the interactive floor.
	if got := MeasuredLatencyClass(20, 500, loaded); got != LatencyBatch {
		t.Fatalf("override interactive floor not applied: %s", got)
	}
}
