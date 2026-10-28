// Package knowledge imports captured source exports into evidence plus proposed
// memory candidates. It is intentionally medium-agnostic: source.kind is data,
// not a core enum.
package knowledge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/distiller"
	"compound/internal/mem"
	"compound/internal/sourceobs"
)

const (
	ExportSchemaVersion            = "compound.knowledge_export.v0.1"
	ExtractionRequestSchemaVersion = "compound.knowledge_extraction_request.v0.1"
	maxEvidenceQuoteChars          = 4000
	ExtractionStatePending         = "pending"
	ExtractionStateDone            = "done"
	ExtractionStateError           = "error"
)

type Export struct {
	SchemaVersion string   `json:"schema_version"`
	Source        Source   `json:"source"`
	Privacy       string   `json:"privacy,omitempty"`
	Producer      Producer `json:"producer,omitempty"`
	Items         []Item   `json:"items"`
}

type Source struct {
	Kind       string `json:"kind"`
	Ref        string `json:"ref"`
	CapturedAt string `json:"captured_at"`
}

type Producer struct {
	Kind    string `json:"kind,omitempty"`
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

type Item struct {
	Ref              string   `json:"ref"`
	URL              string   `json:"url,omitempty"`
	Title            string   `json:"title,omitempty"`
	Authors          []string `json:"authors,omitempty"`
	AuthorHandle     string   `json:"author_handle,omitempty"`
	PublishedAt      string   `json:"published_at,omitempty"`
	OccurredAt       string   `json:"occurred_at,omitempty"`
	BookmarkedAt     string   `json:"bookmarked_at,omitempty"`
	CapturedAt       string   `json:"captured_at,omitempty"`
	Topics           []string `json:"topics,omitempty"`
	MediaRefs        []string `json:"media_refs,omitempty"`
	OperatorNote     string   `json:"operator_note,omitempty"`
	CandidateClasses []string `json:"candidate_classes,omitempty"`
	Content          string   `json:"content,omitempty"`
	ContentRef       string   `json:"content_ref,omitempty"`
	Lang             string   `json:"lang,omitempty"`
}

type ImportReport struct {
	Items                int      `json:"items"`
	Imported             int      `json:"imported"`
	Echoes               int      `json:"echoes"`
	Reused               int      `json:"reused"`
	EvidenceIDs          []string `json:"evidence_ids"`
	CandidateIDs         []string `json:"candidate_ids"`
	ExtractionIDs        []string `json:"extraction_ids"`
	SourceObservationIDs []string `json:"source_observation_ids,omitempty"`
	SourceCandidateIDs   []string `json:"source_candidate_ids,omitempty"`
	NoPromoteIDs         []string `json:"no_promote_ids,omitempty"`
}

type ExtractionRequest struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             string                   `json:"id"`
	ItemRef        string                   `json:"item_ref"`
	EvidenceRef    string                   `json:"evidence_ref"`
	SourceKind     string                   `json:"source_kind"`
	Title          string                   `json:"title,omitempty"`
	ContentDigest  string                   `json:"content_digest"`
	State          string                   `json:"state"`
	CreatedAt      string                   `json:"created_at"`
	CompletedAt    string                   `json:"completed_at,omitempty"`
	CandidateIDs   []string                 `json:"candidate_ids,omitempty"`
	EssenceLines   []EssenceLine            `json:"essence_lines,omitempty"`
	EssenceResidue []string                 `json:"essence_residue,omitempty"`
	RouteEvidence  *ExtractionRouteEvidence `json:"route_evidence,omitempty"`
	Error          string                   `json:"error,omitempty"`
}

type EssenceLine struct {
	PredicateHint string `json:"predicate_hint,omitempty"`
	Statement     string `json:"statement"`
	Quote         string `json:"quote"`
}

type ExtractionRouteEvidence struct {
	Provider                string                `json:"provider"`
	Model                   string                `json:"model,omitempty"`
	Called                  bool                  `json:"called"`
	Usage                   distiller.OpenAIUsage `json:"usage,omitempty"`
	LatencyMS               int64                 `json:"latency_ms,omitempty"`
	LiveModelCalls          int                   `json:"live_model_calls,omitempty"`
	EssenceLines            int                   `json:"essence_lines,omitempty"`
	ShapedCandidates        int                   `json:"shaped_candidates,omitempty"`
	ResidueLines            int                   `json:"residue_lines,omitempty"`
	HallucinationRejections int                   `json:"hallucination_rejections,omitempty"`
	Fixture                 string                `json:"fixture,omitempty"`
}

type ExtractionModelEvidenceRow struct {
	ID                      string   `json:"id"`
	ExtractionID            string   `json:"extraction_id"`
	ItemRef                 string   `json:"item_ref"`
	EvidenceRef             string   `json:"evidence_ref"`
	Provider                string   `json:"provider"`
	Model                   string   `json:"model,omitempty"`
	ExtractedBy             string   `json:"extracted_by"`
	EssenceLines            int      `json:"essence_lines"`
	ShapedCandidates        int      `json:"shaped_candidates"`
	ResidueLines            int      `json:"residue_lines"`
	HallucinationRejections int      `json:"hallucination_rejections"`
	CandidateIDs            []string `json:"candidate_ids,omitempty"`
	ObservedAt              string   `json:"observed_at"`
}

func (r ExtractionRequest) Validate() error {
	var missing []string
	if strings.TrimSpace(r.SchemaVersion) == "" {
		missing = append(missing, "schema_version")
	} else if r.SchemaVersion != ExtractionRequestSchemaVersion {
		return fmt.Errorf("schema_version must be %q, got %q", ExtractionRequestSchemaVersion, r.SchemaVersion)
	}
	if strings.TrimSpace(r.ID) == "" {
		missing = append(missing, "id")
	}
	if strings.TrimSpace(r.ItemRef) == "" {
		missing = append(missing, "item_ref")
	}
	if strings.TrimSpace(r.EvidenceRef) == "" {
		missing = append(missing, "evidence_ref")
	}
	if strings.TrimSpace(r.SourceKind) == "" {
		missing = append(missing, "source_kind")
	}
	if strings.TrimSpace(r.ContentDigest) == "" {
		missing = append(missing, "content_digest")
	}
	if strings.TrimSpace(r.CreatedAt) == "" {
		missing = append(missing, "created_at")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing %s", strings.Join(missing, ", "))
	}
	if _, err := time.Parse(time.RFC3339, r.CreatedAt); err != nil {
		return fmt.Errorf("created_at must be RFC3339: %w", err)
	}
	if strings.TrimSpace(r.CompletedAt) != "" {
		if _, err := time.Parse(time.RFC3339, r.CompletedAt); err != nil {
			return fmt.Errorf("completed_at must be RFC3339: %w", err)
		}
	}
	if !oneOf(r.State, []string{ExtractionStatePending, ExtractionStateDone, ExtractionStateError}) {
		return fmt.Errorf("state must be one of [%s %s %s], got %q", ExtractionStatePending, ExtractionStateDone, ExtractionStateError, r.State)
	}
	return nil
}

