package distiller

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/envelope"
	"compound/internal/finding"
)

func TestLLMExtractorNormalizesProviderResponse(t *testing.T) {
	raw := []byte(`{
		"schema_version":"compound.distiller.llm_extraction.v0.1",
		"trace_ref":"llm_trace_1",
		"extractor":"fixture-critic",
		"model":"fixture-small",
		"source_refs":["raw:turn_7"],
		"candidates":[
			{"kind":"memory","entity":"project:api","predicate":"backend_language","object":"go","confidence":0.91},
			{"kind":"capability_observation","subject":"skill:paywall-cro","statement":"paywall","detail":"missed_when","confidence":0.74,"evidence_refs":["msg_9"]},
			{"kind":"capability_observation","subject":"skill:noisy-copy","statement":"paywall","detail":"false_triggered_by","confidence":0.8,"evidence_refs":["msg_10"]}
		]
	}`)

	a, err := LLMExtractor{}.Extract(raw, DefaultPolicy())
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	b, err := LLMExtractor{}.Extract(raw, DefaultPolicy())
	if err != nil {
		t.Fatalf("extract replay: %v", err)
	}
	if a.TraceRef != "llm_trace_1" || len(a.Candidates) != 3 {
		t.Fatalf("unexpected trace shape: %+v", a)
	}
	if a.Candidates[0].ID == "" || a.Candidates[0].ID != b.Candidates[0].ID {
		t.Fatalf("candidate id must be deterministic across replay: %q / %q", a.Candidates[0].ID, b.Candidates[0].ID)
	}
	if a.Candidates[0].PolicyVersion != "default.v1" {
		t.Fatalf("policy version = %q", a.Candidates[0].PolicyVersion)
	}
	if a.Candidates[0].Derivation != "llm_extracted" {
		t.Fatalf("derivation = %q", a.Candidates[0].Derivation)
	}
	if got := strings.Join(a.Candidates[0].EvidenceRefs, ","); got != "raw:turn_7" {
		t.Fatalf("missing candidate evidence should inherit response source_refs; got %q", got)
	}
	if got := strings.Join(a.Candidates[1].EvidenceRefs, ","); got != "msg_9" {
		t.Fatalf("candidate evidence refs should win over response source_refs; got %q", got)
	}
	if a.Candidates[2].Detail != "false_triggered_by" {
		t.Fatalf("false-trigger capability observation detail = %q", a.Candidates[2].Detail)
	}
	if a.Candidates[0].PromotionBasis != "" {
		t.Fatalf("LLM extractor must not accept promotion basis, got %q", a.Candidates[0].PromotionBasis)
	}
}

func TestGroundedLLMExtractorRequiresCandidateEvidenceRefs(t *testing.T) {
	req := LLMExtractionRequest{
		TraceRef:   "trace_grounded",
		SourceRefs: []string{"raw_trace:trace_grounded"},
	}
	raw := []byte(`{
		"schema_version":"compound.distiller.llm_extraction.v0.1",
		"trace_ref":"trace_grounded",
		"source_refs":["raw_trace:trace_grounded"],
		"candidates":[
			{"kind":"skill","subject":"skill:missing-evidence","statement":"missing citation","confidence":0.82}
		]
	}`)
	_, err := NewGroundedLLMExtractor(req).Extract(raw, DefaultPolicy())
	if err == nil || !strings.Contains(err.Error(), "missing evidence_refs") {
		t.Fatalf("err = %v, want missing evidence_refs", err)
	}
}

func TestGroundedLLMExtractorRejectsUngroundedEvidenceRefs(t *testing.T) {
	req := LLMExtractionRequest{
		TraceRef:   "trace_grounded",
		SourceRefs: []string{"raw_trace:trace_grounded", "receipt:rec_grounded"},
	}
	raw := []byte(`{
		"schema_version":"compound.distiller.llm_extraction.v0.1",
		"trace_ref":"trace_grounded",
		"source_refs":["raw_trace:trace_grounded"],
		"candidates":[
			{"kind":"skill","subject":"skill:made-up","statement":"invented citation","confidence":0.82,"evidence_refs":["made_up:source"]}
		]
	}`)
	_, err := NewGroundedLLMExtractor(req).Extract(raw, DefaultPolicy())
	if err == nil || !strings.Contains(err.Error(), "not present in request source_refs") {
		t.Fatalf("err = %v, want ungrounded evidence ref rejection", err)
	}
}

