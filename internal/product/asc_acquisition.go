package product

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// App Store Connect's "App Downloads Detailed" analytics report is the only
// first-party surface that attributes a download to where it came from. It is
// produced by the Analytics Reports API (an ONGOING or ONE_TIME_SNAPSHOT
// request), downloaded as a gzipped TSV, and carries one row per
// date x download type x version x device x source x territory combination.
//
// This adapter reads that already-exported TSV. Like every other adapter in
// this family it never fetches from App Store Connect itself: fetching is the
// report owner's job, and duplicating it here would create a second fetcher
// competing with the existing export path.
const (
	// ascAcquisitionFirstTimeDownloadType is the only Download Type value that
	// represents acquisition. The report also carries "Redownload",
	// "Auto-update" and "Manual update" rows, which are retention/maintenance
	// events for an install that already happened.
	//
	// This constant is load-bearing rather than cosmetic: on real report days
	// the auto-update rows routinely outnumber the acquisition rows, so an
	// adapter that summed Counts without filtering would report update traffic
	// as growth and would move whenever a release shipped.
	ascAcquisitionFirstTimeDownloadType = "first-time download"

	// ascAcquisitionPaidShareMetric is the derived ratio metric: paid
	// first-time downloads over all first-time downloads, computed within one
	// window rather than stored per row.
	ascAcquisitionPaidShareMetric = "paid_download_share"
)

// ASCAcquisitionSignalOptions configures LoadASCAcquisitionSignal. It mirrors
// ASCFunnelSeriesSignalOptions field for field on purpose: three adapters in
// this family now read one CLI shape, so a caller who has learned one has
// learned all of them.
type ASCAcquisitionSignalOptions struct {
	ProductID    string
	AppID        string
	Metric       string
	BeforeWindow string
	AfterWindow  string
	ChangeEvent  string
}

// ascAcquisitionRow is one parsed report row, reduced to the fields this
// adapter is allowed to reason about.
//
// There is deliberately no AppName field. The report carries one, and matching
// on it would be the easy mistake: display names are localized, change with
// metadata edits, and are not identity. AppleIdentifier is the only identifier
// the report owner treats as stable, so it is the only one representable here
// and the adapter is structurally unable to match on anything else.
type ascAcquisitionRow struct {
	Date            string
	AppleIdentifier string
	DownloadType    string
	SourceType      string
	Campaign        string
	Counts          float64
}

// isFirstTimeDownload reports whether the row is an acquisition event.
func (r ascAcquisitionRow) isFirstTimeDownload() bool {
	return strings.EqualFold(strings.TrimSpace(r.DownloadType), ascAcquisitionFirstTimeDownloadType)
}

// isPaid reports whether the row is attributable to a paid campaign.
//
// Paid attribution is decided by the Campaign column carrying a value, NOT by
// Source Type. This is the distinction the report's shape forces and it is
// easy to get backwards: Apple Search Ads installs are reported under the same
// "App Store search" source type as organic search installs, and are separated
// from them only by Campaign. An adapter that split paid from organic on
// Source Type would therefore classify every paid install as organic and
// report a paid share of exactly zero no matter how much was spent.
//
// Reading Campaign also degrades in the safe direction: if the report ever
// grows a distinct paid source type, campaign-bearing rows are still counted
// as paid.
func (r ascAcquisitionRow) isPaid() bool {
	return strings.TrimSpace(r.Campaign) != ""
}

// ascAcquisitionRequiredColumns are the report headers this adapter cannot
// operate without. A report missing any of them is rejected at parse time
// rather than silently contributing zeros.
var ascAcquisitionRequiredColumns = []string{
	"Date",
	"App Apple Identifier",
	"Download Type",
	"Counts",
}

// Source Type and Campaign are deliberately NOT required. A reduced export
// still parses, but a report without Campaign can only answer total-download
// questions: paid and organic metrics fail closed against it rather than
// reporting everything as organic.

type ascAcquisitionAggregate struct {
	Value float64
	Rows  int
}

