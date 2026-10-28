package capgraph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

const capgraphSemanticCacheSchemaVersion = "compound.capgraph.semantic_cache.v0.1"

var (
	errNodeVectorCacheMiss             = errors.New("node vector cache missing")
	errNodeVectorCacheCorrupt          = errors.New("node vector cache corrupt")
	errNodeVectorCacheIdentityMismatch = errors.New("node vector cache identity mismatch")
)

// NodeEmbedder is structural so capgraph never imports the breadth engine.
type NodeEmbedder interface {
	Identity() string
	Dim() int
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

type nodeVectorManifest struct {
	SchemaVersion    string                          `json:"schema_version"`
	EmbedderIdentity string                          `json:"embedder_identity"`
	Dim              int                             `json:"dim"`
	Nodes            map[string]nodeVectorCacheEntry `json:"nodes"`
}

type nodeVectorCacheEntry struct {
	ContentHash string `json:"content_hash"`
}

type nodeVectorFile struct {
	Vectors map[string][]float32 `json:"vectors"`
}

func nodeEmbedText(n Node) string {
	parts := make([]string, 0, 1+len(n.Triggers)+1+len(n.When)+len(n.Boundary))
	parts = appendNonEmpty(parts, n.Name)
	parts = append(parts, n.Triggers...)
	parts = appendNonEmpty(parts, n.Kind)
	for _, when := range n.When {
		parts = append(parts, when.Values...)
	}
	parts = append(parts, n.Boundary...)
	return strings.Join(parts, " ")
}

func semanticScores(ctx context.Context, embedder NodeEmbedder, compoundHome string, nodes []Node, task string) (map[string]float64, error) {
	if embedder == nil {
		return nil, nil
	}
	taskVectors, err := embedder.Embed(ctx, []string{task})
	if err != nil {
		return nil, err
	}
	if len(taskVectors) != 1 {
		return nil, fmt.Errorf("semantic task embed returned %d vectors", len(taskVectors))
	}
	taskVector, err := normalizeSemanticVector(taskVectors[0])
	if err != nil {
		return nil, fmt.Errorf("semantic task vector: %w", err)
	}
	identity := strings.TrimSpace(embedder.Identity())
	if identity == "" {
		identity = "unknown"
	}
	dim := embedder.Dim()
	if dim <= 0 {
		dim = len(taskVector)
	}
	cacheDir := filepath.Join(compoundHome, "indexes", "capgraph", sanitizedEmbedderIdentity(identity))
	manifest, vectors, err := readNodeVectorCache(cacheDir, identity, dim)
	if err != nil {
		switch {
		case errors.Is(err, errNodeVectorCacheMiss), errors.Is(err, errNodeVectorCacheCorrupt):
			manifest = nodeVectorManifest{}
			vectors = nil
		default:
			return nil, err
		}
	}
	currentManifest := nodeVectorManifest{
		SchemaVersion:    capgraphSemanticCacheSchemaVersion,
		EmbedderIdentity: identity,
		Dim:              dim,
		Nodes:            map[string]nodeVectorCacheEntry{},
	}
	currentVectors := map[string][]float32{}
	type pendingNode struct {
		id          string
		text        string
		contentHash string
	}
	var pending []pendingNode
	for _, node := range nodes {
		text := nodeEmbedText(node)
		contentHash := semanticContentHash(text)
		currentManifest.Nodes[node.ID] = nodeVectorCacheEntry{ContentHash: contentHash}
		if manifest.Nodes != nil && manifest.Nodes[node.ID].ContentHash == contentHash {
			if vec := vectors[node.ID]; len(vec) == dim {
				currentVectors[node.ID] = vec
				continue
			}
		}
		pending = append(pending, pendingNode{id: node.ID, text: text, contentHash: contentHash})
	}
	// Embed pending nodes in bounded chunks and persist progress when a chunk
	// fails: a cold start over a large graph cannot finish inside one surfacing
	// timeout, and without partial persistence every later call would re-embed
	// the full set and time out the same way — a permanently degraded lane.
	for start := 0; start < len(pending); start += semanticEmbedChunk {
		end := start + semanticEmbedChunk
		if end > len(pending) {
			end = len(pending)
		}
		chunk := pending[start:end]
		texts := make([]string, 0, len(chunk))
		for _, item := range chunk {
			texts = append(texts, item.text)
		}
		embedded, err := embedder.Embed(ctx, texts)
		if err != nil {
			_ = writeNodeVectorCache(cacheDir, partialNodeVectorManifest(currentManifest, currentVectors), currentVectors)
			return nil, err
		}
		if len(embedded) != len(chunk) {
			return nil, fmt.Errorf("semantic node embed returned %d vectors for %d nodes", len(embedded), len(chunk))
		}
		for i, vec := range embedded {
			normalized, err := normalizeSemanticVector(vec)
			if err != nil {
				return nil, fmt.Errorf("semantic node vector %s: %w", chunk[i].id, err)
			}
			if len(normalized) != dim {
				return nil, fmt.Errorf("semantic node vector %s dim %d != %d", chunk[i].id, len(normalized), dim)
			}
			currentVectors[chunk[i].id] = normalized
		}
	}
	cacheCurrent := len(pending) == 0 && nodeVectorCacheCurrent(manifest, vectors, currentManifest, currentVectors)
	// Cache write failures should not degrade a successful semantic ranking; the
	// next call can rebuild from the same source graph.
	if !cacheCurrent {
		_ = writeNodeVectorCache(cacheDir, currentManifest, currentVectors)
	}
	out := make(map[string]float64, len(currentVectors))
	for id, vec := range currentVectors {
		if len(vec) == len(taskVector) {
			out[id] = semanticCosine(taskVector, vec)
		}
	}
	return out, nil
}

func nodeVectorCacheCurrent(loaded nodeVectorManifest, loadedVectors map[string][]float32, current nodeVectorManifest, currentVectors map[string][]float32) bool {
	if loaded.SchemaVersion != current.SchemaVersion || loaded.EmbedderIdentity != current.EmbedderIdentity || loaded.Dim != current.Dim ||
		len(loaded.Nodes) != len(current.Nodes) || len(loadedVectors) != len(currentVectors) {
		return false
	}
	for id, entry := range current.Nodes {
		if loaded.Nodes[id] != entry || len(loadedVectors[id]) != current.Dim || len(currentVectors[id]) != current.Dim {
			return false
		}
	}
	return true
}

// semanticEmbedChunk bounds one embed request so an interrupted cold start
// keeps per-chunk progress. Sized to a typical embedding-server batch.
const semanticEmbedChunk = 64

// partialNodeVectorManifest keeps only entries whose vectors were actually
// embedded, so an interrupted cold start never records a content hash it
// cannot back with a vector.
func partialNodeVectorManifest(full nodeVectorManifest, vectors map[string][]float32) nodeVectorManifest {
	out := nodeVectorManifest{
		SchemaVersion:    full.SchemaVersion,
		EmbedderIdentity: full.EmbedderIdentity,
		Dim:              full.Dim,
		Nodes:            map[string]nodeVectorCacheEntry{},
	}
	for id := range vectors {
		if entry, ok := full.Nodes[id]; ok {
			out.Nodes[id] = entry
		}
	}
	return out
}

func readNodeVectorCache(dir, identity string, dim int) (nodeVectorManifest, map[string][]float32, error) {
	manifest, err := readNodeVectorManifest(dir)
	if err != nil {
		return nodeVectorManifest{}, nil, classifyNodeVectorCacheRead("manifest", err)
	}
	if manifest.SchemaVersion != capgraphSemanticCacheSchemaVersion {
		return nodeVectorManifest{}, nil, fmt.Errorf("%w: schema %q", errNodeVectorCacheCorrupt, manifest.SchemaVersion)
	}
	if manifest.EmbedderIdentity != identity {
		return nodeVectorManifest{}, nil, fmt.Errorf("%w: cache %q configured %q", errNodeVectorCacheIdentityMismatch, manifest.EmbedderIdentity, identity)
	}
	if manifest.Dim != dim {
		return nodeVectorManifest{}, nil, fmt.Errorf("%w: cache dim %d configured dim %d", errNodeVectorCacheIdentityMismatch, manifest.Dim, dim)
	}
	vectors, err := readNodeVectors(dir, dim)
	if err != nil {
		return nodeVectorManifest{}, nil, classifyNodeVectorCacheRead("vectors", err)
	}
	return manifest, vectors, nil
}

func classifyNodeVectorCacheRead(component string, err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s: %v", errNodeVectorCacheMiss, component, err)
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return fmt.Errorf("read node vector cache %s: %w", component, err)
	}
	return fmt.Errorf("%w: %s: %v", errNodeVectorCacheCorrupt, component, err)
}

