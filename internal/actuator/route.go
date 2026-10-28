package actuator

import (
	"fmt"
	"sort"
	"strings"

	"compound/internal/envelope"
)

// DecisionMode is where the actuator sends a known work's next move.
//
//	auto — execute now, unattended, through the existing harness/act seams.
//	hold — surface a bounded instruction card for a human; the actuator never
//	       touches an irreversible/public/financial/budget-exhausted move.
//	deny — policy refused outright; nothing executes and nothing is queued.
type DecisionMode string

const (
	ModeAuto DecisionMode = "auto"
	ModeHold DecisionMode = "hold"
	ModeDeny DecisionMode = "deny"
)

// Decision is the actuator's routing verdict for one ActionIntent. It carries
// the reasons (so a hold/deny is a plain-language card, not a silent refusal —
// the "decision homework" failure class) and the harness to dispatch through
// when auto.
type Decision struct {
	Mode          DecisionMode `json:"mode"`
	Reasons       []string     `json:"reasons"`
	Channel       string       `json:"channel,omitempty"`
	Harness       string       `json:"harness,omitempty"`
	PolicyOutcome string       `json:"policy_outcome"`
	Headroom      *Headroom    `json:"headroom,omitempty"`
}

// autoMutationClasses are the ONLY mutation classes the actuator will execute
// unattended. Every other class (external_side_effect, public, destructive,
// financial, security) is held for a human by construction — this is the
// structural half of "execute within limits", independent of what the budget
// says. A wide-open budget can never open one of these.
var autoMutationClasses = map[string]bool{
	"read":             true,
	"local_reversible": true,
}

// autoReversibilities are the ONLY reversibility levels the actuator executes
// unattended. Even a local_reversible move that is only "manual" or "hard" to
// reverse is held — unattended execution is reserved for moves the system can
// undo cheaply if they were wrong.
var autoReversibilities = map[string]bool{
	"easy": true,
}

// Route is the actuator's classifier. It is a pure function of three already-
// authoritative inputs — the ActionIntent's declared mutation_class +
// reversibility, the PolicyDecision outcome, and the budget headroom — so it
// owns no authority and re-judges nothing: it only decides whether an already-
// evaluated move may FLOW (auto) or must be HELD for a human.
//
// Precedence (fail-closed at every step):
//  1. policy deny          -> deny
//  2. policy hold          -> hold (policy already judged a human is needed)
//  3. budget not allowed   -> hold (limits exhausted / undeclared channel)
//  4. class not auto-safe  -> hold (external/public/financial/...)
//  5. reversibility !easy  -> hold (can't cheaply undo)
//  6. otherwise            -> auto
func Route(intent envelope.ActionIntent, policyOutcome string, headroom Headroom) Decision {
	d := Decision{
		PolicyOutcome: policyOutcome,
		Headroom:      &headroom,
	}
	switch policyOutcome {
	case "deny":
		d.Mode = ModeDeny
		d.Reasons = append(d.Reasons, "policy denied this action outright")
		return d
	case "hold":
		d.Mode = ModeHold
		d.Reasons = append(d.Reasons, "policy held this action for operator judgment")
		return d
	case "allow", "allow_with_obligations":
		// fall through to the actuator's own safety gates
	default:
		d.Mode = ModeHold
		d.Reasons = append(d.Reasons, fmt.Sprintf("unknown policy outcome %q — holding fail-closed", policyOutcome))
		return d
	}

	// Policy allows. Now the actuator's structural safety gates decide whether
	// an allowed move may run UNATTENDED or must still be held for a human.
	var holds []string
	if !headroom.Allowed {
		holds = append(holds, "budget: "+strings.Join(headroom.Reasons, "; "))
	}
	if !autoMutationClasses[intent.MutationClass] {
		holds = append(holds, fmt.Sprintf("mutation_class %q is not in the auto-execute set %s — external/public/financial moves stay with a human", intent.MutationClass, sortedKeys(autoMutationClasses)))
	}
	if intent.MutationClass != "read" && !autoReversibilities[intent.Reversibility] {
		holds = append(holds, fmt.Sprintf("reversibility %q is not easy — only cheaply-reversible moves run unattended", intent.Reversibility))
	}

	if len(holds) > 0 {
		d.Mode = ModeHold
		d.Reasons = append(d.Reasons, holds...)
		return d
	}
	d.Mode = ModeAuto
	d.Reasons = append(d.Reasons, fmt.Sprintf("policy %s, class %q, reversibility %q, budget headroom ok — safe to execute unattended", policyOutcome, intent.MutationClass, intent.Reversibility))
	return d
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
