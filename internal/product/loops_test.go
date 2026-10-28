package product

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/mem"
	"compound/internal/work"
)

var loopTestNow = time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

func TestBuildLoopModelJoinsProductFeedbackLoop(t *testing.T) {
	home := seedLoopModelHome(t)
	model, err := BuildLoopModel(home, LoopOptions{Recent: 1, StaleAfter: 48 * time.Hour, Now: loopTestNow})
	if err != nil {
		t.Fatal(err)
	}
	loop := findProductLoop(model.Products, "product:loop-demo")
	if loop.ProductID == "" {
		t.Fatalf("loop products = %+v", model.Products)
	}
	gscSignals := loop.SignalsByMetric["gsc_clicks"]
	if len(gscSignals) != 1 || gscSignals[0].SignalID != "sig_loop_gsc_new" {
		t.Fatalf("gsc signals = %+v", gscSignals)
	}
	if gscSignals[0].Provider != "google_search_console" || gscSignals[0].TransitionRef == "" {
		t.Fatalf("gsc signal provider/transition = %+v", gscSignals[0])
	}
	if !hasLoopHypothesis(loop.OpenHypotheses, "hyp_loop_search_ready") || !hasLoopHypothesis(loop.OpenHypotheses, "hyp_loop_activation_missing") {
		t.Fatalf("open hypotheses = %+v", loop.OpenHypotheses)
	}
	searchHypothesis := findLoopHypothesis(loop.OpenHypotheses, "hyp_loop_search_ready")
	if len(searchHypothesis.Metrics) != 1 ||
		searchHypothesis.Metrics[0] != "gsc_clicks" ||
		searchHypothesis.ObservationWindow != "7d after change" ||
		searchHypothesis.NextAction != "review search clicks after window" ||
		len(searchHypothesis.EvidenceRefs) == 0 {
		t.Fatalf("open hypothesis should expose measurement/review contract: %+v", searchHypothesis)
	}
	if hasLoopHypothesis(loop.OpenHypotheses, "hyp_loop_reviewed_conversion") {
		t.Fatalf("reviewed hypothesis should not remain open: %+v", loop.OpenHypotheses)
	}
	missingUnknown := findLoopMissingSignalSource(loop.OpenHypothesesByMissingSignalSource, "unknown")
	if !hasString(missingUnknown.HypothesisIDs, "hyp_loop_activation_missing") ||
		!hasString(missingUnknown.Metrics, "activation_rate") ||
		!hasString(missingUnknown.NextActions, "wait for activation evidence") {
		t.Fatalf("missing signal source projection should expose open evidence debt: %+v", loop.OpenHypothesesByMissingSignalSource)
	}
	missingCard := findLoopActionCard(missingUnknown.ActionCards, "product_signal_import")
	if missingCard.ID == "" ||
		!hasString(missingCard.Command, "signal") ||
		missingCard.Boundary == "" ||
		missingCard.Proof == "" {
		t.Fatalf("missing signal group should expose executable import card: %+v", missingUnknown.ActionCards)
	}
	reviewPlanCard := findLoopActionCard(loop.ActionCards, "product_review_plan")
	if reviewPlanCard.ID == "" ||
		!hasString(reviewPlanCard.Command, "review-plan") ||
		!hasString(reviewPlanCard.Command, "hyp_loop_search_ready") ||
		!hasString(reviewPlanCard.Command, "sig_loop_gsc_new") {
		t.Fatalf("loop action cards should expose exact review-plan command: %+v", loop.ActionCards)
	}
	unmatched := findLoopSignalHypothesisGap(loop.SignalsWithoutHypothesis, "Published unmatched pricing copy.")
	if !hasString(unmatched.SignalIDs, "sig_loop_unmatched") ||
		!hasString(unmatched.Metrics, "custom_unmatched_metric") ||
		!hasString(unmatched.Providers, "unknown") {
		t.Fatalf("signal-without-hypothesis group should preserve unmatched signal evidence: %+v", loop.SignalsWithoutHypothesis)
	}
	unmatchedCard := findLoopActionCard(unmatched.ActionCards, "product_signal_hypothesis_review")
	if unmatchedCard.ID == "" ||
		!hasString(unmatchedCard.Command, "action-plan") ||
		!hasString(unmatchedCard.Command, "--card") ||
		!hasString(unmatchedCard.Command, unmatchedCard.ID) ||
		!hasString(unmatchedCard.RequiredInputs, "signal ids: sig_loop_unmatched") ||
		unmatchedCard.Boundary == "" ||
		unmatchedCard.Proof == "" {
		t.Fatalf("signal-without-hypothesis group should expose reviewed synthesis card: %+v", unmatched.ActionCards)
	}
	if len(loop.Reviews) != 1 || loop.Reviews[0].ID != "review_loop_conversion" || loop.Reviews[0].Verdict != "supported" || loop.Reviews[0].SignalID != "sig_loop_conversion_reviewed" {
		t.Fatalf("reviews = %+v", loop.Reviews)
	}
	nextActionGroup := findLoopReviewNextAction(loop.ClosedReviewsByNextAction, "keep measuring one more window")
	if !hasString(nextActionGroup.ReviewIDs, "review_loop_conversion") ||
		!hasString(nextActionGroup.HypothesisIDs, "hyp_loop_reviewed_conversion") ||
		!hasString(nextActionGroup.SignalIDs, "sig_loop_conversion_reviewed") {
		t.Fatalf("closed review next-action projection should expose follow-up grouping: %+v", loop.ClosedReviewsByNextAction)
	}
	nextCard := findLoopActionCard(nextActionGroup.ActionCards, "product_next_bounded_move")
	if nextCard.ID == "" ||
		!hasString(nextCard.Command, "action-plan") ||
		!hasString(nextCard.Command, "--card") ||
		!hasString(nextCard.Command, nextCard.ID) ||
		nextCard.ActionSummary != "keep measuring one more window" ||
		nextCard.Boundary == "" ||
		nextCard.Proof == "" {
		t.Fatalf("closed review next-action should expose bounded follow-up card: %+v", nextActionGroup.ActionCards)
	}
	if loop.OpenSourceIssues.Count != 1 || len(loop.OpenSourceIssues.Refs) != 1 {
		t.Fatalf("source issues = %+v", loop.OpenSourceIssues)
	}
	for _, gap := range []string{"signals_without_hypothesis", "hypothesis_without_signal", "review_ready"} {
		if !hasString(loop.LoopGaps, gap) {
			t.Fatalf("gap %q missing from %+v", gap, loop.LoopGaps)
		}
	}
	if loop.Freshness.LatestSignalAt != "2026-07-02T11:00:00Z" || loop.Freshness.LatestReviewAt != "2026-07-02T08:00:00Z" || loop.Freshness.Stale {
		t.Fatalf("freshness = %+v", loop.Freshness)
	}
}

