// Package pg implements the persist interfaces backed by Postgres + pgvector.
// It is experimental: COMPOUND_STORE=postgres plus a separate explicit opt-in
// and nonsecret profile label are required before a DSN is parsed or a running
// Postgres instance with the vector extension is contacted.
//
// The schema is auto-created on first connection via EnsureSchema().
// The file-based backend remains the default; this package is never imported
// unless the operator explicitly selects the postgres backend.
package pg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"compound/internal/envelope"
	"compound/internal/persist"
)

type dbExecer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store is the Postgres-backed implementation of all persist interfaces.
type Store struct {
	pool *pgxpool.Pool
	cfg  persist.Config
}

// Open connects to Postgres and verifies the vector extension is available.
// It does NOT create tables — call EnsureSchema() for that.
func Open(ctx context.Context, cfg persist.Config) (*Store, error) {
	if err := cfg.ValidatePostgresExperiment(); err != nil {
		return nil, fmt.Errorf("persist/pg: %w", err)
	}
	profile := cfg.PostgresTargetRef()

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("persist/pg: connection configuration for %s is invalid", profile)
	}

	// Conservative pool for a local-first tool
	poolCfg.MaxConns = 4
	poolCfg.MinConns = 1
	poolCfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("persist/pg: connection pool for %s could not be created", profile)
	}

	// Verify connectivity
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("persist/pg: %s is unavailable", profile)
	}

	return &Store{pool: pool, cfg: cfg}, nil
}

// Close releases the connection pool.
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// EnsureSchema creates the vector extension and all required tables if they
// do not exist. Idempotent — safe to call on every startup.
func (s *Store) EnsureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE EXTENSION IF NOT EXISTS vector`,
		schemaDDL(s.cfg.VectorDimensions),
	}
	for _, stmt := range stmts {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("persist/pg: ensure schema: %w", err)
		}
	}
	return nil
}

func schemaDDL(vectorDims int) string {
	return fmt.Sprintf(`
