// Package routeev composes the EV card (compound.route.evcard.v0.1): a deterministic PURE
// function that prices a served-lane deviation in one commensurable unit — expected
// cost-to-success, EV(lane) = cost_per_attempt(lane) / posterior(lane, class) — and classifies
// it against the policy line's own EV as priced_right, overpaid, dominated, or unpriceable.
//
// This package does NO I/O, reads NO clock, and touches NO store (capgraph, lanebeliefs,
// eventstore, ledger). Every input — costs, posteriors, evidence counts, belief refs, ledger
// window, risk grade, route-plan ref, computed-from revision — is injected by the caller. S2
// (the `compound route ev` CLI, not yet built) is the only place that resolves those inputs
// from the real doors; this package never imports them, so a wiring mistake elsewhere can never
// make the composer develop a second belief or routing opinion of its own. Compose is the only
// entry point and it is a straight function of its argument: same ComposeInput in, byte-
// identical Card out, every time, on every machine, forever.
//
// Unit law (spec docs_private/plan/ev-card-spec.md §2): the card prices in exactly ONE
// commensurable unit per capability class — expected cost-to-success — never a fabricated
// cross-dimension utility scalar. A lane whose cost or posterior is missing or unusable makes
// the whole card refuse rather than guess (verdict unpriceable); it never invents a number.
//
// Verdict truth table (spec §3):
//
//	priced_right  posterior(deviation) >= required_posterior
//	overpaid      posterior(deviation) <  required_posterior, and required_posterior <= 1
//	dominated     required_posterior > 1 — no evidence could ever justify the deviation
//	              (the knight-vs-bishop case: required posterior exceeds 1 — certainty cannot
//	              justify the deviation)
//	unpriceable   a required input (cost_per_attempt or posterior) is missing or out of range
//	              on either lane; the card NAMES every missing input in missing_inputs and
//	              fabricates no ev/required_posterior number for the lane(s) that lack one
//
// required_posterior p* = cost_per_attempt(deviation) / ev(policy_line) — the posterior the
// deviation would need, at its own cost, to match the policy line's expected cost-to-success.
//
// Determinism (§7 S1 depth tests): the only non-obvious source of nondeterminism in a pure
// function over scalar inputs is slice-input ORDER — belief_refs. Compose sorts a defensive
// copy of each lane's belief_refs before echoing them back, so citing the same refs in a
// different order never changes the marshaled bytes. Duplicates are preserved as given —
// this composer orders refs, it does not interpret or deduplicate their semantics. Nothing
// here iterates a map, and computed_from_revision is caller-supplied, never clock-derived, so
// two calls with equal inputs produce equal output down to the byte.
package routeev

import "sort"

// SchemaVersion is the card's schema tag, stamped verbatim into every Card. Bump it (v0.2, …)
// only when the card SHAPE changes; the composer logic can change independently.
const SchemaVersion = "compound.route.evcard.v0.1"

// Verdict is the card's classification of the deviation against the policy line. It is a closed
// four-value enum; Compose returns exactly one of these strings, never anything else.
const (
	VerdictPricedRight = "priced_right"
	VerdictOverpaid    = "overpaid"
	VerdictDominated   = "dominated"
	VerdictUnpriceable = "unpriceable"
)

// riskGradeJudge is the one risk_grade value this composer interprets (spec §4: a judge-grade
// deviation MUST carry a staked WorkPrediction). risk_grade is otherwise a caller-supplied
// pass-through string this package does not validate against attention.RiskGrade's enum — S1
// stays free of that import so it cannot grow a second opinion about what grades exist.
const riskGradeJudge = "judge"

// LaneInput is one lane's priced evidence, entirely as the caller supplies it. CostPerAttempt
// and Posterior are pointers because BOTH are optional: a lane the caller cannot yet price
// (no ledger import, no lanebeliefs posterior) supplies nil rather than a fabricated 0 or 1,
// and Compose reports that absence by name instead of guessing. Attempts and Completions are
// the RAW evidence counts behind Posterior — carried so the card can show a 2/2-Beta-prior
// posterior as visibly prior-only (0 attempts) rather than reading it as corroborated (spec
// §8.3): they are plain ints, not pointers, because "no evidence yet" (0, 0) is itself a
// meaningful, always-representable value, unlike a cost or a posterior.
type LaneInput struct {
	// LaneID identifies the lane (the harness×route×model×effort×budget tuple's external name).
	LaneID string
	// CostPerAttempt is the ledger-derived cost of one attempt on this lane, in the capability
	// class's ledger currency/unit. Nil when no ledger window has priced this lane yet.
	CostPerAttempt *float64
	// Posterior is p_success for this lane in this capability class — a lanebeliefs Beta
	// posterior mean, or nil when no belief exists yet for this (lane, class) pair.
	Posterior *float64
	// Attempts and Completions are the raw receipt counts the posterior above was folded from
	// (0, 0 for a bare Beta prior with no reinforcing evidence). They are display evidence only:
	// Compose does not gate on them, does not recompute Posterior from them, and does not enforce
	// MIN_LANE_RECEIPTS — that floor belongs to the belief producer (internal/lanebeliefs), not
	// to this composer.
	Attempts    int
	Completions int
	// BeliefRefs cites the belief(s) Posterior came from. Order does not affect the composed
	// card — see the package doc's determinism note.
	BeliefRefs []string
	// LedgerWindow names the window CostPerAttempt was computed over (e.g. a fixed N-day window
	// or a receipt-count window — spec §8.2 leaves the choice to the caller/S2; this composer
	// only echoes whatever string it is given, uninterpreted).
	LedgerWindow string
}