func TestBuildLoopModelSuppressesConsumedClosedReviewActionCard(t *testing.T) {
	home := seedLoopModelHome(t)
	before, err := BuildLoopModel(home, LoopOptions{Recent: 1, StaleAfter: 48 * time.Hour, Now: loopTestNow})
	if err != nil {
		t.Fatal(err)
	}
	beforeLoop := findProductLoop(before.Products, "product:loop-demo")
	beforeGroup := findLoopReviewNextAction(beforeLoop.ClosedReviewsByNextAction, "keep measuring one more window")
	beforeCard := findLoopActionCard(beforeGroup.ActionCards, "product_next_bounded_move")
	if beforeCard.ID == "" {
		t.Fatalf("fixture should expose closed-review next-action card before consumption: %+v", beforeGroup)
	}

	seedCompletedProductActionWork(t, home, "product:loop-demo", beforeCard.ID, beforeGroup.ReviewIDs[0])

	after, err := BuildLoopModel(home, LoopOptions{Recent: 1, StaleAfter: 48 * time.Hour, Now: loopTestNow})
	if err != nil {
		t.Fatal(err)
	}
	afterLoop := findProductLoop(after.Products, "product:loop-demo")
	afterGroup := findLoopReviewNextAction(afterLoop.ClosedReviewsByNextAction, "keep measuring one more window")
	if !afterGroup.Consumed {
		t.Fatalf("closed review next-action should be marked consumed after completed work evidence: %+v", afterGroup)
	}
	if !hasString(afterGroup.ConsumedByWorkIDs, "work_loop_consumed_action") {
		t.Fatalf("consumed work ids = %+v, want completed work id", afterGroup.ConsumedByWorkIDs)
	}
	if !hasString(afterGroup.ConsumedEvidenceRefs, "product_review:review_loop_conversion") || !hasString(afterGroup.ConsumedEvidenceRefs, "transition:tr_loop_consumed_action") {
		t.Fatalf("consumed evidence refs should preserve review/transition proof: %+v", afterGroup.ConsumedEvidenceRefs)
	}
	if card := findLoopActionCard(afterGroup.ActionCards, "product_next_bounded_move"); card.ID != "" {
		t.Fatalf("consumed closed review must not keep executable action card on group: %+v", afterGroup.ActionCards)
	}
	for _, card := range afterLoop.ActionCards {
		if card.ID == beforeCard.ID {
			t.Fatalf("consumed closed review must not remain in top-level action cards: %+v", afterLoop.ActionCards)
		}
	}
	if err := after.Validate(); err != nil {
		t.Fatalf("consumed loop model should validate: %v", err)
	}
}

