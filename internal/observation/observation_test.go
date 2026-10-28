package observation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/eventsource"
	"compound/internal/eventstore"
	"compound/internal/knowledge"
	"compound/internal/mem"
	"compound/internal/product"
	"compound/internal/reviewartifact"
	"compound/internal/routeevidence"
	"compound/internal/sourceobs"
	"compound/internal/work"
)

var tObservation = time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

func TestPlanRoutesObservationBatchToExistingOwners(t *testing.T) {
	value := 123.0
	batch := validBatch()
	batch.Observations = []Observation{
		{ID: "obs_metric", Kind: "metric", Metric: "clicks", Value: &value, ObservedAt: tObservation.Format(time.RFC3339), Confidence: 1},
		{ID: "obs_content", Kind: "content", ObservedAt: tObservation.Format(time.RFC3339), Confidence: 0.8},
		{ID: "obs_event", Kind: "event", ObservedAt: tObservation.Format(time.RFC3339), Confidence: 1},
		{ID: "obs_route", Kind: "route", ObservedAt: tObservation.Format(time.RFC3339), Confidence: 1},
		{ID: "obs_tool", Kind: "tool_health", ObservedAt: tObservation.Format(time.RFC3339), Confidence: 1},
		{ID: "obs_social", Kind: "distribution_feedback", ObservedAt: tObservation.Format(time.RFC3339), Confidence: 0.7},
		{ID: "obs_unknown", Kind: "mystery", ObservedAt: tObservation.Format(time.RFC3339), Confidence: 0.5},
	}
	report, err := ObserveBatch("", nil, batch, "fixtures/observation/mixed.json", Options{}, tObservation)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"obs_metric":  RouteProductSignal,
		"obs_content": RouteKnowledgeExport,
		"obs_event":   RouteTraceEnvelope,
		"obs_route":   RouteRouteEvidence,
		"obs_tool":    RouteSourceObservation,
		"obs_social":  RouteCandidateOnly,
		"obs_unknown": RouteUnsupportedKind,
	}
	if len(report.Decisions) != len(want) {
		t.Fatalf("decisions = %+v", report.Decisions)
	}
	for _, decision := range report.Decisions {
		if got := decision.Route; got != want[decision.ObservationID] {
			t.Fatalf("%s route = %s, want %s", decision.ObservationID, got, want[decision.ObservationID])
		}
	}
	if report.RouteCounts[RouteProductSignal] != 1 || report.RouteCounts[RouteCandidateOnly] != 1 || report.RouteCounts[RouteUnsupportedKind] != 1 {
		t.Fatalf("route counts = %+v", report.RouteCounts)
	}
}

func TestApplyProductMetricObservationsUsesProductSignalOwner(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := product.Load("../../fixtures/product/market-loop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := product.Apply(home, store, bundle, "fixtures/product/market-loop.yaml", tObservation); err != nil {
		t.Fatal(err)
	}
	before, err := product.BuildLoopModel(home, product.LoopOptions{ProductID: "product:focus-notes", Now: tObservation})
	if err != nil {
		t.Fatal(err)
	}
	batch := validBatch()
	batch.Subject.ID = "product:focus-notes"
	batch.Subject.Surface = "landing-page"
	value := 321.0
	batch.Observations = []Observation{
		{
			ID:           "obs_gsc_clicks",
			Kind:         "metric",
			Metric:       "clicks",
			Value:        &value,
			ObservedAt:   tObservation.Format(time.RFC3339),
			Window:       Window{Start: "2026-07-01", End: "2026-07-03"},
			RawRef:       "gsc-row:fixture-clicks",
			Confidence:   1,
			EvidenceRefs: []string{"fixture:gsc:clicks"},
		},
		{ID: "obs_social", Kind: "distribution_feedback", ObservedAt: tObservation.Format(time.RFC3339), Confidence: 0.6},
	}
	report, err := ObserveBatch(home, store, batch, "fixtures/observation/gsc-product-metric.json", Options{ApplyProductSignals: true}, tObservation)
	if err != nil {
		t.Fatal(err)
	}
	if report.DryRun || !report.AppliedProductSignals {
		t.Fatalf("apply flags = dry_run:%t applied:%t", report.DryRun, report.AppliedProductSignals)
	}
	if len(report.ProductApplyResults) != 1 || len(report.ProductApplyResults[0].SignalIDs) != 1 || len(report.ProductApplyResults[0].TransitionIDs) != 1 {
		t.Fatalf("apply results = %+v", report.ProductApplyResults)
	}
	if len(report.SkippedApply) != 1 || report.SkippedApply[0] != "obs_social:"+RouteCandidateOnly {
		t.Fatalf("skipped apply = %+v", report.SkippedApply)
	}
	after, err := product.BuildLoopModel(home, product.LoopOptions{ProductID: "product:focus-notes", Now: tObservation})
	if err != nil {
		t.Fatal(err)
	}
	if signalCount(after, "gsc_clicks") != signalCount(before, "gsc_clicks")+1 {
		t.Fatalf("gsc product loop signals before=%+v after=%+v", before.Products, after.Products)
	}
}

