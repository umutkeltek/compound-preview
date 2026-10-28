// Package laneselect holds the pure preparation-lane ranking shared by the dispatch surface
// (internal/work) and the evidence adapter (internal/routeevidence). It is a leaf (stdlib
// only): keeping the pure ranking here breaks the work<->routeevidence import cycle and lets
// the ranking be tested without a home, a registry, or a ledger.
package laneselect

import (
	"fmt"
	"sort"
	"strings"
)

const (
	// PreparationLaneRankingSchemaVersion versions the recorded selection receipt.
	PreparationLaneRankingSchemaVersion = "compound.preparation_lane_ranking.v0.1"

	// DefaultPreparationFloorTokensPerSecond is the cadence floor a preparation lane must
	// clear to count as fast enough for the queue. It is tunable (adapter option / CLI); it
	// is not load-bearing for correctness — the tests pin behavior with explicit floors.
	DefaultPreparationFloorTokensPerSecond = 12.0

	PreparationLaneStatusRanked  = "ranked"
	PreparationLaneStatusBlocked = "blocked_no_eligible_lane"
	PreparationLaneStatusNoLanes = "no_candidate_lanes"

	PreparationLaneAuthorityRef = "compound://registries/model-router"
)

const (
	LaneLivenessAlive   = "alive"
	LaneLivenessDown    = "down"
	LaneLivenessUnknown = "unknown"
)

// preparationCapabilityClasses are the classify/extract/summarize-shaped classes this
// selection routes (decision briefs and their kin). Every other capability class keeps the
// existing dispatch default byte-identical. This is a policy allowlist, deliberately narrow;
// widen it here, never by hardcoding lane facts.
var preparationCapabilityClasses = map[string]bool{
	"classify-extract": true,
	"draft-prose":      true,
}

// IsPreparationClass reports whether a capability class is routed by the preparation-lane
// selection. Non-preparation classes (judge-critique, build-implement, ...) are out of scope.
func IsPreparationClass(class string) bool {
	return preparationCapabilityClasses[strings.TrimSpace(class)]
}

// PreparationLaneInput is one candidate lane's evidence, already gathered from the
// Compound-owned surfaces (model-router registry fit/cost, route-evidence throughput,
// liveness probe). The ranking is a pure function of these inputs.
type PreparationLaneInput struct {
	Lane            string
	Label           string
	ClaimLevel      string // primary|capable|assumed|"" (registry claim for the class)
	RoutingPosition int    // position in routing_defaults[class]
	CostClass       string // free|local|api-marginal|premium|...
	Tier            string
	Harness         string
	ProviderRoute   string
	TokensPerSecond float64
	ThroughputKnown bool
	ThroughputRuns  int
	Liveness        string // alive|down|unknown ("" => unknown)
}

// PreparationLaneFilter is the veto seam. A filter may refuse a lane BEFORE it becomes
// winner-eligible (capacity walled/dead/expiring — work_697dedbfca03). A refused lane stays
// visible in the ranking with the filter's reason; it is never silently dropped. This packet
// ships with zero filters; the capacity planner plugs its veto here.
type PreparationLaneFilter func(PreparationLaneInput) (refuse bool, reason string)

// PreparationLaneScore is one ranked lane plus every input the ranking relied on (Law 9:
// the receipt carries its own evidence so the routing choice is auditable).
type PreparationLaneScore struct {
	Lane            string  `json:"lane"`
	Label           string  `json:"label,omitempty"`
	ClaimLevel      string  `json:"claim_level,omitempty"`
	FitTier         int     `json:"fit_tier"`
	RoutingPosition int     `json:"routing_position"`
	CostClass       string  `json:"cost_class,omitempty"`
	CostRank        int     `json:"cost_rank"`
	TokensPerSecond float64 `json:"tokens_per_second"`
	ThroughputKnown bool    `json:"throughput_known"`
	ThroughputRuns  int     `json:"throughput_runs"`
	MeetsFloor      bool    `json:"meets_floor"`
	Liveness        string  `json:"liveness"`
	Eligible        bool    `json:"eligible"`
	OmitReason      string  `json:"omit_reason,omitempty"`
	Rationale       string  `json:"rationale"`
}

// PreparationLaneRanking is the recorded selection: the floor it applied, every candidate
// with its inputs, and the winner (or a blocked status when no lane is eligible).
type PreparationLaneRanking struct {
	SchemaVersion        string                 `json:"schema_version"`
	AuthorityRef         string                 `json:"authority_ref"`
	CapabilityClass      string                 `json:"capability_class"`
	FloorTokensPerSecond float64                `json:"floor_tokens_per_second"`
	Status               string                 `json:"status"`
	Candidates           []PreparationLaneScore `json:"candidates"`
	Winner               *PreparationLaneScore  `json:"winner,omitempty"`
	Warnings             []string               `json:"warnings,omitempty"`
}

