package product

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func v2FunnelTestRow(date, metric string, value any) map[string]any {
	key, name, population := "app_store_discovery_and_engagement_standard", "App Store Discovery and Engagement Standard", "published_impression_counts"
	if metric == "first_time_downloads" {
		key, name, population = "app_downloads_standard", "App Downloads Standard", "published_first_time_download_counts"
	} else if metric == "proceeds_usd" {
		key, name, population = "app_store_purchases_standard", "App Store Purchases Standard", "published_proceeds_usd"
	} else if metric == "product_page_views" {
		population = "published_product_page_view_counts"
	}
	return map[string]any{
		"schema_version": "compound.asc_app_daily.v2", "asc_app_id": "123456789", "date": date,
		metric: value, "coverage": map[string]any{"complete_funnel": false},
		"metric_evidence": map[string]any{metric: map[string]any{
			"status": "observed", "reason": nil, "population": population,
			"source_report_key": key, "matched_rows": 1,
		}},
		"source_reports": map[string]any{key: map[string]any{
			"report_id": "report-neutral", "report_name": name, "instance_id": "instance-neutral-" + date,
			"processing_date": date, "granularity": "DAILY", "segment_inventory_complete": true,
			"expected_segment_ids": []string{"segment-neutral"},
			"segments":             []map[string]any{{"id": "segment-neutral", "sha256": strings.Repeat("a", 64), "byte_count": 100}},
		}},
	}
}

func writeV2FunnelTestRows(t *testing.T, rows ...map[string]any) string {
	t.Helper()
	var b strings.Builder
	for _, row := range rows {
		body, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(body)
		b.WriteByte('\n')
	}
	path := filepath.Join(t.TempDir(), "daily.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func v2FunnelTestOptions(metric string) ASCFunnelSeriesSignalOptions {
	return ASCFunnelSeriesSignalOptions{ProductID: "product:neutral", AppID: "123456789", Metric: metric,
		BeforeWindow: "2026-07-01/2026-07-01", AfterWindow: "2026-07-08/2026-07-08", ChangeEvent: "Synthetic change."}
}

func TestASCFunnelV2ObservedZeroAndSignedUSD(t *testing.T) {
	for _, tc := range []struct {
		metric        string
		before, after float64
	}{
		{"impressions", 0, 4}, {"proceeds_usd", -2.5, -1.75},
	} {
		t.Run(tc.metric, func(t *testing.T) {
			path := writeV2FunnelTestRows(t, v2FunnelTestRow("2026-07-01", tc.metric, tc.before), v2FunnelTestRow("2026-07-08", tc.metric, tc.after))
			doc, err := LoadASCFunnelSeriesSignal(path, v2FunnelTestOptions(tc.metric))
			if err != nil {
				t.Fatal(err)
			}
			if doc.Signal.Before != tc.before || doc.Signal.After != tc.after {
				t.Fatalf("wrong values: %+v", doc.Signal)
			}
		})
	}
}

func TestASCFunnelV2RejectsUnprovenMetricAndSnapshotEvidence(t *testing.T) {
	tests := map[string]func(map[string]any){
		"legacy": func(r map[string]any) {
			delete(r, "schema_version")
			r["coverage"] = map[string]any{"complete_funnel": true}
		},
		"boolean value":     func(r map[string]any) { r["impressions"] = true },
		"null":              func(r map[string]any) { r["impressions"] = nil },
		"missing value":     func(r map[string]any) { delete(r, "impressions") },
		"missing evidence":  func(r map[string]any) { delete(r, "metric_evidence") },
		"negative count":    func(r map[string]any) { r["impressions"] = -1 },
		"unknown status":    func(r map[string]any) { v2MetricEvidence(r)["status"] = "unknown" },
		"suppressed reason": func(r map[string]any) { v2MetricEvidence(r)["reason"] = "suppressed_or_invalid" },
		"wrong population":  func(r map[string]any) { v2MetricEvidence(r)["population"] = "unique_people" },
		"no matching row":   func(r map[string]any) { v2MetricEvidence(r)["matched_rows"] = 0 },
		"missing source":    func(r map[string]any) { delete(r, "source_reports") },
		"wrong report":      func(r map[string]any) { v2SourceEvidence(r)["report_name"] = "App Downloads Standard" },
		"open inventory":    func(r map[string]any) { v2SourceEvidence(r)["segment_inventory_complete"] = false },
		"empty inventory":   func(r map[string]any) { v2SourceEvidence(r)["expected_segment_ids"] = []string{} },
		"duplicate expected": func(r map[string]any) {
			v2SourceEvidence(r)["expected_segment_ids"] = []string{"segment-neutral", "segment-neutral"}
		},
		"unfetched segment": func(r map[string]any) {
			v2SourceEvidence(r)["expected_segment_ids"] = []string{"segment-neutral", "missing"}
		},
		"bad hash":                func(r map[string]any) { v2SourceEvidence(r)["segments"].([]map[string]any)[0]["sha256"] = "not-a-hash" },
		"zero byte body":          func(r map[string]any) { v2SourceEvidence(r)["segments"].([]map[string]any)[0]["byte_count"] = 0 },
		"wrong granularity":       func(r map[string]any) { v2SourceEvidence(r)["granularity"] = "WEEKLY" },
		"invalid processing date": func(r map[string]any) { v2SourceEvidence(r)["processing_date"] = "2026-02-30" },
		"invalid row date":        func(r map[string]any) { r["date"] = "2026-02-30" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			row := v2FunnelTestRow("2026-07-01", "impressions", 5)
			mutate(row)
			path := writeV2FunnelTestRows(t, row, v2FunnelTestRow("2026-07-08", "impressions", 6))
			if _, err := LoadASCFunnelSeriesSignal(path, v2FunnelTestOptions("impressions")); err == nil {
				t.Fatal("unproven value admitted")
			}
		})
	}
}

