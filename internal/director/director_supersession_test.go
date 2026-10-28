package director

import (
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/routeevidence"
)

// TestDraftSupersedesMatchingReviewCard is the W5 J9-R3H red falsifier: a
// product_hypothesis_draft for (productID, changeEvent) is the SAME group
// information as the matching product_signal_without_hypothesis card, one step
// further along. Proposing both is redundant; the older review card must NOT
// starve the newer draft under the round-robin cap. The review card must be
// recorded as superseded_by_draft (truthful skip reason), not silently dropped
// and not pushed into cap_reached.
//
// Fixture shape:
//   - 4 review-only products (2 signals each → review card, no draft).
//   - 1 matching-pair product (3 signals → review card + draft).
//
// All 6 product_loop candidates sort by SourceID. Review cards start with
// "product-loop-signal-hypothesis-…" which sorts BEFORE draft SourceIDs that
// start with "product:". So the round-robin first picks the 5 review cards
// (alpha's review wins slot 5), then the draft is cap_reached.
//
// After the fix:
//   - The draft supersedes the review card for the SAME (productID,
//     changeEvent) group. The review card for that pair is recorded in
//     Skipped with reason="superseded_by_draft" and never reaches Proposals.
//   - The draft reaches Proposals.
//   - The 4 review-only reviews are still proposed (no regression).
func TestDraftSupersedesMatchingReviewCard(t *testing.T) {
	home := t.TempDir()
	if _, err := routeevidence.ImportModelCapabilitiesYAML(home, filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml"), directorTestNow); err != nil {
		t.Fatalf("import route fixture: %v", err)
	}
	now := directorTestNow
	// Four review-only products: 2 signals each, so loop model emits a
	// signals_without_hypothesis group + action card but largestStarvedSignalGroup
	// filters them out (< 3 signals), so no draft is emitted.
	for i, ev := range []string{"event one", "event two", "event three", "event four"} {
		sids := []string{
			signalIDFor(i, 1),
			signalIDFor(i, 2),
		}
		seedStarvedProduct(t, home, productOther(i+1), ev, "gsc_clicks", sids, now)
	}
	// Alpha with 3 signals: both a review card AND a draft candidate.
	alphaSigs := []string{"sig_alpha_1", "sig_alpha_2", "sig_alpha_3"}
	seedStarvedProduct(t, home, "product:alpha-app", "After public release, the angler-intent subtitle", "gsc_clicks", alphaSigs, now)

	report, err := Propose(home, Options{Apply: false, Now: now})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}

	// The (alpha, X) draft MUST be proposed.
	var alphaDraft *Proposal
	for i, p := range report.Proposals {
		if p.Kind != "product_hypothesis_draft" {
			continue
		}
		if hasProductIDRef(p.EvidenceRefs, "product:alpha-app") {
			alphaDraft = &report.Proposals[i]
			break
		}
	}
	if alphaDraft == nil {
		t.Fatalf("draft for (product:alpha-app, …) must be proposed after supersession; proposals=%+v skipped=%+v", report.Proposals, report.Skipped)
	}

	// The (alpha, X) review card MUST NOT be in proposals.
	for _, p := range report.Proposals {
		if p.Kind != "product_signal_without_hypothesis" {
			continue
		}
		if hasProductIDRef(p.EvidenceRefs, "product:alpha-app") {
			t.Fatalf("(alpha, X) review card must be superseded by draft, but appeared in Proposals: %+v", p)
		}
	}

	// The (alpha, X) review card MUST appear in Skipped with reason
	// "superseded_by_draft" (truthful, not silently dropped, not cap_reached).
	var alphaReviewSkip *SkippedCandidate
	for i, s := range report.Skipped {
		if s.Kind != "product_signal_without_hypothesis" {
			continue
		}
		if hasProductIDRef(s.EvidenceRefs, "product:alpha-app") {
			alphaReviewSkip = &report.Skipped[i]
			break
		}
	}
	if alphaReviewSkip == nil {
		t.Fatalf("(alpha, X) review card must be in Skipped with reason=superseded_by_draft; got %+v", report.Skipped)
	}
	if alphaReviewSkip.Reason != "superseded_by_draft" {
		t.Fatalf("(alpha, X) review card skip reason = %q, want %q", alphaReviewSkip.Reason, "superseded_by_draft")
	}
	if len(alphaReviewSkip.EvidenceRefs) == 0 {
		t.Fatalf("(alpha, X) review card skip must carry evidence refs, got %+v", alphaReviewSkip)
	}
}

