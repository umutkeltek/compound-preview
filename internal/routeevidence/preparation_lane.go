package routeevidence

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"compound/internal/capgraph"
	"compound/internal/laneselect"
)

// PreparationLaneOptions configures the adapter that gathers evidence for the pure
// laneselect.RankPreparationLanes ranking.
type PreparationLaneOptions struct {
	FloorTokensPerSecond float64
	PrivacySensitive     bool
	// Liveness is an optional lane(model id) -> alive|down|unknown map (a probe seam). Absent
	// lanes default to unknown; the pure ranking never assumes a lane is alive.
	Liveness map[string]string
	// Filters are the veto seam (capacity planner, work_697dedbfca03). Nil in this packet.
	Filters []laneselect.PreparationLaneFilter
}

type preparationAgentLaneEvidence struct {
	NodeID   string
	HasClass bool
}

// PlanPreparationLane reads the Compound model-router authority (fit + cost, account/privacy
// gated) and the route-evidence ledger (observed tokens/sec) for a preparation-class
// capability, then ranks via the pure laneselect ranking. It is the impure adapter over the
// pure ranking; the registry and ledger are read through their existing owners, never cached
// or rebuilt.
func PlanPreparationLane(homePath, class string, opts PreparationLaneOptions, now time.Time) (laneselect.PreparationLaneRanking, error) {
	if !laneselect.IsPreparationClass(class) {
		return laneselect.PreparationLaneRanking{}, fmt.Errorf("capability class %q is not a preparation class", class)
	}
	plan, err := PlanCapabilityRoute(homePath, class, CapabilityRoutePlanOptions{
		PrivacySensitive:            opts.PrivacySensitive,
		SuppressPendingObservations: true,
	}, now)
	if err != nil {
		return laneselect.PreparationLaneRanking{}, err
	}
	targetStats, profileStats, err := loadRouteThroughput(homePath)
	if err != nil {
		return laneselect.PreparationLaneRanking{}, err
	}
	agentLaneEvidence, graphPresent, err := loadPreparationAgentLaneEvidence(homePath, class, plan.Chain)
	if err != nil {
		return laneselect.PreparationLaneRanking{}, err
	}
	inputs := make([]laneselect.PreparationLaneInput, 0, len(plan.Chain))
	for _, lane := range plan.Chain {
		tps, runs, known := laneThroughput(lane, targetStats, profileStats)
		liveness := laneselect.LaneLivenessUnknown
		if opts.Liveness != nil {
			if v, ok := opts.Liveness[lane.ModelID]; ok {
				liveness = laneselect.NormalizeLiveness(v)
			}
		}
		claimLevel := lane.ClaimLevel
		if evidence, ok := agentLaneEvidence[lane.ModelID]; ok && !evidence.HasClass {
			// An exact tuple is stronger than a loose model-router claim. If that
			// agent-lane node does not carry the requested capable_of edge, do not
			// present the lane as proven fit. Absence of an exact tuple is merely
			// unresolved because the two registries do not yet share a total ID.
			claimLevel = ""
		}
		inputs = append(inputs, laneselect.PreparationLaneInput{
			Lane:            lane.ModelID,
			Label:           lane.Lane,
			ClaimLevel:      claimLevel,
			RoutingPosition: lane.Position,
			CostClass:       lane.CostClass,
			Tier:            lane.Tier,
			Harness:         lane.Harness,
			ProviderRoute:   lane.ProviderRoute,
			TokensPerSecond: tps,
			ThroughputKnown: known,
			ThroughputRuns:  runs,
			Liveness:        liveness,
		})
	}
	ranking := laneselect.RankPreparationLanes(class, opts.FloorTokensPerSecond, inputs, opts.Filters...)
	matched := 0
	for i := range ranking.Candidates {
		candidate := &ranking.Candidates[i]
		evidence, ok := agentLaneEvidence[candidate.Lane]
		switch {
		case ok && evidence.HasClass:
			matched++
			candidate.Rationale += " agent_lane=" + evidence.NodeID + " class_fit=capable_of"
		case ok:
			matched++
			candidate.Rationale += " agent_lane=" + evidence.NodeID + " class_fit=missing"
		case graphPresent:
			candidate.Rationale += " agent_lane=unresolved"
		default:
			candidate.Rationale += " agent_lane=unavailable"
		}
	}
	if graphPresent {
		ranking.Warnings = append(ranking.Warnings, fmt.Sprintf("agent-lane capability graph consumed: %d/%d exact candidate tuples matched", matched, len(ranking.Candidates)))
	} else {
		ranking.Warnings = append(ranking.Warnings, "agent-lane capability graph unavailable; model-router fit remains unresolved against agent-lane tuples")
	}
	ranking.Warnings = append(ranking.Warnings, plan.Warnings...)
	return ranking, nil
}

