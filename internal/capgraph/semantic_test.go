package capgraph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type semanticStubEmbedder struct {
	identity   string
	dim        int
	vectors    map[string][]float32
	defaultVec []float32
	err        error
	calls      [][]string
}

func newSemanticStub(identity string) *semanticStubEmbedder {
	return &semanticStubEmbedder{
		identity:   identity,
		dim:        2,
		vectors:    map[string][]float32{},
		defaultVec: []float32{0, 1},
	}
}

func (s *semanticStubEmbedder) Identity() string { return s.identity }

func (s *semanticStubEmbedder) Dim() int { return s.dim }

func (s *semanticStubEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	copied := append([]string(nil), texts...)
	s.calls = append(s.calls, copied)
	if s.err != nil {
		return nil, s.err
	}
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		if vec, ok := s.vectors[text]; ok {
			out = append(out, append([]float32(nil), vec...))
			continue
		}
		out = append(out, append([]float32(nil), s.defaultVec...))
	}
	return out, nil
}

func (s *semanticStubEmbedder) resetCalls() {
	s.calls = nil
}

func seedSemanticCache(t *testing.T, identity string) (string, *Store, Node, string, *semanticStubEmbedder) {
	t.Helper()
	home, store := newStore(t)
	node := Node{
		ID: "skill:semantic-cache", Kind: "skill", Name: "Public Boundary Redact",
		Triggers: []string{"redact before publishing"}, Weight: 1,
	}
	if err := store.Add(node); err != nil {
		t.Fatal(err)
	}
	task := "send this everywhere"
	stub := newSemanticStub(identity)
	stub.vectors[task] = []float32{1, 0}
	stub.vectors[nodeEmbedText(node)] = []float32{0.9, 0.1}
	if _, err := store.SurfaceForContext(SurfaceContext{Task: task, Embedder: stub}); err != nil {
		t.Fatal(err)
	}
	return home, store, node, task, stub
}

func TestSemanticSurfacesNodeWithoutLiteralOverlap(t *testing.T) {
	_, store := newStore(t)
	node := Node{
		ID:       "skill:public-boundary-redact",
		Kind:     "skill",
		Name:     "Public Boundary Redact",
		Triggers: []string{"redact before publishing"},
		Boundary: []string{"scan before public release"},
		Weight:   1,
	}
	if err := store.Add(node); err != nil {
		t.Fatal(err)
	}
	task := "share the repo with the world"
	literal, err := store.SurfaceForContext(SurfaceContext{Task: task})
	if err != nil {
		t.Fatal(err)
	}
	if surfacedByID(literal, node.ID) != nil {
		t.Fatalf("literal-only surface should miss semantic node: %+v", literal)
	}
	stub := newSemanticStub("stub:v1:2:f32")
	stub.vectors[task] = []float32{1, 0}
	stub.vectors[nodeEmbedText(node)] = []float32{0.8, 0.6}
	semantic, err := store.SurfaceForContext(SurfaceContext{Task: task, Embedder: stub})
	if err != nil {
		t.Fatal(err)
	}
	hit := surfacedByID(semantic, node.ID)
	if hit == nil {
		t.Fatalf("semantic surface missed node: %+v", semantic)
	}
	if !strings.Contains(hit.Why, "+semantic=") {
		t.Fatalf("semantic contribution not explained: %+v", hit)
	}
}

func TestNilEmbedderBehaviorUnchanged(t *testing.T) {
	_, store := newStore(t)
	node := Node{ID: "skill:pricing", Kind: "skill", Name: "Pricing", Triggers: []string{"pricing"}, Weight: 1}
	if err := store.Add(node); err != nil {
		t.Fatal(err)
	}
	task := "fix pricing"
	literal, err := store.SurfaceForContext(SurfaceContext{Task: task})
	if err != nil {
		t.Fatal(err)
	}
	stub := newSemanticStub("stub:low:2:f32")
	stub.vectors[task] = []float32{1, 0}
	stub.vectors[nodeEmbedText(node)] = []float32{0, 1}
	lowSemantic, err := store.SurfaceForContext(SurfaceContext{Task: task, Embedder: stub})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(literal, lowSemantic) {
		t.Fatalf("nil embedder behavior changed:\nliteral=%+v\nlow_semantic=%+v", literal, lowSemantic)
	}
}

