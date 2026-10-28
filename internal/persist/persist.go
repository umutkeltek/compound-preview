// Package persist is the storage abstraction layer — the logical seam between
// Compound's domain objects and their physical persistence. It defines interfaces
// that the existing file-based stores satisfy implicitly, and that the
// Postgres+pgvector backend (internal/persist/pg) implements explicitly.
//
// Design constraints (ADR-0005):
//   - The file backend remains the supported DEFAULT. Postgres requires both
//     COMPOUND_STORE=postgres and a separate experimental opt-in.
//   - Domain import paths and persistence interfaces stay additive. Configuration
//     admission returns an explicit error so unknown or partial selection cannot
//     silently fall back to the supported file backend.
//   - The interfaces reference existing domain types (envelope, mem) to avoid mapping overhead.
//   - No import cycle: persist imports envelope/mem; they never import persist.
//
// Classification: leaf (shared infrastructure seam; owns no authority object).
package persist

import (
	"os"
	"strconv"
	"strings"
	"time"

	"compound/internal/envelope"
)

// ─── Ledger ───────────────────────────────────────────────────────────────────

// Claim is the durable, immutable assertion in the ledger (mirrors mem.Claim shape).
// The pg backend stores these in a claims table with MVCC-by-fold semantics.
type Claim struct {
	Seq             int64             `json:"seq"`
	ID              string            `json:"id"`
	Entity          string            `json:"entity"`
	Predicate       string            `json:"predicate"`
	Object          string            `json:"object"`
	ObservationDate string            `json:"observation_date"`
	Confidence      float64           `json:"confidence"`
	Derivation      string            `json:"derivation"`
	ClaimType       string            `json:"claim_type,omitempty"`
	CanonClass      string            `json:"canon_class,omitempty"`
	Qualifiers      map[string]string `json:"qualifiers,omitempty"`
	Visibility      string            `json:"visibility"`
	State           string            `json:"state"`
	SourceTraceRef  string            `json:"source_trace_ref"`
	EvidenceRefs    []string          `json:"evidence_refs,omitempty"`
	CreatedAt       string            `json:"created_at"`
}

// Edge is a version-chain relationship between claims (supersedes, reinforces, etc.).
type Edge struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Kind      string `json:"kind"`
	Seq       int64  `json:"seq"`
	ClaimID   string `json:"claim_id"`
	CreatedAt string `json:"created_at,omitempty"`
}

// Candidate is an untrusted proposed fact awaiting adjudication (mirrors mem.Candidate).
type Candidate struct {
	ID              string            `json:"id"`
	Entity          string            `json:"entity"`
	Predicate       string            `json:"predicate"`
	Object          string            `json:"object"`
	ObservationDate string            `json:"observation_date,omitempty"`
	Confidence      float64           `json:"confidence"`
	Derivation      string            `json:"derivation"`
	ClaimType       string            `json:"claim_type,omitempty"`
	CanonClass      string            `json:"canon_class,omitempty"`
	Qualifiers      map[string]string `json:"qualifiers,omitempty"`
	Visibility      string            `json:"visibility"`
	State           string            `json:"state"`
	SourceTraceRef  string            `json:"source_trace_ref"`
	EvidenceRefs    []string          `json:"evidence_refs,omitempty"`
	Reason          string            `json:"reason,omitempty"`
	AdjudicatedBy   string            `json:"adjudicated_by,omitempty"`
	AdjudicatedAt   string            `json:"adjudicated_at,omitempty"`
	CreatedAt       string            `json:"created_at"`
}

// ClaimFilter narrows a fold query.
type ClaimFilter struct {
	Entity     string
	Topic      string
	Visibility []string
	State      string
	SinceSeq   int64
	Limit      int
}

// CandidateFilter narrows a candidate list query.
type CandidateFilter struct {
	State      string
	Entity     string
	Visibility string
	Limit      int
}

// Position is the current stance for an entity+topic, derived by folding claims.
type Position struct {
	Entity    string `json:"entity"`
	Topic     string `json:"topic"`
	Claim     Claim  `json:"claim"`
	Seq       int64  `json:"seq"`
	UpdatedAt string `json:"updated_at"`
}

// LedgerStore is the claim/candidate persistence surface.
// The file-based mem.Store satisfies this through its existing methods;
// the pg backend implements it with SQL + MVCC.
type LedgerStore interface {
	// AppendClaim adds an immutable claim to the log and returns its sequence number.
	AppendClaim(claim Claim, now time.Time) (seq int64, err error)
	// AppendEdge records a version-chain edge (supersedes, reinforces, etc.).
	AppendEdge(edge Edge, now time.Time) error
	// FoldClaims returns claims matching the filter, folded to current state.
	FoldClaims(filter ClaimFilter) ([]Claim, error)
	// CurrentPosition derives the current stance for entity+topic by folding.
	CurrentPosition(entity, topic string) (*Position, error)
	// NextSeq returns the next monotonic sequence number.
	NextSeq() (int64, error)

	// AppendCandidate writes a new candidate to the inbox.
	AppendCandidate(cand Candidate, now time.Time) error
	// UpdateCandidate mutates a candidate's state (adjudication).
	UpdateCandidate(id string, mutate func(*Candidate) error) error
	// ListCandidates returns candidates matching the filter.
	ListCandidates(filter CandidateFilter) ([]Candidate, error)
	// GetCandidate returns one candidate by ID.
	GetCandidate(id string) (*Candidate, error)
}

