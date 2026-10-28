package product

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/mem"
	"compound/internal/sourceobs"
	"compound/internal/work"
)

const LoopModelSchemaVersion = "compound.product_loops.v0.1"

type LoopOptions struct {
	ProductID  string
	Recent     int
	StaleDays  int
	StaleAfter time.Duration
	Now        time.Time
}

type LoopModel struct {
	SchemaVersion string        `json:"schema_version"`
	GeneratedAt   string        `json:"generated_at"`
	Products      []ProductLoop `json:"products"`
}

type ProductLoop struct {
	OpenHypothesesByMissingSignalSource []LoopMissingSignalSource `json:"open_hypotheses_by_missing_signal_source,omitempty"`
	SignalsWithoutHypothesis            []LoopSignalHypothesisGap `json:"signals_without_hypothesis,omitempty"`
	ClosedReviewsByNextAction           []LoopReviewNextAction    `json:"closed_reviews_by_next_action,omitempty"`
	ActionCards                         []LoopActionCard          `json:"action_cards,omitempty"`
	ProductID                           string                    `json:"product_id"`
	Name                                string                    `json:"name"`
	SignalsByMetric                     map[string][]LoopSignal   `json:"signals_by_metric"`
	OpenHypotheses                      []LoopHypothesis          `json:"open_hypotheses"`
	Reviews                             []LoopReview              `json:"reviews"`
	OpenSourceIssues                    LoopSourceIssues          `json:"open_source_issues"`
	Freshness                           LoopFreshness             `json:"freshness"`
	LoopGaps                            []string                  `json:"loop_gaps"`
	// ReleaseAwareness is the additive evidence-only release-reconciliation
	// surface. It distinguishes candidate/submitted/live observations per
	// channel+platform with provenance and never auto-claims current-live.
	// Absent when no sourceobs release observation has been imported yet.
	ReleaseAwareness *ReleaseAwareness `json:"release_awareness,omitempty"`
}

type LoopSignal struct {
	SignalID      string  `json:"signal_id"`
	Provider      string  `json:"provider"`
	Metric        string  `json:"metric"`
	Before        float64 `json:"before"`
	After         float64 `json:"after"`
	ObservedAt    string  `json:"observed_at"`
	ChangeEvent   string  `json:"change_event,omitempty"`
	TransitionRef string  `json:"transition_ref,omitempty"`
}

type LoopHypothesis struct {
	ID                string   `json:"id"`
	Statement         string   `json:"statement"`
	ExpectedEffect    string   `json:"expected_effect"`
	Metrics           []string `json:"metrics,omitempty"`
	ObservationWindow string   `json:"observation_window,omitempty"`
	NextAction        string   `json:"next_action,omitempty"`
	EvidenceRefs      []string `json:"evidence_refs,omitempty"`
	State             string   `json:"state"`
	CreatedAt         string   `json:"created_at,omitempty"`
}

type LoopReview struct {
	ID           string `json:"id"`
	HypothesisID string `json:"hypothesis_id"`
	SignalID     string `json:"signal_id,omitempty"`
	Verdict      string `json:"verdict"`
	NextAction   string `json:"next_action,omitempty"`
	ReviewedAt   string `json:"reviewed_at,omitempty"`
}

type LoopMissingSignalSource struct {
	Source        string           `json:"source"`
	HypothesisIDs []string         `json:"hypothesis_ids"`
	Metrics       []string         `json:"metrics,omitempty"`
	NextActions   []string         `json:"next_actions,omitempty"`
	ActionCards   []LoopActionCard `json:"action_cards,omitempty"`
}

type LoopSignalHypothesisGap struct {
	ChangeEvent          string           `json:"change_event"`
	SignalIDs            []string         `json:"signal_ids"`
	Metrics              []string         `json:"metrics,omitempty"`
	Providers            []string         `json:"providers,omitempty"`
	Consumed             bool             `json:"consumed,omitempty"`
	ConsumedByWorkIDs    []string         `json:"consumed_by_work_ids,omitempty"`
	ConsumedEvidenceRefs []string         `json:"consumed_evidence_refs,omitempty"`
	ActionCards          []LoopActionCard `json:"action_cards,omitempty"`
}

type LoopReviewNextAction struct {
	NextAction           string           `json:"next_action"`
	ReviewIDs            []string         `json:"review_ids"`
	HypothesisIDs        []string         `json:"hypothesis_ids,omitempty"`
	SignalIDs            []string         `json:"signal_ids,omitempty"`
	Consumed             bool             `json:"consumed,omitempty"`
	ConsumedByWorkIDs    []string         `json:"consumed_by_work_ids,omitempty"`
	ConsumedEvidenceRefs []string         `json:"consumed_evidence_refs,omitempty"`
	ActionCards          []LoopActionCard `json:"action_cards,omitempty"`
}

type LoopActionCard struct {
	ID                 string   `json:"id"`
	Kind               string   `json:"kind"`
	Title              string   `json:"title"`
	Command            []string `json:"command,omitempty"`
	ActionSummary      string   `json:"action_summary,omitempty"`
	RequiredInputs     []string `json:"required_inputs,omitempty"`
	ExpectedValidation []string `json:"expected_validation,omitempty"`
	StopConditions     []string `json:"stop_conditions,omitempty"`
	Boundary           string   `json:"boundary,omitempty"`
	Proof              string   `json:"proof,omitempty"`
}

type LoopSourceIssues struct {
	Count int      `json:"count"`
	Refs  []string `json:"refs"`
}

