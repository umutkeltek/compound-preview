package contract

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/breadth"
	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/envelope"
	"compound/internal/mem"
)

type countingRuntimeEngine struct {
	queryCalls int
}

func (e *countingRuntimeEngine) Capabilities() mem.EngineCapabilities {
	return mem.EngineCapabilities{Name: "counting-runtime", Breadth: true}
}

func (e *countingRuntimeEngine) Index([]mem.Claim, int64) error { return nil }

func (e *countingRuntimeEngine) Query(string, string, []string, int) ([]mem.Hit, error) {
	e.queryCalls++
	return nil, nil
}

func TestMaterializeRuntimeProjectsOneSharedSnapshot(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 16, 21, 0, 0, 0, time.UTC)
	writeLease(t, home, "work_runtime_materialization", "codex-test", now.Add(-time.Minute))

	engine := &countingRuntimeEngine{}
	priorFactory := breadthEngineFactory
	breadthEngineFactory = func(string) (mem.Engine, func()) { return engine, func() {} }
	t.Cleanup(func() { breadthEngineFactory = priorFactory })
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "counting-runtime")

	in := Input{
		Harness: "codex", Cwd: "/repo/product", Task: "shared runtime materialization",
		RepoKind: "product", RepoKindVia: "flag", Mode: "execute",
	}
	got, err := MaterializeRuntime(in, DefaultPolicy(), home, now)
	if err != nil {
		t.Fatal(err)
	}
	if engine.queryCalls != 1 {
		t.Fatalf("shared materialization breadth queries = %d, want 1", engine.queryCalls)
	}
	if got.ContextPack.Peers == nil || len(got.ContextPack.Peers.ActiveLeases) != 1 {
		t.Fatalf("combined pack lost pack-only peers projection: %+v", got.ContextPack.Peers)
	}

	engine.queryCalls = 0
	wantContract, err := CompileWithRuntime(in, DefaultPolicy(), home, now)
	if err != nil {
		t.Fatal(err)
	}
	if engine.queryCalls != 1 {
		t.Fatalf("CompileWithRuntime breadth queries = %d, want 1", engine.queryCalls)
	}
	engine.queryCalls = 0
	wantPack, wantTrace, err := PackWithCapabilitiesTraced(in, DefaultPolicy(), home, now)
	if err != nil {
		t.Fatal(err)
	}
	if engine.queryCalls != 1 {
		t.Fatalf("PackWithCapabilitiesTraced breadth queries = %d, want 1", engine.queryCalls)
	}

	assertRuntimeJSONEqual(t, "effective contract", got.EffectiveContract, wantContract)
	assertRuntimeJSONEqual(t, "context pack", got.ContextPack, wantPack)
	if !reflect.DeepEqual(got.EngineDependencyTrace, wantTrace) {
		t.Fatalf("engine trace drift:\ngot  %+v\nwant %+v", got.EngineDependencyTrace, wantTrace)
	}
	if got.ContextPack.Retrieval == nil || got.ContextPack.Retrieval.Level != envelope.RetrievalLevelUsed || got.ContextPack.Retrieval.ObservedEngine != "counting-runtime" {
		t.Fatalf("shared materialization did not project its engine trace into ContextPack: trace=%+v retrieval=%+v", got.EngineDependencyTrace, got.ContextPack.Retrieval)
	}

	sealed, err := SealRuntimeMaterialization(got)
	if err != nil {
		t.Fatal(err)
	}
	alternate := got
	alternate.ContextPack.Retrieval = &envelope.RetrievalPosture{
		Level:     envelope.RetrievalLevelNotAttempted,
		Fallback:  envelope.RetrievalFallbackLedgerRanker,
		Authority: envelope.RetrievalAuthorityCompoundLedger,
	}
	alternateSealed, err := SealRuntimeMaterialization(alternate)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.ContextPack.ID == alternateSealed.ContextPack.ID {
		t.Fatalf("sealed ContextPack id did not bind exact retrieval posture: %s", sealed.ContextPack.ID)
	}
	if sealed.EffectiveContract.ID != alternateSealed.EffectiveContract.ID || sealed.ContextPack.IDs.ContractID != alternateSealed.ContextPack.IDs.ContractID {
		t.Fatalf("posture-only change split shared contract pair: primary=%s/%s alternate=%s/%s",
			sealed.EffectiveContract.ID, sealed.ContextPack.IDs.ContractID,
			alternateSealed.EffectiveContract.ID, alternateSealed.ContextPack.IDs.ContractID)
	}
}

func assertRuntimeJSONEqual(t *testing.T, label string, got, want any) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("%s bytes drifted:\ngot  %s\nwant %s", label, gotJSON, wantJSON)
	}
}

type runtimeSemanticEmbedder struct {
	task      string
	taskCalls int
}