-- Claims: append-only, MVCC-by-fold. Never updated, never deleted.
CREATE TABLE IF NOT EXISTS claims (
    seq             BIGSERIAL PRIMARY KEY,
    id              TEXT NOT NULL UNIQUE,
    entity          TEXT NOT NULL,
    predicate       TEXT NOT NULL,
    object          TEXT NOT NULL,
    observation_date TEXT,
    confidence      DOUBLE PRECISION DEFAULT 0,
    derivation      TEXT DEFAULT 'deterministic',
    claim_type      TEXT DEFAULT '',
    canon_class     TEXT DEFAULT '',
    qualifiers      JSONB DEFAULT '{}',
    visibility      TEXT DEFAULT 'candidate',
    state           TEXT DEFAULT 'promoted',
    source_trace_ref TEXT DEFAULT '',
    evidence_refs   JSONB DEFAULT '[]',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_claims_entity ON claims(entity);
CREATE INDEX IF NOT EXISTS idx_claims_visibility ON claims(visibility);
CREATE INDEX IF NOT EXISTS idx_claims_state ON claims(state);

-- Edges: version-chain relationships (supersedes, reinforces, etc.)
CREATE TABLE IF NOT EXISTS edges (
    id          BIGSERIAL PRIMARY KEY,
    from_id     TEXT NOT NULL,
    to_id       TEXT NOT NULL,
    kind        TEXT NOT NULL,
    claim_id    TEXT NOT NULL,
    seq         BIGINT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_edges_from ON edges(from_id);
CREATE INDEX IF NOT EXISTS idx_edges_claim ON edges(claim_id);
DO $$ BEGIN
    ALTER TABLE edges ADD CONSTRAINT edges_natural_key UNIQUE (from_id, to_id, kind, claim_id, seq);
EXCEPTION WHEN duplicate_table THEN NULL;
END $$;

-- Candidates: untrusted proposed facts awaiting adjudication.
CREATE TABLE IF NOT EXISTS candidates (
    id              TEXT PRIMARY KEY,
    entity          TEXT NOT NULL,
    predicate       TEXT NOT NULL,
    object          TEXT NOT NULL,
    observation_date TEXT,
    confidence      DOUBLE PRECISION DEFAULT 0,
    derivation      TEXT DEFAULT 'extracted',
    claim_type      TEXT DEFAULT '',
    canon_class     TEXT DEFAULT '',
    qualifiers      JSONB DEFAULT '{}',
    visibility      TEXT DEFAULT 'candidate',
    state           TEXT DEFAULT 'proposed',
    source_trace_ref TEXT DEFAULT '',
    evidence_refs   JSONB DEFAULT '[]',
    reason          TEXT DEFAULT '',
    adjudicated_by  TEXT DEFAULT '',
    adjudicated_at  TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_candidates_state ON candidates(state);
CREATE INDEX IF NOT EXISTS idx_candidates_entity ON candidates(entity);

-- Databases created before the observation_date column was added do not have it.
ALTER TABLE candidates ADD COLUMN IF NOT EXISTS observation_date TEXT;

-- Events: append-only event log (replaces JSONL segments).
CREATE TABLE IF NOT EXISTS events (
    id          TEXT PRIMARY KEY,
    kind        TEXT NOT NULL DEFAULT '',
    payload     JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_events_kind ON events(kind);

-- Work log: the work event stream (replaces work.jsonl).
CREATE TABLE IF NOT EXISTS work_log (
    seq         BIGSERIAL PRIMARY KEY,
    data        JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Work envelopes: current state of each work item.
CREATE TABLE IF NOT EXISTS work_envelopes (
    work_id     TEXT PRIMARY KEY,
    state       TEXT NOT NULL,
    owner       TEXT DEFAULT '',
    payload     JSONB NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_work_state ON work_envelopes(state);

-- Vectors: pgvector embeddings for semantic recall.
CREATE TABLE IF NOT EXISTS vectors (
    ref         TEXT PRIMARY KEY,
    embedding   vector(%d) NOT NULL,
    metadata    JSONB DEFAULT '{}',
    embedder    TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_vectors_embedding ON vectors
    USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);

-- Databases created before embedder custody was added do not have this column.
-- Empty means "written before stamping" and is ignored by the custody check.
ALTER TABLE vectors ADD COLUMN IF NOT EXISTS embedder TEXT NOT NULL DEFAULT '';
`, vectorDims)
}

// ─── LedgerStore implementation ───────────────────────────────────────────────

var _ persist.LedgerStore = (*Store)(nil)

func (s *Store) AppendClaim(claim persist.Claim, now time.Time) (int64, error) {
	return appendClaimExec(context.Background(), s.pool, claim, now)
}

func appendClaimExec(ctx context.Context, exec dbExecer, claim persist.Claim, now time.Time) (int64, error) {
	createdAt := now
	if claim.CreatedAt != "" {
		if t, err := time.Parse(time.RFC3339, claim.CreatedAt); err == nil {
			createdAt = t
		}
	}
	qualifiers, _ := json.Marshal(claim.Qualifiers)
	evidenceRefs, _ := json.Marshal(claim.EvidenceRefs)

	var seq int64
	err := exec.QueryRow(ctx, `
		INSERT INTO claims (id, entity, predicate, object, observation_date, confidence,
			derivation, claim_type, canon_class, qualifiers, visibility, state,
			source_trace_ref, evidence_refs, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT (id) DO UPDATE SET
			entity = claims.entity
		WHERE (
			claims.entity = EXCLUDED.entity AND
			claims.predicate = EXCLUDED.predicate AND
			claims.object = EXCLUDED.object AND
			claims.observation_date IS NOT DISTINCT FROM EXCLUDED.observation_date AND
			claims.confidence IS NOT DISTINCT FROM EXCLUDED.confidence AND
			claims.derivation IS NOT DISTINCT FROM EXCLUDED.derivation AND
			claims.claim_type IS NOT DISTINCT FROM EXCLUDED.claim_type AND
			claims.canon_class IS NOT DISTINCT FROM EXCLUDED.canon_class AND
			claims.qualifiers IS NOT DISTINCT FROM EXCLUDED.qualifiers AND
			claims.visibility IS NOT DISTINCT FROM EXCLUDED.visibility AND
			claims.state IS NOT DISTINCT FROM EXCLUDED.state AND
			claims.source_trace_ref IS NOT DISTINCT FROM EXCLUDED.source_trace_ref AND
			claims.evidence_refs IS NOT DISTINCT FROM EXCLUDED.evidence_refs
		)
		RETURNING seq`,
		claim.ID, claim.Entity, claim.Predicate, claim.Object, claim.ObservationDate,
		claim.Confidence, claim.Derivation, claim.ClaimType, claim.CanonClass,
		qualifiers, claim.Visibility, claim.State, claim.SourceTraceRef,
		evidenceRefs, createdAt,
	).Scan(&seq)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("persist/pg: append claim: id %s exists with different content", claim.ID)
		}
		return 0, fmt.Errorf("persist/pg: append claim: %w", err)
	}
	return seq, nil
}

func (s *Store) BulkAppendClaims(ctx context.Context, claims []persist.Claim) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("persist/pg: begin claims bulk tx: %w", err)
	}
	defer tx.Rollback(ctx)
	written := 0
	for _, claim := range claims {
		if _, err := appendClaimExec(ctx, tx, claim, time.Now()); err != nil {
			return 0, fmt.Errorf("persist/pg: bulk claims at %s: %w", claim.ID, err)
		}
		written++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("persist/pg: commit claims bulk tx: %w", err)
	}
	return written, nil
}

func (s *Store) AppendEdge(edge persist.Edge, now time.Time) error {
	return appendEdgeExec(context.Background(), s.pool, edge, now)
}

func appendEdgeExec(ctx context.Context, exec dbExecer, edge persist.Edge, now time.Time) error {
	createdAt := now
	if edge.CreatedAt != "" {
		if t, err := time.Parse(time.RFC3339, edge.CreatedAt); err == nil {
			createdAt = t
		}
	}
	_, err := exec.Exec(ctx, `
		INSERT INTO edges (from_id, to_id, kind, claim_id, seq, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT ON CONSTRAINT edges_natural_key DO NOTHING`,
		edge.From, edge.To, edge.Kind, edge.ClaimID, edge.Seq, createdAt,
	)
	if err != nil {
		return fmt.Errorf("persist/pg: append edge: %w", err)
	}
	return nil
}

func (s *Store) BulkAppendEdges(ctx context.Context, edges []persist.Edge) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("persist/pg: begin edges bulk tx: %w", err)
	}
	defer tx.Rollback(ctx)
	written := 0
	for _, edge := range edges {
		if err := appendEdgeExec(ctx, tx, edge, time.Now()); err != nil {
			return 0, fmt.Errorf("persist/pg: bulk edges at %s->%s: %w", edge.From, edge.To, err)
		}
		written++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("persist/pg: commit edges bulk tx: %w", err)
	}
	return written, nil
}

func (s *Store) FoldClaims(filter persist.ClaimFilter) ([]persist.Claim, error) {
	ctx := context.Background()
	query := `SELECT seq, id, entity, predicate, object, observation_date, confidence,
		derivation, claim_type, canon_class, qualifiers, visibility, state,
		source_trace_ref, evidence_refs, created_at FROM claims WHERE 1=1`
	args := []any{}
	argN := 1

	if filter.Entity != "" {
		query += fmt.Sprintf(" AND entity = $%d", argN)
		args = append(args, filter.Entity)
		argN++
	}
	if filter.State != "" {
		query += fmt.Sprintf(" AND state = $%d", argN)
		args = append(args, filter.State)
		argN++
	}
	if len(filter.Visibility) > 0 {
		query += fmt.Sprintf(" AND visibility = ANY($%d)", argN)
		args = append(args, filter.Visibility)
		argN++
	}
	if filter.SinceSeq > 0 {
		query += fmt.Sprintf(" AND seq > $%d", argN)
		args = append(args, filter.SinceSeq)
		argN++
	}

	query += " ORDER BY seq ASC"
	if filter.Limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d", argN)
		args = append(args, filter.Limit)
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("persist/pg: fold claims: %w", err)
	}
	defer rows.Close()

	var claims []persist.Claim
	for rows.Next() {
		var c persist.Claim
		var qualifiers, evidenceRefs []byte
		var createdAt time.Time
		if err := rows.Scan(&c.Seq, &c.ID, &c.Entity, &c.Predicate, &c.Object,
			&c.ObservationDate, &c.Confidence, &c.Derivation, &c.ClaimType,
			&c.CanonClass, &qualifiers, &c.Visibility, &c.State,
			&c.SourceTraceRef, &evidenceRefs, &createdAt); err != nil {
			return nil, fmt.Errorf("persist/pg: scan claim: %w", err)
		}
		json.Unmarshal(qualifiers, &c.Qualifiers)
		json.Unmarshal(evidenceRefs, &c.EvidenceRefs)
		c.CreatedAt = createdAt.Format(time.RFC3339)
		claims = append(claims, c)
	}
	return claims, rows.Err()
}

func (s *Store) CurrentPosition(entity, topic string) (*persist.Position, error) {
	claims, err := s.FoldClaims(persist.ClaimFilter{
		Entity:     entity,
		Visibility: []string{"ambient", "contextual", "operational"},
		State:      "promoted",
	})
	if err != nil {
		return nil, err
	}
	// Fold: last claim per predicate wins (MVCC-by-fold)
	byPredicate := map[string]persist.Claim{}
	for _, c := range claims {
		byPredicate[c.Predicate] = c // last wins (ascending seq order)
	}
	if topic != "" {
		if c, ok := byPredicate[topic]; ok {
			return &persist.Position{
				Entity:    entity,
				Topic:     topic,
				Claim:     c,
				Seq:       c.Seq,
				UpdatedAt: c.CreatedAt,
			}, nil
		}
		return nil, nil
	}
	// Return the highest-seq position
	var best *persist.Position
	for pred, c := range byPredicate {
		if best == nil || c.Seq > best.Seq {
			best = &persist.Position{
				Entity:    entity,
				Topic:     pred,
				Claim:     c,
				Seq:       c.Seq,
				UpdatedAt: c.CreatedAt,
			}
		}
	}
	return best, nil
}

func (s *Store) NextSeq() (int64, error) {
	ctx := context.Background()
	var seq int64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM claims`).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("persist/pg: next seq: %w", err)
	}
	return seq, nil
}

func (s *Store) AppendCandidate(cand persist.Candidate, now time.Time) error {
	return appendCandidateExec(context.Background(), s.pool, cand, now)
}

func appendCandidateExec(ctx context.Context, exec dbExecer, cand persist.Candidate, now time.Time) error {
	qualifiers, _ := json.Marshal(cand.Qualifiers)
	evidenceRefs, _ := json.Marshal(cand.EvidenceRefs)
	var adjudicatedAt *time.Time
	if cand.AdjudicatedAt != "" {
		if t, err := time.Parse(time.RFC3339, cand.AdjudicatedAt); err == nil {
			adjudicatedAt = &t
		}
	}
	var id string
	err := exec.QueryRow(ctx, `
		INSERT INTO candidates (id, entity, predicate, object, observation_date, confidence,
			derivation, claim_type, canon_class, qualifiers, visibility, state,
			source_trace_ref, evidence_refs, reason, adjudicated_by, adjudicated_at, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		ON CONFLICT (id) DO UPDATE SET
			entity = candidates.entity
		WHERE (
			candidates.entity = EXCLUDED.entity AND
			candidates.predicate = EXCLUDED.predicate AND
			candidates.object = EXCLUDED.object AND
			candidates.observation_date IS NOT DISTINCT FROM EXCLUDED.observation_date AND
			candidates.confidence IS NOT DISTINCT FROM EXCLUDED.confidence AND
			candidates.derivation IS NOT DISTINCT FROM EXCLUDED.derivation AND
			candidates.claim_type IS NOT DISTINCT FROM EXCLUDED.claim_type AND
			candidates.canon_class IS NOT DISTINCT FROM EXCLUDED.canon_class AND
			candidates.qualifiers IS NOT DISTINCT FROM EXCLUDED.qualifiers AND
			candidates.visibility IS NOT DISTINCT FROM EXCLUDED.visibility AND
			candidates.state IS NOT DISTINCT FROM EXCLUDED.state AND
			candidates.source_trace_ref IS NOT DISTINCT FROM EXCLUDED.source_trace_ref AND
			candidates.evidence_refs IS NOT DISTINCT FROM EXCLUDED.evidence_refs AND
			candidates.reason IS NOT DISTINCT FROM EXCLUDED.reason AND
			candidates.adjudicated_by IS NOT DISTINCT FROM EXCLUDED.adjudicated_by AND
			candidates.adjudicated_at IS NOT DISTINCT FROM EXCLUDED.adjudicated_at
		)
		RETURNING id`,
		cand.ID, cand.Entity, cand.Predicate, cand.Object, cand.ObservationDate,
		cand.Confidence, cand.Derivation, cand.ClaimType, cand.CanonClass,
		qualifiers, cand.Visibility, cand.State, cand.SourceTraceRef,
		evidenceRefs, cand.Reason, cand.AdjudicatedBy, adjudicatedAt, now,
	).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("persist/pg: append candidate: id %s exists with different content", cand.ID)
		}
		return fmt.Errorf("persist/pg: append candidate: %w", err)
	}
	return nil
}

func (s *Store) BulkAppendCandidates(ctx context.Context, cands []persist.Candidate) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("persist/pg: begin candidates bulk tx: %w", err)
	}
	defer tx.Rollback(ctx)
	written := 0
	for _, cand := range cands {
		if err := appendCandidateExec(ctx, tx, cand, time.Now()); err != nil {
			return 0, fmt.Errorf("persist/pg: bulk candidates at %s: %w", cand.ID, err)
		}
		written++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("persist/pg: commit candidates bulk tx: %w", err)
	}
	return written, nil
}

func (s *Store) UpdateCandidate(id string, mutate func(*persist.Candidate) error) error {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("persist/pg: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	cand, err := scanCandidateFromRow(tx.QueryRow(ctx, `
		SELECT id, entity, predicate, object, observation_date, confidence, derivation,
			claim_type, canon_class, qualifiers, visibility, state, source_trace_ref,
			evidence_refs, reason, adjudicated_by, adjudicated_at, created_at
		FROM candidates WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return fmt.Errorf("persist/pg: get candidate for update: %w", err)
	}

	if err := mutate(&cand); err != nil {
		return err
	}

	qualifiers, _ := json.Marshal(cand.Qualifiers)
	evidenceRefs, _ := json.Marshal(cand.EvidenceRefs)
	var adjudicatedAt *time.Time
	if cand.AdjudicatedAt != "" {
		if t, err := time.Parse(time.RFC3339, cand.AdjudicatedAt); err == nil {
			adjudicatedAt = &t
		}
	}

	_, err = tx.Exec(ctx, `
		UPDATE candidates SET
			entity=$1, predicate=$2, object=$3, observation_date=$4, confidence=$5,
			derivation=$6, claim_type=$7, canon_class=$8, qualifiers=$9, visibility=$10,
			state=$11, source_trace_ref=$12, evidence_refs=$13, reason=$14,
			adjudicated_by=$15, adjudicated_at=$16
		WHERE id=$17`,
		cand.Entity, cand.Predicate, cand.Object, cand.ObservationDate, cand.Confidence,
		cand.Derivation, cand.ClaimType, cand.CanonClass, qualifiers, cand.Visibility,
		cand.State, cand.SourceTraceRef, evidenceRefs, cand.Reason,
		cand.AdjudicatedBy, adjudicatedAt, cand.ID,
	)
	if err != nil {
		return fmt.Errorf("persist/pg: update candidate: %w", err)
	}
	return tx.Commit(ctx)
}

// scanCandidateFromRow populates a persist.Candidate from a pgx row scanner.
func scanCandidateFromRow(row pgx.Row) (persist.Candidate, error) {
	var c persist.Candidate
	var createdAt time.Time
	var qualifiers, evidenceRefs []byte
	var adjudicatedAt *time.Time
	err := row.Scan(&c.ID, &c.Entity, &c.Predicate, &c.Object, &c.ObservationDate,
		&c.Confidence, &c.Derivation, &c.ClaimType, &c.CanonClass, &qualifiers,
		&c.Visibility, &c.State, &c.SourceTraceRef, &evidenceRefs, &c.Reason,
		&c.AdjudicatedBy, &adjudicatedAt, &createdAt)
	if err != nil {
		return persist.Candidate{}, err
	}
	if len(qualifiers) > 0 {
		_ = json.Unmarshal(qualifiers, &c.Qualifiers)
	}
	if len(evidenceRefs) > 0 {
		_ = json.Unmarshal(evidenceRefs, &c.EvidenceRefs)
	}
	if adjudicatedAt != nil {
		c.AdjudicatedAt = adjudicatedAt.UTC().Format(time.RFC3339)
	}
	c.CreatedAt = createdAt.Format(time.RFC3339)
	return c, nil
}

func (s *Store) ListCandidates(filter persist.CandidateFilter) ([]persist.Candidate, error) {
	ctx := context.Background()
	query := `SELECT id, entity, predicate, object, observation_date, confidence, derivation,
		claim_type, canon_class, qualifiers, visibility, state, source_trace_ref,
		evidence_refs, reason, adjudicated_by, adjudicated_at, created_at FROM candidates WHERE 1=1`
	args := []any{}
	argN := 1

	if filter.State != "" {
		query += fmt.Sprintf(" AND state = $%d", argN)
		args = append(args, filter.State)
		argN++
	}
	if filter.Entity != "" {
		query += fmt.Sprintf(" AND entity = $%d", argN)
		args = append(args, filter.Entity)
		argN++
	}
	query += " ORDER BY created_at DESC"
	if filter.Limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d", argN)
		args = append(args, filter.Limit)
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("persist/pg: list candidates: %w", err)
	}
	defer rows.Close()

	var cands []persist.Candidate
	for rows.Next() {
		c, err := scanCandidateFromRow(rows)
		if err != nil {
			return nil, fmt.Errorf("persist/pg: scan candidate: %w", err)
		}
		cands = append(cands, c)
	}
	return cands, rows.Err()
}