// LoadASCAcquisitionSignal reads an exported "App Downloads Detailed" TSV and
// turns a before/after window comparison for one product, app and acquisition
// metric into a ProductSignal. It is the acquisition-schema sibling of
// LoadASCFunnelSeriesSignal.
func LoadASCAcquisitionSignal(path string, opts ASCAcquisitionSignalOptions) (SignalDocument, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return SignalDocument{}, err
	}
	rows, hasCampaign, err := parseASCAcquisitionRows(b, path)
	if err != nil {
		return SignalDocument{}, err
	}
	return adaptASCAcquisitionSignal(rows, hasCampaign, opts)
}

// parseASCAcquisitionRows parses the tab-separated report body. It returns the
// rows and whether the export carried a Campaign column, because that fact
// decides which metrics are answerable at all.
func parseASCAcquisitionRows(b []byte, source string) ([]ascAcquisitionRow, bool, error) {
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(string(b)), "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return nil, false, fmt.Errorf("ASC acquisition report %s is empty", source)
	}
	index := map[string]int{}
	for i, name := range strings.Split(lines[0], "\t") {
		index[strings.TrimSpace(name)] = i
	}
	for _, required := range ascAcquisitionRequiredColumns {
		if _, ok := index[required]; !ok {
			return nil, false, fmt.Errorf("ASC acquisition report %s missing required column %q", source, required)
		}
	}
	_, hasCampaign := index["Campaign"]

	field := func(cols []string, name string) string {
		at, ok := index[name]
		if !ok || at >= len(cols) {
			return ""
		}
		return strings.TrimSpace(cols[at])
	}

	var rows []ascAcquisitionRow
	for i, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		cols := strings.Split(line, "\t")
		row := ascAcquisitionRow{
			Date:            field(cols, "Date"),
			AppleIdentifier: field(cols, "App Apple Identifier"),
			DownloadType:    field(cols, "Download Type"),
			SourceType:      field(cols, "Source Type"),
			Campaign:        field(cols, "Campaign"),
		}
		if row.AppleIdentifier == "" || row.Date == "" {
			return nil, false, fmt.Errorf("ASC acquisition report %s line %d missing App Apple Identifier/Date", source, i+2)
		}
		raw := field(cols, "Counts")
		if raw == "" {
			return nil, false, fmt.Errorf("ASC acquisition report %s line %d has empty Counts", source, i+2)
		}
		count, err := strconv.ParseFloat(strings.ReplaceAll(raw, ",", ""), 64)
		if err != nil {
			return nil, false, fmt.Errorf("ASC acquisition report %s line %d Counts %q: %w", source, i+2, raw, err)
		}
		row.Counts = count
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, false, fmt.Errorf("ASC acquisition report %s has no data rows", source)
	}
	return rows, hasCampaign, nil
}

func adaptASCAcquisitionSignal(rows []ascAcquisitionRow, hasCampaign bool, opts ASCAcquisitionSignalOptions) (SignalDocument, error) {
	productID := strings.TrimSpace(opts.ProductID)
	appID := strings.TrimSpace(opts.AppID)
	metric := strings.TrimSpace(opts.Metric)
	changeEvent := strings.TrimSpace(opts.ChangeEvent)
	if productID == "" || appID == "" || metric == "" || changeEvent == "" {
		return SignalDocument{}, fmt.Errorf("ASC acquisition signal requires product, app id, metric and change event")
	}
	beforeRange, err := parseDateRange(opts.BeforeWindow)
	if err != nil {
		return SignalDocument{}, fmt.Errorf("ASC acquisition before window: %w", err)
	}
	afterRange, err := parseDateRange(opts.AfterWindow)
	if err != nil {
		return SignalDocument{}, fmt.Errorf("ASC acquisition after window: %w", err)
	}
	before, err := aggregateASCAcquisitionWindow(rows, hasCampaign, appID, metric, beforeRange)
	if err != nil {
		return SignalDocument{}, err
	}
	after, err := aggregateASCAcquisitionWindow(rows, hasCampaign, appID, metric, afterRange)
	if err != nil {
		return SignalDocument{}, err
	}
	metricName := canonicalMetricName("app_store_connect", metric)
	sourceRef := "asc:acquisition:" + appID + ":" + beforeRange.Raw + ":" + afterRange.Raw
	confounders := []string{
		"explicit window comparison; causality not implied",
		"counts only first-time downloads; redownloads and updates are excluded and never contribute",
		"paid attribution is read from the report's campaign column, so paid installs that the report leaves unattributed are counted as organic",
		"the report attributes downloads, never spend; cost per install cannot be derived from this source alone",
	}
	evidenceRefs := compact([]string{
		"asc:acquisition:" + appID,
		"asc:acquisition:metric:" + normalizeIDPart(metric),
		"asc:acquisition:before:" + beforeRange.Raw,
		"asc:acquisition:after:" + afterRange.Raw,
		fmt.Sprintf("asc:acquisition:before_first_time_download_rows:%d", before.Rows),
		fmt.Sprintf("asc:acquisition:after_first_time_download_rows:%d", after.Rows),
		"asc:acquisition:before_value:" + shortFloat(before.Value),
		"asc:acquisition:after_value:" + shortFloat(after.Value),
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
				Lens:  "provider_window_comparison",
				Claim: "records an explicit before/after acquisition comparison without causal attribution",
			}},
			NextAction:   "Use as product feedback evidence; do not infer causality without a reviewed hypothesis.",
			EvidenceRefs: evidenceRefs,
			ObservedAt:   afterRange.End.Format("2006-01-02") + "T00:00:00Z",
		},
		EvidenceRefs: evidenceRefs,
	}, nil
}