func TestNodeVectorCacheIncremental(t *testing.T) {
	home, store := newStore(t)
	nodeA := Node{ID: "skill:alpha", Kind: "skill", Name: "Alpha", Triggers: []string{"alpha"}}
	nodeB := Node{ID: "skill:beta", Kind: "skill", Name: "Beta", Triggers: []string{"beta"}}
	for _, node := range []Node{nodeA, nodeB} {
		if err := store.Add(node); err != nil {
			t.Fatal(err)
		}
	}
	task := "share with the world"
	stub := newSemanticStub("stub:v1:2:f32")
	stub.vectors[task] = []float32{1, 0}
	stub.vectors[nodeEmbedText(nodeA)] = []float32{0.8, 0.6}
	stub.vectors[nodeEmbedText(nodeB)] = []float32{0.7, 0.7}
	if _, err := store.SurfaceForContext(SurfaceContext{Task: task, Embedder: stub}); err != nil {
		t.Fatal(err)
	}
	if len(stub.calls) != 2 || !reflect.DeepEqual(stub.calls[0], []string{task}) || len(stub.calls[1]) != 2 {
		t.Fatalf("first surface should embed task plus all nodes, calls=%+v", stub.calls)
	}
	stub.resetCalls()
	if _, err := store.SurfaceForContext(SurfaceContext{Task: task, Embedder: stub}); err != nil {
		t.Fatal(err)
	}
	if len(stub.calls) != 1 || !reflect.DeepEqual(stub.calls[0], []string{task}) {
		t.Fatalf("cached surface should embed only task, calls=%+v", stub.calls)
	}
	updatedA := nodeA
	updatedA.Triggers = []string{"alpha changed"}
	if err := store.Add(updatedA); err != nil {
		t.Fatal(err)
	}
	stub.vectors[nodeEmbedText(updatedA)] = []float32{0.9, 0.1}
	stub.resetCalls()
	if _, err := store.SurfaceForContext(SurfaceContext{Task: task, Embedder: stub}); err != nil {
		t.Fatal(err)
	}
	if len(stub.calls) != 2 || !reflect.DeepEqual(stub.calls[0], []string{task}) || !reflect.DeepEqual(stub.calls[1], []string{nodeEmbedText(updatedA)}) {
		t.Fatalf("changed node should be the only node re-embedded, calls=%+v", stub.calls)
	}
	rebuild := newSemanticStub("stub:v2:2:f32")
	rebuild.vectors[task] = []float32{1, 0}
	rebuild.vectors[nodeEmbedText(updatedA)] = []float32{0.9, 0.1}
	rebuild.vectors[nodeEmbedText(nodeB)] = []float32{0.7, 0.7}
	if _, err := store.SurfaceForContext(SurfaceContext{Task: task, Embedder: rebuild}); err != nil {
		t.Fatal(err)
	}
	if len(rebuild.calls) != 2 || !reflect.DeepEqual(rebuild.calls[0], []string{task}) || len(rebuild.calls[1]) != 2 {
		t.Fatalf("new identity should rebuild all node vectors, calls=%+v", rebuild.calls)
	}
	dirs, err := os.ReadDir(filepath.Join(home, "indexes", "capgraph"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 2 {
		t.Fatalf("identity-separated cache dirs = %d, want 2", len(dirs))
	}
}

func TestWarmNodeVectorCacheDoesNotRewriteUntilNodesChange(t *testing.T) {
	home, store := newStore(t)
	nodes := []Node{
		{ID: "skill:alpha", Kind: "skill", Name: "Alpha", Triggers: []string{"alpha"}},
		{ID: "skill:beta", Kind: "skill", Name: "Beta", Triggers: []string{"beta"}},
	}
	for _, node := range nodes {
		if err := store.Add(node); err != nil {
			t.Fatal(err)
		}
	}
	task := "share with the world"
	stub := newSemanticStub("stub:mtime:2:f32")
	stub.vectors[task] = []float32{1, 0}
	if _, err := store.SurfaceForContext(SurfaceContext{Task: task, Embedder: stub}); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(home, "indexes", "capgraph", sanitizedEmbedderIdentity(stub.Identity()))
	paths := []string{filepath.Join(cacheDir, "manifest.json"), filepath.Join(cacheDir, "vectors.json")}
	sentinel := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	for _, path := range paths {
		if err := os.Chtimes(path, sentinel, sentinel); err != nil {
			t.Fatal(err)
		}
	}
	stub.resetCalls()
	if _, err := store.SurfaceForContext(SurfaceContext{Task: task, Embedder: stub}); err != nil {
		t.Fatal(err)
	}
	if len(stub.calls) != 1 || !reflect.DeepEqual(stub.calls[0], []string{task}) {
		t.Fatalf("warm capgraph embedded cached nodes: %+v", stub.calls)
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(sentinel) {
			t.Fatalf("warm capgraph rewrote %s: mtime=%s want=%s", filepath.Base(path), info.ModTime(), sentinel)
		}
	}

	updated := nodes[0]
	updated.Triggers = []string{"alpha changed"}
	if err := store.Add(updated); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SurfaceForContext(SurfaceContext{Task: task, Embedder: stub}); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.ModTime().Equal(sentinel) {
			t.Fatalf("changed node did not rewrite %s", filepath.Base(path))
		}
	}
}

func TestMalformedNodeVectorCacheRebuilds(t *testing.T) {
	home, store, node, task, _ := seedSemanticCache(t, "stub:malformed:2:f32")
	cacheDir := filepath.Join(home, "indexes", "capgraph", sanitizedEmbedderIdentity("stub:malformed:2:f32"))
	if err := os.WriteFile(filepath.Join(cacheDir, "manifest.json"), []byte(`{"schema_version":`), 0o644); err != nil {
		t.Fatal(err)
	}

	rebuild := newSemanticStub("stub:malformed:2:f32")
	rebuild.vectors[task] = []float32{1, 0}
	rebuild.vectors[nodeEmbedText(node)] = []float32{0.9, 0.1}
	got, err := store.SurfaceForContext(SurfaceContext{Task: task, Embedder: rebuild})
	if err != nil {
		t.Fatal(err)
	}
	if surfacedByID(got, node.ID) == nil {
		t.Fatalf("rebuilt semantic cache missed node: %+v", got)
	}
	if len(rebuild.calls) != 2 || !reflect.DeepEqual(rebuild.calls[0], []string{task}) || !reflect.DeepEqual(rebuild.calls[1], []string{nodeEmbedText(node)}) {
		t.Fatalf("malformed cache rebuild calls = %+v, want task then node", rebuild.calls)
	}
	manifest, err := readNodeVectorManifest(cacheDir)
	if err != nil {
		t.Fatalf("rewritten manifest is not healthy: %v", err)
	}
	if manifest.SchemaVersion != capgraphSemanticCacheSchemaVersion {
		t.Fatalf("rewritten schema = %q, want %q", manifest.SchemaVersion, capgraphSemanticCacheSchemaVersion)
	}
}

func TestNodeVectorCacheOperationalReadErrorDegradesWithoutNodeWork(t *testing.T) {
	const identity = "stub:operational:2:f32"
	home, store, _, task, _ := seedSemanticCache(t, identity)
	literal, err := store.SurfaceForContext(SurfaceContext{Task: task})
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(home, "indexes", "capgraph", sanitizedEmbedderIdentity(identity))
	manifestPath := filepath.Join(cacheDir, "manifest.json")
	vectorsPath := filepath.Join(cacheDir, "vectors.json")
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(manifestPath, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(vectorsPath, sentinel, sentinel); err != nil {
		t.Fatal(err)
	}

	probe := newSemanticStub(identity)
	probe.vectors[task] = []float32{1, 0}
	got, err := store.SurfaceForContext(SurfaceContext{Task: task, Embedder: probe})
	if err != nil {
		t.Fatalf("semantic cache read failure should degrade to lexical: %v", err)
	}
	if !reflect.DeepEqual(got, literal) {
		t.Fatalf("operational cache error changed lexical result:\nliteral=%+v\ngot=%+v", literal, got)
	}
	if len(probe.calls) != 1 || !reflect.DeepEqual(probe.calls[0], []string{task}) {
		t.Fatalf("operational cache error embed calls = %+v, want task only", probe.calls)
	}
	if info, err := os.Stat(vectorsPath); err != nil {
		t.Fatal(err)
	} else if !info.ModTime().Equal(sentinel) {
		t.Fatalf("operational cache error rewrote vectors: mtime=%s want=%s", info.ModTime(), sentinel)
	}
	if info, err := os.Stat(manifestPath); err != nil {
		t.Fatal(err)
	} else if !info.IsDir() {
		t.Fatalf("operational manifest path was replaced: mode=%s", info.Mode())
	}
	stats, err := store.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.SemanticLane != semanticLaneDegraded {
		t.Fatalf("semantic lane = %q, want degraded", stats.SemanticLane)
	}
}

func TestNodeVectorCacheIdentityMismatchDegradesWithoutRewrite(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*nodeVectorManifest)
	}{
		{name: "embedder identity", mutate: func(m *nodeVectorManifest) { m.EmbedderIdentity = "other:identity:2:f32" }},
		{name: "dimension identity", mutate: func(m *nodeVectorManifest) { m.Dim++ }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const identity = "stub:identity-guard:2:f32"
			home, store, _, task, _ := seedSemanticCache(t, identity)
			literal, err := store.SurfaceForContext(SurfaceContext{Task: task})
			if err != nil {
				t.Fatal(err)
			}
			cacheDir := filepath.Join(home, "indexes", "capgraph", sanitizedEmbedderIdentity(identity))
			manifestPath := filepath.Join(cacheDir, "manifest.json")
			vectorsPath := filepath.Join(cacheDir, "vectors.json")
			raw, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			var manifest nodeVectorManifest
			if err := json.Unmarshal(raw, &manifest); err != nil {
				t.Fatal(err)
			}
			tt.mutate(&manifest)
			raw, err = json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifestPath, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			sentinel := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
			for _, path := range []string{manifestPath, vectorsPath} {
				if err := os.Chtimes(path, sentinel, sentinel); err != nil {
					t.Fatal(err)
				}
			}

			probe := newSemanticStub(identity)
			probe.vectors[task] = []float32{1, 0}
			got, err := store.SurfaceForContext(SurfaceContext{Task: task, Embedder: probe})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, literal) {
				t.Fatalf("identity mismatch changed lexical result:\nliteral=%+v\ngot=%+v", literal, got)
			}
			if len(probe.calls) != 1 || !reflect.DeepEqual(probe.calls[0], []string{task}) {
				t.Fatalf("identity mismatch embed calls = %+v, want task only", probe.calls)
			}
			for _, path := range []string{manifestPath, vectorsPath} {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if !info.ModTime().Equal(sentinel) {
					t.Fatalf("identity mismatch rewrote %s: mtime=%s want=%s", filepath.Base(path), info.ModTime(), sentinel)
				}
			}
		})
	}
}

