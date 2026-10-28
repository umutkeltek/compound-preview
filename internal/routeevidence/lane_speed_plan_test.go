package routeevidence

import (
	"path/filepath"
	"strings"
	"testing"
)

func importSpeedFixture(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	source := filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml")
	if _, err := ImportModelCapabilitiesYAML(home, source, t0); err != nil {
		t.Fatalf("import model capabilities: %v", err)
	}
	return home
}

// probe of the fixture's auto/extract lane (harness switchback, provider_route auto/extract,
// effort low). Numbers below classify as BATCH under the default operator SLA: throughput
// (5 tok/s) clears the batch floor but not the interactive floor; ttft (40s) clears the batch
// ceiling but not the interactive one.
func autoExtractBatchProbe() []byte {
	return []byte(`{
	  "harness": "switchback",
	  "route": "auto/extract",
	  "model": "auto/extract",
	  "effort": "low",
	  "lane_ref": "auto/extract",
	  "tokens_per_sec": 5.0,
	  "ttft_ms": 40000,
	  "measured_at": "2026-06-29T11:00:00Z",
	  "source": "probe-orchestrator",
	  "evidence_refs": ["trace:probe_auto_extract"]
	}`)
}

// TestMeasuredSpeedDemotesAndVetoesInteractiveLane is THE falsifier for the operator predicate
// (2026-07-13): speed is MEASURED, never declared. A lane declared interactive-capable but
// MEASURED slow (1) gets its role changed in route authority (demoted) and (2) is vetoed from
// interactive route plans, while (3) the same lane is admitted for a batch plan that can afford
// the wait. Every assertion below FAILS before the measured-speed adjustment + worth-the-wait
// veto exist (the lane stays eligible and in the interactive chain).
func TestMeasuredSpeedDemotesAndVetoesInteractiveLane(t *testing.T) {
	home := importSpeedFixture(t)

	// Baseline: interactive plan, NO measured speed yet. auto/extract is declared interactive
	// (tier free) so it leads the chain with its declared role; the local-batch lane is already
	// vetoed for interactive because its declared class is batch.
	baseline, err := PlanCapabilityRoute(home, "classify-extract", CapabilityRoutePlanOptions{LatencyClass: "interactive"}, t0)
	if err != nil {
		t.Fatalf("baseline plan: %v", err)
	}
	if baseline.RequiredLatencyClass != "interactive" {
		t.Fatalf("required latency class not recorded: %+v", baseline)
	}
	baseAuto, ok := laneByID(baseline.Chain, "auto/extract")
	if !ok {
		t.Fatalf("baseline chain missing auto/extract: %+v", baseline.Chain)
	}
	if baseAuto.GateRole != "eligible" || baseAuto.SpeedAdjustment != nil {
		t.Fatalf("baseline auto/extract must be undemoted eligible: %+v", baseAuto)
	}
	if _, vetoed := laneByID(baseline.Omitted, "mac/qwen-fixture"); !vetoed {
		t.Fatalf("declared-batch lane must be latency-vetoed for interactive baseline: %+v", baseline.Omitted)
	}

	// Feed a measured receipt: auto/extract is actually batch-speed.
	if _, wrote, err := IngestLaneSpeedProbe(home, autoExtractBatchProbe(), t0); err != nil || !wrote {
		t.Fatalf("ingest speed probe: wrote=%v err=%v", wrote, err)
	}

	// (1)+(2) Interactive plan after measurement: auto/extract is DEMOTED (role changed in route
	// authority) AND vetoed out of the interactive chain -> route plan output visibly changed.
	interactive, err := PlanCapabilityRoute(home, "classify-extract", CapabilityRoutePlanOptions{LatencyClass: "interactive"}, t0)
	if err != nil {
		t.Fatalf("interactive plan: %v", err)
	}
	if _, stillChained := laneByID(interactive.Chain, "auto/extract"); stillChained {
		t.Fatalf("measured-slow lane must be vetoed from interactive chain: %+v", interactive.Chain)
	}
	demoted, ok := laneByID(interactive.Omitted, "auto/extract")
	if !ok {
		t.Fatalf("auto/extract must appear in omitted with a speed veto: %+v", interactive.Omitted)
	}
	if demoted.GateRole != "speed_demoted_batch" {
		t.Fatalf("role not changed in route authority: got %q want speed_demoted_batch", demoted.GateRole)
	}
	if demoted.SpeedAdjustment == nil {
		t.Fatalf("missing speed adjustment receipt on demoted lane: %+v", demoted)
	}
	adj := demoted.SpeedAdjustment
	if adj.DeclaredLatencyClass != "interactive" || adj.MeasuredLatencyClass != "batch" ||
		adj.PriorGateRole != "eligible" || adj.ProbeID == "" {
		t.Fatalf("speed adjustment not a receipt-backed demotion: %+v", adj)
	}
	if !strings.Contains(demoted.OmitReason, "latency veto") || !strings.Contains(demoted.OmitReason, "measured") {
		t.Fatalf("omit reason must be a measured latency veto: %q", demoted.OmitReason)
	}
	// The fast declared-interactive fallback (scout/chat) is still admitted.
	if _, ok := laneByID(interactive.Chain, "scout/chat"); !ok {
		t.Fatalf("fast fallback scout/chat must remain in interactive chain: %+v", interactive.Chain)
	}

	// (3) Worth-the-wait: the SAME measured-slow lane is admitted for a batch plan, still carrying
	// its demotion receipt so the role change is not hidden.
	batch, err := PlanCapabilityRoute(home, "classify-extract", CapabilityRoutePlanOptions{LatencyClass: "batch"}, t0)
	if err != nil {
		t.Fatalf("batch plan: %v", err)
	}
	admitted, ok := laneByID(batch.Chain, "auto/extract")
	if !ok {
		t.Fatalf("batch plan must admit the measured-slow lane (worth the wait): %+v", batch.Chain)
	}
	if admitted.SpeedAdjustment == nil || admitted.GateRole != "speed_demoted_batch" {
		t.Fatalf("admitted-for-batch lane must still show its demotion: %+v", admitted)
	}
	if _, ok := laneByID(batch.Chain, "mac/qwen-fixture"); !ok {
		t.Fatalf("declared-batch lane must be admitted for a batch plan: %+v", batch.Chain)
	}
}