func TestBlockedPrivateInputDoesNotApply(t *testing.T) {
	value := 1.0
	batch := validBatch()
	batch.Source.Privacy = "credential"
	batch.Observations = []Observation{{
		ID:         "obs_token",
		Kind:       "metric",
		Metric:     "clicks",
		Value:      &value,
		ObservedAt: tObservation.Format(time.RFC3339),
		Confidence: 1,
	}}
	report, err := ObserveBatch("", nil, batch, "fixtures/observation/blocked.json", Options{}, tObservation)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Decisions) != 1 || report.Decisions[0].Route != RouteBlockedPrivate {
		t.Fatalf("decisions = %+v", report.Decisions)
	}
}

func TestValidateBatchRejectsUnsafeExternalFields(t *testing.T) {
	value := 1.0
	base := validBatch()
	base.Observations = []Observation{{
		ID:         "obs_safe",
		Kind:       "metric",
		Metric:     "clicks",
		Value:      &value,
		ObservedAt: tObservation.Format(time.RFC3339),
		Confidence: 1,
		RawRef:     "gsc-row:fixture-clicks",
	}}

	cases := []struct {
		name string
		edit func(*Batch)
		want string
	}{
		{
			name: "duplicate observation id",
			edit: func(batch *Batch) {
				batch.Observations = append(batch.Observations, batch.Observations[0])
			},
			want: "duplicate observation id",
		},
		{
			name: "dimension count",
			edit: func(batch *Batch) {
				dims := map[string]string{}
				for i := 0; i < maxObservationDimensions+1; i++ {
					dims[fmt.Sprintf("k_%02d", i)] = "v"
				}
				batch.Observations[0].Dimensions = dims
			},
			want: "too many dimensions",
		},
		{
			name: "secret dimension key",
			edit: func(batch *Batch) {
				batch.Observations[0].Dimensions = map[string]string{"api_key": "redacted"}
			},
			want: "secret-like dimension key",
		},
		{
			name: "secret url query",
			edit: func(batch *Batch) {
				batch.Observations[0].URL = "https://example.invalid/callback?access_token=redacted"
			},
			want: "secret-like",
		},
		{
			name: "absolute raw ref without private source",
			edit: func(batch *Batch) {
				batch.Observations[0].RawRef = "/tmp/compound/private-source.json"
			},
			want: "absolute local path",
		},
		{
			name: "unsupported raw ref scheme",
			edit: func(batch *Batch) {
				batch.Observations[0].RawRef = "file:///tmp/compound/private-source.json"
			},
			want: "unsupported ref scheme",
		},
		{
			name: "oversized content",
			edit: func(batch *Batch) {
				batch.Observations[0].Content = strings.Repeat("x", maxObservationContentLen+1)
			},
			want: "content exceeds",
		},
		{
			name: "too many evidence refs",
			edit: func(batch *Batch) {
				for i := 0; i < maxEvidenceRefs+1; i++ {
					batch.Observations[0].EvidenceRefs = append(batch.Observations[0].EvidenceRefs, fmt.Sprintf("fixture:evidence:%02d", i))
				}
			},
			want: "too many evidence_refs",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			batch := base
			batch.Observations = append([]Observation(nil), base.Observations...)
			tc.edit(&batch)
			err := ValidateBatch(batch)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateBatch error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateBatchAllowsPrivateAbsoluteRefsWhenExplicit(t *testing.T) {
	value := 1.0
	batch := validBatch()
	batch.Source.Privacy = "private"
	batch.Observations = []Observation{{
		ID:         "obs_private_ref",
		Kind:       "metric",
		Metric:     "clicks",
		Value:      &value,
		ObservedAt: tObservation.Format(time.RFC3339),
		Confidence: 1,
		RawRef:     "/tmp/compound/private-source.json",
	}}
	if err := ValidateBatch(batch); err != nil {
		t.Fatalf("ValidateBatch private absolute ref: %v", err)
	}
}

func TestApplySourceObservationsWritesSourceobsNoPromoteMaterial(t *testing.T) {
	home := t.TempDir()
	batch := validBatch()
	batch.Source = Source{
		Kind:       "compound_projection",
		Ref:        "compound://projection/workbench-read-model",
		CapturedAt: tObservation.Format(time.RFC3339),
		Collector:  "fixture-generated-source-edge",
		Privacy:    "synthetic",
	}
	batch.Subject = Subject{
		Kind:    "projection",
		ID:      "compound://projection/workbench",
		Surface: "operator",
		Channel: "workbench",
	}
	batch.Observations = []Observation{{
		ID:           "obs_generated_workbench",
		Kind:         "generated_view",
		ObservedAt:   tObservation.Format(time.RFC3339),
		Title:        "Fixture workbench projection drift",
		Content:      "Synthetic generated view says the workbench projection drifted from source authority.",
		RawRef:       "compound://projection/workbench-read-model#fixture",
		Confidence:   0.9,
		EvidenceRefs: []string{"artifact_sha256:fixture-workbench"},
	}}

	report, err := ObserveBatch(home, nil, batch, "fixtures/observation/generated-system-source.json", Options{ApplySourceObservations: true}, tObservation)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || report.DryRun || !report.AppliedSourceObservations || report.AppliedProductSignals {
		t.Fatalf("source observation apply report = %+v", report)
	}
	if len(report.SourceObservationApplyResults) != 1 {
		t.Fatalf("source observation results = %+v", report.SourceObservationApplyResults)
	}
	result := report.SourceObservationApplyResults[0]
	if result.ObservationID != "obs_generated_workbench" || result.SourceObservationID == "" || len(result.CandidateIDs) != 1 || len(result.NoPromoteIDs) != 1 {
		t.Fatalf("source observation result = %+v", result)
	}
	var candidate sourceobs.CandidateRecord
	if err := candidates.ReadCandidate(home, candidates.MethodUpgrade, result.CandidateIDs[0], &candidate); err != nil {
		t.Fatal(err)
	}
	if candidate.SourceObservationID != result.SourceObservationID || candidate.Verdict != candidates.NoPromote || candidate.PromoteAllowed {
		t.Fatalf("candidate should be no-promote source evidence: %+v", candidate)
	}
	noPromoteLedger, err := os.ReadFile(filepath.Join(home, "ledger", "no-promote-decisions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(noPromoteLedger), result.NoPromoteIDs[0]) || !strings.Contains(string(noPromoteLedger), result.SourceObservationID) {
		t.Fatalf("no-promote ledger does not contain result %+v:\n%s", result, string(noPromoteLedger))
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "product-signals.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("source observation apply must not create product signals ledger: %v", err)
	}
}

func TestApplyKnowledgeRecordsEvidenceCandidatesAndExtraction(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	batch := validBatch()
	batch.Source = Source{
		Kind:       "web_capture",
		Ref:        "https://example.invalid/research/observation-intake",
		CapturedAt: tObservation.Format(time.RFC3339),
		Collector:  "fixture-content-edge",
		Privacy:    "synthetic",
	}
	batch.Subject = Subject{
		Kind:    "research_note",
		ID:      "note:observation-intake",
		Surface: "knowledge",
		Channel: "web",
	}
	batch.Observations = []Observation{{
		ID:           "obs_content_knowledge",
		Kind:         "content",
		ObservedAt:   tObservation.Format(time.RFC3339),
		Title:        "Observation intake keeps payloads as evidence",
		Content:      "External payloads should become citable evidence before any memory claim or decision is promoted.",
		URL:          "https://example.invalid/research/observation-intake",
		RawRef:       "web-row:observation-intake",
		Confidence:   0.92,
		Dimensions:   map[string]string{"topic": "observation_intake", "domain": "knowledge"},
		EvidenceRefs: []string{"artifact_sha256:fixture-content"},
	}}

	report, err := ObserveBatch(home, store, batch, "fixtures/observation/content-knowledge-export.json", Options{ApplyKnowledge: true}, tObservation)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || report.DryRun || !report.AppliedKnowledge || report.AppliedProductSignals || report.AppliedSourceObservations {
		t.Fatalf("knowledge apply report = %+v", report)
	}
	if len(report.KnowledgeApplyResults) != 1 {
		t.Fatalf("knowledge apply results = %+v", report.KnowledgeApplyResults)
	}
	result := report.KnowledgeApplyResults[0]
	if result.ObservationID != "obs_content_knowledge" || len(result.EvidenceIDs) != 1 || len(result.CandidateIDs) == 0 || len(result.ExtractionIDs) != 1 || result.Owner != "internal/knowledge" {
		t.Fatalf("knowledge result = %+v", result)
	}
	evidence, err := store.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 1 || evidence[0].SourceKind != "web_capture" || !strings.Contains(evidence[0].QuoteText, "citable evidence") {
		t.Fatalf("knowledge evidence = %+v", evidence)
	}
	var extraction knowledge.ExtractionRequest
	readJSONFile(t, filepath.Join(home, "inbox", "knowledge-extractions", result.ExtractionIDs[0]+".json"), &extraction)
	if extraction.State != knowledge.ExtractionStatePending || extraction.EvidenceRef != result.EvidenceIDs[0] || extraction.SourceKind != "web_capture" {
		t.Fatalf("knowledge extraction = %+v", extraction)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "product-signals.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("knowledge apply must not create product signals ledger: %v", err)
	}
}

func TestApplyRouteEvidenceRecordsJoinedRoutingTransition(t *testing.T) {
	home := t.TempDir()
	batch := validBatch()
	batch.Source = Source{
		Kind:       "route_gateway",
		Ref:        "compound://routes/synthetic-provider-call",
		CapturedAt: tObservation.Format(time.RFC3339),
		Collector:  "fixture-route-edge",
		Privacy:    "synthetic",
	}
	batch.Subject = Subject{
		Kind:    "execution_route",
		ID:      "route:auto/extract",
		Surface: "routing",
		Channel: "model_gateway",
	}
	batch.Observations = []Observation{{
		ID:         "obs_route_evidence",
		Kind:       "route",
		ObservedAt: tObservation.Format(time.RFC3339),
		RawRef:     "provider-ledger-row:fixture-route",
		Confidence: 1,
		Dimensions: map[string]string{
			"request_id":        "req_fixture_observation_route",
			"work_id":           "work_fixture_route",
			"client_profile":    "observation.fixture",
			"protocol":          "openai-compatible",
			"provider":          "fixture-provider",
			"model":             "fixture-model",
			"prompt_tokens":     "120",
			"completion_tokens": "30",
			"total_tokens":      "150",
			"cost_micros":       "2400",
			"latency_ms":        "87",
			"status":            "ok",
		},
		EvidenceRefs: []string{"artifact_sha256:fixture-route"},
	}}
	report, err := ObserveBatch(home, nil, batch, "fixtures/observation/route-evidence.json", Options{ApplyRouteEvidence: true}, tObservation)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || report.DryRun || !report.AppliedRouteEvidence || report.AppliedProductSignals || report.AppliedKnowledge || report.AppliedSourceObservations {
		t.Fatalf("route evidence apply report = %+v", report)
	}
	if len(report.RouteEvidenceApplyResults) != 1 {
		t.Fatalf("route evidence results = %+v", report.RouteEvidenceApplyResults)
	}
	result := report.RouteEvidenceApplyResults[0]
	if result.ObservationID != "obs_route_evidence" || result.RouteEvidenceID == "" || result.TransitionID == "" || result.Owner != "internal/routeevidence" {
		t.Fatalf("route evidence result = %+v", result)
	}
	ev, ok, err := routeevidence.LookupRequestID(home, "req_fixture_observation_route")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || ev.ID != result.RouteEvidenceID || ev.TransitionID != result.TransitionID {
		t.Fatalf("route evidence lookup = %+v ok=%t result=%+v", ev, ok, result)
	}
	if ev.SelectedTarget.Provider != "fixture-provider" || ev.SelectedTarget.Model != "fixture-model" || ev.Usage.TotalTokens != 150 || ev.CostMicros != 2400 || ev.LatencyMS != 87 {
		t.Fatalf("route evidence = %+v", ev)
	}
	events, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if exists, err := events.Exists(result.TransitionID); err != nil || !exists {
		t.Fatalf("route transition missing: exists=%v err=%v", exists, err)
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "product-signals.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("route evidence apply must not create product signals ledger: %v", err)
	}
}

func TestApplyEventsWritesTraceEnvelopeMaterial(t *testing.T) {
	home := t.TempDir()
	batch := validBatch()
	batch.Source = Source{
		Kind:       "event_stream",
		Ref:        "compound://events/synthetic-observation",
		CapturedAt: tObservation.Format(time.RFC3339),
		Collector:  "fixture-event-edge",
		Privacy:    "synthetic",
	}
	batch.Subject = Subject{
		Kind:    "workflow",
		ID:      "workflow:observation-intake",
		Surface: "runtime",
		Channel: "event_stream",
	}
	batch.Observations = []Observation{{
		ID:         "obs_event_trace",
		Kind:       "event",
		ObservedAt: tObservation.Format(time.RFC3339),
		Title:      "Observation event became trace material",
		RawRef:     "event-row:fixture-observation-trace",
		Confidence: 1,
		Dimensions: map[string]string{
			"event_kind":       "workflow.step.completed",
			"event_class":      "workflow",
			"mutation_class":   "local_artifact_write",
			"metadata_quality": "complete",
			"project":          "compound",
			"path":             "state/observation-event-imports/fixture.ndjson",
			"caller":           "fixture-event-edge",
			"run_id":           "run_fixture_event",
		},
		EvidenceRefs: []string{"artifact_sha256:fixture-event"},
	}}
	report, err := ObserveBatch(home, nil, batch, "fixtures/observation/event-trace-envelope.json", Options{ApplyEvents: true}, tObservation)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || report.DryRun || !report.AppliedEvents || report.AppliedProductSignals || report.AppliedKnowledge || report.AppliedRouteEvidence || report.AppliedSourceObservations {
		t.Fatalf("event apply report = %+v", report)
	}
	if len(report.EventApplyResults) != 1 {
		t.Fatalf("event apply results = %+v", report.EventApplyResults)
	}
	result := report.EventApplyResults[0]
	if len(result.ObservationIDs) != 1 || result.ObservationIDs[0] != "obs_event_trace" || len(result.TraceIDs) != 1 || result.SourceAuthorityID == "" || result.Owner != "internal/eventsource" {
		t.Fatalf("event apply result = %+v", result)
	}
	authority, err := eventsource.LoadAuthority(home, 5)
	if err != nil {
		t.Fatal(err)
	}
	if authority.Status != "ok" || authority.TraceCount != 1 || len(authority.SourceAuthority) != 1 || authority.SourceAuthority[0].ID != result.SourceAuthorityID {
		t.Fatalf("event authority = %+v result=%+v", authority, result)
	}
	events, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	trace, err := events.Read(result.TraceIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	traceBody := trace.Bytes
	if !strings.Contains(string(traceBody), "Observation event became trace material") || !strings.Contains(string(traceBody), "artifact_sha256:fixture-event") {
		t.Fatalf("trace body missing observation evidence:\n%s", string(traceBody))
	}
	if _, err := os.Stat(filepath.Join(home, "ledger", "product-signals.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("event apply must not create product signals ledger: %v", err)
	}
}

func TestSourceIntakeProjectionSummarizesRecordedObservationBatches(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := product.Load("../../fixtures/product/market-loop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := product.Apply(home, store, bundle, "fixtures/product/market-loop.yaml", tObservation); err != nil {
		t.Fatal(err)
	}

	fixtures := []struct {
		path string
		opts Options
	}{
		{"../../fixtures/observation/gsc-product-metric.json", Options{ApplyProductSignals: true}},
		{"../../fixtures/observation/content-knowledge-export.json", Options{ApplyKnowledge: true}},
		{"../../fixtures/observation/generated-system-source.json", Options{ApplySourceObservations: true}},
		{"../../fixtures/observation/route-evidence.json", Options{ApplyRouteEvidence: true}},
		{"../../fixtures/observation/event-trace-envelope.json", Options{ApplyEvents: true}},
		{"../../fixtures/observation/social-distribution-feedback.json", Options{ReviewCandidates: true}},
	}
	for _, fixture := range fixtures {
		batch, err := LoadBatch(fixture.path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ObserveBatch(home, store, batch, fixture.path, fixture.opts, tObservation); err != nil {
			t.Fatalf("observe %s: %v", fixture.path, err)
		}
	}

	privateBlocked := validBatch()
	privateBlocked.Source.Privacy = "credential"
	value := 1.0
	privateBlocked.Observations = []Observation{{
		ID:         "obs_private_token",
		Kind:       "metric",
		Metric:     "clicks",
		Value:      &value,
		ObservedAt: tObservation.Format(time.RFC3339),
		Confidence: 1,
	}}
	if _, err := ObserveBatch(home, store, privateBlocked, "fixtures/observation/private-blocked.json", Options{ApplyProductSignals: true}, tObservation); err != nil {
		t.Fatal(err)
	}

	projection, err := LoadSourceIntakeProjection(home, tObservation, 0)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Status != "ok" || projection.RecordCount != 7 || projection.RowCount < 7 {
		t.Fatalf("projection = %+v", projection)
	}
	for _, route := range []string{RouteProductSignal, RouteKnowledgeExport, RouteSourceObservation, RouteRouteEvidence, RouteTraceEnvelope, RouteCandidateOnly, RouteBlockedPrivate} {
		if projection.Counts.ByRoute[route] == 0 {
			t.Fatalf("route %s missing from projection counts: %+v", route, projection.Counts.ByRoute)
		}
	}

	productRow := findIntakeRow(projection.Rows, RouteProductSignal)
	if productRow == nil || productRow.Owner != "internal/product" || productRow.SourceCapturedAt == "" || productRow.SubjectID != "product:focus-notes" || len(productRow.AppliedIDs) == 0 {
		t.Fatalf("product row = %+v", productRow)
	}
	if len(productRow.NextCommand) == 0 || productRow.NextCommand[0] != "compound" {
		t.Fatalf("product row next command = %+v", productRow)
	}
	knowledgeRow := findIntakeRow(projection.Rows, RouteKnowledgeExport)
	if knowledgeRow == nil || knowledgeRow.Owner != "internal/knowledge" || !containsStringPrefix(knowledgeRow.AppliedIDs, "extraction:") {
		t.Fatalf("knowledge row = %+v", knowledgeRow)
	}
	extractionID := strings.TrimPrefix(firstStringPrefix(knowledgeRow.AppliedIDs, "extraction:"), "extraction:")
	extractionCard := findSourceIntakeActionCard(knowledgeRow.ActionCards, "knowledge_extraction_pending")
	if extractionCard == nil ||
		!containsString(extractionCard.Command, "--item") ||
		!containsString(extractionCard.Command, extractionID) ||
		!containsString(extractionCard.Command, "--fixtures") ||
		containsString(extractionCard.Command, "--pending") ||
		!strings.Contains(extractionCard.Boundary, "no automatic memory promotion") {
		t.Fatalf("knowledge extraction action card = %+v", extractionCard)
	}
	var req knowledge.ExtractionRequest
	reqPath := filepath.Join(home, "inbox", "knowledge-extractions", extractionID+".json")
	readJSONFile(t, reqPath, &req)
	req.State = knowledge.ExtractionStateDone
	req.CandidateIDs = []string{"cand_done_fixture"}
	writeJSONFile(t, reqPath, req)
	afterDone, err := LoadSourceIntakeProjection(home, tObservation, 0)
	if err != nil {
		t.Fatal(err)
	}
	doneKnowledgeRow := findIntakeRow(afterDone.Rows, RouteKnowledgeExport)
	if doneKnowledgeRow == nil {
		t.Fatalf("done knowledge row missing: %+v", afterDone.Rows)
	}
	if card := findSourceIntakeActionCard(doneKnowledgeRow.ActionCards, "knowledge_extraction_pending"); card != nil {
		t.Fatalf("done extraction should not keep source-intake card: %+v", card)
	}
	eventRow := findIntakeRow(projection.Rows, RouteTraceEnvelope)
	if eventRow == nil || eventRow.Owner != "internal/eventsource" || len(eventRow.AppliedIDs) == 0 {
		t.Fatalf("event row = %+v", eventRow)
	}
	candidateRow := findIntakeRow(projection.Rows, RouteCandidateOnly)
	if candidateRow == nil || candidateRow.Status != "review_recorded" || len(candidateRow.AppliedIDs) == 0 {
		t.Fatalf("candidate row = %+v", candidateRow)
	}
	privateRow := findIntakeRow(projection.Rows, RouteBlockedPrivate)
	if privateRow == nil || privateRow.Status != "blocked_private_input" || len(privateRow.BlockedRows) == 0 {
		t.Fatalf("private row = %+v", privateRow)
	}
	if projection.Counts.Applied == 0 || projection.Counts.ReviewRecorded == 0 || projection.Counts.BlockedPrivate == 0 {
		t.Fatalf("projection counts = %+v", projection.Counts)
	}
}

func TestObservationReviewQueueResolvesConsumedCandidateVerdict(t *testing.T) {
	home := t.TempDir()
	batch := validBatch()
	batch.Source = Source{
		Kind:       "operating_system_recipe",
		Ref:        "brain://operating-system/solo-builder-os/recipes/oracle",
		CapturedAt: tObservation.Format(time.RFC3339),
		Collector:  "fixture-codex",
		Privacy:    "private",
	}
	batch.Subject = Subject{
		Kind:    "capability",
		ID:      "capability:oracle-second-opinion-evidence-gate",
		Surface: "architecture-review",
		Channel: "oracle-cli",
	}
	batch.Observations = []Observation{{
		ID:         "obs_oracle_second_opinion",
		ObservedAt: tObservation.Format(time.RFC3339),
		Kind:       "distribution_feedback",
		Title:      "Oracle second opinion should remain evidence, not authority",
		Content:    "Oracle output should be evidence/no-promote material, not doctrine authority.",
		Confidence: 0.86,
		EvidenceRefs: []string{
			"brain_recipe:oracle",
			"tool:tools/second_opinion_preflight.ts",
			"detector:oracle-as-authority",
		},
	}}

	report, err := ObserveBatch(home, nil, batch, "runtime/oracle-observation.json", Options{ReviewCandidates: true}, tObservation)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.CandidateReviewResults) != 1 {
		t.Fatalf("candidate review results = %+v", report.CandidateReviewResults)
	}
	candidateID := report.CandidateReviewResults[0].CandidateID
	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: ObservationReviewArea}, tObservation)
	if err != nil {
		t.Fatal(err)
	}
	AnnotateReviewQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.ReviewRequired != 1 || queue.Counts.ActionableCount != 1 {
		t.Fatalf("queue before verdict = %+v", queue.Counts)
	}

	var candidate ObservationCandidateRecord
	if err := candidates.ReadCandidate(home, candidates.CapabilityObs, candidateID, &candidate); err != nil {
		t.Fatal(err)
	}
	type verdictCandidate struct {
		ObservationCandidateRecord
		Verdict string `json:"verdict,omitempty"`
	}
	if err := candidates.WriteCandidate(home, candidates.CapabilityObs, candidateID, verdictCandidate{
		ObservationCandidateRecord: candidate,
		Verdict:                    string(candidates.Park),
	}); err != nil {
		t.Fatal(err)
	}

	queue, err = reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: ObservationReviewArea}, tObservation)
	if err != nil {
		t.Fatal(err)
	}
	AnnotateReviewQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.ReviewRequired != 0 || queue.Counts.ActionableCount != 0 || queue.Counts.OK != 1 {
		t.Fatalf("queue after consumed verdict = %+v", queue.Counts)
	}
	if len(queue.Items) != 1 || queue.Items[0].Status != "resolved" || queue.Items[0].DecisionCard != nil || len(queue.Items[0].Actions) != 0 {
		t.Fatalf("resolved queue item = %+v", queue.Items)
	}
	if !strings.Contains(queue.Items[0].NextAction, "verdict park") {
		t.Fatalf("resolved next action = %q", queue.Items[0].NextAction)
	}
}

func TestSourceIntakeProjectionBuildsActionCardsForRowsNeedingAttention(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := product.Load("../../fixtures/product/market-loop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := product.Apply(home, store, bundle, "fixtures/product/market-loop.yaml", tObservation); err != nil {
		t.Fatal(err)
	}

	value := 1.0
	stale := validBatch()
	stale.Source.CapturedAt = tObservation.Add(-96 * time.Hour).Format(time.RFC3339)
	stale.Subject.ID = "product:focus-notes"
	stale.Observations = []Observation{{
		ID:         "obs_stale_clicks",
		Kind:       "metric",
		Metric:     "clicks",
		Value:      &value,
		ObservedAt: tObservation.Add(-96 * time.Hour).Format(time.RFC3339),
		Confidence: 1,
	}}
	if _, err := ObserveBatch(home, store, stale, "fixtures/observation/stale-product-metric.json", Options{ApplyProductSignals: true}, tObservation); err != nil {
		t.Fatal(err)
	}

	mixed := validBatch()
	mixed.Subject.ID = "product:focus-notes"
	mixed.Observations = []Observation{
		{
			ID:         "obs_apply_clicks",
			Kind:       "metric",
			Metric:     "clicks",
			Value:      &value,
			ObservedAt: tObservation.Format(time.RFC3339),
			Confidence: 1,
		},
		{
			ID:         "obs_skipped_feedback",
			Kind:       "distribution_feedback",
			ObservedAt: tObservation.Format(time.RFC3339),
			Confidence: 0.7,
		},
	}
	if _, err := ObserveBatch(home, store, mixed, "fixtures/observation/mixed-product-feedback.json", Options{ApplyProductSignals: true}, tObservation); err != nil {
		t.Fatal(err)
	}

	privateBlocked := validBatch()
	privateBlocked.Source.Privacy = "credential"
	privateBlocked.Observations = []Observation{{
		ID:         "obs_private_token",
		Kind:       "metric",
		Metric:     "clicks",
		Value:      &value,
		ObservedAt: tObservation.Format(time.RFC3339),
		Confidence: 1,
	}}
	if _, err := ObserveBatch(home, store, privateBlocked, "fixtures/observation/private-blocked.json", Options{ApplyProductSignals: true}, tObservation); err != nil {
		t.Fatal(err)
	}

	unsupported := validBatch()
	unsupported.Observations = []Observation{{
		ID:         "obs_unknown_payload",
		Kind:       "mystery",
		ObservedAt: tObservation.Format(time.RFC3339),
		Confidence: 0.5,
	}}
	if _, err := ObserveBatch(home, store, unsupported, "fixtures/observation/unsupported-kind.json", Options{ApplyProductSignals: true}, tObservation); err != nil {
		t.Fatal(err)
	}

	projection, err := LoadSourceIntakeProjection(home, tObservation, 0)
	if err != nil {
		t.Fatal(err)
	}
	if projection.ActionCardCount < 4 || projection.Counts.ActionRequired < 4 || len(projection.ActionCards) < 4 {
		t.Fatalf("source-intake action cards missing: counts=%+v cards=%+v", projection.Counts, projection.ActionCards)
	}
	for _, kind := range []string{"stale_source", "skipped_route", "blocked_private_input", "unsupported_kind"} {
		card := findSourceIntakeActionCard(projection.ActionCards, kind)
		if card == nil {
			t.Fatalf("missing %s action card: %+v", kind, projection.ActionCards)
		}
		if card.Title == "" || len(card.Command) == 0 || len(card.RequiredInputs) == 0 || len(card.ExpectedValidation) == 0 || len(card.StopConditions) == 0 || card.Boundary == "" || card.Proof == "" {
			t.Fatalf("action card %s incomplete: %+v", kind, card)
		}
	}
	if row := findIntakeRow(projection.Rows, RouteBlockedPrivate); row == nil || len(row.ActionCards) == 0 {
		t.Fatalf("blocked row missing action card: %+v", row)
	}
}

func TestSourceIntakeActionCardBuildsWorkCreatePlanContext(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := product.Load("../../fixtures/product/market-loop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := product.Apply(home, store, bundle, "fixtures/product/market-loop.yaml", tObservation); err != nil {
		t.Fatal(err)
	}
	value := 1.0
	batch := validBatch()
	batch.Subject.ID = "product:focus-notes"
	batch.Observations = []Observation{
		{ID: "obs_apply_clicks", Kind: "metric", Metric: "clicks", Value: &value, ObservedAt: tObservation.Format(time.RFC3339), Confidence: 1},
		{ID: "obs_skipped_feedback", Kind: "distribution_feedback", ObservedAt: tObservation.Format(time.RFC3339), Confidence: 0.7},
	}
	if _, err := ObserveBatch(home, store, batch, "fixtures/observation/mixed-product-feedback.json", Options{ApplyProductSignals: true}, tObservation); err != nil {
		t.Fatal(err)
	}
	projection, err := LoadSourceIntakeProjection(home, tObservation, 0)
	if err != nil {
		t.Fatal(err)
	}
	card := findSourceIntakeActionCard(projection.ActionCards, "skipped_route")
	if card == nil {
		t.Fatalf("missing skipped action card: %+v", projection.ActionCards)
	}
	workStore, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	report, err := BuildSourceIntakeWorkPlan(home, workStore, card.ID, tObservation, SourceIntakeWorkPlanOptions{Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "recorded" || report.WorkPlan.SchemaVersion != work.CreatePlanSchemaVersion || report.WorkPlan.WorkState != "proposed" || report.WorkPlan.PlanPath == "" {
		t.Fatalf("source-intake work plan = %+v", report)
	}
	if _, err := workStore.Get(report.WorkPlan.WorkID); err == nil {
		t.Fatalf("work-plan should not admit work %s", report.WorkPlan.WorkID)
	}
	ctx := report.WorkPlan.ActionContext
	if ctx == nil || ctx.Kind != "source_intake_action_card" || ctx.SourceID != card.ID || !sameStrings(ctx.Command, card.Command) || ctx.Boundary != card.Boundary || ctx.Proof != card.Proof {
		t.Fatalf("work action context does not preserve card: ctx=%+v card=%+v", ctx, card)
	}
	if len(ctx.RequiredInputs) == 0 || len(ctx.ExpectedValidation) == 0 || len(ctx.StopConditions) == 0 {
		t.Fatalf("work action context missing card gates: %+v", ctx)
	}
	if !containsString(report.SourceIntake.EvidenceRefs, "source_intake_card:"+card.ID) || !containsString(report.SourceIntake.NonGoals, "do not execute the source-intake action command while creating or admitting this work-plan") {
		t.Fatalf("work draft missing evidence/non-goal: %+v", report.SourceIntake)
	}
}

func validBatch() Batch {
	return Batch{
		SchemaVersion: BatchSchemaVersion,
		Source: Source{
			Kind:       "google_search_console",
			Ref:        "site:https://example.invalid",
			CapturedAt: tObservation.Format(time.RFC3339),
			Collector:  "fixture-source-edge",
			Privacy:    "synthetic",
		},
		Subject: Subject{
			Kind:    "product",
			ID:      "product:fixture-search",
			Surface: "search",
			Channel: "organic_search",
		},
	}
}

func findIntakeRow(rows []SourceIntakeRow, route string) *SourceIntakeRow {
	for i := range rows {
		if rows[i].Route == route {
			return &rows[i]
		}
	}
	return nil
}

func findSourceIntakeActionCard(cards []SourceIntakeActionCard, kind string) *SourceIntakeActionCard {
	for i := range cards {
		if cards[i].Kind == kind {
			return &cards[i]
		}
	}
	return nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsStringPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func firstStringPrefix(values []string, prefix string) string {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return value
		}
	}
	return ""
}

func signalCount(model product.LoopModel, metric string) int {
	if len(model.Products) == 0 {
		return 0
	}
	return len(model.Products[0].SignalsByMetric[metric])
}

func readJSONFile(t *testing.T, path string, dest any) {
	t.Helper()
	var data []byte
	if filepath.Base(filepath.Dir(path)) == "events" && strings.HasSuffix(path, ".json") {
		store, err := eventstore.Open(filepath.Dir(filepath.Dir(path)))
		if err != nil {
			t.Fatal(err)
		}
		record, err := store.Read(strings.TrimSuffix(filepath.Base(path), ".json"))
		if err != nil {
			t.Fatal(err)
		}
		data = record.Bytes
	} else {
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := json.Unmarshal(data, dest); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", " ")
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
