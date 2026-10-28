package product

import (
	"strings"
	"testing"
)

const ascFunnelFixture = "../../fixtures/product/asc-funnel-series.jsonl"

func TestLoadASCFunnelSeriesSignalAggregatesCompleteObservedCalendarWindows(t *testing.T) {
	doc, err := LoadASCFunnelSeriesSignal(ascFunnelFixture, ASCFunnelSeriesSignalOptions{
		ProductID: "product:focus-notes", AppID: "123456789", Metric: "impressions",
		BeforeWindow: "2026-07-01/2026-07-03", AfterWindow: "2026-07-08/2026-07-10", ChangeEvent: "Synthetic change.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSignalDocument(doc); err != nil {
		t.Fatal(err)
	}
	if doc.Signal.Before != 300 || doc.Signal.After != 400 {
		t.Fatalf("wrong published values: %+v", doc.Signal)
	}
	for _, ref := range []string{"asc:funnel_series:before_observed_metric_days:3", "asc:funnel_series:after_observed_metric_days:3"} {
		if !hasString(doc.Signal.EvidenceRefs, ref) {
			t.Fatalf("missing complete-window evidence %s", ref)
		}
	}
}

func TestLoadASCFunnelSeriesSignalMatchesByAppIDNotDisplayName(t *testing.T) {
	doc, err := LoadASCFunnelSeriesSignal(ascFunnelFixture, ASCFunnelSeriesSignalOptions{
		ProductID: "product:focus-notes", AppID: "123456789", Metric: "impressions",
		BeforeWindow: "2026-07-01/2026-07-01", AfterWindow: "2026-07-03/2026-07-03", ChangeEvent: "Synthetic change.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Signal.Before != 100 || doc.Signal.After != 200 {
		t.Fatalf("name-colliding app leaked: %+v", doc.Signal)
	}
}

func TestLoadASCFunnelSeriesSignalRejectsUnknownAndLegacyDays(t *testing.T) {
	for _, tc := range []struct{ app, before string }{{"987654321", "2026-07-02/2026-07-02"}, {"123456789", "2026-06-15/2026-06-15"}} {
		_, err := LoadASCFunnelSeriesSignal(ascFunnelFixture, ASCFunnelSeriesSignalOptions{ProductID: "product:neutral", AppID: tc.app, Metric: "impressions", BeforeWindow: tc.before, AfterWindow: "2026-07-01/2026-07-01", ChangeEvent: "Synthetic change."})
		if err == nil {
			t.Fatal("unknown/legacy day admitted")
		}
	}
}

func TestLoadASCFunnelSeriesSignalRejectsUnsupportedMetric(t *testing.T) {
	_, err := LoadASCFunnelSeriesSignal(ascFunnelFixture, ASCFunnelSeriesSignalOptions{ProductID: "product:neutral", AppID: "123456789", Metric: "not_a_metric", BeforeWindow: "2026-07-01/2026-07-03", AfterWindow: "2026-07-08/2026-07-10", ChangeEvent: "Synthetic change."})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("wrong unsupported metric result: %v", err)
	}
}
