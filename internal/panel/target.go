package panel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/mem"
)

type TargetInput struct {
	Kind       string          `json:"kind"`
	Ref        string          `json:"ref"`
	Visibility string          `json:"visibility"`
	Summary    string          `json:"summary"`
	Payload    json.RawMessage `json:"payload"`
}

// Panel candidate loads derive a target from exactly one custodied body: the
// descriptor-rooted page/record read IS the payload source, so a candidate
// replaced on disk after custody validation cannot smuggle a new body into a
// derived target. The vars are test seams that inject that swap and prove the
// loaded target still carries the frozen body.
var (
	readPanelCandidatePage   = candidates.ReadPage
	readPanelCandidateRecord = candidates.ReadRecord
)

func LoadMemoryCandidateTargets(homePath string, states []string, limit int) ([]TargetInput, error) {
	store, err := mem.Open(homePath)
	if err != nil {
		return nil, err
	}
	stateSet := targetStateSet(states)
	cands, err := store.AllCandidates()
	if err != nil {
		return nil, err
	}
	out := []TargetInput{}
	for _, cand := range cands {
		if !stateSet[cand.State] {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(homePath, candidates.InboxDir(candidates.Memory), cand.ID+".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, TargetInput{
			Kind:       "memory_candidate",
			Ref:        cand.ID,
			Visibility: cand.Visibility,
			Summary:    memoryCandidateSummary(cand),
			Payload:    memoryCandidatePanelPayload(homePath, cand, raw),
		})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func LoadSkillCandidateTargets(homePath string, limit int) ([]TargetInput, error) {
	dir := filepath.Join(homePath, "reviews", "skill-candidates")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	out := []TargetInput{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".decision.json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var review capgraph.SkillCandidateReview
		if err := json.Unmarshal(raw, &review); err != nil {
			return nil, fmt.Errorf("parse skill candidate review %s: %w", path, err)
		}
		ref := strings.TrimSuffix(entry.Name(), ".json")
		if strings.TrimSpace(review.ID) != "" {
			ref = review.ID
		}
		if hasSkillCandidateDecisionSidecar(dir, ref) {
			continue
		}
		out = append(out, TargetInput{
			Kind:       "skill_candidate",
			Ref:        ref,
			Visibility: "internal",
			Summary:    skillCandidateSummary(review),
			Payload:    json.RawMessage(append([]byte(nil), raw...)),
		})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func LoadCapabilityObservationTargets(homePath string, limit int) ([]TargetInput, error) {
	page, err := readPanelCandidatePage(homePath, candidates.CapabilityObs, "", 0)
	if err != nil {
		return nil, err
	}
	out := []TargetInput{}
	for _, entry := range page.Entries {
		if entry.Corruption != nil {
			if !strings.HasSuffix(string(entry.EntryCursor), ".json") {
				continue
			}
			return nil, entry.Corruption
		}
		if entry.Record == nil {
			continue
		}
		var obs capabilityObservationTarget
		if err := json.Unmarshal(entry.Record.Body, &obs); err != nil {
			return nil, fmt.Errorf("decode capability observation %s: %w", entry.Record.ID, err)
		}
		if !capabilityObservationAwaitingVerdict(obs) {
			continue
		}
		out = append(out, TargetInput{
			Kind:       "capability_observation",
			Ref:        entry.Record.ID,
			Visibility: "internal",
			Summary:    capabilityObservationSummary(obs),
			Payload:    json.RawMessage(append([]byte(nil), entry.Record.Body...)),
		})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func LoadDecisionCandidateTargets(homePath string, limit int) ([]TargetInput, error) {
	page, err := readPanelCandidatePage(homePath, candidates.Decision, "", 0)
	if err != nil {
		return nil, err
	}
	out := []TargetInput{}
	for _, entry := range page.Entries {
		if entry.Corruption != nil {
			if !strings.HasSuffix(string(entry.EntryCursor), ".json") {
				continue
			}
			return nil, entry.Corruption
		}
		if entry.Record == nil {
			continue
		}
		var target decisionCandidateTarget
		if err := json.Unmarshal(entry.Record.Body, &target); err != nil {
			return nil, fmt.Errorf("decode decision candidate %s: %w", entry.Record.ID, err)
		}
		if !decisionCandidateAwaitingPlan(target) {
			continue
		}
		out = append(out, TargetInput{
			Kind:       "decision_candidate",
			Ref:        entry.Record.ID,
			Visibility: "internal",
			Summary:    decisionCandidateSummary(target),
			Payload:    json.RawMessage(append([]byte(nil), entry.Record.Body...)),
		})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func LoadTargetInput(homePath, targetKind, ref string) (TargetInput, error) {
	switch targetKind {
	case "memory_candidate":
		record, err := readPanelCandidateRecord(homePath, candidates.Memory, ref)
		if err != nil {
			return TargetInput{}, err
		}
		var cand mem.Candidate
		if err := json.Unmarshal(record.Body, &cand); err != nil {
			return TargetInput{}, err
		}
		return TargetInput{Kind: "memory_candidate", Ref: cand.ID, Visibility: cand.Visibility, Summary: memoryCandidateSummary(cand), Payload: memoryCandidatePanelPayload(homePath, cand, record.Body)}, nil
	case "skill_candidate":
		path := filepath.Join(homePath, "reviews", "skill-candidates", ref+".json")
		raw, err := os.ReadFile(path)
		if err != nil {
			return TargetInput{}, err
		}
		var review capgraph.SkillCandidateReview
		if err := json.Unmarshal(raw, &review); err != nil {
			return TargetInput{}, fmt.Errorf("parse skill candidate review %s: %w", path, err)
		}
		targetRef := ref
		if strings.TrimSpace(review.ID) != "" {
			targetRef = review.ID
		}
		return TargetInput{Kind: "skill_candidate", Ref: targetRef, Visibility: "internal", Summary: skillCandidateSummary(review), Payload: json.RawMessage(append([]byte(nil), raw...))}, nil
	case "capability_observation":
		record, err := readPanelCandidateRecord(homePath, candidates.CapabilityObs, ref)
		if err != nil {
			return TargetInput{}, err
		}
		var obs capabilityObservationTarget
		if err := json.Unmarshal(record.Body, &obs); err != nil {
			return TargetInput{}, fmt.Errorf("decode capability observation %s: %w", ref, err)
		}
		return TargetInput{Kind: "capability_observation", Ref: ref, Visibility: "internal", Summary: capabilityObservationSummary(obs), Payload: json.RawMessage(append([]byte(nil), record.Body...))}, nil
	case "decision_candidate":
		record, err := readPanelCandidateRecord(homePath, candidates.Decision, ref)
		if err != nil {
			return TargetInput{}, err
		}
		var target decisionCandidateTarget
		if err := json.Unmarshal(record.Body, &target); err != nil {
			return TargetInput{}, fmt.Errorf("decode decision candidate %s: %w", ref, err)
		}
		return TargetInput{Kind: "decision_candidate", Ref: ref, Visibility: "internal", Summary: decisionCandidateSummary(target), Payload: json.RawMessage(append([]byte(nil), record.Body...))}, nil
	case "comparison":
		return loadComparisonTarget(homePath, ref)
	default:
		return TargetInput{}, fmt.Errorf("unsupported target kind %q", targetKind)
	}
}

func targetStateSet(states []string) map[string]bool {
	out := map[string]bool{}
	for _, state := range states {
		state = strings.TrimSpace(state)
		if state != "" {
			out[state] = true
		}
	}
	if len(out) == 0 {
		out["proposed"] = true
	}
	return out
}

func memoryCandidateSummary(c mem.Candidate) string {
	basis := c.PromotionBasis
	if basis == "" {
		basis = "none"
	}
	return fmt.Sprintf("%s %s %s (basis=%s confidence=%.2f)", c.Entity, c.Predicate, c.Object, basis, c.Confidence)
}

type capabilityObservationTarget struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	Subject      string   `json:"subject"`
	Statement    string   `json:"statement"`
	Detail       string   `json:"detail"`
	Confidence   float64  `json:"confidence"`
	State        string   `json:"state,omitempty"`
	Verdict      string   `json:"verdict,omitempty"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
}

type decisionCandidateTarget struct {
	ID           string            `json:"id"`
	Kind         string            `json:"kind"`
	Entity       string            `json:"entity,omitempty"`
	Predicate    string            `json:"predicate,omitempty"`
	Object       string            `json:"object,omitempty"`
	Subject      string            `json:"subject,omitempty"`
	Statement    string            `json:"statement,omitempty"`
	Detail       string            `json:"detail,omitempty"`
	Confidence   float64           `json:"confidence,omitempty"`
	Qualifiers   map[string]string `json:"qualifiers,omitempty"`
	EvidenceRefs []string          `json:"evidence_refs,omitempty"`
	Verdict      string            `json:"verdict,omitempty"`
}

func hasSkillCandidateDecisionSidecar(dir, reviewID string) bool {
	matches, err := filepath.Glob(filepath.Join(dir, reviewID+".*.decision.json"))
	return err == nil && len(matches) > 0
}

func skillCandidateSummary(review capgraph.SkillCandidateReview) string {
	name := firstNonEmptyString(review.External.Name, review.External.Slug, review.External.ID, review.CandidateID, review.ID)
	overlaps := make([]string, 0, len(review.OverlapRefs))
	for _, overlap := range review.OverlapRefs {
		ref := firstNonEmptyString(overlap.NodeID, overlap.Name)
		if ref != "" {
			overlaps = append(overlaps, ref)
		}
	}
	overlapText := "none"
	if len(overlaps) > 0 {
		overlapText = strings.Join(overlaps, ",")
	}
	return fmt.Sprintf("%s classification=%s disposition=%s overlaps=%s", name, emptyDashString(review.ProposedClassification), emptyDashString(review.RecommendedDisposition), overlapText)
}

func capabilityObservationSummary(obs capabilityObservationTarget) string {
	return fmt.Sprintf("%s %s %s (confidence=%.2f)", obs.Subject, obs.Detail, obs.Statement, obs.Confidence)
}

func decisionCandidateSummary(target decisionCandidateTarget) string {
	subject := firstNonEmptyString(target.Subject, target.Entity, target.Qualifiers["decision_id"], target.ID)
	choice := firstNonEmptyString(target.Statement, target.Object, target.Detail, "review decision candidate")
	scope := firstNonEmptyString(target.Qualifiers["scope"], target.Predicate, "-")
	return fmt.Sprintf("%s choice=%s scope=%s verdict=%s confidence=%.2f", subject, choice, scope, emptyDashString(target.Verdict), target.Confidence)
}

func capabilityObservationAwaitingVerdict(obs capabilityObservationTarget) bool {
	if strings.TrimSpace(obs.Verdict) != "" {
		return false
	}
	switch strings.TrimSpace(obs.State) {
	case "", "pending", "proposed", "awaiting_verdict", "needs_review":
		return true
	default:
		return false
	}
}

func decisionCandidateAwaitingPlan(target decisionCandidateTarget) bool {
	return strings.TrimSpace(target.Verdict) == candidates.Park
}

func memoryCandidatePanelPayload(homePath string, cand mem.Candidate, raw []byte) json.RawMessage {
	payload := append([]byte(nil), raw...)
	if enriched, ok := captureExhaustTranscriptPayload(homePath, cand, payload); ok {
		return enriched
	}
	provenance, ok := knowledgeExtractionSourceProvenance(homePath, cand)
	if !ok {
		return json.RawMessage(payload)
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return json.RawMessage(payload)
	}
	if _, exists := body["source_provenance"]; exists {
		return json.RawMessage(payload)
	}
	body["source_provenance"] = provenance
	data, err := json.Marshal(body)
	if err != nil {
		return json.RawMessage(payload)
	}
	return json.RawMessage(data)
}

// captureExhaustTranscriptPayload inlines the actual captured body into the memory-candidate
// payload sent to panel lenses for a `compound capture`-minted candidate. CaptureExhaust
// (internal/mem/capture.go) mints a candidate whose Object is only a content-addressed cid
// pointer ("sha256:<hex>"); the transcript text itself lives in the rebuildable
// observed/capture blob dir, never in the candidate record. Without this, a lens targeting a
// capture-minted candidate (e.g. the relational-dynamics lens set) would see the bare cid and
// have no transcript text to quote in evidence_refs. Mirrors the knowledge-extraction
// provenance enrichment above: additive, best-effort, never fails the target load — a missing
// or unreadable capture blob just leaves the payload as the candidate record alone.
func captureExhaustTranscriptPayload(homePath string, cand mem.Candidate, payload []byte) (json.RawMessage, bool) {
	if strings.TrimSpace(cand.Predicate) != "captured_exhaust" {
		return nil, false
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, false
	}
	if _, exists := body["transcript_text"]; exists {
		return json.RawMessage(payload), true
	}
	cid := strings.TrimSpace(cand.Object)
	if cid == "" {
		return nil, false
	}
	store, err := mem.Open(homePath)
	if err != nil {
		return nil, false
	}
	text, err := store.ReadCapture(cid)
	if err != nil {
		return nil, false
	}
	body["transcript_text"] = string(text)
	data, err := json.Marshal(body)
	if err != nil {
		return nil, false
	}
	return json.RawMessage(data), true
}

func knowledgeExtractionSourceProvenance(homePath string, cand mem.Candidate) (map[string]any, bool) {
	extractionID := strings.TrimSpace(cand.Qualifiers["extraction_id"])
	sourceTrace := strings.TrimSpace(cand.SourceTraceRef)
	if strings.HasPrefix(sourceTrace, "knowledge_extraction:") {
		extractionID = strings.TrimSpace(strings.TrimPrefix(sourceTrace, "knowledge_extraction:"))
	}
	if extractionID == "" || strings.TrimSpace(cand.Qualifiers["source"]) != "knowledge_extraction" {
		return nil, false
	}
	provenance := map[string]any{
		"status":           "missing_extraction_request",
		"source_trace_ref": sourceTrace,
		"extraction_id":    extractionID,
	}
	request, err := loadKnowledgeExtractionRequest(homePath, extractionID)
	if err != nil {
		provenance["error"] = err.Error()
		return provenance, true
	}
	provenance["status"] = "resolved"
	provenance["item_ref"] = request.ItemRef
	provenance["evidence_ref"] = request.EvidenceRef
	provenance["source_kind"] = request.SourceKind
	provenance["title"] = request.Title
	provenance["content_digest"] = request.ContentDigest
	provenance["request_state"] = request.State
	provenance["candidate_count"] = len(request.CandidateIDs)
	if request.RouteEvidence.Provider != "" {
		provenance["provider"] = request.RouteEvidence.Provider
	}
	if request.RouteEvidence.Model != "" {
		provenance["model"] = request.RouteEvidence.Model
	}
	if request.RouteEvidence.Called {
		provenance["model_called"] = request.RouteEvidence.Called
	}
	if request.RouteEvidence.LatencyMS > 0 {
		provenance["latency_ms"] = request.RouteEvidence.LatencyMS
	}
	if request.RouteEvidence.Usage.TotalTokens > 0 {
		provenance["total_tokens"] = request.RouteEvidence.Usage.TotalTokens
	}
	return provenance, true
}

type knowledgeExtractionRequestForPanel struct {
	ID            string                           `json:"id"`
	ItemRef       string                           `json:"item_ref"`
	EvidenceRef   string                           `json:"evidence_ref"`
	SourceKind    string                           `json:"source_kind"`
	Title         string                           `json:"title"`
	ContentDigest string                           `json:"content_digest"`
	State         string                           `json:"state"`
	CandidateIDs  []string                         `json:"candidate_ids"`
	RouteEvidence knowledgeExtractionRouteForPanel `json:"route_evidence"`
}

type knowledgeExtractionRouteForPanel struct {
	Provider  string                           `json:"provider"`
	Model     string                           `json:"model"`
	Called    bool                             `json:"called"`
	Usage     knowledgeExtractionUsageForPanel `json:"usage"`
	LatencyMS int64                            `json:"latency_ms"`
}

type knowledgeExtractionUsageForPanel struct {
	TotalTokens int `json:"total_tokens"`
}

func loadKnowledgeExtractionRequest(homePath, extractionID string) (knowledgeExtractionRequestForPanel, error) {
	path := filepath.Join(homePath, "inbox", "knowledge-extractions", extractionID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return knowledgeExtractionRequestForPanel{}, err
	}
	var request knowledgeExtractionRequestForPanel
	if err := json.Unmarshal(data, &request); err != nil {
		return knowledgeExtractionRequestForPanel{}, fmt.Errorf("parse extraction request %s: %w", extractionID, err)
	}
	return request, nil
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func emptyDashString(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return strings.TrimSpace(value)
}