// loadPreparationAgentLaneEvidence consumes the existing agent-lane CapabilityGraph surface
// without inventing a second registry. The only lossless public join available today is the
// exported short-lane-id resolver. An exact tuple may confirm or contradict class fit; a missing
// tuple stays unresolved because imported harness×route×model×effort nodes and model-router IDs
// are not yet a total one-to-one namespace.
func loadPreparationAgentLaneEvidence(homePath, class string, lanes []CapabilityRouteLane) (map[string]preparationAgentLaneEvidence, bool, error) {
	out := map[string]preparationAgentLaneEvidence{}
	if !capgraph.Exists(homePath) {
		return out, false, nil
	}
	store, err := capgraph.OpenReadOnly(homePath)
	if err != nil {
		return nil, true, fmt.Errorf("read agent-lane capability graph: %w", err)
	}
	nodes, err := store.List()
	if err != nil {
		return nil, true, fmt.Errorf("read agent-lane nodes: %w", err)
	}
	edges, err := store.Edges()
	if err != nil {
		return nil, true, fmt.Errorf("read agent-lane edges: %w", err)
	}
	nodesByID := map[string]capgraph.Node{}
	for _, node := range nodes {
		if node.Kind == "agent-lane" && !node.Retired {
			nodesByID[node.ID] = node
		}
	}
	classRef := "task-class:" + strings.ToLower(strings.TrimSpace(class))
	capable := map[string]bool{}
	for _, edge := range edges {
		if edge.Kind == capgraph.EdCapableOf && strings.EqualFold(strings.TrimSpace(edge.To), classRef) {
			capable[edge.From] = true
		}
	}
	for _, lane := range lanes {
		nodeID, ok := capgraph.AgentLaneNodeIDFromShortLaneID(lane.ModelID)
		if !ok {
			continue
		}
		if _, ok := nodesByID[nodeID]; !ok {
			continue
		}
		out[lane.ModelID] = preparationAgentLaneEvidence{NodeID: nodeID, HasClass: capable[nodeID]}
	}
	return out, true, nil
}

type laneThroughputStat struct {
	completionTokens int
	latencyMS        int64
	runs             int
}

// loadRouteThroughput scans the route-evidence ledger and aggregates completed-run token
// throughput, indexed by (provider/model) and by client_profile. Only completed records with
// positive latency and completion tokens contribute — a failed or timing-less run is not
// evidence of speed.
func loadRouteThroughput(homePath string) (targetStats, profileStats map[string]laneThroughputStat, err error) {
	targetStats = map[string]laneThroughputStat{}
	profileStats = map[string]laneThroughputStat{}
	path := ExecutionRouteEvidenceLedgerPath(homePath)
	f, openErr := os.Open(path)
	if openErr != nil {
		if os.IsNotExist(openErr) {
			return targetStats, profileStats, nil
		}
		return nil, nil, fmt.Errorf("read route evidence ledger: %w", openErr)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		ev, decErr := Decode([]byte(raw))
		if decErr != nil {
			return nil, nil, fmt.Errorf("decode route evidence ledger: %w", decErr)
		}
		if !strings.EqualFold(ev.Status, "completed") || ev.LatencyMS <= 0 || ev.Usage.CompletionTokens <= 0 {
			continue
		}
		selectedKey := throughputKey(ev.SelectedTarget.Provider, ev.SelectedTarget.Model)
		accumulateThroughput(targetStats, selectedKey, ev)
		if strings.TrimSpace(ev.ResolvedProvider) != "" && strings.TrimSpace(ev.ResolvedModel) != "" {
			if resolved := throughputKey(ev.ResolvedProvider, ev.ResolvedModel); resolved != selectedKey {
				accumulateThroughput(targetStats, resolved, ev)
			}
		}
		if profile := strings.ToLower(strings.TrimSpace(ev.ClientProfile)); profile != "" {
			accumulateThroughput(profileStats, profile, ev)
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return nil, nil, fmt.Errorf("scan route evidence ledger: %w", scanErr)
	}
	return targetStats, profileStats, nil
}

func accumulateThroughput(stats map[string]laneThroughputStat, key string, ev Evidence) {
	if key == "" {
		return
	}
	s := stats[key]
	s.completionTokens += ev.Usage.CompletionTokens
	s.latencyMS += ev.LatencyMS
	s.runs++
	stats[key] = s
}

// laneThroughput resolves observed tokens/sec for a lane by matching route evidence to its
// resolution targets (direct lanes) or its provider_route client-profile (adaptive lanes).
// Absent evidence returns known=false so the ranking scores it conservatively.
func laneThroughput(lane CapabilityRouteLane, targetStats, profileStats map[string]laneThroughputStat) (tps float64, runs int, known bool) {
	var agg laneThroughputStat
	seen := map[string]bool{}
	add := func(s laneThroughputStat) {
		agg.completionTokens += s.completionTokens
		agg.latencyMS += s.latencyMS
		agg.runs += s.runs
	}
	for _, tgt := range lane.Resolution.Targets {
		key := throughputKey(tgt.Provider, tgt.Model)
		if key == "" || seen["t:"+key] {
			continue
		}
		if s, ok := targetStats[key]; ok {
			seen["t:"+key] = true
			add(s)
		}
	}
	if pr := strings.ToLower(strings.TrimSpace(lane.ProviderRoute)); pr != "" && !seen["p:"+pr] {
		if s, ok := profileStats[pr]; ok {
			seen["p:"+pr] = true
			add(s)
		}
	}
	if agg.runs == 0 || agg.latencyMS <= 0 {
		return 0, agg.runs, false
	}
	return float64(agg.completionTokens) / (float64(agg.latencyMS) / 1000.0), agg.runs, true
}

func throughputKey(provider, model string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.ToLower(strings.TrimSpace(model))
	if provider == "" && model == "" {
		return ""
	}
	return provider + "/" + model
}
