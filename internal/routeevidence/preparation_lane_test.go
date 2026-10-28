package routeevidence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/capgraph"
	"compound/internal/laneselect"
)

var prepNow = time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

// seedThroughput appends `runs` completed route-evidence records for provider/model,
// each carrying completionTokens over latencyMS, straight to the ledger jsonl.
func seedThroughput(t *testing.T, home, provider, model string, completionTokens int, latencyMS int64, runs int) {
	t.Helper()
	ledger := ExecutionRouteEvidenceLedgerPath(home)
	if err := os.MkdirAll(filepath.Dir(ledger), 0o755); err != nil {
		t.Fatalf("mkdir ledger: %v", err)
	}
	f, err := os.OpenFile(ledger, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	defer f.Close()
	for i := 0; i < runs; i++ {
		ev, err := Build(BuildInput{
			CreatedAt:     prepNow.Add(time.Duration(i) * time.Minute),
			RequestID:     provider + "/" + model + "/seed/" + time.Duration(i).String(),
			ClientProfile: "compound.prep.seed",
			Protocol:      "openai",
			Provider:      provider,
			Model:         model,
			Usage:         Usage{CompletionTokens: completionTokens, TotalTokens: completionTokens},
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

func laneScoreByID(r laneselect.PreparationLaneRanking, id string) (laneselect.PreparationLaneScore, bool) {
	for _, c := range r.Candidates {
		if c.Lane == id {
			return c, true
		}
	}
	return laneselect.PreparationLaneScore{}, false
}

func importPreparationRegistry(t *testing.T, home string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "model-capabilities.yaml")
	data := `version: 1
purpose: "Synthetic preparation ranking fixture."
capability_classes: [classify-extract]
models:
  slow/free:
    lane: "slow free"
    harness: "fixture"
    provider_route: "slow/free"
    resolution:
      mode: direct
      targets:
        - provider: slow
          model: slow/free
    cost_class: free
    tier: free
    capabilities:
      classify-extract:
        level: primary
        evidence: "test://slow-free-fit"
  fast/cheap:
    lane: "fast cheap"
    harness: "fixture"
    provider_route: "fast/cheap"
    resolution:
      mode: direct
      targets:
        - provider: fast
          model: fast/cheap
    cost_class: api-marginal
    tier: cheap
    capabilities:
      classify-extract:
        level: primary
        evidence: "test://fast-cheap-fit"
routing_defaults:
  classify-extract: [slow/free, fast/cheap]
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write model registry: %v", err)
	}
	if _, err := ImportModelCapabilitiesYAML(home, path, prepNow); err != nil {
		t.Fatalf("import model capabilities: %v", err)
	}
}

func addAgentLaneCapability(t *testing.T, home, lane, class string) string {
	t.Helper()
	nodeID, ok := capgraph.AgentLaneNodeIDFromShortLaneID(lane)
	if !ok {
		t.Fatalf("resolve agent-lane id for %q", lane)
	}
	store, err := capgraph.Open(home)
	if err != nil {
		t.Fatalf("open capability graph: %v", err)
	}
	if err := store.Add(capgraph.Node{ID: nodeID, Kind: "agent-lane", Name: lane, EvidenceOnly: true}); err != nil {
		t.Fatalf("add agent-lane node: %v", err)
	}
	if err := store.AddEdge(capgraph.Edge{
		Kind:       capgraph.EdCapableOf,
		From:       nodeID,
		To:         "task-class:" + class,
		Confidence: 1,
	}, prepNow); err != nil {
		t.Fatalf("add agent-lane capability edge: %v", err)
	}
	return nodeID
}

// Green-at-birth against the landed selector: the adapter-level seam must preserve the
// operator ruling too, not only the pure ranker. A measured slow/free lane loses to a
// measured fast/cheap lane, and the persisted score echoes every speed/cost input.
func TestPlanPreparationLaneSlowFreeLosesToFastCheapAndRecordsInputs(t *testing.T) {
	home := t.TempDir()
	importPreparationRegistry(t, home)
	seedThroughput(t, home, "slow", "slow/free", 3, 1000, 4)
	seedThroughput(t, home, "fast", "fast/cheap", 45, 1000, 4)

	got, err := PlanPreparationLane(home, "classify-extract", PreparationLaneOptions{
		FloorTokensPerSecond: 12,
		Liveness: map[string]string{
			"slow/free":  laneselect.LaneLivenessAlive,
			"fast/cheap": laneselect.LaneLivenessAlive,
		},
	}, prepNow)
	if err != nil {
		t.Fatalf("plan preparation lane: %v", err)
	}
	if got.Winner == nil || got.Winner.Lane != "fast/cheap" {
		t.Fatalf("slow-free must lose to fast-cheap; winner=%v candidates=%+v", got.Winner, got.Candidates)
	}
	for _, id := range []string{"slow/free", "fast/cheap"} {
		score, ok := laneScoreByID(got, id)
		if !ok || !score.ThroughputKnown || score.ThroughputRuns != 4 || score.CostClass == "" || score.Rationale == "" {
			t.Fatalf("selection input for %s was not persisted: %+v", id, score)
		}
	}
}

// Green-at-birth against the landed selector: missing throughput is below-floor evidence,
// never permission to assume the cheapest lane is fast.
func TestPlanPreparationLaneUnknownThroughputConservative(t *testing.T) {
	home := t.TempDir()
	importPreparationRegistry(t, home)
	seedThroughput(t, home, "fast", "fast/cheap", 45, 1000, 3)

	got, err := PlanPreparationLane(home, "classify-extract", PreparationLaneOptions{
		FloorTokensPerSecond: 12,
		Liveness: map[string]string{
			"slow/free":  laneselect.LaneLivenessAlive,
			"fast/cheap": laneselect.LaneLivenessAlive,
		},
	}, prepNow)
	if err != nil {
		t.Fatalf("plan preparation lane: %v", err)
	}
	if got.Winner == nil || got.Winner.Lane != "fast/cheap" {
		t.Fatalf("known-fast lane must beat unknown-throughput free lane; winner=%v", got.Winner)
	}
	unknown, ok := laneScoreByID(got, "slow/free")
	if !ok || unknown.ThroughputKnown || unknown.MeetsFloor {
		t.Fatalf("unknown throughput must remain conservative: %+v", unknown)
	}
}

// Green-at-birth against the landed selector: the liveness seam is a hard veto, not a
// ranking hint. Even the fastest/free lane stays visible but can never win when down.
func TestPlanPreparationLaneDownLaneNeverSelected(t *testing.T) {
	home := t.TempDir()
	importPreparationRegistry(t, home)
	seedThroughput(t, home, "slow", "slow/free", 100, 1000, 3)
	seedThroughput(t, home, "fast", "fast/cheap", 20, 1000, 3)

	got, err := PlanPreparationLane(home, "classify-extract", PreparationLaneOptions{
		FloorTokensPerSecond: 12,
		Liveness: map[string]string{
			"slow/free":  laneselect.LaneLivenessDown,
			"fast/cheap": laneselect.LaneLivenessAlive,
		},
	}, prepNow)
	if err != nil {
		t.Fatalf("plan preparation lane: %v", err)
	}
	if got.Winner == nil || got.Winner.Lane != "fast/cheap" {
		t.Fatalf("down lane must never win; winner=%v", got.Winner)
	}
	down, ok := laneScoreByID(got, "slow/free")
	if !ok || down.Eligible || !strings.Contains(down.OmitReason, "down") {
		t.Fatalf("down lane must remain visible as refused: %+v", down)
	}
}

// Red-first missing-surface falsifier: when an exact agent-lane tuple exists, its capable_of
// edge participates in fit and its node ref lands in the recorded candidate rationale. A
// registry claim contradicted by the matching graph node is not treated as proven fit.
func TestPlanPreparationLaneConsumesAgentLaneCapabilityGraph(t *testing.T) {
	home := t.TempDir()
	importPreparationRegistry(t, home)
	slowNode := addAgentLaneCapability(t, home, "slow/free", "judge-critique")
	fastNode := addAgentLaneCapability(t, home, "fast/cheap", "classify-extract")
	seedThroughput(t, home, "slow", "slow/free", 100, 1000, 3)
	seedThroughput(t, home, "fast", "fast/cheap", 20, 1000, 3)

	got, err := PlanPreparationLane(home, "classify-extract", PreparationLaneOptions{
		FloorTokensPerSecond: 12,
		Liveness: map[string]string{
			"slow/free":  laneselect.LaneLivenessAlive,
			"fast/cheap": laneselect.LaneLivenessAlive,
		},
	}, prepNow)
	if err != nil {
		t.Fatalf("plan preparation lane: %v", err)
	}
	if got.Winner == nil || got.Winner.Lane != "fast/cheap" {
		t.Fatalf("graph-confirmed class fit must beat contradicted fit; winner=%v candidates=%+v", got.Winner, got.Candidates)
	}
	slow, _ := laneScoreByID(got, "slow/free")
	fast, _ := laneScoreByID(got, "fast/cheap")
	if slow.FitTier != 0 || !strings.Contains(slow.Rationale, slowNode) || !strings.Contains(slow.Rationale, "class_fit=missing") {
		t.Fatalf("contradicted agent-lane fit was not recorded: %+v", slow)
	}
	if !strings.Contains(fast.Rationale, fastNode) || !strings.Contains(fast.Rationale, "class_fit=capable_of") {
		t.Fatalf("agent-lane capability input was not recorded: %+v", fast)
	}
}

// Adapter: reads the Compound model-router authority + route-evidence throughput and produces
// a ranking. Proves observed tokens/sec (not the curated routing_defaults order) picks the winner.
func TestPlanPreparationLaneReadsRegistryAndThroughput(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml")
	if _, err := ImportModelCapabilitiesYAML(home, source, prepNow); err != nil {
		t.Fatalf("import model capabilities: %v", err)
	}
	// scout/chat is fast (600 t/s); mac/qwen is slow (5 t/s). auto/extract is adaptive with no
	// throughput evidence -> unknown. Floor 50 t/s.
	seedThroughput(t, home, "scout", "scout/chat", 600, 1000, 3)
	seedThroughput(t, home, "lmstudio", "qwen-fixture-served", 5, 1000, 3)

	got, err := PlanPreparationLane(home, "classify-extract", PreparationLaneOptions{FloorTokensPerSecond: 50}, prepNow)
	if err != nil {
		t.Fatalf("plan preparation lane: %v", err)
	}
	if got.Winner == nil {
		t.Fatalf("expected a winner: %+v", got)
	}
	// auto/extract is routing_defaults[0] (primary) but has unknown throughput; scout/chat is
	// capable + measured-fast -> speed must promote scout/chat over the curated top lane.
	if got.Winner.Lane != "scout/chat" {
		t.Fatalf("measured-fast lane must win over unknown-throughput curated top; winner=%s candidates=%+v", got.Winner.Lane, got.Candidates)
	}
	auto, ok := laneScoreByID(got, "auto/extract")
	if !ok || auto.MeetsFloor {
		t.Fatalf("adaptive lane with no evidence must be unknown/below-floor: %+v", auto)
	}
	scout, _ := laneScoreByID(got, "scout/chat")
	if scout.ThroughputRuns != 3 || scout.TokensPerSecond < 50 {
		t.Fatalf("scout throughput must aggregate the seeded runs: %+v", scout)
	}
}

// A non-preparation class is refused by the adapter (defense in depth; the dispatch surface
// also gates on IsPreparationClass).
func TestPlanPreparationLaneRefusesNonPreparationClass(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml")
	if _, err := ImportModelCapabilitiesYAML(home, source, prepNow); err != nil {
		t.Fatalf("import model capabilities: %v", err)
	}
	if _, err := PlanPreparationLane(home, "judge-critique", PreparationLaneOptions{}, prepNow); err == nil {
		t.Fatalf("judge-critique is not a preparation class; adapter must refuse")
	}
}
