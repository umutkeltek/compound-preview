package breadth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

const (
	defaultEmbedBatch     = 32
	defaultEmbedTimeout   = 10 * time.Second
	defaultEmbedBits      = "f32"
	defaultOllamaBaseURL  = "http://" + "localhost:11434/v1"
	openAIEmbeddingMaxRaw = 16 << 20
)

// EmbedderAdapter is the only semantic-lane dependency the breadth engine owns.
// Implementations return L2-normalized f32 vectors and reject non-finite values.
type EmbedderAdapter interface {
	Identity() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dim() int
}

type OpenAICompatEmbedderConfig struct {
	Provider   string
	Model      string
	BaseURL    string
	APIKey     string
	Batch      int
	Timeout    time.Duration
	HTTPClient *http.Client
}

type openAICompatEmbedder struct {
	provider string
	model    string
	baseURL  string
	apiKey   string
	batch    int
	timeout  time.Duration
	client   *http.Client
	dim      int
}

func NewOpenAICompatEmbedder(cfg OpenAICompatEmbedderConfig) (EmbedderAdapter, error) {
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if provider == "" {
		provider = "ollama"
	}
	if provider != "ollama" && provider != "openai" {
		return nil, fmt.Errorf("unsupported embed provider %q", cfg.Provider)
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		return nil, errors.New("embed model is required")
	}
	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		baseURL = defaultOllamaBaseURL
	}
	baseURL = normalizeOpenAICompatBaseURL(baseURL)
	batch := cfg.Batch
	if batch <= 0 {
		batch = defaultEmbedBatch
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultEmbedTimeout
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &openAICompatEmbedder{
		provider: provider,
		model:    model,
		baseURL:  strings.TrimRight(baseURL, "/"),
		apiKey:   strings.TrimSpace(cfg.APIKey),
		batch:    batch,
		timeout:  timeout,
		client:   client,
		dim:      inferEmbeddingDim(model),
	}, nil
}

func (e *openAICompatEmbedder) Identity() string {
	return fmt.Sprintf("%s:%s:%d:%s", e.provider, e.model, e.Dim(), defaultEmbedBits)
}

func (e *openAICompatEmbedder) Dim() int { return e.dim }

func (e *openAICompatEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += e.batch {
		end := start + e.batch
		if end > len(texts) {
			end = len(texts)
		}
		vecs, err := e.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	if len(out) != len(texts) {
		return nil, fmt.Errorf("embedder returned %d vectors for %d texts", len(out), len(texts))
	}
	return out, nil
}

type openAIEmbeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type openAIEmbeddingResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error any `json:"error,omitempty"`
}

func (e *openAICompatEmbedder) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body := openAIEmbeddingRequest{Model: e.model, Input: texts}
	rawBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, embeddingsURL(e.baseURL), bytes.NewReader(rawBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rawResp, err := io.ReadAll(io.LimitReader(resp.Body, openAIEmbeddingMaxRaw))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("openai-compatible embeddings status %d: %s", resp.StatusCode, strings.TrimSpace(string(rawResp)))
	}
	var decoded openAIEmbeddingResponse
	if err := json.Unmarshal(rawResp, &decoded); err != nil {
		return nil, fmt.Errorf("parse openai-compatible embeddings response: %w", err)
	}
	if len(decoded.Data) != len(texts) {
		return nil, fmt.Errorf("openai-compatible embeddings returned %d vectors for %d texts", len(decoded.Data), len(texts))
	}
	useIndex := false
	for _, item := range decoded.Data {
		if item.Index != 0 {
			useIndex = true
			break
		}
	}
	out := make([][]float32, len(texts))
	for order, item := range decoded.Data {
		pos := order
		if useIndex {
			pos = item.Index
		}
		if pos < 0 || pos >= len(texts) {
			return nil, fmt.Errorf("openai-compatible embeddings index %d out of range", pos)
		}
		vec, err := normalizeVector(item.Embedding)
		if err != nil {
			return nil, fmt.Errorf("embedding %d: %w", pos, err)
		}
		if e.dim == 0 {
			e.dim = len(vec)
		}
		if len(vec) != e.dim {
			return nil, fmt.Errorf("embedding dim %d != configured dim %d", len(vec), e.dim)
		}
		out[pos] = vec
	}
	for i, vec := range out {
		if vec == nil {
			return nil, fmt.Errorf("openai-compatible embeddings missing index %d", i)
		}
	}
	return out, nil
}

func embeddingsURL(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(base, "/embeddings") {
		return base
	}
	return base + "/embeddings"
}