type LoopFreshness struct {
	LatestSignalAt string `json:"latest_signal_at,omitempty"`
	LatestReviewAt string `json:"latest_review_at,omitempty"`
	Stale          bool   `json:"stale"`
}

func BuildLoopModel(homePath string, opts LoopOptions) (LoopModel, error) {
	return buildLoopModel(homePath, opts, sourceobs.LoadReleaseObservationsReport)
}

func buildLoopModel(homePath string, opts LoopOptions, readRelease func(string, []string, time.Time) (sourceobs.ReleaseObservationsReport, error)) (LoopModel, error) {
	opts = normalizeLoopOptions(opts)
	store, err := mem.Open(homePath)
	if err != nil {
		return LoopModel{}, err
	}
	authority, err := Authority(homePath, store)
	if err != nil {
		return LoopModel{}, err
	}
	reviews, err := loadReviews(homePath)
	if err != nil {
		return LoopModel{}, err
	}
	consumedActions, err := loopProductActionConsumptionIndex(homePath)
	if err != nil {
		return LoopModel{}, err
	}
	metadata, err := productLoopMetadata(store)
	if err != nil {
		return LoopModel{}, err
	}
	model := LoopModel{
		SchemaVersion: LoopModelSchemaVersion,
		GeneratedAt:   opts.Now.UTC().Format(time.RFC3339),
		Products:      []ProductLoop{},
	}
	var productIDs []string
	for _, item := range authority.Products {
		if opts.ProductID == "" || item.ProductID == opts.ProductID {
			productIDs = append(productIDs, item.ProductID)
		}
	}
	releases := sourceobs.ReleaseObservationsReport{}
	if len(productIDs) > 0 {
		releases, err = readRelease(homePath, productIDs, opts.Now)
		if err != nil {
			// Incomplete release custody cannot certify any partial observations,
			// but must not hide unrelated product signals, hypotheses, or reviews.
			// Replace the whole report with visible, portfolio-wide invalidity.
			releases = sourceobs.ReleaseObservationsReport{Issues: []sourceobs.ReleaseObservationIssue{
				{Reason: fmt.Sprintf("release evidence: %v", err)},
			}}
		}
	}
	releasesByProduct := releaseReportsByProduct(releases, productIDs)
	for _, item := range authority.Products {
		if opts.ProductID != "" && item.ProductID != opts.ProductID {
			continue
		}
		loop, err := buildProductLoop(homePath, item.ProductID, metadata[item.ProductID], reviews, consumedActions, releasesByProduct[item.ProductID], opts)
		if err != nil {
			return LoopModel{}, err
		}
		model.Products = append(model.Products, loop)
	}
	sort.Slice(model.Products, func(i, j int) bool {
		return model.Products[i].ProductID < model.Products[j].ProductID
	})
	return model, nil
}

func (m LoopModel) Validate() error {
	if m.SchemaVersion != LoopModelSchemaVersion {
		return fmt.Errorf("schema_version = %q, want %q", m.SchemaVersion, LoopModelSchemaVersion)
	}
	if strings.TrimSpace(m.GeneratedAt) == "" {
		return fmt.Errorf("generated_at required")
	}
	if _, err := time.Parse(time.RFC3339, m.GeneratedAt); err != nil {
		return fmt.Errorf("generated_at invalid: %w", err)
	}
	for _, product := range m.Products {
		if strings.TrimSpace(product.ProductID) == "" {
			return fmt.Errorf("product_id required")
		}
		if strings.TrimSpace(product.Name) == "" {
			return fmt.Errorf("product %s name required", product.ProductID)
		}
		if product.OpenSourceIssues.Count != len(product.OpenSourceIssues.Refs) {
			return fmt.Errorf("product %s open_source_issues count mismatch", product.ProductID)
		}
		for _, group := range product.OpenHypothesesByMissingSignalSource {
			if strings.TrimSpace(group.Source) == "" || len(group.HypothesisIDs) == 0 {
				return fmt.Errorf("product %s missing-signal source group invalid", product.ProductID)
			}
		}
		for _, group := range product.SignalsWithoutHypothesis {
			if strings.TrimSpace(group.ChangeEvent) == "" || len(group.SignalIDs) == 0 {
				return fmt.Errorf("product %s signal-without-hypothesis group invalid", product.ProductID)
			}
			if group.Consumed && len(group.ConsumedByWorkIDs) == 0 {
				return fmt.Errorf("product %s signal-without-hypothesis group %q consumed without work evidence", product.ProductID, group.ChangeEvent)
			}
			for _, card := range group.ActionCards {
				if err := validateLoopActionCard(product.ProductID, card); err != nil {
					return err
				}
			}
		}
		for _, group := range product.ClosedReviewsByNextAction {
			if strings.TrimSpace(group.NextAction) == "" || len(group.ReviewIDs) == 0 {
				return fmt.Errorf("product %s closed-review next-action group invalid", product.ProductID)
			}
			if group.Consumed && len(group.ConsumedByWorkIDs) == 0 {
				return fmt.Errorf("product %s closed-review next-action %q consumed without work evidence", product.ProductID, group.NextAction)
			}
		}
		for _, card := range product.ActionCards {
			if err := validateLoopActionCard(product.ProductID, card); err != nil {
				return err
			}
		}
		for _, group := range product.OpenHypothesesByMissingSignalSource {
			for _, card := range group.ActionCards {
				if err := validateLoopActionCard(product.ProductID, card); err != nil {
					return err
				}
			}
		}
		for _, group := range product.ClosedReviewsByNextAction {
			for _, card := range group.ActionCards {
				if err := validateLoopActionCard(product.ProductID, card); err != nil {
					return err
				}
			}
		}
		for metric, signals := range product.SignalsByMetric {
			if strings.TrimSpace(metric) == "" {
				return fmt.Errorf("product %s has empty metric key", product.ProductID)
			}
			for _, signal := range signals {
				if strings.TrimSpace(signal.SignalID) == "" {
					return fmt.Errorf("product %s signal_id required", product.ProductID)
				}
				if signal.Metric != metric {
					return fmt.Errorf("product %s signal %s metric %q mismatches key %q", product.ProductID, signal.SignalID, signal.Metric, metric)
				}
			}
		}
	}
	return nil
}

