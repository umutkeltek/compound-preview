package corpus

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/sourceobs"
)

const embedBatchSize = 64

type Embedder interface {
	Identity() string
	Dim() int
	Embed(context.Context, []string) ([][]float32, error)
}

type IndexOptions struct {
	Rebuild bool
}

type IndexReport struct {
	SchemaVersion    string `json:"schema_version"`
	Lane             string `json:"lane"`
	IndexDir         string `json:"index_dir"`
	EmbedderIdentity string `json:"embedder_identity"`
	Items            int    `json:"items"`
	TotalChunks      int    `json:"total_chunks"`
	EmbeddedChunks   int    `json:"embedded_chunks"`
	ReusedChunks     int    `json:"reused_chunks"`
	Added            int    `json:"added"`
	Changed          int    `json:"changed"`
	Removed          int    `json:"removed"`
	Rebuild          bool   `json:"rebuild"`
}

type IdentityFile struct {
	SchemaVersion       string      `json:"schema_version"`
	Lane                string      `json:"lane"`
	SourceFamily        string      `json:"source_family"`
	EmbedderIdentity    string      `json:"embedder_identity"`
	PrefixSchemaVersion string      `json:"prefix_schema_version"`
	Chunk               ChunkConfig `json:"chunk"`
	GenerationLabel     string      `json:"generation_label,omitempty"`
	SourceManifestRef   string      `json:"source_manifest_ref"`
}

type Manifest struct {
	SchemaVersion       string                         `json:"schema_version"`
	Lane                string                         `json:"lane"`
	SourceFamily        string                         `json:"source_family"`
	SourceManifestRef   string                         `json:"source_manifest_ref"`
	EmbedderIdentity    string                         `json:"embedder_identity"`
	PrefixSchemaVersion string                         `json:"prefix_schema_version"`
	GenerationLabel     string                         `json:"generation_label,omitempty"`
	Chunk               ChunkConfig                    `json:"chunk"`
	Items               []sourceobs.SourceManifestItem `json:"items"`
}

type VectorFile struct {
	Vectors map[string][]float32 `json:"vectors"`
}

type ProgressFile struct {
	SchemaVersion    string `json:"schema_version"`
	Lane             string `json:"lane"`
	EmbedderIdentity string `json:"embedder_identity"`
	TotalChunks      int    `json:"total_chunks"`
	EmbeddedChunks   int    `json:"embedded_chunks"`
	PendingChunks    int    `json:"pending_chunks"`
	LastChunkID      string `json:"last_chunk_id,omitempty"`
	UpdatedAt        string `json:"updated_at"`
	Error            string `json:"error,omitempty"`
}

type preparedIndex struct {
	identity IdentityFile
	manifest Manifest
	chunks   []Chunk
	hashes   map[string]string
}