func normalizeOpenAICompatBaseURL(baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return baseURL
	}
	if strings.Contains(baseURL, "://") {
		return baseURL
	}
	low := strings.ToLower(baseURL)
	if strings.HasPrefix(low, "localhost:") || strings.HasPrefix(low, "127.0.0.1:") || strings.HasPrefix(low, "[::1]:") {
		return "http://" + baseURL
	}
	return baseURL
}

func inferEmbeddingDim(model string) int {
	low := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(low, "nomic-embed-text"):
		return 768
	case strings.Contains(low, "mxbai-embed-large"):
		return 1024
	case strings.Contains(low, "bge-m3"):
		return 1024
	case strings.Contains(low, "qwen3-embedding"):
		return 4096
	case strings.Contains(low, "gemini-embedding-2"):
		return 3072
	case strings.Contains(low, "gemini-embedding"):
		return 3072
	case strings.Contains(low, "text-embedding-3-large"):
		return 3072
	case strings.Contains(low, "text-embedding-3-small"), strings.Contains(low, "text-embedding-ada-002"):
		return 1536
	default:
		return 0
	}
}

type fakeEmbedder struct {
	identity   string
	dim        int
	vectors    map[string][]float32
	failAtCall int
	err        error
	calls      int
}

func newFakeEmbedder(dim int) *fakeEmbedder {
	return &fakeEmbedder{identity: fmt.Sprintf("fake:hash:%d:%s", dim, defaultEmbedBits), dim: dim}
}

func (f *fakeEmbedder) Identity() string {
	if f.identity != "" {
		return f.identity
	}
	return fmt.Sprintf("fake:hash:%d:%s", f.dim, defaultEmbedBits)
}

func (f *fakeEmbedder) Dim() int { return f.dim }

func (f *fakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	f.calls++
	if f.failAtCall > 0 && f.calls >= f.failAtCall {
		if f.err != nil {
			return nil, f.err
		}
		return nil, errors.New("fake embedder unavailable")
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		if f.vectors != nil {
			if vec, ok := f.vectors[text]; ok {
				normalized, err := normalizeVector(vec)
				if err != nil {
					return nil, fmt.Errorf("fake vector %q: %w", text, err)
				}
				if len(normalized) != f.dim {
					return nil, fmt.Errorf("fake vector %q dim %d != %d", text, len(normalized), f.dim)
				}
				out[i] = normalized
				continue
			}
		}
		vec, err := hashUnitVector(text, f.dim)
		if err != nil {
			return nil, fmt.Errorf("fake embedder: %w", err)
		}
		out[i] = vec
	}
	return out, nil
}

func normalizeVector(vec []float32) ([]float32, error) {
	if len(vec) == 0 {
		return nil, errors.New("empty vector")
	}
	sum := 0.0
	for _, v := range vec {
		fv := float64(v)
		if math.IsNaN(fv) || math.IsInf(fv, 0) {
			return nil, fmt.Errorf("non-finite vector value %v", v)
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

// hashUnitVector derives a deterministic unit vector from arbitrary text via
// SHA-256 blocks. A real hash output degenerating to a zero or non-finite
// vector is astronomically unlikely but not provably impossible, so the
// degenerate branch is an error, never a panic, propagated to the caller
// (the enclosing fakeEmbedder.Embed already returns an error).
func hashUnitVector(text string, dim int) ([]float32, error) {
	return hashUnitVectorFromRaw(text, hashRawVector(text, dim))
}

// hashRawVector computes the raw (unnormalized) SHA-256-derived vector.
func hashRawVector(text string, dim int) []float32 {
	if dim <= 0 {
		dim = 8
	}
	vec := make([]float32, dim)
	filled := 0
	for block := uint64(0); filled < dim; block++ {
		var seed [8]byte
		binary.LittleEndian.PutUint64(seed[:], block)
		h := sha256.Sum256(append(seed[:], text...))
		for i := 0; i+4 <= len(h) && filled < dim; i += 4 {
			u := binary.LittleEndian.Uint32(h[i : i+4])
			v := (float64(u)/float64(math.MaxUint32))*2 - 1
			vec[filled] = float32(v)
			filled++
		}
	}
	return vec
}

// hashUnitVectorFromRaw normalizes a raw hash vector, naming the source text
// in the error if it degenerates (zero vector / non-finite value). Split out
// from hashUnitVector so the degenerate branch is directly testable with a
// crafted raw vector, since a real (text, dim) pair essentially never
// produces one.
func hashUnitVectorFromRaw(text string, raw []float32) ([]float32, error) {
	normalized, err := normalizeVector(raw)
	if err != nil {
		return nil, fmt.Errorf("hash embedding degenerate for %q: %w", text, err)
	}
	return normalized, nil
}
