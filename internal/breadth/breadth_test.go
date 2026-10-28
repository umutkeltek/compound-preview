package breadth

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"compound/internal/mem"
)

type recordingBreadthEmbedder struct {
	inner   *fakeEmbedder
	batches [][]string
}

func newRecordingBreadthEmbedder(dim int) *recordingBreadthEmbedder {
	return &recordingBreadthEmbedder{inner: newFakeEmbedder(dim)}
}

func (e *recordingBreadthEmbedder) Identity() string { return e.inner.Identity() }
func (e *recordingBreadthEmbedder) Dim() int         { return e.inner.Dim() }
func (e *recordingBreadthEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e.batches = append(e.batches, append([]string(nil), texts...))
	return e.inner.Embed(ctx, texts)
}

func TestTokenizeSplitsIdentifiers(t *testing.T) {
	got := Tokenize("compoundMemory sidecar_breadth EngineHTTP route-plan")
	want := []string{"compound", "memory", "sidecar", "breadth", "engine", "http", "route", "plan"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tokens = %v, want %v", got, want)
	}
}

func TestQueryStripsStopwordsSoTerseEntityClaimsRank(t *testing.T) {
	// Falsifier for entity/predicate recall dilution (measured 2026-07-07): a
	// natural-language question must retrieve the terse entity-keyed claim, not be
	// crowded out of the top-k by stopword-heavy meta-claims. Pre-stopword-filter,
	// the filler terms (what/is/the/for/project) matched the noise claims and
	// pushed the target below k. Live proof this fixes: golden-set recall bakeoff.
	var eng Engine
	eng.Index([]mem.ClaimText{
		{ClaimID: "svc_alpha_database", Text: "alpha database managed postgres instance in a cloud region"},
		{ClaimID: "noise_dec", Text: "decision what is the goal for the project we decided is the binding file"},
		{ClaimID: "noise_pack", Text: "the context pack is what carries the source revisions for the runtime"},
		{ClaimID: "noise_policy", Text: "the policy decision is what governs the action for the work at the gate"},
		{ClaimID: "noise_paper", Text: "document parsing is what the vision language model does for the two stage"},
	}, 1)
	got := eng.Query("What is the database for the alpha project?", mem.GateAll, nil, 3)
	if len(got) == 0 {
		t.Fatalf("query returned no hits")
	}
	if got[0].ClaimID != "svc_alpha_database" {
		t.Fatalf("terse entity claim must rank #1 after stopword filtering; top-3 = %+v", got)
	}
}

func TestSalientTermsFallsBackWhenAllStopwords(t *testing.T) {
	// An all-stopword query must not filter to nothing (that would return zero
	// hits); salientTerms falls back to the unfiltered tokens.
	if got := salientTerms("what is the"); len(got) == 0 {
		t.Fatalf("all-stopword query must fall back to unfiltered tokens, got empty")
	}
	if got := salientTerms("alpha database"); !reflect.DeepEqual(got, []string{"alpha", "database"}) {
		t.Fatalf("salient terms = %v, want [alpha database]", got)
	}
}

func TestQueryEnforcesAllowlistFailClosed(t *testing.T) {
	var eng Engine
	eng.Index([]mem.ClaimText{
		{ClaimID: "c1", Text: "compound memory engine"},
		{ClaimID: "c2", Text: "unrelated memory"},
	}, 1)
	if got := eng.Query("memory", mem.GateList, nil, 10); len(got) != 0 {
		t.Fatalf("empty allowlist must fail closed, got %+v", got)
	}
	got := eng.Query("memory", mem.GateList, []string{"c1"}, 10)
	if len(got) != 1 || got[0].ClaimID != "c1" {
		t.Fatalf("allowlist query = %+v, want c1 only", got)
	}
}

func TestReferenceCoverageUnionCatchesSubstring(t *testing.T) {
	var eng Engine
	eng.Index([]mem.ClaimText{{ClaimID: "c1", Text: "compounding substrate"}}, 1)
	got := eng.Query("compound", mem.GateList, []string{"c1"}, 10)
	if len(got) != 1 || got[0].ClaimID != "c1" {
		t.Fatalf("substring reference hit should be covered, got %+v", got)
	}
}