func Index(ctx context.Context, homePath string, lane LaneConfig, embedder Embedder, opts IndexOptions) (IndexReport, error) {
	lane, err := ValidateLaneConfig(lane)
	if err != nil {
		return IndexReport{}, err
	}
	if err := validateEmbedder(lane, embedder); err != nil {
		return IndexReport{}, err
	}
	dir := IndexDir(homePath, lane)
	prepared, err := prepareIndex(homePath, lane)
	if err != nil {
		return IndexReport{}, err
	}
	if err := refuseMixedIdentity(dir, prepared.identity.EmbedderIdentity); err != nil {
		return IndexReport{}, err
	}
	oldManifest, _ := readManifest(dir)
	oldVectors, _ := readVectors(dir)
	currentVectors := map[string][]float32{}
	reused := 0
	if !opts.Rebuild && oldVectors.Vectors != nil {
		oldHashes := manifestChunkHashes(oldManifest)
		for _, chunk := range prepared.chunks {
			if oldHashes[chunk.ID] != chunk.ChunkHash {
				continue
			}
			vec := oldVectors.Vectors[chunk.ID]
			if len(vec) != lane.Embedder.Dims {
				continue
			}
			normalized, err := normalizeVector(vec)
			if err != nil {
				continue
			}
			currentVectors[chunk.ID] = normalized
			reused++
		}
	}
	report := IndexReport{
		SchemaVersion:    ManifestSchemaVersion,
		Lane:             lane.Lane,
		IndexDir:         dir,
		EmbedderIdentity: lane.Embedder.Identity(),
		Items:            len(prepared.manifest.Items),
		TotalChunks:      len(prepared.chunks),
		ReusedChunks:     reused,
		Rebuild:          opts.Rebuild,
	}
	report.Added, report.Changed, report.Removed = stalenessCounts(oldManifest, prepared.manifest)
	pending := pendingChunks(prepared.chunks, currentVectors)
	for start := 0; start < len(pending); start += embedBatchSize {
		end := start + embedBatchSize
		if end > len(pending) {
			end = len(pending)
		}
		batch := pending[start:end]
		texts := make([]string, 0, len(batch))
		for _, chunk := range batch {
			texts = append(texts, chunk.EmbedText)
		}
		vectors, err := embedder.Embed(ctx, texts)
		if err != nil {
			writeErr := persistIndex(dir, prepared, currentVectors, progressFor(lane, len(prepared.chunks), currentVectors, err.Error()))
			if writeErr != nil {
				return report, fmt.Errorf("%w (also failed persisting partial progress: %v)", err, writeErr)
			}
			return report, err
		}
		if len(vectors) != len(batch) {
			err := fmt.Errorf("embedder returned %d vectors for %d chunks", len(vectors), len(batch))
			_ = persistIndex(dir, prepared, currentVectors, progressFor(lane, len(prepared.chunks), currentVectors, err.Error()))
			return report, err
		}
		for i, vec := range vectors {
			normalized, err := normalizeVector(vec)
			if err != nil {
				err = fmt.Errorf("chunk %s vector: %w", batch[i].ID, err)
				_ = persistIndex(dir, prepared, currentVectors, progressFor(lane, len(prepared.chunks), currentVectors, err.Error()))
				return report, err
			}
			if len(normalized) != lane.Embedder.Dims {
				err := fmt.Errorf("chunk %s vector dim %d != %d", batch[i].ID, len(normalized), lane.Embedder.Dims)
				_ = persistIndex(dir, prepared, currentVectors, progressFor(lane, len(prepared.chunks), currentVectors, err.Error()))
				return report, err
			}
			currentVectors[batch[i].ID] = normalized
		}
		if err := persistIndex(dir, prepared, currentVectors, progressFor(lane, len(prepared.chunks), currentVectors, "")); err != nil {
			return report, err
		}
	}
	if len(pending) == 0 {
		if err := persistIndex(dir, prepared, currentVectors, progressFor(lane, len(prepared.chunks), currentVectors, "")); err != nil {
			return report, err
		}
	}
	report.EmbeddedChunks = len(currentVectors)
	return report, nil
}

