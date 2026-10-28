package product

import (
	"os"
	"path/filepath"
	"testing"

	"compound/internal/mem"
)

func TestLoadGSCReportSignalsImportsLatestDateSnapshots(t *testing.T) {
	assertGoogleReportSignals(t, googleReportCase{
		name:       "gsc",
		sourcePath: "../../fixtures/product/gsc-report.json",
		sourceKind: "google_search_console",
		load: func(path string, metric string) ([]SignalDocument, error) {
			return LoadGSCReportSignals(path, GSCReportSignalOptions{ProductID: "product:google-demo", Site: "example.com", Metric: metric})
		},
		wantMetrics: []string{"gsc_clicks", "gsc_impressions", "gsc_ctr", "gsc_avg_position"},
		wantWindow:  "snapshot 2026-06-30",
		wantValues: map[string]float64{
			"gsc_clicks":       42,
			"gsc_impressions":  420,
			"gsc_ctr":          0.1,
			"gsc_avg_position": 3.4,
		},
		filter:           "position",
		filterWantMetric: "gsc_avg_position",
	})
}

func TestLoadGA4ReportSignalsImportsLatestDateSnapshots(t *testing.T) {
	assertGoogleReportSignals(t, googleReportCase{
		name:       "ga4",
		sourcePath: "../../fixtures/product/ga4-report.json",
		sourceKind: "google_analytics",
		load: func(path string, metric string) ([]SignalDocument, error) {
			return LoadGA4ReportSignals(path, GA4ReportSignalOptions{ProductID: "product:google-demo", Property: "properties/000000", Metric: metric})
		},
		wantMetrics: []string{"ga4_active_users", "ga4_sessions", "ga4_conversions"},
		wantWindow:  "snapshot 2026-06-30",
		wantValues: map[string]float64{
			"ga4_active_users": 84,
			"ga4_sessions":     126,
			"ga4_conversions":  7,
		},
		filter:           "sessions",
		filterWantMetric: "ga4_sessions",
	})
}

func TestLoadAdSenseReportSignalsImportsLatestDateSnapshots(t *testing.T) {
	assertGoogleReportSignals(t, googleReportCase{
		name:       "adsense",
		sourcePath: "../../fixtures/product/adsense-report.json",
		sourceKind: "google_adsense",
		load: func(path string, metric string) ([]SignalDocument, error) {
			return LoadAdSenseReportSignals(path, AdSenseReportSignalOptions{ProductID: "product:google-demo", Account: "pub-demo-account", Metric: metric})
		},
		wantMetrics: []string{"adsense_estimated_earnings", "adsense_page_rpm", "adsense_impressions"},
		wantWindow:  "snapshot 2026-06-30",
		wantValues: map[string]float64{
			"adsense_estimated_earnings": 12.34,
			"adsense_page_rpm":           4.56,
			"adsense_impressions":        2700,
		},
		filter:           "page_rpm",
		filterWantMetric: "adsense_page_rpm",
	})
}

func TestGoogleReportSignalsRejectInvalidShapes(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		body string
		load func(string) ([]SignalDocument, error)
	}{
		{
			name: "gsc_mismatched_site",
			body: `{"site":"wrong.example","rows":[{"date":"2026-06-30","clicks":1,"impressions":2,"ctr":0.5,"position":1.2}]}`,
			load: func(path string) ([]SignalDocument, error) {
				return LoadGSCReportSignals(path, GSCReportSignalOptions{ProductID: "product:google-demo", Site: "example.com"})
			},
		},
		{
			name: "ga4_missing_metric",
			body: `{"property":"properties/000000","rows":[{"date":"2026-06-30","active_users":1,"sessions":2}]}`,
			load: func(path string) ([]SignalDocument, error) {
				return LoadGA4ReportSignals(path, GA4ReportSignalOptions{ProductID: "product:google-demo", Property: "properties/000000"})
			},
		},
		{
			name: "adsense_bad_date",
			body: `{"account":"pub-demo-account","rows":[{"date":"06/30/2026","estimated_earnings":1.2,"page_rpm":3.4,"impressions":5}]}`,
			load: func(path string) ([]SignalDocument, error) {
				return LoadAdSenseReportSignals(path, AdSenseReportSignalOptions{ProductID: "product:google-demo", Account: "pub-demo-account"})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, tt.name+".json")
			if err := os.WriteFile(path, []byte(tt.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := tt.load(path); err == nil {
				t.Fatal("invalid Google report shape should be rejected")
			}
		})
	}
}

type googleReportCase struct {
	name             string
	sourcePath       string
	sourceKind       string
	load             func(path string, metric string) ([]SignalDocument, error)
	wantMetrics      []string
	wantWindow       string
	wantValues       map[string]float64
	filter           string
	filterWantMetric string
}

func assertGoogleReportSignals(t *testing.T, tc googleReportCase) {
	t.Helper()
	docs, err := tc.load(tc.sourcePath, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != len(tc.wantMetrics) {
		t.Fatalf("%s docs = %d; want %d", tc.name, len(docs), len(tc.wantMetrics))
	}
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range docs {
		if err := ValidateSignalDocument(doc); err != nil {
			t.Fatal(err)
		}
		if doc.Source.Kind != tc.sourceKind {
			t.Fatalf("source kind = %q", doc.Source.Kind)
		}
		if doc.Signal.Before != doc.Signal.After {
			t.Fatalf("signal should be neutral before=after: %+v", doc.Signal)
		}
		if doc.Signal.Window != tc.wantWindow {
			t.Fatalf("window = %q; want %q", doc.Signal.Window, tc.wantWindow)
		}
		if _, err := ApplySignal(home, store, doc, tc.sourcePath, tProduct); err != nil {
			t.Fatal(err)
		}
	}
	tl, err := LoadTimeline(home, "product:google-demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Signals) != len(tc.wantMetrics) {
		t.Fatalf("timeline signals = %+v", tl.Signals)
	}
	if len(tl.Transitions) != len(tc.wantMetrics) {
		t.Fatalf("timeline transitions = %+v", tl.Transitions)
	}
	for _, metric := range tc.wantMetrics {
		s := findSignalByMetric(tl.Signals, metric)
		if s.ID == "" {
			t.Fatalf("missing metric %q in %+v", metric, tl.Signals)
		}
		if s.Before != tc.wantValues[metric] || s.After != tc.wantValues[metric] {
			t.Fatalf("%s value = %.4g -> %.4g; want %.4g", metric, s.Before, s.After, tc.wantValues[metric])
		}
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("Google signal import should not promote claims ledger: %v", err)
	}
	filtered, err := tc.load(tc.sourcePath, tc.filter)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].Signal.MetricName != tc.filterWantMetric {
		t.Fatalf("filtered docs = %+v", filtered)
	}
}

func findSignalByMetric(signals []Signal, metric string) Signal {
	for _, signal := range signals {
		if signal.MetricName == metric {
			return signal
		}
	}
	return Signal{}
}
