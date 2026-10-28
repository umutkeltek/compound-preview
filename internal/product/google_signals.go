package product

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

type GSCReportSignalOptions struct {
	ProductID string
	Site      string
	Metric    string
}

type GA4ReportSignalOptions struct {
	ProductID string
	Property  string
	Metric    string
}

type AdSenseReportSignalOptions struct {
	ProductID string
	Account   string
	Metric    string
}

type gscReportDocument struct {
	Site string         `json:"site"`
	Rows []gscReportRow `json:"rows"`
}

type gscReportRow struct {
	Date        string   `json:"date"`
	Clicks      *float64 `json:"clicks"`
	Impressions *float64 `json:"impressions"`
	CTR         *float64 `json:"ctr"`
	Position    *float64 `json:"position"`
}

type ga4ReportDocument struct {
	Property string         `json:"property"`
	Rows     []ga4ReportRow `json:"rows"`
}

type ga4ReportRow struct {
	Date        string   `json:"date"`
	ActiveUsers *float64 `json:"active_users"`
	Sessions    *float64 `json:"sessions"`
	Conversions *float64 `json:"conversions"`
}

type adsenseReportDocument struct {
	Account string             `json:"account"`
	Rows    []adsenseReportRow `json:"rows"`
}

type adsenseReportRow struct {
	Date              string   `json:"date"`
	EstimatedEarnings *float64 `json:"estimated_earnings"`
	PageRPM           *float64 `json:"page_rpm"`
	Impressions       *float64 `json:"impressions"`
}

func LoadGSCReportSignals(path string, opts GSCReportSignalOptions) ([]SignalDocument, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc gscReportDocument
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse GSC report %s: %w", path, err)
	}
	return adaptGSCReportSignals(doc, opts)
}

func LoadGA4ReportSignals(path string, opts GA4ReportSignalOptions) ([]SignalDocument, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc ga4ReportDocument
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse GA4 report %s: %w", path, err)
	}
	return adaptGA4ReportSignals(doc, opts)
}

func LoadAdSenseReportSignals(path string, opts AdSenseReportSignalOptions) ([]SignalDocument, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc adsenseReportDocument
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse AdSense report %s: %w", path, err)
	}
	return adaptAdSenseReportSignals(doc, opts)
}

func adaptGSCReportSignals(doc gscReportDocument, opts GSCReportSignalOptions) ([]SignalDocument, error) {
	productID := strings.TrimSpace(opts.ProductID)
	site := strings.TrimSpace(opts.Site)
	if productID == "" || site == "" {
		return nil, fmt.Errorf("GSC report product_id site required")
	}
	if strings.TrimSpace(doc.Site) == "" {
		return nil, fmt.Errorf("GSC report site required")
	}
	if strings.TrimSpace(doc.Site) != site {
		return nil, fmt.Errorf("GSC report site %q does not match %q", doc.Site, site)
	}
	rows, date, err := latestGSCRows(doc.Rows)
	if err != nil {
		return nil, err
	}
	metrics, err := selectedGoogleMetrics(opts.Metric, []string{"clicks", "impressions", "ctr", "avg_position"}, normalizeGSCMetric, "GSC report")
	if err != nil {
		return nil, err
	}
	docs := make([]SignalDocument, 0, len(metrics))
	for _, metric := range metrics {
		value, err := aggregateGSCMetric(rows, metric, date)
		if err != nil {
			return nil, err
		}
		docs = append(docs, googleSnapshotSignal(googleSnapshotInput{
			ProductID: productID,
			Provider:  "google_search_console",
			SourceRef: "gsc:search_analytics:" + site + ":" + date + ":" + metric,
			SourceID:  site,
			Surface:   "search",
			Metric:    metric,
			Value:     value,
			Date:      date,
			Change:    "Google Search Console report snapshot observed",
			Refs: []string{
				"gsc:search_analytics:" + date + ":" + site,
				"gsc:site:" + site,
				fmt.Sprintf("gsc:%s:%.4g", metric, value),
			},
		}))
	}
	return docs, nil
}

func adaptGA4ReportSignals(doc ga4ReportDocument, opts GA4ReportSignalOptions) ([]SignalDocument, error) {
	productID := strings.TrimSpace(opts.ProductID)
	property := strings.TrimSpace(opts.Property)
	if productID == "" || property == "" {
		return nil, fmt.Errorf("GA4 report product_id property required")
	}
	if strings.TrimSpace(doc.Property) == "" {
		return nil, fmt.Errorf("GA4 report property required")
	}
	if strings.TrimSpace(doc.Property) != property {
		return nil, fmt.Errorf("GA4 report property %q does not match %q", doc.Property, property)
	}
	rows, date, err := latestGA4Rows(doc.Rows)
	if err != nil {
		return nil, err
	}
	metrics, err := selectedGoogleMetrics(opts.Metric, []string{"active_users", "sessions", "conversions"}, normalizeIDPart, "GA4 report")
	if err != nil {
		return nil, err
	}
	docs := make([]SignalDocument, 0, len(metrics))
	for _, metric := range metrics {
		value, err := aggregateGA4Metric(rows, metric, date)
		if err != nil {
			return nil, err
		}
		docs = append(docs, googleSnapshotSignal(googleSnapshotInput{
			ProductID: productID,
			Provider:  "google_analytics",
			SourceRef: "ga4:run_report:" + property + ":" + date + ":" + metric,
			SourceID:  property,
			Surface:   "web_analytics",
			Metric:    metric,
			Value:     value,
			Date:      date,
			Change:    "Google Analytics report snapshot observed",
			Refs: []string{
				"ga4:run_report:" + date + ":" + property,
				"ga4:property:" + property,
				fmt.Sprintf("ga4:%s:%.4g", metric, value),
			},
		}))
	}
	return docs, nil
}