func v2MetricEvidence(r map[string]any) map[string]any {
	return r["metric_evidence"].(map[string]any)["impressions"].(map[string]any)
}
func v2SourceEvidence(r map[string]any) map[string]any {
	return r["source_reports"].(map[string]any)["app_store_discovery_and_engagement_standard"].(map[string]any)
}

func TestASCFunnelV2RequiresEveryInclusiveCalendarDay(t *testing.T) {
	rows := []map[string]any{}
	for _, date := range []string{"2026-07-01", "2026-07-03", "2026-07-08", "2026-07-09", "2026-07-10"} {
		rows = append(rows, v2FunnelTestRow(date, "impressions", 10))
	}
	opts := v2FunnelTestOptions("impressions")
	opts.BeforeWindow, opts.AfterWindow = "2026-07-01/2026-07-03", "2026-07-08/2026-07-10"
	if _, err := LoadASCFunnelSeriesSignal(writeV2FunnelTestRows(t, rows...), opts); err == nil || !strings.Contains(err.Error(), "2026-07-02") {
		t.Fatalf("missing interior day not identified: %v", err)
	}
	rows = append(rows, v2FunnelTestRow("2026-07-02", "impressions", 0))
	doc, err := LoadASCFunnelSeriesSignal(writeV2FunnelTestRows(t, rows...), opts)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Signal.Before != 20 || doc.Signal.After != 30 {
		t.Fatalf("complete window values wrong: %+v", doc.Signal)
	}
}

func TestASCFunnelV2RejectsDuplicateAppDate(t *testing.T) {
	row := v2FunnelTestRow("2026-07-01", "impressions", 5)
	path := writeV2FunnelTestRows(t, row, row, v2FunnelTestRow("2026-07-08", "impressions", 6))
	if _, err := LoadASCFunnelSeriesSignal(path, v2FunnelTestOptions("impressions")); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate admitted: %v", err)
	}
}

func TestASCFunnelV2ExplicitlyRejectsObsoleteConversionAndMissingMetric(t *testing.T) {
	path := writeV2FunnelTestRows(t, v2FunnelTestRow("2026-07-01", "impressions", 5), v2FunnelTestRow("2026-07-08", "impressions", 6))
	for _, metric := range []string{"product_page_conversion_rate", "app_store_product_page_conversion", "", "paying_users"} {
		_, err := LoadASCFunnelSeriesSignal(path, v2FunnelTestOptions(metric))
		if err == nil {
			t.Fatalf("%q silently substituted or accepted", metric)
		}
		if strings.Contains(metric, "conversion") && !strings.Contains(err.Error(), "unmatched") {
			t.Fatalf("%q does not explain unmatched population: %v", metric, err)
		}
	}
}

func TestASCFunnelV2MatchesSharedProducerSchema(t *testing.T) {
	body, err := os.ReadFile("../../fixtures/product/asc-daily-v2-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var shared struct {
		SchemaVersion string            `json:"schema_version"`
		Populations   map[string]string `json:"observed_populations"`
	}
	if err := json.Unmarshal(body, &shared); err != nil {
		t.Fatal(err)
	}
	if shared.SchemaVersion != ascFunnelRowSchemaV2 {
		t.Fatal("producer/consumer schema drift")
	}
	for _, metric := range []string{"impressions", "product_page_views", "first_time_downloads", "proceeds_usd"} {
		contract, err := ascFunnelMetric(metric)
		if err != nil {
			t.Fatal(err)
		}
		if contract.Population != shared.Populations[metric] {
			t.Fatalf("population drift for %s", metric)
		}
	}
}
