package laneselect

import "testing"

func laneScoreByID(r PreparationLaneRanking, id string) (PreparationLaneScore, bool) {
	for _, c := range r.Candidates {
		if c.Lane == id {
			return c, true
		}
	}
	return PreparationLaneScore{}, false
}

// f1: a slow-free lane must NOT beat a fast-cheap lane of the same capability fit.
// On a cost-only (naive) ranker the free lane wins; the speed-above-floor dimension
// must flip that. This is the operator ruling made into a scored dimension.
func TestRankPreparationLanesSlowFreeLosesToFastCheap(t *testing.T) {
	inputs := []PreparationLaneInput{
		{Lane: "free-slow", ClaimLevel: "primary", CostClass: "free", RoutingPosition: 0, TokensPerSecond: 3, ThroughputKnown: true, ThroughputRuns: 4, Liveness: LaneLivenessAlive},
		{Lane: "cheap-fast", ClaimLevel: "primary", CostClass: "api-marginal", RoutingPosition: 1, TokensPerSecond: 45, ThroughputKnown: true, ThroughputRuns: 4, Liveness: LaneLivenessAlive},
	}
	got := RankPreparationLanes("classify-extract", 12, inputs)
	if got.Winner == nil {
		t.Fatalf("expected a winner, got none: %+v", got)
	}
	if got.Winner.Lane != "cheap-fast" {
		t.Fatalf("slow-free must lose to fast-cheap; winner=%s ranking=%+v", got.Winner.Lane, got.Candidates)
	}
	slow, _ := laneScoreByID(got, "free-slow")
	if slow.MeetsFloor {
		t.Fatalf("free-slow (3 t/s) must not meet a 12 t/s floor")
	}
}

// f5: a lane with no throughput evidence is scored unknown, never assumed fast — it must
// lose to a known-fast lane of the same fit.
func TestRankPreparationLanesUnknownThroughputConservative(t *testing.T) {
	inputs := []PreparationLaneInput{
		{Lane: "unknown-thpt", ClaimLevel: "primary", CostClass: "free", RoutingPosition: 0, ThroughputKnown: false, Liveness: LaneLivenessAlive},
		{Lane: "known-fast", ClaimLevel: "primary", CostClass: "free", RoutingPosition: 1, TokensPerSecond: 30, ThroughputKnown: true, ThroughputRuns: 3, Liveness: LaneLivenessAlive},
	}
	got := RankPreparationLanes("classify-extract", 12, inputs)
	if got.Winner == nil || got.Winner.Lane != "known-fast" {
		t.Fatalf("unknown throughput must not be assumed fast; winner=%v", got.Winner)
	}
	unknown, _ := laneScoreByID(got, "unknown-thpt")
	if unknown.MeetsFloor {
		t.Fatalf("unknown-throughput lane must not report meets_floor")
	}
}

// Liveness gate: a lane whose probe says down is never the winner, even if it is the
// highest-fit, fastest, cheapest lane.
func TestRankPreparationLanesDownLaneNeverWins(t *testing.T) {
	inputs := []PreparationLaneInput{
		{Lane: "down-star", ClaimLevel: "primary", CostClass: "free", RoutingPosition: 0, TokensPerSecond: 99, ThroughputKnown: true, ThroughputRuns: 9, Liveness: LaneLivenessDown},
		{Lane: "live-ok", ClaimLevel: "capable", CostClass: "api-marginal", RoutingPosition: 1, TokensPerSecond: 20, ThroughputKnown: true, ThroughputRuns: 3, Liveness: LaneLivenessAlive},
	}
	got := RankPreparationLanes("classify-extract", 12, inputs)
	if got.Winner == nil || got.Winner.Lane != "live-ok" {
		t.Fatalf("down lane must never win; winner=%v", got.Winner)
	}
	down, ok := laneScoreByID(got, "down-star")
	if !ok {
		t.Fatalf("down lane must stay visible in the ranking, not silently dropped")
	}
	if down.Eligible || down.OmitReason == "" {
		t.Fatalf("down lane must be ineligible with a reason: %+v", down)
	}
}

// The veto seam: a filter (the capacity planner, work_697dedbfca03) can refuse a lane
// BEFORE it becomes winner-eligible; the refused lane stays visible with the filter's reason.
func TestRankPreparationLanesFilterSeamRefuses(t *testing.T) {
	inputs := []PreparationLaneInput{
		{Lane: "walled", ClaimLevel: "primary", CostClass: "free", RoutingPosition: 0, TokensPerSecond: 99, ThroughputKnown: true, ThroughputRuns: 9, Liveness: LaneLivenessAlive},
		{Lane: "healthy", ClaimLevel: "primary", CostClass: "free", RoutingPosition: 1, TokensPerSecond: 20, ThroughputKnown: true, ThroughputRuns: 3, Liveness: LaneLivenessAlive},
	}
	veto := func(in PreparationLaneInput) (bool, string) {
		if in.Lane == "walled" {
			return true, "capacity walled (usedPercent=100)"
		}
		return false, ""
	}
	got := RankPreparationLanes("classify-extract", 12, inputs, veto)
	if got.Winner == nil || got.Winner.Lane != "healthy" {
		t.Fatalf("filter must remove walled lane from winner eligibility; winner=%v", got.Winner)
	}
	walled, ok := laneScoreByID(got, "walled")
	if !ok || walled.Eligible || walled.OmitReason != "capacity walled (usedPercent=100)" {
		t.Fatalf("refused lane must stay visible with the filter reason: %+v", walled)
	}
}

