package routeev

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func ptr(v float64) *float64 { return &v }

// sampleFullyPricedInput is a baseline fully-priced input reused by the depth tests below (which
// care about determinism / ref-order, not about a specific verdict).
func sampleFullyPricedInput() ComposeInput {
	return ComposeInput{
		CapabilityClass: "judge-critique",
		PolicyLine: LaneInput{
			LaneID:         "lane_policy_a",
			CostPerAttempt: ptr(0.5),
			Posterior:      ptr(0.5),
			Attempts:       20,
			Completions:    10,
			BeliefRefs:     []string{"belief_a", "belief_b", "belief_c"},
			LedgerWindow:   "receipts:200",
		},
		Deviation: LaneInput{
			LaneID:         "lane_dev_x",
			CostPerAttempt: ptr(0.25),
			Posterior:      ptr(0.5),
			Attempts:       6,
			Completions:    3,
			BeliefRefs:     []string{"belief_x", "belief_y"},
			LedgerWindow:   "receipts:12",
		},
		RiskGrade:            "judge",
		RoutePlanRef:         "rp_test_sample",
		ComputedFromRevision: "rev_test_sample",
	}
}

// TestCompose_GoldenFixtures covers the three cases spec §7 S1 mandates: dominated, unpriceable,
// boundary. Every fixture uses dyadic (power-of-two) cost/posterior values on purpose, so every
// division in the composer is exact float64 arithmetic — no fixture here depends on decimal
// rounding, which would make the golden JSON fragile to unrelated Go/runtime changes.
//
// Ledger-window convention (spec §8.2's open question, decided here for S1's own fixtures): a
// receipts-count window, written "receipts:N". This composer does not interpret the string either
// way (LedgerWindow is opaque) — this is only the convention S1's fixtures pick for concreteness.
func TestCompose_GoldenFixtures(t *testing.T) {
	cases := []struct {
		name string
		in   ComposeInput

		wantVerdict       string
		wantRequired      *float64
		wantMissing       []string
		wantPredictionReq bool
	}{
		{
			// Dominated: the deviation's best possible case (posterior 1.0, i.e. certainty) still
			// cannot reach required_posterior — required_posterior exceeds 1, so certainty cannot
			// justify the deviation. This is the knight-vs-bishop reference case (spec §1, §3):
			// no evidence, however strong, could ever move this verdict.
			name: "dominated",
			in: ComposeInput{
				CapabilityClass: "judge-critique",
				PolicyLine: LaneInput{
					LaneID:         "lane_policy_a",
					CostPerAttempt: ptr(0.25),
					Posterior:      ptr(1.0),
					Attempts:       40,
					Completions:    40,
					BeliefRefs:     []string{"belief_b", "belief_a"},
					LedgerWindow:   "receipts:200",
				},
				Deviation: LaneInput{
					LaneID:         "lane_dev_x",
					CostPerAttempt: ptr(2.0),
					Posterior:      ptr(0.5),
					Attempts:       6,
					Completions:    3,
					BeliefRefs:     []string{"belief_z", "belief_y"},
					LedgerWindow:   "receipts:12",
				},
				RiskGrade:            "judge",
				RoutePlanRef:         "rp_test_dominated",
				ComputedFromRevision: "rev_test_dominated",
			},
			wantVerdict:       VerdictDominated,
			wantRequired:      ptr(8.0),
			wantPredictionReq: true,
		},
		{
			// Unpriceable: the deviation's ledger cost has not been imported yet (nil). The card
			// must name the gap and must not fabricate a deviation EV or a required_posterior —
			// both depend on the missing number.
			name: "unpriceable",
			in: ComposeInput{
				CapabilityClass: "judge-critique",
				PolicyLine: LaneInput{
					LaneID:         "lane_policy_a",
					CostPerAttempt: ptr(0.25),
					Posterior:      ptr(0.5),
					Attempts:       20,
					Completions:    10,
					BeliefRefs:     []string{"belief_a"},
					LedgerWindow:   "receipts:200",
				},
				Deviation: LaneInput{
					LaneID:      "lane_dev_y",
					Posterior:   ptr(0.25),
					Attempts:    3,
					Completions: 1,
					BeliefRefs:  []string{"belief_c", "belief_b"},
				},
				RiskGrade:            "gate",
				RoutePlanRef:         "rp_test_unpriceable",
				ComputedFromRevision: "rev_test_unpriceable",
			},
			wantVerdict:       VerdictUnpriceable,
			wantMissing:       []string{"deviation.cost_per_attempt"},
			wantPredictionReq: false,
		},
		{
			// Boundary is doubly pointed: posterior_deviation == required_posterior exactly (the
			// >= edge spec §3 names for priced_right), AND that posterior is 0.5 on ZERO evidence
			// (Attempts: 0, Completions: 0) — the exact Beta(2,2) prior mean (spec §8.3: a 2/2
			// prior must never be read as knowledge). priced_right is still the correct verdict
			// (Compose prices posteriors, it does not gate on evidence count — that floor belongs
			// to lanebeliefs, not this composer), but the card shows attempts=0 alongside the
			// verdict so a reader can see this priced_right rests on a bare prior.
			name: "boundary",
			in: ComposeInput{
				CapabilityClass: "judge-critique",
				PolicyLine: LaneInput{
					LaneID:         "lane_policy_a",
					CostPerAttempt: ptr(0.25),
					Posterior:      ptr(0.5),
					Attempts:       20,
					Completions:    10,
					BeliefRefs:     []string{"belief_a"},
					LedgerWindow:   "receipts:200",
				},
				Deviation: LaneInput{
					LaneID:         "lane_dev_z",
					CostPerAttempt: ptr(0.25),
					Posterior:      ptr(0.5),
					Attempts:       0,
					Completions:    0,
					BeliefRefs:     []string{"belief_d"},
					LedgerWindow:   "receipts:0",
				},
				RiskGrade:            "judge",
				RoutePlanRef:         "rp_test_boundary",
				ComputedFromRevision: "rev_test_boundary",
			},
			wantVerdict:       VerdictPricedRight,
			wantRequired:      ptr(0.5),
			wantPredictionReq: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			card := Compose(tc.in)

			if card.Verdict != tc.wantVerdict {
				t.Errorf("verdict = %q, want %q", card.Verdict, tc.wantVerdict)
			}
			if card.PredictionRequired != tc.wantPredictionReq {
				t.Errorf("prediction_required = %v, want %v", card.PredictionRequired, tc.wantPredictionReq)
			}
			switch {
			case tc.wantRequired == nil && card.RequiredPosterior != nil:
				t.Errorf("required_posterior = %v, want nil", *card.RequiredPosterior)
			case tc.wantRequired != nil && card.RequiredPosterior == nil:
				t.Errorf("required_posterior = nil, want %v", *tc.wantRequired)
			case tc.wantRequired != nil && *card.RequiredPosterior != *tc.wantRequired:
				t.Errorf("required_posterior = %v, want %v", *card.RequiredPosterior, *tc.wantRequired)
			}
			if !reflect.DeepEqual(card.MissingInputs, tc.wantMissing) {
				t.Errorf("missing_inputs = %v, want %v", card.MissingInputs, tc.wantMissing)
			}

			got, err := json.MarshalIndent(card, "", "  ")
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got = append(got, '\n')

			goldenPath := filepath.Join("testdata", tc.name+".golden.json")
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden %s: %v (run `UPDATE_GOLDEN=1 go test ./internal/routeev/...` to create it after a deliberate shape change)", goldenPath, err)
			}
			if string(got) != string(want) {
				t.Errorf("golden mismatch for %s:\n--- got ---\n%s\n--- want (testdata/%s.golden.json) ---\n%s", tc.name, got, tc.name, want)
			}
		})
	}
}