func TestLLMExtractorRejectsMalformedAuthorityAndKind(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "malformed json",
			raw:  `{"trace_ref":`,
			want: "parse llm extraction response",
		},
		{
			name: "missing confidence",
			raw:  `{"trace_ref":"t","candidates":[{"kind":"memory","entity":"e","predicate":"p","object":"o"}]}`,
			want: "missing confidence",
		},
		{
			name: "unknown kind",
			raw:  `{"trace_ref":"t","candidates":[{"kind":"../../evil","subject":"x","confidence":0.9}]}`,
			want: "unknown kind",
		},
		{
			name: "bad confidence",
			raw:  `{"trace_ref":"t","candidates":[{"kind":"skill","subject":"x","confidence":1.2}]}`,
			want: "outside [0,1]",
		},
		{
			name: "promotion attempt",
			raw:  `{"trace_ref":"t","candidates":[{"kind":"memory","entity":"e","predicate":"p","object":"o","confidence":0.9,"promotion_basis":"trusted_deterministic_import"}]}`,
			want: "cannot promote memory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LLMExtractor{}.Extract([]byte(tt.raw), DefaultPolicy())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestLLMExtractorRunParksLiveMemory(t *testing.T) {
	home, store := setup(t)
	raw := []byte(`{
		"trace_ref":"llm_live_trace",
		"candidates":[
			{"kind":"memory","entity":"svc","predicate":"lang","object":"go","confidence":0.95,"promotion_basis":"explicit_confirmation"},
			{"kind":"eval_case","subject":"distill.memory","statement":"missed evidence citation","confidence":0.8}
		]
	}`)

	res, err := Run(home, store, raw, DefaultPolicy(), LLMExtractor{}, t0)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Decisions) != 2 {
		t.Fatalf("decisions = %d, want 2", len(res.Decisions))
	}
	if res.Decisions[0].Kind != KindMemory || res.Decisions[0].Verdict != Park {
		t.Fatalf("LLM memory must park in live mode; got %+v", res.Decisions[0])
	}
	if res.Decisions[0].PromotionBasis != "" {
		t.Fatalf("LLM memory retained reserved authority: %+v", res.Decisions[0])
	}
	if res.Decisions[1].Kind != KindEvalCase || res.Decisions[1].Verdict != Operationalize {
		t.Fatalf("eval case should operationalize; got %+v", res.Decisions[1])
	}
	if cur, _ := store.AllCurrent(); len(cur) != 0 {
		t.Fatalf("LLM output must not promote canon directly; got %d current claims", len(cur))
	}
	parked, err := store.AllCandidates()
	if err != nil || len(parked) != 1 || parked[0].State != "parked" || parked[0].PromotionBasis != "" {
		t.Fatalf("parked model memory = %+v, err=%v", parked, err)
	}
}

func TestBuildLLMExtractionRequestCarriesPriorCoverageAndSignals(t *testing.T) {
	home, store := setup(t)
	run(t, home, store, `{"trace_ref":"seed","candidates":[
		{"kind":"memory","entity":"svc","predicate":"lang","object":"go","confidence":0.9,"claim_type":"decision"},
		{"kind":"method_upgrade","subject":"distill.memory","statement":"resolve relative dates absolute at extraction","confidence":0.9},
		{"kind":"policy_patch","subject":"distill.policy","statement":"require source refs for capability feedback","confidence":0.9},
		{"kind":"eval_case","subject":"distill.skill","statement":"missed an obvious skill trigger","confidence":1.0}
	]}`)
	run(t, home, store, `{"trace_ref":"seed_duplicate","candidates":[
		{"kind":"method_upgrade","subject":"distill.memory","statement":"resolve relative dates absolute at extraction","confidence":0.9}
	]}`)

	prior, err := LoadPriorStateFromHome(home, store, DefaultPolicy())
	if err != nil {
		t.Fatalf("prior: %v", err)
	}
	req := BuildLLMExtractionRequest("trace_new", []byte("raw transcript"), DefaultPolicy(), prior)
	if req.SchemaVersion != LLMExtractionSchemaVersion {
		t.Fatalf("schema version = %q", req.SchemaVersion)
	}
	if req.PriorCoverage.CurrentClaims != 1 || req.PriorCoverage.Decisions != 1 {
		t.Fatalf("prior coverage = %+v", req.PriorCoverage)
	}
	if req.PriorCoverage.MethodUpgrades != 1 || req.PriorCoverage.PolicyPatches != 1 || req.PriorCoverage.EvalCases != 1 || req.PriorCoverage.NoPromoteDecisions != 1 {
		t.Fatalf("prior coverage missing distiller critique signals: %+v", req.PriorCoverage)
	}
	for _, want := range []string{KindMethodUpgrade, KindPolicyPatch, KindEvalCase, "no_promote_decision"} {
		if !hasPriorSignalKind(req.PriorSignals, want) {
			t.Fatalf("prior signals missing %q: %+v", want, req.PriorSignals)
		}
	}
	if !strings.Contains(strings.Join(req.Instructions, "\n"), "cannot promote memory directly") {
		t.Fatalf("request instructions do not preserve authority boundary: %v", req.Instructions)
	}
	if !strings.Contains(strings.Join(req.Instructions, "\n"), "prior_signals as bounded critique history") {
		t.Fatalf("request instructions do not tell extractor how to use prior signals: %v", req.Instructions)
	}
}