func adaptAdSenseReportSignals(doc adsenseReportDocument, opts AdSenseReportSignalOptions) ([]SignalDocument, error) {
	productID := strings.TrimSpace(opts.ProductID)
	account := strings.TrimSpace(opts.Account)
	if productID == "" || account == "" {
		return nil, fmt.Errorf("AdSense report product_id account required")
	}
	if strings.TrimSpace(doc.Account) == "" {
		return nil, fmt.Errorf("AdSense report account required")
	}
	if strings.TrimSpace(doc.Account) != account {
		return nil, fmt.Errorf("AdSense report account %q does not match %q", doc.Account, account)
	}
	rows, date, err := latestAdSenseRows(doc.Rows)
	if err != nil {
		return nil, err
	}
	metrics, err := selectedGoogleMetrics(opts.Metric, []string{"estimated_earnings", "page_rpm", "impressions"}, normalizeIDPart, "AdSense report")
	if err != nil {
		return nil, err
	}
	docs := make([]SignalDocument, 0, len(metrics))
	for _, metric := range metrics {
		value, err := aggregateAdSenseMetric(rows, metric, date)
		if err != nil {
			return nil, err
		}
		docs = append(docs, googleSnapshotSignal(googleSnapshotInput{
			ProductID: productID,
			Provider:  "google_adsense",
			SourceRef: "adsense:report:" + account + ":" + date + ":" + metric,
			SourceID:  account,
			Surface:   "monetization",
			Metric:    metric,
			Value:     value,
			Date:      date,
			Change:    "Google AdSense report snapshot observed",
			Refs: []string{
				"adsense:report:" + date + ":" + account,
				"adsense:account:" + account,
				fmt.Sprintf("adsense:%s:%.4g", metric, value),
			},
		}))
	}
	return docs, nil
}

func selectedGoogleMetrics(raw string, defaults []string, normalize func(string) string, label string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return defaults, nil
	}
	metric := normalize(raw)
	for _, allowed := range defaults {
		if metric == allowed {
			return []string{metric}, nil
		}
	}
	return nil, fmt.Errorf("%s metric %q unsupported", label, raw)
}

func normalizeGSCMetric(metric string) string {
	switch normalizeIDPart(metric) {
	case "position", "average_position", "avg_position":
		return "avg_position"
	default:
		return normalizeIDPart(metric)
	}
}

func latestGSCRows(rows []gscReportRow) ([]gscReportRow, string, error) {
	if len(rows) == 0 {
		return nil, "", fmt.Errorf("GSC report rows required")
	}
	var latest time.Time
	var latestRaw string
	var out []gscReportRow
	for i, row := range rows {
		date, parsed, err := parseGoogleReportDate(row.Date)
		if err != nil {
			return nil, "", fmt.Errorf("GSC report row %d: %w", i+1, err)
		}
		if latestRaw == "" || parsed.After(latest) {
			latest = parsed
			latestRaw = date
			out = []gscReportRow{row}
			continue
		}
		if parsed.Equal(latest) {
			out = append(out, row)
		}
	}
	return out, latestRaw, nil
}

func latestGA4Rows(rows []ga4ReportRow) ([]ga4ReportRow, string, error) {
	if len(rows) == 0 {
		return nil, "", fmt.Errorf("GA4 report rows required")
	}
	var latest time.Time
	var latestRaw string
	var out []ga4ReportRow
	for i, row := range rows {
		date, parsed, err := parseGoogleReportDate(row.Date)
		if err != nil {
			return nil, "", fmt.Errorf("GA4 report row %d: %w", i+1, err)
		}
		if latestRaw == "" || parsed.After(latest) {
			latest = parsed
			latestRaw = date
			out = []ga4ReportRow{row}
			continue
		}
		if parsed.Equal(latest) {
			out = append(out, row)
		}
	}
	return out, latestRaw, nil
}

func latestAdSenseRows(rows []adsenseReportRow) ([]adsenseReportRow, string, error) {
	if len(rows) == 0 {
		return nil, "", fmt.Errorf("AdSense report rows required")
	}
	var latest time.Time
	var latestRaw string
	var out []adsenseReportRow
	for i, row := range rows {
		date, parsed, err := parseGoogleReportDate(row.Date)
		if err != nil {
			return nil, "", fmt.Errorf("AdSense report row %d: %w", i+1, err)
		}
		if latestRaw == "" || parsed.After(latest) {
			latest = parsed
			latestRaw = date
			out = []adsenseReportRow{row}
			continue
		}
		if parsed.Equal(latest) {
			out = append(out, row)
		}
	}
	return out, latestRaw, nil
}

