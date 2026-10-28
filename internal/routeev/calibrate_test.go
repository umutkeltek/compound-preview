package routeev

import "testing"

// TestCalibrateDivergenceTable proves the FULL divergence table (spec §5, §7 S4 depth test), all
// cells: every one of the four Verdict values crossed with every one of the three ActualOutcome
// values. failed and cancelled must behave IDENTICALLY everywhere (the table never distinguishes
// them — only "did the work complete" changes the verdict), and unpriceable must NEVER diverge,
// for any actual outcome, including completed.
func TestCalibrateDivergenceTable(t *testing.T) {
	tests := []struct {
		name         string
		verdict      string
		outcome      string
		wantDiverged bool
		wantReason   string
	}{
		{"priced_right+completed", VerdictPricedRight, ActualOutcomeCompleted, false, ReasonCardVindicated},
		{"priced_right+failed", VerdictPricedRight, ActualOutcomeFailed, true, ReasonDeviationDidNotPay},
		{"priced_right+cancelled", VerdictPricedRight, ActualOutcomeCancelled, true, ReasonDeviationDidNotPay},

		{"overpaid+completed", VerdictOverpaid, ActualOutcomeCompleted, true, ReasonCardUnderpriced},
		{"overpaid+failed", VerdictOverpaid, ActualOutcomeFailed, false, ReasonCardVindicated},
		{"overpaid+cancelled", VerdictOverpaid, ActualOutcomeCancelled, false, ReasonCardVindicated},

		{"dominated+completed", VerdictDominated, ActualOutcomeCompleted, true, ReasonCertificateContradicted},
		{"dominated+failed", VerdictDominated, ActualOutcomeFailed, false, ReasonCardVindicated},
		{"dominated+cancelled", VerdictDominated, ActualOutcomeCancelled, false, ReasonCardVindicated},

		{"unpriceable+completed", VerdictUnpriceable, ActualOutcomeCompleted, false, ReasonUnpriceableNoClaim},
		{"unpriceable+failed", VerdictUnpriceable, ActualOutcomeFailed, false, ReasonUnpriceableNoClaim},
		{"unpriceable+cancelled", VerdictUnpriceable, ActualOutcomeCancelled, false, ReasonUnpriceableNoClaim},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Calibrate(Card{Verdict: tc.verdict}, tc.outcome)
			if got.Diverged != tc.wantDiverged {
				t.Errorf("Diverged = %v, want %v", got.Diverged, tc.wantDiverged)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if got.Verdict != tc.verdict {
				t.Errorf("Verdict = %q, want %q (must echo the card's verdict)", got.Verdict, tc.verdict)
			}
			if got.ActualOutcome != tc.outcome {
				t.Errorf("ActualOutcome = %q, want %q (must echo verbatim)", got.ActualOutcome, tc.outcome)
			}
		})
	}
}

// TestCalibrateDominatedIsLoudestNotOverpaid proves dominated+completed gets a DISTINCT reason
// from overpaid+completed — the spec calls out dominated's contradiction as "worth flagging
// loudest", which only means something if it is mechanically distinguishable from a plain
// overpaid miss, not merely folded into the same code.
func TestCalibrateDominatedIsLoudestNotOverpaid(t *testing.T) {
	overpaid := Calibrate(Card{Verdict: VerdictOverpaid}, ActualOutcomeCompleted)
	dominated := Calibrate(Card{Verdict: VerdictDominated}, ActualOutcomeCompleted)
	if overpaid.Reason == dominated.Reason {
		t.Fatalf("overpaid and dominated completions must not share a reason code, both got %q", overpaid.Reason)
	}
	if !overpaid.Diverged || !dominated.Diverged {
		t.Fatalf("both overpaid+completed and dominated+completed must diverge: overpaid=%v dominated=%v", overpaid.Diverged, dominated.Diverged)
	}
}

// TestCalibrateUnpriceableNeverDivergesRegardlessOfOutcome proves unpriceable is its own explicit
// case, not a default that happens to fall out of the completed/not-completed split — even an
// outcome string outside the named three-value domain must not diverge an unpriceable card.
func TestCalibrateUnpriceableNeverDivergesRegardlessOfOutcome(t *testing.T) {
	for _, outcome := range []string{ActualOutcomeCompleted, ActualOutcomeFailed, ActualOutcomeCancelled, "", "unknown_outcome"} {
		got := Calibrate(Card{Verdict: VerdictUnpriceable}, outcome)
		if got.Diverged {
			t.Errorf("unpriceable + outcome %q diverged, want never diverged", outcome)
		}
		if got.Reason != ReasonUnpriceableNoClaim {
			t.Errorf("unpriceable + outcome %q reason = %q, want %q", outcome, got.Reason, ReasonUnpriceableNoClaim)
		}
	}
}

// TestCalibrateDeterministic proves Calibrate is a pure function: the same (card, outcome) pair
// produces a byte-identical result every time, and the composed Card's other fields (which
// Calibrate never reads) do not perturb it.
func TestCalibrateDeterministic(t *testing.T) {
	full := Compose(ComposeInput{
		CapabilityClass: "judge-critique",
		PolicyLine: LaneInput{
			LaneID: "policy", CostPerAttempt: f64p(0.037), Posterior: f64p(0.90),
		},
		Deviation: LaneInput{
			LaneID: "deviation", CostPerAttempt: f64p(0.029), Posterior: f64p(0.50),
		},
		RiskGrade:            "judge",
		RoutePlanRef:         "route_plan_ref_1",
		ComputedFromRevision: "rev_1",
	})

	a := Calibrate(full, ActualOutcomeFailed)
	b := Calibrate(full, ActualOutcomeFailed)
	if a != b {
		t.Fatalf("Calibrate is not deterministic: %+v vs %+v", a, b)
	}

	bare := Card{Verdict: full.Verdict}
	c := Calibrate(bare, ActualOutcomeFailed)
	if a.Diverged != c.Diverged || a.Reason != c.Reason {
		t.Fatalf("Calibrate must depend on Verdict alone, not the rest of the Card: full=%+v bare=%+v", a, c)
	}
}

func f64p(v float64) *float64 { return &v }