func prepareIndex(homePath string, lane LaneConfig) (preparedIndex, error) {
	manifest, err := sourceobs.LoadSourceManifest(homePath, lane.SourceFamily)
	if err != nil {
		return preparedIndex{}, err
	}
	generationLabel := lane.Generation
	if generationLabel == "" && manifest.FamilyMeta != nil {
		generationLabel = strings.TrimSpace(manifest.FamilyMeta.GenerationLabel)
	}
	out := Manifest{
		SchemaVersion:       ManifestSchemaVersion,
		Lane:                lane.Lane,
		SourceFamily:        lane.SourceFamily,
		SourceManifestRef:   manifest.AuthorityRef,
		EmbedderIdentity:    lane.Embedder.Identity(),
		PrefixSchemaVersion: PrefixSchemaVersion,
		GenerationLabel:     generationLabel,
		Chunk:               lane.Chunk,
	}
	var chunks []Chunk
	for _, item := range manifest.Items {
		if lane.Generation != "" && strings.TrimSpace(item.CorpusGeneration) != lane.Generation {
			continue
		}
		// The shared current-item predicate is deliberately before body access:
		// rebuilds and incremental indexing have the same zero-read/zero-embedder
		// refusal boundary as query and body resolution.
		if err := validateCurrentItemAccess(lane, item); err != nil {
			return preparedIndex{}, fmt.Errorf("corpus index: %w", err)
		}
		body, err := readItemBody(homePath, item)
		if err != nil {
			return preparedIndex{}, err
		}
		itemChunks, err := ChunkItem(item, body, lane.Chunk)
		if err != nil {
			return preparedIndex{}, err
		}
		copyItem := item
		copyItem.Chunks = make([]sourceobs.SourceManifestChunk, 0, len(itemChunks))
		for _, chunk := range itemChunks {
			copyItem.Chunks = append(copyItem.Chunks, sourceobs.SourceManifestChunk{Ordinal: chunk.Ordinal, ChunkHash: chunk.ChunkHash})
			chunks = append(chunks, chunk)
		}
		copyItem.PageHash = sourceobs.HashGbrainPageFromChunks(copyItem.Chunks)
		copyItem.Embedding = &sourceobs.SourceManifestEmbedding{
			Provider: lane.Embedder.Provider,
			Model:    lane.Embedder.Model,
			Dims:     lane.Embedder.Dims,
		}
		if copyItem.CorpusGeneration == "" {
			copyItem.CorpusGeneration = generationLabel
		}
		out.Items = append(out.Items, copyItem)
	}
	hashes := map[string]string{}
	for _, chunk := range chunks {
		hashes[chunk.ID] = chunk.ChunkHash
	}
	return preparedIndex{
		identity: IdentityFile{
			SchemaVersion:       IdentitySchemaVersion,
			Lane:                lane.Lane,
			SourceFamily:        lane.SourceFamily,
			EmbedderIdentity:    lane.Embedder.Identity(),
			PrefixSchemaVersion: PrefixSchemaVersion,
			Chunk:               lane.Chunk,
			GenerationLabel:     generationLabel,
			SourceManifestRef:   manifest.AuthorityRef,
		},
		manifest: out,
		chunks:   chunks,
		hashes:   hashes,
	}, nil
}

func validateEmbedder(lane LaneConfig, embedder Embedder) error {
	if embedder == nil {
		return fmt.Errorf("embedder required")
	}
	if strings.TrimSpace(embedder.Identity()) != lane.Embedder.Identity() {
		return fmt.Errorf("%w: config %q embedder %q", ErrEmbedderMismatch, lane.Embedder.Identity(), embedder.Identity())
	}
	if dim := embedder.Dim(); dim > 0 && dim != lane.Embedder.Dims {
		return fmt.Errorf("embedder dim %d != config dim %d", dim, lane.Embedder.Dims)
	}
	return nil
}

func refuseMixedIdentity(dir, expected string) error {
	identity, err := readIdentity(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if identity.EmbedderIdentity != "" && identity.EmbedderIdentity != expected {
		return fmt.Errorf("%w: index %q configured %q", ErrEmbedderMismatch, dir, identity.EmbedderIdentity)
	}
	return nil
}

func readItemBody(homePath string, item sourceobs.SourceManifestItem) (string, error) {
	raw, status, err := readItemBodyBytes(homePath, item)
	if err != nil {
		return "", fmt.Errorf("read manifest item %s: %w", item.ID, err)
	}
	// Indexing must surface missing/corrupt/wrong-size bodies as a hard error
	// rather than a per-hit refusal: the embedder cannot chunk empty bytes,
	// and a silent skip would leave a partial index behind. The corpus query
	// path keeps using readItemBodyBytes' refusal statuses directly.
	if status != ChunkTextOK {
		switch item.BodyPolicy {
		case "owned_snapshot":
			return "", fmt.Errorf("read manifest item %s owned body %s: %s (artifact missing, corrupt, or size-mismatch)", item.ID, item.BodyRef, status)
		default:
			return "", fmt.Errorf("read manifest item %s source: %s", item.ID, status)
		}
	}
	return string(raw), nil
}

func pendingChunks(chunks []Chunk, vectors map[string][]float32) []Chunk {
	var out []Chunk
	for _, chunk := range chunks {
		if _, ok := vectors[chunk.ID]; !ok {
			out = append(out, chunk)
		}
	}
	return out
}

func persistIndex(dir string, prepared preparedIndex, vectors map[string][]float32, progress ProgressFile) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(dir, "identity.json"), prepared.identity); err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(dir, "manifest.json"), prepared.manifest); err != nil {
		return err
	}
	ordered := VectorFile{Vectors: map[string][]float32{}}
	ids := make([]string, 0, len(vectors))
	for id := range vectors {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		ordered.Vectors[id] = vectors[id]
	}
	if err := writeJSONFile(filepath.Join(dir, "vectors.json"), ordered); err != nil {
		return err
	}
	return writeJSONFile(filepath.Join(dir, "progress.json"), progress)
}