func (s *Store) GetCandidate(id string) (*persist.Candidate, error) {
	ctx := context.Background()
	c, err := scanCandidateFromRow(s.pool.QueryRow(ctx, `
		SELECT id, entity, predicate, object, observation_date, confidence, derivation,
			claim_type, canon_class, qualifiers, visibility, state, source_trace_ref,
			evidence_refs, reason, adjudicated_by, adjudicated_at, created_at
		FROM candidates WHERE id = $1`, id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("persist/pg: get candidate: %w", err)
	}
	return &c, nil
}

// ─── EventStore implementation ────────────────────────────────────────────────

var _ persist.EventStore = (*Store)(nil)

func (s *Store) PutJSON(id string, value any) error {
	ctx := context.Background()
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("persist/pg: marshal event: %w", err)
	}
	// Idempotent on same bytes; conflict on same-ID-different-bytes.
	// An event store must never silently overwrite history.
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO events (id, payload, created_at) VALUES ($1, $2, now())
		ON CONFLICT (id) DO NOTHING`, id, payload)
	if err != nil {
		return fmt.Errorf("persist/pg: put event: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return nil // inserted
	}
	var existing []byte
	if err := s.pool.QueryRow(ctx, `SELECT payload FROM events WHERE id = $1`, id).Scan(&existing); err != nil {
		return fmt.Errorf("persist/pg: conflict check: %w", err)
	}
	if !bytes.Equal(existing, payload) {
		return fmt.Errorf("persist/pg: event conflict: id %s exists with different payload", id)
	}
	return nil // idempotent: same id, same bytes
}

func (s *Store) PutJSONIfAbsent(id string, value any) (bool, error) {
	ctx := context.Background()
	payload, err := json.Marshal(value)
	if err != nil {
		return false, fmt.Errorf("persist/pg: marshal event: %w", err)
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO events (id, payload, created_at) VALUES ($1, $2, now())
		ON CONFLICT (id) DO NOTHING`, id, payload)
	if err != nil {
		return false, fmt.Errorf("persist/pg: put if absent: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return true, nil // created
	}
	// Already exists — detect same-ID-different-bytes corruption.
	var existing []byte
	if err := s.pool.QueryRow(ctx, `SELECT payload FROM events WHERE id = $1`, id).Scan(&existing); err != nil {
		return false, fmt.Errorf("persist/pg: conflict check: %w", err)
	}
	if !bytes.Equal(existing, payload) {
		return false, fmt.Errorf("persist/pg: event conflict: id %s exists with different payload", id)
	}
	return false, nil // already present, same bytes
}