func TestBuildLLMExtractionRequestFromSpineSources(t *testing.T) {
	home, store := setup(t)
	run(t, home, store, `{"trace_ref":"seed","candidates":[{"kind":"memory","entity":"svc","predicate":"lang","object":"go","confidence":0.9,"claim_type":"decision"}]}`)
	prior, err := LoadPriorStateFromHome(home, store, DefaultPolicy())
	if err != nil {
		t.Fatalf("prior: %v", err)
	}

	ctx := &envelope.ContextPack{
		Header:    llmTestHeader(envelope.SVContextPack, "ctx_distill", "work_distill", ""),
		SummaryMD: "Use product context; governance excluded.",
		Included: []envelope.IncludedRef{{
			Ref:         "repo://docs",
			SourceClass: "source",
			Reason:      "product task",
			Confidence:  0.9,
			Status:      "current",
		}},
		Excluded: []envelope.ExcludedRef{{
			Ref:    "control-plane://workflow-contract",
			Reason: "not ambient for product repo",
		}},
		ActiveInstructions: []string{"keep context scoped"},
		RequiredChecks:     []string{"memory doctor"},
		Avoid:              []string{"full OS governance dump"},
	}
	work := &envelope.WorkEnvelope{
		Header:           llmTestHeader(envelope.SVWork, "work_distill", "work_distill", "ctx_distill"),
		Owner:            "distiller:llm",
		State:            "running",
		DesiredOutcome:   "extract useful candidates from a bounded run",
		SuccessPredicate: "LLM request packet contains context, work, receipt, and prior coverage",
		NonGoals:         []string{"call a live model", "promote memory"},
		ActionIntentIDs:  []string{"act_distill"},
	}
	receipt := &envelope.Receipt{
		Header:   llmTestHeader(envelope.SVReceipt, "rec_distill", "work_distill", "ctx_distill"),
		Outcome:  "completed",
		MoveType: "memory_delta",
		StateDelta: envelope.StateDelta{
			Changed:      true,
			Summary:      "distiller found a reusable extraction gap",
			FilesChanged: []string{"internal/distiller/llm_extractor.go"},
			Artifacts:    []string{"artifact_request_packet"},
			Verification: []string{"go test ./internal/distiller"},
		},
		Cost:      envelope.ReceiptCost{ModelCalls: []string{}},
		ProofRefs: []string{"proof:request-packet", "artifact:request-packet"},
		Skills: envelope.ReceiptSkills{
			Used:           []string{"capability://skill:golang-testing"},
			Missed:         []string{"capability://skill:distiller-request-packet"},
			FalseTriggered: []string{"capability://skill:generic-copy"},
			Candidate:      []string{"capability://skill:offered-only"},
		},
		Memory: envelope.ReceiptMemory{Candidates: []string{"cand_method"}, Promoted: []string{}, Parked: []string{"cand_method"}},
		Next:   envelope.ReceiptNext{SuggestedWork: []string{"add offline eval runner"}, ReviewTrigger: "method debt > 0"},
	}
	hook := &envelope.HookEvent{
		Header:      llmTestHeader(envelope.SVHookEvent, "hook_distill", "work_distill", "ctx_distill"),
		Harness:     "codex",
		HookName:    "PostToolUse",
		InputHash:   "sha256:abc",
		SideEffects: []string{"wrote_candidate"},
		OutputRef:   "artifact_request_packet",
		DurationMS:  42,
		Status:      "ok",
	}

	req, err := BuildLLMExtractionRequestFromSources("trace_spine", LLMExtractionSources{
		RawTrace:    []byte("operator said this should become reusable"),
		ContextPack: ctx,
		Work:        work,
		Receipt:     receipt,
		Extra:       []envelope.Envelope{hook},
	}, DefaultPolicy(), prior)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if req.Context == nil || req.Context.ID != "ctx_distill" || len(req.Context.Excluded) != 1 {
		t.Fatalf("context summary missing excluded refs: %+v", req.Context)
	}
	if req.Work == nil || req.Work.SuccessPredicate == "" || len(req.Work.NonGoals) != 2 {
		t.Fatalf("work summary incomplete: %+v", req.Work)
	}
	if req.Receipt == nil || req.Receipt.StateDelta.Summary == "" || req.Receipt.Next.ReviewTrigger == "" {
		t.Fatalf("receipt summary incomplete: %+v", req.Receipt)
	}
	if req.SourcePrivacy != "private" {
		t.Fatalf("request source privacy = %q, want most-restrictive private", req.SourcePrivacy)
	}
	if req.Receipt.Cost.ModelCalls == nil {
		t.Fatalf("receipt cost metadata missing: %+v", req.Receipt.Cost)
	}
	if !hasString(req.Receipt.ProofRefs, "proof:request-packet") || !hasString(req.Receipt.ProofRefs, "artifact:request-packet") {
		t.Fatalf("receipt proof refs missing: %+v", req.Receipt)
	}
	if len(req.CapabilityFeedback) != 2 {
		t.Fatalf("capability feedback = %+v, want 2 explicit items", req.CapabilityFeedback)
	}
	if req.CapabilityFeedback[0].Subject != "skill:distiller-request-packet" || req.CapabilityFeedback[0].Detail != "missed_when" {
		t.Fatalf("missed feedback not normalized: %+v", req.CapabilityFeedback[0])
	}
	if req.CapabilityFeedback[1].Subject != "skill:generic-copy" || req.CapabilityFeedback[1].Detail != "false_triggered_by" {
		t.Fatalf("false-trigger feedback not normalized: %+v", req.CapabilityFeedback[1])
	}
	for _, feedback := range req.CapabilityFeedback {
		if feedback.Statement != receipt.StateDelta.Summary {
			t.Fatalf("feedback context = %q, want receipt summary %q", feedback.Statement, receipt.StateDelta.Summary)
		}
		if hasString(feedback.EvidenceRefs, "capability://skill:offered-only") {
			t.Fatalf("candidate offers must stay inert, got evidence refs %+v", feedback.EvidenceRefs)
		}
	}
	if len(req.Envelopes) != 1 || req.Envelopes[0].Kind != "hook_event" || !strings.Contains(req.Envelopes[0].Summary, "PostToolUse") {
		t.Fatalf("extra envelope summary = %+v", req.Envelopes)
	}
	if req.Envelopes[0].OutputRef != hook.OutputRef {
		t.Fatalf("extra envelope output_ref = %q, want %q", req.Envelopes[0].OutputRef, hook.OutputRef)
	}
	for _, want := range []string{"raw_trace:trace_spine", "context_pack:ctx_distill", "work:work_distill", "receipt:rec_distill", "proof:request-packet", "artifact:request-packet", "hook_event:hook_distill", hook.OutputRef} {
		if !hasString(req.SourceRefs, want) {
			t.Fatalf("source refs missing %q: %v", want, req.SourceRefs)
		}
	}
	raw := []byte(`{
		"schema_version":"compound.distiller.llm_extraction.v0.1",
		"trace_ref":"trace_spine",
		"extractor":"fixture",
		"model":"fixture-model",
		"source_refs":["artifact_request_packet"],
		"candidates":[
			{"kind":"memory","entity":"distiller","predicate":"output_ref_grounding","object":"hook output refs are citeable","confidence":0.91,"evidence_refs":["artifact_request_packet"]}
		]
	}`)
	trace, err := NewGroundedLLMExtractor(req).Extract(raw, DefaultPolicy())
	if err != nil {
		t.Fatalf("grounded output_ref extraction: %v", err)
	}
	if len(trace.Candidates) != 1 || !hasString(trace.Candidates[0].EvidenceRefs, hook.OutputRef) {
		t.Fatalf("output_ref evidence not preserved: %+v", trace.Candidates)
	}
	if trace.Candidates[0].Visibility != "private" {
		t.Fatalf("grounded candidate visibility = %q, want source-derived private", trace.Candidates[0].Visibility)
	}
	if req.PriorCoverage.CurrentClaims != 1 || req.PriorCoverage.Decisions != 1 {
		t.Fatalf("prior coverage = %+v", req.PriorCoverage)
	}
	if !strings.Contains(strings.Join(req.Instructions, "\n"), "Use context/work/receipt as evidence-bearing source material") {
		t.Fatalf("instructions missing source-boundary rule: %v", req.Instructions)
	}
	if !strings.Contains(strings.Join(req.Instructions, "\n"), "Do not infer missed_when or false_triggered_by from skills.candidate offers") {
		t.Fatalf("instructions missing candidate-offer no-inference rule: %v", req.Instructions)
	}
}