func validateLoopActionCard(productID string, card LoopActionCard) error {
	if strings.TrimSpace(card.ID) == "" {
		return fmt.Errorf("product %s action card id required", productID)
	}
	if strings.TrimSpace(card.Kind) == "" {
		return fmt.Errorf("product %s action card %s kind required", productID, card.ID)
	}
	if strings.TrimSpace(card.Title) == "" {
		return fmt.Errorf("product %s action card %s title required", productID, card.ID)
	}
	if len(card.Command) == 0 {
		return fmt.Errorf("product %s action card %s command required", productID, card.ID)
	}
	if strings.TrimSpace(card.Boundary) == "" {
		return fmt.Errorf("product %s action card %s boundary required", productID, card.ID)
	}
	if strings.TrimSpace(card.Proof) == "" {
		return fmt.Errorf("product %s action card %s proof required", productID, card.ID)
	}
	return nil
}

func normalizeLoopOptions(opts LoopOptions) LoopOptions {
	if opts.Recent <= 0 {
		opts.Recent = 10
	}
	if opts.StaleAfter <= 0 {
		days := opts.StaleDays
		if days <= 0 {
			days = 7
		}
		opts.StaleAfter = time.Duration(days) * 24 * time.Hour
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	opts.ProductID = strings.TrimSpace(opts.ProductID)
	return opts
}

func buildProductLoop(homePath, productID string, metadata loopProductMetadata, reviews []Review, consumedActions loopActionConsumptionIndex, releases sourceobs.ReleaseObservationsReport, opts LoopOptions) (ProductLoop, error) {
	tl, err := LoadTimeline(homePath, productID)
	if err != nil {
		return ProductLoop{}, err
	}
	doctor, err := Doctor(homePath, productID)
	if err != nil {
		return ProductLoop{}, err
	}
	if metadata.Name == "" {
		metadata.Name = productID
	}
	openHypotheses := openLoopHypotheses(tl.Hypotheses)
	signalsByMetric, includedSignals := loopSignalsByMetric(tl.Signals, tl.Transitions, opts.Recent)
	productReviews := loopReviews(productID, reviews, opts.Recent)
	missingSignalSources := loopMissingSignalSources(productID, openHypotheses, tl.Signals)
	signalHypothesisGaps := loopSignalsWithoutHypothesis(productID, tl.Hypotheses, includedSignals, consumedActions)
	reviewsByNextAction := loopReviewNextActions(productID, productReviews, consumedActions)
	actionCards := loopActionCards(missingSignalSources, signalHypothesisGaps, reviewsByNextAction, loopReviewReadyActionCards(productID, doctor.ReviewReady))
	sourceIssues := loopSourceIssues(doctor.SourceIssues)
	// Reduce the shared read-only release snapshot. Invalid evidence remains
	// visible in the view; an unreadable inventory invalidates release awareness
	// for the portfolio without suppressing unrelated product-loop content.
	var releaseAwareness *ReleaseAwareness
	awareness := buildReleaseAwareness(productID, metadata.ReleaseIdentity, ReleaseAwarenessOptions{Now: opts.Now, StaleAfter: opts.StaleAfter}, releases)
	if awareness.ObservationCount > 0 || awareness.Status != "unobserved" {
		releaseAwareness = &awareness
	}

	return ProductLoop{
		ProductID:                           productID,
		Name:                                metadata.Name,
		SignalsByMetric:                     signalsByMetric,
		OpenHypotheses:                      openHypotheses,
		OpenHypothesesByMissingSignalSource: missingSignalSources,
		SignalsWithoutHypothesis:            signalHypothesisGaps,
		ActionCards:                         actionCards,
		Reviews:                             productReviews,
		ClosedReviewsByNextAction:           reviewsByNextAction,
		OpenSourceIssues:                    sourceIssues,
		Freshness:                           loopFreshness(tl.Signals, productReviews, opts.Now, opts.StaleAfter),
		LoopGaps:                            loopGaps(tl.Hypotheses, tl.Signals, includedSignals, doctor),
		ReleaseAwareness:                    releaseAwareness,
	}, nil
}

// loopProductMetadata is the recalled name and release contract, loaded in
// one claims pass for the portfolio. Missing claims mean an empty identity;
// unreadable claims are an error, never silently ignored.
type loopProductMetadata struct {
	Name            string
	ReleaseIdentity ReleaseIdentity
}

func productLoopMetadata(store *mem.Store) (map[string]loopProductMetadata, error) {
	out := map[string]loopProductMetadata{}
	claims, err := store.AllCurrent()
	if err != nil {
		return nil, err
	}
	for _, claim := range claims {
		if claim.Qualifiers["domain"] != "product" {
			continue
		}
		item := out[claim.Entity]
		switch claim.Predicate {
		case "name", "display_name":
			item.Name = claim.Object
		case "release_version":
			item.ReleaseIdentity.Version = claim.Object
		case "release_build":
			item.ReleaseIdentity.Build = claim.Object
		}
		out[claim.Entity] = item
	}
	return out, nil
}

// Partition the snapshot once; product reducers never scan the home or the
// other products' evidence. Unattributable malformed evidence remains visible.
func releaseReportsByProduct(report sourceobs.ReleaseObservationsReport, productIDs []string) map[string]sourceobs.ReleaseObservationsReport {
	out := map[string]sourceobs.ReleaseObservationsReport{}
	for _, record := range report.Records {
		id := record.Observation.ProductID
		item := out[id]
		item.Records = append(item.Records, record)
		out[id] = item
	}
	for _, issue := range report.Issues {
		for _, id := range productIDs {
			if !issue.AppliesTo(id) {
				continue
			}
			item := out[id]
			item.Issues = append(item.Issues, issue)
			out[id] = item
		}
	}

	return out
}

func loopSignalsByMetric(signals []Signal, transitions []TimelineTransition, recent int) (map[string][]LoopSignal, []Signal) {
	transitionBySignal := map[string]string{}
	for _, tr := range transitions {
		kind, id, ok := strings.Cut(tr.ObjectRef, ":")
		if ok && kind == "product_signal" && id != "" {
			transitionBySignal[id] = tr.ID
		}
	}
	ordered := append([]Signal(nil), signals...)
	sort.Slice(ordered, func(i, j int) bool {
		if compareRFC3339(ordered[i].ObservedAt, ordered[j].ObservedAt) == 0 {
			return ordered[i].ID < ordered[j].ID
		}
		return compareRFC3339(ordered[i].ObservedAt, ordered[j].ObservedAt) > 0
	})
	out := map[string][]LoopSignal{}
	var included []Signal
	for _, signal := range ordered {
		metric := strings.TrimSpace(signal.MetricName)
		if metric == "" {
			metric = "unknown"
		}
		if len(out[metric]) >= recent {
			continue
		}
		out[metric] = append(out[metric], LoopSignal{
			SignalID:      signal.ID,
			Provider:      loopSignalProvider(signal),
			Metric:        metric,
			Before:        signal.Before,
			After:         signal.After,
			ObservedAt:    signal.ObservedAt,
			ChangeEvent:   signal.ChangeEvent,
			TransitionRef: transitionBySignal[signal.ID],
		})
		included = append(included, signal)
	}
	return out, included
}

func loopSignalProvider(signal Signal) string {
	// Bridged ads signals share one metric family (ads_*) across every ad
	// platform, so the platform lives in the evidence refs (ads:<platform>:…),
	// exactly as the ads bridge writes them.
	for _, ref := range signal.EvidenceRefs {
		if !strings.HasPrefix(ref, "ads:") {
			continue
		}
		if parts := strings.SplitN(ref, ":", 3); len(parts) >= 2 {
			if _, ok := LookupAdPlatform(parts[1]); ok {
				return parts[1]
			}
		}
	}
	return loopMetricProvider(signal.MetricName)
}

func loopMetricProvider(metricName string) string {
	metric := normalizeIDPart(metricName)
	for key, canonical := range canonicalSignalMetricNames {
		if normalizeIDPart(canonical) != metric {
			continue
		}
		provider, _, ok := strings.Cut(key, ":")
		if ok && provider != "" {
			return provider
		}
	}
	switch {
	case strings.HasPrefix(metric, "gsc_"):
		return "google_search_console"
	case strings.HasPrefix(metric, "ga4_"):
		return "google_analytics"
	case strings.HasPrefix(metric, "adsense_"):
		return "google_adsense"
	case strings.HasPrefix(metric, "app_store_connect_") || strings.HasPrefix(metric, "app_store_"):
		return "app_store_connect"
	case strings.HasPrefix(metric, "ads_"):
		return "ads"
	case strings.HasPrefix(metric, "revenuecat_"):
		return "revenuecat"
	case strings.HasPrefix(metric, "posthog_"):
		return "posthog"
	default:
		return "unknown"
	}
}

func openLoopHypotheses(hypotheses []HypothesisCandidate) []LoopHypothesis {
	var out []LoopHypothesis
	for _, h := range hypotheses {
		if !isOpenLoopHypothesis(h) {
			continue
		}
		out = append(out, LoopHypothesis{
			ID:                h.ID,
			Statement:         h.ChangeEvent,
			ExpectedEffect:    h.ExpectedEffect,
			Metrics:           compact(h.Metrics),
			ObservationWindow: h.ObservationWindow,
			NextAction:        h.NextAction,
			EvidenceRefs:      compact(h.EvidenceRefs),
			State:             "open",
			CreatedAt:         h.CreatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if compareRFC3339(out[i].CreatedAt, out[j].CreatedAt) == 0 {
			return out[i].ID < out[j].ID
		}
		return compareRFC3339(out[i].CreatedAt, out[j].CreatedAt) > 0
	})
	return out
}

func isOpenLoopHypothesis(h HypothesisCandidate) bool {
	result := strings.TrimSpace(h.Result)
	verdict := strings.TrimSpace(h.Verdict)
	if result != "" && result != "unknown" {
		return false
	}
	return verdict == "" || verdict == candidates.Park
}

func loopMissingSignalSources(productID string, openHypotheses []LoopHypothesis, signals []Signal) []LoopMissingSignalSource {
	bySource := map[string]*LoopMissingSignalSource{}
	for _, h := range openHypotheses {
		if loopHypothesisHasSignal(h, signals) {
			continue
		}
		sources := loopHypothesisMetricSources(h.Metrics)
		if len(sources) == 0 {
			sources = []string{"unknown"}
		}
		for _, source := range sources {
			group := bySource[source]
			if group == nil {
				group = &LoopMissingSignalSource{Source: source}
				bySource[source] = group
			}
			group.HypothesisIDs = append(group.HypothesisIDs, h.ID)
			group.Metrics = append(group.Metrics, h.Metrics...)
			group.NextActions = append(group.NextActions, h.NextAction)
		}
	}
	out := make([]LoopMissingSignalSource, 0, len(bySource))
	for _, group := range bySource {
		group.HypothesisIDs = sortedUnique(group.HypothesisIDs)
		group.Metrics = sortedUnique(group.Metrics)
		group.NextActions = sortedUnique(group.NextActions)
		group.ActionCards = []LoopActionCard{missingSignalSourceActionCard(productID, *group)}
		out = append(out, *group)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Source < out[j].Source
	})
	return out
}

func loopHypothesisHasSignal(h LoopHypothesis, signals []Signal) bool {
	explicitSignalID := productSignalRef(h.EvidenceRefs)
	for _, signal := range signals {
		if explicitSignalID != "" && signal.ID == explicitSignalID {
			return true
		}
		signalMetric := normalizeIDPart(signal.MetricName)
		for _, metric := range h.Metrics {
			if signalMetric != "" && normalizeIDPart(metric) == signalMetric {
				return true
			}
		}
		if normalizeIDPart(h.Statement) != "" && normalizeIDPart(h.Statement) == normalizeIDPart(signal.ChangeEvent) {
			return true
		}
	}
	return false
}

func loopSignalsWithoutHypothesis(productID string, hypotheses []HypothesisCandidate, signals []Signal, consumedActions loopActionConsumptionIndex) []LoopSignalHypothesisGap {
	byChange := map[string]*LoopSignalHypothesisGap{}
	for _, signal := range signals {
		changeEvent := strings.TrimSpace(signal.ChangeEvent)
		if changeEvent == "" || signalHasHypothesis(hypotheses, signal) {
			continue
		}
		key := normalizeIDPart(productID + " " + changeEvent)
		if key == "" {
			key = normalizeIDPart(productID + " " + signal.ID)
		}
		group := byChange[key]
		if group == nil {
			group = &LoopSignalHypothesisGap{ChangeEvent: changeEvent}
			byChange[key] = group
		}
		group.SignalIDs = append(group.SignalIDs, signal.ID)
		group.Metrics = append(group.Metrics, signal.MetricName)
		group.Providers = append(group.Providers, loopMetricProvider(signal.MetricName))
	}
	out := make([]LoopSignalHypothesisGap, 0, len(byChange))
	for _, group := range byChange {
		group.SignalIDs = sortedUnique(group.SignalIDs)
		group.Metrics = sortedUnique(group.Metrics)
		group.Providers = sortedUnique(group.Providers)
		card := signalWithoutHypothesisActionCard(productID, *group)
		if consumed, ok := consumedActions[card.ID]; ok {
			group.Consumed = true
			group.ConsumedByWorkIDs = append([]string{}, consumed.WorkIDs...)
			group.ConsumedEvidenceRefs = append([]string{}, consumed.EvidenceRefs...)
		} else {
			group.ActionCards = []LoopActionCard{card}
		}
		out = append(out, *group)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ChangeEvent == out[j].ChangeEvent {
			return strings.Join(out[i].SignalIDs, ",") < strings.Join(out[j].SignalIDs, ",")
		}
		return out[i].ChangeEvent < out[j].ChangeEvent
	})
	return out
}