func (s *Store) Read(ref string) (*persist.EventRecord, error) {
	ctx := context.Background()
	var rec persist.EventRecord
	var payload []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, kind, payload, created_at FROM events WHERE id = $1`, ref,
	).Scan(&rec.ID, &rec.Kind, &payload, &rec.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("persist/pg: read event: %w", err)
	}
	rec.Bytes = payload
	return &rec, nil
}

func (s *Store) Scan(opts persist.EventScanOptions) ([]persist.EventRecord, error) {
	ctx := context.Background()
	query := `SELECT id, kind, payload, created_at FROM events WHERE 1=1`
	args := []any{}
	argN := 1

	if len(opts.Prefixes) > 0 {
		conditions := ""
		for i, p := range opts.Prefixes {
			if i > 0 {
				conditions += " OR"
			}
			conditions += fmt.Sprintf(" id LIKE $%d", argN)
			args = append(args, p+"%")
			argN++
		}
		query += " AND (" + conditions + ")"
	}
	query += " ORDER BY created_at ASC"
	if opts.Limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d", argN)
		args = append(args, opts.Limit)
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("persist/pg: scan events: %w", err)
	}
	defer rows.Close()

	var recs []persist.EventRecord
	for rows.Next() {
		var rec persist.EventRecord
		var payload []byte
		if err := rows.Scan(&rec.ID, &rec.Kind, &payload, &rec.CreatedAt); err != nil {
			return nil, fmt.Errorf("persist/pg: scan event: %w", err)
		}
		rec.Bytes = payload
		recs = append(recs, rec)
	}
	return recs, rows.Err()
}

func (s *Store) AppendWorkLog(data []byte) error {
	ctx := context.Background()
	_, err := s.pool.Exec(ctx, `INSERT INTO work_log (data) VALUES ($1)`, data)
	if err != nil {
		return fmt.Errorf("persist/pg: append work log: %w", err)
	}
	return nil
}

func (s *Store) ReadWorkLog() ([]byte, error) {
	ctx := context.Background()
	rows, err := s.pool.Query(ctx, `SELECT data FROM work_log ORDER BY seq ASC`)
	if err != nil {
		return nil, fmt.Errorf("persist/pg: read work log: %w", err)
	}
	defer rows.Close()

	var buf []byte
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		buf = append(buf, data...)
		buf = append(buf, '\n')
	}
	return buf, rows.Err()
}

// ─── WorkStore implementation ─────────────────────────────────────────────────

var _ persist.WorkStore = (*Store)(nil)

func (s *Store) PutWork(env envelope.WorkEnvelope) error {
	ctx := context.Background()
	payload, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("persist/pg: marshal work: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO work_envelopes (work_id, state, owner, payload, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (work_id) DO UPDATE SET state=$2, owner=$3, payload=$4, updated_at=now()`,
		env.IDs.WorkID, env.State, env.Owner, payload)
	if err != nil {
		return fmt.Errorf("persist/pg: put work: %w", err)
	}
	return nil
}