func TestBuildLoopModelSuppressesConsumedSignalHypothesisActionCard(t *testing.T) {
	home := seedLoopModelHome(t)
	before, err := BuildLoopModel(home, LoopOptions{Recent: 1, StaleAfter: 48 * time.Hour, Now: loopTestNow})
	if err != nil {
		t.Fatal(err)
	}
	beforeLoop := findProductLoop(before.Products, "product:loop-demo")
	beforeGroup := findLoopSignalHypothesisGap(beforeLoop.SignalsWithoutHypothesis, "Published unmatched pricing copy.")
	beforeCard := findLoopActionCard(beforeGroup.ActionCards, "product_signal_hypothesis_review")
	if beforeCard.ID == "" {
		t.Fatalf("fixture should expose signal-hypothesis review card before consumption: %+v", beforeGroup)
	}

	seedCompletedProductActionWork(t, home, "product:loop-demo", beforeCard.ID, "")

	after, err := BuildLoopModel(home, LoopOptions{Recent: 1, StaleAfter: 48 * time.Hour, Now: loopTestNow})
	if err != nil {
		t.Fatal(err)
	}
	afterLoop := findProductLoop(after.Products, "product:loop-demo")
	afterGroup := findLoopSignalHypothesisGap(afterLoop.SignalsWithoutHypothesis, "Published unmatched pricing copy.")
	if !afterGroup.Consumed {
		t.Fatalf("signal-hypothesis gap should be marked consumed after completed work evidence: %+v", afterGroup)
	}
	if !hasString(afterGroup.ConsumedByWorkIDs, "work_loop_consumed_action") {
		t.Fatalf("consumed work ids = %+v, want completed work id", afterGroup.ConsumedByWorkIDs)
	}
	if !hasString(afterGroup.ConsumedEvidenceRefs, "work:work_loop_consumed_action") ||
		!hasString(afterGroup.ConsumedEvidenceRefs, "transition:tr_loop_consumed_action") {
		t.Fatalf("consumed evidence refs should preserve work/transition proof: %+v", afterGroup.ConsumedEvidenceRefs)
	}
	if card := findLoopActionCard(afterGroup.ActionCards, "product_signal_hypothesis_review"); card.ID != "" {
		t.Fatalf("consumed signal-hypothesis group must not keep executable action card: %+v", afterGroup.ActionCards)
	}
	for _, card := range afterLoop.ActionCards {
		if card.ID == beforeCard.ID {
			t.Fatalf("consumed signal-hypothesis card must not remain top-level action card: %+v", afterLoop.ActionCards)
		}
	}
	if err := after.Validate(); err != nil {
		t.Fatalf("consumed signal-hypothesis loop model should validate: %v", err)
	}
}

