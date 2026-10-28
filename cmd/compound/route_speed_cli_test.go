package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/home"
)

// TestRouteSpeedObserveAndVetoCLI drives the whole slice through the CLI: ingest a measured
// lane-speed probe RESULT from a file, then a `route plan --latency-class interactive` visibly
// demotes and vetoes the measured-slow lane, while `--latency-class batch` re-admits it.
func TestRouteSpeedObserveAndVetoCLI(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	src := filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml")
	captureStdout(t, func() { cmdRoute([]string{"import-model-capabilities", src}) })

	// Interactive plan BEFORE any measurement: auto/extract leads the chain.
	before := captureStdout(t, func() {
		cmdRoute([]string{"plan", "--capability-class", "classify-extract", "--latency-class", "interactive"})
	})
	if !strings.Contains(before, "required_latency_class: interactive") {
		t.Fatalf("plan did not record latency class:\n%s", before)
	}
	if !firstLaneContains(before, "auto/extract") {
		t.Fatalf("baseline interactive chain must lead with auto/extract:\n%s", before)
	}

	// Ingest a measured probe: auto/extract is actually batch-speed.
	probe := filepath.Join(t.TempDir(), "probe.json")
	if err := os.WriteFile(probe, []byte(`{"harness":"switchback","route":"auto/extract","model":"auto/extract","effort":"low","lane_ref":"auto/extract","tokens_per_sec":5,"ttft_ms":40000,"source":"probe-orchestrator"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	obs := captureStdout(t, func() { cmdRoute([]string{"speed", "observe", "--from", probe}) })
	if !strings.Contains(obs, "measured_latency_class: batch") || !strings.Contains(obs, "recorded: true") {
		t.Fatalf("observe did not classify/record the probe:\n%s", obs)
	}

	// `route speed list` surfaces the measurement.
	list := captureStdout(t, func() { cmdRoute([]string{"speed", "list"}) })
	if !strings.Contains(list, "measured=batch") || !strings.Contains(list, "auto") {
		t.Fatalf("speed list missing the probe:\n%s", list)
	}

	// Interactive plan AFTER measurement: auto/extract is demoted + vetoed out of the chain.
	after := captureStdout(t, func() {
		cmdRoute([]string{"plan", "--capability-class", "classify-extract", "--latency-class", "interactive"})
	})
	if firstLaneContains(after, "auto/extract") {
		t.Fatalf("measured-slow lane must not lead the interactive chain anymore:\n%s", after)
	}
	if !strings.Contains(after, "omitted:") || !strings.Contains(after, "auto/extract") || !strings.Contains(after, "latency veto") {
		t.Fatalf("interactive plan must omit auto/extract with a latency veto:\n%s", after)
	}
	if !strings.Contains(after, "speed_demoted_batch") {
		t.Fatalf("role change (speed_demoted_batch) must be visible in plan output:\n%s", after)
	}

	// Batch plan: the same lane is admitted (worth the wait) and still carries its demotion note.
	batch := captureStdout(t, func() {
		cmdRoute([]string{"plan", "--capability-class", "classify-extract", "--latency-class", "batch"})
	})
	if !strings.Contains(batch, "speed_adjustment:") || !strings.Contains(batch, "declared=interactive measured=batch") {
		t.Fatalf("batch plan must admit the lane with a visible demotion note:\n%s", batch)
	}
	firstBatchLane := firstLaneLine(batch)
	if !strings.Contains(firstBatchLane, "auto/extract") {
		t.Fatalf("batch plan should still admit auto/extract in the chain:\n%s", batch)
	}
}

func firstLaneLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "- lane:") {
			return line
		}
	}
	return ""
}

func firstLaneContains(out, want string) bool {
	return strings.Contains(firstLaneLine(out), want)
}
