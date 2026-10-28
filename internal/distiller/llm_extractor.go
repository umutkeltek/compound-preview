package distiller

import (
	"encoding/json"
	"fmt"
	"strings"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/finding"
)

const LLMExtractionSchemaVersion = "compound.distiller.llm_extraction.v0.1"

// FindingSourcePrefix is the evidence-ref kind we accept when looking for finding refs in
// source_refs. Anything else (and any other FindingRef.ID prefix) fails closed at hydrate time.
const FindingSourcePrefix = "finding:"

// WorkEvidencePrefix is the evidence-ref kind used to mark the work_id the producer of the
// finding recorded (when present), and that gets preserved into SourceRefs.
const WorkEvidencePrefix = "work:"

// FindingRef is the deferred-body reference to a finding stored in COMPOUND_HOME/observed/findings.
// It carries id+cid+work_id on disk and over the wire; the body is materialized separately
// into RawTrace by the hydrator at model-execution time. The ref-only shape keeps the request
// artifact free of finding bodies while still letting the model see them through the existing
// RawTrace field — the bodies flow to the model ONLY through the in-memory hydrated request,
// not through what is recorded or routed as evidence.
type FindingRef struct {
	ID     string `json:"id"`
	CID    string `json:"cid"`
	WorkID string `json:"work_id,omitempty"`
}

// LLMExtractionRequest is the provider-neutral packet sent to a model lane.
// It is data, not a live client: callers can route it through Switchback, a local
// model, or a fixture without changing distiller authority rules.
type LLMExtractionRequest struct {
	SchemaVersion      string                  `json:"schema_version"`
	TraceRef           string                  `json:"trace_ref"`
	SourcePrivacy      string                  `json:"source_privacy,omitempty"`
	Policy             Policy                  `json:"policy"`
	PriorCoverage      PriorCoverage           `json:"prior_coverage"`
	PriorSignals       []LLMPriorSignal        `json:"prior_signals,omitempty"`
	RawTrace           string                  `json:"raw_trace,omitempty"`
	SourceRefs         []string                `json:"source_refs,omitempty"`
	FindingRefs        []FindingRef            `json:"finding_refs,omitempty"`
	Work               *LLMWork                `json:"work,omitempty"`
	Context            *LLMContext             `json:"context,omitempty"`
	Receipt            *LLMReceipt             `json:"receipt,omitempty"`
	CapabilityFeedback []LLMCapabilityFeedback `json:"capability_feedback,omitempty"`
	Envelopes          []LLMEnvelope           `json:"envelopes,omitempty"`
	Instructions       []string                `json:"instructions"`
}

type LLMPriorSignal struct {
	Kind          string   `json:"kind"`
	ID            string   `json:"id"`
	Subject       string   `json:"subject,omitempty"`
	Statement     string   `json:"statement,omitempty"`
	Detail        string   `json:"detail,omitempty"`
	CandidateRef  string   `json:"candidate_ref,omitempty"`
	CandidateKind string   `json:"candidate_kind,omitempty"`
	Basis         string   `json:"basis,omitempty"`
	Status        string   `json:"status,omitempty"`
	EvidenceRefs  []string `json:"evidence_refs,omitempty"`
}

func BuildLLMExtractionRequest(traceRef string, rawTrace []byte, policy Policy, prior PriorState) LLMExtractionRequest {
	req, _ := BuildLLMExtractionRequestFromSources(traceRef, LLMExtractionSources{RawTrace: rawTrace}, policy, prior)
	return req
}

const (
	maxPriorSignalsPerKind   = 4
	maxPriorNoPromoteSignals = 6
)

type LLMExtractionSources struct {
	RawTrace    []byte
	Work        *envelope.WorkEnvelope
	ContextPack *envelope.ContextPack
	Receipt     *envelope.Receipt
	Extra       []envelope.Envelope
}