func TestBuildLoopModelStaleWindowAndProductFilter(t *testing.T) {
	home := seedLoopModelHome(t)
	model, err := BuildLoopModel(home, LoopOptions{ProductID: "product:loop-demo", Recent: 10, StaleDays: 7, Now: loopTestNow.Add(8 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.Products) != 1 || model.Products[0].ProductID != "product:loop-demo" {
		t.Fatalf("filtered products = %+v", model.Products)
	}
	if !model.Products[0].Freshness.Stale {
		t.Fatalf("freshness should be stale after stale window: %+v", model.Products[0].Freshness)
	}
	missing, err := BuildLoopModel(home, LoopOptions{ProductID: "product:missing", Now: loopTestNow})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing.Products) != 0 {
		t.Fatalf("missing product filter = %+v", missing.Products)
	}
}

func TestDryRunDerivedHypothesisClosesAfterSignalReview(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}

	_, reviewedReceiptPath := writeAcceptedOutboundDryRunFixture(t, home)
	evidencePlan, err := BuildOutboundDryRunEvidencePlan(home, reviewedReceiptPath, "", tProduct)
	if err != nil {
		t.Fatal(err)
	}
	evidencePlanPaths, err := RecordOutboundDryRunEvidencePlan(home, evidencePlan)
	if err != nil {
		t.Fatal(err)
	}
	bridgeReview := OutboundDryRunEvidenceReview{
		SchemaVersion:     OutboundDryRunEvidenceReviewSchemaVersion,
		ID:                evidencePlan.ReviewDraft.ReviewID,
		ProductID:         evidencePlan.ProductID,
		ReceiptID:         evidencePlan.ReceiptID,
		Decision:          "create_hypothesis_candidate",
		Rationale:         "Synthetic dry-run evidence is useful enough to observe with a later product signal.",
		Confidence:        0.7,
		ExpectedEffect:    "reviewed outbound drafts improve product-message clarity before public distribution",
		PrimaryMetrics:    []string{"posthog_activation_rate"},
		ObservationWindow: "7d after public launch",
		NextAction:        "import the first post-launch activation signal and review the hypothesis",
		EvidenceRefs:      evidencePlan.EvidenceRefs,
	}
	bridgeReport, err := ApplyOutboundDryRunEvidenceReview(home, store, bridgeReview, evidencePlanPaths.ReviewYAMLPath, tProduct.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if bridgeReport.HypothesisID == "" {
		t.Fatalf("bridge report did not create hypothesis: %+v", bridgeReport)
	}

	openModel, err := BuildLoopModel(home, LoopOptions{ProductID: evidencePlan.ProductID, Now: tProduct.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	openLoop := findProductLoop(openModel.Products, evidencePlan.ProductID)
	if !hasLoopHypothesis(openLoop.OpenHypotheses, bridgeReport.HypothesisID) {
		t.Fatalf("dry-run-derived hypothesis should be open before signal review: %+v", openLoop.OpenHypotheses)
	}
	missingPostHog := findLoopMissingSignalSource(openLoop.OpenHypothesesByMissingSignalSource, "posthog")
	if !hasString(missingPostHog.HypothesisIDs, bridgeReport.HypothesisID) ||
		!hasString(missingPostHog.Metrics, "posthog_activation_rate") ||
		!hasString(missingPostHog.NextActions, "import the first post-launch activation signal and review the hypothesis") {
		t.Fatalf("dry-run-derived open hypothesis should project missing posthog signal source: %+v", openLoop.OpenHypothesesByMissingSignalSource)
	}

	signalDoc := SignalDocument{
		SchemaVersion: SignalSchemaVersion,
		Source:        SignalSource{Kind: "posthog", Ref: "fixture:posthog:activation"},
		Signal: Signal{
			ID:           "sig_outbound_activation_after_public_launch",
			ProductID:    evidencePlan.ProductID,
			Surface:      "activation",
			ChangeEvent:  evidencePlan.ChangeEvent,
			MetricName:   "posthog_activation_rate",
			Before:       0.21,
			After:        0.28,
			Window:       "2026-07-01/2026-07-07",
			Confidence:   0.66,
			Confounders:  []string{"fixture seasonality checked"},
			EvidenceRefs: []string{"fixture:posthog-activation-after-launch"},
			ObservedAt:   tProduct.Add(7 * 24 * time.Hour).Format(time.RFC3339),
		},
		EvidenceRefs: []string{"fixture:posthog-export"},
	}
	signalReport, err := ApplySignal(home, store, signalDoc, "fixture/posthog-activation.yaml", tProduct.Add(7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(signalReport.SignalIDs) != 1 || len(signalReport.TransitionIDs) != 1 {
		t.Fatalf("signal report should leave signal and transition proof: %+v", signalReport)
	}

	reviewPlan, err := BuildReviewPlan(home, bridgeReport.HypothesisID, signalDoc.Signal.ID, "", tProduct.Add(8*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if reviewPlan.HypothesisID != bridgeReport.HypothesisID ||
		reviewPlan.SignalID != signalDoc.Signal.ID ||
		!hasString(reviewPlan.EvidenceRefs, "product_signal:"+signalDoc.Signal.ID) ||
		!hasString(reviewPlan.EvidenceRefs, "product_hypothesis:"+bridgeReport.HypothesisID) {
		t.Fatalf("review plan should join hypothesis and signal evidence: %+v", reviewPlan)
	}

	review := Review{
		SchemaVersion: ReviewSchemaVersion,
		ID:            reviewPlan.ReviewDraft.ReviewID,
		ProductID:     evidencePlan.ProductID,
		HypothesisID:  bridgeReport.HypothesisID,
		Result:        "supported",
		Rationale:     "Activation increased in the observed window and fixture confounders were checked.",
		Confidence:    0.72,
		NextAction:    "prepare the next bounded outbound experiment",
		EvidenceRefs:  reviewPlan.EvidenceRefs,
		ReviewedBy:    "operator",
	}
	reviewReport, err := ApplyReview(home, store, review, "fixture/product-review.review.yaml", tProduct.Add(8*24*time.Hour+time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if reviewReport.Status != "updated" || reviewReport.CandidateOutcome != "committed" || reviewReport.Result != "supported" || reviewReport.TransitionID == "" {
		t.Fatalf("review report should close hypothesis with transition evidence: %+v", reviewReport)
	}

	closedModel, err := BuildLoopModel(home, LoopOptions{ProductID: evidencePlan.ProductID, Now: tProduct.Add(9 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	closedLoop := findProductLoop(closedModel.Products, evidencePlan.ProductID)
	if hasLoopHypothesis(closedLoop.OpenHypotheses, bridgeReport.HypothesisID) {
		t.Fatalf("reviewed hypothesis should leave open set: %+v", closedLoop.OpenHypotheses)
	}
	if len(closedLoop.Reviews) != 1 ||
		closedLoop.Reviews[0].ID != review.ID ||
		closedLoop.Reviews[0].Verdict != "supported" ||
		closedLoop.Reviews[0].SignalID != signalDoc.Signal.ID {
		t.Fatalf("loop history should retain applied review and signal ref: %+v", closedLoop.Reviews)
	}
	if len(closedLoop.OpenHypothesesByMissingSignalSource) != 0 {
		t.Fatalf("closed dry-run-derived loop should not retain missing source debt: %+v", closedLoop.OpenHypothesesByMissingSignalSource)
	}
	closedNextAction := findLoopReviewNextAction(closedLoop.ClosedReviewsByNextAction, "prepare the next bounded outbound experiment")
	if !hasString(closedNextAction.ReviewIDs, review.ID) ||
		!hasString(closedNextAction.HypothesisIDs, bridgeReport.HypothesisID) ||
		!hasString(closedNextAction.SignalIDs, signalDoc.Signal.ID) {
		t.Fatalf("closed dry-run-derived review should group by next bounded move: %+v", closedLoop.ClosedReviewsByNextAction)
	}
}

func seedLoopModelHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	bundle := Bundle{
		SchemaVersion: SchemaVersion,
		Product: Product{
			ID:             "product:loop-demo",
			Name:           "Loop Demo",
			OwnerEntity:    "project:loop-demo",
			Surfaces:       []Surface{{ID: "landing", Kind: "web", Ref: "https://example.invalid/loop-demo"}},
			Channels:       []string{"web"},
			Positioning:    "Synthetic feedback-loop test surface.",
			PrivacyPosture: "test_fixture",
		},
		Hypotheses: []Hypothesis{
			{
				ID:                "hyp_loop_search_ready",
				ProductID:         "product:loop-demo",
				ChangeEvent:       "Updated search title copy.",
				ExpectedEffect:    "Search clicks increase.",
				Metrics:           []string{"gsc_clicks"},
				ObservationWindow: "7d after change",
				Result:            "unknown",
				NextAction:        "review search clicks after window",
				EvidenceRefs:      []string{"fixture:search-title"},
			},
			{
				ID:                "hyp_loop_activation_missing",
				ProductID:         "product:loop-demo",
				ChangeEvent:       "Changed activation flow.",
				ExpectedEffect:    "Activation improves.",
				Metrics:           []string{"activation_rate"},
				ObservationWindow: "7d after change",
				Result:            "unknown",
				NextAction:        "wait for activation evidence",
				EvidenceRefs:      []string{"fixture:activation-plan"},
			},
			{
				ID:                "hyp_loop_reviewed_conversion",
				ProductID:         "product:loop-demo",
				ChangeEvent:       "Updated conversion screenshot.",
				ExpectedEffect:    "Conversion improves.",
				Metrics:           []string{"app_store_product_page_conversion"},
				ObservationWindow: "7d after change",
				Result:            "unknown",
				NextAction:        "review conversion evidence",
				EvidenceRefs:      []string{"fixture:conversion-plan"},
			},
		},
		EvidenceRefs: []string{"fixture:loop-product"},
	}
	if _, err := Apply(home, store, bundle, "fixture/product.yaml", loopTestNow.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	other := bundle
	other.Product.ID = "product:other-loop"
	other.Product.Name = "Other Loop"
	other.Product.Surfaces = []Surface{{ID: "landing", Kind: "web", Ref: "https://example.invalid/other-loop"}}
	other.Hypotheses = nil
	if _, err := Apply(home, store, other, "fixture/other-product.yaml", loopTestNow.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	applyLoopSignal(t, home, store, SignalDocument{
		SchemaVersion: SignalSchemaVersion,
		Source:        SignalSource{Kind: "google_search_console", Ref: "gsc:site:example.invalid:old"},
		Signal: Signal{
			ID:          "sig_loop_gsc_old",
			ProductID:   "product:loop-demo",
			Surface:     "landing",
			ChangeEvent: "Updated search title copy.",
			MetricName:  "gsc_clicks",
			Before:      100,
			After:       110,
			Window:      "2026-06-30/2026-07-01",
			Confidence:  0.5,
			EvidenceRefs: []string{
				"fixture:gsc-old",
			},
			ObservedAt: "2026-07-01T10:00:00Z",
		},
	})
	applyLoopSignal(t, home, store, SignalDocument{
		SchemaVersion: SignalSchemaVersion,
		Source:        SignalSource{Kind: "google_search_console", Ref: "gsc:site:example.invalid:new"},
		Signal: Signal{
			ID:          "sig_loop_gsc_new",
			ProductID:   "product:loop-demo",
			Surface:     "landing",
			ChangeEvent: "Updated search title copy.",
			MetricName:  "gsc_clicks",
			Before:      100,
			After:       140,
			Window:      "2026-07-01/2026-07-02",
			Confidence:  0.6,
			EvidenceRefs: []string{
				"fixture:gsc-new",
			},
			ObservedAt: "2026-07-02T10:00:00Z",
		},
	})
	applyLoopSignal(t, home, store, SignalDocument{
		SchemaVersion: SignalSchemaVersion,
		Source:        SignalSource{Kind: "posthog", Ref: "posthog:project:loop"},
		Signal: Signal{
			ID:           "sig_loop_unmatched",
			ProductID:    "product:loop-demo",
			Surface:      "landing",
			ChangeEvent:  "Published unmatched pricing copy.",
			MetricName:   "custom_unmatched_metric",
			Before:       4,
			After:        5,
			Window:       "2026-07-02",
			Confidence:   0.4,
			EvidenceRefs: []string{"fixture:unmatched"},
			ObservedAt:   "2026-07-02T11:00:00Z",
		},
	})
	applyLoopSignal(t, home, store, SignalDocument{
		SchemaVersion: SignalSchemaVersion,
		Source:        SignalSource{Kind: "app_store_connect", Ref: "asc:app:loop"},
		Signal: Signal{
			ID:           "sig_loop_conversion_reviewed",
			ProductID:    "product:loop-demo",
			Surface:      "app-store",
			ChangeEvent:  "Updated conversion screenshot.",
			MetricName:   "app_store_product_page_conversion",
			Before:       0.10,
			After:        0.14,
			Window:       "2026-06-25/2026-07-01",
			Confidence:   0.7,
			EvidenceRefs: []string{"fixture:conversion"},
			ObservedAt:   "2026-07-01T09:00:00Z",
		},
	})
	review := Review{
		SchemaVersion: ReviewSchemaVersion,
		ID:            "review_loop_conversion",
		ProductID:     "product:loop-demo",
		HypothesisID:  "hyp_loop_reviewed_conversion",
		Result:        "supported",
		Rationale:     "Reviewed conversion movement with confounders noted.",
		Confidence:    0.7,
		NextAction:    "keep measuring one more window",
		EvidenceRefs:  []string{"product_signal:sig_loop_conversion_reviewed", "fixture:review"},
		ReviewedBy:    "operator",
		ReviewedAt:    "2026-07-02T08:00:00Z",
	}
	if _, err := ApplyReview(home, store, review, "fixture/review.yaml", loopTestNow.Add(-4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RecordSignalDoctorNoPromote(home, SignalDoctorReport{
		SourceRef:     "fixture:bad-source",
		SourceHash:    "hash_loop_bad_source",
		SchemaVersion: SignalExportSchemaVersion,
		Status:        "fail",
		ProductID:     "product:loop-demo",
		SourceKind:    "google_search_console",
		Checks:        []SignalCheck{{Name: "strict_contract", Status: "fail", Detail: "synthetic invalid field"}},
	}, loopTestNow); err != nil {
		t.Fatal(err)
	}
	return home
}

func applyLoopSignal(t *testing.T, home string, store *mem.Store, doc SignalDocument) {
	t.Helper()
	if _, err := ApplySignal(home, store, doc, "fixture/"+doc.Signal.ID+".yaml", loopTestNow); err != nil {
		t.Fatal(err)
	}
}

func findProductLoop(products []ProductLoop, id string) ProductLoop {
	for _, product := range products {
		if product.ProductID == id {
			return product
		}
	}
	return ProductLoop{}
}

func hasLoopHypothesis(hypotheses []LoopHypothesis, id string) bool {
	return findLoopHypothesis(hypotheses, id).ID != ""
}

func findLoopHypothesis(hypotheses []LoopHypothesis, id string) LoopHypothesis {
	for _, hypothesis := range hypotheses {
		if hypothesis.ID == id {
			return hypothesis
		}
	}
	return LoopHypothesis{}
}

func findLoopMissingSignalSource(groups []LoopMissingSignalSource, source string) LoopMissingSignalSource {
	for _, group := range groups {
		if group.Source == source {
			return group
		}
	}
	return LoopMissingSignalSource{}
}

func findLoopSignalHypothesisGap(groups []LoopSignalHypothesisGap, changeEvent string) LoopSignalHypothesisGap {
	for _, group := range groups {
		if group.ChangeEvent == changeEvent {
			return group
		}
	}
	return LoopSignalHypothesisGap{}
}

func findLoopReviewNextAction(groups []LoopReviewNextAction, nextAction string) LoopReviewNextAction {
	for _, group := range groups {
		if group.NextAction == nextAction {
			return group
		}
	}
	return LoopReviewNextAction{}
}

func findLoopActionCard(cards []LoopActionCard, kind string) LoopActionCard {
	for _, card := range cards {
		if card.Kind == kind {
			return card
		}
	}
	return LoopActionCard{}
}

func seedCompletedProductActionWork(t *testing.T, home, productID, cardID, reviewID string) {
	t.Helper()
	evidenceRefs := []string{
		"product_action_card:" + cardID,
		"product_loop:" + productID,
	}
	if strings.TrimSpace(reviewID) != "" {
		evidenceRefs = append(evidenceRefs, "product_review:"+reviewID)
	}
	w := work.BuildWork(work.CreateInput{
		WorkID:           "work_loop_consumed_action",
		Owner:            productID,
		Actor:            "codex",
		State:            "completed",
		SuccessPredicate: "completed reviewed product action card",
		DesiredOutcome:   "product loop next-action card is consumed by WorkEnvelope evidence",
		EvidenceRefs:     evidenceRefs,
	}, loopTestNow.Add(time.Hour))
	w.IDs.TransitionID = "tr_loop_consumed_action"
	w.PolicyDecisionID = "pol_loop_consumed_action"
	w.ArtifactIDs = []string{"artifact:refs/loop-consumed-action.md"}
	w.VerificationIDs = []string{"ver_loop_consumed_action"}

	workDir := filepath.Join(home, "work", "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(w, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, w.ID+".json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
