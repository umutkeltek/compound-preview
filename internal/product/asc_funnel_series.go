package product

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"time"
)

// ASCFunnelSeriesSignalOptions configures LoadASCFunnelSeriesSignal. It mirrors
// ASCSalesSeriesSignalOptions deliberately: same explicit before/after window
// shape, same required fields, so the two adapters read as one family from the
// CLI down.
type ASCFunnelSeriesSignalOptions struct {
	ProductID    string
	AppID        string
	Metric       string
	BeforeWindow string
	AfterWindow  string
	ChangeEvent  string
}

type funnelWindowAggregate struct {
	Value float64
	Rows  int
	Start string
	End   string
}

// LoadASCFunnelSeriesSignal reads an existing ASC funnel daily-app-series JSONL
// export (already produced upstream; this never fetches ASC data itself) and
// turns a before/after window comparison for one product/app/metric into a
// ProductSignal. It is the funnel-schema sibling of LoadASCSalesSeriesSignal.
func LoadASCFunnelSeriesSignal(path string, opts ASCFunnelSeriesSignalOptions) (SignalDocument, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return SignalDocument{}, err
	}
	rows, err := parseASCFunnelSeriesRows(b, path)
	if err != nil {
		return SignalDocument{}, err
	}
	return adaptASCFunnelSeriesSignal(rows, opts)
}

func parseASCFunnelSeriesRows(b []byte, source string) ([]ascFunnelSeriesRow, error) {
	var rows []ascFunnelSeriesRow
	seen := map[string]bool{}
	for i, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row ascFunnelSeriesRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("parse ASC funnel series %s line %d: %w", source, i+1, err)
		}
		if strings.TrimSpace(row.ASCAppID) == "" || strings.TrimSpace(row.Date) == "" {
			return nil, fmt.Errorf("ASC funnel series %s line %d missing asc_app_id/date", source, i+1)
		}
		if _, err := time.Parse("2006-01-02", row.Date); err != nil {
			return nil, fmt.Errorf("ASC funnel invalid date %q", row.Date)
		}
		if !ascFunnelIdentity(row.ASCAppID) {
			return nil, fmt.Errorf("ASC funnel invalid asc_app_id")
		}
		key := row.ASCAppID + "|" + row.Date
		if seen[key] {
			return nil, fmt.Errorf("ASC funnel duplicate app/date %s", key)
		}
		seen[key] = true
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("ASC funnel series %s has no rows", source)
	}
	return rows, nil
}

func adaptASCFunnelSeriesSignal(rows []ascFunnelSeriesRow, opts ASCFunnelSeriesSignalOptions) (SignalDocument, error) {
	productID := strings.TrimSpace(opts.ProductID)
	appID := strings.TrimSpace(opts.AppID)
	metric := strings.TrimSpace(opts.Metric)
	if _, err := ascFunnelMetric(metric); err != nil {
		return SignalDocument{}, err
	}
	changeEvent := strings.TrimSpace(opts.ChangeEvent)
	if productID == "" || appID == "" || changeEvent == "" {
		return SignalDocument{}, fmt.Errorf("ASC funnel series product_id app_id change_event required")
	}
	beforeRange, err := parseDateRange(opts.BeforeWindow)
	if err != nil {
		return SignalDocument{}, fmt.Errorf("ASC funnel series before window: %w", err)
	}
	afterRange, err := parseDateRange(opts.AfterWindow)
	if err != nil {
		return SignalDocument{}, fmt.Errorf("ASC funnel series after window: %w", err)
	}
	before, err := aggregateFunnelWindow(rows, appID, metric, beforeRange)
	if err != nil {
		return SignalDocument{}, err
	}
	after, err := aggregateFunnelWindow(rows, appID, metric, afterRange)
	if err != nil {
		return SignalDocument{}, err
	}
	metricName := canonicalMetricName("app_store_connect", metric)
	sourceRef := "asc:funnel_series:" + appID + ":" + beforeRange.Raw + ":" + afterRange.Raw
	confounders := []string{
		"explicit window comparison; causality not implied",
		"seasonality, channel mix, price, reviews, release timing may confound movement",
		"every requested calendar day has observed v2 evidence for this published metric; missing and suppressed values are refused",
		"published dimensional counts are not unique users or unsuppressed population totals",
	}
	evidenceRefs := compact([]string{
		"asc:funnel_series:" + appID,
		"asc:funnel_series:metric:" + normalizeIDPart(metric),
		"asc:funnel_series:before:" + beforeRange.Raw,
		"asc:funnel_series:after:" + afterRange.Raw,
		fmt.Sprintf("asc:funnel_series:before_observed_metric_days:%d", before.Rows),
		fmt.Sprintf("asc:funnel_series:after_observed_metric_days:%d", after.Rows),
		"asc:funnel_series:before_value:" + shortFloat(before.Value),
		"asc:funnel_series:after_value:" + shortFloat(after.Value),
	})
	return SignalDocument{
		SchemaVersion: SignalSchemaVersion,
		Source: SignalSource{
			Kind: "app_store_connect",
			Ref:  sourceRef,
		},
		Signal: Signal{
			ID:          stableID("sig_", productID, "app_store_connect", appID, metricName, beforeRange.Raw, afterRange.Raw, changeEvent),
			ProductID:   productID,
			Surface:     "app_store",
			ChangeEvent: changeEvent,
			MetricName:  metricName,
			Before:      before.Value,
			After:       after.Value,
			Window:      "before " + beforeRange.Raw + "; after " + afterRange.Raw,
			Confidence:  0.6,
			Confounders: confounders,
			Interpretations: []Interpretation{{
				Lens:  "window_comparison",
				Claim: "records before/after App Store Connect funnel metric movement for review, not proof of causality",
			}},
			NextAction:   "Review against the experiment ledger and confounders before promoting any product hypothesis.",
			EvidenceRefs: evidenceRefs,
			ObservedAt:   afterRange.End.Format("2006-01-02") + "T00:00:00Z",
		},
		EvidenceRefs: evidenceRefs,
	}, nil
}

// aggregateFunnelWindow requires one observed selected-metric row per inclusive
// calendar day. It never drops unknown days or derives a conversion population.
func aggregateFunnelWindow(rows []ascFunnelSeriesRow, appID, metric string, window dateRange) (funnelWindowAggregate, error) {
	contract, err := ascFunnelMetric(metric)
	if err != nil {
		return funnelWindowAggregate{}, err
	}
	byDate := map[string]ascFunnelSeriesRow{}
	for _, row := range rows {
		if row.ASCAppID == appID {
			byDate[row.Date] = row
		}
	}
	var out funnelWindowAggregate
	for day := window.Start; !day.After(window.End); day = day.AddDate(0, 0, 1) {
		date := day.Format("2006-01-02")
		row, exists := byDate[date]
		if !exists {
			return funnelWindowAggregate{}, fmt.Errorf("ASC funnel app_id %q missing calendar day %s metric %s", appID, date, metric)
		}
		value, err := ascFunnelObservedValue(row, metric, contract)
		if err != nil {
			return funnelWindowAggregate{}, err
		}
		out.Value += value
		if math.IsNaN(out.Value) || math.IsInf(out.Value, 0) {
			return funnelWindowAggregate{}, fmt.Errorf("ASC funnel aggregate is nonfinite")
		}
		out.Rows++
		if out.Start == "" {
			out.Start = date
		}
		out.End = date
	}
	return out, nil
}