type LLMWork struct {
	ID                 string   `json:"id"`
	Owner              string   `json:"owner"`
	State              string   `json:"state"`
	DesiredOutcome     string   `json:"desired_outcome,omitempty"`
	SuccessPredicate   string   `json:"success_predicate"`
	NonGoals           []string `json:"non_goals,omitempty"`
	Risk               string   `json:"risk"`
	Privacy            string   `json:"privacy"`
	ActionIntentIDs    []string `json:"run_ids,omitempty"`
	RunAttemptIDs      []string `json:"run_attempt_ids,omitempty"`
	ArtifactIDs        []string `json:"artifact_ids,omitempty"`
	VerificationIDs    []string `json:"verification_ids,omitempty"`
	MemoryCandidateIDs []string `json:"memory_candidate_ids,omitempty"`
}

type LLMContext struct {
	ID                 string                 `json:"id"`
	SummaryMD          string                 `json:"summary_md,omitempty"`
	Included           []envelope.IncludedRef `json:"included"`
	Excluded           []envelope.ExcludedRef `json:"excluded"`
	ActiveInstructions []string               `json:"active_instructions,omitempty"`
	ActiveDecisions    []string               `json:"active_decisions,omitempty"`
	RequiredChecks     []string               `json:"required_checks,omitempty"`
	Avoid              []string               `json:"avoid,omitempty"`
}

type LLMReceipt struct {
	ID         string                 `json:"id"`
	Outcome    string                 `json:"outcome"`
	MoveType   string                 `json:"move_type"`
	StateDelta envelope.StateDelta    `json:"state_delta"`
	ProofRefs  []string               `json:"proof_refs,omitempty"`
	Cost       envelope.ReceiptCost   `json:"cost"`
	Skills     envelope.ReceiptSkills `json:"skills"`
	Memory     envelope.ReceiptMemory `json:"memory"`
	Next       envelope.ReceiptNext   `json:"next"`
}