func (s *Store) GetWork(workID string) (*envelope.WorkEnvelope, error) {
	ctx := context.Background()
	var payload []byte
	err := s.pool.QueryRow(ctx, `
		SELECT payload FROM work_envelopes WHERE work_id = $1`, workID,
	).Scan(&payload)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("persist/pg: get work: %w", err)
	}
	var env envelope.WorkEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("persist/pg: unmarshal work: %w", err)
	}
	return &env, nil
}

func (s *Store) ListWork() ([]envelope.WorkEnvelope, error) {
	return s.listWork(`WHERE state NOT IN ('completed','failed','cancelled')`)
}

func (s *Store) ListAllWork() ([]envelope.WorkEnvelope, error) {
	return s.listWork("")
}

func (s *Store) listWork(where string) ([]envelope.WorkEnvelope, error) {
	ctx := context.Background()
	rows, err := s.pool.Query(ctx,
		`SELECT payload FROM work_envelopes `+where+` ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("persist/pg: list work: %w", err)
	}
	defer rows.Close()

	var envs []envelope.WorkEnvelope
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var env envelope.WorkEnvelope
		if err := json.Unmarshal(payload, &env); err != nil {
			return nil, err
		}
		envs = append(envs, env)
	}
	return envs, rows.Err()
}

// ─── VectorStore implementation (pgvector) ────────────────────────────────────

var _ persist.VectorStore = (*Store)(nil)

func (s *Store) Upsert(ref string, embedding []float32, metadata map[string]string) error {
	ctx := context.Background()
	meta, _ := json.Marshal(metadata)
	vec := pgvector.NewVector(embedding)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO vectors (ref, embedding, metadata, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (ref) DO UPDATE SET embedding=$2, metadata=$3, updated_at=now()`,
		ref, vec, meta)
	if err != nil {
		return fmt.Errorf("persist/pg: upsert vector: %w", err)
	}
	return nil
}

