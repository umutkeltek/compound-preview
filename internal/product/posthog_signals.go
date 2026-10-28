package product

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// PostHog is the distribution loop's own measurement source: a distribution post
// is minted as a hypothesis waiting on a PostHog metric (default posthog_signups),
// and this adapter turns a raw PostHog report export into the ProductSignal shape
// the loop already consumes — so the loop closes from a real export, not hand-fed
// YAML. It mirrors the Google report adapters, but where those record a single
// snapshot, PostHog carries a genuine before -> after delta across the two latest
// dates in the report, which is exactly the movement the distribution hypothesis
// is waiting to confirm.

type PostHogReportSignalOptions struct {
	ProductID string
	Project   string
	Metric    string
}

type posthogReportDocument struct {
	Project string             `json:"project"`
	Rows    []posthogReportRow `json:"rows"`
}

type posthogReportRow struct {
	Date           string   `json:"date"`
	Signups        *float64 `json:"signups"`
	ActivationRate *float64 `json:"activation_rate"`
	ActiveUsers    *float64 `json:"active_users"`
}

func LoadPostHogReportSignals(path string, opts PostHogReportSignalOptions) ([]SignalDocument, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc posthogReportDocument
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse PostHog report %s: %w", path, err)
	}
	return adaptPostHogReportSignals(doc, opts)
}

func adaptPostHogReportSignals(doc posthogReportDocument, opts PostHogReportSignalOptions) ([]SignalDocument, error) {
	productID := strings.TrimSpace(opts.ProductID)
	project := strings.TrimSpace(opts.Project)
	if productID == "" || project == "" {
		return nil, fmt.Errorf("PostHog report product_id project required")
	}
	if strings.TrimSpace(doc.Project) == "" {
		return nil, fmt.Errorf("PostHog report project required")
	}
	if strings.TrimSpace(doc.Project) != project {
		return nil, fmt.Errorf("PostHog report project %q does not match %q", doc.Project, project)
	}
	metrics, err := selectedGoogleMetrics(opts.Metric, []string{"signups", "activation_rate", "active_users"}, normalizeIDPart, "PostHog report")
	if err != nil {
		return nil, err
	}
	docs := make([]SignalDocument, 0, len(metrics))
	for _, metric := range metrics {
		before, after, prevDate, latestDate, err := posthogBeforeAfter(doc.Rows, metric)
		if err != nil {
			return nil, err
		}
		docs = append(docs, posthogSignal(posthogSignalInput{
			ProductID:  productID,
			Project:    project,
			SourceRef:  "posthog:report:" + project + ":" + latestDate + ":" + metric,
			Surface:    "product_analytics",
			Metric:     metric,
			Before:     before,
			After:      after,
			PrevDate:   prevDate,
			LatestDate: latestDate,
			Refs: []string{
				"posthog:report:" + latestDate + ":" + project,
				"posthog:project:" + project,
				fmt.Sprintf("posthog:%s:%.4g->%.4g", metric, before, after),
			},
		}))
	}
	return docs, nil
}

// posthogBeforeAfter collapses the report rows into a single before -> after
// movement: after is the metric on the latest date, before is the metric on the
// date immediately preceding it. With a single date there is no prior baseline,
// so the signal degrades to a snapshot (before == after) rather than implying a
// movement that was never measured.
func posthogBeforeAfter(rows []posthogReportRow, metric string) (before, after float64, prevDate, latestDate string, err error) {
	if len(rows) == 0 {
		return 0, 0, "", "", fmt.Errorf("PostHog report rows required")
	}
	byDate := map[string][]float64{}
	var dates []string
	for i, row := range rows {
		date, _, perr := parseGoogleReportDate(row.Date)
		if perr != nil {
			return 0, 0, "", "", fmt.Errorf("PostHog report row %d: %w", i+1, perr)
		}
		value, verr := posthogMetricValue(row, metric, date)
		if verr != nil {
			return 0, 0, "", "", verr
		}
		if _, seen := byDate[date]; !seen {
			dates = append(dates, date)
		}
		byDate[date] = append(byDate[date], value)
	}
	sort.Strings(dates)
	latestDate = dates[len(dates)-1]
	after = posthogAggregate(byDate[latestDate], metric)
	if len(dates) >= 2 {
		prevDate = dates[len(dates)-2]
		before = posthogAggregate(byDate[prevDate], metric)
	} else {
		prevDate = latestDate
		before = after
	}
	return before, after, prevDate, latestDate, nil
}

func posthogMetricValue(row posthogReportRow, metric, date string) (float64, error) {
	switch metric {
	case "signups":
		return requiredGoogleMetric(row.Signups, "PostHog report", metric, date)
	case "activation_rate":
		return requiredGoogleMetric(row.ActivationRate, "PostHog report", metric, date)
	case "active_users":
		return requiredGoogleMetric(row.ActiveUsers, "PostHog report", metric, date)
	default:
		return 0, fmt.Errorf("PostHog report metric %q unsupported", metric)
	}
}

// posthogAggregate sums a date's rows for count metrics and averages them for the
// rate metric, so multiple rows on one date collapse sensibly.
func posthogAggregate(values []float64, metric string) float64 {
	var sum float64
	for _, v := range values {
		sum += v
	}
	if metric == "activation_rate" {
		return sum / float64(len(values))
	}
	return sum
}

type posthogSignalInput struct {
	ProductID  string
	Project    string
	SourceRef  string
	Surface    string
	Metric     string
	Before     float64
	After      float64
	PrevDate   string
	LatestDate string
	Refs       []string
}

func posthogSignal(in posthogSignalInput) SignalDocument {
	metricName := canonicalMetricName("posthog", in.Metric)
	evidenceRefs := compact(in.Refs)
	window := "snapshot " + in.LatestDate
	change := "PostHog report snapshot observed"
	if in.PrevDate != in.LatestDate {
		window = in.PrevDate + " -> " + in.LatestDate
		change = fmt.Sprintf("PostHog %s moved following distribution activity", in.Metric)
	}
	return SignalDocument{
		SchemaVersion: SignalSchemaVersion,
		Source: SignalSource{
			Kind: "posthog",
			Ref:  in.SourceRef,
		},
		Signal: Signal{
			ID:          stableID("sig_", in.ProductID, "posthog", in.Project, metricName, in.LatestDate),
			ProductID:   in.ProductID,
			Surface:     in.Surface,
			ChangeEvent: change,
			MetricName:  metricName,
			Before:      in.Before,
			After:       in.After,
			Window:      window,
			Confidence:  0.72,
			Confounders: []string{
				"correlation with distribution activity, not proven causation",
				"PostHog reporting can lag or sample data",
			},
			Interpretations: []Interpretation{{
				Lens:  "provider_measurement",
				Claim: "records the observed PostHog metric movement; causality requires a reviewed hypothesis",
			}},
			NextAction:   "Join to the matching distribution hypothesis; confirm before scaling this post shape.",
			EvidenceRefs: evidenceRefs,
			ObservedAt:   in.LatestDate + "T00:00:00Z",
		},
		EvidenceRefs: evidenceRefs,
	}
}
