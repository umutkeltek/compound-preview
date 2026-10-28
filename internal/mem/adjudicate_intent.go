// adjudicate_intent.go — extracted pre-lock rule surface for adjudicate().
//
// WHY THIS FILE EXISTS
// ====================
//
// The legacy Store.adjudicate god function (mem.go, ~264 lines) ran six
// distinct concerns through one entry point, with the same authority rules
// stated twice (pre-lock and lock-held) — the in-function comment at
// mem.go:594-596 explicitly admits the duplication is defensive:
//
//	// promote: the prior-lookup + seq allocation + claim/edge append run
//	// under one lock so seq stays monotonic-unique and supersession is
//	// consistent even across concurrent writers (single-writer is the norm;
//	// the lock makes MVCC-by-fold mechanically true, not just a phrase).
//
// The first step of the deepening (the smallest change that proves the
// slice) is to lift the *pre-lock* rule set into a pure function. The
// in-lock re-check stays for now — it carries a side effect (stamping the
// operational-decision authority qualifier on the candidate's qualifier
// map), which makes it non-pure and a separate refactor.
//
// PURE-FUNCTION CONTRACT
// ======================
//
//	func validateAdjudicationIntent(c Candidate, intent AdjudicationIntent) error
//
// MUST NOT:
//   - call any Store method
//   - call any I/O (file, network, time.Now)
//   - mutate the passed Candidate
//   - depend on global mutable state
//
// MUST return a non-nil error if and only if the same intent against the
// same candidate would have failed at the corresponding pre-lock check in
// the legacy adjudicate() function. The order of rules below matches the
// legacy order (verdict → state → operational-decision qualifiers →
// explicit-confirmation) so the in-lock re-check's behavior is unchanged
// when the candidate state is identical between the two passes.
//
// The companion test TestValidateAdjudicationIntentEnforcesEachRuleInIsolation
// (mem_test.go) enforces purity via before/after JSON snapshot comparison
// and asserts each rule with one focused subtest.
package mem

import (
	"fmt"
	"strings"

	"compound/internal/candidates"
)

// AdjudicationIntent is the structured shape an adjudicate() caller submits.
// It carries the (already-validated) intent to apply; the runtime rules
// check it against the current candidate state without ever mutating the
// candidate itself.
//
// Verdict is the verdict (one of Verdicts). WorkID is the optional
// governance work-id. OperationalDecision is true iff this promote is
// minting the reserved operational decision authority stamp (claim_type
// must be "decision", and the policy decisions authority is the only
// legitimate path for ordinary memory candidates that want to). Actor is
// the recorded adjudicator; it is propagated but not validated here.
// ExplicitConfirmationRef, when set, requires the candidate to be parked,
// the verdict to be "promote", and OperationalDecision to be false.
type AdjudicationIntent struct {
	Verdict                 string
	WorkID                  string
	OperationalDecision     bool
	ExplicitConfirmationRef string
	Actor                   string
}

// validateAdjudicationIntent checks that an adjudication intent is
// well-formed for a given candidate, *before* any I/O or lock is taken.
// See package comment for the pure-function contract.
func validateAdjudicationIntent(c Candidate, intent AdjudicationIntent) error {
	if !oneOf(intent.Verdict, Verdicts) {
		return fmt.Errorf("verdict must be one of %v, got %q", Verdicts, intent.Verdict)
	}
	if c.State == "promoted" {
		return fmt.Errorf("candidate %s already promoted", c.ID)
	}
	if c.Qualifiers != nil && strings.TrimSpace(c.Qualifiers[OperationalDecisionAuthorityQualifier]) != "" && !intent.OperationalDecision {
		return fmt.Errorf("candidate %s uses reserved operational decision authority qualifier; import it through the policy decisions authority", c.ID)
	}
	if !intent.OperationalDecision && intent.Verdict == "promote" && isOperationalDecisionKind(c.Qualifiers["kind"]) {
		return fmt.Errorf("candidate %s uses reserved operational decision kind %q; import it through the policy decisions authority", c.ID, c.Qualifiers["kind"])
	}
	if intent.OperationalDecision && intent.Verdict == "promote" {
		if c.ClaimType != "decision" {
			return fmt.Errorf("candidate %s cannot receive operational decision authority: claim_type must be decision", c.ID)
		}
	}
	if intent.ExplicitConfirmationRef != "" {
		if intent.Verdict != "promote" || intent.OperationalDecision {
			return fmt.Errorf("explicit confirmation is only valid for ordinary memory promotion")
		}
	} else if intent.Verdict == "promote" && c.PromotionBasis == candidates.BasisExplicitConfirmation {
		return fmt.Errorf("candidate %s explicit confirmation requires governed confirmation evidence", c.ID)
	}
	return nil
}