func TestReferenceCoverageSurvivesTightK(t *testing.T) {
	var eng Engine
	eng.Index([]mem.ClaimText{
		{ClaimID: "substring", Text: "compounding substrate"},
		{ClaimID: "exact1", Text: "compound memory"},
		{ClaimID: "exact2", Text: "compound engine"},
	}, 1)
	got := eng.Query("compound", mem.GateList, []string{"substring", "exact1", "exact2"}, 3)
	seen := map[string]bool{}
	for _, h := range got {
		seen[h.ClaimID] = true
	}
	if !seen["substring"] || !seen["exact1"] || !seen["exact2"] {
		t.Fatalf("all reference hits should survive top-k, got %+v", got)
	}
}

func TestReferenceCoverageMirrorsReferenceTopKWhenReferenceSetExceedsK(t *testing.T) {
	claims := []mem.Claim{
		{ID: "z_first", Entity: "compound", Predicate: "topic", Object: "alpha"},
		{ID: "a_second", Entity: "compound", Predicate: "topic", Object: "beta"},
		{ID: "b_third", Entity: "compound", Predicate: "topic", Object: "gamma"},
	}
	allow := []string{"z_first", "a_second", "b_third"}
	var reference mem.LexicalEngine
	if err := reference.Index(claims, 1); err != nil {
		t.Fatal(err)
	}
	referenceHits, err := reference.Query("compound", mem.GateList, allow, 2)
	if err != nil {
		t.Fatal(err)
	}

	var eng Engine
	eng.Index([]mem.ClaimText{
		{ClaimID: claims[0].ID, Text: mem.ClaimIndexText(claims[0])},
		{ClaimID: claims[1].ID, Text: mem.ClaimIndexText(claims[1])},
		{ClaimID: claims[2].ID, Text: mem.ClaimIndexText(claims[2])},
	}, 1)
	got := eng.Query("compound", mem.GateList, allow, 2)
	gotIDs := map[string]bool{}
	for _, h := range got {
		gotIDs[h.ClaimID] = true
	}
	for _, h := range referenceHits {
		if !gotIDs[h.ClaimID] {
			t.Fatalf("breadth result should cover reference top-k hit %s; reference=%+v got=%+v", h.ClaimID, referenceHits, got)
		}
	}
}

func TestBM25RanksSpecificTerms(t *testing.T) {
	var eng Engine
	eng.Index([]mem.ClaimText{
		{ClaimID: "specific", Text: "compound memory engine"},
		{ClaimID: "broad", Text: "compound only"},
	}, 1)
	got := eng.Query("memory engine", mem.GateList, []string{"specific", "broad"}, 10)
	if len(got) == 0 || got[0].ClaimID != "specific" {
		t.Fatalf("BM25 should rank specific term match first, got %+v", got)
	}
}

func TestRunProtocolWireLoop(t *testing.T) {
	var in bytes.Buffer
	enc := json.NewEncoder(&in)
	for _, req := range []mem.Request{
		{SchemaVersion: mem.ProtocolVersion, RequestID: "caps", Op: mem.OpCapabilities},
		{SchemaVersion: mem.ProtocolVersion, RequestID: "idx", Op: mem.OpIndex, CorpusGen: 7, Claims: []mem.ClaimText{{ClaimID: "c1", Text: "compounding substrate"}}},
		{SchemaVersion: mem.ProtocolVersion, RequestID: "query", Op: mem.OpQuery, Text: "compound", Gate: mem.GateList, Allowlist: []string{"c1"}, K: 10},
	} {
		if err := enc.Encode(req); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := RunProtocol(&in, &out, ProtocolConfig{}); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&out)
	var caps, idx, query mem.Response
	for _, resp := range []*mem.Response{&caps, &idx, &query} {
		if err := dec.Decode(resp); err != nil {
			t.Fatal(err)
		}
	}
	if caps.Name != Name || !caps.Breadth || caps.Embedder != DefaultEmbedder {
		t.Fatalf("capabilities = %+v", caps)
	}
	if idx.Indexed != 1 || idx.CorpusGen != 7 {
		t.Fatalf("index response = %+v", idx)
	}
	if len(query.Hits) != 1 || query.Hits[0].ClaimID != "c1" {
		t.Fatalf("query response = %+v", query)
	}
}