// TestCompose_VerdictMatrix rounds out the truth table beyond the three mandated golden fixtures
// (overpaid, and priced_right off the boundary) with direct numeric assertions — no golden file
// needed since these only exercise logic the golden fixtures don't already pin down byte-for-byte.
func TestCompose_VerdictMatrix(t *testing.T) {
	cases := []struct {
		name         string
		policyCost   float64
		policyPost   float64
		devCost      float64
		devPost      float64
		wantVerdict  string
		wantRequired float64
	}{
		{name: "overpaid", policyCost: 0.5, policyPost: 0.5, devCost: 0.25, devPost: 0.125, wantVerdict: VerdictOverpaid, wantRequired: 0.25},
		{name: "priced_right_above_boundary", policyCost: 0.5, policyPost: 0.5, devCost: 0.25, devPost: 0.5, wantVerdict: VerdictPricedRight, wantRequired: 0.25},
		{name: "dominated_certainty_cannot_save_it", policyCost: 0.25, policyPost: 1.0, devCost: 2.0, devPost: 1.0, wantVerdict: VerdictDominated, wantRequired: 8.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			card := Compose(ComposeInput{
				CapabilityClass: "judge-critique",
				PolicyLine:      LaneInput{LaneID: "lane_policy", CostPerAttempt: ptr(tc.policyCost), Posterior: ptr(tc.policyPost)},
				Deviation:       LaneInput{LaneID: "lane_dev", CostPerAttempt: ptr(tc.devCost), Posterior: ptr(tc.devPost)},
			})
			if card.Verdict != tc.wantVerdict {
				t.Fatalf("verdict = %q, want %q", card.Verdict, tc.wantVerdict)
			}
			if card.RequiredPosterior == nil {
				t.Fatalf("required_posterior is nil, want %v", tc.wantRequired)
			}
			if *card.RequiredPosterior != tc.wantRequired {
				t.Fatalf("required_posterior = %v, want %v", *card.RequiredPosterior, tc.wantRequired)
			}
		})
	}
}

