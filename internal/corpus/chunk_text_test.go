package corpus

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/sourceobs"
)

func TestCorpusHitCarriesChunkText(t *testing.T) {
	fixture := seedCorpusTextFixture(t, "public", "User: synthetic matched source text.")

	report, err := Query(context.Background(), fixture.homePath, fixture.lane, fixture.embedder, "matched", 1)
	if err != nil {
		t.Fatalf("query corpus: %v", err)
	}
	hit := onlyCorpusTextHit(t, report)
	text, status, hasText := corpusHitTextFields(t, hit)
	if status != "ok" {
		t.Fatalf("text status = %q, want ok", status)
	}
	if !hasText {
		t.Fatal("query hit has no text field")
	}
	if text != fixture.chunks[0].Text {
		t.Fatalf("hit text = %q, want source chunk %q", text, fixture.chunks[0].Text)
	}
}

func TestCorpusChunkTextIsNotEmbedText(t *testing.T) {
	fixture := seedCorpusTextFixture(t, "public", "Assistant: synthetic body excludes its metadata prefix.")

	report, err := Query(context.Background(), fixture.homePath, fixture.lane, fixture.embedder, "body", 1)
	if err != nil {
		t.Fatalf("query corpus: %v", err)
	}
	text, status, hasText := corpusHitTextFields(t, onlyCorpusTextHit(t, report))
	if status != "ok" || !hasText {
		t.Fatalf("text status = %q has_text=%t, want ok text", status, hasText)
	}
	if strings.Contains(text, fixture.chunks[0].Prefix) {
		t.Fatalf("hit text contains embedding prefix %q: %q", fixture.chunks[0].Prefix, text)
	}
	if text == fixture.chunks[0].EmbedText {
		t.Fatalf("hit text returned EmbedText: %q", text)
	}
}

func TestCorpusChunkTextStaleSourceIsRefused(t *testing.T) {
	fixture := seedCorpusTextFixture(t, "public", "User: synthetic indexed source text.")
	writeRaw(t, fixture.sourcePath, "User: synthetic source changed after indexing.")

	report, err := Query(context.Background(), fixture.homePath, fixture.lane, fixture.embedder, "source", 1)
	if err != nil {
		t.Fatalf("query stale corpus: %v", err)
	}
	text, status, hasText := corpusHitTextFields(t, onlyCorpusTextHit(t, report))
	if status != "stale_source" {
		t.Fatalf("text status = %q, want stale_source", status)
	}
	if hasText || text != "" {
		t.Fatalf("stale hit exposed text %q", text)
	}
}

func TestCorpusPrivateItemTextSuppressedByDefault(t *testing.T) {
	fixture := seedCorpusTextFixture(t, "private", "User: synthetic private source text.")

	report, err := Query(context.Background(), fixture.homePath, fixture.lane, fixture.embedder, "private", 1)
	if err != nil {
		t.Fatalf("query private corpus: %v", err)
	}
	text, status, hasText := corpusHitTextFields(t, onlyCorpusTextHit(t, report))
	if status != "suppressed_private" {
		t.Fatalf("text status = %q, want suppressed_private", status)
	}
	if hasText || text != "" {
		t.Fatalf("private hit exposed text by default: %q", text)
	}

	included, err := QueryWithOptions(context.Background(), fixture.homePath, fixture.lane, fixture.embedder, "private", 1, QueryOptions{IncludeText: true})
	if err != nil {
		t.Fatalf("query private corpus with text opt-in: %v", err)
	}
	text, status, hasText = corpusHitTextFields(t, onlyCorpusTextHit(t, included))
	if status != "ok" || !hasText {
		t.Fatalf("opted-in text status = %q has_text=%t, want ok text", status, hasText)
	}
	if text != fixture.chunks[0].Text {
		t.Fatalf("opted-in text = %q, want source chunk %q", text, fixture.chunks[0].Text)
	}
}

func TestCorpusChunkTextIndexOutOfRange(t *testing.T) {
	fixture := seedCorpusTextFixture(t, "public", "User: synthetic bounded source text.")
	dir := IndexDir(fixture.homePath, fixture.lane)
	manifest, err := readManifest(dir)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	manifest.Items[0].Chunks = append(manifest.Items[0].Chunks, sourceobs.SourceManifestChunk{
		Ordinal:   99,
		ChunkHash: "sha256:synthetic-out-of-range",
	})
	writeJSON(t, filepath.Join(dir, "manifest.json"), manifest)
	vectors := readVectorFile(t, filepath.Join(dir, "vectors.json"))
	vectors.Vectors[manifest.Items[0].ID+"#99"] = []float32{1, 0, 0}
	writeJSON(t, filepath.Join(dir, "vectors.json"), vectors)

	if _, err := Query(context.Background(), fixture.homePath, fixture.lane, fixture.embedder, "bounded", 10); err == nil {
		t.Fatal("query with out-of-range chunk ordinal succeeded, want error")
	}
}