func signalHasHypothesis(hypotheses []HypothesisCandidate, signal Signal) bool {
	for _, h := range hypotheses {
		if signalHypothesisMatchReason(h, signal) != "" {
			return true
		}
	}
	return false
}

func loopHypothesisMetricSources(metrics []string) []string {
	var sources []string
	for _, metric := range metrics {
		sources = append(sources, loopMetricProvider(metric))
	}
	return sortedUnique(sources)
}

func loopReviews(productID string, reviews []Review, recent int) []LoopReview {
	var out []LoopReview
	for _, review := range reviews {
		if review.ProductID != productID {
			continue
		}
		out = append(out, LoopReview{
			ID:           review.ID,
			HypothesisID: review.HypothesisID,
			SignalID:     productSignalRef(review.EvidenceRefs),
			Verdict:      review.Result,
			NextAction:   review.NextAction,
			ReviewedAt:   firstNonEmpty(review.ReviewedAt, review.AppliedAt),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if compareRFC3339(out[i].ReviewedAt, out[j].ReviewedAt) == 0 {
			return out[i].ID < out[j].ID
		}
		return compareRFC3339(out[i].ReviewedAt, out[j].ReviewedAt) > 0
	})
	if len(out) > recent {
		out = out[:recent]
	}
	return out
}

type loopActionConsumption struct {
	WorkIDs      []string
	EvidenceRefs []string
}

type loopActionConsumptionIndex map[string]loopActionConsumption

func loopProductActionConsumptionIndex(homePath string) (loopActionConsumptionIndex, error) {
	out := loopActionConsumptionIndex{}
	store, err := work.OpenReadOnly(homePath)
	if err != nil {
		return out, err
	}
	works, err := store.ListWithArchived()
	if err != nil {
		return out, err
	}
	for _, item := range works {
		if item.State != "completed" {
			continue
		}
		var actionCardIDs []string
		var productReviewRefs []string
		for _, ref := range item.EvidenceRefs {
			ref = strings.TrimSpace(ref)
			if id, ok := strings.CutPrefix(ref, "product_action_card:"); ok && strings.TrimSpace(id) != "" {
				actionCardIDs = append(actionCardIDs, strings.TrimSpace(id))
				continue
			}
			if id, ok := strings.CutPrefix(ref, "product_review:"); ok && strings.TrimSpace(id) != "" {
				productReviewRefs = append(productReviewRefs, "product_review:"+strings.TrimSpace(id))
			}
		}
		for _, cardID := range sortedUnique(actionCardIDs) {
			consumed := out[cardID]
			consumed.WorkIDs = append(consumed.WorkIDs, item.ID)
			refs := []string{"work:" + item.ID}
			if item.IDs.TransitionID != "" {
				refs = append(refs, "transition:"+item.IDs.TransitionID)
			}
			refs = append(refs, productReviewRefs...)
			consumed.EvidenceRefs = append(consumed.EvidenceRefs, refs...)
			out[cardID] = consumed
		}
	}
	for cardID, consumed := range out {
		consumed.WorkIDs = sortedUnique(consumed.WorkIDs)
		consumed.EvidenceRefs = sortedUnique(consumed.EvidenceRefs)
		out[cardID] = consumed
	}
	return out, nil
}

func loopReviewNextActions(productID string, reviews []LoopReview, consumedActions loopActionConsumptionIndex) []LoopReviewNextAction {
	byAction := map[string]*LoopReviewNextAction{}
	for _, review := range reviews {
		nextAction := strings.TrimSpace(review.NextAction)
		if nextAction == "" {
			nextAction = "unspecified"
		}
		group := byAction[nextAction]
		if group == nil {
			group = &LoopReviewNextAction{NextAction: nextAction}
			byAction[nextAction] = group
		}
		group.ReviewIDs = append(group.ReviewIDs, review.ID)
		group.HypothesisIDs = append(group.HypothesisIDs, review.HypothesisID)
		group.SignalIDs = append(group.SignalIDs, review.SignalID)
	}
	out := make([]LoopReviewNextAction, 0, len(byAction))
	for _, group := range byAction {
		group.ReviewIDs = sortedUnique(group.ReviewIDs)
		group.HypothesisIDs = sortedUnique(group.HypothesisIDs)
		group.SignalIDs = sortedUnique(group.SignalIDs)
		card := closedReviewNextActionCard(productID, *group)
		if consumed, ok := consumedActions[card.ID]; ok {
			group.Consumed = true
			group.ConsumedByWorkIDs = append([]string{}, consumed.WorkIDs...)
			group.ConsumedEvidenceRefs = append([]string{}, consumed.EvidenceRefs...)
		} else {
			group.ActionCards = []LoopActionCard{card}
		}
		out = append(out, *group)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].NextAction < out[j].NextAction
	})
	return out
}

