package corpus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/sourceobs"
)

func TestChunkItemBuildsCappedDeterministicPrefixedChunks(t *testing.T) {
	item := sourceobs.SourceManifestItem{
		ID:         "item_a",
		SourceKind: "chatgpt",
		PageSlug:   strings.Repeat("long-title-", 20),
		CompoundEntityRefs: []string{
			"entity:zeta",
			"entity:alpha",
			"entity:alpha",
			"entity:" + strings.Repeat("wide", 20),
		},
	}
	body := "User: keep this exact opening\n\nAssistant: keep this exact answer"

	chunks, err := ChunkItem(item, body, ChunkConfig{Unit: "runes", Size: 80, Overlap: 10, Boundary: "structure"})
	if err != nil {
		t.Fatalf("chunk item: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatalf("expected chunks")
	}
	first := chunks[0]
	if first.ID != "item_a#0" || first.Ordinal != 0 {
		t.Fatalf("first chunk identity = %s/%d", first.ID, first.Ordinal)
	}
	prefix := first.Prefix
	if !strings.HasPrefix(prefix, "[chatgpt") || !strings.HasSuffix(prefix, "]") {
		t.Fatalf("prefix shape = %q", prefix)
	}
	if got := runeLen(prefix); got > 240 {
		t.Fatalf("prefix runes = %d, want <= 240: %q", got, prefix)
	}
	if !strings.Contains(first.EmbedText, "keep this exact opening") {
		t.Fatalf("chunk text was truncated inside embed text: %q", first.EmbedText)
	}
	sum := sha256.Sum256([]byte(first.EmbedText))
	if first.ChunkHash != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("chunk hash not over fully prefixed embed text")
	}
}

func TestIndexRunResumesPersistedProgress(t *testing.T) {
	homePath := t.TempDir()
	lane, totalChunks := seedCorpusLane(t, homePath, 70, 24, 0)
	first := &fakeEmbedder{identity: lane.Embedder.Identity(), dim: lane.Embedder.Dims, failOnCall: 2}

	_, err := Index(context.Background(), homePath, lane, first, IndexOptions{Rebuild: true})
	if err == nil {
		t.Fatalf("first index run should fail after persisting first batch")
	}
	if first.embedded != 64 {
		t.Fatalf("first run embedded %d texts, want one persisted batch of 64", first.embedded)
	}

	second := &fakeEmbedder{identity: lane.Embedder.Identity(), dim: lane.Embedder.Dims}
	report, err := Index(context.Background(), homePath, lane, second, IndexOptions{})
	if err != nil {
		t.Fatalf("resume index: %v", err)
	}
	if report.TotalChunks != totalChunks {
		t.Fatalf("total chunks = %d, want %d", report.TotalChunks, totalChunks)
	}
	if second.embedded != totalChunks-64 {
		t.Fatalf("resume embedded %d texts, want only pending %d", second.embedded, totalChunks-64)
	}
	vectors := readVectorFile(t, filepath.Join(IndexDir(homePath, lane), "vectors.json"))
	if len(vectors.Vectors) != totalChunks {
		t.Fatalf("vectors persisted = %d, want %d", len(vectors.Vectors), totalChunks)
	}
}

func TestRebuildDeterminism(t *testing.T) {
	homePath := t.TempDir()
	lane, _ := seedCorpusLane(t, homePath, 8, 48, 4)
	embedder := &fakeEmbedder{identity: lane.Embedder.Identity(), dim: lane.Embedder.Dims}
	if _, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true}); err != nil {
		t.Fatalf("first rebuild: %v", err)
	}
	firstHits, err := Query(context.Background(), homePath, lane, embedder, "needle", 5)
	if err != nil {
		t.Fatalf("first query: %v", err)
	}
	if _, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true}); err != nil {
		t.Fatalf("second rebuild: %v", err)
	}
	secondHits, err := Query(context.Background(), homePath, lane, embedder, "needle", 5)
	if err != nil {
		t.Fatalf("second query: %v", err)
	}
	if !reflect.DeepEqual(firstHits.Hits, secondHits.Hits) {
		t.Fatalf("rebuild changed hit set\nfirst:  %+v\nsecond: %+v", firstHits.Hits, secondHits.Hits)
	}
}

func TestDoctorFailsWhenChunkRecordMissingOrdinalOrHash(t *testing.T) {
	homePath := t.TempDir()
	lane, _ := seedCorpusLane(t, homePath, 1, 48, 0)
	dir := IndexDir(homePath, lane)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(dir, "identity.json"), map[string]any{
		"schema_version":        IdentitySchemaVersion,
		"embedder_identity":     lane.Embedder.Identity(),
		"prefix_schema_version": "v1",
	})
	writeRaw(t, filepath.Join(dir, "manifest.json"), `{
  "schema_version": "compound.corpus.manifest.v0.1",
  "lane": "corpus-fixture",
  "items": [{
    "id": "item_a",
    "source_kind": "chatgpt",
    "source_ref": "fixture",
    "file_hash": "sha256:x",
    "privacy": "private",
    "body_policy": "source_in_place",
    "chunks": [{"chunk_hash": "sha256:ok"}, {"ordinal": 1}]
  }]
}`)
	writeJSON(t, filepath.Join(dir, "vectors.json"), VectorFile{Vectors: map[string][]float32{}})

	report, err := Doctor(homePath, lane)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "fail" || report.MissingChunkRecords != 2 {
		t.Fatalf("doctor report = %+v, want fail with two missing chunk fields", report)
	}
}