func TestEmbedFailureDegradesObservably(t *testing.T) {
	_, store := newStore(t)
	node := Node{ID: "skill:pricing", Kind: "skill", Name: "Pricing", Triggers: []string{"pricing"}}
	if err := store.Add(node); err != nil {
		t.Fatal(err)
	}
	task := "pricing"
	literal, err := store.SurfaceForContext(SurfaceContext{Task: task})
	if err != nil {
		t.Fatal(err)
	}
	stub := newSemanticStub("stub:error:2:f32")
	stub.err = errors.New("embedder down")
	got, err := store.SurfaceForContext(SurfaceContext{Task: task, Embedder: stub})
	if err != nil {
		t.Fatalf("embed failure should not propagate: %v", err)
	}
	if !reflect.DeepEqual(literal, got) {
		t.Fatalf("embed failure should degrade to literal results:\nliteral=%+v\ngot=%+v", literal, got)
	}
	stats, err := store.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.SemanticLane != semanticLaneDegraded {
		t.Fatalf("semantic lane = %q, want degraded", stats.SemanticLane)
	}
}

func TestInstructionRuleWhenGateSatisfiedSemantically(t *testing.T) {
	_, store := newStore(t)
	rule := Node{
		ID:       "instruction_rule:public-boundary-redact",
		Kind:     "instruction_rule",
		Name:     "Public Boundary Redact",
		Triggers: []string{"redact release gate"},
		Weight:   1,
		When: []InstructionRuleWhen{{
			Kind:   "task-class",
			Values: []string{"public"},
		}},
		Boundary: []string{"redact before public publishing"},
	}
	if err := store.Add(rule); err != nil {
		t.Fatal(err)
	}
	task := "share this with everyone"
	literal, err := store.SurfaceForContext(SurfaceContext{Task: task})
	if err != nil {
		t.Fatal(err)
	}
	if surfacedByID(literal, rule.ID) != nil {
		t.Fatalf("literal-only when gate should not match: %+v", literal)
	}
	stub := newSemanticStub("stub:rule:2:f32")
	stub.vectors[task] = []float32{1, 0}
	stub.vectors[nodeEmbedText(rule)] = []float32{0.82, 0.57}
	semantic, err := store.SurfaceForContext(SurfaceContext{Task: task, Embedder: stub})
	if err != nil {
		t.Fatal(err)
	}
	hit := surfacedByID(semantic, rule.ID)
	if hit == nil {
		t.Fatalf("semantic when gate did not surface rule: %+v", semantic)
	}
	if !strings.Contains(hit.Why, "+semantic=") {
		t.Fatalf("semantic when-gate match not explained: %+v", hit)
	}
}