func TestRunProtocolQueryBeforeIndexFailsTyped(t *testing.T) {
	var in bytes.Buffer
	if err := json.NewEncoder(&in).Encode(mem.Request{SchemaVersion: mem.ProtocolVersion, RequestID: "query", Op: mem.OpQuery, Text: "x", Gate: mem.GateList, Allowlist: []string{"c1"}, K: 1}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := RunProtocol(&in, &out, ProtocolConfig{}); err != nil {
		t.Fatal(err)
	}
	var resp mem.Response
	if err := json.NewDecoder(&out).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Error == nil || resp.Error.Code != mem.CodeIndexMissing {
		t.Fatalf("query before index response = %+v", resp)
	}
}

func TestFixtureCorpusReferenceCovered(t *testing.T) {
	claims := []mem.ClaimText{
		{ClaimID: "c_compounding", Text: "compounding substrate"},
		{ClaimID: "c_memory", Text: "memory engine route"},
		{ClaimID: "c_camel", Text: "ContextPack hookReadiness"},
	}
	allow := []string{"c_compounding", "c_memory", "c_camel"}
	tests := []struct {
		query string
		k     int
	}{
		{query: "compound", k: 1},
		{query: "memory engine", k: 2},
		{query: "hook readiness", k: 1},
	}
	var eng Engine
	eng.Index(claims, 1)
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			reference := referenceContains(tt.query, docsFromClaims(claims))
			got := eng.Query(tt.query, mem.GateList, allow, tt.k)
			gotIDs := map[string]bool{}
			for _, h := range got {
				gotIDs[h.ClaimID] = true
			}
			for i, h := range reference {
				if i >= tt.k {
					break
				}
				if !gotIDs[h.ClaimID] {
					t.Fatalf("query %q missing reference hit %s in %+v", tt.query, h.ClaimID, got)
				}
			}
		})
	}
}

func TestVectorIncrementalIndexEqualsFullRebuild(t *testing.T) {
	initial := []mem.ClaimText{
		{ClaimID: "c1", Entity: "alpha", ClaimType: "decision", Text: "alpha owns the runtime spine"},
		{ClaimID: "c2", Entity: "beta", ClaimType: "fact", Text: "beta keeps the ledger visible"},
	}
	added := mem.ClaimText{ClaimID: "c3", Entity: "gamma", ClaimType: "fact", Text: "gamma carries receipts"}
	all := append(append([]mem.ClaimText(nil), initial...), added)

	incrementalHome := t.TempDir()
	incremental, err := NewEngine(EngineConfig{Embedder: newFakeEmbedder(6), CompoundHome: incrementalHome})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := incremental.IndexWithError(initial, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := incremental.IndexWithError(all, 2); err != nil {
		t.Fatal(err)
	}
	incrementalVectors := readVectorsFile(t, incrementalHome, incremental.embedder)

	fullHome := t.TempDir()
	full, err := NewEngine(EngineConfig{Embedder: newFakeEmbedder(6), CompoundHome: fullHome})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := full.IndexWithError(all, 2); err != nil {
		t.Fatal(err)
	}
	fullVectors := readVectorsFile(t, fullHome, full.embedder)

	if !bytes.Equal(incrementalVectors, fullVectors) {
		t.Fatalf("incremental vectors.bin differs from full rebuild")
	}
}

func TestWarmVectorCacheDoesNotRewriteUntilCorpusChanges(t *testing.T) {
	home := t.TempDir()
	claims := []mem.ClaimText{
		{ClaimID: "c1", Text: "deployment automation releases artifacts"},
		{ClaimID: "c2", Text: "capability graph routes tools"},
	}
	seedEmbedder := newFakeEmbedder(6)
	seed, err := NewEngine(EngineConfig{Embedder: seedEmbedder, CompoundHome: home})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.IndexWithError(claims, 7); err != nil {
		t.Fatal(err)
	}
	wantHits := seed.Query("release artifacts", mem.GateList, []string{"c1", "c2"}, 1)
	cacheDir := vectorCacheDir(t, home, seedEmbedder)
	paths := []string{filepath.Join(cacheDir, "manifest.json"), filepath.Join(cacheDir, "vectors.bin")}
	sentinel := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	for _, path := range paths {
		if err := os.Chtimes(path, sentinel, sentinel); err != nil {
			t.Fatal(err)
		}
	}

	warmEmbedder := newFakeEmbedder(6)
	warm, err := NewEngine(EngineConfig{Embedder: warmEmbedder, CompoundHome: home})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := warm.IndexWithError(claims, 7); err != nil {
		t.Fatal(err)
	}
	if warmEmbedder.calls != 0 {
		t.Fatalf("warm cache embedded %d document batch(es), want 0", warmEmbedder.calls)
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(sentinel) {
			t.Fatalf("warm cache rewrote %s: mtime=%s want=%s", filepath.Base(path), info.ModTime(), sentinel)
		}
	}
	hits := warm.Query("release artifacts", mem.GateList, []string{"c1", "c2"}, 1)
	if !reflect.DeepEqual(hits, wantHits) {
		t.Fatalf("warm semantic result drifted:\ngot  %+v\nwant %+v", hits, wantHits)
	}

	changedGeneration, err := NewEngine(EngineConfig{Embedder: newFakeEmbedder(6), CompoundHome: home})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := changedGeneration.IndexWithError(claims, 8); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.ModTime().Equal(sentinel) {
			t.Fatalf("changed corpus generation did not rewrite %s", filepath.Base(path))
		}
	}
}