// TestCorpusChunkTextStaleIndexIsRefused covers the drift the file_hash gate cannot
// see: the source is byte-identical, but the index was written by a different chunk
// schema. Re-chunking with today's chunker would then hand back text for an ordinal
// that was never embedded under that ordinal — a plausible-looking wrong answer.
func TestCorpusChunkTextStaleIndexIsRefused(t *testing.T) {
	fixture := seedCorpusTextFixture(t, "public", "User: synthetic stable source text.")
	dir := IndexDir(fixture.homePath, fixture.lane)
	manifest, err := readManifest(dir)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	manifest.PrefixSchemaVersion = "v0"
	writeJSON(t, filepath.Join(dir, "manifest.json"), manifest)

	report, err := Query(context.Background(), fixture.homePath, fixture.lane, fixture.embedder, "stable", 1)
	if err != nil {
		t.Fatalf("stale chunk schema aborted the whole query: %v", err)
	}
	text, status, hasText := corpusHitTextFields(t, onlyCorpusTextHit(t, report))
	if status != string(ChunkTextStaleIndex) {
		t.Fatalf("text status = %q, want %q", status, ChunkTextStaleIndex)
	}
	if hasText || text != "" {
		t.Fatalf("stale-index hit exposed text %q", text)
	}
}

type corpusTextFixture struct {
	homePath   string
	sourcePath string
	lane       LaneConfig
	embedder   *fakeEmbedder
	chunks     []Chunk
}

// corpusFixtureOpts carries the manifest fields a fixture wants to vary. Title
// resolution reads page_slug/source_ref/privacy, so those must be settable per test.
type corpusFixtureOpts struct {
	privacy  string
	pageSlug string
	fileName string
	body     string
}

func seedCorpusTextFixture(t *testing.T, itemPrivacy, body string) corpusTextFixture {
	t.Helper()
	return seedCorpusTextFixtureWith(t, corpusFixtureOpts{
		privacy:  itemPrivacy,
		pageSlug: "Synthetic Corpus Fixture",
		fileName: "synthetic-corpus-source.md",
		body:     body,
	})
}

func seedCorpusTextFixtureWith(t *testing.T, opts corpusFixtureOpts) corpusTextFixture {
	t.Helper()
	itemPrivacy, body := opts.privacy, opts.body
	fileName := opts.fileName
	if fileName == "" {
		fileName = "synthetic-corpus-source.md"
	}
	homePath := t.TempDir()
	sourcePath := filepath.Join(t.TempDir(), fileName)
	writeRaw(t, sourcePath, body)

	const family = "synthetic_corpus_text"
	manifest, err := sourceobs.BuildSourceManifest(family, []string{sourcePath}, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatalf("build source manifest: %v", err)
	}
	manifest.Items[0].ID = "item_fixture"
	manifest.Items[0].SourceKind = "synthetic_dialogue"
	manifest.Items[0].PageSlug = opts.pageSlug
	manifest.Items[0].Privacy = itemPrivacy
	manifest.Items[0].CorpusGeneration = "generation-fixture"
	if _, err := sourceobs.RecordSourceManifest(homePath, manifest); err != nil {
		t.Fatalf("record source manifest: %v", err)
	}

	lane := LaneConfig{
		Lane:         "corpus-synthetic",
		SourceFamily: family,
		Generation:   "generation-fixture",
		Privacy:      "public",
		Embedder: EmbedderConfig{
			Provider: "local",
			Model:    "synthetic-embedder",
			Dims:     3,
			Bits:     "f32",
		},
		Chunk: ChunkConfig{Unit: "runes", Size: 256, Overlap: 16, Boundary: "structure"},
	}
	chunks, err := ChunkItem(manifest.Items[0], body, lane.Chunk)
	if err != nil {
		t.Fatalf("chunk fixture: %v", err)
	}
	embedder := &fakeEmbedder{identity: lane.Embedder.Identity(), dim: lane.Embedder.Dims}
	if _, err := Index(context.Background(), homePath, lane, embedder, IndexOptions{Rebuild: true}); err != nil {
		t.Fatalf("index fixture: %v", err)
	}
	return corpusTextFixture{
		homePath:   homePath,
		sourcePath: sourcePath,
		lane:       lane,
		embedder:   embedder,
		chunks:     chunks,
	}
}

func onlyCorpusTextHit(t *testing.T, report QueryReport) QueryHit {
	t.Helper()
	if len(report.Hits) != 1 {
		t.Fatalf("query hits = %d, want 1", len(report.Hits))
	}
	return report.Hits[0]
}

func corpusHitTextFields(t *testing.T, hit QueryHit) (text, status string, hasText bool) {
	t.Helper()
	raw, err := json.Marshal(hit)
	if err != nil {
		t.Fatalf("marshal query hit: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("parse query hit: %v", err)
	}
	text, hasText = fields["text"].(string)
	status, _ = fields["text_status"].(string)
	return text, status, hasText
}

// TestCorpusMissingSourceDegradesOneHit guards a regression this feature could reintroduce.
// Items are body_policy=source_in_place: the body lives OUTSIDE COMPOUND_HOME and can be
// moved or deleted without the index knowing. Resolving text must therefore degrade the
// single affected hit, never abort the search — before this guard, one deleted file made
// the whole query return an error and the lane became unsearchable.
func TestCorpusMissingSourceDegradesOneHit(t *testing.T) {
	fixture := seedCorpusTextFixture(t, "public", "User: synthetic matched source text.")
	if err := os.Remove(fixture.sourcePath); err != nil {
		t.Fatalf("remove source: %v", err)
	}

	report, err := Query(context.Background(), fixture.homePath, fixture.lane, fixture.embedder, "matched", 1)
	if err != nil {
		t.Fatalf("one missing source aborted the whole query: %v", err)
	}
	hit := onlyCorpusTextHit(t, report)
	_, status, hasText := corpusHitTextFields(t, hit)
	if status != string(ChunkTextSourceUnavailable) {
		t.Fatalf("text status = %q, want %q", status, ChunkTextSourceUnavailable)
	}
	if hasText {
		t.Fatal("unreadable source must not yield text")
	}
}
