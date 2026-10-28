package distiller_test

import (
	"strings"
	"testing"
	"time"

	"compound/internal/contract"
	"compound/internal/distiller"
	"compound/internal/envelope"
	"compound/internal/harness"
	"compound/internal/hook"
	"compound/internal/mem"
	"compound/internal/policy"
	"compound/internal/runattempt"
	"compound/internal/work"
)

var runtimeSourcesT0 = time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)

func TestLoadRuntimeSourcesFromClosedHookWork(t *testing.T) {
	homePath := t.TempDir()
	memStore, err := mem.Open(homePath)
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	deps := hook.Deps{
		Registry: harness.DefaultRegistry(),
		Orient:   contract.DefaultPolicy(),
		Policy:   policy.DefaultConfig(),
		HomePath: homePath,
		Store:    workStore,
	}

	start, err := hook.Run(hook.Fixture{
		Hook:     "user-prompt-submit",
		Harness:  "codex",
		Cwd:      "/repo/productX",
		Task:     "extract reusable pricing workflow",
		RepoKind: "product",
		Prompt:   "prepare a pricing experiment and remember only durable lessons",
	}, deps, runtimeSourcesT0)
	if err != nil {
		t.Fatalf("user prompt hook: %v", err)
	}
	ctxReq := findProduced[*envelope.ContextRequest](t, start.Produced)
	ec := findProduced[*envelope.EffectiveContract](t, start.Produced)
	pack := findProduced[*envelope.ContextPack](t, start.Produced)
	w := findProduced[*envelope.WorkEnvelope](t, start.Produced)
	if w.IDs.ContextPackID != pack.ID {
		t.Fatalf("work context_pack_id = %q, want %q", w.IDs.ContextPackID, pack.ID)
	}

	if _, err := workStore.Advance(w.ID, work.AdvanceInput{To: "ready", Note: "admit prompt work"}, runtimeSourcesT0); err != nil {
		t.Fatalf("advance ready: %v", err)
	}
	if _, err := workStore.Claim(w.ID, "codex", runtimeSourcesT0); err != nil {
		t.Fatalf("claim: %v", err)
	}
	runPricingDistillAttempt, err := runattempt.Open(homePath).Mint(w.ID, "codex", "codex", runtimeSourcesT0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workStore.Advance(w.ID, work.AdvanceInput{To: "running", RunAttemptIDs: []string{runPricingDistillAttempt.ID}}, runtimeSourcesT0); err != nil {
		t.Fatalf("advance running: %v", err)
	}
	if _, err := workStore.Advance(w.ID, work.AdvanceInput{To: "verifying", ArtifactID: "artifact_pricing_summary", VerificationID: "ver_pricing_summary"}, runtimeSourcesT0); err != nil {
		t.Fatalf("advance verifying: %v", err)
	}
	closed, err := hook.Run(hook.Fixture{
		Hook:     "session-end",
		Harness:  "codex",
		WorkID:   w.ID,
		MoveType: "outward_delta",
		Outcome:  "completed",
		Summary:  "pricing experiment prepared with reusable workflow notes",
	}, deps, runtimeSourcesT0)
	if err != nil {
		t.Fatalf("session end hook: %v", err)
	}
	rec := findProduced[*envelope.Receipt](t, closed.Produced)
	tr := findProduced[*envelope.Transition](t, closed.Produced)

	src, err := distiller.LoadRuntimeSources(homePath, w.ID)
	if err != nil {
		t.Fatalf("load runtime sources: %v", err)
	}
	if src.Work == nil || src.Work.ID != w.ID || src.Work.State != "completed" {
		t.Fatalf("loaded work = %+v, want completed %s", src.Work, w.ID)
	}
	if src.ContextPack == nil || src.ContextPack.ID != pack.ID {
		t.Fatalf("loaded context pack = %+v, want %s", src.ContextPack, pack.ID)
	}
	if src.Receipt == nil || src.Receipt.ID != rec.ID {
		t.Fatalf("loaded receipt = %+v, want %s", src.Receipt, rec.ID)
	}
	if countEnvelopeKind(src.Extra, "hook_event") < 2 ||
		countEnvelopeKind(src.Extra, "context_request") != 1 ||
		countEnvelopeKind(src.Extra, "effective_contract") != 1 ||
		countEnvelopeKind(src.Extra, "transition") != 1 {
		t.Fatalf("loaded extra = %+v, want hook events, context request, effective contract, plus one transition", src.Extra)
	}

	prior, err := distiller.LoadPriorStateFromHome(homePath, memStore, distiller.DefaultPolicy())
	if err != nil {
		t.Fatalf("prior: %v", err)
	}
	req, err := distiller.BuildLLMExtractionRequestFromSources("runtime:"+w.ID, src, distiller.DefaultPolicy(), prior)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for _, want := range []string{
		"work:" + w.ID,
		"context_pack:" + pack.ID,
		"receipt:" + rec.ID,
		"context_request:" + ctxReq.ID,
		"effective_contract:" + ec.ID,
		"transition:" + tr.ID,
	} {
		if !containsString(req.SourceRefs, want) {
			t.Fatalf("request source refs missing %q: %v", want, req.SourceRefs)
		}
	}
	if req.RawTrace != "" {
		t.Fatalf("runtime loader should not invent raw trace: %q", req.RawTrace)
	}
	if req.Work == nil || req.Work.State != "completed" {
		t.Fatalf("request work summary missing completed work: %+v", req.Work)
	}
	if containsString(req.Work.ActionIntentIDs, "run_pricing_distill") {
		t.Fatalf("ActionIntentIDs must not contain legacy run id after S1; got %v", req.Work.ActionIntentIDs)
	}
	if !containsString(req.Work.RunAttemptIDs, runPricingDistillAttempt.ID) {
		t.Fatalf("RunAttemptIDs missing %q, got %v", runPricingDistillAttempt.ID, req.Work.RunAttemptIDs)
	}
	if req.Context == nil || !strings.Contains(req.Context.SummaryMD, "product") {
		t.Fatalf("request context summary incomplete: %+v", req.Context)
	}
	if req.Receipt == nil || req.Receipt.StateDelta.Summary == "" {
		t.Fatalf("request receipt summary incomplete: %+v", req.Receipt)
	}
	if countLLMKind(req.Envelopes, "hook_event") < 2 ||
		countLLMKind(req.Envelopes, "context_request") != 1 ||
		countLLMKind(req.Envelopes, "effective_contract") != 1 ||
		countLLMKind(req.Envelopes, "transition") != 1 {
		t.Fatalf("request extra envelopes = %+v, want hook events, context request, effective contract, plus transition %s", req.Envelopes, tr.ID)
	}
	if !containsString(req.SourceRefs, "hook_event:"+start.Event.ID) || !containsString(req.SourceRefs, "hook_event:"+closed.Event.ID) {
		t.Fatalf("request source refs missing hook events %q/%q: %v", start.Event.ID, closed.Event.ID, req.SourceRefs)
	}
}

func findProduced[T envelope.Envelope](t *testing.T, produced []envelope.Envelope) T {
	t.Helper()
	var zero T
	for _, item := range produced {
		if typed, ok := item.(T); ok {
			return typed
		}
	}
	t.Fatalf("produced does not include %T: %v", zero, produced)
	return zero
}

func countEnvelopeKind(items []envelope.Envelope, kind string) int {
	var count int
	for _, item := range items {
		if item.Kind() == kind {
			count++
		}
	}
	return count
}

func countLLMKind(items []distiller.LLMEnvelope, kind string) int {
	var count int
	for _, item := range items {
		if item.Kind == kind {
			count++
		}
	}
	return count
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