type ExtractOptions struct {
	Pending bool
	ItemID  string
	// Limit bounds a --pending batch: 0 or absent = unlimited; a negative value
	// is a deterministic usage error (nothing processed). It caps the number of
	// in-scope pending items attempted this run, so an idle-lane dispatcher can
	// burn a large queue in bounded, receipted batches.
	Limit int
	// SourceKind, when non-empty, restricts a --pending batch to requests whose
	// source_kind matches exactly. Out-of-scope requests are invisible to the
	// batch (neither processed nor skipped), like the item path's non-match.
	SourceKind          string
	FixturesDir         string
	OpenAICompatibleURL string
	Model               string
	APIKeyEnv           string
	EvalCorpusDir       string
	EvalRoot            string
}

type ExtractReport struct {
	Processed     int      `json:"processed"`
	Created       int      `json:"created"`
	Skipped       int      `json:"skipped"`
	Failed        int      `json:"failed"`
	CandidateIDs  []string `json:"candidate_ids"`
	ExtractionIDs []string `json:"extraction_ids"`
}

type DistributionEvent struct {
	CanonicalRef     string `json:"canonical_ref"`
	EchoSourceKind   string `json:"echo_source_kind"`
	EchoSourceRef    string `json:"echo_source_ref"`
	FirstEvidenceRef string `json:"first_evidence_ref"`
	ObservedAt       string `json:"observed_at"`
}

type SourceRegistryRow struct {
	SourceID       string  `json:"source_id"`
	Kind           string  `json:"kind"`
	Ref            string  `json:"ref"`
	FirstSeen      string  `json:"first_seen"`
	TrustPosterior float64 `json:"trust_posterior"`
}

type ImportIndexRow struct {
	CanonicalRef string `json:"canonical_ref"`
	EvidenceRef  string `json:"evidence_ref"`
	ImportedAt   string `json:"imported_at"`
	ExportDigest string `json:"export_digest"`
	SourceKind   string `json:"source_kind"`
	SourceRef    string `json:"source_ref"`
}

func LoadExport(path string) (Export, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Export{}, nil, err
	}
	export, err := DecodeExport(data)
	if err != nil {
		return Export{}, nil, fmt.Errorf("parse knowledge export %s: %w", path, err)
	}
	return export, data, nil
}

func DecodeExport(data []byte) (Export, error) {
	var export Export
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&export); err != nil {
		return Export{}, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return Export{}, fmt.Errorf("trailing JSON")
		}
		return Export{}, fmt.Errorf("trailing JSON: %w", err)
	}
	if err := export.Validate(); err != nil {
		return Export{}, err
	}
	return export, nil
}

func (e *Export) Validate() error {
	if strings.TrimSpace(e.SchemaVersion) == "" {
		return fmt.Errorf("schema_version required")
	}
	if e.SchemaVersion != ExportSchemaVersion {
		return fmt.Errorf("schema_version must be %s, got %q", ExportSchemaVersion, e.SchemaVersion)
	}
	e.Source.Kind = strings.TrimSpace(e.Source.Kind)
	e.Source.Ref = strings.TrimSpace(e.Source.Ref)
	e.Source.CapturedAt = strings.TrimSpace(e.Source.CapturedAt)
	if e.Source.Kind == "" {
		return fmt.Errorf("source.kind required")
	}
	if e.Source.Ref == "" {
		return fmt.Errorf("source.ref required")
	}
	if e.Source.CapturedAt == "" {
		return fmt.Errorf("source.captured_at required")
	}
	if _, err := time.Parse(time.RFC3339, e.Source.CapturedAt); err != nil {
		return fmt.Errorf("source.captured_at must be RFC3339: %w", err)
	}
	e.Privacy = strings.TrimSpace(e.Privacy)
	if e.Privacy == "" {
		e.Privacy = defaultPrivacy(e.Source.Kind)
	}
	if e.Privacy != "public" && e.Privacy != "private" {
		return fmt.Errorf("privacy must be public or private, got %q", e.Privacy)
	}
	if len(e.Items) == 0 {
		return fmt.Errorf("items nonempty")
	}
	for i := range e.Items {
		if err := e.Items[i].validate(i); err != nil {
			return err
		}
	}
	return nil
}

func (i *Item) validate(index int) error {
	i.Ref = strings.TrimSpace(i.Ref)
	i.URL = strings.TrimSpace(i.URL)
	i.Title = strings.TrimSpace(i.Title)
	i.AuthorHandle = strings.TrimSpace(i.AuthorHandle)
	i.PublishedAt = strings.TrimSpace(i.PublishedAt)
	i.OccurredAt = strings.TrimSpace(i.OccurredAt)
	i.BookmarkedAt = strings.TrimSpace(i.BookmarkedAt)
	i.CapturedAt = strings.TrimSpace(i.CapturedAt)
	i.Content = strings.TrimSpace(i.Content)
	i.ContentRef = strings.TrimSpace(i.ContentRef)
	i.Lang = strings.TrimSpace(i.Lang)
	i.OperatorNote = strings.TrimSpace(i.OperatorNote)
	if i.Ref == "" {
		return fmt.Errorf("items[%d].ref required", index)
	}
	if i.Content == "" && i.ContentRef == "" {
		return fmt.Errorf("items[%d] needs content or content_ref", index)
	}
	i.Authors = cleanStrings(i.Authors)
	i.Topics = cleanStrings(i.Topics)
	i.MediaRefs = cleanStrings(i.MediaRefs)
	i.CandidateClasses = normalizeCandidateClasses(i.CandidateClasses)
	for _, check := range []struct {
		field string
		value string
	}{
		{"occurred_at", i.OccurredAt},
		{"bookmarked_at", i.BookmarkedAt},
		{"captured_at", i.CapturedAt},
	} {
		if check.value == "" {
			continue
		}
		if _, err := time.Parse(time.RFC3339, check.value); err != nil {
			return fmt.Errorf("items[%d].%s must be RFC3339: %w", index, check.field, err)
		}
	}
	return nil
}

func ImportFile(homePath string, store *mem.Store, path string, now time.Time) (ImportReport, error) {
	export, data, err := LoadExport(path)
	if err != nil {
		return ImportReport{}, err
	}
	return Import(homePath, store, export, digestBytes(data), now)
}

