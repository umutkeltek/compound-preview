package product

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/mem"
)

type CanonicalMetricsOptions struct {
	ProductID string
}

type CanonicalMetricsReport struct {
	Status             string   `json:"status"`
	ProductID          string   `json:"product_id"`
	SourceRef          string   `json:"source_ref"`
	SourceHash         string   `json:"source_hash"`
	IdeasRead          int      `json:"ideas_read"`
	SignalsCreated     int      `json:"signals_created"`
	SignalsExisting    int      `json:"signals_existing"`
	HypothesesCreated  int      `json:"hypotheses_created"`
	HypothesesExisting int      `json:"hypotheses_existing"`
	SignalIDs          []string `json:"signal_ids,omitempty"`
	HypothesisIDs      []string `json:"hypothesis_ids,omitempty"`
	EvidenceIDs        []string `json:"evidence_ids,omitempty"`
	TransitionIDs      []string `json:"transition_ids,omitempty"`
	SkippedIdeaIDs     []string `json:"skipped_idea_ids,omitempty"`
	SkippedIdeaReasons []string `json:"skipped_idea_reasons,omitempty"`
}

type canonicalMetricsDocument struct {
	Kind     string                 `json:"kind"`
	Date     string                 `json:"date"`
	PulledAt string                 `json:"pulled_at"`
	Name     string                 `json:"name"`
	Product  any                    `json:"product"`
	Current  map[string]any         `json:"current"`
	Ideas    []canonicalMetricsIdea `json:"ideas"`
}

type canonicalMetricsIdea struct {
	ID                  string                    `json:"id"`
	Status              string                    `json:"status"`
	AutomationLevel     string                    `json:"automation_level"`
	ApprovalGate        string                    `json:"approval_gate"`
	Surface             string                    `json:"surface"`
	Score               float64                   `json:"score"`
	Title               string                    `json:"title"`
	Why                 string                    `json:"why"`
	SuggestedNextAction string                    `json:"suggested_next_action"`
	Evidence            []canonicalMetricEvidence `json:"evidence"`
}

type canonicalMetricEvidence struct {
	Metric        string   `json:"metric"`
	Value         *float64 `json:"value"`
	AsOf          string   `json:"as_of"`
	ProductRecord string   `json:"product_record"`
}