func TestBuildLLMExtractionRequestSummarizesTraceEnvelopeSources(t *testing.T) {
	trace := &envelope.TraceEnvelope{
		Header: llmTestHeader(envelope.SVTraceEnvelope, "trace_event_batch", "", ""),
		Source: envelope.TraceSource{
			System:      "os-event",
			Stream:      "control-events",
			SourceID:    "evt_batch",
			SourceKind:  "snapshot.generated",
			EventClass:  "snapshot",
			MutationRaw: "read_only",
		},
		MutationClass:   "read",
		Summary:         "Generated deterministic event snapshot.",
		ToolsModelsUsed: []string{"caller:scheduler"},
		FilesTouched:    []string{"/workspace/snapshots/daily.json"},
		Outputs:         []string{"path:/workspace/snapshots/daily.json"},
		Cost:            envelope.ReceiptCost{ModelCalls: []string{}},
	}
	trace.EvidenceRefs = []string{"path:/workspace/snapshots/daily.json"}

	req, err := BuildLLMExtractionRequestFromSources("event-batch:control-events", LLMExtractionSources{
		Extra: []envelope.Envelope{trace},
	}, DefaultPolicy(), PriorState{})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if countLLMKind(req.Envelopes, "trace_envelope") != 1 {
		t.Fatalf("request envelopes = %+v, want trace envelope", req.Envelopes)
	}
	env := req.Envelopes[0]
	if env.ID != trace.ID || env.SchemaVersion != envelope.SVTraceEnvelope {
		t.Fatalf("trace summary identity = %+v", env)
	}
	if !strings.Contains(env.Summary, "control-events") || !strings.Contains(env.Summary, "Generated deterministic event snapshot.") {
		t.Fatalf("trace summary does not preserve source context: %+v", env)
	}
	if env.Trace == nil {
		t.Fatalf("trace summary missing structured trace context: %+v", env)
	}
	if env.Trace.Source.System != "os-event" ||
		env.Trace.Source.Stream != "control-events" ||
		env.Trace.Source.SourceID != "evt_batch" ||
		env.Trace.Source.EventClass != "snapshot" ||
		env.Trace.Source.MutationRaw != "read_only" ||
		env.Trace.MutationClass != "read" {
		t.Fatalf("trace metadata not preserved: %+v", env.Trace)
	}
	if !hasString(env.Trace.FilesTouched, "/workspace/snapshots/daily.json") ||
		!hasString(env.Trace.ToolsModelsUsed, "caller:scheduler") ||
		!hasString(env.Trace.Outputs, "path:/workspace/snapshots/daily.json") {
		t.Fatalf("trace operational refs not preserved: %+v", env.Trace)
	}
	for _, want := range []string{"trace_envelope:" + trace.ID, "path:/workspace/snapshots/daily.json"} {
		if !hasString(req.SourceRefs, want) {
			t.Fatalf("source refs missing %q: %v", want, req.SourceRefs)
		}
	}
	if !strings.Contains(strings.Join(req.Instructions, "\n"), "TraceEnvelope") {
		t.Fatalf("instructions missing trace envelope boundary: %v", req.Instructions)
	}
}

