package product

import (
	"fmt"
	"strings"
	"testing"
)

// W5 J9-R3G fan-out regression: a hypothesis that matches N signals must yield
// exactly ONE "Create product review plan" action card — today the loop emits
// one per (hypothesis, signal) pair, which flood the director's proposal
// budget with identical-titled cards. These tests pin the contract directly
// against loopReviewReadyActionCards so the dedupe cannot regress unnoticed.

func makeReviewReady(hyp, signal string) ReviewReady {
	return ReviewReady{
		HypothesisID:      hyp,
		SignalID:          signal,
		MetricName:        "gsc_clicks",
		Window:            "2026-07-01/2026-07-02",
		MatchReason:       "metric_match",
		ReviewPlanCommand: fmt.Sprintf("compound product review-plan --hypothesis %s --signal %s", hyp, signal),
	}
}

func TestLoopReviewReadyActionCardsCollapsesByHypothesis_FanOut(t *testing.T) {
	const hypID = "hyp_fanout_collapse"
	const want = 50
	ready := make([]ReviewReady, 0, want)
	for i := 0; i < want; i++ {
		ready = append(ready, makeReviewReady(hypID, fmt.Sprintf("sig_fanout_%03d", i)))
	}
	cards := loopReviewReadyActionCards("product:fanout", ready)
	if len(cards) != 1 {
		t.Fatalf("hypothesis matching %d signals should emit exactly ONE review-plan card; got %d cards:\n%+v", want, len(cards), cards)
	}
	card := cards[0]
	if card.ID != "product-review-plan-"+normalizeIDPart("product:fanout "+hypID) {
		t.Fatalf("card id should be stable per (product, hypothesis); got %q", card.ID)
	}
	if card.Kind != "product_review_plan" || !strings.HasPrefix(card.Title, "Create product review plan") {
		t.Fatalf("card kind/title = (%q,%q); want product_review_plan/Create product review plan…", card.Kind, card.Title)
	}
	if !strings.Contains(card.Title, fmt.Sprintf("%d", want)) {
		t.Fatalf("card title should expose the matched-signal count for the operator; got %q", card.Title)
	}
	repLine := ""
	for _, line := range card.RequiredInputs {
		if strings.HasPrefix(line, "representative signal: ") {
			repLine = line
			break
		}
	}
	if repLine == "" {
		t.Fatalf("card should name the representative signal in required_inputs; got %+v", card.RequiredInputs)
	}
	repSignal := strings.TrimPrefix(repLine, "representative signal: ")
	wantRep := "sig_fanout_000" // smallest signal id == first
	if repSignal != wantRep {
		t.Fatalf("representative signal should be the deterministic smallest signal id; got %q want %q", repSignal, wantRep)
	}
	countLine := ""
	for _, line := range card.RequiredInputs {
		if strings.HasPrefix(line, "matched signals: ") {
			countLine = line
			break
		}
	}
	if countLine == "" {
		t.Fatalf("card should expose how many signals matched; got %+v", card.RequiredInputs)
	}
	if !strings.Contains(countLine, fmt.Sprintf("%d", want)) {
		t.Fatalf("matched-signal count line should expose %d, got %q", want, countLine)
	}
	cmd := strings.Join(card.Command, " ")
	for _, fragment := range []string{
		"compound", "product", "review-plan",
		"--hypothesis", hypID,
		"--signal", repSignal,
	} {
		if !strings.Contains(cmd, fragment) {
			t.Fatalf("card command should be a runnable review-plan invocation with the representative signal; missing %q in %q", fragment, cmd)
		}
	}
}

func TestLoopReviewReadyActionCardsCollapsesByHypothesis_TwoHypotheses(t *testing.T) {
	const hypA = "hyp_fanout_alpha"
	const hypB = "hyp_fanout_beta"
	var ready []ReviewReady
	for i := 0; i < 7; i++ {
		ready = append(ready, makeReviewReady(hypA, fmt.Sprintf("sig_a_%03d", i)))
	}
	for i := 0; i < 11; i++ {
		ready = append(ready, makeReviewReady(hypB, fmt.Sprintf("sig_b_%03d", i)))
	}
	cards := loopReviewReadyActionCards("product:two", ready)
	if len(cards) != 2 {
		t.Fatalf("two hypotheses should emit exactly TWO review-plan cards; got %d:\n%+v", len(cards), cards)
	}
	ids := map[string]string{}
	for _, card := range cards {
		if card.ID == "" || ids[card.ID] != "" {
			t.Fatalf("card ids must be unique within a product; got %+v", cards)
		}
		ids[card.ID] = card.Title
	}
	wantA := "product-review-plan-" + normalizeIDPart("product:two "+hypA)
	wantB := "product-review-plan-" + normalizeIDPart("product:two "+hypB)
	if ids[wantA] == "" || ids[wantB] == "" {
		t.Fatalf("expected ids %q and %q; got %+v", wantA, wantB, ids)
	}
	for _, card := range cards {
		if !strings.Contains(card.Title, "7") && !strings.Contains(card.Title, "11") {
			t.Fatalf("card title should expose the matched-signal count for its hypothesis; got %q", card.Title)
		}
	}
}

func TestLoopReviewReadyActionCardsSingleSignalUnchanged(t *testing.T) {
	const hypID = "hyp_single_signal"
	const signal = "sig_only_one"
	ready := []ReviewReady{makeReviewReady(hypID, signal)}
	cards := loopReviewReadyActionCards("product:single", ready)
	if len(cards) != 1 {
		t.Fatalf("single-signal hypothesis should still emit ONE review-plan card; got %d", len(cards))
	}
	card := cards[0]
	// The new contract collapses the id to (product, hypothesis); even with
	// one signal the id is the same shape — that's intentional and proves the
	// dedupe does not change for the single-signal corner case.
	if card.ID != "product-review-plan-"+normalizeIDPart("product:single "+hypID) {
		t.Fatalf("single-signal card id should be stable per (product, hypothesis); got %q", card.ID)
	}
	if !strings.HasPrefix(card.Title, "Create product review plan") {
		t.Fatalf("single-signal card title should remain Create product review plan…; got %q", card.Title)
	}
	if strings.Contains(card.Title, "(") {
		t.Fatalf("single-signal card should not bolt on a misleading fan-out count suffix; got %q", card.Title)
	}
	countFound := false
	for _, line := range card.RequiredInputs {
		if strings.HasPrefix(line, "matched signals: ") && strings.Contains(line, "1") {
			countFound = true
			break
		}
	}
	if !countFound {
		t.Fatalf("single-signal card should still surface matched count for the operator; got %+v", card.RequiredInputs)
	}
	for _, fragment := range []string{"--hypothesis", hypID, "--signal", signal} {
		if !strings.Contains(strings.Join(card.Command, " "), fragment) {
			t.Fatalf("card command should keep hypothesis %q signal %q; got %q", hypID, signal, card.Command)
		}
	}
}