func parseGoogleReportDate(raw string) (string, time.Time, error) {
	date := strings.TrimSpace(raw)
	if date == "" {
		return "", time.Time{}, fmt.Errorf("date required")
	}
	parsed, err := time.Parse("2006-01-02", date)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("date must be YYYY-MM-DD")
	}
	return date, parsed, nil
}

func aggregateGSCMetric(rows []gscReportRow, metric, date string) (float64, error) {
	var sum float64
	for _, row := range rows {
		value, err := gscMetricValue(row, metric, date)
		if err != nil {
			return 0, err
		}
		sum += value
	}
	if metric == "ctr" || metric == "avg_position" {
		return sum / float64(len(rows)), nil
	}
	return sum, nil
}

func aggregateGA4Metric(rows []ga4ReportRow, metric, date string) (float64, error) {
	var sum float64
	for _, row := range rows {
		value, err := ga4MetricValue(row, metric, date)
		if err != nil {
			return 0, err
		}
		sum += value
	}
	return sum, nil
}

func aggregateAdSenseMetric(rows []adsenseReportRow, metric, date string) (float64, error) {
	var sum float64
	for _, row := range rows {
		value, err := adsenseMetricValue(row, metric, date)
		if err != nil {
			return 0, err
		}
		sum += value
	}
	if metric == "page_rpm" {
		return sum / float64(len(rows)), nil
	}
	return sum, nil
}

func gscMetricValue(row gscReportRow, metric, date string) (float64, error) {
	switch metric {
	case "clicks":
		return requiredGoogleMetric(row.Clicks, "GSC report", metric, date)
	case "impressions":
		return requiredGoogleMetric(row.Impressions, "GSC report", metric, date)
	case "ctr":
		return requiredGoogleMetric(row.CTR, "GSC report", metric, date)
	case "avg_position":
		return requiredGoogleMetric(row.Position, "GSC report", metric, date)
	default:
		return 0, fmt.Errorf("GSC report metric %q unsupported", metric)
	}
}

func ga4MetricValue(row ga4ReportRow, metric, date string) (float64, error) {
	switch metric {
	case "active_users":
		return requiredGoogleMetric(row.ActiveUsers, "GA4 report", metric, date)
	case "sessions":
		return requiredGoogleMetric(row.Sessions, "GA4 report", metric, date)
	case "conversions":
		return requiredGoogleMetric(row.Conversions, "GA4 report", metric, date)
	default:
		return 0, fmt.Errorf("GA4 report metric %q unsupported", metric)
	}
}

func adsenseMetricValue(row adsenseReportRow, metric, date string) (float64, error) {
	switch metric {
	case "estimated_earnings":
		return requiredGoogleMetric(row.EstimatedEarnings, "AdSense report", metric, date)
	case "page_rpm":
		return requiredGoogleMetric(row.PageRPM, "AdSense report", metric, date)
	case "impressions":
		return requiredGoogleMetric(row.Impressions, "AdSense report", metric, date)
	default:
		return 0, fmt.Errorf("AdSense report metric %q unsupported", metric)
	}
}

func requiredGoogleMetric(value *float64, label, metric, date string) (float64, error) {
	if value == nil {
		return 0, fmt.Errorf("%s metric %q missing for date %s", label, metric, date)
	}
	return *value, nil
}

type googleSnapshotInput struct {
	ProductID string
	Provider  string
	SourceRef string
	SourceID  string
	Surface   string
	Metric    string
	Value     float64
	Date      string
	Change    string
	Refs      []string
}

func googleSnapshotSignal(in googleSnapshotInput) SignalDocument {
	metricName := canonicalMetricName(in.Provider, in.Metric)
	evidenceRefs := compact(in.Refs)
	return SignalDocument{
		SchemaVersion: SignalSchemaVersion,
		Source: SignalSource{
			Kind: in.Provider,
			Ref:  in.SourceRef,
		},
		Signal: Signal{
			ID:          stableID("sig_", in.ProductID, in.Provider, in.SourceID, metricName, in.Date),
			ProductID:   in.ProductID,
			Surface:     in.Surface,
			ChangeEvent: in.Change,
			MetricName:  metricName,
			Before:      in.Value,
			After:       in.Value,
			Window:      "snapshot " + in.Date,
			Confidence:  0.72,
			Confounders: []string{
				"snapshot observation; no causal movement is implied",
				"provider reporting can lag or sample data",
			},
			Interpretations: []Interpretation{{
				Lens:  "provider_snapshot",
				Claim: "records observed Google provider metric without causal attribution",
			}},
			NextAction:   "Use as product feedback evidence; do not infer causality without reviewed hypothesis.",
			EvidenceRefs: evidenceRefs,
			ObservedAt:   in.Date + "T00:00:00Z",
		},
		EvidenceRefs: evidenceRefs,
	}
}