func TestBuildLLMExtractionRequestRejectsInvalidSourceEnvelope(t *testing.T) {
	badContext := &envelope.ContextPack{
		Header:   llmTestHeader(envelope.SVContextPack, "ctx_bad", "", ""),
		Included: []envelope.IncludedRef{},
		// Excluded must be present by envelope contract.
	}
	_, err := BuildLLMExtractionRequestFromSources("trace_bad", LLMExtractionSources{ContextPack: badContext}, DefaultPolicy(), PriorState{})
	if err == nil || !strings.Contains(err.Error(), "context pack invalid") {
		t.Fatalf("err = %v, want context pack invalid", err)
	}
}

func hasPriorSignalKind(signals []LLMPriorSignal, kind string) bool {
	for _, signal := range signals {
		if signal.Kind == kind {
			return true
		}
	}
	return false
}

func llmTestHeader(schemaVersion, id, workID, contextPackID string) envelope.Header {
	return envelope.Header{
		SchemaVersion: schemaVersion,
		ID:            id,
		CreatedAt:     "2026-06-29T12:00:00Z",
		Producer:      envelope.Producer{Kind: "test", Name: "compound"},
		IDs:           envelope.IDs{WorkID: workID, ContextPackID: contextPackID},
		Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
		Privacy:       "private",
		Risk:          "tier1",
		EvidenceRefs:  []string{},
		Uncertainty:   envelope.Uncertainty{Level: "known"},
	}
}