func TestVectorCacheLoadGuardsDimAndEmbedderStamp(t *testing.T) {
	t.Run("corrupt row dimension rebuilds derived cache", func(t *testing.T) {
		home := t.TempDir()
		embedder := newFakeEmbedder(4)
		eng, err := NewEngine(EngineConfig{Embedder: embedder, CompoundHome: home})
		if err != nil {
			t.Fatal(err)
		}
		claims := []mem.ClaimText{{ClaimID: "c1", Entity: "alpha", ClaimType: "fact", Text: "alpha cache row"}}
		if _, err := eng.IndexWithError(claims, 1); err != nil {
			t.Fatal(err)
		}
		path := vectorsFilePath(t, home, embedder)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rowDimOffset := firstRowDimOffset(t, raw)
		binary.LittleEndian.PutUint32(raw[rowDimOffset:rowDimOffset+4], uint32(99))
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		reload, err := NewEngine(EngineConfig{Embedder: newFakeEmbedder(4), CompoundHome: home})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reload.IndexWithError(claims, 2); err != nil {
			t.Fatalf("corrupt derived cache should rebuild: %v", err)
		}
		if _, _, err := reload.cache.load(); err != nil {
			t.Fatalf("rebuilt cache still invalid: %v", err)
		}
	})

	t.Run("embedder stamp mismatch hard refuses load", func(t *testing.T) {
		home := t.TempDir()
		embedder := newFakeEmbedder(4)
		eng, err := NewEngine(EngineConfig{Embedder: embedder, CompoundHome: home})
		if err != nil {
			t.Fatal(err)
		}
		claims := []mem.ClaimText{{ClaimID: "c1", Entity: "alpha", ClaimType: "fact", Text: "alpha cache row"}}
		if _, err := eng.IndexWithError(claims, 1); err != nil {
			t.Fatal(err)
		}
		manifestPath := filepath.Join(vectorCacheDir(t, home, embedder), "manifest.json")
		raw, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		var manifest vectorManifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			t.Fatal(err)
		}
		manifest.Embedder.Model = "other-space"
		raw, err = json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(manifestPath, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		reload, err := NewEngine(EngineConfig{Embedder: newFakeEmbedder(4), CompoundHome: home})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reload.IndexWithError(claims, 2); !errors.Is(err, errVectorEmbedderMismatch) {
			t.Fatalf("IndexWithError stamp mismatch err = %v, want %v", err, errVectorEmbedderMismatch)
		}
	})
}