// f2/f4: a selection with no recorded inputs is a failure. Every candidate must echo the
// evidence the ranking relied on (cost, claim, throughput, floor), so the choice is auditable.
func TestRankPreparationLanesRecordsInputs(t *testing.T) {
	inputs := []PreparationLaneInput{
		{Lane: "free-fast", ClaimLevel: "primary", CostClass: "free", RoutingPosition: 0, TokensPerSecond: 40, ThroughputKnown: true, ThroughputRuns: 5, Liveness: LaneLivenessAlive},
		{Lane: "paid-fast", ClaimLevel: "capable", CostClass: "api-marginal", RoutingPosition: 1, TokensPerSecond: 40, ThroughputKnown: true, ThroughputRuns: 5, Liveness: LaneLivenessAlive},
	}
	got := RankPreparationLanes("classify-extract", 12, inputs)
	if got.SchemaVersion == "" || got.FloorTokensPerSecond <= 0 {
		t.Fatalf("ranking must record its schema + floor: %+v", got)
	}
	if len(got.Candidates) != len(inputs) {
		t.Fatalf("every candidate must be recorded, not dropped: %d != %d", len(got.Candidates), len(inputs))
	}
	for _, c := range got.Candidates {
		if c.Lane == "" || c.CostClass == "" || c.ClaimLevel == "" || c.Rationale == "" {
			t.Fatalf("candidate missing recorded inputs: %+v", c)
		}
		if !c.ThroughputKnown || c.ThroughputRuns == 0 {
			t.Fatalf("known-throughput candidate must record its sample: %+v", c)
		}
	}
	// Same fit + both above floor -> cost breaks the tie: free beats paid.
	if got.Winner == nil || got.Winner.Lane != "free-fast" {
		t.Fatalf("cost tiebreak: free-fast should win over paid-fast; winner=%v", got.Winner)
	}
}

// Fit dominates as the coarse gate, but primary and capable share a fit tier so that speed
// can reorder within proven-fit; assumed/supervised lanes rank strictly below.
func TestRankPreparationLanesSpeedReordersWithinProvenFit(t *testing.T) {
	inputs := []PreparationLaneInput{
		// primary but unknown throughput (was routing_defaults position 0)
		{Lane: "primary-unknown", ClaimLevel: "primary", CostClass: "free", RoutingPosition: 0, ThroughputKnown: false, Liveness: LaneLivenessAlive},
		// capable and fast -> same fit tier as primary, meets floor
		{Lane: "capable-fast", ClaimLevel: "capable", CostClass: "free", RoutingPosition: 1, TokensPerSecond: 60, ThroughputKnown: true, ThroughputRuns: 6, Liveness: LaneLivenessAlive},
		// assumed lane is a lower fit tier regardless of speed
		{Lane: "assumed-fast", ClaimLevel: "assumed", CostClass: "local", RoutingPosition: 2, TokensPerSecond: 80, ThroughputKnown: true, ThroughputRuns: 6, Liveness: LaneLivenessAlive},
	}
	got := RankPreparationLanes("classify-extract", 12, inputs)
	if got.Winner == nil || got.Winner.Lane != "capable-fast" {
		t.Fatalf("speed must reorder within proven fit; winner=%v", got.Winner)
	}
	// assumed lane must rank last despite being the fastest.
	last := got.Candidates[len(got.Candidates)-1]
	if last.Lane != "assumed-fast" {
		t.Fatalf("assumed/supervised lane must rank below proven-fit lanes; last=%s", last.Lane)
	}
}

// No eligible lane (all down/filtered) blocks with a named status; it never invents a winner.
func TestRankPreparationLanesNoEligibleLaneBlocks(t *testing.T) {
	inputs := []PreparationLaneInput{
		{Lane: "down-1", ClaimLevel: "primary", CostClass: "free", TokensPerSecond: 50, ThroughputKnown: true, ThroughputRuns: 2, Liveness: LaneLivenessDown},
	}
	got := RankPreparationLanes("classify-extract", 12, inputs)
	if got.Winner != nil {
		t.Fatalf("no eligible lane must not yield a winner: %+v", got.Winner)
	}
	if got.Status != PreparationLaneStatusBlocked {
		t.Fatalf("status should be blocked_no_eligible_lane, got %s", got.Status)
	}
}

func TestIsPreparationClass(t *testing.T) {
	cases := map[string]bool{
		"classify-extract": true,
		"draft-prose":      true,
		"judge-critique":   false,
		"build-implement":  false,
		"plan-decompose":   false,
		"":                 false,
	}
	for class, want := range cases {
		if got := IsPreparationClass(class); got != want {
			t.Fatalf("IsPreparationClass(%q)=%v want %v", class, got, want)
		}
	}
}