// flakySemanticEmbedder succeeds for a fixed number of Embed calls, then
// errors — the shape of a cold start dying at a surfacing timeout mid-graph.
type flakySemanticEmbedder struct {
	semanticStubEmbedder
	failAfterCalls int
}

func (f *flakySemanticEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if f.failAfterCalls > 0 && len(f.calls) >= f.failAfterCalls {
		f.calls = append(f.calls, append([]string(nil), texts...))
		return nil, errors.New("embed backend timed out")
	}
	return f.semanticStubEmbedder.Embed(ctx, texts)
}

// A cold start that dies mid-graph must keep the chunks it finished; the next
// call embeds only the remainder. Without partial persistence this test fails:
// the second call re-embeds every node and the lane can never converge.
func TestSemanticCachePersistsPartialProgressOnEmbedFailure(t *testing.T) {
	home := t.TempDir()
	nodeCount := semanticEmbedChunk*2 + 2 // three chunks: 64+64+2
	nodes := make([]Node, 0, nodeCount)
	for i := 0; i < nodeCount; i++ {
		nodes = append(nodes, Node{ID: fmt.Sprintf("node-%03d", i), Kind: "skill", Name: fmt.Sprintf("skill %03d", i), Weight: 1})
	}

	flaky := &flakySemanticEmbedder{failAfterCalls: 2} // task embed + first chunk succeed, second chunk dies
	flaky.identity = "stub:partial:2"
	flaky.dim = 2
	flaky.defaultVec = []float32{0, 1}
	if _, err := semanticScores(context.Background(), flaky, home, nodes, "any task"); err == nil {
		t.Fatalf("expected embed failure to propagate")
	}

	healthy := newSemanticStub("stub:partial:2")
	if _, err := semanticScores(context.Background(), healthy, home, nodes, "any task"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if len(healthy.calls) < 2 {
		t.Fatalf("expected task + remainder embed calls, got %d calls", len(healthy.calls))
	}
	remainder := 0
	for _, call := range healthy.calls[1:] {
		remainder += len(call)
	}
	if want := nodeCount - semanticEmbedChunk; remainder != want {
		t.Fatalf("second call embedded %d node texts, want only the un-persisted remainder %d", remainder, want)
	}
}