func TestVectorCacheOldSchemaRebuildsSemanticIndex(t *testing.T) {
	home := t.TempDir()
	claims := []mem.ClaimText{
		{ClaimID: "winner", Text: "orchestration receipts"},
		{ClaimID: "decoy", Text: "system vocabulary"},
	}
	query := "system"
	vectors := map[string][]float32{
		query:                  {1, 0},
		wrapDocText(claims[0]): {1, 0},
		wrapDocText(claims[1]): {0, 1},
	}
	seedEmbedder := newFakeEmbedder(2)
	seedEmbedder.vectors = vectors
	seed, err := NewEngine(EngineConfig{Embedder: seedEmbedder, CompoundHome: home})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.IndexWithError(claims, 1); err != nil {
		t.Fatal(err)
	}

	manifestPath := filepath.Join(vectorCacheDir(t, home, seedEmbedder), "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var oldManifest vectorManifest
	if err := json.Unmarshal(raw, &oldManifest); err != nil {
		t.Fatal(err)
	}
	oldManifest.SchemaVersion = "compound.breadth.index.v0"
	raw, err = json.Marshal(oldManifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	rebuildEmbedder := newFakeEmbedder(2)
	rebuildEmbedder.vectors = vectors
	reload, err := NewEngine(EngineConfig{Embedder: rebuildEmbedder, CompoundHome: home})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reload.IndexWithError(claims, 2); err != nil {
		t.Fatalf("old derived-cache schema should rebuild: %v", err)
	}
	if rebuildEmbedder.calls != 1 {
		t.Fatalf("schema rebuild embedded %d document batch(es), want 1", rebuildEmbedder.calls)
	}
	hits := reload.Query(query, mem.GateList, []string{"winner", "decoy"}, 1)
	if len(hits) != 1 || hits[0].ClaimID != "winner" {
		t.Fatalf("rebuilt semantic query = %+v, want winner", hits)
	}
	manifest, _, err := reload.cache.load()
	if err != nil {
		t.Fatalf("rewritten cache is not healthy: %v", err)
	}
	if manifest.SchemaVersion != vectorSchemaVersion {
		t.Fatalf("rewritten cache schema = %q, want %q", manifest.SchemaVersion, vectorSchemaVersion)
	}
}

func TestVectorCacheCorruptionRebuildsSemanticIndex(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(t *testing.T, home string, embedder EmbedderAdapter)
	}{
		{
			name: "malformed manifest JSON",
			corrupt: func(t *testing.T, home string, embedder EmbedderAdapter) {
				t.Helper()
				path := filepath.Join(vectorCacheDir(t, home, embedder), "manifest.json")
				if err := os.WriteFile(path, []byte(`{"schema_version":`), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "truncated vector file",
			corrupt: func(t *testing.T, home string, embedder EmbedderAdapter) {
				t.Helper()
				path := vectorsFilePath(t, home, embedder)
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw[:len(raw)-1], 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "invalid vector row content",
			corrupt: func(t *testing.T, home string, embedder EmbedderAdapter) {
				t.Helper()
				path := vectorsFilePath(t, home, embedder)
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				const firstRowIDLengthOffset = 8 + 4 + 8
				binary.LittleEndian.PutUint32(raw[firstRowIDLengthOffset:firstRowIDLengthOffset+4], 0)
				if err := os.WriteFile(path, raw, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			claims := []mem.ClaimText{
				{ClaimID: "winner", Text: "orchestration receipts"},
				{ClaimID: "decoy", Text: "system vocabulary"},
			}
			query := "system"
			vectors := map[string][]float32{
				query:                  {1, 0},
				wrapDocText(claims[0]): {1, 0},
				wrapDocText(claims[1]): {0, 1},
			}
			seedEmbedder := newFakeEmbedder(2)
			seedEmbedder.vectors = vectors
			seed, err := NewEngine(EngineConfig{Embedder: seedEmbedder, CompoundHome: home})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := seed.IndexWithError(claims, 1); err != nil {
				t.Fatal(err)
			}
			tt.corrupt(t, home, seedEmbedder)

			rebuildEmbedder := newFakeEmbedder(2)
			rebuildEmbedder.vectors = vectors
			reload, err := NewEngine(EngineConfig{Embedder: rebuildEmbedder, CompoundHome: home})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reload.IndexWithError(claims, 2); err != nil {
				t.Fatalf("corrupt derived cache should rebuild: %v", err)
			}
			if rebuildEmbedder.calls != 1 {
				t.Fatalf("cache rebuild embedded %d document batch(es), want 1", rebuildEmbedder.calls)
			}
			hits := reload.Query(query, mem.GateList, []string{"winner", "decoy"}, 1)
			if len(hits) != 1 || hits[0].ClaimID != "winner" {
				t.Fatalf("rebuilt semantic query = %+v, want winner", hits)
			}
			if _, _, err := reload.cache.load(); err != nil {
				t.Fatalf("rewritten cache is not healthy: %v", err)
			}
		})
	}
}

func TestVectorCacheOperationalReadErrorIsReturned(t *testing.T) {
	home := t.TempDir()
	embedder := newFakeEmbedder(2)
	eng, err := NewEngine(EngineConfig{Embedder: embedder, CompoundHome: home})
	if err != nil {
		t.Fatal(err)
	}
	claims := []mem.ClaimText{{ClaimID: "c1", Text: "operational read errors stay visible"}}
	if _, err := eng.IndexWithError(claims, 1); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(vectorCacheDir(t, home, embedder), "manifest.json")
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(manifestPath, 0o755); err != nil {
		t.Fatal(err)
	}

	reloadEmbedder := newFakeEmbedder(2)
	reload, err := NewEngine(EngineConfig{Embedder: reloadEmbedder, CompoundHome: home})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reload.IndexWithError(claims, 2); err == nil {
		t.Fatal("IndexWithError returned nil for a manifest read-path failure")
	} else {
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			t.Fatalf("IndexWithError error = %T %v, want preserved *os.PathError", err, err)
		}
	}
	if reloadEmbedder.calls != 0 {
		t.Fatalf("operational read error triggered %d rebuild embed call(s), want 0", reloadEmbedder.calls)
	}
}

func TestVectorCacheRejectsUntrustedRowCountBeforeAllocation(t *testing.T) {
	tests := []struct {
		name  string
		count uint64
	}{
		{name: "beyond platform int capacity", count: ^uint64(0)},
		{name: "inconsistent with manifest", count: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			claims := []mem.ClaimText{
				{ClaimID: "winner", Text: "orchestration receipts"},
				{ClaimID: "decoy", Text: "system vocabulary"},
			}
			query := "system"
			vectors := map[string][]float32{
				query:                  {1, 0},
				wrapDocText(claims[0]): {1, 0},
				wrapDocText(claims[1]): {0, 1},
			}
			seedEmbedder := newFakeEmbedder(2)
			seedEmbedder.vectors = vectors
			seed, err := NewEngine(EngineConfig{Embedder: seedEmbedder, CompoundHome: home})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := seed.IndexWithError(claims, 1); err != nil {
				t.Fatal(err)
			}
			path := vectorsFilePath(t, home, seedEmbedder)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			const vectorCountOffset = 8 + 4
			binary.LittleEndian.PutUint64(raw[vectorCountOffset:vectorCountOffset+8], tt.count)
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatal(err)
			}

			rebuildEmbedder := newRecordingBreadthEmbedder(2)
			rebuildEmbedder.inner.vectors = vectors
			reload, err := NewEngine(EngineConfig{Embedder: rebuildEmbedder, CompoundHome: home})
			if err != nil {
				t.Fatal(err)
			}
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						t.Fatalf("untrusted row count panicked before rebuild: %v", recovered)
					}
				}()
				if _, err := reload.IndexWithError(claims, 2); err != nil {
					t.Fatalf("untrusted row count should rebuild: %v", err)
				}
			}()
			wantBatch := []string{wrapDocText(claims[0]), wrapDocText(claims[1])}
			if len(rebuildEmbedder.batches) != 1 || !reflect.DeepEqual(rebuildEmbedder.batches[0], wantBatch) {
				t.Fatalf("row-count rebuild batches = %+v, want one full-doc batch %+v", rebuildEmbedder.batches, wantBatch)
			}
			hits := reload.Query(query, mem.GateList, []string{"winner", "decoy"}, 1)
			if len(hits) != 1 || hits[0].ClaimID != "winner" {
				t.Fatalf("rebuilt semantic query = %+v, want winner", hits)
			}
			manifest, rows, err := reload.cache.load()
			if err != nil {
				t.Fatalf("rewritten cache is not healthy: %v", err)
			}
			if manifest.ClaimCount != len(claims) || manifest.EmbeddedCount != len(claims) || len(rows) != len(claims) {
				t.Fatalf("rewritten cache counts = claims:%d embedded:%d rows:%d, want %d", manifest.ClaimCount, manifest.EmbeddedCount, len(rows), len(claims))
			}
		})
	}
}