// TestMeasuredSpeedAdjustmentIndependentOfLatencyVeto proves part (2) is not entangled with the
// veto: with NO required latency class, a measured-slow lane still carries its demotion receipt
// (role changed) and stays in the chain (no veto). It also guards that the manifest authority is
// never mutated -- the demotion is derived at plan time from the receipt, not written back.
func TestMeasuredSpeedAdjustmentIndependentOfLatencyVeto(t *testing.T) {
	home := importSpeedFixture(t)
	if _, wrote, err := IngestLaneSpeedProbe(home, autoExtractBatchProbe(), t0); err != nil || !wrote {
		t.Fatalf("ingest probe: wrote=%v err=%v", wrote, err)
	}

	plan, err := PlanCapabilityRoute(home, "classify-extract", CapabilityRoutePlanOptions{}, t0)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	lane, ok := laneByID(plan.Chain, "auto/extract")
	if !ok {
		t.Fatalf("no latency class -> lane stays in chain: %+v", plan.Chain)
	}
	if lane.GateRole != "speed_demoted_batch" || lane.SpeedAdjustment == nil {
		t.Fatalf("demotion receipt must be present even without a veto: %+v", lane)
	}
	if plan.RequiredLatencyClass != "" {
		t.Fatalf("no latency class requested but plan recorded %q", plan.RequiredLatencyClass)
	}

	// Manifest authority is untouched: re-projecting shows auto/extract with no demotion baked in.
	projection, err := ProjectModelCapabilities(home, t0)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range projection.Lanes {
		if l.ModelID == "auto/extract" {
			for _, c := range l.Capabilities {
				if strings.Contains(c.GateRole, "speed_demoted") {
					t.Fatalf("demotion must not be written back into manifest authority: %+v", l)
				}
			}
		}
	}
}

// TestCapabilityRoutePlanNoLatencyNoProbeIsByteIdentical guards additivity: the classify-extract
// plan with neither probes nor a latency class keeps its full three-lane chain.
func TestCapabilityRoutePlanNoLatencyNoProbeIsByteIdentical(t *testing.T) {
	home := importSpeedFixture(t)
	plan, err := PlanCapabilityRoute(home, "classify-extract", CapabilityRoutePlanOptions{}, t0)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Chain) != 3 {
		t.Fatalf("additive change altered a no-latency plan: chain=%+v", plan.Chain)
	}
	for _, lane := range plan.Chain {
		if lane.SpeedAdjustment != nil || strings.HasPrefix(lane.GateRole, "speed_demoted") {
			t.Fatalf("no probes must mean no demotion: %+v", lane)
		}
	}
}
