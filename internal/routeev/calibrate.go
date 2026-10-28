// Calibrate closes the feedback half of the EV card (spec docs_private/plan/ev-card-spec.md §5,
// §7 S4): it checks a composed Card's verdict against what the deviation's work actually closed
// as, and reports whether the verdict's implicit stake held ("vindicated") or was contradicted
// ("diverged"). Like Compose, Calibrate does NO I/O, reads NO clock, and touches NO store — a
// straight function of a Card and an actual outcome string, byte-identical on every call for the
// same inputs. internal/mem's evcard_calibration.go is the only consumer that turns a diverged
// CalibrationResult into observed-class exhaust; this file never writes anything and never mints.
package routeev

// ActualOutcome* names the close-outcome domain the spec closes over: actualOutcome ∈
// completed|failed|cancelled — the same three-value domain internal/work's CloseInput.Outcome /
// WorkPrediction.Outcome owns (envelope.CloseOutcomes). Calibrate does not import that enum and
// does not validate actualOutcome against it: it trusts the caller-supplied string as a pass-
// through and interprets exactly one value, ActualOutcomeCompleted, the same way Compose (above,
// riskGradeJudge) interprets exactly one value out of RiskGrade's larger caller-owned domain —
// this package stays free of that import so it cannot grow a second opinion about what outcomes
// exist. Any string other than ActualOutcomeCompleted is treated as "did not complete", which
// matches the spec's divergence table: it never distinguishes failed from cancelled, only
// "did the work complete" changes the verdict.
const (
	ActualOutcomeCompleted = "completed"
	ActualOutcomeFailed    = "failed"
	ActualOutcomeCancelled = "cancelled"
)

// Reason is a closed, mechanical explanation for a CalibrationResult — never free text, so a
// downstream reader (the observed-candidate statement, a future audit) can switch on it
// exhaustively instead of parsing prose.
const (
	// ReasonDeviationDidNotPay: verdict was priced_right (the deviation looked worth its price on
	// current evidence) but the work did not complete — the priced deviation did not pay.
	ReasonDeviationDidNotPay = "priced_deviation_did_not_pay"
	// ReasonCardUnderpriced: verdict was overpaid (not yet justified by evidence, but not
	// impossible either) and the work completed anyway — the card underpriced the lane.
	ReasonCardUnderpriced = "card_underpriced_lane"
	// ReasonCertificateContradicted: verdict was dominated (required_posterior > 1 — no evidence
	// could ever have justified the deviation) and the work completed anyway. Kept as its OWN
	// reason rather than folded into ReasonCardUnderpriced on purpose: a dominated verdict staked
	// the strongest possible claim ("no evidence justifies this"), so its contradiction is the
	// loudest divergence the table can produce and a downstream reader must be able to tell it
	// apart from a merely-overpaid miss.
	ReasonCertificateContradicted = "card_certificate_contradicted"
	// ReasonCardVindicated: the outcome matched what the verdict staked — priced_right +
	// completed, or overpaid/dominated + not-completed. Not a divergence.
	ReasonCardVindicated = "card_vindicated"
	// ReasonUnpriceableNoClaim: verdict was unpriceable — the card staked no claim at all, so no
	// outcome can ever diverge from it. Its own explicit case (see Calibrate), never a fallthrough
	// default.
	ReasonUnpriceableNoClaim = "unpriceable_no_claim"
)

// CalibrationResult is Calibrate's output: whether the card's verdict diverged from what actually
// happened, plus enough closed-vocabulary detail for a caller to render or file it without
// re-deriving anything from the Card it was computed against.
type CalibrationResult struct {
	// Diverged is true exactly when the verdict's implicit stake was contradicted by the actual
	// outcome (see the divergence table on Calibrate). False both for a vindicated card AND for
	// an unpriceable one — unpriceable can never diverge, it staked no claim.
	Diverged bool `json:"diverged"`
	// Verdict echoes the Card's verdict this result was computed against — one of
	// VerdictPricedRight, VerdictOverpaid, VerdictDominated, VerdictUnpriceable.
	Verdict string `json:"verdict"`
	// ActualOutcome echoes the caller-supplied outcome verbatim.
	ActualOutcome string `json:"actual_outcome"`
	// Reason is a closed mechanical code — one of the Reason* constants above, never free text.
	Reason string `json:"reason"`
}

// Calibrate compares a composed Card's verdict against the deviation's actual close outcome and
// reports whether the card was vindicated or contradicted. It is a pure function: the same
// (card.Verdict, actualOutcome) pair always produces a byte-identical CalibrationResult.
//
// Divergence table (spec §5, exhaustive over the four verdicts × completed-or-not):
//
//	priced_right + completed           -> not diverged (card_vindicated)
//	priced_right + failed or cancelled -> DIVERGED     (priced_deviation_did_not_pay)
//	overpaid     + completed           -> DIVERGED     (card_underpriced_lane)
//	overpaid     + failed or cancelled -> not diverged (card_vindicated)
//	dominated    + completed           -> DIVERGED     (card_certificate_contradicted — loudest)
//	dominated    + failed or cancelled -> not diverged (card_vindicated)
//	unpriceable  + any outcome         -> NEVER diverged (unpriceable_no_claim)
//
// unpriceable is checked and returned FIRST, as its own explicit case, so it can never fall
// through into the completed/not-completed branch below by accident (spec: "encode this as its
// own explicit case, not a default"). The remaining three verdicts are a closed switch over
// Compose's own Verdict* enum — Calibrate is only ever called on a Card Compose produced, so no
// further default case is reachable.
func Calibrate(card Card, actualOutcome string) CalibrationResult {
	result := CalibrationResult{
		Verdict:       card.Verdict,
		ActualOutcome: actualOutcome,
	}

	if card.Verdict == VerdictUnpriceable {
		result.Reason = ReasonUnpriceableNoClaim
		return result
	}

	completed := actualOutcome == ActualOutcomeCompleted

	switch card.Verdict {
	case VerdictPricedRight:
		if completed {
			result.Reason = ReasonCardVindicated
		} else {
			result.Diverged = true
			result.Reason = ReasonDeviationDidNotPay
		}
	case VerdictOverpaid:
		if completed {
			result.Diverged = true
			result.Reason = ReasonCardUnderpriced
		} else {
			result.Reason = ReasonCardVindicated
		}
	case VerdictDominated:
		if completed {
			result.Diverged = true
			result.Reason = ReasonCertificateContradicted
		} else {
			result.Reason = ReasonCardVindicated
		}
	}
	return result
}