func TestVectorQueryDegradesToLexicalWhenEmbedderUnavailable(t *testing.T) {
	embedder := newFakeEmbedder(4)
	embedder.failAtCall = 2
	embedder.err = errors.New("embed server down")
	var in bytes.Buffer
	enc := json.NewEncoder(&in)
	for _, req := range []mem.Request{
		{SchemaVersion: mem.ProtocolVersion, RequestID: "idx", Op: mem.OpIndex, CorpusGen: 7, Claims: []mem.ClaimText{
			{ClaimID: "substring", Entity: "compound", ClaimType: "fact", Text: "compounding substrate"},
			{ClaimID: "exact", Entity: "compound", ClaimType: "fact", Text: "compound memory"},
		}},
		{SchemaVersion: mem.ProtocolVersion, RequestID: "query", Op: mem.OpQuery, Text: "compound", Gate: mem.GateList, Allowlist: []string{"substring", "exact"}, K: 2},
	} {
		if err := enc.Encode(req); err != nil {
			t.Fatal(err)
		}
	}
	var out, stderr bytes.Buffer
	if err := RunProtocol(&in, &out, ProtocolConfig{Embedder: embedder, CompoundHome: t.TempDir(), Stderr: &stderr}); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&out)
	var idx, query mem.Response
	if err := dec.Decode(&idx); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(&query); err != nil {
		t.Fatal(err)
	}
	if !idx.OK {
		t.Fatalf("index response = %+v", idx)
	}
	if !query.OK || !reflect.DeepEqual(query.DegradedLanes, []string{"vector"}) {
		t.Fatalf("query should degrade vector lane only, got %+v", query)
	}
	seen := map[string]bool{}
	for _, h := range query.Hits {
		seen[h.ClaimID] = true
	}
	if !seen["substring"] || !seen["exact"] {
		t.Fatalf("degraded lexical query must preserve reference hits, got %+v", query.Hits)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("vector lane degraded")) {
		t.Fatalf("stderr missing degradation warning: %q", stderr.String())
	}
}