func (s *Store) Search(queryVec []float32, k int, filter map[string]string) ([]persist.VectorHit, error) {
	ctx := context.Background()
	vec := pgvector.NewVector(queryVec)

	query := `SELECT ref, 1 - (embedding <=> $1) AS score, metadata
		FROM vectors ORDER BY embedding <=> $1 LIMIT $2`
	args := []any{vec, k}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("persist/pg: vector search: %w", err)
	}
	defer rows.Close()

	var hits []persist.VectorHit
	for rows.Next() {
		var h persist.VectorHit
		var meta []byte
		if err := rows.Scan(&h.Ref, &h.Score, &meta); err != nil {
			return nil, fmt.Errorf("persist/pg: scan vector hit: %w", err)
		}
		json.Unmarshal(meta, &h.Metadata)
		// Apply metadata filter client-side (simple equality)
		if len(filter) > 0 {
			match := true
			for k, v := range filter {
				if h.Metadata[k] != v {
					match = false
					break
				}
			}
			if !match {
				continue
			}
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

func (s *Store) Delete(ref string) error {
	ctx := context.Background()
	_, err := s.pool.Exec(ctx, `DELETE FROM vectors WHERE ref = $1`, ref)
	if err != nil {
		return fmt.Errorf("persist/pg: delete vector: %w", err)
	}
	return nil
}

func (s *Store) Count() (int64, error) {
	ctx := context.Background()
	var count int64
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM vectors`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("persist/pg: count vectors: %w", err)
	}
	return count, nil
}

// VectorRefs returns the set of all refs that already have vectors stored.
// Used by incremental embed to skip claims that are already embedded.
func (s *Store) VectorRefs() (map[string]bool, error) {
	ctx := context.Background()
	rows, err := s.pool.Query(ctx, `SELECT ref FROM vectors`)
	if err != nil {
		return nil, fmt.Errorf("persist/pg: list vector refs: %w", err)
	}
	defer rows.Close()
	refs := make(map[string]bool)
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, fmt.Errorf("persist/pg: scan vector ref: %w", err)
		}
		refs[ref] = true
	}
	return refs, rows.Err()
}

// ─── Diagnostics ─────────────────────────────────────────────────────────────────

// TableCounts returns row counts for all store tables. Used by `compound store status`.
func (s *Store) TableCounts(ctx context.Context) map[string]int64 {
	tables := []string{"claims", "edges", "candidates", "events", "work_log", "work_envelopes", "vectors"}
	counts := make(map[string]int64, len(tables))
	for _, t := range tables {
		var n int64
		err := s.pool.QueryRow(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", t)).Scan(&n)
		if err == nil {
			counts[t] = n
		}
	}
	return counts
}

// PoolStats exposes connection pool diagnostics for operator surfaces.
func (s *Store) PoolStats() pgxpoolStats {
	stat := s.pool.Stat()
	return pgxpoolStats{
		AcquireCount:      stat.AcquireCount(),
		TotalConns:        stat.TotalConns(),
		IdleConns:         stat.IdleConns(),
		MaxConns:          stat.MaxConns(),
		ConstructingConns: stat.ConstructingConns(),
	}
}

type pgxpoolStats struct {
	AcquireCount      int64 `json:"acquire_count"`
	TotalConns        int32 `json:"total_conns"`
	IdleConns         int32 `json:"idle_conns"`
	MaxConns          int32 `json:"max_conns"`
	ConstructingConns int32 `json:"constructing_conns"`
}