// ─── Events ───────────────────────────────────────────────────────────────────

// EventRecord is one event in the append-only log.
type EventRecord struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Bytes     []byte    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

// EventScanOptions controls a scan over the event log.
type EventScanOptions struct {
	Prefixes    []string
	RegularOnly bool
	Limit       int
}

// EventStore is the append-only event persistence surface.
// The file-based eventstore.Store satisfies this through its existing methods;
// the pg backend implements it with an events table + append-only inserts.
type EventStore interface {
	// PutJSON writes a JSON record, creating or overwriting.
	PutJSON(id string, value any) error
	// PutJSONIfAbsent writes only if the ID does not exist.
	PutJSONIfAbsent(id string, value any) (created bool, err error)
	// Read returns one event by reference.
	Read(ref string) (*EventRecord, error)
	// Scan returns events matching the options.
	Scan(opts EventScanOptions) ([]EventRecord, error)
	// AppendWorkLog appends a line to the work event stream.
	AppendWorkLog(data []byte) error
	// ReadWorkLog returns the full work event stream.
	ReadWorkLog() ([]byte, error)
}

// ─── Work ─────────────────────────────────────────────────────────────────────

// WorkStore is the work envelope persistence surface.
// The file-based work.Store satisfies this through its existing methods;
// the pg backend implements it with a work_envelopes table.
type WorkStore interface {
	// PutWork writes or overwrites a work envelope.
	PutWork(env envelope.WorkEnvelope) error
	// GetWork returns one envelope by work_id.
	GetWork(workID string) (*envelope.WorkEnvelope, error)
	// ListWork returns all non-terminal work envelopes.
	ListWork() ([]envelope.WorkEnvelope, error)
	// ListAllWork returns all envelopes including terminal/archived.
	ListAllWork() ([]envelope.WorkEnvelope, error)
}

// ─── Vectors (pgvector) ───────────────────────────────────────────────────────