// TestDistillerRequestFromFindingRecordsPrivateBodyReferenceOnly is the ref-only invariant:
// a request built from a finding ID never persists the body in the request artifact. Bodies
// are materialized only into the hydrated in-memory copy (via RawTrace); the stored request
// is ref-only and source_privacy always reads `private`.
func TestDistillerRequestFromFindingRecordsPrivateBodyReferenceOnly(t *testing.T) {
	home := t.TempDir()
	rec, err := finding.Record(home, finding.Finding{Topic: "bridge-test", Body: "body-must-not-appear-in-request", WorkID: "work_bridge"}, t0)
	if err != nil {
		t.Fatalf("record finding: %v", err)
	}

	req, err := BuildLLMExtractionRequestFromFindings(home, "trace_bridge_test", []string{rec.ID}, DefaultPolicy(), PriorState{})
	if err != nil {
		t.Fatalf("build request from finding: %v", err)
	}
	if req.SourcePrivacy != "private" {
		t.Fatalf("source_privacy = %q, want private", req.SourcePrivacy)
	}
	if !findingSrcRefHasID(req.SourceRefs, rec.ID) {
		t.Fatalf("source_refs missing finding ref: %+v", req.SourceRefs)
	}
	if !containsString(req.SourceRefs, "work:"+rec.WorkID) {
		t.Fatalf("source_refs missing work ref: %+v", req.SourceRefs)
	}
	if len(req.FindingRefs) != 1 {
		t.Fatalf("finding_refs = %+v, want exactly one entry", req.FindingRefs)
	}
	if req.FindingRefs[0].ID != rec.ID || req.FindingRefs[0].CID != rec.CID {
		t.Fatalf("finding_ref mismatch: %+v", req.FindingRefs[0])
	}
	if req.RawTrace != "" {
		t.Fatalf("stored request RawTrace must be empty, got %q", req.RawTrace)
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw := string(data)
	if strings.Contains(raw, "body-must-not-appear-in-request") {
		t.Fatalf("request artifact must not contain the finding body: %s", raw)
	}
}

// TestFindingRequestHydratesOnlyForModelExecutionAndValidatesCID proves the body is materialized
// only in RawTrace on the in-memory copy returned to the model — the recorded request stays
// bodyless; CID mismatches fail closed.
func TestFindingRequestHydratesOnlyForModelExecutionAndValidatesCID(t *testing.T) {
	home := t.TempDir()
	rec, err := finding.Record(home, finding.Finding{Topic: "hydrate-test", Body: "hydrate-payload", WorkID: "work_hydrate"}, t0)
	if err != nil {
		t.Fatalf("record finding: %v", err)
	}

	req, err := BuildLLMExtractionRequestFromFindings(home, "trace_hydrate_test", []string{rec.ID}, DefaultPolicy(), PriorState{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if req.RawTrace != "" {
		t.Fatalf("stored request RawTrace must be empty, got %q", req.RawTrace)
	}

	hydrated, err := HydrateLLMExtractionRequest(home, req)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if !strings.Contains(hydrated.RawTrace, "hydrate-payload") {
		t.Fatalf("hydrated RawTrace must contain finding body, got %q", hydrated.RawTrace)
	}
	if !strings.Contains(hydrated.RawTrace, "[finding:"+rec.ID) {
		t.Fatalf("hydrated RawTrace must label finding: %q", hydrated.RawTrace)
	}
	if req.RawTrace != "" {
		t.Fatalf("hydrate must not mutate input request's RawTrace")
	}

	empty := req
	empty.FindingRefs = nil
	if _, err := HydrateLLMExtractionRequest(home, empty); err == nil {
		t.Fatalf("missing finding refs must fail closed")
	}

	missing := req
	missing.FindingRefs = []FindingRef{{ID: "find_does_not_exist", CID: "sha256:" + strings.Repeat("0", 64)}}
	if _, err := HydrateLLMExtractionRequest(home, missing); err == nil {
		t.Fatalf("nonexistent finding id must fail closed")
	}

	bcid := req
	bcid.FindingRefs = []FindingRef{{ID: rec.ID, CID: "sha256:" + strings.Repeat("f", 64)}}
	if _, err := HydrateLLMExtractionRequest(home, bcid); err == nil || !strings.Contains(err.Error(), "requested CID") {
		t.Fatalf("CID mismatch must fail closed: %v", err)
	}

	unsupported := req
	unsupported.FindingRefs = []FindingRef{{ID: "trace_xyz", CID: "sha256:" + strings.Repeat("a", 64)}}
	if _, err := HydrateLLMExtractionRequest(home, unsupported); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unsupported ref kind must fail closed: %v", err)
	}
}

// TestFindingRequestReplayParksCandidateWithoutClaim proves replay of a stored request through a
// model citation of `finding:<id>` produces a parked candidate, never a Claim — the live
// distiller policy blocks promotion.
func TestFindingRequestReplayParksCandidateWithoutClaim(t *testing.T) {
	home, store := setup(t)
	rec, err := finding.Record(home, finding.Finding{Topic: "replay-park", Body: "replay-park-payload", WorkID: "work_replay_park"}, t0)
	if err != nil {
		t.Fatalf("record finding: %v", err)
	}
	req, err := BuildLLMExtractionRequestFromFindings(home, "trace_replay_park", []string{rec.ID}, DefaultPolicy(), PriorState{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	recorded, err := RecordLLMExtractionRequest(home, req)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	loaded, err := LoadLLMExtractionRequest(home, recorded.Ref)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	hydration, err := HydrateLLMExtractionRequest(home, loaded)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}

	raw := []byte(`{
		"schema_version":"` + LLMExtractionSchemaVersion + `",
		"trace_ref":"` + loaded.TraceRef + `",
		"source_refs":["finding:` + rec.ID + `"],
		"candidates":[{"kind":"memory","entity":"work_replay_park","predicate":"observes","object":"replay-park observation","confidence":0.9,"evidence_refs":["finding:` + rec.ID + `"]}]
	}`)

	res, err := Run(home, store, raw, DefaultPolicy(), NewGroundedLLMExtractor(hydration), t0)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Decisions) != 1 {
		t.Fatalf("decisions = %d, want exactly 1", len(res.Decisions))
	}
	dec := res.Decisions[0]
	if dec.ClaimID != "" {
		t.Fatalf("live distiller policy must not promote finding-grounded memory; got claim %s", dec.ClaimID)
	}
	if dec.Verdict != Park {
		t.Fatalf("finding-grounded memory must park, got verdict=%s", dec.Verdict)
	}
}

// TestFindingRequestRejectsInventedEvidenceRef proves a model citing a `finding:` ref it did
// not receive closes fast.
func TestFindingRequestRejectsInventedEvidenceRef(t *testing.T) {
	home := t.TempDir()
	rec, err := finding.Record(home, finding.Finding{Topic: "reject-invented", Body: "allowed-payload", WorkID: "work_reject"}, t0)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	req, err := BuildLLMExtractionRequestFromFindings(home, "trace_reject_invented", []string{rec.ID}, DefaultPolicy(), PriorState{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	badResponse := []byte(`{
		"schema_version":"` + LLMExtractionSchemaVersion + `",
		"trace_ref":"` + req.TraceRef + `",
		"source_refs":["finding:` + rec.ID + `"],
		"candidates":[{"kind":"memory","entity":"work_reject","predicate":"observes","object":"fabricated finding reference","confidence":0.9,"evidence_refs":["finding:find_does_not_exist"]}]
	}`)
	_, err = NewGroundedLLMExtractor(req).Extract(badResponse, DefaultPolicy())
	if err == nil || !strings.Contains(err.Error(), "evidence_ref") {
		t.Fatalf("invented evidence ref must fail closed, got %v", err)
	}
}

// TestDistillerRequestFromFindingPreservesProducerEvidenceRefs asserts BuildLLMExtractionRequestFromFindings
// preserves every finding.EvidenceRefs entry into request.SourceRefs (producer's chain), not
// only the synthesised finding:/work: refs.
func TestDistillerRequestFromFindingPreservesProducerEvidenceRefs(t *testing.T) {
	home := t.TempDir()
	rec, err := finding.Record(home, finding.Finding{
		Topic:        "preserve-evidence",
		Body:         "preserve-evidence-body",
		WorkID:       "work_preserve",
		EvidenceRefs: []string{"receipt:abc123", "context_pack:cp_xyz", "finding:" + recGeneratedEvidenceID()},
	}, t0)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	req, err := BuildLLMExtractionRequestFromFindings(home, "trace_preserve", []string{rec.ID}, DefaultPolicy(), PriorState{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, want := range []string{"finding:" + rec.ID, "work:" + "work_preserve", "receipt:abc123", "context_pack:cp_xyz"} {
		if !containsString(req.SourceRefs, want) {
			t.Fatalf("source_refs missing %q: %+v", want, req.SourceRefs)
		}
	}
}

// TestFindingRequestRejectsTamperedFindingFile is the CID-integrity falsifier: a finding file
// whose on-disk CID no longer matches the body must fail closed at hydration time, even if
// the request's recorded ref CID still matches the on-disk CID (the hydrate path always
// recomputes via finding.CID(body)).
func TestFindingRequestRejectsTamperedFindingFile(t *testing.T) {
	home := t.TempDir()
	rec, err := finding.Record(home, finding.Finding{Topic: "tamper", Body: "original-body", WorkID: "work_tamper"}, t0)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	req, err := BuildLLMExtractionRequestFromFindings(home, "trace_tamper", []string{rec.ID}, DefaultPolicy(), PriorState{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Tamper the file: overwrite body and the persisted CID to remain internally
	// consistent, but the recompute-and-compare path must still fail closed because the
	// request-ref CID agrees with the on-disk CID but disagrees with the recomputed actual.
	path := filepath.Join(home, finding.Subdir, rec.ID+".json")
	tampered := `{
		"id": "` + rec.ID + `",
		"cid": "sha256:` + strings.Repeat("c", 64) + `",
		"topic": "` + rec.Topic + `",
		"body": "tampered-body",
		"work_id": "work_tamper",
		"source_class": "evidence",
		"created_at": "` + rec.CreatedAt + `"
	}`
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatalf("write tampered: %v", err)
	}
	if _, err := HydrateLLMExtractionRequest(home, req); err == nil || !strings.Contains(err.Error(), "tampered") {
		t.Fatalf("tampered body must fail closed at hydrate, got %v", err)
	}
}

// TestDistillerExtractKeepsStoredRequestRefOnlySeesHydratedModel is the audit invariant:
// BuildExecutionRouteEvidence MUST be constructed from the ref-only storedReq — its
// SourceRefs stay free of bodies — while the hydrated `req` is what flows to the provider.
// Since cmdDistillerExtract shells through ExtractWithOpenAICompatible which would actually
// call the network, we exercise the same seam via BuildExecutionRouteEvidence directly so we
// can prove it sees the stored-only refs.
func TestDistillerExtractKeepsStoredRequestRefOnlySeesHydratedModel(t *testing.T) {
	home := t.TempDir()
	rec, err := finding.Record(home, finding.Finding{Topic: "route-sep", Body: "route-sep-body", WorkID: "work_route_sep"}, t0)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	storedReq, err := BuildLLMExtractionRequestFromFindings(home, "trace_route_sep", []string{rec.ID}, DefaultPolicy(), PriorState{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	hydratedReq, err := HydrateLLMExtractionRequest(home, storedReq)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}

	// The hydrated copy carries the body in RawTrace; the stored form must not.
	if hydratedReq.RawTrace == "" || !strings.Contains(hydratedReq.RawTrace, "route-sep-body") {
		t.Fatalf("hydrated RawTrace missing body: %q", hydratedReq.RawTrace)
	}
	if storedReq.RawTrace != "" {
		t.Fatalf("storedReq must stay ref-only (RawTrace empty), got %q", storedReq.RawTrace)
	}

	report := LiveExtractionReport{
		SchemaVersion: LiveExtractionSchemaVersion,
		Provider:      "openai-compatible",
		Model:         "fixture",
		Called:        true,
	}
	routeStored, err := BuildExecutionRouteEvidence(storedReq, report, t0)
	if err != nil {
		t.Fatalf("build stored route: %v", err)
	}
	routeHydrated, err := BuildExecutionRouteEvidence(hydratedReq, report, t0)
	if err != nil {
		t.Fatalf("build hydrated route: %v", err)
	}
	if len(routeStored.EvidenceRefs) != len(routeHydrated.EvidenceRefs) {
		t.Fatalf("route evidence len differs: stored=%+v hydrated=%+v", routeStored.EvidenceRefs, routeHydrated.EvidenceRefs)
	}
	for i := range routeStored.EvidenceRefs {
		if routeStored.EvidenceRefs[i] != routeHydrated.EvidenceRefs[i] {
			t.Fatalf("route evidence refs diverge (route/ref-only sees bodies?): stored=%+v hydrated=%+v", routeStored.EvidenceRefs, routeHydrated.EvidenceRefs)
		}
	}
}

func recGeneratedEvidenceID() string {
	return "find_evidence" + strings.Repeat("0", 8)
}

// TestHydrateRequiresCIDFalsifier is the missing-CID falsifier: a FindingRef with an empty
// CID must fail closed at hydrate even though the requested ref CID is technically
// present-but-empty (BuildLLMExtractionRequestFromFindings always populates it, so an
// empty fr.CID indicates a request that was constructed outside the build path and must
// not be materialised).
func TestHydrateRequiresCIDFalsifier(t *testing.T) {
	home := t.TempDir()
	rec, err := finding.Record(home, finding.Finding{Topic: "missing-cid", Body: "missing-cid-body", WorkID: "work_missing_cid"}, t0)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	req := LLMExtractionRequest{
		SchemaVersion: LLMExtractionSchemaVersion,
		TraceRef:      "trace_missing_cid",
		FindingRefs:   []FindingRef{{ID: rec.ID, CID: ""}},
	}
	if _, err := HydrateLLMExtractionRequest(home, req); err == nil || !strings.Contains(err.Error(), "CID is empty") {
		t.Fatalf("missing requested CID must fail closed: %v", err)
	}
}

// keep the import of filepath referenced from the tampered-body test: it is used above to
// construct the path of the on-disk finding file we tamper with.
var _ = filepath.Separator

func findingSrcRefHasID(set []string, id string) bool {
	want := "finding:" + id
	for _, s := range set {
		if s == want {
			return true
		}
	}
	return false
}
