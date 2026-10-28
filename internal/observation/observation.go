// Package observation owns the generic external observation intake seam.
//
// Observation batches are a public grammar for rented source edges. They route
// into existing meaning owners; they do not create provider-specific authority.
package observation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/eventsource"
	"compound/internal/knowledge"
	"compound/internal/mem"
	"compound/internal/product"
	"compound/internal/routeevidence"
	"compound/internal/sourceobs"
)

const (
	BatchSchemaVersion = "compound.observation_batch.v0.1"

	RouteProductSignal     = "product_signal"
	RouteKnowledgeExport   = "knowledge_export"
	RouteTraceEnvelope     = "trace_envelope"
	RouteRouteEvidence     = "route_evidence"
	RouteSourceObservation = "source_observation"
	RouteCandidateOnly     = "candidate_only"
	RouteUnsupportedKind   = "unsupported_kind"
	RouteBlockedPrivate    = "blocked_private_input"

	maxObservationDimensions = 32
	maxDimensionKeyLen       = 80
	maxDimensionValueLen     = 512
	maxObservationIDLen      = 160
	maxObservationKindLen    = 80
	maxObservationMetricLen  = 160
	maxObservationTitleLen   = 512
	maxObservationContentLen = 64 * 1024
	maxObservationURLLen     = 2048
	maxObservationRefLen     = 2048
	maxEvidenceRefs          = 32
	maxEvidenceRefLen        = 256
)

// Batch is the parent grammar for external payloads that Compound may route
// into existing owners.
type Batch struct {
	SchemaVersion string        `json:"schema_version"`
	Source        Source        `json:"source"`
	Subject       Subject       `json:"subject"`
	Observations  []Observation `json:"observations"`
}

type Source struct {
	Kind       string `json:"kind"`
	Ref        string `json:"ref"`
	CapturedAt string `json:"captured_at"`
	Collector  string `json:"collector,omitempty"`
	Privacy    string `json:"privacy,omitempty"`
}

type Subject struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Surface string `json:"surface,omitempty"`
	Channel string `json:"channel,omitempty"`
}

type Observation struct {
	ID           string            `json:"id,omitempty"`
	ObservedAt   string            `json:"observed_at"`
	Kind         string            `json:"kind"`
	Metric       string            `json:"metric,omitempty"`
	Value        *float64          `json:"value,omitempty"`
	Window       Window            `json:"window,omitempty"`
	Dimensions   map[string]string `json:"dimensions,omitempty"`
	RawRef       string            `json:"raw_ref,omitempty"`
	Confidence   float64           `json:"confidence,omitempty"`
	Title        string            `json:"title,omitempty"`
	Content      string            `json:"content,omitempty"`
	URL          string            `json:"url,omitempty"`
	EvidenceRefs []string          `json:"evidence_refs,omitempty"`
}

