package pg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"

	"compound/internal/mem"
)

// claimRefPrefix is the legacy vector ref namespace ("claim:<id>") written by
// older embed runs. The canonical namespace is the BARE claim id, matching the
// ledger-computed allowlist (mem's relevance gate carries bare claim ids) — the
// engine normalizes legacy rows at its boundary until they are re-embedded.
const (
	claimRefPrefix = "claim:"

	// vectorEmbedderLockKey1/2 form a hardcoded 64-bit namespaced advisory lock key
	// for pg_advisory_xact_lock. We deliberately avoid the old hash-based key (which
	// had a small collision space and could alias with unrelated keys). 8392753 is
	// an arbitrary namespace picked to avoid Postgres key conventions; key2=1 is the
	// lock instance for embedder-stamped vector writes.
	vectorEmbedderLockKey1 = 8392753
	vectorEmbedderLockKey2 = 1
)

// PgEngine implements mem.Engine backed by Postgres + pgvector. It embeds query
// text via an OpenAI-compatible /v1/embeddings endpoint (e.g. Ollama) and
// searches the vectors table using the HNSW index. Vectors are populated
// out-of-band by "compound store embed"; Index() is a no-op and CorpusGen is
// derived from the ledger copy's real high-water mark (the corpus lives in
// Postgres, not in the engine process).
type PgEngine struct {
	store      *Store
	embedURL   string // OpenAI-compatible base (scheme://host:port/v1)
	embedModel string // e.g. "nomic-embed-text"
	dims       int
}

// NewPgEngine returns a mem.Engine that queries pgvector for semantic recall.
// embedURL is the OpenAI-compatible base (scheme://host:port/v1); the engine
// appends "/embeddings". dims must match the stored vector dimension.
func NewPgEngine(store *Store, embedURL, embedModel string, dims int) *PgEngine {
	return &PgEngine{
		store:      store,
		embedURL:   strings.TrimRight(embedURL, "/"),
		embedModel: embedModel,
		dims:       dims,
	}
}

func (e *PgEngine) Capabilities() mem.EngineCapabilities {
	return mem.EngineCapabilities{
		Name:      "pgvector",
		Breadth:   true,
		Embedder:  e.embedderIdentity(),
		CorpusGen: e.corpusGen(),
	}
}

func (e *PgEngine) embedderIdentity() string {
	return fmt.Sprintf("ollama:%s:%d", e.embedModel, e.dims)
}

// corpusGen derives the engine generation from the ledger copy's real high-water
// mark (max seq across claims AND edges — one shared seq space, mirroring
// mem.maxLedgerSeq). It is never adopted from a caller, so a stale caller cannot
// fake freshness. An unreadable mark reports 0 (lags everything → re-Index),
// mirroring mem.currentGen.
func (e *PgEngine) corpusGen() int64 {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gen, err := e.store.LedgerHighWater(ctx)
	if err != nil {
		return 0
	}
	return gen
}

// Index is a no-op: vectors are populated out-of-band by "compound store embed",
// and the handed gen is deliberately IGNORED — CorpusGen is derived from the
// ledger high-water mark (see corpusGen), never adopted, so an Index call can
// never fake freshness.
func (e *PgEngine) Index(_ []mem.Claim, _ int64) error { return nil }

// Query embeds the text via the configured endpoint, then searches pgvector
// with an allowlist gate (GateList → WHERE ref = ANY; GateAll → unfiltered).
// An empty allowlist under GateList matches nothing (fail-closed, matching
// LexicalEngine semantics). A vectors table indexed with a DIFFERENT embedder
// is a hard refusal (mem.ErrEmbedderMismatch), never a silent recall degrade.
func (e *PgEngine) Query(text, gate string, allowlist []string, k int) ([]mem.Hit, error) {
	if gate != mem.GateAll && len(allowlist) == 0 {
		return nil, nil // fail-closed: empty allowlist matches nothing
	}
	if err := e.checkEmbedder(); err != nil {
		return nil, err
	}
	vec, err := e.embed(text)
	if err != nil {
		return nil, fmt.Errorf("pgvector engine: embed: %w", err)
	}
	hits, err := e.searchAllowlisted(vec, gate, allowlist, k)
	if err != nil {
		return nil, fmt.Errorf("pgvector engine: search: %w", err)
	}
	return hits, nil
}

// checkEmbedder refuses breadth when the vectors table was indexed with a
// DIFFERENT embedder than this engine's — a silent model swap mixes two
// embedding spaces in one index and degrades recall invisibly, so this fails
// closed, never degrades. Rows stamped ” (written before stamping existed)
// do not trip the guard.
func (e *PgEngine) checkEmbedder() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	embedders, err := e.store.VectorEmbedders(ctx)
	if err != nil {
		return fmt.Errorf("pgvector engine: embedder check: %w", err)
	}
	for _, emb := range embedders {
		if emb != e.embedderIdentity() {
			return fmt.Errorf("pgvector engine: indexed with %q, querying with %q: %w",
				emb, e.embedderIdentity(), mem.ErrEmbedderMismatch)
		}
	}
	return nil
}

// --- embedding ---

type pgEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type pgEmbedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func (e *PgEngine) embed(text string) ([]float32, error) {
	reqBody, _ := json.Marshal(pgEmbedRequest{Model: e.embedModel, Input: []string{text}})
	url := e.embedURL + "/embeddings"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("embed API %d: %s", resp.StatusCode, string(body))
	}
	var result pgEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("embed decode: %w", err)
	}
	if len(result.Data) == 0 || len(result.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embed returned empty vector")
	}
	return result.Data[0].Embedding, nil
}

// --- search ---

// searchAllowlisted runs the ANN search under the gate. The allowlist carries
// BARE claim ids (the ledger's gate); stored refs are matched in both the
// canonical bare namespace and the legacy "claim:"-prefixed one, and every hit
// is normalized back to a bare claim id, so ClaimID always resolves against the
// owned ledger.
func (e *PgEngine) searchAllowlisted(queryVec []float32, gate string, allowlist []string, k int) ([]mem.Hit, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	vec := pgvector.NewVector(queryVec)

	var (
		rows interface {
			Next() bool
			Scan(...any) error
			Close()
			Err() error
		}
		err error
	)

	if gate == mem.GateAll {
		rows, err = e.store.pool.Query(ctx,
			`SELECT ref, 1 - (embedding <=> $1) AS score FROM vectors ORDER BY embedding <=> $1 LIMIT $2`,
			vec, k,
		)
	} else {
		refs := make([]string, 0, len(allowlist)*2)
		for _, id := range allowlist {
			refs = append(refs, id, claimRefPrefix+id)
		}
		rows, err = e.store.pool.Query(ctx,
			`SELECT ref, 1 - (embedding <=> $1) AS score FROM vectors WHERE ref = ANY($2) ORDER BY embedding <=> $1 LIMIT $3`,
			vec, refs, k,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("persist/pg: allowlisted search: %w", err)
	}
	defer rows.Close()

	var hits []mem.Hit
	for rows.Next() {
		var h mem.Hit
		var ref string
		if err := rows.Scan(&ref, &h.Score); err != nil {
			return nil, fmt.Errorf("persist/pg: scan hit: %w", err)
		}
		h.ClaimID = strings.TrimPrefix(ref, claimRefPrefix)
		h.Kind = mem.KindIndexHit
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// --- store-side freshness + embedder custody ---

// LedgerHighWater is the highest seq across claims AND edges — the real
// freshness mark of the pg ledger copy (mem.maxLedgerSeq parity: both families
// draw from one seq space).
func (s *Store) LedgerHighWater(ctx context.Context) (int64, error) {
	var hi int64
	err := s.pool.QueryRow(ctx, `SELECT GREATEST(
		(SELECT COALESCE(MAX(seq), 0) FROM claims),
		(SELECT COALESCE(MAX(seq), 0) FROM edges))`).Scan(&hi)
	if err != nil {
		return 0, fmt.Errorf("persist/pg: ledger high-water: %w", err)
	}
	return hi, nil
}

// VectorEmbedders returns the distinct non-empty embedder identities stamped on
// stored vectors (” = written before embedder stamping existed; excluded).
func (s *Store) VectorEmbedders(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT embedder FROM vectors WHERE embedder <> ''`)
	if err != nil {
		return nil, fmt.Errorf("persist/pg: list vector embedders: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var emb string
		if err := rows.Scan(&emb); err != nil {
			return nil, fmt.Errorf("persist/pg: scan vector embedder: %w", err)
		}
		out = append(out, emb)
	}
	return out, rows.Err()
}

// UpsertWithEmbedder is Upsert plus embedder-identity custody: it stamps the
// vectors row with the producing embedder and REFUSES (mem.ErrEmbedderMismatch)
// when existing rows carry a different identity — a silent model swap must never
// mix two embedding spaces in one index.
//
// The embedder check+write is serialized with a transaction-scoped advisory lock
// so that concurrent writers cannot both see an empty table and insert different
// embedders under READ COMMITTED.
func (s *Store) UpsertWithEmbedder(ref string, embedding []float32, metadata map[string]string, embedder string) error {
	ctx := context.Background()
	meta, _ := json.Marshal(metadata)
	vec := pgvector.NewVector(embedding)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("persist/pg: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Serialize all embedder-stamped writes to prevent a race where two
	// transactions see no conflicting rows and commit different embedders.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, vectorEmbedderLockKey1, vectorEmbedderLockKey2); err != nil {
		return fmt.Errorf("persist/pg: embedder lock: %w", err)
	}

	var other string
	err = tx.QueryRow(ctx, `
		SELECT embedder FROM vectors WHERE embedder <> '' AND embedder <> $1 LIMIT 1`,
		embedder).Scan(&other)
	if err == nil {
		return fmt.Errorf("persist/pg: vectors indexed with %q, writing with %q: %w",
			other, embedder, mem.ErrEmbedderMismatch)
	}
	if err != pgx.ErrNoRows {
		return fmt.Errorf("persist/pg: embedder check: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO vectors (ref, embedding, metadata, embedder, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (ref) DO UPDATE SET embedding=$2, metadata=$3, embedder=$4, updated_at=now()`,
		ref, vec, meta, embedder)
	if err != nil {
		return fmt.Errorf("persist/pg: upsert vector: %w", err)
	}
	return tx.Commit(ctx)
}

// Close releases the underlying connection pool. Callers that create a PgEngine
// for a single query batch should defer Close.
func (e *PgEngine) Close() {
	if e.store != nil && e.store.pool != nil {
		e.store.pool.Close()
	}
}