// LaneCardInputs echoes the citation inputs behind a lane's priced numbers, in normalized form.
type LaneCardInputs struct {
	// BeliefRefs is in.BeliefRefs sorted lexicographically (see package doc: determinism). Nil
	// when the caller supplied none.
	BeliefRefs []string `json:"belief_refs,omitempty"`
	// LedgerWindow is echoed verbatim from the input.
	LedgerWindow string `json:"ledger_window,omitempty"`
}

// LaneCard is one lane's priced view inside the composed Card. EV is nil whenever that lane's
// own CostPerAttempt or Posterior was missing or out of range — a lane never gets a fabricated
// EV just because its counterpart lane is fully priced (each lane's EV depends only on its own
// two numbers).
type LaneCard struct {
	LaneID string `json:"lane_id"`
	// EV is cost_per_attempt / posterior for this lane, or nil if either input was missing or
	// out of range (see missing_inputs on the enclosing Card for which one).
	EV *float64 `json:"ev,omitempty"`
	// CostPerAttempt and Posterior are echoed exactly as given (even if out of range — this
	// composer never edits or clamps a caller's number, it only declines to compute EV from an
	// unusable one). Nil means the caller supplied none.
	CostPerAttempt *float64 `json:"cost_per_attempt,omitempty"`
	Posterior      *float64 `json:"posterior,omitempty"`
	// Attempts and Completions are echoed from the input unchanged, so a reader can see at a
	// glance whether Posterior rests on real evidence or is still a bare Beta prior.
	Attempts    int            `json:"attempts"`
	Completions int            `json:"completions"`
	Inputs      LaneCardInputs `json:"inputs"`
}

// ComposeInput is the full set of caller-supplied inputs for one EV card. Nothing in this
// struct is optional at the Go level except the two LaneInput pointer fields (cost, posterior);
// every other field is a plain scalar the caller must decide (even if that decision is "" or
// false) — Compose has no defaults to fall back to, by design (a pure function has no clock and
// no config to consult).
type ComposeInput struct {
	CapabilityClass string
	PolicyLine      LaneInput
	Deviation       LaneInput
	// RiskGrade is a caller-supplied pass-through string (attention.RiskGrade's value, e.g.
	// "judge", "gate", "auto" — this package does not import or validate that enum). The single
	// value it interprets is "judge", to set PredictionRequired (spec §4).
	RiskGrade string
	// RoutePlanRef cites the `route plan` resolution the policy line came from (S2 resolves the
	// policy lane via that SAME path — this package takes the ref as given and does not verify
	// it, so it cannot become a second routing opinion).
	RoutePlanRef string
	// ComputedFromRevision names the source revision (belief/ledger state) this card was
	// computed against. Caller-supplied, never clock-derived — see package doc: determinism.
	ComputedFromRevision string
}

// Card is the composed EV card, compound.route.evcard.v0.1. Field order matches spec §3's
// example exactly, with one addition: MissingInputs, required to satisfy the unpriceable
// verdict's naming obligation (spec §2, §7 S1 depth test) — the example in §3 depicts a priced
// card, not an unpriceable one, so it has no occasion to show that field.
type Card struct {
	SchemaVersion   string   `json:"schema_version"`
	CapabilityClass string   `json:"capability_class"`
	PolicyLine      LaneCard `json:"policy_line"`
	Deviation       LaneCard `json:"deviation"`
	// RequiredPosterior is p* = cost_per_attempt(deviation) / ev(policy_line). Nil whenever
	// Verdict is unpriceable — p* is only meaningful once every input it depends on (both
	// lanes' costs and posteriors) is present and in range.
	RequiredPosterior *float64 `json:"required_posterior,omitempty"`
	// Verdict is exactly one of VerdictPricedRight, VerdictOverpaid, VerdictDominated,
	// VerdictUnpriceable.
	Verdict              string `json:"verdict"`
	RiskGrade            string `json:"risk_grade,omitempty"`
	PredictionRequired   bool   `json:"prediction_required"`
	RoutePlanRef         string `json:"route_plan_ref,omitempty"`
	ComputedFromRevision string `json:"computed_from_revision,omitempty"`
	// MissingInputs names, as dotted "<lane>.<field>" paths, every required input that was
	// missing or out of range. Populated ONLY when Verdict is unpriceable; empty (omitted from
	// JSON) otherwise. This is the field that lets the card refuse instead of guess: never a
	// fabricated number, always a named gap.
	MissingInputs []string `json:"missing_inputs,omitempty"`
}

