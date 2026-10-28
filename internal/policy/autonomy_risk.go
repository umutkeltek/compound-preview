package policy

// The autonomous-admission risk frontier is intentionally stricter than the
// ordinary policy evaluator. A StandingGrant may allow an action inside its
// envelope, but it may never make public, destructive, financial, security,
// irreversible, or wide-blast work eligible for model-driven admission.

import (
	"fmt"
	"strings"
	"time"

	"compound/internal/envelope"
)

const (
	AutonomyRiskAuthorityRef        = "compound://policy/autonomy-operator-frontier"
	AutonomyEligibilityAuthorityRef = "compound://artifacts/autonomy-eligibility"
)

var autonomyOperatorOnlyMutationClasses = map[string]bool{
	"external_side_effect": true,
	"public":               true,
	"destructive":          true,
	"financial":            true,
	"security":             true,
}

var autonomyOperatorOnlyReversibilities = map[string]bool{
	"irreversible": true,
	"hard":         true,
	"none":         true,
}

// AutonomyOperatorOnlyReason returns the first non-overridable reason an
// ActionIntent cannot be admitted autonomously. The built-in wide radii are
// always enforced; config may widen that frontier but cannot narrow it.
func AutonomyOperatorOnlyReason(ai envelope.ActionIntent, cfg Config) string {
	if autonomyOperatorOnlyMutationClasses[strings.TrimSpace(ai.MutationClass)] {
		return fmt.Sprintf("mutation_class %q is operator-only for autonomous admission", ai.MutationClass)
	}
	if autonomyOperatorOnlyReversibilities[strings.TrimSpace(ai.Reversibility)] {
		return fmt.Sprintf("reversibility %q is operator-only for autonomous admission", ai.Reversibility)
	}
	wide := map[string]bool{}
	for _, value := range DefaultConfig().WideBlastRadii {
		wide[value] = true
	}
	for _, value := range cfg.WideBlastRadii {
		wide[strings.TrimSpace(value)] = true
	}
	if wide[strings.TrimSpace(ai.BlastRadius)] {
		return fmt.Sprintf("blast_radius %q is operator-only for autonomous admission", ai.BlastRadius)
	}
	return ""
}

func AutonomyRiskDenyDecision(ai envelope.ActionIntent, reason string, now time.Time) envelope.PolicyDecision {
	pd := mkDecision(ai, "deny", []string{reason}, nil, "", "", now)
	pd.EvidenceRefs = mergeStrings(pd.EvidenceRefs, AutonomyRiskAuthorityRef)
	return pd
}

func AutonomyEligibilityDenyDecision(ai envelope.ActionIntent, reason string, now time.Time) envelope.PolicyDecision {
	pd := mkDecision(ai, "deny", []string{reason}, nil, "", "", now)
	pd.EvidenceRefs = mergeStrings(pd.EvidenceRefs, AutonomyEligibilityAuthorityRef)
	return pd
}