// VectorHit is one result from a similarity search.
type VectorHit struct {
	Ref      string            `json:"ref"`
	Score    float64           `json:"score"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// VectorStore is the semantic recall surface backed by pgvector.
// This has NO file-based equivalent — it is new capability that replaces
// the O(n) directory scan in memory recall with indexed similarity search.
type VectorStore interface {
	// Upsert inserts or updates an embedding for a reference.
	Upsert(ref string, embedding []float32, metadata map[string]string) error
	// Search returns the k nearest neighbors to the query vector.
	Search(queryVec []float32, k int, filter map[string]string) ([]VectorHit, error)
	// Delete removes an embedding by reference.
	Delete(ref string) error
	// Count returns the number of stored embeddings.
	Count() (int64, error)
}

// ─── Backend selection ────────────────────────────────────────────────────────

// Backend names the active storage engine.
type Backend string

const (
	// BackendFile is the default JSON-file-on-disk storage (current behavior).
	BackendFile Backend = "file"
	// BackendPostgres is the experimental Postgres+pgvector storage. Backend
	// selection alone is intentionally insufficient for admission.
	BackendPostgres Backend = "postgres"

	// ExperimentalPostgresEnv is the second, explicit gate required in addition
	// to selecting BackendPostgres. Postgres is preserved for parity work but is
	// outside the supported public-alpha backend surface.
	ExperimentalPostgresEnv = "COMPOUND_EXPERIMENTAL_POSTGRES"
	// PostgresProfileEnv names a nonsecret operator profile. This opaque label,
	// never a raw or masked DSN, is the only connection target that may be
	// rendered or persisted in Compound authority objects.
	PostgresProfileEnv = "COMPOUND_POSTGRES_PROFILE"
)

// Config holds the storage backend selection and connection parameters.
type Config struct {
	// Backend selects the storage engine. Default: BackendFile.
	Backend Backend
	// DSN is the Postgres connection string (required when Backend=postgres).
	// Example: "postgres://<user>:<password>@<host>:5432/compound?sslmode=disable"
	DSN string
	// ExperimentalPostgres is the separate, explicit acknowledgement that the
	// selected backend is experimental and unsupported by the public alpha.
	ExperimentalPostgres bool
	// PostgresProfile is a nonsecret opaque label used for reports, policy
	// targets, receipts, and transitions. It must never contain connection data.
	PostgresProfile string
	// VectorDimensions is the embedding dimension for pgvector columns.
	// Must match the embedding model output. Default: 1536 (OpenAI ada-002).
	VectorDimensions int
}

// ConfigError is a safe, typed backend-selection refusal. Reason and Recovery
// are fixed contract text; rejected environment values and DSNs are never
// copied into the error.
type ConfigError struct {
	Field    string
	Reason   string
	Recovery string
}

func (e *ConfigError) Error() string {
	if e == nil {
		return ""
	}
	if e.Recovery == "" {
		return e.Field + ": " + e.Reason
	}
	return e.Field + ": " + e.Reason + "; " + e.Recovery
}

// DefaultConfig returns the file backend (zero-change default).
func DefaultConfig() Config {
	return Config{
		Backend:          BackendFile,
		VectorDimensions: 1536,
	}
}

// ConfigFromEnv reads backend configuration from the environment. The file
// backend is the sole supported public-alpha backend and remains the
// zero-configuration default. Selecting Postgres requires a second explicit
// experimental opt-in plus a nonsecret profile label and a DSN. Unknown or
// partial selections fail closed rather than falling back to files.
func ConfigFromEnv() (Config, error) {
	cfg := DefaultConfig()

	store := envOr("COMPOUND_STORE", "file")
	switch Backend(store) {
	case BackendPostgres:
		cfg.Backend = BackendPostgres
		cfg.ExperimentalPostgres = os.Getenv(ExperimentalPostgresEnv) == "1"
		cfg.PostgresProfile = strings.TrimSpace(os.Getenv(PostgresProfileEnv))
		cfg.DSN = strings.TrimSpace(os.Getenv("COMPOUND_DSN"))
	case BackendFile:
		for _, key := range []string{ExperimentalPostgresEnv, PostgresProfileEnv, "COMPOUND_DSN", "COMPOUND_VECTOR_DIMS"} {
			if os.Getenv(key) != "" {
				return Config{}, &ConfigError{
					Field:    "COMPOUND_STORE",
					Reason:   "partial Postgres configuration is present without selecting the Postgres backend",
					Recovery: "unset Postgres-only variables for file mode, or select postgres with the complete experimental gate",
				}
			}
		}
	default:
		return Config{}, &ConfigError{
			Field:    "COMPOUND_STORE",
			Reason:   "unsupported COMPOUND_STORE value",
			Recovery: "use file (supported) or postgres with the explicit experimental gate",
		}
	}

	if dims := os.Getenv("COMPOUND_VECTOR_DIMS"); dims != "" {
		d, ok := parseExactPositiveDecimal(dims)
		if !ok {
			return Config{}, &ConfigError{
				Field:    "COMPOUND_VECTOR_DIMS",
				Reason:   "vector dimensions must be an exact positive base-10 integer",
				Recovery: "set a positive dimension matching the experimental embedding model",
			}
		}
		cfg.VectorDimensions = d
	}

	if cfg.Backend == BackendPostgres {
		if err := cfg.ValidatePostgresExperiment(); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

func parseExactPositiveDecimal(raw string) (int, bool) {
	if raw == "" {
		return 0, false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(raw)
	return n, err == nil && n > 0
}

// ValidatePostgresExperiment protects every construction path, including
// callers that build Config directly instead of using ConfigFromEnv. It must be
// called before parsing a DSN, dialing a service, or writing any Postgres row.
func (c Config) ValidatePostgresExperiment() error {
	if c.Backend != BackendPostgres {
		return &ConfigError{
			Field:    "COMPOUND_STORE",
			Reason:   "Postgres backend is not selected",
			Recovery: "set COMPOUND_STORE=postgres only for explicit experimental use",
		}
	}
	if !c.ExperimentalPostgres {
		return &ConfigError{
			Field:    ExperimentalPostgresEnv,
			Reason:   "Postgres is experimental and outside the supported public alpha",
			Recovery: "set " + ExperimentalPostgresEnv + "=1 to opt in explicitly",
		}
	}
	if !validPostgresProfile(c.PostgresProfile) {
		return &ConfigError{
			Field:    PostgresProfileEnv,
			Reason:   "a safe profile label is required (1-64 ASCII letters, digits, '.', '_' or '-', beginning with a letter or digit)",
			Recovery: "set " + PostgresProfileEnv + " to a nonsecret label; never put a DSN or credential in it",
		}
	}
	if strings.TrimSpace(c.DSN) == "" {
		return &ConfigError{
			Field:    "COMPOUND_DSN",
			Reason:   "connection configuration is required for the selected experimental profile",
			Recovery: "set COMPOUND_DSN in the process environment; Compound will not render or persist it",
		}
	}
	if c.VectorDimensions <= 0 {
		return &ConfigError{
			Field:    "COMPOUND_VECTOR_DIMS",
			Reason:   "vector dimensions must be positive",
			Recovery: "set a positive dimension matching the experimental embedding model",
		}
	}
	return nil
}

// PostgresTargetRef returns the only Postgres identity safe for reports and
// authority objects. Callers must validate the config first.
func (c Config) PostgresTargetRef() string {
	if !validPostgresProfile(c.PostgresProfile) {
		return ""
	}
	return "postgres-profile:" + c.PostgresProfile
}

func validPostgresProfile(profile string) bool {
	if len(profile) == 0 || len(profile) > 64 {
		return false
	}
	for i := 0; i < len(profile); i++ {
		ch := profile[i]
		alphaNumeric := ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9'
		if i == 0 && !alphaNumeric {
			return false
		}
		if !alphaNumeric && ch != '.' && ch != '_' && ch != '-' {
			return false
		}
	}
	return true
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