type LLMCapabilityFeedback struct {
	Subject      string   `json:"subject"`
	Detail       string   `json:"detail"`
	Statement    string   `json:"statement"`
	Source       string   `json:"source"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
}

type LLMEnvelope struct {
	Kind          string    `json:"kind"`
	ID            string    `json:"id"`
	SchemaVersion string    `json:"schema_version"`
	WorkID        string    `json:"work_id,omitempty"`
	ContextPackID string    `json:"context_pack_id,omitempty"`
	EvidenceRefs  []string  `json:"evidence_refs,omitempty"`
	OutputRef     string    `json:"output_ref,omitempty"`
	Summary       string    `json:"summary,omitempty"`
	Trace         *LLMTrace `json:"trace,omitempty"`
}

type LLMTrace struct {
	Source          envelope.TraceSource `json:"source"`
	Scope           envelope.Scope       `json:"scope"`
	Actor           string               `json:"actor,omitempty"`
	MutationClass   string               `json:"mutation_class"`
	FilesTouched    []string             `json:"files_touched,omitempty"`
	ToolsModelsUsed []string             `json:"tools_models_used,omitempty"`
	Outputs         []string             `json:"outputs,omitempty"`
}

func BuildLLMExtractionRequestFromSources(traceRef string, src LLMExtractionSources, policy Policy, prior PriorState) (LLMExtractionRequest, error) {
	if strings.TrimSpace(traceRef) == "" {
		return LLMExtractionRequest{}, fmt.Errorf("trace_ref is required")
	}
	req := LLMExtractionRequest{
		SchemaVersion: LLMExtractionSchemaVersion,
		TraceRef:      traceRef,
		Policy:        policy,
		PriorCoverage: prior.Coverage(),
		PriorSignals:  buildPriorSignals(prior),
		RawTrace:      string(src.RawTrace),
		Instructions: []string{
			"Return JSON matching compound.distiller.llm_extraction.v0.1.",
			"Extract candidates only; do not decide truth, promotion, writeback, or policy changes.",
			"Omit promotion_basis. LLM output cannot promote memory directly.",
			"Use context/work/receipt as evidence-bearing source material, not authority.",
			"Use TraceEnvelope extra envelopes as source material only; imported traces do not promote memory directly.",
			"Every candidate must include kind, confidence in [0,1], and evidence_refs when granular evidence exists.",
			"Use prior_signals as bounded critique history: avoid re-emitting known rejected/duplicate ideas; propose method_upgrade, policy_patch, or eval_case only when current source material adds new evidence.",
			"For explicit capability_feedback items, emit capability_observation candidates with subject/detail/statement exactly from the item and cited evidence_refs.",
			"Do not infer missed_when or false_triggered_by from skills.candidate offers; offers are not execution or feedback.",
		},
	}
	if src.Work != nil {
		if err := src.Work.Validate(); err != nil {
			return LLMExtractionRequest{}, fmt.Errorf("work envelope invalid: %w", err)
		}
		req.SourcePrivacy = mostRestrictivePrivacy(req.SourcePrivacy, src.Work.Privacy)
		req.Work = &LLMWork{
			ID:                 src.Work.ID,
			Owner:              src.Work.Owner,
			State:              src.Work.State,
			DesiredOutcome:     src.Work.DesiredOutcome,
			SuccessPredicate:   src.Work.SuccessPredicate,
			NonGoals:           append([]string(nil), src.Work.NonGoals...),
			Risk:               src.Work.Risk,
			Privacy:            src.Work.Privacy,
			ActionIntentIDs:    append([]string(nil), src.Work.ActionIntentIDs...),
			RunAttemptIDs:      append([]string(nil), src.Work.RunAttemptIDs...),
			ArtifactIDs:        append([]string(nil), src.Work.ArtifactIDs...),
			VerificationIDs:    append([]string(nil), src.Work.VerificationIDs...),
			MemoryCandidateIDs: append([]string(nil), src.Work.MemoryCandidateIDs...),
		}
		addSourceRef(&req, "work:"+src.Work.ID)
		addSourceRefs(&req, src.Work.EvidenceRefs)
	}
	if src.ContextPack != nil {
		if err := src.ContextPack.Validate(); err != nil {
			return LLMExtractionRequest{}, fmt.Errorf("context pack invalid: %w", err)
		}
		req.SourcePrivacy = mostRestrictivePrivacy(req.SourcePrivacy, src.ContextPack.Privacy)
		req.Context = &LLMContext{
			ID:                 src.ContextPack.ID,
			SummaryMD:          src.ContextPack.SummaryMD,
			Included:           append([]envelope.IncludedRef(nil), src.ContextPack.Included...),
			Excluded:           append([]envelope.ExcludedRef(nil), src.ContextPack.Excluded...),
			ActiveInstructions: append([]string(nil), src.ContextPack.ActiveInstructions...),
			ActiveDecisions:    append([]string(nil), src.ContextPack.ActiveDecisions...),
			RequiredChecks:     append([]string(nil), src.ContextPack.RequiredChecks...),
			Avoid:              append([]string(nil), src.ContextPack.Avoid...),
		}
		addSourceRef(&req, "context_pack:"+src.ContextPack.ID)
		addSourceRefs(&req, src.ContextPack.EvidenceRefs)
	}
	if src.Receipt != nil {
		if err := src.Receipt.Validate(); err != nil {
			return LLMExtractionRequest{}, fmt.Errorf("receipt invalid: %w", err)
		}
		req.SourcePrivacy = mostRestrictivePrivacy(req.SourcePrivacy, src.Receipt.Privacy)
		req.Receipt = &LLMReceipt{
			ID:         src.Receipt.ID,
			Outcome:    src.Receipt.Outcome,
			MoveType:   src.Receipt.MoveType,
			StateDelta: src.Receipt.StateDelta,
			ProofRefs:  append([]string(nil), src.Receipt.ProofRefs...),
			Cost:       src.Receipt.Cost,
			Skills:     src.Receipt.Skills,
			Memory:     src.Receipt.Memory,
			Next:       src.Receipt.Next,
		}
		req.CapabilityFeedback = capabilityFeedbackFromReceipt(src.Receipt)
		addSourceRef(&req, "receipt:"+src.Receipt.ID)
		addSourceRefs(&req, src.Receipt.EvidenceRefs)
		addSourceRefs(&req, src.Receipt.ProofRefs)
	}
	for _, e := range src.Extra {
		if e == nil {
			continue
		}
		if err := e.Validate(); err != nil {
			return LLMExtractionRequest{}, fmt.Errorf("%s envelope invalid: %w", e.Kind(), err)
		}
		req.SourcePrivacy = mostRestrictivePrivacy(req.SourcePrivacy, envelopePrivacy(e))
		summary := summarizeEnvelope(e)
		req.Envelopes = append(req.Envelopes, summary)
		addSourceRef(&req, summary.Kind+":"+summary.ID)
		addSourceRefs(&req, summary.EvidenceRefs)
		addSourceRef(&req, summary.OutputRef)
	}
	if len(src.RawTrace) > 0 {
		addSourceRef(&req, "raw_trace:"+traceRef)
	}
	return req, nil
}

func buildPriorSignals(prior PriorState) []LLMPriorSignal {
	var signals []LLMPriorSignal
	for _, kind := range []string{KindMethodUpgrade, KindPolicyPatch, KindEvalCase} {
		signals = appendPriorCandidateSignals(signals, prior.CandidatesByKind[kind])
	}
	for i, row := range prior.NoPromoteDecisions {
		if i >= maxPriorNoPromoteSignals {
			break
		}
		if strings.TrimSpace(row.ID) == "" && strings.TrimSpace(row.CandidateRef) == "" {
			continue
		}
		signals = append(signals, LLMPriorSignal{
			Kind:          "no_promote_decision",
			ID:            strings.TrimSpace(row.ID),
			CandidateRef:  strings.TrimSpace(row.CandidateRef),
			CandidateKind: strings.TrimSpace(row.CandidateKind),
			Basis:         strings.TrimSpace(row.Basis),
			Status:        strings.TrimSpace(row.Status),
		})
	}
	return signals
}

func appendPriorCandidateSignals(signals []LLMPriorSignal, candidates []PriorCandidate) []LLMPriorSignal {
	for i, c := range candidates {
		if i >= maxPriorSignalsPerKind {
			break
		}
		if strings.TrimSpace(c.ID) == "" {
			continue
		}
		signals = append(signals, LLMPriorSignal{
			Kind:         strings.TrimSpace(c.Kind),
			ID:           strings.TrimSpace(c.ID),
			Subject:      strings.TrimSpace(c.Subject),
			Statement:    strings.TrimSpace(c.Statement),
			Detail:       strings.TrimSpace(c.Detail),
			EvidenceRefs: cleanRefs(c.EvidenceRefs),
		})
	}
	return signals
}

func capabilityFeedbackFromReceipt(rec *envelope.Receipt) []LLMCapabilityFeedback {
	if rec == nil {
		return nil
	}
	statement := strings.TrimSpace(rec.StateDelta.Summary)
	if statement == "" {
		statement = strings.TrimSpace(rec.Next.ReviewTrigger)
	}
	if statement == "" {
		statement = strings.TrimSpace(rec.MoveType)
	}
	evidenceRefs := uniqueRefs(append([]string{"receipt:" + rec.ID}, rec.EvidenceRefs...))
	var out []LLMCapabilityFeedback
	for _, ref := range rec.Skills.Missed {
		subject := capabilitySubject(ref)
		if subject == "" {
			continue
		}
		out = append(out, LLMCapabilityFeedback{
			Subject:      subject,
			Detail:       "missed_when",
			Statement:    statement,
			Source:       "receipt.skills.missed",
			EvidenceRefs: evidenceRefs,
		})
	}
	for _, ref := range rec.Skills.FalseTriggered {
		subject := capabilitySubject(ref)
		if subject == "" {
			continue
		}
		out = append(out, LLMCapabilityFeedback{
			Subject:      subject,
			Detail:       "false_triggered_by",
			Statement:    statement,
			Source:       "receipt.skills.false_triggered",
			EvidenceRefs: evidenceRefs,
		})
	}
	return out
}

func capabilitySubject(ref string) string {
	ref = strings.TrimSpace(ref)
	ref = strings.TrimPrefix(ref, "capability://")
	return strings.TrimSpace(ref)
}

func uniqueRefs(refs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out
}

// LLMExtractionResponse is the model-shaped result consumed by LLMExtractor.
// It intentionally has no client/provider dependency and no authority-bearing fields.
type LLMExtractionResponse struct {
	SchemaVersion string         `json:"schema_version,omitempty"`
	TraceRef      string         `json:"trace_ref"`
	Extractor     string         `json:"extractor,omitempty"`
	Model         string         `json:"model,omitempty"`
	SourceRefs    []string       `json:"source_refs,omitempty"`
	Candidates    []LLMCandidate `json:"candidates"`
	Critique      []string       `json:"critique,omitempty"`
	Warnings      []string       `json:"warnings,omitempty"`
}

// LLMCandidate mirrors Candidate but keeps authority-sensitive fields under parser control.
type LLMCandidate struct {
	Kind           string            `json:"kind"`
	Entity         string            `json:"entity,omitempty"`
	Predicate      string            `json:"predicate,omitempty"`
	Object         string            `json:"object,omitempty"`
	Subject        string            `json:"subject,omitempty"`
	Statement      string            `json:"statement,omitempty"`
	Detail         string            `json:"detail,omitempty"`
	Confidence     *float64          `json:"confidence"`
	Derivation     string            `json:"derivation,omitempty"`
	ClaimType      string            `json:"claim_type,omitempty"`
	Qualifiers     map[string]string `json:"qualifiers,omitempty"`
	PromotionBasis string            `json:"promotion_basis,omitempty"`
	EvidenceRefs   []string          `json:"evidence_refs,omitempty"`
}

// LLMExtractor consumes an already-produced model response. It does not call a model.
// When bound to an extraction request, it also gates candidate evidence refs against
// the request source refs so a model cannot invent citations that enter review queues.
type LLMExtractor struct {
	AllowedEvidenceRefs []string
	RequireEvidenceRefs bool
	CandidateVisibility string
}

// NewGroundedLLMExtractor returns a request-bound extractor for eval/live extraction
// and offline replay with a saved request packet.
// Standalone `distiller run --llm-response` intentionally uses the zero-value extractor
// because it has no request packet to define the evidence allowlist.
func NewGroundedLLMExtractor(req LLMExtractionRequest) LLMExtractor {
	return LLMExtractor{
		AllowedEvidenceRefs: append([]string(nil), req.SourceRefs...),
		RequireEvidenceRefs: true,
		CandidateVisibility: memoryVisibilityForSourcePrivacy(req.SourcePrivacy),
	}
}

func (e LLMExtractor) Extract(raw []byte, policy Policy) (Trace, error) {
	var response LLMExtractionResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return Trace{}, fmt.Errorf("parse llm extraction response: %w", err)
	}
	if strings.TrimSpace(response.TraceRef) == "" {
		return Trace{}, fmt.Errorf("llm extraction response missing trace_ref")
	}

	trace := Trace{
		TraceRef:   response.TraceRef,
		Candidates: make([]Candidate, 0, len(response.Candidates)),
	}
	for i, lc := range response.Candidates {
		c, err := e.toCandidate(i, lc, response)
		if err != nil {
			return Trace{}, err
		}
		trace.Candidates = append(trace.Candidates, c)
	}
	return normalizeTrace(trace, policy)
}

func (e LLMExtractor) toCandidate(index int, lc LLMCandidate, response LLMExtractionResponse) (Candidate, error) {
	promotionBasis := strings.TrimSpace(lc.PromotionBasis)
	if promotionBasis != "" && !strings.EqualFold(promotionBasis, candidates.BasisExplicitConfirmation) {
		return Candidate{}, fmt.Errorf("llm candidate %d attempted promotion_basis %q; model output cannot promote memory", index, lc.PromotionBasis)
	}
	if lc.Confidence == nil {
		return Candidate{}, fmt.Errorf("llm candidate %d missing confidence", index)
	}
	if *lc.Confidence < 0 || *lc.Confidence > 1 {
		return Candidate{}, fmt.Errorf("llm candidate %d confidence %.2f outside [0,1]", index, *lc.Confidence)
	}

	derivation := strings.TrimSpace(lc.Derivation)
	if derivation == "" {
		derivation = "llm_extracted"
	}
	evidenceRefs := cleanRefs(lc.EvidenceRefs)
	if len(evidenceRefs) == 0 && e.RequireEvidenceRefs {
		return Candidate{}, fmt.Errorf("llm candidate %d missing evidence_refs; request-grounded extraction requires candidate-specific source refs", index)
	}
	if len(evidenceRefs) == 0 && len(response.SourceRefs) > 0 {
		evidenceRefs = append(evidenceRefs, response.SourceRefs...)
	}
	if err := e.validateEvidenceRefs(index, evidenceRefs); err != nil {
		return Candidate{}, err
	}

	return Candidate{
		Kind:         lc.Kind,
		Entity:       lc.Entity,
		Predicate:    lc.Predicate,
		Object:       lc.Object,
		Subject:      lc.Subject,
		Statement:    lc.Statement,
		Detail:       lc.Detail,
		Confidence:   *lc.Confidence,
		Derivation:   derivation,
		ClaimType:    lc.ClaimType,
		Qualifiers:   lc.Qualifiers,
		Visibility:   e.CandidateVisibility,
		EvidenceRefs: evidenceRefs,
	}, nil
}

func mostRestrictivePrivacy(current, candidate string) string {
	rank := map[string]int{"public": 1, "private": 2, "sensitive": 3, "secret": 4}
	if rank[candidate] > rank[current] {
		return candidate
	}
	return current
}

func memoryVisibilityForSourcePrivacy(privacy string) string {
	switch privacy {
	case "public":
		return "operational"
	case "private", "sensitive", "secret":
		return "private"
	default:
		return ""
	}
}

func envelopePrivacy(e envelope.Envelope) string {
	switch v := e.(type) {
	case *envelope.EffectiveContract:
		return v.Privacy
	case *envelope.ContextRequest:
		return v.Privacy
	case *envelope.ContextPack:
		return v.Privacy
	case *envelope.WorkEnvelope:
		return v.Privacy
	case *envelope.ActionIntent:
		return v.Privacy
	case *envelope.PolicyDecision:
		return v.Privacy
	case *envelope.HookEvent:
		return v.Privacy
	case *envelope.TraceEnvelope:
		return v.Privacy
	case *envelope.Receipt:
		return v.Privacy
	case *envelope.Transition:
		return v.Privacy
	default:
		return ""
	}
}

func (e LLMExtractor) validateEvidenceRefs(index int, refs []string) error {
	if len(e.AllowedEvidenceRefs) == 0 {
		return nil
	}
	allowed := map[string]bool{}
	for _, ref := range e.AllowedEvidenceRefs {
		ref = strings.TrimSpace(ref)
		if ref != "" {
			allowed[ref] = true
		}
	}
	for _, ref := range refs {
		if !allowed[ref] {
			return fmt.Errorf("llm candidate %d evidence_ref %q not present in request source_refs", index, ref)
		}
	}
	return nil
}

func cleanRefs(refs []string) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref != "" {
			out = append(out, ref)
		}
	}
	return out
}

func summarizeEnvelope(e envelope.Envelope) LLMEnvelope {
	switch v := e.(type) {
	case *envelope.EffectiveContract:
		return LLMEnvelope{
			Kind:          v.Kind(),
			ID:            v.ID,
			SchemaVersion: v.SchemaVersion,
			EvidenceRefs:  append([]string(nil), v.EvidenceRefs...),
			Summary:       strings.TrimSpace(v.Harness + " " + v.TaskMode),
		}
	case *envelope.HookEvent:
		return LLMEnvelope{
			Kind:          v.Kind(),
			ID:            v.ID,
			SchemaVersion: v.SchemaVersion,
			WorkID:        v.IDs.WorkID,
			ContextPackID: v.IDs.ContextPackID,
			EvidenceRefs:  append([]string(nil), v.EvidenceRefs...),
			OutputRef:     v.OutputRef,
			Summary:       strings.TrimSpace(strings.TrimSpace(v.HookName+" "+v.Status+" "+v.FailOpenReason) + " " + v.OutputRef),
		}
	case *envelope.Transition:
		return LLMEnvelope{
			Kind:          v.Kind(),
			ID:            v.ID,
			SchemaVersion: v.SchemaVersion,
			WorkID:        v.IDs.WorkID,
			ContextPackID: v.IDs.ContextPackID,
			EvidenceRefs:  append([]string(nil), v.EvidenceRefs...),
			Summary:       v.StateDelta,
		}
	case *envelope.TraceEnvelope:
		return LLMEnvelope{
			Kind:          v.Kind(),
			ID:            v.ID,
			SchemaVersion: v.SchemaVersion,
			WorkID:        v.IDs.WorkID,
			ContextPackID: v.IDs.ContextPackID,
			EvidenceRefs:  append([]string(nil), v.EvidenceRefs...),
			Summary: strings.TrimSpace(strings.Join([]string{
				v.Source.System,
				v.Source.Stream,
				v.Source.SourceKind,
				v.MutationClass,
				v.Summary,
			}, " ")),
			Trace: &LLMTrace{
				Source:          v.Source,
				Scope:           v.Scope,
				Actor:           v.Actor,
				MutationClass:   v.MutationClass,
				FilesTouched:    append([]string(nil), v.FilesTouched...),
				ToolsModelsUsed: append([]string(nil), v.ToolsModelsUsed...),
				Outputs:         append([]string(nil), v.Outputs...),
			},
		}
	case *envelope.ContextRequest:
		return LLMEnvelope{
			Kind:          v.Kind(),
			ID:            v.ID,
			SchemaVersion: v.SchemaVersion,
			WorkID:        v.IDs.WorkID,
			ContextPackID: v.IDs.ContextPackID,
			EvidenceRefs:  append([]string(nil), v.EvidenceRefs...),
			Summary:       strings.TrimSpace(v.Harness + " " + v.RequestedMode + " " + v.TaskGuess),
		}
	default:
		return LLMEnvelope{Kind: e.Kind(), Summary: "typed envelope included"}
	}
}

func addSourceRefs(req *LLMExtractionRequest, refs []string) {
	for _, ref := range refs {
		addSourceRef(req, ref)
	}
}

func addSourceRef(req *LLMExtractionRequest, ref string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return
	}
	for _, existing := range req.SourceRefs {
		if existing == ref {
			return
		}
	}
	req.SourceRefs = append(req.SourceRefs, ref)
}

// BuildLLMExtractionRequestFromFindings constructs a ref-only LLMExtractionRequest whose
// FindingRefs section defers body hydration until model-execution time. Each supplied id is
// resolved through finding.Show exactly once; the resolved record is used to capture
// (id, cid, work_id, EvidenceRefs) atomically — the recorded request carries id+cid+work_id
// only and SourceRefs is primed with finding:<id>, work:<work_id> (when present), AND every
// evidence ref the finding itself recorded (the producer's evidence chain is preserved into
// route/request evidence without a second pass and the TOCTOU window it would otherwise
// open). SourcePrivacy is forced to "private" so the route/request evidence is properly
// classified. The artifact is replay-equivalent by construction: same finding content →
// same request id.
func BuildLLMExtractionRequestFromFindings(homePath, traceRef string, findingIDs []string, policy Policy, prior PriorState) (LLMExtractionRequest, error) {
	if strings.TrimSpace(homePath) == "" {
		return LLMExtractionRequest{}, fmt.Errorf("COMPOUND_HOME required to build finding-grounded request")
	}
	if strings.TrimSpace(traceRef) == "" {
		return LLMExtractionRequest{}, fmt.Errorf("trace_ref is required")
	}
	seen := map[string]bool{}
	refs := make([]FindingRef, 0, len(findingIDs))
	producerEvidence := make([][]string, 0, len(findingIDs))
	for _, id := range findingIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !strings.HasPrefix(id, "find_") {
			return LLMExtractionRequest{}, fmt.Errorf("finding id %q must start with find_", id)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		f, err := finding.Show(homePath, id)
		if err != nil {
			return LLMExtractionRequest{}, fmt.Errorf("resolve finding %q: %w", id, err)
		}
		// The loaded finding's ID must match what was requested — id fabrication by the
		// store layer must fail closed.
		if strings.TrimSpace(f.ID) != id {
			return LLMExtractionRequest{}, fmt.Errorf("finding id mismatch: requested %q, store returned %q", id, f.ID)
		}
		if strings.TrimSpace(f.CID) == "" {
			return LLMExtractionRequest{}, fmt.Errorf("finding %q missing CID", id)
		}
		refs = append(refs, FindingRef{ID: f.ID, CID: f.CID, WorkID: strings.TrimSpace(f.WorkID)})
		producerEvidence = append(producerEvidence, append([]string(nil), f.EvidenceRefs...))
	}
	if len(refs) == 0 {
		return LLMExtractionRequest{}, fmt.Errorf("at least one finding id is required")
	}
	req, err := BuildLLMExtractionRequestFromSources(traceRef, LLMExtractionSources{}, policy, prior)
	if err != nil {
		return LLMExtractionRequest{}, err
	}
	req.SourcePrivacy = "private" // finding bodies are operator-private by construction
	req.FindingRefs = refs
	for i, fr := range refs {
		addSourceRef(&req, FindingSourcePrefix+fr.ID)
		if strings.TrimSpace(fr.WorkID) != "" {
			addSourceRef(&req, WorkEvidencePrefix+fr.WorkID)
		}
		for _, ev := range producerEvidence[i] {
			addSourceRef(&req, strings.TrimSpace(ev))
		}
	}
	return req, nil
}

// HydrateLLMExtractionRequest returns an independent copy of req whose FindingRef entries
// have been verified against finding.Show (CID integrity is enforced by recomputing
// finding.CID(body) and comparing BOTH the persisted record's f.CID AND the requested ref
// fr.CID against it — finding.Show alone does not validate body/CID integrity, so a tampered
// finding file must fail closed) and whose bodies are materialized into RawTrace so the
// model prompt can carry them. The stored/ref-only request is left untouched: the
// recorded/routed path always sees RawTrace empty when sourced from a finding-grounded build.
func HydrateLLMExtractionRequest(homePath string, req LLMExtractionRequest) (LLMExtractionRequest, error) {
	if strings.TrimSpace(homePath) == "" {
		return LLMExtractionRequest{}, fmt.Errorf("COMPOUND_HOME required to hydrate finding-grounded request")
	}
	out := req
	out.FindingRefs = make([]FindingRef, len(req.FindingRefs))
	copy(out.FindingRefs, req.FindingRefs)
	if len(out.FindingRefs) == 0 {
		return LLMExtractionRequest{}, fmt.Errorf("finding-grounded request has no FindingRefs to hydrate")
	}
	var b strings.Builder
	for i, fr := range out.FindingRefs {
		id := strings.TrimSpace(fr.ID)
		if id == "" || !strings.HasPrefix(id, "find_") {
			return LLMExtractionRequest{}, fmt.Errorf("finding_ref[%d] %q: unsupported non-finding ref", i, id)
		}
		// The requested ref must carry a non-empty CID; every build path through
		// BuildLLMExtractionRequestFromFindings populates it, so an empty fr.CID is evidence
		// the request came through an unauthorised path.
		if strings.TrimSpace(fr.CID) == "" {
			return LLMExtractionRequest{}, fmt.Errorf("finding_ref[%d] %q: requested CID is empty (every build path populates it; refuse to materialise without it)", i, id)
		}
		f, err := finding.Show(homePath, id)
		if err != nil {
			return LLMExtractionRequest{}, fmt.Errorf("finding_ref[%d] %q: %w", i, id, err)
		}
		// Loaded ID must match the requested one — refuse id substitution.
		if strings.TrimSpace(f.ID) != id {
			return LLMExtractionRequest{}, fmt.Errorf("finding_ref[%d] %q: store returned mismatched id %q", i, id, f.ID)
		}
		// Recompute the actual CID from the on-disk body; this catches a tampered file even
		// when the persisted f.CID was also rewritten to remain internally consistent.
		actualCID := finding.CID(f.Body)
		if !strings.EqualFold(f.CID, actualCID) {
			return LLMExtractionRequest{}, fmt.Errorf("finding_ref[%d] %q: tampered body — persisted CID %q does not match recomputed CID %q", i, id, f.CID, actualCID)
		}
		if !strings.EqualFold(fr.CID, actualCID) {
			return LLMExtractionRequest{}, fmt.Errorf("finding_ref[%d] %q: requested CID %q does not match actual CID %q", i, id, fr.CID, actualCID)
		}
		out.FindingRefs[i] = FindingRef{ID: f.ID, CID: f.CID, WorkID: strings.TrimSpace(f.WorkID)}
		fmt.Fprintf(&b, "[finding:%s cid=%s work=%s]\n%s\n", f.ID, f.CID, f.WorkID, f.Body)
	}
	out.RawTrace = b.String()
	return out, nil
}