// RankPreparationLanes ranks candidate lanes for a preparation-class dispatch by, in order,
// (1) capability-class fit, (2) token-generation speed above the floor, (3) cost.
//
// Fit is the coarse gate: primary and capable claims share a fit tier so that measured speed
// can reorder within proven-fit (this is the operator ruling made into a scored dimension —
// a curated top lane no longer wins on position alone); assumed/supervised lanes rank strictly
// below, and lanes with no claim for the class rank last. Unknown throughput is scored as
// below-floor — never assumed fast. A lane whose liveness probe says down, or which a filter
// refuses, is ineligible (never the winner) but stays visible for audit.
func RankPreparationLanes(class string, floor float64, inputs []PreparationLaneInput, filters ...PreparationLaneFilter) PreparationLaneRanking {
	if floor <= 0 {
		floor = DefaultPreparationFloorTokensPerSecond
	}
	ranking := PreparationLaneRanking{
		SchemaVersion:        PreparationLaneRankingSchemaVersion,
		AuthorityRef:         PreparationLaneAuthorityRef,
		CapabilityClass:      strings.TrimSpace(class),
		FloorTokensPerSecond: floor,
		Candidates:           make([]PreparationLaneScore, 0, len(inputs)),
	}
	for _, in := range inputs {
		liveness := NormalizeLiveness(in.Liveness)
		score := PreparationLaneScore{
			Lane:            in.Lane,
			Label:           in.Label,
			ClaimLevel:      in.ClaimLevel,
			FitTier:         claimFitTier(in.ClaimLevel),
			RoutingPosition: in.RoutingPosition,
			CostClass:       in.CostClass,
			CostRank:        costRank(in.CostClass),
			TokensPerSecond: in.TokensPerSecond,
			ThroughputKnown: in.ThroughputKnown,
			ThroughputRuns:  in.ThroughputRuns,
			MeetsFloor:      in.ThroughputKnown && in.TokensPerSecond >= floor,
			Liveness:        liveness,
			Eligible:        true,
		}
		if liveness == LaneLivenessDown {
			score.Eligible = false
			score.OmitReason = "liveness probe reports lane down"
		}
		if score.Eligible {
			for _, f := range filters {
				if f == nil {
					continue
				}
				if refuse, reason := f(in); refuse {
					score.Eligible = false
					if strings.TrimSpace(reason) == "" {
						reason = "refused by filter"
					}
					score.OmitReason = reason
					break
				}
			}
		}
		score.Rationale = preparationRationale(score, floor)
		ranking.Candidates = append(ranking.Candidates, score)
	}
	sortPreparationScores(ranking.Candidates)
	if len(ranking.Candidates) > 0 && ranking.Candidates[0].Eligible {
		winner := ranking.Candidates[0]
		ranking.Winner = &winner
	}
	switch {
	case len(ranking.Candidates) == 0:
		ranking.Status = PreparationLaneStatusNoLanes
	case ranking.Winner == nil:
		ranking.Status = PreparationLaneStatusBlocked
	default:
		ranking.Status = PreparationLaneStatusRanked
	}
	return ranking
}

// sortPreparationScores orders best-first: eligible before ineligible, then fit tier, then
// speed-above-floor, then cost, with deterministic tiebreaks (throughput, routing position,
// lane id).
func sortPreparationScores(scores []PreparationLaneScore) {
	sort.SliceStable(scores, func(i, j int) bool {
		a, b := scores[i], scores[j]
		if a.Eligible != b.Eligible {
			return a.Eligible // eligible first
		}
		if a.FitTier != b.FitTier {
			return a.FitTier > b.FitTier
		}
		if a.MeetsFloor != b.MeetsFloor {
			return a.MeetsFloor // meets-floor first
		}
		if a.CostRank != b.CostRank {
			return a.CostRank < b.CostRank
		}
		if a.TokensPerSecond != b.TokensPerSecond {
			return a.TokensPerSecond > b.TokensPerSecond
		}
		if a.RoutingPosition != b.RoutingPosition {
			return a.RoutingPosition < b.RoutingPosition
		}
		return a.Lane < b.Lane
	})
}

// claimFitTier maps a registry claim level to a coarse fit tier. primary and capable are both
// "proven fit for the class" (tier 2) so speed can reorder within them; assumed is supervised
// (tier 1); anything else has no claim for the class (tier 0).
func claimFitTier(level string) int {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "primary", "capable":
		return 2
	case "assumed":
		return 1
	default:
		return 0
	}
}

// costRank orders cost classes cheapest-first. Unknown cost is treated as mid-rank, never as
// free — a lane is not assumed cheap on missing data.
func costRank(costClass string) int {
	switch strings.ToLower(strings.TrimSpace(costClass)) {
	case "free":
		return 0
	case "local":
		return 1
	case "api-marginal", "cheap":
		return 2
	case "premium", "paid", "subscription-fixed":
		return 3
	default:
		return 2
	}
}

// NormalizeLiveness collapses a liveness string to one of alive|down|unknown.
func NormalizeLiveness(liveness string) string {
	switch strings.ToLower(strings.TrimSpace(liveness)) {
	case LaneLivenessAlive:
		return LaneLivenessAlive
	case LaneLivenessDown:
		return LaneLivenessDown
	default:
		return LaneLivenessUnknown
	}
}

func preparationRationale(s PreparationLaneScore, floor float64) string {
	if !s.Eligible {
		return "ineligible: " + s.OmitReason
	}
	speed := "throughput unknown (scored below floor)"
	if s.ThroughputKnown {
		if s.MeetsFloor {
			speed = fmt.Sprintf("%.1f t/s >= floor %.1f (n=%d)", s.TokensPerSecond, floor, s.ThroughputRuns)
		} else {
			speed = fmt.Sprintf("%.1f t/s < floor %.1f (n=%d)", s.TokensPerSecond, floor, s.ThroughputRuns)
		}
	}
	claim := s.ClaimLevel
	if claim == "" {
		claim = "no-claim"
	}
	return fmt.Sprintf("fit=%d(%s) speed=%s cost=%s", s.FitTier, claim, speed, s.CostClass)
}
