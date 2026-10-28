package product

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/mem"
)

// harnessDraftHome seeds a COMPOUND_HOME with a signals log carrying one or
// more ProductSignal rows the tests can cite by id (MintHypothesisDraftCandidate
// looks the observed_at range up in loadSignals). The signal rows are the
// bare minimum schema LoadSignal accepts; richer fields do not change
// observationWindowForDraft. The log lands at ledger/product-signals.jsonl,
// the path signalsLog resolves to (not the package root).
func harnessDraftHome(t *testing.T, signals ...Signal) string {
	t.Helper()
	home := t.TempDir()
	ledgerDir := filepath.Join(home, "ledger")
	if err := os.MkdirAll(ledgerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(home, "ledger", "product-signals.jsonl")
	lines := []string{}
	for _, s := range signals {
		s.ProductID = "product:alpha-app"
		if strings.TrimSpace(s.ID) == "" {
			s.ID = "sig_" + filepath.Base(home)
		}
		if strings.TrimSpace(s.ObservedAt) == "" {
			s.ObservedAt = "2026-08-01T00:00:00Z"
		}
		if strings.TrimSpace(s.MetricName) == "" {
			s.MetricName = "app_store_connect_iap_units"
		}
		body, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(body))
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

// TestHypothesisDraftProducesCandidateAcceptedByBuildHypothesisPlan pins the
// packet's "the existing hypothesis-plan door accepts the drafted candidate"
// invariant: the minted draft carries every field the plan door reads, and the
// window it derives spans the signals.
//
// The draft is approved first on purpose. The placeholder gate
// (BuildHypothesisPlan refusing an unfilled expected_effect) is the whole point
// of hypothesis_approve.go, so "accepted" now means "accepted once the operator
// has written the sentence" — proving the SHAPE is plan-valid without also
// asserting that an unfilled draft may be planned, which must stay refused.
func TestHypothesisDraftProducesCandidateAcceptedByBuildHypothesisPlan(t *testing.T) {
	home := harnessDraftHome(t,
		Signal{ID: "sig_a", ObservedAt: "2026-08-01T00:00:00Z", MetricName: "app_store_connect_iap_units", ChangeEvent: "Pro IAP price step"},
		Signal{ID: "sig_b", ObservedAt: "2026-08-08T00:00:00Z", MetricName: "app_store_connect_iap_units", ChangeEvent: "Pro IAP price step"},
	)
	id, err := MintHypothesisDraftCandidate(home,
		"product:alpha-app",
		"Pro IAP price step",
		[]string{"app_store_connect_iap_units"},
		[]string{"sig_a", "sig_b"},
		[]string{"product_loop:alpha-app"},
		tProduct,
	)
	if err != nil {
		t.Fatalf("MintHypothesisDraftCandidate: %v", err)
	}
	if !strings.HasPrefix(id, "hyp_draft_") {
		t.Fatalf("candidate id must use the hyp_draft_ prefix, got %q", id)
	}
	if _, err := HypothesisApprove(home, HypothesisApproveInput{
		CandidateID:    id,
		ExpectedEffect: "IAP units should rise after the Pro price step",
		ReviewedBy:     "test-operator",
	}, tProduct); err != nil {
		t.Fatalf("HypothesisApprove on a fresh draft: %v", err)
	}
	plan, err := BuildHypothesisPlan(home, id, "", tProduct)
	if err != nil {
		t.Fatalf("BuildHypothesisPlan must accept an approved draft: %v", err)
	}
	if plan.ProductID != "product:alpha-app" || plan.ChangeEvent != "Pro IAP price step" {
		t.Fatalf("plan must carry the draft inputs: %+v", plan)
	}
	if !strings.HasPrefix(plan.ObservationWindow, "2026-08-01") || !strings.Contains(plan.ObservationWindow, "2026-08-08") {
		t.Fatalf("observation_window must span the signal dates, got %q", plan.ObservationWindow)
	}
}

// TestHypothesisDraftNeverInventsExpectedEffect pins the packet's
// "never invented number or effect" guarantee. The placeholder contains the
// literal "OPERATOR MUST WRITE" string, never a parsed metric number, never a
// falsifiable claim.
func TestHypothesisDraftNeverInventsExpectedEffect(t *testing.T) {
	home := harnessDraftHome(t,
		Signal{ID: "sig_x", ObservedAt: "2026-08-01T00:00:00Z", MetricName: "app_store_connect_iap_units", ChangeEvent: "Pro IAP price step"},
	)
	id, err := MintHypothesisDraftCandidate(home,
		"product:alpha-app",
		"Pro IAP price step",
		[]string{"app_store_connect_iap_units"},
		[]string{"sig_x"},
		nil,
		tProduct,
	)
	if err != nil {
		t.Fatalf("MintHypothesisDraftCandidate: %v", err)
	}
	got, err := ReadHypothesisDraftCandidate(home, id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.ExpectedEffect, "OPERATOR MUST WRITE") {
		t.Fatalf("draft must carry the placeholder, got %q", got.ExpectedEffect)
	}
	if got.ExpectedEffect != fmtPlace("OPERATOR MUST WRITE: what should %s do after %s?", "app_store_connect_iap_units", "Pro IAP price step") {
		t.Fatalf("unexpected placeholder shape: %q", got.ExpectedEffect)
	}
	numbers := regexp.MustCompile(`\b\d+(\.\d+)?%?\b`)
	if numbers.MatchString(got.ExpectedEffect) {
		t.Fatalf("draft must not invent a number, got %q", got.ExpectedEffect)
	}
}

// TestHypothesisDraftIsIdempotent: the second call with the same (productID,
// changeEvent) returns the SAME id and does NOT mint a duplicate. The on-disk
// record stays byte-identical.
func TestHypothesisDraftIsIdempotent(t *testing.T) {
	home := harnessDraftHome(t,
		Signal{ID: "sig_idem", ObservedAt: "2026-08-01T00:00:00Z", MetricName: "iap_units", ChangeEvent: "Pro IAP price step"},
	)
	first, err := MintHypothesisDraftCandidate(home,
		"product:alpha-app", "Pro IAP price step",
		[]string{"iap_units"}, []string{"sig_idem"},
		[]string{"product_loop:idem"}, tProduct,
	)
	if err != nil {
		t.Fatal(err)
	}
	firstBytes := readCandidate(t, home, first)
	second, err := MintHypothesisDraftCandidate(home,
		"product:alpha-app", "Pro IAP price step",
		[]string{"iap_units"}, []string{"sig_idem"},
		[]string{"product_loop:idem"}, tProduct.AddDate(0, 0, 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("idempotent id required, got %q vs %q", first, second)
	}
	secondBytes := readCandidate(t, home, second)
	if string(firstBytes) != string(secondBytes) {
		t.Fatalf("idempotent record required, first=%s second=%s", firstBytes, secondBytes)
	}
}

// TestHypothesisDraftSkipsProductWithOpenOrParkedHypothesis: a parked
// hypothesis already exists for the same (product, change_event) — the writer
// must NOT add a second one. The function returns the same id without
// re-minting.
func TestHypothesisDraftSkipsProductWithOpenOrParkedHypothesis(t *testing.T) {
	home := harnessDraftHome(t,
		Signal{ID: "sig_y", ObservedAt: "2026-08-01T00:00:00Z", MetricName: "iap_units", ChangeEvent: "Pro IAP price step"},
	)
	first, err := MintHypothesisDraftCandidate(home,
		"product:alpha-app", "Pro IAP price step",
		[]string{"iap_units"}, []string{"sig_y"},
		nil, tProduct,
	)
	if err != nil {
		t.Fatal(err)
	}
	// Mutate the first candidate so a second mint would be detectable even
	// if the deterministic id collided silently.
	pokeHypothesisCandidate(t, home, first, func(c *HypothesisCandidate) {
		c.ExpectedEffect = "OPERATOR-ALREADY-FILLED-XYZ"
	})
	second, err := MintHypothesisDraftCandidate(home,
		"product:alpha-app", "Pro IAP price step",
		[]string{"iap_units"}, []string{"sig_y"},
		nil, tProduct,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("must reuse the parked hypothesis id, got %q vs %q", first, second)
	}
	got := readCandidate(t, home, first)
	if !strings.Contains(string(got), "OPERATOR-ALREADY-FILLED-XYZ") {
		t.Fatalf("the existing hypothesis must NOT be overwritten by the draft: %s", got)
	}
}

// TestHypothesisDraftProducesOnePerSignalEvidenceRef: the writer attaches one
// product_signal:<id> per signal, plus any extras the caller passed in, then
// unique-sorts. Stops "the director forgot a signal" regressions.
func TestHypothesisDraftProducesOnePerSignalEvidenceRef(t *testing.T) {
	home := harnessDraftHome(t,
		Signal{ID: "sig_one", ObservedAt: "2026-08-01T00:00:00Z", MetricName: "iap_units", ChangeEvent: "Pro IAP price step"},
		Signal{ID: "sig_two", ObservedAt: "2026-08-02T00:00:00Z", MetricName: "iap_units", ChangeEvent: "Pro IAP price step"},
	)
	id, err := MintHypothesisDraftCandidate(home,
		"product:alpha-app", "Pro IAP price step",
		[]string{"iap_units"}, []string{"sig_one", "sig_two"},
		[]string{"product_loop:alpha-app"}, tProduct,
	)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadHypothesisDraftCandidate(home, id)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"product_signal:sig_one": false,
		"product_signal:sig_two": false,
		"product_loop:alpha-app": false,
	}
	for _, ref := range got.EvidenceRefs {
		if _, ok := want[ref]; ok {
			want[ref] = true
		}
	}
	for ref, seen := range want {
		if !seen {
			t.Fatalf("evidence_refs missing %q in %v", ref, got.EvidenceRefs)
		}
	}
}

// TestHypothesisDraftFallsBackWhenSignalsMissingObservedAt pins the safe
// "review window required" fallback when no signal id resolves to a parseable
// observed_at — the draft still has the two required fields BuildHypothesisPlan
// reads (product_id, expected_effect), and BuildHypothesisPlan supplies its
// own observation_window default downstream.
func TestHypothesisDraftFallsBackWhenSignalsMissingObservedAt(t *testing.T) {
	home := harnessDraftHome(t) // no signals on disk
	id, err := MintHypothesisDraftCandidate(home,
		"product:alpha-app", "Pro IAP price step",
		[]string{"iap_units"}, []string{"sig_missing"}, nil, tProduct,
	)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadHypothesisDraftCandidate(home, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.ObservationWindow != HypothesisDraftScopePlaceholder {
		t.Fatalf("expected fallback observation_window %q, got %q", HypothesisDraftScopePlaceholder, got.ObservationWindow)
	}
	if _, err := HypothesisApprove(home, HypothesisApproveInput{
		CandidateID:    id,
		ExpectedEffect: "IAP units should rise after the Pro price step",
		ReviewedBy:     "test-operator",
	}, tProduct); err != nil {
		t.Fatalf("HypothesisApprove on a windowless draft: %v", err)
	}
	if _, err := BuildHypothesisPlan(home, id, "", tProduct); err != nil {
		t.Fatalf("BuildHypothesisPlan must still accept a windowless draft once approved: %v", err)
	}
}

func readCandidate(t *testing.T, home, id string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(home, candidates.InboxDir(candidates.ProductHypothesis), id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func pokeHypothesisCandidate(t *testing.T, home, id string, mutate func(*HypothesisCandidate)) {
	t.Helper()
	body := readCandidate(t, home, id)
	var c HypothesisCandidate
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatal(err)
	}
	mutate(&c)
	out, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, candidates.InboxDir(candidates.ProductHypothesis), id+".json"), append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fmtPlace(format, metric, event string) string {
	// tiny formatter so this test does NOT depend on fmt.Printf's argument
	// validation order
	b := []byte{}
	i := 0
	for k := 0; k < len(format); k++ {
		c := format[k]
		if c == '%' && i < 2 && k+1 < len(format) {
			next := format[k+1]
			switch next {
			case 's':
				if i == 0 {
					b = append(b, metric...)
				} else {
					b = append(b, event...)
				}
				i++
				k++
				continue
			}
		}
		b = append(b, c)
	}
	return string(b)
}

// keep mem import live so the package still compiles under integration that
// loads the home store (the writer does not, but other tests in this directory do).
var _ = mem.Open

// keep time import referenced so go vet does not chase a phantom
var _ time.Time = tProduct

// Dedupe contract 2026-09-15: when an equivalent hypothesis candidate
// already exists under a DIFFERENT id (minted by another source), the draft
// writer returns that ACTUAL id — never a freshly calculated id that names
// nothing on disk.
func TestMintHypothesisDraftCandidateReturnsExistingCandidateID(t *testing.T) {
	home := t.TempDir()
	productID := "product:synth-notes"
	changeEvent := "v2.4 ships the new onboarding screen"
	existingID := "hyp_from_review_abcdef123456"
	existing := HypothesisCandidate{
		SchemaVersion:     ProductHypothesisCandidate,
		ID:                existingID,
		Kind:              string(candidates.ProductHypothesis),
		ProductID:         productID,
		ChangeEvent:       changeEvent,
		ExpectedEffect:    "another source minted this equivalent hypothesis first",
		Metrics:           []string{"synthetic_first_time_downloads"},
		ObservationWindow: "28d",
		Result:            "unknown",
		NextAction:        "awaiting review",
		Verdict:           candidates.Park,
		CreatedAt:         tProduct.Format(time.RFC3339),
	}
	if err := writeHypothesisCandidate(home, existing); err != nil {
		t.Fatal(err)
	}

	id, err := MintHypothesisDraftCandidate(home, productID, changeEvent, []string{"synthetic_first_time_downloads"}, nil, nil, tProduct)
	if err != nil {
		t.Fatalf("MintHypothesisDraftCandidate: %v", err)
	}
	if id != existingID {
		t.Fatalf("dedupe returned id %q, want the ACTUAL existing candidate id %q", id, existingID)
	}
	// And no second candidate was written.
	hyps, err := loadHypotheses(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(hyps) != 1 || hyps[0].ID != existingID {
		t.Fatalf("dedupe must not write a second candidate, got %d records", len(hyps))
	}
}