func loopActionCards(missing []LoopMissingSignalSource, signalGaps []LoopSignalHypothesisGap, reviews []LoopReviewNextAction, reviewReady []LoopActionCard) []LoopActionCard {
	byID := map[string]LoopActionCard{}
	for _, group := range missing {
		for _, card := range group.ActionCards {
			if card.ID != "" {
				byID[card.ID] = card
			}
		}
	}
	for _, group := range signalGaps {
		for _, card := range group.ActionCards {
			if card.ID != "" {
				byID[card.ID] = card
			}
		}
	}
	for _, group := range reviews {
		for _, card := range group.ActionCards {
			if card.ID != "" {
				byID[card.ID] = card
			}
		}
	}
	for _, card := range reviewReady {
		if card.ID != "" {
			byID[card.ID] = card
		}
	}
	out := make([]LoopActionCard, 0, len(byID))
	for _, card := range byID {
		out = append(out, card)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind == out[j].Kind {
			return out[i].ID < out[j].ID
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

func missingSignalSourceActionCard(productID string, group LoopMissingSignalSource) LoopActionCard {
	source := strings.TrimSpace(group.Source)
	if source == "" {
		source = "unknown"
	}
	metricText := strings.Join(group.Metrics, ",")
	if metricText == "" {
		metricText = "unscoped metric"
	}
	return LoopActionCard{
		ID:      "product-loop-missing-signal-" + normalizeIDPart(productID+" "+source),
		Kind:    "product_signal_import",
		Title:   fmt.Sprintf("Import %s signal evidence for %s", source, productID),
		Command: []string{"compound", "product", "signal", "<signal.yaml|source-export.yaml>"},
		RequiredInputs: []string{
			fmt.Sprintf("ProductSignal/source-export for %s from %s", productID, source),
			"observed metric evidence for: " + metricText,
			"change event and confounders tied to the open hypothesis",
		},
		ExpectedValidation: []string{
			"`compound product signal` exits ok and prints signal + transition ids",
			fmt.Sprintf("`compound product loops --product %s --json` no longer lists the same hypotheses under missing source %s when evidence matches", productID, source),
		},
		StopConditions: []string{
			"no observed source data exists yet",
			"source collection would require credentials, spend, public distribution, or another gated external action",
			"signal file still contains placeholders or private data not intended for this Compound home",
		},
		Boundary: "local ProductSignal import only; does not fetch source data, authenticate accounts, publish, spend, or infer causality",
		Proof:    "ProductSignal and Transition are recorded, then product-loop projection moves from missing signal debt toward review-ready or closed review evidence",
	}
}

func signalWithoutHypothesisActionCard(productID string, group LoopSignalHypothesisGap) LoopActionCard {
	changeEvent := strings.TrimSpace(group.ChangeEvent)
	if changeEvent == "" {
		changeEvent = "unscoped product signal"
	}
	metricText := strings.Join(group.Metrics, ",")
	if metricText == "" {
		metricText = "unscoped metric"
	}
	cardID := signalWithoutHypothesisCardID(productID, changeEvent)
	return LoopActionCard{
		ID:            cardID,
		Kind:          "product_signal_hypothesis_review",
		Title:         "Review product signals without a hypothesis",
		Command:       []string{"compound", "product", "action-plan", "--product", productID, "--card", cardID},
		ActionSummary: fmt.Sprintf("Review %d product signals without a matching hypothesis: %s", len(group.SignalIDs), changeEvent),
		RequiredInputs: []string{
			"signal ids: " + strings.Join(group.SignalIDs, ","),
			"metrics: " + metricText,
			"change event: " + changeEvent,
			"operator/product judgment: create hypothesis candidate, park, or reject through reviewed action",
		},
		ExpectedValidation: []string{
			"`compound product action-plan --product <id> --card <card_id>` writes gated product action review artifacts",
			"no ProductHypothesis candidate is created until an explicit reviewed action/work receipt exists",
			fmt.Sprintf("future `compound product loops --product %s --json` either shows matching hypothesis/review evidence or preserves this signal gap", productID),
		},
		StopConditions: []string{
			"signal already matches a ProductHypothesis by explicit ref, metric, or change event",
			"review would infer causality without checking confounders and source freshness",
			"next move requires public, financial, credential, destructive, or security-sensitive action without policy",
		},
		Boundary: "review-gated synthesis only; does not mutate ProductSignal, create ProductHypothesis, infer causality, call providers, publish, spend, or promote memory",
		Proof:    "product action review/work receipt or later ProductHypothesis evidence cites signal ids and this product_action_card ref",
	}
}

func signalWithoutHypothesisCardID(productID, changeEvent string) string {
	changeEvent = strings.TrimSpace(changeEvent)
	if changeEvent == "" {
		changeEvent = "unscoped product signal"
	}
	return "product-loop-signal-hypothesis-" + normalizeIDPart(productID+" "+changeEvent)
}

func closedReviewNextActionCard(productID string, group LoopReviewNextAction) LoopActionCard {
	next := strings.TrimSpace(group.NextAction)
	if next == "" {
		next = "unspecified"
	}
	cardID := "product-loop-review-next-" + normalizeIDPart(productID+" "+next)
	return LoopActionCard{
		ID:            cardID,
		Kind:          "product_next_bounded_move",
		Title:         "Follow reviewed product next action",
		Command:       []string{"compound", "product", "action-plan", "--product", productID, "--card", cardID},
		ActionSummary: next,
		RequiredInputs: []string{
			"bounded next move: " + next,
			"review evidence refs: " + strings.Join(group.ReviewIDs, ","),
		},
		ExpectedValidation: []string{
			"next work leaves ProductSignal, ProductHypothesis, Review, WorkEnvelope receipt, or Transition evidence",
			"future product-loop projection cites the review ids or resulting signal/hypothesis refs",
		},
		StopConditions: []string{
			"next move requires public, financial, destructive, credential, or security-sensitive action without policy",
			"no measurable signal, owner, or proof surface can be named",
		},
		Boundary: "review next_action is evidence for a bounded move, not standing approval for external side effects",
		Proof:    "a future receipt/transition or product-loop evidence ref ties the bounded move back to these review ids",
	}
}

// loopReviewReadyActionCards collapses the (hypothesis, signal)-per-pair
// ReviewReady list into ONE action card per (product, hypothesis). The
// matcher that produces ReviewReady is unchanged — a single hypothesis can
// legitimately match many signals (same metric, different observation),
// but the operator only needs to start one review plan per hypothesis; which
// signal it pairs with is a choice inside that plan, not a reason to mint
// another card. Without this dedupe the hourly director mints N identical
// "Create product review plan" candidates for one starved product and crowds
// every other product-loop candidate out of the 5-proposal budget.
//
// Deterministic representative signal rule: we group ready by HypothesisID,
// then within each group sort by SignalID ascending and take the FIRST.
// Signal IDs are stable identifiers assigned when the signal lands, so the
// smallest one is a reproducible pick regardless of input order. The
// representative signal is the one passed to the runnable review-plan
// command; the matched-signal count and the full sorted list are surfaced
// in RequiredInputs so the operator can see the fan-out without being
// buried by N copies of the same card.
func loopReviewReadyActionCards(productID string, ready []ReviewReady) []LoopActionCard {
	byHypothesis := map[string][]ReviewReady{}
	for _, item := range ready {
		hyp := strings.TrimSpace(item.HypothesisID)
		if hyp == "" {
			continue
		}
		byHypothesis[hyp] = append(byHypothesis[hyp], item)
	}
	hypothesisIDs := make([]string, 0, len(byHypothesis))
	for id := range byHypothesis {
		hypothesisIDs = append(hypothesisIDs, id)
	}
	sort.Strings(hypothesisIDs)

	out := make([]LoopActionCard, 0, len(hypothesisIDs))
	for _, hypID := range hypothesisIDs {
		items := byHypothesis[hypID]
		sort.Slice(items, func(i, j int) bool { return items[i].SignalID < items[j].SignalID })
		repSignalID := strings.TrimSpace(items[0].SignalID)
		if repSignalID == "" {
			continue
		}
		signalIDs := make([]string, 0, len(items))
		for _, item := range items {
			signalIDs = append(signalIDs, strings.TrimSpace(item.SignalID))
		}
		sort.Strings(signalIDs)
		matched := strconv.Itoa(len(items))
		title := "Create product review plan"
		if len(items) > 1 {
			title = "Create product review plan (" + matched + " signals matched)"
		}
		command := []string{"compound", "product", "review-plan", "--hypothesis", hypID, "--signal", repSignalID}
		out = append(out, LoopActionCard{
			ID:      "product-review-plan-" + normalizeIDPart(productID+" "+hypID),
			Kind:    "product_review_plan",
			Title:   title,
			Command: command,
			RequiredInputs: []string{
				"hypothesis: " + hypID,
				"representative signal: " + repSignalID,
				"matched signals: " + matched + " (" + strings.Join(signalIDs, ",") + ")",
				"optional target file only if the review needs a concrete artifact",
			},
			ExpectedValidation: []string{
				"`compound product review-plan` prints plan, review, and review_yaml paths",
				"review-plan creates review artifacts only; reviewer still applies `compound product review <review.yaml>` explicitly",
			},
			StopConditions: []string{
				"signal and hypothesis do not match current product-loop evidence",
				"review would infer causality without checking confounders",
			},
			Boundary: "review-plan writes gated review artifacts only; it does not apply review, mutate product surfaces, publish, or promote memory",
			Proof:    "review-plan output contains review_yaml; later explicit product review closes or parks the loop with receipt evidence",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func productSignalRef(refs []string) string {
	for _, ref := range refs {
		id, ok := strings.CutPrefix(strings.TrimSpace(ref), "product_signal:")
		if ok {
			return id
		}
	}
	return ""
}

func loopSourceIssues(issues []ProductSourceIssue) LoopSourceIssues {
	refs := make([]string, 0, len(issues))
	for _, issue := range issues {
		refs = append(refs, "product_source_issue:"+issue.ID)
	}
	sort.Strings(refs)
	return LoopSourceIssues{Count: len(refs), Refs: refs}
}

func loopFreshness(signals []Signal, reviews []LoopReview, now time.Time, staleAfter time.Duration) LoopFreshness {
	var latestSignalAt string
	for _, signal := range signals {
		latestSignalAt = maxRFC3339(latestSignalAt, signal.ObservedAt)
	}
	var latestReviewAt string
	for _, review := range reviews {
		latestReviewAt = maxRFC3339(latestReviewAt, review.ReviewedAt)
	}
	freshness := LoopFreshness{LatestSignalAt: latestSignalAt, LatestReviewAt: latestReviewAt}
	latest, err := time.Parse(time.RFC3339, latestSignalAt)
	if latestSignalAt == "" || err != nil {
		freshness.Stale = true
		return freshness
	}
	freshness.Stale = now.UTC().Sub(latest) > staleAfter
	return freshness
}

func loopGaps(hypotheses []HypothesisCandidate, signals []Signal, recentSignals []Signal, doctor DoctorReport) []string {
	var gaps []string
	if hasSignalWithoutHypothesis(hypotheses, recentSignals) {
		gaps = append(gaps, "signals_without_hypothesis")
	}
	if hasHypothesisWithoutSignal(hypotheses, signals) {
		gaps = append(gaps, "hypothesis_without_signal")
	}
	if doctor.ReviewReadyCount > 0 {
		gaps = append(gaps, "review_ready")
	}
	return gaps
}

func hasSignalWithoutHypothesis(hypotheses []HypothesisCandidate, signals []Signal) bool {
	for _, signal := range signals {
		if strings.TrimSpace(signal.ChangeEvent) == "" {
			continue
		}
		if !signalHasHypothesis(hypotheses, signal) {
			return true
		}
	}
	return false
}

func hasHypothesisWithoutSignal(hypotheses []HypothesisCandidate, signals []Signal) bool {
	for _, h := range hypotheses {
		if !isOpenLoopHypothesis(h) {
			continue
		}
		matched := false
		for _, signal := range signals {
			if signalHypothesisMatchReason(h, signal) != "" {
				matched = true
				break
			}
		}
		if !matched {
			return true
		}
	}
	return false
}

func sortedUnique(values []string) []string {
	out := compact(values)
	sort.Strings(out)
	return out
}

func compareRFC3339(a, b string) int {
	at, aerr := time.Parse(time.RFC3339, a)
	bt, berr := time.Parse(time.RFC3339, b)
	switch {
	case aerr == nil && berr == nil:
		if at.Before(bt) {
			return -1
		}
		if at.After(bt) {
			return 1
		}
		return 0
	case a == b:
		return 0
	case a == "":
		return -1
	case b == "":
		return 1
	case a < b:
		return -1
	default:
		return 1
	}
}

func maxRFC3339(a, b string) string {
	if compareRFC3339(a, b) >= 0 {
		return a
	}
	return b
}

func DecodeLoopModel(data []byte) (LoopModel, error) {
	var model LoopModel
	if err := json.Unmarshal(data, &model); err != nil {
		return LoopModel{}, err
	}
	return model, model.Validate()
}