// Compose builds the EV card for one (policy_line, deviation) pair. It is a pure function: no
// I/O, no clock, no store reads — see the package doc.
func Compose(in ComposeInput) Card {
	var missing []string

	policyCost, policyCostOK := validPositive(in.PolicyLine.CostPerAttempt)
	if !policyCostOK {
		missing = append(missing, "policy_line.cost_per_attempt")
	}
	policyPosterior, policyPosteriorOK := validProbability(in.PolicyLine.Posterior)
	if !policyPosteriorOK {
		missing = append(missing, "policy_line.posterior")
	}
	deviationCost, deviationCostOK := validPositive(in.Deviation.CostPerAttempt)
	if !deviationCostOK {
		missing = append(missing, "deviation.cost_per_attempt")
	}
	deviationPosterior, deviationPosteriorOK := validProbability(in.Deviation.Posterior)
	if !deviationPosteriorOK {
		missing = append(missing, "deviation.posterior")
	}

	var policyEV, deviationEV *float64
	if policyCostOK && policyPosteriorOK {
		v := policyCost / policyPosterior
		policyEV = &v
	}
	if deviationCostOK && deviationPosteriorOK {
		v := deviationCost / deviationPosterior
		deviationEV = &v
	}

	card := Card{
		SchemaVersion:        SchemaVersion,
		CapabilityClass:      in.CapabilityClass,
		PolicyLine:           composeLaneCard(in.PolicyLine, policyEV),
		Deviation:            composeLaneCard(in.Deviation, deviationEV),
		RiskGrade:            in.RiskGrade,
		PredictionRequired:   in.RiskGrade == riskGradeJudge,
		RoutePlanRef:         in.RoutePlanRef,
		ComputedFromRevision: in.ComputedFromRevision,
	}

	if len(missing) > 0 {
		// Unpriceable: name every gap, fabricate nothing. required_posterior needs BOTH lanes'
		// cost+posterior, so any single missing input already makes it unrecoverable — no partial
		// p* is computed or shown.
		card.Verdict = VerdictUnpriceable
		card.MissingInputs = missing
		return card
	}

	// Every input is present and in range past this point: policyEV is non-nil by construction.
	requiredPosterior := deviationCost / *policyEV
	card.RequiredPosterior = &requiredPosterior

	switch {
	case requiredPosterior > 1:
		// knight-vs-bishop case: certainty (posterior 1.0) still would not reach p*, so no
		// evidence could ever justify the deviation.
		card.Verdict = VerdictDominated
	case deviationPosterior >= requiredPosterior:
		card.Verdict = VerdictPricedRight
	default:
		card.Verdict = VerdictOverpaid
	}
	return card
}

// composeLaneCard builds one lane's output view. ev is whatever Compose already decided for
// this lane (nil if that lane's own inputs were incomplete) — composeLaneCard does not
// recompute it, so the displayed ev and any required_posterior derived from it are guaranteed
// to agree with each other bit-for-bit.
func composeLaneCard(in LaneInput, ev *float64) LaneCard {
	return LaneCard{
		LaneID:         in.LaneID,
		EV:             ev,
		CostPerAttempt: in.CostPerAttempt,
		Posterior:      in.Posterior,
		Attempts:       in.Attempts,
		Completions:    in.Completions,
		Inputs: LaneCardInputs{
			BeliefRefs:   sortedCopy(in.BeliefRefs),
			LedgerWindow: in.LedgerWindow,
		},
	}
}

// validPositive reports whether v is present and a usable cost (> 0). A nil or non-positive
// cost cannot price a lane (and 0/negative would either divide-by-zero or invert verdict logic
// nonsensically), so both are treated as "missing" for pricing purposes — the raw value is
// still echoed back on the LaneCard unchanged (composeLaneCard), only EV computation refuses it.
func validPositive(v *float64) (float64, bool) {
	if v == nil || *v <= 0 {
		return 0, false
	}
	return *v, true
}

// validProbability reports whether v is present and a usable posterior (0, 1]. Zero is refused
// (division by a zero posterior is undefined and JSON cannot marshal the resulting Inf/NaN);
// anything above 1 is not a probability. As with cost, the raw value is still echoed back on the
// LaneCard unchanged — only EV computation refuses it.
func validProbability(v *float64) (float64, bool) {
	if v == nil || *v <= 0 || *v > 1 {
		return 0, false
	}
	return *v, true
}

// sortedCopy returns refs sorted lexicographically in a fresh slice, or nil for an empty input.
// This is the ONE normalization Compose applies to any input (package doc: determinism) — it
// exists solely so that citing the same belief refs in a different order can never change the
// marshaled card. Duplicates are preserved as given.
func sortedCopy(refs []string) []string {
	if len(refs) == 0 {
		return nil
	}
	out := make([]string, len(refs))
	copy(out, refs)
	sort.Strings(out)
	return out
}