func Import(homePath string, store *mem.Store, export Export, exportDigest string, now time.Time) (ImportReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return ImportReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if store == nil {
		return ImportReport{}, fmt.Errorf("memory store required")
	}
	if exportDigest == "" {
		exportDigest = digestStruct(export)
	}
	if err := export.Validate(); err != nil {
		return ImportReport{}, err
	}
	if err := ensureSourceRegistry(homePath, export.Source, now); err != nil {
		return ImportReport{}, err
	}

	imports, err := loadImportIndex(homePath)
	if err != nil {
		return ImportReport{}, err
	}
	report := ImportReport{Items: len(export.Items), EvidenceIDs: []string{}, CandidateIDs: []string{}, ExtractionIDs: []string{}}
	for _, item := range export.Items {
		canonical := canonicalRef(item)
		if prior, ok := findExactImport(imports, canonical, exportDigest); ok {
			report.Reused++
			report.EvidenceIDs = append(report.EvidenceIDs, prior.EvidenceRef)
			applied, err := applySourceObservation(homePath, export, item, prior.EvidenceRef, now)
			if err != nil {
				return report, err
			}
			addSourceObservationResult(&report, applied)
			continue
		}

		prior, hasRecent := findRecentImport(imports, canonical, now, 7*24*time.Hour)
		evidence, err := store.RecordEvidence(mem.EvidenceItem{
			SourceKind: export.Source.Kind,
			SourceID:   item.Ref,
			QuoteText:  boundedQuote(item.Content),
			EventTime:  itemEventTime(item),
			Actor:      "tool",
			Visibility: evidenceVisibility(export.Privacy),
		}, now)
		if err != nil {
			return report, err
		}
		report.EvidenceIDs = append(report.EvidenceIDs, evidence.ID)
		row := ImportIndexRow{
			CanonicalRef: canonical,
			EvidenceRef:  evidence.ID,
			ImportedAt:   rfc3339(now),
			ExportDigest: exportDigest,
			SourceKind:   export.Source.Kind,
			SourceRef:    export.Source.Ref,
		}

		if hasRecent {
			if err := appendDistributionEvent(homePath, DistributionEvent{
				CanonicalRef:     canonical,
				EchoSourceKind:   export.Source.Kind,
				EchoSourceRef:    item.Ref,
				FirstEvidenceRef: prior.EvidenceRef,
				ObservedAt:       rfc3339(now),
			}); err != nil {
				return report, err
			}
			if err := appendImportRow(homePath, row); err != nil {
				return report, err
			}
			imports = append(imports, row)
			report.Echoes++
			continue
		}

		applied, err := applySourceObservation(homePath, export, item, evidence.ID, now)
		if err != nil {
			return report, err
		}
		addSourceObservationResult(&report, applied)

		candidates := candidatesForItem(export, item, evidence.ID, now)
		for _, candidate := range candidates {
			if _, err := store.RecordStableCandidate(candidate); err != nil {
				return report, err
			}
			report.CandidateIDs = append(report.CandidateIDs, candidate.ID)
		}
		extraction, err := writeExtractionRequest(homePath, export, item, evidence.ID, now)
		if err != nil {
			return report, err
		}
		if err := appendImportRow(homePath, row); err != nil {
			return report, err
		}
		imports = append(imports, row)
		report.Imported++
		report.ExtractionIDs = append(report.ExtractionIDs, extraction.ID)
	}
	return report, nil
}

func Extract(homePath string, store *mem.Store, opts ExtractOptions, now time.Time) (ExtractReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return ExtractReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if store == nil {
		return ExtractReport{}, fmt.Errorf("memory store required")
	}
	if opts.Limit < 0 {
		// Deterministic usage error before any work: a bounded batch cannot have
		// a negative bound.
		return ExtractReport{}, fmt.Errorf("--limit must be non-negative, got %d", opts.Limit)
	}
	if !opts.Pending && strings.TrimSpace(opts.ItemID) == "" {
		return ExtractReport{}, fmt.Errorf("use --pending or --item")
	}
	if strings.TrimSpace(opts.FixturesDir) == "" && (strings.TrimSpace(opts.OpenAICompatibleURL) == "" || strings.TrimSpace(opts.Model) == "" || strings.TrimSpace(opts.EvalCorpusDir) == "") {
		return ExtractReport{}, fmt.Errorf("use --fixtures or --openai-compatible-url with --model and --eval-corpus")
	}
	requests, err := loadExtractionRequests(homePath)
	if err != nil {
		return ExtractReport{}, err
	}
	// Deterministic selection order (created_at, then id) so concurrent bounded
	// batch runners pick the same items in the same order instead of shuffling
	// with directory-read order — otherwise `--limit` selects a nondeterministic
	// slice across runners.
	sortExtractionRequests(requests)
	report := ExtractReport{CandidateIDs: []string{}, ExtractionIDs: []string{}}
	attempted := 0
	for _, req := range requests {
		// Source-kind filter restricts a --pending batch's working set to one
		// exact kind; out-of-scope requests are invisible to this batch (neither
		// processed nor skipped), mirroring the item path's non-match continue.
		if opts.Pending && strings.TrimSpace(opts.SourceKind) != "" && req.SourceKind != opts.SourceKind {
			continue
		}
		if !opts.Pending && !matchesExtractionItem(req, opts.ItemID) {
			continue
		}
		if opts.Pending && req.State != ExtractionStatePending {
			report.Skipped++
			continue
		}
		if req.State == ExtractionStateDone && len(req.CandidateIDs) > 0 {
			report.Skipped++
			report.CandidateIDs = append(report.CandidateIDs, req.CandidateIDs...)
			report.ExtractionIDs = append(report.ExtractionIDs, req.ID)
			continue
		}
		// Limit bounds the batch: stop once we have attempted Limit in-scope
		// items this run (a failed attempt still consumes the bound so a poison
		// item cannot make a bounded batch process an unbounded tail).
		if opts.Limit > 0 && attempted >= opts.Limit {
			break
		}
		attempted++
		candidateIDs, route, err := extractOne(homePath, store, &req, opts, now)
		if err != nil {
			// Fail soft per item: one bad model response must not strand the
			// remaining pending items (same lesson as panel apply). Failed
			// items keep state=error and stay retryable via --item.
			req.State = ExtractionStateError
			req.Error = err.Error()
			_ = writeExtractionRequestFile(homePath, req)
			report.Failed++
			continue
		}
		req.State = ExtractionStateDone
		req.CompletedAt = rfc3339(now)
		req.CandidateIDs = candidateIDs
		req.RouteEvidence = &route
		req.Error = ""
		if err := writeExtractionRequestFile(homePath, req); err != nil {
			return report, err
		}
		report.Processed++
		report.Created += len(candidateIDs)
		report.CandidateIDs = append(report.CandidateIDs, candidateIDs...)
		report.ExtractionIDs = append(report.ExtractionIDs, req.ID)
	}
	if !opts.Pending && strings.TrimSpace(opts.ItemID) != "" && report.Processed == 0 && len(report.ExtractionIDs) == 0 && report.Failed == 0 {
		return report, fmt.Errorf("knowledge extraction item %q not found", opts.ItemID)
	}
	if report.Failed > 0 {
		return report, fmt.Errorf("knowledge extraction completed with %d failure(s); failed items keep state=error and are retryable via --item", report.Failed)
	}
	return report, nil
}

func extractOne(homePath string, store *mem.Store, req *ExtractionRequest, opts ExtractOptions, now time.Time) ([]string, ExtractionRouteEvidence, error) {
	evidence, ok, err := store.GetEvidence(req.EvidenceRef)
	if err != nil {
		return nil, ExtractionRouteEvidence{}, err
	}
	if !ok {
		return nil, ExtractionRouteEvidence{}, fmt.Errorf("evidence %s not found", req.EvidenceRef)
	}
	llmReq := buildKnowledgeExtractionRequest(*req, evidence)
	var raw []byte
	route := ExtractionRouteEvidence{}
	if strings.TrimSpace(opts.FixturesDir) != "" {
		fixture := filepath.Join(opts.FixturesDir, fixtureExtractionName(*req)+".json")
		raw, err = os.ReadFile(fixture)
		if err != nil {
			return nil, route, err
		}
		route = ExtractionRouteEvidence{Provider: "fixture", Called: false, Fixture: fixture}
	} else {
		raw, route, err = callKnowledgeEssenceOpenAICompatible(*req, evidence, opts, now)
		if err != nil {
			return nil, route, err
		}
	}
	raw, err = normalizeKnowledgeExtractionRaw(req, evidence, llmReq, raw, &route)
	if err != nil {
		return nil, route, err
	}
	trace, err := distiller.NewGroundedLLMExtractor(llmReq).Extract(raw, distiller.DefaultPolicy())
	if err != nil {
		return nil, route, err
	}
	if len(trace.Candidates) == 0 {
		return nil, route, fmt.Errorf("knowledge extraction produced zero candidates")
	}
	var candidateIDs []string
	for _, dc := range trace.Candidates {
		if dc.Kind != distiller.KindMemory {
			return nil, route, fmt.Errorf("knowledge extraction only accepts memory candidates, got %q", dc.Kind)
		}
		c := memoryCandidateFromExtraction(*req, dc, evidence, route, now)
		if _, err := store.RecordStableCandidate(c); err != nil {
			return nil, route, err
		}
		candidateIDs = append(candidateIDs, c.ID)
	}
	if err := appendExtractionModelEvidence(homePath, *req, route, candidateIDs, now); err != nil {
		return nil, route, err
	}
	return candidateIDs, route, nil
}