func readNodeVectorManifest(dir string) (nodeVectorManifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nodeVectorManifest{}, err
	}
	var manifest nodeVectorManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nodeVectorManifest{}, err
	}
	return manifest, nil
}

func readNodeVectors(dir string, dim int) (map[string][]float32, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "vectors.json"))
	if err != nil {
		return nil, err
	}
	var file nodeVectorFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, err
	}
	if file.Vectors == nil {
		return nil, errors.New("missing vectors")
	}
	out := make(map[string][]float32, len(file.Vectors))
	for id, vec := range file.Vectors {
		normalized, err := normalizeSemanticVector(vec)
		if err != nil {
			return nil, err
		}
		if len(normalized) != dim {
			return nil, fmt.Errorf("vector %s dim %d != %d", id, len(normalized), dim)
		}
		out[id] = normalized
	}
	return out, nil
}

func writeNodeVectorCache(dir string, manifest nodeVectorManifest, vectors map[string][]float32) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	nodeIDs := make([]string, 0, len(vectors))
	for id := range vectors {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Strings(nodeIDs)
	ordered := nodeVectorFile{Vectors: map[string][]float32{}}
	for _, id := range nodeIDs {
		ordered.Vectors[id] = vectors[id]
	}
	vectorsRaw, err := json.MarshalIndent(ordered, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "vectors.json"), append(vectorsRaw, '\n'), 0o644); err != nil {
		return err
	}
	manifestRaw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), append(manifestRaw, '\n'), 0o644)
}

func sanitizedEmbedderIdentity(identity string) string {
	var b strings.Builder
	for _, r := range identity {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := strings.Trim(b.String(), "_")
	if name == "" {
		name = "unknown"
	}
	sum := sha256.Sum256([]byte(identity))
	return name + "-" + hex.EncodeToString(sum[:])[:12]
}

func semanticContentHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func normalizeSemanticVector(vec []float32) ([]float32, error) {
	if len(vec) == 0 {
		return nil, errors.New("empty vector")
	}
	var sum float64
	for _, v := range vec {
		fv := float64(v)
		if math.IsNaN(fv) || math.IsInf(fv, 0) {
			return nil, errors.New("non-finite vector")
		}
		sum += fv * fv
	}
	if sum == 0 {
		return nil, errors.New("zero vector")
	}
	inv := 1 / math.Sqrt(sum)
	out := make([]float32, len(vec))
	for i, v := range vec {
		out[i] = float32(float64(v) * inv)
	}
	return out, nil
}

func semanticCosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}

func appendNonEmpty(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	return append(values, value)
}
