package product

import (
	"fmt"
	"strings"
	"time"
)

// The ads → loop bridge. Ad rows are acquisition evidence, and the product
// loop only moves on ProductSignals (signal → hypothesis → review). This
// turns two windows of imported rows into before/after signals through the
// SAME ApplySignal door every other adapter uses, so an ads window can be
// reviewed against a hypothesis like any App Store or Search Console change.

// adsSignalMetrics lists the bridgeable aggregates. The ledger stays the
// source; a signal is a reviewed reading of it, never a second store.
var adsSignalMetrics = []string{"spend", "impressions", "clicks", "conversions", "revenue", "ctr", "cpc", "cpa", "roas"}

type AdsSignalOptions struct {
	Platform    string
	ProductID   string
	Account     string
	Before      string
	After       string
	Metric      string
	ChangeEvent string
}

// BuildAdsSignals compares two date windows of the ads ledger for one
// platform × product and returns one SignalDocument per requested metric.
// The source ref names the ledger, the windows, and the metric, so the
// signal is reproducible from the rows it summarises.
func BuildAdsSignals(homePath string, opts AdsSignalOptions) ([]SignalDocument, error) {
	platform, ok := LookupAdPlatform(opts.Platform)
	if !ok {
		return nil, fmt.Errorf("ads signal: unknown platform %q (known: %s)", opts.Platform, strings.Join(AdPlatformIDs(), ", "))
	}
	productID := strings.TrimSpace(opts.ProductID)
	if productID == "" {
		return nil, fmt.Errorf("ads signal: --product is required")
	}
	before, err := parseDateRange(opts.Before)
	if err != nil {
		return nil, fmt.Errorf("ads signal: --before %w", err)
	}
	after, err := parseDateRange(opts.After)
	if err != nil {
		return nil, fmt.Errorf("ads signal: --after %w", err)
	}
	metrics, err := selectedAdsMetrics(opts.Metric)
	if err != nil {
		return nil, err
	}
	history, err := LoadAdRows(homePath)
	if err != nil {
		return nil, err
	}
	account := strings.TrimSpace(opts.Account)
	var beforeRows, afterRows []AdRow
	for _, r := range LatestAdRows(history) {
		if r.Platform != platform.ID || r.ProductID != productID {
			continue
		}
		if account != "" && r.Account != account {
			continue
		}
		switch {
		case r.Date >= before.Raw[:10] && r.Date <= before.Raw[11:]:
			beforeRows = append(beforeRows, r)
		case r.Date >= after.Raw[:10] && r.Date <= after.Raw[11:]:
			afterRows = append(afterRows, r)
		}
	}
	if len(beforeRows) == 0 && len(afterRows) == 0 {
		return nil, fmt.Errorf("ads signal: no %s rows for %s in %s or %s; import a report covering those dates first", platform.ID, productID, before.Raw, after.Raw)
	}
	bt := adsTotals(beforeRows)
	at := adsTotals(afterRows)
	if !adsSpendIsOneCurrency(bt) || !adsSpendIsOneCurrency(at) {
		return nil, fmt.Errorf("ads signal: spend inside a window is not one resolved currency (rows disagree, or carry no currency at all); re-import with --currency before bridging")
	}
	change := strings.TrimSpace(opts.ChangeEvent)
	if change == "" {
		change = fmt.Sprintf("%s paid acquisition window comparison observed", platform.Label)
	}
	docs := make([]SignalDocument, 0, len(metrics))
	for _, metric := range metrics {
		bv, bok := adsMetricValue(bt, metric)
		av, aok := adsMetricValue(at, metric)
		if !bok && !aok {
			continue
		}
		metricName := "ads_" + metric
		refs := compact([]string{
			"ads:" + platform.ID + ":ledger:product-ads.jsonl",
			"ads:" + platform.ID + ":window:" + before.Raw,
			"ads:" + platform.ID + ":window:" + after.Raw,
			fmt.Sprintf("ads:%s:%s:%.4g->%.4g", platform.ID, metric, bv, av),
			fmt.Sprintf("ads:%s:rows:%d->%d", platform.ID, len(beforeRows), len(afterRows)),
		})
		if account != "" {
			refs = append(refs, "ads:"+platform.ID+":account:"+account)
		}
		docs = append(docs, SignalDocument{
			SchemaVersion: SignalSchemaVersion,
			Source: SignalSource{
				Kind: platform.ID,
				Ref:  "ads:" + platform.ID + ":" + productID + ":" + metricName + ":" + before.Raw + "->" + after.Raw,
			},
			Signal: Signal{
				ID:          stableID("sig_", productID, platform.ID, account, metricName, before.Raw, after.Raw),
				ProductID:   productID,
				Surface:     "paid-acquisition",
				ChangeEvent: change,
				MetricName:  metricName,
				Before:      bv,
				After:       av,
				Window:      before.Raw + " vs " + after.Raw,
				Confidence:  0.7,
				Confounders: []string{
					"platform-reported attribution; not independently verified",
					"delivery and auction dynamics can move spend independently of the product change",
					"windows of unequal row coverage compare imported days, not calendar days",
				},
				Interpretations: []Interpretation{{
					Lens:  "paid_acquisition",
					Claim: "compares platform-reported campaign totals across two windows without causal attribution",
				}},
				NextAction:   "Pair with a hypothesis before reading this as causal; record the verdict through the loop review.",
				EvidenceRefs: refs,
				ObservedAt:   after.End.Format("2006-01-02") + "T00:00:00Z",
			},
			EvidenceRefs: refs,
		})
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("ads signal: none of the requested metrics is defined on these rows (%s)", strings.Join(metrics, ","))
	}
	return docs, nil
}

func selectedAdsMetrics(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return []string{"spend", "conversions", "cpa"}, nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		m := normalizeIDPart(part)
		if m == "" {
			continue
		}
		known := false
		for _, allowed := range adsSignalMetrics {
			if m == allowed {
				known = true
				break
			}
		}
		if !known {
			return nil, fmt.Errorf("ads signal: metric %q unsupported (known: %s)", part, strings.Join(adsSignalMetrics, ", "))
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("ads signal: --metric names no metric")
	}
	return out, nil
}

// adsMetricValue reads one aggregate; ok is false when the metric is
// undefined on these rows (a ratio with a zero denominator, revenue with no
// revenue column) so the caller can refuse to record a fabricated zero.
func adsMetricValue(t AdsTotals, metric string) (float64, bool) {
	switch metric {
	case "spend":
		return t.Spend, t.Rows > 0
	case "impressions":
		return t.Impressions, t.Rows > 0
	case "clicks":
		return t.Clicks, t.Rows > 0
	case "conversions":
		return t.Conversions, t.Rows > 0
	case "revenue":
		return t.Revenue, t.HasRevenue
	case "ctr":
		return t.CTR, t.Impressions > 0
	case "cpc":
		// The money ratios are defined only where the reducer computed
		// them; reading 0 back out as a value would bridge a number the
		// projection deliberately withheld.
		return t.CPC, t.Clicks > 0 && adsSpendIsOneCurrency(t)
	case "cpa":
		return t.CPA, t.Conversions > 0 && adsSpendIsOneCurrency(t)
	case "roas":
		return t.ROAS, adsROASIsComplete(t)
	}
	return 0, false
}

// adsSignalObservedAt keeps the bridge's timestamp derivation in one place
// for tests that assert replay stability.
func adsSignalObservedAt(after dateRange) string {
	return after.End.UTC().Format("2006-01-02") + "T00:00:00Z"
}

var _ = adsSignalObservedAt
var _ = time.Now