type Window struct {
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`
}

type Options struct {
	ApplyEvents             bool
	ApplyRouteEvidence      bool
	ApplyProductSignals     bool
	ReviewCandidates        bool
	ApplySourceObservations bool
	ApplyKnowledge          bool
}

type Report struct {
	GeneratedAt                   string                         `json:"generated_at,omitempty"`
	IntakeRecordID                string                         `json:"intake_record_id,omitempty"`
	IntakeRecordPath              string                         `json:"intake_record_path,omitempty"`
	IntakeProjectionCommand       []string                       `json:"intake_projection_command,omitempty"`
	AppliedEvents                 bool                           `json:"applied_events"`
	EventApplyResults             []EventApplyResult             `json:"event_apply_results,omitempty"`
	AppliedRouteEvidence          bool                           `json:"applied_route_evidence"`
	RouteEvidenceApplyResults     []RouteEvidenceApplyResult     `json:"route_evidence_apply_results,omitempty"`
	AppliedKnowledge              bool                           `json:"applied_knowledge"`
	KnowledgeApplyResults         []KnowledgeApplyResult         `json:"knowledge_apply_results,omitempty"`
	SchemaVersion                 string                         `json:"schema_version"`
	Status                        string                         `json:"status"`
	SourcePath                    string                         `json:"source_path,omitempty"`
	DryRun                        bool                           `json:"dry_run"`
	AppliedProductSignals         bool                           `json:"applied_product_signals"`
	ReviewCandidates              bool                           `json:"review_candidates"`
	AppliedSourceObservations     bool                           `json:"applied_source_observations"`
	Source                        Source                         `json:"source"`
	Subject                       Subject                        `json:"subject"`
	RouteCounts                   map[string]int                 `json:"route_counts"`
	Decisions                     []RouteDecision                `json:"decisions"`
	ProductApplyResults           []ProductApplyResult           `json:"product_apply_results,omitempty"`
	CandidateReviewResults        []CandidateReviewResult        `json:"candidate_review_results,omitempty"`
	SourceObservationApplyResults []SourceObservationApplyResult `json:"source_observation_apply_results,omitempty"`
	SkippedApply                  []string                       `json:"skipped_apply,omitempty"`
	SkippedReview                 []string                       `json:"skipped_review,omitempty"`
}

type RouteDecision struct {
	ObservationID   string   `json:"observation_id"`
	ObservationKind string   `json:"observation_kind"`
	ObservedAt      string   `json:"observed_at,omitempty"`
	Route           string   `json:"route"`
	Owner           string   `json:"owner"`
	Reason          string   `json:"reason"`
	Command         []string `json:"command,omitempty"`
	EvidenceRefs    []string `json:"evidence_refs,omitempty"`
}

type ProductApplyResult struct {
	ObservationID string   `json:"observation_id"`
	SignalIDs     []string `json:"signal_ids,omitempty"`
	EvidenceIDs   []string `json:"evidence_ids,omitempty"`
	TransitionIDs []string `json:"transition_ids,omitempty"`
}

type SourceObservationApplyResult struct {
	ObservationID       string   `json:"observation_id"`
	SourceObservationID string   `json:"source_observation_id"`
	CandidateIDs        []string `json:"candidate_ids,omitempty"`
	NoPromoteIDs        []string `json:"no_promote_ids,omitempty"`
	Owner               string   `json:"owner"`
	NextCommand         []string `json:"next_command,omitempty"`
}

type KnowledgeApplyResult struct {
	ObservationID string   `json:"observation_id"`
	EvidenceIDs   []string `json:"evidence_ids,omitempty"`
	CandidateIDs  []string `json:"candidate_ids,omitempty"`
	ExtractionIDs []string `json:"extraction_ids,omitempty"`
	Owner         string   `json:"owner"`
	NextCommand   []string `json:"next_command,omitempty"`
}

type RouteEvidenceApplyResult struct {
	ObservationID   string   `json:"observation_id"`
	RouteEvidenceID string   `json:"route_evidence_id"`
	TransitionID    string   `json:"transition_id"`
	Owner           string   `json:"owner"`
	NextCommand     []string `json:"next_command,omitempty"`
}

type EventApplyResult struct {
	ObservationIDs    []string `json:"observation_ids,omitempty"`
	TraceIDs          []string `json:"trace_ids,omitempty"`
	SourceAuthorityID string   `json:"source_authority_id,omitempty"`
	AuthorityRef      string   `json:"authority_ref,omitempty"`
	Imported          int      `json:"imported"`
	Skipped           int      `json:"skipped"`
	Owner             string   `json:"owner"`
	NextCommand       []string `json:"next_command,omitempty"`
}

func LoadBatch(path string) (Batch, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Batch{}, fmt.Errorf("read observation batch %s: %w", path, err)
	}
	var batch Batch
	if err := json.Unmarshal(b, &batch); err != nil {
		return Batch{}, fmt.Errorf("parse observation batch %s: %w", path, err)
	}
	return batch, nil
}

func ObserveBatch(homePath string, store *mem.Store, batch Batch, sourcePath string, opts Options, now time.Time) (report Report, err error) {
	if err := ValidateBatch(batch); err != nil {
		return Report{SchemaVersion: BatchSchemaVersion, Status: "fail", SourcePath: sourcePath}, err
	}
	report = Plan(batch, sourcePath)
	report.GeneratedAt = now.UTC().Format(time.RFC3339)
	defer func() {
		if err != nil || !shouldRecordSourceIntake(opts) || strings.TrimSpace(homePath) == "" {
			return
		}
		record, recordErr := RecordSourceIntakeReport(homePath, report, now)
		if recordErr != nil {
			err = recordErr
			return
		}
		report.IntakeRecordID = record.ID
		report.IntakeRecordPath = record.Path
		report.IntakeProjectionCommand = []string{"compound", "source", "intake", "--json"}
	}()
	if opts.ReviewCandidates {
		if strings.TrimSpace(homePath) == "" {
			return report, fmt.Errorf("COMPOUND_HOME required recording observation candidate reviews")
		}
		report.DryRun = false
		report.ReviewCandidates = true
		for _, decision := range report.Decisions {
			if decision.Route != RouteCandidateOnly {
				report.SkippedReview = append(report.SkippedReview, decision.ObservationID+":"+decision.Route)
				continue
			}
			obs, ok := findObservation(batch, decision.ObservationID)
			if !ok {
				return report, fmt.Errorf("observation %q missing during review planning", decision.ObservationID)
			}
			result, err := RecordCandidateReviewPlan(homePath, batch, obs, decision, sourcePath, now)
			if err != nil {
				return report, err
			}
			report.CandidateReviewResults = append(report.CandidateReviewResults, result)
		}
		return report, nil
	}
	if opts.ApplyRouteEvidence {
		if strings.TrimSpace(homePath) == "" {
			return report, fmt.Errorf("COMPOUND_HOME required applying route evidence")
		}
		report.DryRun = false
		report.AppliedRouteEvidence = true
		for _, decision := range report.Decisions {
			if decision.Route != RouteRouteEvidence {
				report.SkippedApply = append(report.SkippedApply, decision.ObservationID+":"+decision.Route)
				continue
			}
			obs, ok := findObservation(batch, decision.ObservationID)
			if !ok {
				return report, fmt.Errorf("observation %q missing during route evidence apply", decision.ObservationID)
			}
			ev, err := RouteEvidenceDocument(batch, obs, sourcePath, now)
			if err != nil {
				return report, err
			}
			applied, err := routeevidence.RecordJoined(homePath, ev)
			if err != nil {
				return report, err
			}
			report.RouteEvidenceApplyResults = append(report.RouteEvidenceApplyResults, RouteEvidenceApplyResult{
				ObservationID:   decision.ObservationID,
				RouteEvidenceID: applied.Evidence.ID,
				TransitionID:    applied.Transition.ID,
				Owner:           "internal/routeevidence",
				NextCommand:     []string{"compound", "route", "evidence-authority", "--json"},
			})
		}
		return report, nil
	}
	if opts.ApplyEvents {
		if strings.TrimSpace(homePath) == "" {
			return report, fmt.Errorf("COMPOUND_HOME required applying event observations")
		}
		report.DryRun = false
		report.AppliedEvents = true
		var rows []map[string]any
		var observationIDs []string
		for _, decision := range report.Decisions {
			if decision.Route != RouteTraceEnvelope {
				report.SkippedApply = append(report.SkippedApply, decision.ObservationID+":"+decision.Route)
				continue
			}
			obs, ok := findObservation(batch, decision.ObservationID)
			if !ok {
				return report, fmt.Errorf("observation %q missing during event apply", decision.ObservationID)
			}
			rows = append(rows, EventSourceRow(batch, obs, sourcePath))
			observationIDs = append(observationIDs, decision.ObservationID)
		}
		if len(rows) == 0 {
			return report, nil
		}
		streamPath, err := writeEventSourceRows(homePath, batch, sourcePath, rows)
		if err != nil {
			return report, err
		}
		applied, err := eventsource.ImportOSEvents(homePath, streamPath, eventsource.ImportOptions{
			Stream:    "observation-batch-events",
			SourceRef: firstNonEmpty(batch.Source.Ref, sourcePath),
		}, now)
		if err != nil {
			return report, err
		}
		report.EventApplyResults = append(report.EventApplyResults, EventApplyResult{
			ObservationIDs:    observationIDs,
			TraceIDs:          applied.TraceIDs,
			SourceAuthorityID: applied.SourceAuthorityID,
			AuthorityRef:      applied.AuthorityRef,
			Imported:          applied.Imported,
			Skipped:           applied.Skipped,
			Owner:             "internal/eventsource",
			NextCommand:       []string{"compound", "event", "authority", "--json"},
		})
		return report, nil
	}
	if opts.ApplySourceObservations {
		if strings.TrimSpace(homePath) == "" {
			return report, fmt.Errorf("COMPOUND_HOME required applying source observations")
		}
		report.DryRun = false
		report.AppliedSourceObservations = true
		for _, decision := range report.Decisions {
			if decision.Route != RouteSourceObservation {
				report.SkippedApply = append(report.SkippedApply, decision.ObservationID+":"+decision.Route)
				continue
			}
			obs, ok := findObservation(batch, decision.ObservationID)
			if !ok {
				return report, fmt.Errorf("observation %q missing during source observation apply", decision.ObservationID)
			}
			sourceObservation, err := SourceObservationDocument(batch, obs, sourcePath)
			if err != nil {
				return report, err
			}
			applied, err := sourceobs.Apply(homePath, sourceObservation, now)
			if err != nil {
				return report, err
			}
			report.SourceObservationApplyResults = append(report.SourceObservationApplyResults, SourceObservationApplyResult{
				ObservationID:       decision.ObservationID,
				SourceObservationID: applied.ObservationID,
				CandidateIDs:        applied.CandidateIDs,
				NoPromoteIDs:        applied.NoPromoteIDs,
				Owner:               "internal/sourceobs",
				NextCommand:         []string{"compound", "orient", "--operator", "--json"},
			})
		}
		return report, nil
	}
	if !opts.ApplyProductSignals {
		if opts.ApplyKnowledge {
			if strings.TrimSpace(homePath) == "" || store == nil {
				return report, fmt.Errorf("home path memory store required applying knowledge")
			}
			report.DryRun = false
			report.AppliedKnowledge = true
			for _, decision := range report.Decisions {
				if decision.Route != RouteKnowledgeExport {
					report.SkippedApply = append(report.SkippedApply, decision.ObservationID+":"+decision.Route)
					continue
				}
				obs, ok := findObservation(batch, decision.ObservationID)
				if !ok {
					return report, fmt.Errorf("observation %q missing during knowledge apply", decision.ObservationID)
				}
				doc, err := KnowledgeExportDocument(batch, obs, sourcePath)
				if err != nil {
					return report, err
				}
				applied, err := knowledge.Import(homePath, store, doc, "", now)
				if err != nil {
					return report, err
				}
				report.KnowledgeApplyResults = append(report.KnowledgeApplyResults, KnowledgeApplyResult{
					ObservationID: decision.ObservationID,
					EvidenceIDs:   applied.EvidenceIDs,
					CandidateIDs:  applied.CandidateIDs,
					ExtractionIDs: applied.ExtractionIDs,
					Owner:         "internal/knowledge",
					NextCommand:   []string{"compound", "knowledge", "extract", "--pending", "--json"},
				})
			}
			return report, nil
		}
		return report, nil
	}
	if strings.TrimSpace(homePath) == "" || store == nil {
		return report, fmt.Errorf("home path and memory store required when applying product signals")
	}
	report.DryRun = false
	report.AppliedProductSignals = true
	for _, decision := range report.Decisions {
		if decision.Route != RouteProductSignal {
			report.SkippedApply = append(report.SkippedApply, decision.ObservationID+":"+decision.Route)
			continue
		}
		obs, ok := findObservation(batch, decision.ObservationID)
		if !ok {
			return report, fmt.Errorf("observation %q missing during apply", decision.ObservationID)
		}
		doc, err := ProductSignalDocument(batch, obs, sourcePath)
		if err != nil {
			return report, err
		}
		applied, err := product.ApplySignal(homePath, store, doc, sourcePath, now)
		if err != nil {
			return report, err
		}
		report.ProductApplyResults = append(report.ProductApplyResults, ProductApplyResult{
			ObservationID: decision.ObservationID,
			SignalIDs:     applied.SignalIDs,
			EvidenceIDs:   applied.EvidenceIDs,
			TransitionIDs: applied.TransitionIDs,
		})
	}
	return report, nil
}

func Plan(batch Batch, sourcePath string) Report {
	report := Report{
		SchemaVersion: BatchSchemaVersion,
		Status:        "ok",
		SourcePath:    sourcePath,
		DryRun:        true,
		Source:        batch.Source,
		Subject:       batch.Subject,
		RouteCounts:   map[string]int{},
	}
	for _, obs := range batch.Observations {
		decision := Route(batch, obs)
		decision.ObservedAt = obs.ObservedAt
		report.Decisions = append(report.Decisions, decision)
		report.RouteCounts[decision.Route]++
	}
	return report
}

func Route(batch Batch, obs Observation) RouteDecision {
	obsID := observationID(batch, obs)
	evidenceRefs := observationEvidenceRefs(batch, obs)
	privateBlockReason := privateBlockedReason(batch, obs)
	if privateBlockReason != "" {
		return RouteDecision{
			ObservationID:   obsID,
			ObservationKind: obs.Kind,
			Route:           RouteBlockedPrivate,
			Owner:           "operator",
			Reason:          privateBlockReason,
			EvidenceRefs:    evidenceRefs,
		}
	}
	switch normalize(obs.Kind) {
	case "metric":
		if normalize(batch.Subject.Kind) == "product" && strings.TrimSpace(batch.Subject.ID) != "" && strings.TrimSpace(obs.Metric) != "" && obs.Value != nil {
			return RouteDecision{
				ObservationID:   obsID,
				ObservationKind: obs.Kind,
				Route:           RouteProductSignal,
				Owner:           "internal/product",
				Reason:          "product metric observation routes to existing ProductSignal owner",
				Command:         []string{"compound", "source", "observe-batch", "<batch.json>", "--apply-product-signals", "--json"},
				EvidenceRefs:    evidenceRefs,
			}
		}
		return unsupported(obsID, obs, "metric observations need product subject id, metric, and value")
	case "content":
		return RouteDecision{
			ObservationID:   obsID,
			ObservationKind: obs.Kind,
			Route:           RouteKnowledgeExport,
			Owner:           "internal/knowledge",
			Reason:          "content observation routes to KnowledgeExport/extraction",
			Command:         []string{"compound", "source", "observe-batch", "<batch.json>", "--apply-knowledge", "--json"},
			EvidenceRefs:    evidenceRefs,
		}
	case "event":
		return RouteDecision{
			ObservationID:   obsID,
			ObservationKind: obs.Kind,
			Route:           RouteTraceEnvelope,
			Owner:           "internal/eventsource",
			Reason:          "event observation routes to TraceEnvelope/EventSource",
			Command:         []string{"compound", "source", "observe-batch", "<batch.json>", "--apply-events", "--json"},
			EvidenceRefs:    evidenceRefs,
		}
	case "route":
		return RouteDecision{
			ObservationID:   obsID,
			ObservationKind: obs.Kind,
			Route:           RouteRouteEvidence,
			Owner:           "internal/routeevidence",
			Reason:          "route observation routes to ExecutionRouteEvidence",
			Command:         []string{"compound", "source", "observe-batch", "<batch.json>", "--apply-route-evidence", "--json"},
			EvidenceRefs:    evidenceRefs,
		}
	case "tool_health", "generated_view", "system":
		return RouteDecision{
			ObservationID:   obsID,
			ObservationKind: obs.Kind,
			Route:           RouteSourceObservation,
			Owner:           "internal/sourceobs",
			Reason:          "system/generated observation routes to SourceObservation",
			Command:         []string{"compound", "source", "observe-batch", "<batch.json>", "--apply-source-observations", "--json"},
			EvidenceRefs:    evidenceRefs,
		}
	case "distribution_feedback", "social_feedback", "support_message", "message", "comment":
		return route(obsID, obs, RouteCandidateOnly, "candidate/review", "feedback/message observation is parked as candidate/review material until adjudicated", evidenceRefs)
	default:
		return unsupported(obsID, obs, "observation kind has no route in compound.observation_batch.v0.1")
	}
}

func ValidateBatch(batch Batch) error {
	var missing []string
	if batch.SchemaVersion != BatchSchemaVersion {
		return fmt.Errorf("schema_version must be %s, got %q", BatchSchemaVersion, batch.SchemaVersion)
	}
	if strings.TrimSpace(batch.Source.Kind) == "" {
		missing = append(missing, "source.kind")
	}
	if strings.TrimSpace(batch.Source.Ref) == "" {
		missing = append(missing, "source.ref")
	}
	if strings.TrimSpace(batch.Source.CapturedAt) == "" {
		missing = append(missing, "source.captured_at")
	}
	if strings.TrimSpace(batch.Subject.Kind) == "" {
		missing = append(missing, "subject.kind")
	}
	if len(batch.Observations) == 0 {
		missing = append(missing, "observations")
	}
	seenIDs := map[string]bool{}
	for i, obs := range batch.Observations {
		prefix := fmt.Sprintf("observations[%d]", i)
		if strings.TrimSpace(obs.Kind) == "" {
			missing = append(missing, prefix+".kind")
		}
		if strings.TrimSpace(obs.ObservedAt) == "" {
			missing = append(missing, prefix+".observed_at")
		}
		if obs.Confidence < 0 || obs.Confidence > 1 {
			return fmt.Errorf("%s.confidence must be between 0 and 1", prefix)
		}
		obsID := observationID(batch, obs)
		if seenIDs[obsID] {
			return fmt.Errorf("%s duplicate observation id %q", prefix, obsID)
		}
		seenIDs[obsID] = true
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing %s", strings.Join(missing, ", "))
	}
	if err := validateSourceFields(batch); err != nil {
		return err
	}
	if err := validateSubjectFields(batch); err != nil {
		return err
	}
	for i, obs := range batch.Observations {
		if err := validateObservationFields(batch, obs, fmt.Sprintf("observations[%d]", i)); err != nil {
			return err
		}
	}
	if _, err := time.Parse(time.RFC3339, batch.Source.CapturedAt); err != nil {
		return fmt.Errorf("source.captured_at must be RFC3339: %w", err)
	}
	for i, obs := range batch.Observations {
		if _, err := time.Parse(time.RFC3339, obs.ObservedAt); err != nil {
			return fmt.Errorf("observations[%d].observed_at must be RFC3339: %w", i, err)
		}
	}
	return nil
}

func validateSourceFields(batch Batch) error {
	checks := []struct {
		field string
		value string
		max   int
		scan  bool
	}{
		{"source.kind", batch.Source.Kind, maxObservationKindLen, true},
		{"source.collector", batch.Source.Collector, maxObservationRefLen, true},
		{"source.privacy", batch.Source.Privacy, maxDimensionValueLen, false},
	}
	for _, check := range checks {
		if err := validateStringValue(check.field, check.value, check.max, check.scan); err != nil {
			return err
		}
	}
	return validateRefValue("source.ref", batch.Source.Ref, batch)
}

func validateSubjectFields(batch Batch) error {
	checks := []struct {
		field string
		value string
		max   int
	}{
		{"subject.kind", batch.Subject.Kind, maxObservationKindLen},
		{"subject.id", batch.Subject.ID, maxObservationRefLen},
		{"subject.surface", batch.Subject.Surface, maxObservationRefLen},
		{"subject.channel", batch.Subject.Channel, maxObservationRefLen},
	}
	for _, check := range checks {
		if err := validateStringValue(check.field, check.value, check.max, true); err != nil {
			return err
		}
	}
	return nil
}

func validateObservationFields(batch Batch, obs Observation, prefix string) error {
	if err := validateStringValue(prefix+".id", obs.ID, maxObservationIDLen, false); err != nil {
		return err
	}
	if err := validateStringValue(prefix+".kind", obs.Kind, maxObservationKindLen, true); err != nil {
		return err
	}
	if err := validateStringValue(prefix+".metric", obs.Metric, maxObservationMetricLen, true); err != nil {
		return err
	}
	if err := validateStringValue(prefix+".title", obs.Title, maxObservationTitleLen, true); err != nil {
		return err
	}
	if err := validateStringValue(prefix+".content", obs.Content, maxObservationContentLen, true); err != nil {
		return err
	}
	if err := validateURLValue(prefix+".url", obs.URL); err != nil {
		return err
	}
	if err := validateRefValue(prefix+".raw_ref", obs.RawRef, batch); err != nil {
		return err
	}
	if err := validateDimensions(prefix+".dimensions", obs.Dimensions); err != nil {
		return err
	}
	return validateEvidenceRefs(prefix+".evidence_refs", obs.EvidenceRefs, batch)
}

func validateStringValue(field, value string, max int, scanSecret bool) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if len(value) > max {
		return fmt.Errorf("%s exceeds %d bytes", field, max)
	}
	if scanSecret && secretLikeValue(value) {
		return fmt.Errorf("%s contains secret-like material", field)
	}
	return nil
}

func validateURLValue(field, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if err := validateStringValue(field, value, maxObservationURLLen, true); err != nil {
		return err
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("%s must be an absolute http(s) URL", field)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("%s unsupported URL scheme %q", field, parsed.Scheme)
	}
	for key, values := range parsed.Query() {
		if secretLikeKey(key) {
			return fmt.Errorf("%s contains secret-like query key %q", field, key)
		}
		for _, v := range values {
			if secretLikeValue(v) {
				return fmt.Errorf("%s contains secret-like query value for %q", field, key)
			}
		}
	}
	return nil
}

func validateRefValue(field, value string, batch Batch) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if err := validateStringValue(field, value, maxObservationRefLen, true); err != nil {
		return err
	}
	if localPathLike(value) && !privateRefsAllowed(batch) {
		return fmt.Errorf("%s absolute local path requires explicit private source privacy", field)
	}
	scheme := refScheme(value)
	if scheme == "" {
		return nil
	}
	if !allowedObservationRefScheme(scheme) {
		return fmt.Errorf("%s unsupported ref scheme %q", field, scheme)
	}
	if scheme == "http" || scheme == "https" {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" {
			return fmt.Errorf("%s must be an absolute http(s) ref", field)
		}
	}
	return nil
}

func validateDimensions(field string, dimensions map[string]string) error {
	if len(dimensions) > maxObservationDimensions {
		return fmt.Errorf("%s has too many dimensions: %d > %d", field, len(dimensions), maxObservationDimensions)
	}
	for key, value := range dimensions {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("%s contains empty key", field)
		}
		if len(key) > maxDimensionKeyLen {
			return fmt.Errorf("%s key %q exceeds %d bytes", field, key, maxDimensionKeyLen)
		}
		if secretLikeKey(key) {
			return fmt.Errorf("%s contains secret-like dimension key %q", field, key)
		}
		if err := validateStringValue(field+"."+key, value, maxDimensionValueLen, true); err != nil {
			return err
		}
	}
	return nil
}

func validateEvidenceRefs(field string, refs []string, batch Batch) error {
	if len(refs) > maxEvidenceRefs {
		return fmt.Errorf("%s has too many evidence_refs: %d > %d", field, len(refs), maxEvidenceRefs)
	}
	for i, ref := range refs {
		if strings.TrimSpace(ref) == "" {
			return fmt.Errorf("%s[%d] is empty", field, i)
		}
		if len(ref) > maxEvidenceRefLen {
			return fmt.Errorf("%s[%d] exceeds %d bytes", field, i, maxEvidenceRefLen)
		}
		if secretLikeValue(ref) {
			return fmt.Errorf("%s[%d] contains secret-like material", field, i)
		}
		if localPathLike(ref) && !privateRefsAllowed(batch) {
			return fmt.Errorf("%s[%d] absolute local path requires explicit private source privacy", field, i)
		}
	}
	return nil
}

func privateRefsAllowed(batch Batch) bool {
	switch normalize(batch.Source.Privacy) {
	case "private", "operator_private", "internal", "credential", "credentials", "secret", "secrets":
		return true
	default:
		return false
	}
}

func localPathLike(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if filepath.IsAbs(value) || strings.HasPrefix(value, "~/") {
		return true
	}
	if len(value) >= 3 && value[1] == ':' && (value[2] == '\\' || value[2] == '/') {
		return true
	}
	return false
}

func refScheme(value string) string {
	i := strings.Index(value, ":")
	if i <= 0 || i > 40 {
		return ""
	}
	candidate := value[:i]
	for _, r := range candidate {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '+' || r == '-' || r == '.' {
			continue
		}
		return ""
	}
	return strings.ToLower(candidate)
}

func allowedObservationRefScheme(scheme string) bool {
	switch scheme {
	case "http", "https", "compound", "site", "app-store", "channel", "inbox", "brain",
		"gsc-row", "asc-row", "web-row", "event-row", "provider-ledger-row",
		"social-reply", "support-message", "fixture", "artifact_sha256", "route_evidence",
		"trace", "receipt", "transition", "product_signal", "source_observation":
		return true
	default:
		return false
	}
}

func secretLikeKey(value string) bool {
	switch normalize(value) {
	case "api_key", "apikey", "access_token", "auth_token", "refresh_token", "id_token",
		"client_secret", "secret", "password", "passwd", "credential", "credentials",
		"authorization", "private_key":
		return true
	default:
		return false
	}
}

func secretLikeValue(value string) bool {
	n := strings.ToLower(strings.TrimSpace(value))
	if n == "" {
		return false
	}
	markers := []string{
		"-----begin private key-----",
		"password=",
		"password:",
		"passwd=",
		"api_key=",
		"api-key=",
		"access_token=",
		"auth_token=",
		"client_secret=",
		"secret=",
		"credential=",
		"authorization: bearer ",
		"bearer sk-",
		"sk-",
		"ghp_",
		"github_pat_",
		"xoxb-",
	}
	for _, marker := range markers {
		if strings.Contains(n, marker) {
			return true
		}
	}
	return strings.Contains(value, "AKIA")
}

func ProductSignalDocument(batch Batch, obs Observation, sourcePath string) (product.SignalDocument, error) {
	if obs.Value == nil {
		return product.SignalDocument{}, fmt.Errorf("observation %q missing metric value", observationID(batch, obs))
	}
	period := observationWindow(obs)
	confidence := obs.Confidence
	if confidence == 0 {
		confidence = 1
	}
	evidenceRefs := observationEvidenceRefs(batch, obs)
	export := product.SignalExportDocument{
		SchemaVersion: product.SignalExportSchemaVersion,
		Source: product.SignalSource{
			Kind: strings.TrimSpace(batch.Source.Kind),
			Ref:  firstNonEmpty(obs.RawRef, batch.Source.Ref, filepath.Base(sourcePath)),
		},
		SignalID:    stableID("sig_", batch.Subject.ID, batch.Source.Kind, batch.Source.Ref, obs.Metric, period, observationID(batch, obs)),
		ProductID:   strings.TrimSpace(batch.Subject.ID),
		Surface:     firstNonEmpty(batch.Subject.Surface, batch.Subject.Channel, "external"),
		ChangeEvent: firstNonEmpty(obs.Title, "External observation "+strings.TrimSpace(obs.Metric)+" from "+strings.TrimSpace(batch.Source.Kind)),
		Metric: product.ExportMetric{
			Name: strings.TrimSpace(obs.Metric),
			Before: product.ExportMetricPoint{
				Value:  *obs.Value,
				Period: period,
				Ref:    firstNonEmpty(obs.RawRef, observationID(batch, obs)+":before"),
			},
			After: product.ExportMetricPoint{
				Value:  *obs.Value,
				Period: period,
				Ref:    firstNonEmpty(obs.RawRef, observationID(batch, obs)+":after"),
			},
		},
		Window:       period,
		Confidence:   confidence,
		EvidenceRefs: evidenceRefs,
		ObservedAt:   obs.ObservedAt,
	}
	return product.AdaptSignalExport(export)
}

func SourceObservationDocument(batch Batch, obs Observation, sourcePath string) (sourceobs.SourceObservation, error) {
	decision := Route(batch, obs)
	if decision.Route != RouteSourceObservation {
		return sourceobs.SourceObservation{}, fmt.Errorf("observation %q routes to %s, not %s", decision.ObservationID, decision.Route, RouteSourceObservation)
	}
	revision := sourceObservationRevision(batch, obs)
	obsKind := normalize(obs.Kind)
	entityHint := sourceObservationEntityHint(batch, obs, decision.ObservationID)
	return sourceobs.SourceObservation{
		SchemaVersion: sourceobs.SchemaVersion,
		ID:            stableID("srcobs_", "observation_batch", batch.Source.Kind, batch.Source.Ref, decision.ObservationID, revision),
		AdapterID:     "compound.observation-batch",
		SourceSystem:  firstNonEmpty(batch.Source.Kind, "observation_batch"),
		ObservedAt:    obs.ObservedAt,
		SourceRef: sourceobs.SourceRef{
			Path:        firstNonEmpty(obs.RawRef, batch.Source.Ref, sourcePath, decision.ObservationID),
			Kind:        firstNonEmpty(obsKind, "system"),
			Owner:       firstNonEmpty(batch.Subject.ID, batch.Subject.Kind, batch.Source.Kind),
			SourceClass: firstNonEmpty(normalize(batch.Subject.Kind), obsKind, "source_observation"),
			Revision:    revision,
		},
		GeneratedView: sourceobs.GeneratedView{
			ViewKind:    firstNonEmpty(obsKind, "system"),
			Consumer:    "compound source observe-batch --apply-source-observations",
			Generator:   firstNonEmpty(batch.Source.Collector, "observation_batch"),
			Marker:      BatchSchemaVersion,
			Freshness:   "observed",
			DriftReason: sourceObservationDetail(batch, obs),
		},
		MapsTo: []sourceobs.MapTarget{{
			ObjectFamily:   sourceObservationObjectFamily(batch, obs),
			CandidateKind:  string(sourceObservationCandidateKind(batch, obs)),
			EntityHint:     entityHint,
			PromoteAllowed: false,
		}},
		EvidenceRefs: observationEvidenceRefs(batch, obs),
	}, nil
}

func KnowledgeExportDocument(batch Batch, obs Observation, sourcePath string) (knowledge.Export, error) {
	obsID := observationID(batch, obs)
	itemRef := firstNonEmpty(obs.RawRef, obsID)
	content := strings.TrimSpace(obs.Content)
	if content == "" {
		content = strings.TrimSpace(obs.Title)
	}
	return knowledge.Export{
		SchemaVersion: knowledge.ExportSchemaVersion,
		Source: knowledge.Source{
			Kind:       firstNonEmpty(batch.Source.Kind, "observation_batch"),
			Ref:        firstNonEmpty(batch.Source.Ref, sourcePath, obsID),
			CapturedAt: batch.Source.CapturedAt,
		},
		Privacy: knowledgePrivacy(batch.Source.Privacy),
		Producer: knowledge.Producer{
			Kind: "observation_batch",
			Name: firstNonEmpty(batch.Source.Collector, "compound source observe-batch"),
		},
		Items: []knowledge.Item{{
			Ref:         itemRef,
			URL:         observationURL(batch, obs),
			Title:       strings.TrimSpace(obs.Title),
			PublishedAt: obs.ObservedAt,
			Topics:      knowledgeTopics(batch, obs),
			Content:     content,
		}},
	}, nil
}

func RouteEvidenceDocument(batch Batch, obs Observation, sourcePath string, now time.Time) (routeevidence.Evidence, error) {
	observedAt, err := time.Parse(time.RFC3339, obs.ObservedAt)
	if err != nil {
		return routeevidence.Evidence{}, fmt.Errorf("route observation %q observed_at invalid: %w", observationID(batch, obs), err)
	}
	if observedAt.IsZero() {
		observedAt = now
	}
	dims := obs.Dimensions
	promptTokens, err := dimensionInt(dims, "prompt_tokens", "input_tokens")
	if err != nil {
		return routeevidence.Evidence{}, err
	}
	completionTokens, err := dimensionInt(dims, "completion_tokens", "output_tokens")
	if err != nil {
		return routeevidence.Evidence{}, err
	}
	totalTokens, err := dimensionInt(dims, "total_tokens")
	if err != nil {
		return routeevidence.Evidence{}, err
	}
	if totalTokens == 0 {
		totalTokens = promptTokens + completionTokens
	}
	costMicros, err := dimensionInt64(dims, "cost_micros")
	if err != nil {
		return routeevidence.Evidence{}, err
	}
	if costMicros == 0 && normalize(obs.Metric) == "cost_micros" && obs.Value != nil {
		costMicros = int64(*obs.Value)
	}
	latencyMS, err := dimensionInt64(dims, "latency_ms")
	if err != nil {
		return routeevidence.Evidence{}, err
	}
	return routeevidence.Build(routeevidence.BuildInput{
		CreatedAt:     observedAt,
		RequestID:     firstNonEmpty(dims["request_id"], obs.RawRef, observationID(batch, obs)),
		WorkID:        dims["work_id"],
		ClientProfile: firstNonEmpty(dims["client_profile"], batch.Source.Collector, batch.Source.Kind, "observation_batch"),
		Protocol:      firstNonEmpty(dims["protocol"], dims["transport"], "observation_batch"),
		Provider:      firstNonEmpty(dims["provider"], batch.Source.Kind, "unknown-provider"),
		Model:         firstNonEmpty(dims["model"], dims["route_alias"], "unknown-model"),
		Usage: routeevidence.Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      totalTokens,
		},
		CostMicros:   costMicros,
		LatencyMS:    latencyMS,
		Status:       firstNonEmpty(dims["status"], "observed"),
		EvidenceRefs: append(observationEvidenceRefs(batch, obs), "observation_batch_source:"+sourcePath),
	})
}

func findObservation(batch Batch, id string) (Observation, bool) {
	for _, obs := range batch.Observations {
		if observationID(batch, obs) == id || strings.TrimSpace(obs.ID) == id {
			return obs, true
		}
	}
	return Observation{}, false
}

func knowledgePrivacy(value string) string {
	if normalize(value) == "private" {
		return "private"
	}
	return "public"
}

func observationURL(batch Batch, obs Observation) string {
	url := strings.TrimSpace(obs.URL)
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return url
	}
	ref := strings.TrimSpace(batch.Source.Ref)
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	return ""
}

func knowledgeTopics(batch Batch, obs Observation) []string {
	topics := compact([]string{
		"subject_kind:" + normalize(batch.Subject.Kind),
		"subject_id:" + strings.TrimSpace(batch.Subject.ID),
		"surface:" + normalize(batch.Subject.Surface),
		"channel:" + normalize(batch.Subject.Channel),
		"observation_kind:" + normalize(obs.Kind),
	})
	for key, value := range obs.Dimensions {
		key = normalize(key)
		value = normalize(value)
		if key == "" || value == "" {
			continue
		}
		topics = append(topics, key+":"+value)
	}
	sort.Strings(topics)
	return compact(topics)
}

func EventSourceRow(batch Batch, obs Observation, sourcePath string) map[string]any {
	dims := obs.Dimensions
	obsID := observationID(batch, obs)
	evidenceRows := make([]map[string]any, 0)
	for _, ref := range observationEvidenceRefs(batch, obs) {
		evidenceRows = append(evidenceRows, map[string]any{
			"kind":     "ref",
			"value":    ref,
			"redacted": false,
		})
	}
	return map[string]any{
		"schema_version":   1,
		"id":               stableID("evt_", batch.Source.Kind, batch.Source.Ref, batch.Subject.Kind, batch.Subject.ID, obsID),
		"ts":               obs.ObservedAt,
		"kind":             firstNonEmpty(dims["event_kind"], obs.Metric, "observation.event"),
		"event_class":      firstNonEmpty(dims["event_class"], batch.Subject.Kind, "observation"),
		"metadata_quality": firstNonEmpty(dims["metadata_quality"], "partial"),
		"mutation_class":   firstNonEmpty(dims["mutation_class"], dims["mutation"], "read_only"),
		"producer": map[string]any{
			"caller":  firstNonEmpty(dims["caller"], batch.Source.Collector, batch.Source.Kind, "observation_batch"),
			"agent":   firstNonEmpty(dims["agent"], "observation_batch"),
			"run_id":  dims["run_id"],
			"version": BatchSchemaVersion,
		},
		"subject": map[string]any{
			"project":      firstNonEmpty(dims["project"], batch.Subject.ID, batch.Subject.Kind),
			"path":         firstNonEmpty(dims["path"], obs.RawRef, batch.Subject.Surface, batch.Subject.Channel, obsID),
			"surface":      batch.Subject.Surface,
			"channel":      batch.Subject.Channel,
			"subject_kind": batch.Subject.Kind,
			"subject_id":   batch.Subject.ID,
		},
		"evidence": evidenceRows,
		"summary":  firstNonEmpty(obs.Title, obs.Content, "Observation event "+obsID),
		"details": map[string]any{
			"observation_id": obsID,
			"source_kind":    batch.Source.Kind,
			"source_ref":     batch.Source.Ref,
			"source_path":    sourcePath,
			"confidence":     obs.Confidence,
			"dimensions":     dims,
		},
	}
}

func writeEventSourceRows(homePath string, batch Batch, sourcePath string, rows []map[string]any) (string, error) {
	dir := filepath.Join(homePath, "state", "observation-event-imports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, stableID("obsev_", batch.Source.Kind, batch.Source.Ref, batch.Subject.Kind, batch.Subject.ID, sourcePath)+".ndjson")
	var b strings.Builder
	for _, row := range rows {
		data, err := json.Marshal(row)
		if err != nil {
			return "", err
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func dimensionInt(dimensions map[string]string, keys ...string) (int, error) {
	value := firstDimension(dimensions, keys...)
	if value == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("dimension %s must be integer: %w", strings.Join(keys, "/"), err)
	}
	if n < 0 {
		return 0, fmt.Errorf("dimension %s must be non-negative", strings.Join(keys, "/"))
	}
	return n, nil
}

func dimensionInt64(dimensions map[string]string, keys ...string) (int64, error) {
	value := firstDimension(dimensions, keys...)
	if value == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("dimension %s must be integer: %w", strings.Join(keys, "/"), err)
	}
	if n < 0 {
		return 0, fmt.Errorf("dimension %s must be non-negative", strings.Join(keys, "/"))
	}
	return n, nil
}

func firstDimension(dimensions map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(dimensions[key]); value != "" {
			return value
		}
	}
	return ""
}

func route(obsID string, obs Observation, routeName, owner, reason string, evidenceRefs []string) RouteDecision {
	return RouteDecision{
		ObservationID:   obsID,
		ObservationKind: obs.Kind,
		Route:           routeName,
		Owner:           owner,
		Reason:          reason,
		EvidenceRefs:    evidenceRefs,
	}
}

func unsupported(obsID string, obs Observation, reason string) RouteDecision {
	return RouteDecision{
		ObservationID:   obsID,
		ObservationKind: obs.Kind,
		Route:           RouteUnsupportedKind,
		Owner:           "",
		Reason:          reason,
		EvidenceRefs:    compact(obs.EvidenceRefs),
	}
}

func privateBlockedReason(batch Batch, obs Observation) string {
	for _, value := range []string{batch.Source.Privacy, obs.Kind, obs.RawRef, obs.Content} {
		n := normalize(value)
		if strings.Contains(n, "credential") || strings.Contains(n, "secret") || strings.Contains(n, "token") {
			return "observation appears to carry credential/secret material; route blocked for explicit private handling"
		}
	}
	return ""
}

func observationID(batch Batch, obs Observation) string {
	if id := strings.TrimSpace(obs.ID); id != "" {
		return id
	}
	return stableID("obs_", batch.Source.Kind, batch.Source.Ref, batch.Subject.Kind, batch.Subject.ID, obs.Kind, obs.Metric, obs.ObservedAt, obs.RawRef)
}

func observationEvidenceRefs(batch Batch, obs Observation) []string {
	refs := append([]string{}, obs.EvidenceRefs...)
	refs = append(refs, compact([]string{
		"observation_batch:" + strings.TrimSpace(batch.Source.Kind) + ":" + strings.TrimSpace(batch.Source.Ref),
		"observation:" + observationID(batch, obs),
		obs.RawRef,
	})...)
	sort.Strings(refs)
	return compact(refs)
}

func sourceObservationRevision(batch Batch, obs Observation) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		batch.Source.Kind,
		batch.Source.Ref,
		batch.Subject.Kind,
		batch.Subject.ID,
		observationID(batch, obs),
		obs.Kind,
		obs.ObservedAt,
		obs.RawRef,
		obs.Title,
		obs.Content,
		strings.Join(obs.EvidenceRefs, "|"),
	}, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

func sourceObservationCandidateKind(batch Batch, obs Observation) candidates.Kind {
	subjectKind := normalize(batch.Subject.Kind)
	observationKind := normalize(obs.Kind)
	switch {
	case observationKind == "tool_health":
		return candidates.CapabilityObs
	case subjectKind == "capability" || subjectKind == "tool" || subjectKind == "harness":
		return candidates.CapabilityObs
	default:
		return candidates.MethodUpgrade
	}
}

func sourceObservationObjectFamily(batch Batch, obs Observation) string {
	if subjectKind := normalize(batch.Subject.Kind); subjectKind != "" {
		return subjectKind
	}
	if observationKind := normalize(obs.Kind); observationKind != "" {
		return observationKind
	}
	return "source"
}

func sourceObservationEntityHint(batch Batch, obs Observation, obsID string) string {
	return firstNonEmpty(batch.Subject.ID, obs.Title, obs.RawRef, batch.Source.Ref, obsID)
}

func sourceObservationDetail(batch Batch, obs Observation) string {
	var parts []string
	if title := strings.TrimSpace(obs.Title); title != "" {
		parts = append(parts, "title="+title)
	}
	if content := strings.TrimSpace(obs.Content); content != "" {
		parts = append(parts, "content="+content)
	}
	if len(obs.Dimensions) > 0 {
		keys := make([]string, 0, len(obs.Dimensions))
		for key := range obs.Dimensions {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var dims []string
		for _, key := range keys {
			dims = append(dims, key+"="+strings.TrimSpace(obs.Dimensions[key]))
		}
		parts = append(parts, "dimensions="+strings.Join(dims, ","))
	}
	if len(parts) == 0 {
		return "source_observation:" + firstNonEmpty(normalize(obs.Kind), "system") + " subject=" + firstNonEmpty(batch.Subject.ID, batch.Subject.Kind, "unknown")
	}
	return strings.Join(parts, "; ")
}

func observationWindow(obs Observation) string {
	if obs.Window.Start != "" || obs.Window.End != "" {
		return strings.Trim(strings.TrimSpace(obs.Window.Start)+".."+strings.TrimSpace(obs.Window.End), ".")
	}
	return strings.TrimSpace(obs.ObservedAt)
}

func stableID(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return prefix + hex.EncodeToString(sum[:])[:12]
}

func normalize(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	prevUnderscore := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevUnderscore = false
			continue
		}
		if !prevUnderscore {
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func compact(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