func (e *runtimeSemanticEmbedder) Identity() string { return "stub:runtime:2:f32" }
func (e *runtimeSemanticEmbedder) Dim() int         { return 2 }
func (e *runtimeSemanticEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		if text == e.task {
			e.taskCalls++
		}
		if text == e.task || strings.Contains(text, "deployment automation") || strings.Contains(text, "Worldwide Delivery") {
			out = append(out, []float32{1, 0})
		} else {
			out = append(out, []float32{0, 1})
		}
	}
	return out, nil
}

func TestMaterializeRuntimeReadOnlySharesInProcessSemanticTaskEmbedding(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := store.Capture([]mem.Candidate{{
		Entity: "delivery-system", Predicate: "architecture", Object: "deployment automation releases verified artifacts",
		Confidence: 0.99, Derivation: "deterministic", ClaimType: "architecture",
		PromotionBasis: candidates.BasisRuntimeProof, SourceTraceRef: "trace_runtime_semantic",
		SnippetHash: "sha256:runtime-semantic", Visibility: "operational",
	}}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := store.Adjudicate(captured[0].ID, candidates.Promote, "test", fixedNow)
	if err != nil || promoted.Claim == nil {
		t.Fatalf("promote semantic claim: result=%+v err=%v", promoted, err)
	}
	graph, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	const capabilityID = "skill:worldwide-delivery"
	if err := graph.Add(capgraph.Node{
		ID: capabilityID, Kind: "skill", Name: "Worldwide Delivery",
		Triggers: []string{"deployment automation"}, Weight: 1,
	}); err != nil {
		t.Fatal(err)
	}

	task := "ship this faster"
	embedder := &runtimeSemanticEmbedder{task: task}
	priorResolver := breadthEmbedderResolver
	breadthEmbedderResolver = func(string) (breadth.EmbedderAdapter, bool) { return embedder, true }
	t.Cleanup(func() { breadthEmbedderResolver = priorResolver })
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "")

	got, err := MaterializeRuntime(Input{
		Harness: "codex", Cwd: "/repo/product", Task: task,
		RepoKind: "product", RepoKindVia: "flag", Mode: "execute", ReadOnlyRetrieval: true,
	}, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if embedder.taskCalls != 1 {
		t.Fatalf("exact semantic task embeddings = %d, want 1 shared by memory and capgraph", embedder.taskCalls)
	}
	if !contains(got.ContextPack.ActiveMemory, "claim://"+promoted.Claim.ID) {
		t.Fatalf("semantic-only memory missing: %v", got.ContextPack.ActiveMemory)
	}
	if !hasIncluded(got.ContextPack, "capability://"+capabilityID) {
		t.Fatalf("semantic-only capability missing: %+v", got.ContextPack.CapabilityOffers)
	}
	if got.EngineDependencyTrace.Outcome != EngineOutcomeUsed || got.EngineDependencyTrace.ObservedEngine != breadth.Name {
		t.Fatalf("in-process semantic trace = %+v", got.EngineDependencyTrace)
	}
}

func TestMaterializeRuntimeOrdinaryKeepsRawCapgraphEmbedderAndSidecar(t *testing.T) {
	home := t.TempDir()
	graph, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Add(capgraph.Node{
		ID: "skill:ordinary-semantic", Kind: "skill", Name: "Ordinary Semantic",
		Triggers: []string{"deployment automation"}, Weight: 1,
	}); err != nil {
		t.Fatal(err)
	}

	task := "ship ordinary orientation"
	embedder := &runtimeSemanticEmbedder{task: task}
	priorResolver := breadthEmbedderResolver
	breadthEmbedderResolver = func(string) (breadth.EmbedderAdapter, bool) { return embedder, true }
	t.Cleanup(func() { breadthEmbedderResolver = priorResolver })
	if resolved := resolveTaskEmbedder(home, task); resolved != embedder {
		t.Fatalf("ordinary capgraph embedder = %T, want raw configured embedder %T", resolved, embedder)
	}

	engine := &countingRuntimeEngine{}
	factoryCalls := 0
	priorFactory := breadthEngineFactory
	breadthEngineFactory = func(string) (mem.Engine, func()) {
		factoryCalls++
		return engine, func() {}
	}
	t.Cleanup(func() { breadthEngineFactory = priorFactory })
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "counting-runtime")

	got, err := MaterializeRuntime(Input{
		Harness: "codex", Cwd: "/repo/product", Task: task,
		RepoKind: "product", RepoKindVia: "flag", Mode: "execute",
	}, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if embedder.taskCalls != 1 {
		t.Fatalf("ordinary capgraph task embeddings = %d, want 1 raw embedder call", embedder.taskCalls)
	}
	if factoryCalls != 1 || engine.queryCalls != 1 {
		t.Fatalf("ordinary memory retrieval factory/query calls = %d/%d, want sidecar 1/1", factoryCalls, engine.queryCalls)
	}
	if got.EngineDependencyTrace.ObservedEngine != "counting-runtime" {
		t.Fatalf("ordinary engine trace = %+v, want sidecar identity", got.EngineDependencyTrace)
	}
}