func TestDoctorAcceptsOwnedZeroByteItemWithoutChunks(t *testing.T) {
	homePath := t.TempDir()
	lane, _ := seedCorpusLane(t, homePath, 1, 48, 0)
	dir := IndexDir(homePath, lane)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(dir, "identity.json"), map[string]any{
		"schema_version":        IdentitySchemaVersion,
		"embedder_identity":     lane.Embedder.Identity(),
		"prefix_schema_version": "v1",
	})
	const emptyDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	writeRaw(t, filepath.Join(dir, "manifest.json"), `{
  "schema_version": "compound.corpus.manifest.v0.1",
  "lane": "corpus-fixture",
  "items": [{
    "id": "item_empty",
    "source_kind": "markdown",
    "source_ref": "fixture-empty.md",
    "file_hash": "sha256:`+emptyDigest+`",
    "privacy": "private",
    "body_policy": "owned_snapshot",
    "body_ref": "artifact:sha256:`+emptyDigest+`",
    "chunks": []
  }]
}`)
	writeJSON(t, filepath.Join(dir, "vectors.json"), VectorFile{Vectors: map[string][]float32{}})

	report, err := Doctor(homePath, lane)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "ok" || report.Items != 1 || report.Chunks != 0 || report.EmbeddedChunks != 0 {
		t.Fatalf("zero-byte doctor report = %+v", report)
	}
}

func TestIndexRefusesMixedEmbedderIdentityInIndexDir(t *testing.T) {
	homePath := t.TempDir()
	lane, _ := seedCorpusLane(t, homePath, 1, 48, 0)
	dir := IndexDir(homePath, lane)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(dir, "identity.json"), map[string]any{
		"schema_version":        IdentitySchemaVersion,
		"embedder_identity":     "local:other:3:f32",
		"prefix_schema_version": "v1",
	})
	embedder := &fakeEmbedder{identity: lane.Embedder.Identity(), dim: lane.Embedder.Dims}
	_, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{})
	if !errors.Is(err, ErrEmbedderMismatch) {
		t.Fatalf("index error = %v, want ErrEmbedderMismatch", err)
	}
}

type fakeEmbedder struct {
	identity   string
	dim        int
	failOnCall int
	calls      int
	embedded   int
	texts      []string
}

func (f *fakeEmbedder) Identity() string { return f.identity }
func (f *fakeEmbedder) Dim() int         { return f.dim }

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	if f.failOnCall > 0 && f.calls == f.failOnCall {
		return nil, fmt.Errorf("fake embedder interrupted")
	}
	f.embedded += len(texts)
	f.texts = append(f.texts, texts...)
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		sum := sha256.Sum256([]byte(text))
		vec := make([]float32, f.dim)
		for i := range vec {
			vec[i] = float32(int(sum[i%len(sum)]) + 1)
		}
		out = append(out, vec)
	}
	return out, nil
}

func seedCorpusLane(t *testing.T, homePath string, paragraphs, chunkSize, overlap int) (LaneConfig, int) {
	t.Helper()
	family := "corpus_fixture"
	bodyPath := filepath.Join(t.TempDir(), "chatgpt-fixture.md")
	var b strings.Builder
	for i := 0; i < paragraphs; i++ {
		fmt.Fprintf(&b, "User: needle paragraph %03d has stable body text.\n\n", i)
	}
	writeRaw(t, bodyPath, b.String())
	manifest, err := sourceobs.BuildSourceManifest(family, []string{bodyPath}, time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("build source manifest: %v", err)
	}
	manifest.Items[0].ID = "item_a"
	manifest.Items[0].SourceKind = "chatgpt"
	manifest.Items[0].PageSlug = "Synthetic ChatGPT Fixture"
	manifest.Items[0].CorpusGeneration = "gen-a"
	if _, err := sourceobs.RecordSourceManifest(homePath, manifest); err != nil {
		t.Fatalf("record source manifest: %v", err)
	}
	lane := LaneConfig{
		Lane:         "corpus-fixture",
		SourceFamily: family,
		Generation:   "gen-a",
		Privacy:      "private",
		Embedder: EmbedderConfig{
			Provider: "local",
			Model:    "fake",
			Dims:     3,
			Bits:     "f32",
		},
		Chunk: ChunkConfig{Unit: "runes", Size: chunkSize, Overlap: overlap, Boundary: "structure"},
	}
	chunks, err := ChunkItem(manifest.Items[0], b.String(), lane.Chunk)
	if err != nil {
		t.Fatalf("chunk fixture: %v", err)
	}
	return lane, len(chunks)
}

func readVectorFile(t *testing.T, path string) VectorFile {
	t.Helper()
	var out VectorFile
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	return out
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	writeRaw(t, path, string(append(raw, '\n')))
}

func writeRaw(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
