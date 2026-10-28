package mem

import "regexp"

// The breadth-engine wire protocol. compound OWNS this protocol; the engine (turbovec sidecar,
// or the pure-Go mock) is rented behind it. It is transport-agnostic: the same request/response
// shapes ride a held coprocess (stdio), a per-op exec, or a socket. One JSON object per line;
// stdout is the data channel (logs go to stderr only); every request carries a schema version +
// request id. Golden fixtures under fixtures/engine/ pin this wire as the swap contract that BOTH
// the mock and the real turbovec sidecar must satisfy. See docs/memory-architecture.md.

const ProtocolVersion = 1

var hashLikeClaimID = regexp.MustCompile(`^(cl_|ev_|rec_|claim_)`)

func isMeaningfulClaimID(id string) bool {
	return id != "" && !hashLikeClaimID.MatchString(id)
}

// Ops.
const (
	OpCapabilities = "capabilities"
	OpIndex        = "index"
	OpQuery        = "query"
)

// Error codes (typed so a caller maps them to refuse-vs-degrade).
const (
	CodeEmbedderMismatch  = "embedder_mismatch"
	CodeDimMismatch       = "dim_mismatch"
	CodeEngineUnavailable = "engine_unavailable"
	CodeIndexMissing      = "index_missing"
	CodeStaleGeneration   = "stale_generation"
	CodeBadRequest        = "bad_request"
)

// ClaimText is a claim reduced to its indexable text — the only thing about a claim the engine
// ever sees. The embedder (in the sidecar) turns this into a vector; the ledger keeps the truth.
type ClaimText struct {
	ClaimID    string  `json:"claim_id"`
	Text       string  `json:"text"`
	Entity     string  `json:"entity,omitempty"`
	ClaimType  string  `json:"claim_type,omitempty"`
	Seq        int64   `json:"seq,omitempty"`
	CreatedAt  string  `json:"created_at,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	Derivation string  `json:"derivation,omitempty"`
	EvidenceN  int     `json:"evidence_n,omitempty"`
}

type WireEdge struct {
	From       string  `json:"from"`
	Kind       string  `json:"kind"`
	To         string  `json:"to"`
	Confidence float64 `json:"confidence"`
}

// Request is one protocol call.
type Request struct {
	SchemaVersion int    `json:"schema_version"`
	RequestID     string `json:"request_id"`
	Op            string `json:"op"`

	// index
	CorpusGen int64       `json:"corpus_gen,omitempty"`
	Claims    []ClaimText `json:"claims,omitempty"`
	Edges     []WireEdge  `json:"edges,omitempty"`

	// query
	Text string `json:"text,omitempty"`
	Gate string `json:"gate,omitempty"`
	// NOT omitempty: an empty allowlist under gate:list means "match nothing" (fail-closed). If it
	// were dropped from the wire, a sidecar could misread an absent field as "no filter" = fail-open.
	Allowlist []string `json:"allowlist"`
	K         int      `json:"k,omitempty"`
}

// Response is one protocol reply. ok=false carries a typed error; logs never ride this channel.
type Response struct {
	OK            bool       `json:"ok"`
	SchemaVersion int        `json:"schema_version,omitempty"` // engine echoes its protocol version; skew is a hard refuse
	RequestID     string     `json:"request_id"`               // NOT omitempty: a missing id can't masquerade as a match
	Error         *WireError `json:"error,omitempty"`

	// capabilities
	Name     string `json:"name,omitempty"`
	Breadth  bool   `json:"breadth,omitempty"`
	Embedder string `json:"embedder,omitempty"`
	Ready    bool   `json:"ready,omitempty"`

	// index
	Indexed int `json:"indexed,omitempty"`

	// query
	Hits          []Hit    `json:"hits,omitempty"`
	DegradedLanes []string `json:"degraded_lanes,omitempty"`

	CorpusGen int64 `json:"corpus_gen,omitempty"`
}

type WireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ClaimIndexText is the indexable text for a claim — the entity/predicate/object surface the
// engine matches or embeds. Stable so the mock and the real sidecar agree.
func ClaimIndexText(c Claim) string {
	text := c.Entity + " " + c.Predicate + " " + c.Object
	if isMeaningfulClaimID(c.ID) {
		text += " " + c.ID
	}
	return text
}