func callKnowledgeEssenceOpenAICompatible(req ExtractionRequest, evidence mem.EvidenceItem, opts ExtractOptions, now time.Time) ([]byte, ExtractionRouteEvidence, error) {
	route := ExtractionRouteEvidence{
		Provider: "openai-compatible",
		Model:    strings.TrimSpace(opts.Model),
	}
	if strings.TrimSpace(opts.EvalCorpusDir) == "" {
		return nil, route, fmt.Errorf("eval corpus required before provider extraction")
	}
	if now.IsZero() {
		now = time.Now()
	}
	eval, err := distiller.EvaluateCorpusDirInRoot(opts.EvalCorpusDir, opts.EvalRoot, now)
	if err != nil {
		return nil, route, err
	}
	if eval.Status != "pass" {
		return nil, route, fmt.Errorf("eval corpus status %s; provider call blocked", eval.Status)
	}
	if eval.LiveModelCalls != 0 {
		return nil, route, fmt.Errorf("eval corpus reported %d live model call(s); provider call blocked", eval.LiveModelCalls)
	}
	if strings.TrimSpace(opts.OpenAICompatibleURL) == "" {
		return nil, route, fmt.Errorf("openai-compatible base_url is required")
	}
	if route.Model == "" {
		return nil, route, fmt.Errorf("model is required")
	}
	apiKey := ""
	if strings.TrimSpace(opts.APIKeyEnv) != "" {
		apiKey = os.Getenv(strings.TrimSpace(opts.APIKeyEnv))
	}
	system, user := buildKnowledgeEssencePrompt(req, evidence)
	raw, usage, latencyMS, err := distiller.ChatCompletionJSON(context.Background(), nil, opts.OpenAICompatibleURL, apiKey, route.Model, system, user)
	if err != nil {
		return nil, route, err
	}
	route.Called = true
	route.Usage = usage
	route.LatencyMS = latencyMS
	route.LiveModelCalls = 1
	return raw, route, nil
}

func buildKnowledgeEssencePrompt(req ExtractionRequest, evidence mem.EvidenceItem) (string, string) {
	system := strings.Join([]string{
		"Return only JSON with top-level essence_lines.",
		"Do not emit Compound's full extraction envelope.",
		"Each essence line has predicate_hint, statement, and quote.",
		"The quote must be copied from the provided evidence text.",
		"Every fact in statement must appear in the evidence quote. Never add numbers, names, dates, or claims that are not in the evidence.",
		`If the evidence contains no durable insight beyond its own words, return {"essence_lines":[]}. An empty list is strongly preferred over restating or paraphrasing the quote.`,
	}, "\n")
	user := strings.Join(compactStrings(
		"Knowledge extraction item: "+req.ItemRef,
		"Title: "+req.Title,
		"Evidence ref: "+req.EvidenceRef,
		"Evidence quote:\n"+evidence.QuoteText,
		"Return shape: {\"essence_lines\":[{\"predicate_hint\":\"reports_result\",\"statement\":\"one grounded meaning claim\",\"quote\":\"short exact supporting quote\"}]}",
	), "\n\n")
	return system, user
}

type essenceEnvelope struct {
	EssenceLines []EssenceLine `json:"essence_lines"`
}

func normalizeKnowledgeExtractionRaw(req *ExtractionRequest, evidence mem.EvidenceItem, llmReq distiller.LLMExtractionRequest, raw []byte, route *ExtractionRouteEvidence) ([]byte, error) {
	var strict distiller.LLMExtractionResponse
	if err := json.Unmarshal(raw, &strict); err == nil && strings.TrimSpace(strict.SchemaVersion) == distiller.LLMExtractionSchemaVersion {
		route.ShapedCandidates = len(strict.Candidates)
		return raw, nil
	}

	var essence essenceEnvelope
	if err := json.Unmarshal(raw, &essence); err != nil {
		return nil, fmt.Errorf("parse essence extraction response: %w", err)
	}
	if len(essence.EssenceLines) == 0 {
		return nil, fmt.Errorf("essence extraction produced zero essence lines")
	}
	req.EssenceLines = cleanEssenceLines(essence.EssenceLines)
	route.EssenceLines = len(req.EssenceLines)

	shaped, residue, hallucinationRejections := shapeEssenceLines(*req, evidence, llmReq, req.EssenceLines)
	req.EssenceResidue = residue
	route.ShapedCandidates = len(shaped.Candidates)
	route.ResidueLines = len(residue)
	route.HallucinationRejections = hallucinationRejections
	if len(shaped.Candidates) == 0 {
		return nil, fmt.Errorf("essence extraction produced zero shaped candidates")
	}
	return json.Marshal(shaped)
}

func cleanEssenceLines(lines []EssenceLine) []EssenceLine {
	out := make([]EssenceLine, 0, len(lines))
	for _, line := range lines {
		cleaned := EssenceLine{
			PredicateHint: strings.TrimSpace(line.PredicateHint),
			Statement:     strings.TrimSpace(line.Statement),
			Quote:         strings.TrimSpace(line.Quote),
		}
		if cleaned.PredicateHint != "" || cleaned.Statement != "" || cleaned.Quote != "" {
			out = append(out, cleaned)
		}
	}
	return out
}

func shapeEssenceLines(req ExtractionRequest, evidence mem.EvidenceItem, llmReq distiller.LLMExtractionRequest, lines []EssenceLine) (distiller.LLMExtractionResponse, []string, int) {
	response := distiller.LLMExtractionResponse{
		SchemaVersion: distiller.LLMExtractionSchemaVersion,
		TraceRef:      llmReq.TraceRef,
		SourceRefs:    []string{req.EvidenceRef},
	}
	residue := []string{}
	hallucinationRejections := 0
	for _, line := range lines {
		statement := strings.TrimSpace(line.Statement)
		quote := strings.TrimSpace(line.Quote)
		if statement == "" || quote == "" {
			residue = append(residue, essenceResidue(line, "missing statement or quote"))
			continue
		}
		if !quoteAppearsInEvidence(evidence.QuoteText, quote) {
			residue = append(residue, essenceResidue(line, "quote not found in evidence"))
			hallucinationRejections++
			continue
		}
		confidence := 0.72
		response.Candidates = append(response.Candidates, distiller.LLMCandidate{
			Kind:       distiller.KindMemory,
			Entity:     knowledgeEntity(req.SourceKind, req.ItemRef),
			Predicate:  predicateFromEssenceHint(line.PredicateHint),
			Object:     statement,
			Confidence: &confidence,
			Derivation: "extracted",
			ClaimType:  "architecture",
			Qualifiers: map[string]string{
				"source":        "knowledge_extraction",
				"extraction_id": req.ID,
				"item_ref":      req.ItemRef,
				"shaped_by":     "deterministic",
			},
			EvidenceRefs: []string{req.EvidenceRef},
		})
	}
	return response, residue, hallucinationRejections
}