// aggregateASCAcquisitionWindow sums one acquisition metric for exactly one
// App Apple Identifier across one date window.
//
// Four invariants are load-bearing here, each covered by a test that fails if
// it is dropped:
//
//  1. Only rows whose Download Type is a first-time download contribute. Every
//     other row describes an install that already existed.
//  2. Matching is by App Apple Identifier only; the row type carries no name.
//  3. Paid/organic is split on Campaign, never on Source Type.
//  4. A window with no qualifying rows is an error, never a zero. Zero
//     downloads and "this window was not covered by the export" are different
//     facts, and collapsing them would let a truncated report read as a
//     collapse in acquisition.
func aggregateASCAcquisitionWindow(rows []ascAcquisitionRow, hasCampaign bool, appID, metric string, window dateRange) (ascAcquisitionAggregate, error) {
	normalizedMetric := normalizeIDPart(metric)
	needsCampaign := normalizedMetric == "paid_first_time_downloads" ||
		normalizedMetric == "organic_first_time_downloads" ||
		normalizedMetric == ascAcquisitionPaidShareMetric
	if needsCampaign && !hasCampaign {
		return ascAcquisitionAggregate{}, fmt.Errorf("ASC acquisition metric %q requires a Campaign column; export does not carry one", metric)
	}
	switch normalizedMetric {
	case "first_time_downloads", "paid_first_time_downloads", "organic_first_time_downloads", ascAcquisitionPaidShareMetric:
	default:
		return ascAcquisitionAggregate{}, fmt.Errorf("ASC acquisition metric %q unsupported", metric)
	}

	var out ascAcquisitionAggregate
	var paidSum, totalSum float64
	for _, row := range rows {
		if strings.TrimSpace(row.AppleIdentifier) != appID {
			continue
		}
		if !row.isFirstTimeDownload() {
			continue
		}
		day, err := time.Parse("2006-01-02", strings.TrimSpace(row.Date))
		if err != nil {
			return ascAcquisitionAggregate{}, fmt.Errorf("ASC acquisition row date %q: %w", row.Date, err)
		}
		if day.Before(window.Start) || day.After(window.End) {
			continue
		}
		out.Rows++
		totalSum += row.Counts
		if row.isPaid() {
			paidSum += row.Counts
		}
	}
	if out.Rows == 0 {
		return ascAcquisitionAggregate{}, fmt.Errorf("ASC acquisition report has no first-time download rows for app %s in window %s", appID, window.Raw)
	}
	switch normalizedMetric {
	case "first_time_downloads":
		out.Value = totalSum
	case "paid_first_time_downloads":
		out.Value = paidSum
	case "organic_first_time_downloads":
		out.Value = totalSum - paidSum
	case ascAcquisitionPaidShareMetric:
		if totalSum == 0 {
			return ascAcquisitionAggregate{}, fmt.Errorf("ASC acquisition report has zero first-time downloads for app %s in window %s; cannot compute paid share", appID, window.Raw)
		}
		out.Value = paidSum / totalSum
	}
	return out, nil
}