// TestCompose_Deterministic is the §7 S1 depth test: identical inputs must marshal to
// byte-identical JSON, twice over, in the same process.
func TestCompose_Deterministic(t *testing.T) {
	input := sampleFullyPricedInput()
	b1, err := json.Marshal(Compose(input))
	if err != nil {
		t.Fatalf("marshal 1: %v", err)
	}
	b2, err := json.Marshal(Compose(input))
	if err != nil {
		t.Fatalf("marshal 2: %v", err)
	}
	if string(b1) != string(b2) {
		t.Fatalf("Compose is not deterministic on identical input:\nfirst:  %s\nsecond: %s", b1, b2)
	}
}

// TestCompose_BeliefRefOrderInvariant is the §7 S1 depth test: permuting belief_refs input order
// must not change the composed card. Compose's normalization is to sort a defensive copy before
// echoing it back (package doc) — this test would fail if that normalization were ever dropped.
func TestCompose_BeliefRefOrderInvariant(t *testing.T) {
	inOrder := sampleFullyPricedInput()
	inOrder.PolicyLine.BeliefRefs = []string{"belief_a", "belief_b", "belief_c"}
	inOrder.Deviation.BeliefRefs = []string{"belief_x", "belief_y"}

	permuted := sampleFullyPricedInput()
	permuted.PolicyLine.BeliefRefs = []string{"belief_c", "belief_a", "belief_b"}
	permuted.Deviation.BeliefRefs = []string{"belief_y", "belief_x"}

	gotInOrder, err := json.Marshal(Compose(inOrder))
	if err != nil {
		t.Fatalf("marshal in-order: %v", err)
	}
	gotPermuted, err := json.Marshal(Compose(permuted))
	if err != nil {
		t.Fatalf("marshal permuted: %v", err)
	}
	if string(gotInOrder) != string(gotPermuted) {
		t.Fatalf("permuting belief_refs order changed the composed card:\nin-order: %s\npermuted: %s", gotInOrder, gotPermuted)
	}
}

// TestCompose_UnpriceableNamesGapsNoFabrication is the §7 S1 depth test: an unpriceable card must
// contain NO ev/required_posterior numbers for the missing side. It checks both the Go struct
// (nil pointers) and the raw marshaled JSON (the keys themselves must be absent, not present as
// null or a fabricated zero) — a bite check against a future accidental change to an omitempty tag.
func TestCompose_UnpriceableNamesGapsNoFabrication(t *testing.T) {
	card := Compose(ComposeInput{
		CapabilityClass: "judge-critique",
		PolicyLine: LaneInput{
			LaneID:         "lane_policy_a",
			CostPerAttempt: ptr(0.25),
			Posterior:      ptr(0.5),
		},
		Deviation: LaneInput{
			LaneID:    "lane_dev_y",
			Posterior: ptr(0.25),
			// CostPerAttempt intentionally omitted: this is the missing input under test.
		},
	})

	if card.Verdict != VerdictUnpriceable {
		t.Fatalf("verdict = %q, want %q", card.Verdict, VerdictUnpriceable)
	}
	if card.Deviation.EV != nil {
		t.Fatalf("deviation.ev must be nil when cost_per_attempt is missing, got %v", *card.Deviation.EV)
	}
	if card.RequiredPosterior != nil {
		t.Fatalf("required_posterior must be nil on an unpriceable card, got %v", *card.RequiredPosterior)
	}
	wantMissing := []string{"deviation.cost_per_attempt"}
	if !reflect.DeepEqual(card.MissingInputs, wantMissing) {
		t.Fatalf("missing_inputs = %v, want %v", card.MissingInputs, wantMissing)
	}

	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := decoded["required_posterior"]; present {
		t.Fatalf("required_posterior key must be absent from JSON on an unpriceable card, got %s", raw)
	}
	var deviation map[string]json.RawMessage
	if err := json.Unmarshal(decoded["deviation"], &deviation); err != nil {
		t.Fatalf("unmarshal deviation: %v", err)
	}
	if _, present := deviation["ev"]; present {
		t.Fatalf("deviation.ev key must be absent from JSON when cost_per_attempt is missing, got %s", raw)
	}
}