func essenceResidue(line EssenceLine, reason string) string {
	parts := compactStrings(
		"reason="+strings.TrimSpace(reason),
		"predicate_hint="+strings.TrimSpace(line.PredicateHint),
		"statement="+strings.TrimSpace(line.Statement),
		"quote="+strings.TrimSpace(line.Quote),
	)
	return strings.Join(parts, " | ")
}

func quoteAppearsInEvidence(evidenceQuote, quote string) bool {
	evidenceQuote = strings.ToLower(strings.Join(strings.Fields(evidenceQuote), " "))
	quote = strings.ToLower(strings.Join(strings.Fields(quote), " "))
	return quote != "" && strings.Contains(evidenceQuote, quote)
}

func predicateFromEssenceHint(hint string) string {
	hint = strings.ToLower(strings.TrimSpace(hint))
	if hint == "" {
		return "claims"
	}
	var b strings.Builder
	lastUnderscore := false
	for _, r := range hint {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore && b.Len() > 0 {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	predicate := strings.Trim(b.String(), "_")
	if predicate == "" {
		return "claims"
	}
	if predicate[0] >= '0' && predicate[0] <= '9' {
		return "claims_" + predicate
	}
	return predicate
}

func buildKnowledgeExtractionRequest(req ExtractionRequest, evidence mem.EvidenceItem) distiller.LLMExtractionRequest {
	body := strings.Join(compactStrings(
		"Knowledge extraction item: "+req.ItemRef,
		"Title: "+req.Title,
		"Evidence ref: "+req.EvidenceRef,
		"Evidence quote:\n"+evidence.QuoteText,
	), "\n\n")
	llmReq := distiller.BuildLLMExtractionRequest("knowledge_extraction:"+req.ID, []byte(body), distiller.DefaultPolicy(), distiller.PriorState{})
	llmReq.SourceRefs = []string{req.EvidenceRef}
	llmReq.Instructions = append(llmReq.Instructions,
		"Emit memory candidates only.",
		"Top-level schema_version must be "+distiller.LLMExtractionSchemaVersion+" and trace_ref must be "+llmReq.TraceRef+".",
		"Candidate kind must be \"memory\" exactly; do not use \"memory_candidate\".",
		"Every candidate MUST set entity, predicate, and object. Use entity \""+knowledgeEntity(req.SourceKind, req.ItemRef)+"\" for claims about this item; use entity \"topic:<short-slug>\" only for a claim about a broader topic this item evidences.",
		"predicate is a short snake_case relation such as claims, reports_result, method, finding, limitation.",
		"object is ONE falsifiable statement grounded in the evidence quote — no speculation beyond it.",
		"Each candidate must cite the provided evidence ref exactly.",
		"Qualifiers may add domain metadata, but Compound will stamp source=knowledge_extraction.",
	)
	return llmReq
}

func memoryCandidateFromExtraction(req ExtractionRequest, dc distiller.Candidate, evidence mem.EvidenceItem, route ExtractionRouteEvidence, now time.Time) mem.Candidate {
	qualifiers := map[string]string{}
	for k, v := range dc.Qualifiers {
		if strings.TrimSpace(k) != "" && strings.TrimSpace(v) != "" {
			qualifiers[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	qualifiers["source"] = "knowledge_extraction"
	qualifiers["extraction_id"] = req.ID
	qualifiers["item_ref"] = req.ItemRef
	if _, ok := qualifiers["extracted_by"]; !ok {
		qualifiers["extracted_by"] = extractionActor(route)
	}
	if _, ok := qualifiers["shaped_by"]; !ok {
		qualifiers["shaped_by"] = "model"
	}
	derivation := dc.Derivation
	if !oneOf(derivation, mem.Derivations) {
		derivation = "extracted"
	}
	visibility := "contextual"
	if evidence.Visibility == "private" {
		visibility = "private"
	}
	return mem.Candidate{
		ID:              "cand_" + shortDigest("knowledge_extraction", req.ID, dc.Entity, dc.Predicate, dc.Object),
		Entity:          dc.Entity,
		Predicate:       dc.Predicate,
		Object:          dc.Object,
		ObservationDate: firstNonEmpty(evidence.EventTime, rfc3339(now)),
		Confidence:      dc.Confidence,
		Derivation:      derivation,
		ClaimType:       dc.ClaimType,
		Qualifiers:      qualifiers,
		SourceTraceRef:  "knowledge_extraction:" + req.ID,
		SnippetHash:     shortDigest(req.EvidenceRef, req.ContentDigest),
		Visibility:      visibility,
		State:           "proposed",
		CreatedAt:       rfc3339(now),
	}
}

func appendExtractionModelEvidence(homePath string, req ExtractionRequest, route ExtractionRouteEvidence, candidateIDs []string, now time.Time) error {
	row := ExtractionModelEvidenceRow{
		ID:                      "kxmodel_" + shortDigest(req.ID, extractionActor(route), rfc3339(now)),
		ExtractionID:            req.ID,
		ItemRef:                 req.ItemRef,
		EvidenceRef:             req.EvidenceRef,
		Provider:                route.Provider,
		Model:                   route.Model,
		ExtractedBy:             extractionActor(route),
		EssenceLines:            route.EssenceLines,
		ShapedCandidates:        route.ShapedCandidates,
		ResidueLines:            route.ResidueLines,
		HallucinationRejections: route.HallucinationRejections,
		CandidateIDs:            append([]string(nil), candidateIDs...),
		ObservedAt:              rfc3339(now),
	}
	return appendJSONL(filepath.Join(homePath, "ledger", "knowledge-extraction-model-evidence.jsonl"), row)
}

func extractionActor(route ExtractionRouteEvidence) string {
	provider := strings.TrimSpace(route.Provider)
	model := strings.TrimSpace(route.Model)
	switch {
	case provider != "" && model != "":
		return provider + "/" + model
	case provider != "":
		return provider
	case model != "":
		return model
	default:
		return "unknown"
	}
}

func ensureSourceRegistry(homePath string, source Source, now time.Time) error {
	path := filepath.Join(homePath, "registries", "knowledge-sources.jsonl")
	rows, err := loadSourceRegistry(path)
	if err != nil {
		return err
	}
	kind := strings.TrimSpace(source.Kind)
	ref := normalizeKindRef(kind, source.Ref)
	sourceID := knowledgeSourceID(kind, source.Ref)
	for _, row := range rows {
		if row.SourceID == sourceID || (strings.TrimSpace(row.Kind) == kind && normalizeKindRef(kind, row.Ref) == ref) {
			return nil
		}
	}
	return appendJSONL(path, SourceRegistryRow{
		SourceID:       sourceID,
		Kind:           kind,
		Ref:            ref,
		FirstSeen:      rfc3339(now),
		TrustPosterior: 0.5,
	})
}

type metadataCandidateField struct {
	qualifier string
	predicate string
	object    string
}

func candidatesForItem(export Export, item Item, evidenceID string, now time.Time) []mem.Candidate {
	entity := knowledgeEntity(export.Source.Kind, item.Ref)
	sourceKind := strings.TrimSpace(export.Source.Kind)
	sourceRef := normalizeKindRef(sourceKind, export.Source.Ref)
	itemRef := normalizeKindRef(sourceKind, item.Ref)
	fields := metadataFields(item)
	if len(fields) == 0 {
		fields = append(fields, metadataCandidateField{
			qualifier: "ref",
			predicate: "reference_ref",
			object:    itemRef,
		})
	}

	candidates := make([]mem.Candidate, 0, len(fields))
	for _, field := range fields {
		candidates = append(candidates, mem.Candidate{
			ID:              "cand_" + shortDigest(entity, evidenceID, field.predicate, field.object),
			Entity:          entity,
			Predicate:       field.predicate,
			Object:          field.object,
			ObservationDate: firstNonEmpty(itemEventTime(item), item.CapturedAt, rfc3339(now)),
			Confidence:      0.8,
			Derivation:      "deterministic",
			// Metadata claims are deterministic source output. LLM-extracted meaning
			// candidates from the extraction bridge must earn promotion separately.
			PromotionBasis: "trusted_deterministic_import",
			Qualifiers: map[string]string{
				"source":         "knowledge_import",
				"source_kind":    sourceKind,
				"source_ref":     sourceRef,
				"item_ref":       itemRef,
				"canonical":      canonicalRef(item),
				"metadata_field": field.qualifier,
			},
			SourceTraceRef: evidenceID,
			SnippetHash:    shortDigest(item.Content, item.ContentRef, evidenceID, field.predicate, field.object),
			Visibility:     candidateVisibility(export.Privacy),
			State:          "proposed",
			CreatedAt:      rfc3339(now),
		})
	}
	return candidates
}

func metadataFields(item Item) []metadataCandidateField {
	fields := []metadataCandidateField{}
	if title := strings.TrimSpace(item.Title); title != "" {
		fields = append(fields, metadataCandidateField{qualifier: "title", predicate: "reference_title", object: title})
	}
	if authors := joinTrimmed(item.Authors, "; "); authors != "" {
		fields = append(fields, metadataCandidateField{qualifier: "authors", predicate: "reference_authors", object: authors})
	}
	if authorHandle := strings.TrimSpace(item.AuthorHandle); authorHandle != "" {
		fields = append(fields, metadataCandidateField{qualifier: "author_handle", predicate: "reference_author_handle", object: authorHandle})
	}
	if publishedAt := strings.TrimSpace(item.PublishedAt); publishedAt != "" {
		fields = append(fields, metadataCandidateField{qualifier: "published_at", predicate: "reference_published_at", object: publishedAt})
	}
	if occurredAt := itemSourceOccurredAt(item); occurredAt != "" {
		fields = append(fields, metadataCandidateField{qualifier: "occurred_at", predicate: "reference_occurred_at", object: occurredAt})
	}
	if capturedAt := strings.TrimSpace(item.CapturedAt); capturedAt != "" {
		fields = append(fields, metadataCandidateField{qualifier: "captured_at", predicate: "reference_captured_at", object: capturedAt})
	}
	if topics := joinTrimmed(item.Topics, "; "); topics != "" {
		fields = append(fields, metadataCandidateField{qualifier: "topics", predicate: "reference_topics", object: topics})
	}
	if mediaRefs := joinTrimmed(item.MediaRefs, "; "); mediaRefs != "" {
		fields = append(fields, metadataCandidateField{qualifier: "media_refs", predicate: "reference_media_refs", object: mediaRefs})
	}
	if operatorNote := strings.TrimSpace(item.OperatorNote); operatorNote != "" {
		fields = append(fields, metadataCandidateField{qualifier: "operator_note", predicate: "reference_operator_note", object: operatorNote})
	}
	if url := strings.TrimSpace(item.URL); url != "" {
		fields = append(fields, metadataCandidateField{qualifier: "url", predicate: "reference_url", object: url})
	}
	return fields
}

type sourceObservationResult struct {
	ObservationID string
	CandidateIDs  []string
	NoPromoteIDs  []string
}

func applySourceObservation(homePath string, export Export, item Item, evidenceID string, now time.Time) (sourceObservationResult, error) {
	obs, ok := sourceObservationForItem(export, item, evidenceID, now)
	if !ok {
		return sourceObservationResult{}, nil
	}
	report, err := sourceobs.Apply(homePath, obs, now)
	if err != nil {
		return sourceObservationResult{}, err
	}
	return sourceObservationResult{
		ObservationID: report.ObservationID,
		CandidateIDs:  report.CandidateIDs,
		NoPromoteIDs:  report.NoPromoteIDs,
	}, nil
}

func addSourceObservationResult(report *ImportReport, applied sourceObservationResult) {
	if applied.ObservationID == "" {
		return
	}
	report.SourceObservationIDs = append(report.SourceObservationIDs, applied.ObservationID)
	report.SourceCandidateIDs = append(report.SourceCandidateIDs, applied.CandidateIDs...)
	report.NoPromoteIDs = append(report.NoPromoteIDs, applied.NoPromoteIDs...)
}

func sourceObservationForItem(export Export, item Item, evidenceID string, now time.Time) (sourceobs.SourceObservation, bool) {
	targets := sourceObservationMapTargets(export, item)
	if len(targets) == 0 {
		return sourceobs.SourceObservation{}, false
	}
	revision := "sha256:" + shortDigest(evidenceID, canonicalRef(item), contentDigest(item), strings.Join(sourceCandidateClasses(export, item), ","))
	observedAt := firstNonEmpty(item.CapturedAt, export.Source.CapturedAt, rfc3339(now))
	return sourceobs.SourceObservation{
		SchemaVersion: sourceobs.SchemaVersion,
		ID:            "srcobs_" + shortDigest("knowledge_import", export.Source.Kind, export.Source.Ref, item.Ref, evidenceID, revision),
		AdapterID:     "compound.knowledge-import",
		SourceSystem:  export.Source.Kind,
		ObservedAt:    observedAt,
		SourceRef: sourceobs.SourceRef{
			Path:        canonicalRef(item),
			Kind:        "knowledge_item",
			Owner:       export.Source.Kind,
			SourceClass: "knowledge_import",
			Revision:    revision,
		},
		GeneratedView: sourceobs.GeneratedView{
			ViewKind:    "knowledge_import",
			Consumer:    "compound knowledge import",
			Generator:   firstNonEmpty(export.Producer.Name, export.Producer.Kind, "knowledge_export"),
			Marker:      ExportSchemaVersion,
			Freshness:   "observed",
			DriftReason: sourceObservationDetail(export, item),
		},
		MapsTo:       targets,
		EvidenceRefs: []string{evidenceID},
	}, true
}

func sourceObservationMapTargets(export Export, item Item) []sourceobs.MapTarget {
	classes := sourceCandidateClasses(export, item)
	if len(classes) == 0 {
		return nil
	}
	entity := knowledgeEntity(export.Source.Kind, item.Ref)
	slug := slugFor(firstNonEmpty(item.Title, item.AuthorHandle, item.Ref, canonicalRef(item)))
	if slug == "" {
		slug = shortDigest(entity)
	}
	targets := make([]sourceobs.MapTarget, 0, len(classes))
	for _, class := range classes {
		switch class {
		case "reference":
			targets = append(targets, sourceobs.MapTarget{
				ObjectFamily:   "reference",
				CandidateKind:  string(candidates.CapabilityObs),
				EntityHint:     entity,
				PromoteAllowed: false,
			})
		case "design_taste_example":
			targets = append(targets, sourceobs.MapTarget{
				ObjectFamily:   "design_taste_example",
				CandidateKind:  string(candidates.Taste),
				EntityHint:     "taste:" + slug,
				PromoteAllowed: false,
			})
		case "product_idea":
			targets = append(targets, sourceobs.MapTarget{
				ObjectFamily:   "product_idea",
				CandidateKind:  string(candidates.ProductHypothesis),
				EntityHint:     "product:unscoped:idea:" + slug,
				PromoteAllowed: false,
			})
		case "technique":
			targets = append(targets, sourceobs.MapTarget{
				ObjectFamily:   "technique",
				CandidateKind:  string(candidates.MethodUpgrade),
				EntityHint:     "method:" + slug,
				PromoteAllowed: false,
			})
		case "market_take":
			targets = append(targets, sourceobs.MapTarget{
				ObjectFamily:   "market_take",
				CandidateKind:  string(candidates.ProductHypothesis),
				EntityHint:     "product:unscoped:market:" + slug,
				PromoteAllowed: false,
			})
		}
	}
	return targets
}

func sourceCandidateClasses(export Export, item Item) []string {
	if len(item.CandidateClasses) > 0 {
		return item.CandidateClasses
	}
	switch normalizeCandidateClass(export.Source.Kind) {
	case "x_bookmarks", "twitter_bookmarks":
		return []string{"reference"}
	default:
		return nil
	}
}

func sourceObservationDetail(export Export, item Item) string {
	parts := compactStrings(
		"source_kind="+export.Source.Kind,
		"classes="+strings.Join(sourceCandidateClasses(export, item), ","),
		"occurred_at="+itemEventTime(item),
	)
	if len(item.MediaRefs) > 0 {
		parts = append(parts, fmt.Sprintf("media_refs=%d", len(item.MediaRefs)))
	}
	if strings.TrimSpace(item.OperatorNote) != "" {
		parts = append(parts, "operator_note=true")
	}
	return strings.Join(parts, " ")
}

func joinTrimmed(values []string, sep string) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, sep)
}

func legacyCandidateForItem(export Export, item Item, evidenceID string, now time.Time) mem.Candidate {
	entity := knowledgeEntity(export.Source.Kind, item.Ref)
	return mem.Candidate{
		ID:              "cand_" + shortDigest(entity, evidenceID, item.Title, strings.Join(item.Authors, "\x00"), strings.Join(item.Topics, "\x00")),
		Entity:          entity,
		Predicate:       "reference_metadata",
		Object:          metadataClaim(item),
		ObservationDate: item.PublishedAt,
		Confidence:      0.8,
		Derivation:      "deterministic",
		// Metadata claims are deterministic tool output from the medium's
		// authoritative API — the definition of a trusted deterministic import.
		// LLM-extracted meaning candidates (extraction bridge) must NOT carry
		// a basis; adjudication earns theirs.
		PromotionBasis: "trusted_deterministic_import",
		Qualifiers: map[string]string{
			"source":      "knowledge_import",
			"source_kind": export.Source.Kind,
			"source_ref":  export.Source.Ref,
			"item_ref":    item.Ref,
			"canonical":   canonicalRef(item),
		},
		SourceTraceRef: evidenceID,
		SnippetHash:    shortDigest(item.Content, item.ContentRef, evidenceID),
		Visibility:     candidateVisibility(export.Privacy),
		State:          "proposed",
		CreatedAt:      rfc3339(now),
	}
}

func metadataClaim(item Item) string {
	parts := []string{}
	if item.Title != "" {
		parts = append(parts, "title="+item.Title)
	}
	if len(item.Authors) > 0 {
		parts = append(parts, "authors="+strings.Join(item.Authors, ", "))
	}
	if item.PublishedAt != "" {
		parts = append(parts, "published_at="+item.PublishedAt)
	}
	if len(item.Topics) > 0 {
		parts = append(parts, "topics="+strings.Join(item.Topics, ", "))
	}
	if item.URL != "" {
		parts = append(parts, "url="+item.URL)
	}
	if len(parts) == 0 {
		return "reference " + item.Ref
	}
	return strings.Join(parts, "; ")
}

func writeExtractionRequest(homePath string, export Export, item Item, evidenceID string, now time.Time) (ExtractionRequest, error) {
	id := "kx_" + shortDigest(canonicalRef(item), evidenceID)
	req := ExtractionRequest{
		SchemaVersion: ExtractionRequestSchemaVersion,
		ID:            id,
		ItemRef:       item.Ref,
		EvidenceRef:   evidenceID,
		SourceKind:    export.Source.Kind,
		Title:         item.Title,
		ContentDigest: contentDigest(item),
		State:         ExtractionStatePending,
		CreatedAt:     rfc3339(now),
	}
	path := filepath.Join(homePath, "inbox", "knowledge-extractions", id+".json")
	if _, err := os.Stat(path); err == nil {
		return req, nil
	} else if !os.IsNotExist(err) {
		return ExtractionRequest{}, err
	}
	if err := writeJSON(path, req); err != nil {
		return ExtractionRequest{}, err
	}
	return req, nil
}

func writeExtractionRequestFile(homePath string, req ExtractionRequest) error {
	return writeJSON(filepath.Join(homePath, "inbox", "knowledge-extractions", req.ID+".json"), req)
}

func loadExtractionRequests(homePath string) ([]ExtractionRequest, error) {
	dir := filepath.Join(homePath, "inbox", "knowledge-extractions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []ExtractionRequest
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var req ExtractionRequest
		if err := readJSONFile(filepath.Join(dir, entry.Name()), &req); err != nil {
			return nil, err
		}
		if req.ID != "" {
			out = append(out, req)
		}
	}
	return out, nil
}

// sortExtractionRequests orders requests deterministically by created_at then id
// so bounded batches (`--limit`) select a stable prefix regardless of the
// directory-read order returned by the filesystem. created_at is compared as an
// RFC3339 instant when both parse; otherwise it falls back to a lexical compare
// so a malformed timestamp still yields a total, stable order.
func sortExtractionRequests(requests []ExtractionRequest) {
	sort.SliceStable(requests, func(i, j int) bool {
		ci, cj := requests[i].CreatedAt, requests[j].CreatedAt
		ti, ei := time.Parse(time.RFC3339, ci)
		tj, ej := time.Parse(time.RFC3339, cj)
		if ei == nil && ej == nil && !ti.Equal(tj) {
			return ti.Before(tj)
		}
		if ci != cj {
			return ci < cj
		}
		return requests[i].ID < requests[j].ID
	})
}

func matchesExtractionItem(req ExtractionRequest, item string) bool {
	item = strings.TrimSpace(item)
	return item != "" && (req.ID == item || req.ItemRef == item)
}

func fixtureExtractionName(req ExtractionRequest) string {
	return strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(req.ID)
}

func appendDistributionEvent(homePath string, event DistributionEvent) error {
	return appendJSONL(filepath.Join(homePath, "ledger", "knowledge-distribution.jsonl"), event)
}

func appendImportRow(homePath string, row ImportIndexRow) error {
	if err := appendImportLedger(homePath, row); err != nil {
		return err
	}
	return appendImportIndex(homePath, row)
}

func appendImportLedger(homePath string, row ImportIndexRow) error {
	return appendJSONL(knowledgeImportLedgerPath(homePath), row)
}

func appendImportIndex(homePath string, row ImportIndexRow) error {
	return appendJSONL(knowledgeImportIndexPath(homePath), row)
}

func loadImportIndex(homePath string) ([]ImportIndexRow, error) {
	indexRows, indexExists, err := readImportRows(knowledgeImportIndexPath(homePath))
	if err != nil {
		return nil, err
	}
	ledgerRows, _, err := readImportRows(knowledgeImportLedgerPath(homePath))
	if err != nil {
		return nil, err
	}
	if len(indexRows) > 0 && len(ledgerRows) == 0 {
		if err := writeImportRows(knowledgeImportLedgerPath(homePath), indexRows); err != nil {
			return nil, err
		}
		return indexRows, nil
	}
	if len(ledgerRows) > 0 && (!indexExists || !sameImportRows(indexRows, ledgerRows)) {
		if err := writeImportRows(knowledgeImportIndexPath(homePath), ledgerRows); err != nil {
			return nil, err
		}
		return ledgerRows, nil
	}
	return indexRows, nil
}

func knowledgeImportIndexPath(homePath string) string {
	return filepath.Join(homePath, "indexes", "knowledge-imports.jsonl")
}

func knowledgeImportLedgerPath(homePath string) string {
	return filepath.Join(homePath, "ledger", "knowledge-imports.jsonl")
}

func readImportRows(path string) ([]ImportIndexRow, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var rows []ImportIndexRow
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row ImportIndexRow
		if err := json.Unmarshal([]byte(line), &row); err != nil || row.CanonicalRef == "" {
			continue
		}
		rows = append(rows, row)
	}
	return rows, true, nil
}

func writeImportRows(path string, rows []ImportIndexRow) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func sameImportRows(a, b []ImportIndexRow) bool {
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

func loadSourceRegistry(path string) ([]SourceRegistryRow, error) {
	var rows []SourceRegistryRow
	return rows, readJSONL(path, func(line []byte) error {
		var row SourceRegistryRow
		if err := json.Unmarshal(line, &row); err != nil || row.Kind == "" || row.Ref == "" {
			return nil
		}
		rows = append(rows, row)
		return nil
	})
}

func findExactImport(rows []ImportIndexRow, canonical, exportDigest string) (ImportIndexRow, bool) {
	for _, row := range rows {
		if row.CanonicalRef == canonical && row.ExportDigest == exportDigest {
			return row, true
		}
	}
	return ImportIndexRow{}, false
}

func findRecentImport(rows []ImportIndexRow, canonical string, now time.Time, window time.Duration) (ImportIndexRow, bool) {
	var best ImportIndexRow
	var bestTime time.Time
	for _, row := range rows {
		if row.CanonicalRef != canonical {
			continue
		}
		importedAt, err := time.Parse(time.RFC3339, row.ImportedAt)
		if err != nil {
			continue
		}
		age := now.Sub(importedAt)
		if age < 0 || age > window {
			continue
		}
		if best.EvidenceRef == "" || importedAt.After(bestTime) {
			best = row
			bestTime = importedAt
		}
	}
	return best, best.EvidenceRef != ""
}

func canonicalRef(item Item) string {
	if strings.TrimSpace(item.URL) != "" {
		return strings.TrimSpace(item.URL)
	}
	return strings.TrimSpace(item.Ref)
}

func boundedQuote(content string) string {
	content = strings.TrimSpace(content)
	if len(content) > maxEvidenceQuoteChars {
		return content[:maxEvidenceQuoteChars]
	}
	return content
}

func contentDigest(item Item) string {
	content := item.Content
	if strings.TrimSpace(content) == "" {
		content = item.ContentRef
	}
	return digestString(content)
}

func evidenceVisibility(privacy string) string {
	if privacy == "private" {
		return "private"
	}
	return "operational"
}

func candidateVisibility(privacy string) string {
	if privacy == "private" {
		return "private"
	}
	return "contextual"
}

func defaultPrivacy(sourceKind string) string {
	switch normalizeCandidateClass(sourceKind) {
	case "x_bookmarks", "twitter_bookmarks":
		return "private"
	default:
		return "public"
	}
}

func itemEventTime(item Item) string {
	return firstNonEmpty(item.OccurredAt, item.BookmarkedAt, item.PublishedAt)
}

func itemSourceOccurredAt(item Item) string {
	return firstNonEmpty(item.OccurredAt, item.BookmarkedAt)
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func readJSONFile(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func appendJSONL(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	return err
}

func readJSONL(path string, fn func([]byte) error) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil
	}
	for _, line := range strings.Split(trimmed, "\n") {
		if err := fn([]byte(line)); err != nil {
			return err
		}
	}
	return nil
}

func cleanStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, value := range in {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func normalizeCandidateClasses(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, value := range in {
		value = normalizeCandidateClass(value)
		if value == "" || seen[value] {
			continue
		}
		switch value {
		case "reference", "design_taste_example", "product_idea", "technique", "market_take", "x_bookmarks", "twitter_bookmarks":
			out = append(out, value)
			seen[value] = true
		}
	}
	return out
}

func normalizeCandidateClass(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.NewReplacer("-", "_", " ", "_", "/", "_").Replace(value)
	for strings.Contains(value, "__") {
		value = strings.ReplaceAll(value, "__", "_")
	}
	value = strings.Trim(value, "_")
	switch value {
	case "design_taste", "design_reference", "design_example", "taste", "taste_example":
		return "design_taste_example"
	case "product", "product_take", "product_hypothesis", "idea":
		return "product_idea"
	case "market", "market_signal", "market_opinion":
		return "market_take"
	default:
		return value
	}
}

func compactStrings(in ...string) []string {
	out := make([]string, 0, len(in))
	for _, value := range in {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func oneOf(value string, allowed []string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func digestString(value string) string {
	return digestBytes([]byte(value))
}

func digestStruct(v any) string {
	data, _ := json.Marshal(v)
	return digestBytes(data)
}

func shortDigest(parts ...string) string {
	return digestString(strings.Join(parts, "\x00"))[:16]
}

func slugFor(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			if b.Len() > 0 {
				last := b.String()[b.Len()-1]
				if last != '-' {
					b.WriteByte('-')
				}
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func rfc3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// knowledgeEntity is the canonical entity id for an imported knowledge item.
func knowledgeEntity(sourceKind, itemRef string) string {
	kind := strings.TrimSpace(sourceKind)
	return "knowledge:" + kind + ":" + normalizeKindRef(kind, itemRef)
}

func knowledgeSourceID(sourceKind, sourceRef string) string {
	kind := strings.TrimSpace(sourceKind)
	return "source:" + kind + ":" + normalizeKindRef(kind, sourceRef)
}

func normalizeKindRef(kind, ref string) string {
	kind = strings.TrimSpace(kind)
	ref = strings.TrimSpace(ref)
	if kind == "" {
		return ref
	}
	prefix := strings.ToLower(kind) + ":"
	for {
		trimmed := strings.TrimSpace(ref)
		if !strings.HasPrefix(strings.ToLower(trimmed), prefix) {
			return trimmed
		}
		ref = strings.TrimSpace(trimmed[len(kind)+1:])
	}
}
