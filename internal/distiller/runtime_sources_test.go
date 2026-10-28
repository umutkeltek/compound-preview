package distiller

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/work"
)

func TestLoadRuntimeSourcesRejectsBrokenClosedWork(t *testing.T) {
	homePath, _ := setup(t)
	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	w, err := workStore.Create(work.CreateInput{
		Owner:            "codex",
		SuccessPredicate: "broken close chain is rejected",
	}, t0)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if _, err := workStore.Claim(w.ID, "codex", t0); err != nil {
		t.Fatalf("claim work: %v", err)
	}
	if _, err := workStore.Advance(w.ID, work.AdvanceInput{Actor: "codex", To: "running"}, t0); err != nil {
		t.Fatalf("advance work: %v", err)
	}
	closed, err := workStore.Close(w.ID, work.CloseInput{Actor: "codex", Outcome: "completed", MoveType: "audit_only", Summary: "closed before corruption"}, t0)
	if err != nil {
		t.Fatalf("close work: %v", err)
	}
	w = closed.Work
	w.IDs.TransitionID = ""
	data, err := json.MarshalIndent(&w, "", "  ")
	if err != nil {
		t.Fatalf("marshal broken work: %v", err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "work", "work", w.ID+".json"), append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write broken work: %v", err)
	}
	_, err = LoadRuntimeSources(homePath, w.ID)
	if err == nil || !strings.Contains(err.Error(), "missing transition_id") {
		t.Fatalf("err = %v, want missing transition_id", err)
	}
}

func TestHookSideEffectJSONPathRejectsUnsafeRefs(t *testing.T) {
	homePath := t.TempDir()
	if _, ok, err := hookSideEffectJSONPath(homePath, "context/injections/hook_123.md"); err != nil || ok {
		t.Fatalf("markdown ref ok=%v err=%v, want ignored", ok, err)
	}
	if _, ok, err := hookSideEffectJSONPath(homePath, "ledger/claims.jsonl"); err != nil || ok {
		t.Fatalf("jsonl ref ok=%v err=%v, want ignored", ok, err)
	}
	if _, _, err := hookSideEffectJSONPath(homePath, filepath.Join("..", "escape.json")); err == nil {
		t.Fatal("expected escaping ref to fail")
	}
	if _, _, err := hookSideEffectJSONPath(homePath, filepath.Join(homePath, "events", "hook.json")); err == nil {
		t.Fatal("expected absolute ref to fail")
	}
	path, ok, err := hookSideEffectJSONPath(homePath, filepath.Join("context", "requests", "ctxreq_123.json"))
	if err != nil || !ok {
		t.Fatalf("safe ref ok=%v err=%v, want accepted", ok, err)
	}
	if want := filepath.Join(homePath, "context", "requests", "ctxreq_123.json"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
}

func TestLoadHookSideEffectEnvelopesKeepsGenericJSONPathsSeparate(t *testing.T) {
	homePath := t.TempDir()
	generic := testTraceEnvelope("trace_generic_side_effect", "evt_generic", "Loaded from a generic side-effect path.")
	writeRuntimeEnvelope(t, filepath.Join(homePath, "rec_report.json"), generic)
	store, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatalf("open event store: %v", err)
	}

	loaded, err := loadHookSideEffectEnvelopes(homePath, store, []envelope.HookEvent{{
		Header:      envelope.Header{ID: "hook_generic_side_effect"},
		SideEffects: []string{"rec_report.json"},
	}}, map[string]bool{})
	if err != nil {
		t.Fatalf("load generic side effect: %v", err)
	}
	if len(loaded) != 1 || loaded[0].Kind() != "trace_envelope" {
		t.Fatalf("loaded = %+v, want generic trace envelope", loaded)
	}
}

func TestLoadTraceEnvelopesFromRuntimeEvents(t *testing.T) {
	homePath := t.TempDir()
	first := testTraceEnvelope("trace_runtime_a", "evt_a", "Generated first snapshot.")
	second := testTraceEnvelope("trace_runtime_b", "evt_b", "Generated second snapshot.")
	writeRuntimeEnvelope(t, filepath.Join(homePath, "events", first.ID+".json"), first)
	writeRuntimeEnvelope(t, filepath.Join(homePath, "events", second.ID+".json"), second)

	loaded, err := LoadTraceEnvelopes(homePath, []string{
		first.ID,
		"compound://events/" + first.ID,
		" events/" + second.ID + ".json ",
	})
	if err != nil {
		t.Fatalf("load trace envelopes: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded traces = %d, want 2", len(loaded))
	}
	if loaded[0].Kind() != "trace_envelope" || loaded[1].Kind() != "trace_envelope" {
		t.Fatalf("loaded kinds = %s/%s", loaded[0].Kind(), loaded[1].Kind())
	}

	_, err = LoadTraceEnvelopes(homePath, []string{"trace_missing"})
	if err == nil || !strings.Contains(err.Error(), "load trace trace_missing") {
		t.Fatalf("err = %v, want missing trace error", err)
	}
}

func testTraceEnvelope(id, sourceID, summary string) *envelope.TraceEnvelope {
	return &envelope.TraceEnvelope{
		Header: envelope.Header{
			SchemaVersion: envelope.SVTraceEnvelope,
			ID:            id,
			CreatedAt:     "2026-06-29T12:00:00Z",
			Producer:      envelope.Producer{Kind: "test", Name: "compound"},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier0",
			EvidenceRefs:  []string{"path:/workspace/events.json"},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Source: envelope.TraceSource{
			System:     "os-event",
			Stream:     "control-events",
			SourceID:   sourceID,
			SourceKind: "snapshot.generated",
		},
		MutationClass:   "read",
		Summary:         summary,
		ToolsModelsUsed: []string{"caller:scheduler"},
		FilesTouched:    []string{"/workspace/events.json"},
		Outputs:         []string{"path:/workspace/events.json"},
		Cost:            envelope.ReceiptCost{ModelCalls: []string{}},
	}
}

func writeRuntimeEnvelope(t *testing.T, path string, env envelope.Envelope) {
	t.Helper()
	if err := env.Validate(); err != nil {
		t.Fatalf("invalid test envelope: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(env, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func countLLMKind(items []LLMEnvelope, kind string) int {
	var count int
	for _, item := range items {
		if item.Kind == kind {
			count++
		}
	}
	return count
}
