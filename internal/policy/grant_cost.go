package policy

import (
	"fmt"
	"math"

	"compound/internal/envelope"
)

// grantCostMisses treats an explicit zero as no spending, not as a request for
// an unbounded budget. Unspecified estimates keep their existing semantics.
func grantCostMisses(g StandingGrant, cost envelope.EstimatedCost) []string {
	var reasons []string
	id := grantID(g)
	if cost.USD != nil {
		if *cost.USD < 0 || math.IsNaN(*cost.USD) || math.IsInf(*cost.USD, 0) {
			reasons = append(reasons, fmt.Sprintf("standing grant %q requires finite non-negative estimated_cost.usd", id))
		} else if g.MaxUSD != nil && (*g.MaxUSD < 0 || math.IsNaN(*g.MaxUSD) || math.IsInf(*g.MaxUSD, 0)) {
			reasons = append(reasons, fmt.Sprintf("standing grant %q requires finite non-negative max_usd", id))
		} else if g.MaxUSD == nil && *cost.USD != 0 {
			reasons = append(reasons, fmt.Sprintf("standing grant %q does not set max_usd for estimated_cost.usd %.2f", id, *cost.USD))
		} else if g.MaxUSD != nil && *cost.USD > *g.MaxUSD {
			reasons = append(reasons, fmt.Sprintf("standing grant %q estimated_cost.usd %.2f exceeds max_usd %.2f", id, *cost.USD, *g.MaxUSD))
		}
	}
	if cost.Tokens != nil {
		if *cost.Tokens < 0 {
			reasons = append(reasons, fmt.Sprintf("standing grant %q requires non-negative estimated_cost.tokens", id))
		} else if g.MaxTokens != nil && *g.MaxTokens < 0 {
			reasons = append(reasons, fmt.Sprintf("standing grant %q requires non-negative max_tokens", id))
		} else if g.MaxTokens == nil && *cost.Tokens != 0 {
			reasons = append(reasons, fmt.Sprintf("standing grant %q does not set max_tokens for estimated_cost.tokens %d", id, *cost.Tokens))
		} else if g.MaxTokens != nil && *cost.Tokens > *g.MaxTokens {
			reasons = append(reasons, fmt.Sprintf("standing grant %q estimated_cost.tokens %d exceeds max_tokens %d", id, *cost.Tokens, *g.MaxTokens))
		}
	}
	return reasons
}
