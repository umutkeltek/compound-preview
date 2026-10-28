package product

import (
	"os"
	"path/filepath"
	"testing"
)

func ph(v float64) *float64 { return &v }

func TestAdaptPostHogReportSignalsDeltaAcrossTwoDates(t *testing.T) {
	doc := posthogReportDocument{
		Project: "proj_demo",
		Rows: []posthogReportRow{
			{Date: "2026-07-14", Signups: ph(5), ActivationRate: ph(0.20), ActiveUsers: ph(40)},
			{Date: "2026-07-21", Signups: ph(14), ActivationRate: ph(0.31), ActiveUsers: ph(70)},
		},
	}
	docs, err := adaptPostHogReportSignals(doc, PostHogReportSignalOptions{ProductID: "product:demo", Project: "proj_demo", Metric: "signups"})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 signal, got %d", len(docs))
	}
	sig := docs[0].Signal
	// The distribution loop watches posthog_signups; the fallback canonical name must land there.
	if sig.MetricName != "posthog_signups" {
		t.Fatalf("expected posthog_signups, got %s", sig.MetricName)
	}
	if sig.Before != 5 || sig.After != 14 {
		t.Fatalf("expected before=5 after=14, got before=%.3f after=%.3f", sig.Before, sig.After)
	}
	if sig.Window != "2026-07-14 -> 2026-07-21" {
		t.Fatalf("expected a two-date window, got %q", sig.Window)
	}
	if docs[0].Source.Kind != "posthog" {
		t.Fatalf("expected source kind posthog, got %s", docs[0].Source.Kind)
	}
	if len(sig.EvidenceRefs) == 0 {
		t.Fatal("expected evidence refs")
	}
}

func TestAdaptPostHogReportSignalsSingleDateIsSnapshot(t *testing.T) {
	doc := posthogReportDocument{
		Project: "proj_demo",
		Rows:    []posthogReportRow{{Date: "2026-07-21", Signups: ph(9)}},
	}
	docs, err := adaptPostHogReportSignals(doc, PostHogReportSignalOptions{ProductID: "product:demo", Project: "proj_demo", Metric: "signups"})
	if err != nil {
		t.Fatal(err)
	}
	sig := docs[0].Signal
	if sig.Before != 9 || sig.After != 9 {
		t.Fatalf("single-date must degrade to snapshot (before==after), got %.3f -> %.3f", sig.Before, sig.After)
	}
	if sig.Window != "snapshot 2026-07-21" {
		t.Fatalf("expected snapshot window, got %q", sig.Window)
	}
}

func TestAdaptPostHogReportSignalsAveragesRateMetric(t *testing.T) {
	doc := posthogReportDocument{
		Project: "proj_demo",
		Rows: []posthogReportRow{
			{Date: "2026-07-21", ActivationRate: ph(0.20)},
			{Date: "2026-07-21", ActivationRate: ph(0.40)},
		},
	}
	docs, err := adaptPostHogReportSignals(doc, PostHogReportSignalOptions{ProductID: "product:demo", Project: "proj_demo", Metric: "activation_rate"})
	if err != nil {
		t.Fatal(err)
	}
	if diff := docs[0].Signal.After - 0.30; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("rate must average same-date rows to 0.30, got %.6f", docs[0].Signal.After)
	}
	if docs[0].Signal.MetricName != "posthog_activation_rate" {
		t.Fatalf("expected aliased posthog_activation_rate, got %s", docs[0].Signal.MetricName)
	}
}

func TestAdaptPostHogReportSignalsProjectMismatch(t *testing.T) {
	doc := posthogReportDocument{Project: "proj_other", Rows: []posthogReportRow{{Date: "2026-07-21", Signups: ph(1)}}}
	if _, err := adaptPostHogReportSignals(doc, PostHogReportSignalOptions{ProductID: "product:demo", Project: "proj_demo"}); err == nil {
		t.Fatal("expected project mismatch error")
	}
}

func TestAdaptPostHogReportSignalsEmptyRows(t *testing.T) {
	doc := posthogReportDocument{Project: "proj_demo"}
	if _, err := adaptPostHogReportSignals(doc, PostHogReportSignalOptions{ProductID: "product:demo", Project: "proj_demo"}); err == nil {
		t.Fatal("expected empty rows error")
	}
}

func TestAdaptPostHogReportSignalsRejectsUnknownMetric(t *testing.T) {
	doc := posthogReportDocument{Project: "proj_demo", Rows: []posthogReportRow{{Date: "2026-07-21", Signups: ph(1)}}}
	if _, err := adaptPostHogReportSignals(doc, PostHogReportSignalOptions{ProductID: "product:demo", Project: "proj_demo", Metric: "churn"}); err == nil {
		t.Fatal("expected unsupported metric error")
	}
}

func TestLoadPostHogReportSignalsFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "posthog-report.json")
	body := `{"project":"proj_demo","rows":[` +
		`{"date":"2026-07-14","signups":3,"activation_rate":0.2,"active_users":30},` +
		`{"date":"2026-07-21","signups":11,"activation_rate":0.3,"active_users":55}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	docs, err := LoadPostHogReportSignals(path, PostHogReportSignalOptions{ProductID: "product:demo", Project: "proj_demo"})
	if err != nil {
		t.Fatal(err)
	}
	// Default expands to all three metrics.
	if len(docs) != 3 {
		t.Fatalf("expected 3 default metric signals, got %d", len(docs))
	}
	var signups *SignalDocument
	for i := range docs {
		if docs[i].Signal.MetricName == "posthog_signups" {
			signups = &docs[i]
		}
	}
	if signups == nil {
		t.Fatal("expected a posthog_signups signal in defaults")
	}
	if signups.Signal.Before != 3 || signups.Signal.After != 11 {
		t.Fatalf("expected 3 -> 11, got %.3f -> %.3f", signups.Signal.Before, signups.Signal.After)
	}
}