// TestDraftDoesNotSuppressUnrelatedReviewCard is the per-group boundary check
// (test b): a draft for (alpha, X) must NOT suppress the review card for
// (beta, Y). Supersession is keyed on (productID, changeEvent); other products
// keep their review cards as proposals.
//
// Fixture:
//   - product:beta-app with 2 signals → review-only card.
//   - product:gamma-app with 3 signals → review + draft (matching pair).
//
// Without supersession: 2 product_loop candidates. Both fit under cap=5; both
// are proposed. With supersession (gamma's draft → gamma's review): still 2
// candidates, but only gamma's draft remains for that pair; beta's review is
// untouched. Both still proposed.
func TestDraftDoesNotSuppressUnrelatedReviewCard(t *testing.T) {
	home := t.TempDir()
	if _, err := routeevidence.ImportModelCapabilitiesYAML(home, filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml"), directorTestNow); err != nil {
		t.Fatalf("import route fixture: %v", err)
	}
	now := directorTestNow
	// Beta: review-only.
	seedStarvedProduct(t, home, "product:beta-app", "Google Search Console report snapshot observed", "gsc_clicks",
		[]string{"sig_beta_1", "sig_beta_2"}, now)
	// Gamma: matching pair.
	seedStarvedProduct(t, home, "product:gamma-app", "Pro IAP price step", "app_store_connect_iap_units",
		[]string{"sig_gamma_1", "sig_gamma_2", "sig_gamma_3"}, now)

	report, err := Propose(home, Options{Apply: false, Now: now})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}

	// Beta's review MUST still be proposed — gamma's draft does not touch it.
	var betaReview *Proposal
	for i, p := range report.Proposals {
		if p.Kind != "product_signal_without_hypothesis" {
			continue
		}
		if hasProductIDRef(p.EvidenceRefs, "product:beta-app") {
			betaReview = &report.Proposals[i]
			break
		}
	}
	if betaReview == nil {
		t.Fatalf("(beta, …) review card must still be proposed — supersession must not leak across product boundaries; proposals=%+v", report.Proposals)
	}

	// Gamma's draft MUST be proposed.
	var gammaDraft *Proposal
	for i, p := range report.Proposals {
		if p.Kind != "product_hypothesis_draft" {
			continue
		}
		if hasProductIDRef(p.EvidenceRefs, "product:gamma-app") {
			gammaDraft = &report.Proposals[i]
			break
		}
	}
	if gammaDraft == nil {
		t.Fatalf("(gamma, …) draft must be proposed; proposals=%+v", report.Proposals)
	}

	// No proposal may carry BOTH beta and gamma product refs (sanity:
	// proposals stay per-group).
	for _, p := range report.Proposals {
		if hasProductIDRef(p.EvidenceRefs, "product:beta-app") && hasProductIDRef(p.EvidenceRefs, "product:gamma-app") {
			t.Fatalf("proposal must not combine beta and gamma refs: %+v", p)
		}
	}
}

// TestReviewOnlyCardStillProposed verifies (test c): products the drafter
// skips (because they have < 3 signals) keep their review cards as proposals.
// No regression in the product-loop flow for starved-by-size products.
func TestReviewOnlyCardStillProposed(t *testing.T) {
	home := t.TempDir()
	if _, err := routeevidence.ImportModelCapabilitiesYAML(home, filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml"), directorTestNow); err != nil {
		t.Fatalf("import route fixture: %v", err)
	}
	now := directorTestNow
	seedStarvedProduct(t, home, "product:delta-app", "tiny change delta", "iap_units",
		[]string{"sig_delta_1", "sig_delta_2"}, now)

	report, err := Propose(home, Options{Apply: false, Now: now})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}

	var deltaReview *Proposal
	for i, p := range report.Proposals {
		if p.Kind != "product_signal_without_hypothesis" {
			continue
		}
		if hasProductIDRef(p.EvidenceRefs, "product:delta-app") {
			deltaReview = &report.Proposals[i]
			break
		}
	}
	if deltaReview == nil {
		t.Fatalf("review-only product (delta) must still be proposed; proposals=%+v skipped=%+v", report.Proposals, report.Skipped)
	}
}

// hasProductIDRef reports whether refs contains a "product_loop:<id>" or
// "product_action_card:<id>" ref tied to the given productID. Used by the
// supersession tests to attribute candidates back to a product without
// relying on the human-readable Title.
func hasProductIDRef(refs []string, productID string) bool {
	for _, ref := range refs {
		if strings.HasPrefix(ref, "product_loop:"+productID) {
			return true
		}
		if strings.HasPrefix(ref, "product_action_card:") {
			// action card IDs are "<prefix>-<normalized productID …>"
			if strings.Contains(ref, normalizeIDForTest(productID)) {
				return true
			}
		}
	}
	return false
}

func normalizeIDForTest(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastUnderscore := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}

func signalIDFor(productIdx, signalIdx int) string {
	return "sig_sup_" + string(rune('a'+productIdx)) + "_" + string(rune('1'+signalIdx-1))
}

func productOther(n int) string {
	return "product:other-" + string(rune('a'+n-1))
}