func TestVectorQueryNeverSurfacesMaskedClaim(t *testing.T) {
	claims := []mem.ClaimText{
		{ClaimID: "allowed", Entity: "public", ClaimType: "fact", Text: "public allowed claim"},
		{ClaimID: "masked", Entity: "secret", ClaimType: "fact", Text: "hidden semantic answer"},
	}
	query := "semantic nearest"
	embedder := newFakeEmbedder(2)
	embedder.vectors = map[string][]float32{
		query:                  {1, 0},
		wrapDocText(claims[0]): {1, 0},
		wrapDocText(claims[1]): {1, 0},
	}
	eng, err := NewEngine(EngineConfig{Embedder: embedder, CompoundHome: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.IndexWithError(claims, 1); err != nil {
		t.Fatal(err)
	}
	got, err := eng.QueryWithLanes(query, mem.GateList, []string{"allowed"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Hits) == 0 {
		t.Fatalf("expected allowed semantic result")
	}
	for _, h := range got.Hits {
		if h.ClaimID == "masked" {
			t.Fatalf("masked claim surfaced with vector lane on: %+v", got.Hits)
		}
		if h.ClaimID != "allowed" {
			t.Fatalf("vector lane added non-allowed id %q in %+v", h.ClaimID, got.Hits)
		}
	}
}

func TestVectorSemanticCosineRanksLexicallyNonmatchingClaimTop(t *testing.T) {
	claims := []mem.ClaimText{
		{ClaimID: "semantic", Entity: "alpha", ClaimType: "fact", Text: "alpha carries receipts"},
		{ClaimID: "far", Entity: "beta", ClaimType: "fact", Text: "beta keeps ledgers"},
	}
	query := "where is the answer hiding"
	embedder := newFakeEmbedder(2)
	embedder.vectors = map[string][]float32{
		query:                  {1, 0},
		wrapDocText(claims[0]): {1, 0},
		wrapDocText(claims[1]): {0, 1},
	}
	eng, err := NewEngine(EngineConfig{Embedder: embedder, CompoundHome: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.IndexWithError(claims, 1); err != nil {
		t.Fatal(err)
	}
	lexical := eng.bm25(query, docsFromClaims(claims))
	if len(lexical) != 0 {
		t.Fatalf("test requires lexically nonmatching claims, got lexical hits %+v", lexical)
	}
	got := eng.Query(query, mem.GateList, []string{"semantic", "far"}, 2)
	if len(got) == 0 || got[0].ClaimID != "semantic" {
		t.Fatalf("semantic cosine should rank non-lexical closest claim first, got %+v", got)
	}
}

func TestVectorSemanticQueryExcludesOrthogonalLexicallyUnrelatedClaim(t *testing.T) {
	claims := []mem.ClaimText{
		{ClaimID: "relevant", Entity: "communication", ClaimType: "preference", Text: "concise progress reports"},
		{ClaimID: "orthogonal", Entity: "storage", ClaimType: "fact", Text: "database migration throughput"},
	}
	query := "notify during execution"
	embedder := newFakeEmbedder(2)
	embedder.vectors = map[string][]float32{
		query:                  {1, 0},
		wrapDocText(claims[0]): {1, 0},
		wrapDocText(claims[1]): {0, 1},
	}
	eng, err := NewEngine(EngineConfig{Embedder: embedder, CompoundHome: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.IndexWithError(claims, 1); err != nil {
		t.Fatal(err)
	}
	docs := docsFromClaims(claims)
	if got := eng.bm25(query, docs); len(got) != 0 {
		t.Fatalf("test requires no lexical hits, got %+v", got)
	}
	if got := referenceContains(query, docs); len(got) != 0 {
		t.Fatalf("test requires no reference hits, got %+v", got)
	}
	got := eng.Query(query, mem.GateList, []string{"relevant", "orthogonal"}, 2)
	if len(got) != 1 || got[0].ClaimID != "relevant" || got[0].Score <= 0 {
		t.Fatalf("semantic query admitted a nonpositive unrelated claim: %+v", got)
	}
}

func TestVectorSemanticBeatsSubstringDecoy(t *testing.T) {
	// Regression guard for the 2026-07-07 reference-reservation bug: substring/reference
	// hits must NOT be reserved ahead of a stronger semantic hit. A decoy that merely
	// shares a common word with the query ("system") but is semantically unrelated must
	// rank BELOW the semantically-correct claim that shares no query token. Before the
	// fix, blind reference reservation put the decoy first and stalled live recall at the
	// lexical baseline (0.879 vs 0.955).
	claims := []mem.ClaimText{
		{ClaimID: "winner", Entity: "dec", ClaimType: "decision", Text: "decision about chaining triggers"},
		{ClaimID: "decoy", Entity: "other", ClaimType: "fact", Text: "the immune system enforces placement"},
	}
	query := "system"
	embedder := newFakeEmbedder(2)
	embedder.vectors = map[string][]float32{
		query:                  {1, 0},
		wrapDocText(claims[0]): {1, 0}, // winner: semantically closest, no query token
		wrapDocText(claims[1]): {0, 1}, // decoy: semantically far, but substring-matches "system"
	}
	eng, err := NewEngine(EngineConfig{Embedder: embedder, CompoundHome: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.IndexWithError(claims, 1); err != nil {
		t.Fatal(err)
	}
	ref := referenceContains(query, docsFromClaims(claims))
	if len(ref) == 0 || ref[0].ClaimID != "decoy" {
		t.Fatalf("test needs the decoy to be the substring/reference hit; got %+v", ref)
	}
	got := eng.Query(query, mem.GateList, []string{"winner", "decoy"}, 2)
	if len(got) == 0 || got[0].ClaimID != "winner" {
		t.Fatalf("semantic winner must outrank substring decoy (no blind reference reservation); got %+v", got)
	}
}

func docsFromClaims(claims []mem.ClaimText) []doc {
	docs := make([]doc, 0, len(claims))
	for _, c := range claims {
		tokens := Tokenize(c.Text)
		freq := map[string]int{}
		for _, tok := range tokens {
			freq[tok]++
		}
		docs = append(docs, doc{claim: c, tokens: tokens, freq: freq})
	}
	return docs
}

func vectorCacheDir(t *testing.T, home string, embedder EmbedderAdapter) string {
	t.Helper()
	cache, err := newVectorCache(home, embedder.Identity())
	if err != nil {
		t.Fatal(err)
	}
	return cache.dir
}

func vectorsFilePath(t *testing.T, home string, embedder EmbedderAdapter) string {
	t.Helper()
	return filepath.Join(vectorCacheDir(t, home, embedder), "vectors.bin")
}

func readVectorsFile(t *testing.T, home string, embedder EmbedderAdapter) []byte {
	t.Helper()
	raw, err := os.ReadFile(vectorsFilePath(t, home, embedder))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func firstRowDimOffset(t *testing.T, raw []byte) int {
	t.Helper()
	const headerLen = 8 + 4 + 8
	if len(raw) < headerLen+4 {
		t.Fatalf("vectors.bin too short: %d bytes", len(raw))
	}
	idLen := int(binary.LittleEndian.Uint32(raw[headerLen : headerLen+4]))
	offset := headerLen + 4 + idLen + 32
	if len(raw) < offset+4 {
		t.Fatalf("vectors.bin too short for first row dim: %d bytes, offset %d", len(raw), offset)
	}
	return offset
}