func ApplyCanonicalMetrics(homePath string, store *mem.Store, path string, opts CanonicalMetricsOptions, now time.Time) (CanonicalMetricsReport, error) {
	productID := strings.TrimSpace(opts.ProductID)
	if productID == "" {
		return CanonicalMetricsReport{Status: "fail"}, fmt.Errorf("canonical metrics product_id required")
	}
	if store == nil {
		return CanonicalMetricsReport{Status: "fail", ProductID: productID}, fmt.Errorf("memory store required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return CanonicalMetricsReport{Status: "fail", ProductID: productID}, err
	}
	var doc canonicalMetricsDocument
	if err := json.Unmarshal(b, &doc); err != nil {
		return CanonicalMetricsReport{Status: "fail", ProductID: productID}, fmt.Errorf("parse canonical metrics %s: %w", path, err)
	}
	sourceHash := "sha256:" + hashBytes(b)
	sourceRef := canonicalMetricsSourceRef(doc, path)
	report := CanonicalMetricsReport{
		Status:     "ok",
		ProductID:  productID,
		SourceRef:  sourceRef,
		SourceHash: sourceHash,
		IdeasRead:  len(doc.Ideas),
	}

	existingSignals, err := loadSignals(homePath)
	if err != nil {
		return report, err
	}
	existingSignalIDs := map[string]bool{}
	for _, signal := range existingSignals {
		existingSignalIDs[signal.ID] = true
	}

	for _, idea := range doc.Ideas {
		hypothesis, signalDocs := adaptCanonicalMetricsIdea(doc, idea, productID, sourceRef, sourceHash, now)
		if hypothesis.ID == "" {
			report.SkippedIdeaIDs = append(report.SkippedIdeaIDs, strings.TrimSpace(idea.ID))
			report.SkippedIdeaReasons = append(report.SkippedIdeaReasons, "idea has no metric evidence")
			continue
		}
		for _, signalDoc := range signalDocs {
			existed := existingSignalIDs[signalDoc.Signal.ID]
			subReport, err := ApplySignal(homePath, store, signalDoc, path, now)
			if err != nil {
				return report, err
			}
			if existed {
				report.SignalsExisting++
			} else {
				report.SignalsCreated++
				existingSignalIDs[signalDoc.Signal.ID] = true
			}
			report.SignalIDs = append(report.SignalIDs, subReport.SignalIDs...)
			report.EvidenceIDs = append(report.EvidenceIDs, subReport.EvidenceIDs...)
			report.TransitionIDs = append(report.TransitionIDs, subReport.TransitionIDs...)
		}
		created, err := writeHypothesisCandidateIfAbsent(homePath, hypothesis)
		if err != nil {
			return report, err
		}
		if created {
			report.HypothesesCreated++
		} else {
			report.HypothesesExisting++
		}
		report.HypothesisIDs = append(report.HypothesisIDs, hypothesis.ID)
	}
	return report, nil
}

func adaptCanonicalMetricsIdea(doc canonicalMetricsDocument, idea canonicalMetricsIdea, productID, sourceRef, sourceHash string, now time.Time) (HypothesisCandidate, []SignalDocument) {
	ideaID := canonicalIdeaID(idea, productID, sourceRef)
	metrics := canonicalIdeaMetrics(idea)
	if len(metrics) == 0 {
		return HypothesisCandidate{}, nil
	}
	changeEvent := "Canonical product metrics snapshot flagged: " + firstNonEmpty(idea.Title, ideaID)
	observedAt := firstNonEmpty(doc.PulledAt, now.UTC().Format(time.RFC3339))
	observationWindow := canonicalObservationWindow(doc, idea)
	baseRefs := compact([]string{
		sourceRef,
		sourceHash,
		"product_metrics:idea:" + ideaID,
		"product_metrics:status:" + normalizeIDPart(idea.Status),
		"product_metrics:gate:" + normalizeIDPart(idea.ApprovalGate),
	})
	hypothesis := HypothesisCandidate{
		SchemaVersion:     ProductHypothesisCandidate,
		ID:                ideaID,
		Kind:              string(candidates.ProductHypothesis),
		ProductID:         productID,
		ChangeEvent:       changeEvent,
		ExpectedEffect:    firstNonEmpty(idea.Why, idea.Title, "canonical product metrics require review"),
		Metrics:           metrics,
		ObservationWindow: observationWindow,
		Result:            "unknown",
		NextAction:        firstNonEmpty(idea.SuggestedNextAction, "review product metrics before product action"),
		Verdict:           candidates.Park,
		EvidenceRefs:      compact(append(baseRefs, canonicalIdeaEvidenceRefs(ideaID, idea)...)),
		CreatedAt:         now.UTC().Format(time.RFC3339),
	}
	var signalDocs []SignalDocument
	for _, ev := range idea.Evidence {
		metric := strings.TrimSpace(ev.Metric)
		if metric == "" || ev.Value == nil {
			continue
		}
		metricRefs := compact(append(baseRefs, canonicalMetricEvidenceRefs(ideaID, ev, *ev.Value)...))
		signal := Signal{
			ID:          stableID("sig_", productID, sourceRef, ideaID, metric, ev.AsOf, shortFloat(*ev.Value)),
			ProductID:   productID,
			Surface:     canonicalIdeaSurface(idea, metric),
			ChangeEvent: changeEvent,
			MetricName:  metric,
			Before:      *ev.Value,
			After:       *ev.Value,
			Window:      "snapshot " + firstNonEmpty(ev.AsOf, doc.Date, "unknown-date"),
			Confidence:  canonicalMetricConfidence(idea.Score),
			Confounders: []string{
				"canonical metrics snapshot; no causal movement is implied",
				"idea requires governed product review before action",
			},
			Interpretations: []Interpretation{{
				Lens:  "product_metrics_snapshot",
				Claim: "source flagged metric condition for review; evidence is observation, not truth",
			}},
			NextAction:   firstNonEmpty(idea.SuggestedNextAction, "review product metrics before product action"),
			EvidenceRefs: metricRefs,
			ObservedAt:   observedAt,
		}
		signalDocs = append(signalDocs, SignalDocument{
			SchemaVersion: SignalSchemaVersion,
			Source: SignalSource{
				Kind: "product_metrics_canonical",
				Ref:  sourceRef,
			},
			Signal:       signal,
			EvidenceRefs: metricRefs,
		})
	}
	return hypothesis, signalDocs
}

func writeHypothesisCandidateIfAbsent(homePath string, c HypothesisCandidate) (bool, error) {
	var existing HypothesisCandidate
	err := candidates.ReadCandidate(homePath, candidates.ProductHypothesis, c.ID, &existing)
	if err == nil {
		if existing.ProductID != "" && existing.ProductID != c.ProductID {
			return false, fmt.Errorf("product hypothesis %q already exists for product %q, not %q", c.ID, existing.ProductID, c.ProductID)
		}
		return false, nil
	}
	if !os.IsNotExist(err) {
		return false, err
	}
	return true, writeHypothesisCandidate(homePath, c)
}

func canonicalMetricsSourceRef(doc canonicalMetricsDocument, path string) string {
	productKey := canonicalDocumentProductKey(doc)
	if productKey == "" {
		productKey = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	date := firstNonEmpty(doc.Date, doc.PulledAt, "undated")
	return "product_metrics:canonical_latest:" + normalizeIDPart(productKey) + ":" + normalizeIDPart(date)
}

func canonicalDocumentProductKey(doc canonicalMetricsDocument) string {
	switch v := doc.Product.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		for _, key := range []string{"slug", "id", "name"} {
			if raw, ok := v[key].(string); ok && strings.TrimSpace(raw) != "" {
				return strings.TrimSpace(raw)
			}
		}
	}
	return ""
}

func canonicalIdeaID(idea canonicalMetricsIdea, productID, sourceRef string) string {
	id := strings.TrimSpace(idea.ID)
	if id == "" || id == "." || id == ".." || id != filepath.Base(id) || strings.ContainsAny(id, `/\`) {
		id = stableID("hyp_", productID, sourceRef, idea.Title, idea.Why)
	}
	return id
}

func canonicalIdeaMetrics(idea canonicalMetricsIdea) []string {
	var metrics []string
	for _, ev := range idea.Evidence {
		if strings.TrimSpace(ev.Metric) != "" {
			metrics = append(metrics, strings.TrimSpace(ev.Metric))
		}
	}
	return compact(metrics)
}

func canonicalIdeaEvidenceRefs(ideaID string, idea canonicalMetricsIdea) []string {
	var refs []string
	for _, ev := range idea.Evidence {
		refs = append(refs, canonicalMetricEvidenceRefs(ideaID, ev, 0)...)
	}
	return compact(refs)
}

func canonicalMetricEvidenceRefs(ideaID string, ev canonicalMetricEvidence, value float64) []string {
	metric := normalizeIDPart(ev.Metric)
	asOf := normalizeIDPart(ev.AsOf)
	refs := []string{
		"product_metrics:idea:" + ideaID + ":metric:" + metric,
		"product_metrics:metric:" + metric,
	}
	if asOf != "" {
		refs = append(refs, "product_metrics:as_of:"+asOf)
	}
	if ev.Value != nil {
		refs = append(refs, "product_metrics:value:"+metric+":"+shortFloat(*ev.Value))
	} else if value != 0 {
		refs = append(refs, "product_metrics:value:"+metric+":"+shortFloat(value))
	}
	if strings.TrimSpace(ev.ProductRecord) != "" {
		refs = append(refs, "product_metrics:record:"+filepath.Base(ev.ProductRecord))
	}
	return compact(refs)
}

func canonicalObservationWindow(doc canonicalMetricsDocument, idea canonicalMetricsIdea) string {
	var dates []string
	for _, ev := range idea.Evidence {
		if strings.TrimSpace(ev.AsOf) != "" {
			dates = append(dates, strings.TrimSpace(ev.AsOf))
		}
	}
	dates = compact(dates)
	if len(dates) == 0 {
		return "canonical latest " + firstNonEmpty(doc.Date, "unknown-date")
	}
	if len(dates) == 1 {
		return "canonical latest " + dates[0]
	}
	return "canonical latest " + dates[0] + ".." + dates[len(dates)-1]
}

func canonicalIdeaSurface(idea canonicalMetricsIdea, metric string) string {
	if strings.TrimSpace(idea.Surface) != "" {
		return strings.TrimSpace(idea.Surface)
	}
	switch {
	case strings.HasPrefix(metric, "app."):
		return "app_store"
	case strings.HasPrefix(metric, "web."):
		return "web"
	default:
		return "product"
	}
}

func canonicalMetricConfidence(score float64) float64 {
	switch {
	case score > 1:
		if score > 100 {
			return 1
		}
		return score / 100
	case score > 0:
		return score
	default:
		return 0.55
	}
}
