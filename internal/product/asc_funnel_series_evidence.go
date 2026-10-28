package product

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

const ascFunnelRowSchemaV2 = "compound.asc_app_daily.v2"

// Nullable values distinguish a published zero from absent or suppressed data.
type ascFunnelSeriesRow struct {
	SchemaVersion      string                             `json:"schema_version"`
	Date               string                             `json:"date"`
	ASCAppID           string                             `json:"asc_app_id"`
	Impressions        *float64                           `json:"impressions"`
	ProductPageViews   *float64                           `json:"product_page_views"`
	FirstTimeDownloads *float64                           `json:"first_time_downloads"`
	ProceedsUSD        *float64                           `json:"proceeds_usd"`
	MetricEvidence     map[string]ascFunnelMetricEvidence `json:"metric_evidence"`
	SourceReports      map[string]ascFunnelReportEvidence `json:"source_reports"`
}

type ascFunnelMetricEvidence struct {
	Status          string          `json:"status"`
	Reason          json.RawMessage `json:"reason"`
	Population      string          `json:"population"`
	SourceReportKey string          `json:"source_report_key"`
	MatchedRows     int64           `json:"matched_rows"`
}

type ascFunnelSegmentEvidence struct {
	ID        string `json:"id"`
	SHA256    string `json:"sha256"`
	ByteCount int64  `json:"byte_count"`
}

type ascFunnelReportEvidence struct {
	ReportID                 string                     `json:"report_id"`
	ReportName               string                     `json:"report_name"`
	InstanceID               string                     `json:"instance_id"`
	ProcessingDate           string                     `json:"processing_date"`
	Granularity              string                     `json:"granularity"`
	SegmentInventoryComplete bool                       `json:"segment_inventory_complete"`
	ExpectedSegmentIDs       []string                   `json:"expected_segment_ids"`
	Segments                 []ascFunnelSegmentEvidence `json:"segments"`
}

type ascFunnelMetricContract struct {
	Population string
	ReportBase string
	ReportName string
	Signed     bool
	Value      func(ascFunnelSeriesRow) *float64
}

func ascFunnelMetric(metric string) (ascFunnelMetricContract, error) {
	switch metric {
	case "impressions":
		return ascFunnelMetricContract{"published_impression_counts", "app_store_discovery_and_engagement", "App Store Discovery and Engagement", false, func(r ascFunnelSeriesRow) *float64 { return r.Impressions }}, nil
	case "product_page_views":
		return ascFunnelMetricContract{"published_product_page_view_counts", "app_store_discovery_and_engagement", "App Store Discovery and Engagement", false, func(r ascFunnelSeriesRow) *float64 { return r.ProductPageViews }}, nil
	case "first_time_downloads":
		return ascFunnelMetricContract{"published_first_time_download_counts", "app_downloads", "App Downloads", false, func(r ascFunnelSeriesRow) *float64 { return r.FirstTimeDownloads }}, nil
	case "proceeds_usd":
		return ascFunnelMetricContract{"published_proceeds_usd", "app_store_purchases", "App Store Purchases", true, func(r ascFunnelSeriesRow) *float64 { return r.ProceedsUSD }}, nil
	case "product_page_conversion_rate", "app_store_product_page_conversion", "conversion_rate":
		return ascFunnelMetricContract{}, fmt.Errorf("ASC funnel conversion rejected: unmatched download and product-page populations")
	case "":
		return ascFunnelMetricContract{}, fmt.Errorf("ASC funnel selected metric is required; no default metric substitution")
	default:
		return ascFunnelMetricContract{}, fmt.Errorf("ASC funnel metric %q unsupported_population", metric)
	}
}

func ascFunnelObservedValue(row ascFunnelSeriesRow, metric string, contract ascFunnelMetricContract) (float64, error) {
	fail := func(reason string) (float64, error) {
		return 0, fmt.Errorf("ASC funnel app_id %q date %s metric %s: %s", row.ASCAppID, row.Date, metric, reason)
	}
	if row.SchemaVersion != ascFunnelRowSchemaV2 {
		return fail("legacy or missing v2 metric evidence")
	}
	value := contract.Value(row)
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || (!contract.Signed && *value < 0) {
		return fail("null, missing or invalid published value")
	}
	e, exists := row.MetricEvidence[metric]
	if !exists || e.Status != "observed" || !bytes.Equal(bytes.TrimSpace(e.Reason), []byte("null")) || e.Population != contract.Population || e.MatchedRows < 1 || e.MatchedRows > 9007199254740991 {
		return fail("selected metric lacks observed matching population evidence")
	}
	suffix := ""
	if e.SourceReportKey == contract.ReportBase+"_standard" {
		suffix = "Standard"
	}
	if e.SourceReportKey == contract.ReportBase+"_detailed" {
		suffix = "Detailed"
	}
	source, exists := row.SourceReports[e.SourceReportKey]
	if suffix == "" || !exists || source.ReportName != contract.ReportName+" "+suffix {
		return fail("source report family/population mismatch")
	}
	if err := validateASCFunnelSource(source); err != nil {
		return fail(err.Error())
	}
	return *value, nil
}

func validateASCFunnelSource(source ascFunnelReportEvidence) error {
	if !ascFunnelIdentity(source.ReportID) || !ascFunnelIdentity(source.InstanceID) || source.Granularity != "DAILY" || !source.SegmentInventoryComplete {
		return fmt.Errorf("incomplete source identity or segment inventory")
	}
	if _, err := time.Parse("2006-01-02", source.ProcessingDate); err != nil {
		return fmt.Errorf("invalid source processing_date")
	}
	if len(source.ExpectedSegmentIDs) == 0 || len(source.ExpectedSegmentIDs) != len(source.Segments) {
		return fmt.Errorf("incomplete source segment bijection")
	}
	expected := make(map[string]bool, len(source.ExpectedSegmentIDs))
	for _, id := range source.ExpectedSegmentIDs {
		if !ascFunnelIdentity(id) || expected[id] {
			return fmt.Errorf("invalid or duplicate declared segment ID")
		}
		expected[id] = true
	}
	for _, segment := range source.Segments {
		hash, err := hex.DecodeString(segment.SHA256)
		if !expected[segment.ID] || segment.ByteCount <= 0 || segment.ByteCount > 9007199254740991 || err != nil || len(hash) != 32 || strings.ToLower(segment.SHA256) != segment.SHA256 {
			return fmt.Errorf("incomplete or invalid fetched source segment")
		}
		delete(expected, segment.ID)
	}
	if len(expected) != 0 {
		return fmt.Errorf("missing fetched source segment")
	}
	return nil
}

func ascFunnelIdentity(value string) bool { return value != "" && strings.TrimSpace(value) == value }