func progressFor(lane LaneConfig, total int, vectors map[string][]float32, errText string) ProgressFile {
	last := ""
	ids := make([]string, 0, len(vectors))
	for id := range vectors {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) > 0 {
		last = ids[len(ids)-1]
	}
	return ProgressFile{
		SchemaVersion:    ProgressSchemaVersion,
		Lane:             lane.Lane,
		EmbedderIdentity: lane.Embedder.Identity(),
		TotalChunks:      total,
		EmbeddedChunks:   len(vectors),
		PendingChunks:    total - len(vectors),
		LastChunkID:      last,
		UpdatedAt:        time.Now().UTC().Format(time.RFC3339),
		Error:            errText,
	}
}

func readIdentity(dir string) (IdentityFile, error) {
	var out IdentityFile
	if err := readJSONFile(filepath.Join(dir, "identity.json"), &out); err != nil {
		return IdentityFile{}, err
	}
	return out, nil
}

func readManifest(dir string) (Manifest, error) {
	var out Manifest
	if err := readJSONFile(filepath.Join(dir, "manifest.json"), &out); err != nil {
		return Manifest{}, err
	}
	return out, nil
}

func readVectors(dir string) (VectorFile, error) {
	var out VectorFile
	if err := readJSONFile(filepath.Join(dir, "vectors.json"), &out); err != nil {
		return VectorFile{}, err
	}
	if out.Vectors == nil {
		out.Vectors = map[string][]float32{}
	}
	return out, nil
}

func readJSONFile(path string, target any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func writeJSONFile(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

func manifestChunkHashes(manifest Manifest) map[string]string {
	out := map[string]string{}
	for _, item := range manifest.Items {
		for _, chunk := range item.Chunks {
			out[fmt.Sprintf("%s#%d", item.ID, chunk.Ordinal)] = strings.TrimSpace(chunk.ChunkHash)
		}
	}
	return out
}

func stalenessCounts(oldManifest, current Manifest) (added, changed, removed int) {
	oldItems := map[string]sourceobs.SourceManifestItem{}
	for _, item := range oldManifest.Items {
		oldItems[item.ID] = item
	}
	currentItems := map[string]sourceobs.SourceManifestItem{}
	for _, item := range current.Items {
		currentItems[item.ID] = item
		old, ok := oldItems[item.ID]
		switch {
		case !ok:
			added++
		case old.FileHash != item.FileHash || old.CorpusGeneration != item.CorpusGeneration:
			changed++
		}
	}
	for id := range oldItems {
		if _, ok := currentItems[id]; !ok {
			removed++
		}
	}
	return added, changed, removed
}

func normalizeVector(vec []float32) ([]float32, error) {
	if len(vec) == 0 {
		return nil, fmt.Errorf("empty vector")
	}
	var sum float64
	for _, v := range vec {
		fv := float64(v)
		if math.IsNaN(fv) || math.IsInf(fv, 0) {
			return nil, fmt.Errorf("non-finite vector")
		}
		sum += fv * fv
	}
	if sum == 0 {
		return nil, fmt.Errorf("zero vector")
	}
	inv := 1 / math.Sqrt(sum)
	out := make([]float32, len(vec))
	for i, v := range vec {
		out[i] = float32(float64(v) * inv)
	}
	return out, nil
}

// laneIsPrivateLocal reports whether the configured lane posture is a
// private-lane + local-embedder pair. It composes the existing
// ProviderIsLocal helper (loopback host check) with the lane's privacy
// string so no new config authority is introduced — the gate re-uses the
// same predicate the lane-validator already relies on for private lanes.
//
// A "private" lane string is necessary but not sufficient — the embedder
// must also resolve to a loopback/local provider so the bytes never leave
// the host. This is the same posture ValidateLaneConfig requires for any
// privacy=private lane to load at all.
func laneIsPrivateLocal(lane LaneConfig) bool {
	if !strings.EqualFold(strings.TrimSpace(lane.Privacy), "private") {
		return false
	}
	return ProviderIsLocal(lane.Embedder.Provider, lane.Embedder.BaseURL)
}
