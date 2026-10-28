package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/routeevidence"
	"compound/internal/work"
)

// seedCLIThroughput appends completed route-evidence records straight to the ledger so the
// preparation-lane selection has observed tokens/sec to rank on.
func seedCLIThroughput(t *testing.T, homePath, provider, model string, completionTokens int, latencyMS int64, runs int) {
	t.Helper()
	ledger := routeevidence.ExecutionRouteEvidenceLedgerPath(homePath)
	if err := os.MkdirAll(filepath.Dir(ledger), 0o755); err != nil {
		t.Fatalf("mkdir ledger: %v", err)
	}
	f, err := os.OpenFile(ledger, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	defer f.Close()
	for i := 0; i < runs; i++ {
		ev, err := routeevidence.Build(routeevidence.BuildInput{
			CreatedAt:     cliTestNow.Add(time.Duration(i) * time.Minute),
			RequestID:     provider + "/" + model + "/cli-seed/" + strconv.Itoa(i),
			ClientProfile: "compound.prep.cli-seed",
			Protocol:      "openai",
			Provider:      provider,
			Model:         model,
			Usage:         routeevidence.Usage{CompletionTokens: completionTokens, TotalTokens: completionTokens},
			LatencyMS:     latencyMS,
			Status:        "completed",
		})
		if err != nil {
			t.Fatalf("build seed evidence: %v", err)
		}
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal seed evidence: %v", err)
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			t.Fatalf("write seed evidence: %v", err)
		}
	}
}

// TestDispatchPlanCLIRoutesPreparationLane proves the whole chain is alive through the real
// binary path: the --capability-class flag -> DispatchPlanOptions -> the wired selector seam
// (cmd init) -> routeevidence.PlanPreparationLane -> model-router registry + route-evidence
// throughput -> a ranking whose measured-fast lane wins and becomes the dispatch actor.
func TestDispatchPlanCLIRoutesPreparationLane(t *testing.T) {
	isolateDispatchCapacityGuard(t)
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	if _, err := routeevidence.ImportModelCapabilitiesYAML(homePath, filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml"), cliTestNow); err != nil {
		t.Fatalf("import model capabilities: %v", err)
	}
	// scout/chat measured fast (600 t/s), mac/qwen slow (5 t/s); auto/extract is adaptive with
	// no evidence -> unknown. The curated routing_defaults top is auto/extract, but speed must
	// promote scout/chat.
	seedCLIThroughput(t, homePath, "scout", "scout/chat", 600, 1000, 3)
	seedCLIThroughput(t, homePath, "lmstudio", "qwen-fixture-served", 5, 1000, 3)

	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	if _, err := workStore.Create(work.CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "preparation brief prepared",
		DesiredOutcome:   "prepared-by-the-time-you-look",
		EvidenceRefs:     []string{"test://prep-cli"},
	}, cliTestNow); err != nil {
		t.Fatalf("create work: %v", err)
	}

	out := captureStdout(t, func() {
		cmdWork([]string{"dispatch-plan", "--capability-class", "classify-extract", "--json"})
	})
	var plan work.DispatchPlan
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("parse dispatch plan JSON: %v\n%s", err, out)
	}
	if plan.LaneSelection == nil {
		t.Fatalf("dispatch plan must carry a lane_selection receipt:\n%s", out)
	}
	if plan.LaneSelection.Winner == nil || plan.LaneSelection.Winner.Lane != "scout/chat" {
		t.Fatalf("measured-fast lane must win over the curated top; selection=%+v", plan.LaneSelection)
	}
	if plan.Actor != "scout/chat" {
		t.Fatalf("dispatch actor must be the winning lane, got %q", plan.Actor)
	}
	// The recorded ranking must carry the inputs it relied on (auditable receipt).
	var scoutSeen bool
	for _, c := range plan.LaneSelection.Candidates {
		if c.Lane == "scout/chat" {
			scoutSeen = true
			if c.ThroughputRuns != 3 || !c.MeetsFloor {
				t.Fatalf("scout candidate must record its measured throughput: %+v", c)
			}
		}
	}
	if !scoutSeen {
		t.Fatalf("ranking must list every candidate lane, including scout/chat: %+v", plan.LaneSelection.Candidates)
	}
}
