# ADR-0005: Storage Interface Extraction

**Status:** Historical proposal — never ratified; not the current runtime decision
**Date:** 2026-08-02
**Supersedes:** none
**Split from:** the draft ADR-0005 "Postgres+pgvector Storage Engine and
Package Consolidation" (dated 2026-07-31; never ratified)
**Historical companions:** ADR-0006 (Postgres+pgvector adoption — never
ratified), ADR-0007 (package consolidation + CLI decomposition)

> This split proposal is retained as design history, not current authority.
> The accepted bounded runtime decision is
> [ADR-0005: Postgres+pgvector Storage Experiment](0005-postgres-pgvector-storage-experiment.md):
> file storage is the sole supported public-alpha backend, while the existing
> Postgres adapter remains an explicitly gated, non-authoritative experiment.

## Context

Compound's durable state lives as JSON under `COMPOUND_HOME`: the ledger,
work envelopes, candidates, claims, and registries are loose files, and
the event store is append-only JSONL segments. Each store exists
implicitly inside its domain package — there is no named surface that
says what a ledger IS, only what the file code happens to do.

The draft ADR-0005 bundled three separable decisions into one document:
the storage seam, Postgres adoption, and package consolidation. Bundled,
none could be ratified on its own merits — the adoption argument dragged
the extraction along with it, and the extraction (which every backend
question depends on) could not be decided until the adoption argument
was won. They are split here into three ADRs. This one keeps only the
extraction.

The seam is the load-bearing decision, and it is valuable regardless of
which backend follows — including none. Naming each store's surface
makes the existing file implementation auditable against an explicit
contract, and turns any future backend into a configuration question
instead of a rewrite. The shape of the seam is also cheapest to settle
BEFORE a second implementation exists, while nothing but the contract
constrains it.

## Decision

Extract four storage interfaces as a logical seam: `LedgerStore`,
`EventStore`, `WorkStore`, `VectorStore`. The sketch below is the
decision; the Go extraction that realizes it must land additive-only
under the rules in "Lane-safety."

This ADR decides the shape of the seam ONLY. It decides no backend. The
existing file stores remain the sole implementation and the permanent
default. Whether a second implementation is ever added, and how a backend
would be selected, was the historical ADR-0006 proposal's question. The
accepted bounded experiment now owns the runtime answer.
Package consolidation (ADR-0007) is independent of this work and needs
no storage change at all.

### Requirements the seam must carry

Two requirements came from futures the draft seam did not account for.
They are part of the decision, not commentary — the sketches below
exist in their current shape BECAUSE of them:

1. **Lifecycle-free persistence (WorkStore).** Storage persists
   envelopes; `internal/work` owns transitions. A storage interface
   that can advance lifecycle state is a storage interface that can
   fork the state machine. See the `WorkStore` sketch for the defect
   this corrects.

2. **Model identity on vectors (VectorStore).** The embedding model is
   part of a vector's identity, and runtime-swappable embedding models
   are a REQUIREMENT, not a future possibility. A vector surface that
   cannot tell its models apart cannot be trusted, and cannot migrate
   incrementally. See the `VectorStore` sketch for the defect this
   corrects.

## Interface sketch

This sketch is the ratifiable artifact. No Go code lands with this ADR;
the extraction that implements it is a later, additive-only change.

```go
// LedgerStore is the claim/candidate persistence surface.
// The existing file ledger satisfies it as extracted; any future
// engine-backed implementation must satisfy the same surface.
type LedgerStore interface {
    AppendClaim(Claim) error
    FoldClaims(filter ClaimFilter) ([]Claim, error)
    CurrentPosition(entity, topic string) (*Position, error)
    AppendCandidate(Candidate) error
    ListCandidates(filter CandidateFilter) ([]Candidate, error)
}

// EventStore is the append-only event log surface.
type EventStore interface {
    PutJSON(id string, value any, indent string) (PutResult, error)
    PutJSONIfAbsent(id string, value any, indent string) (PutResult, error)
    Read(ref string) (Record, error)
    Scan(opts ScanOptions) ([]Entry, error)
    AppendWorkLog(data []byte) error
}

// WorkStore persists work envelopes. It is deliberately LIFECYCLE-FREE:
// it stores the envelope as given and never decides what a write means.
//
// The draft seam carried Advance(workID, to, opts) on this interface.
// That put the work state machine behind a storage boundary: two
// backends implementing Advance independently could disagree about
// which transitions are legal, and the state machine would have forked
// — two authorities over one lifecycle, exactly the split the envelope
// spine forbids (one meaning owner; governance enforced at adapters).
// Transitions are validated ONCE, in internal/work: the state machine
// reads the envelope, checks from->to legality, applies the change,
// and persists the RESULT through this seam. A store that cannot
// advance anything cannot fork anything.
//
// Concurrent writers to one envelope are reconciled by a storage-level
// envelope-revision CAS — an implementation concern (atomic
// rename-plus-revision-check for the file backend; a revision-guarded
// UPDATE for a transactional one) — never by lifecycle logic in the
// store.
type WorkStore interface {
    Create(env WorkEnvelope) error
    Get(workID string) (*WorkEnvelope, error)
    Put(env WorkEnvelope) error // persist as given; the caller owns the transition
    List(filter WorkFilter) ([]WorkEnvelope, error)
}

// ModelSpec names the embedding model a vector lives in. Dim is part
// of the identity: the same model id at a different output dimension
// (a truncated / MRL projection) is a different vector space.
type ModelSpec struct {
    ID  string // e.g. "voyage-3", "text-embedding-3-large"
    Dim int    // must equal len(vec) on every write
}

// VectorStore is the semantic recall surface.
//
// The draft seam carried bare []float32 with no model identity on
// Upsert OR Search. That is a correctness defect, not an ergonomics
// gap: two embedding models carve the space differently, so a query
// embedded by model A cosine-matched against model B's vectors returns
// confidently-ranked garbage — every hit wrong, every score plausible.
// This interface makes cross-model comparison UNREPRESENTABLE: every
// vector is addressed by (model, ref), every search runs inside one
// model's partition, and a dimension mismatch is refused, not
// truncated or padded. Impossible by construction, not by convention.
//
// Model addressability is also what makes embedding models
// RUNTIME-SWAPPABLE — the requirement the draft missed:
//   - re-embedding is incremental: writing a ref under model B leaves
//     the model-A vector intact, so the corpus migrates ref-by-ref on
//     touch, not in one big-bang re-index;
//   - two models coexist: a new model can be A/B'd against the
//     incumbent on live traffic, and search can serve from whichever
//     partition holds each ref, instead of requiring the whole corpus
//     rebuilt before anything can be compared.
type VectorStore interface {
    // Upsert stores vec under (model, ref). Ref + model uniquely
    // address a slot, so the same ref holds one vector per model.
    // Refused unless len(vec) == model.Dim.
    Upsert(model ModelSpec, ref string, vec []float32, metadata map[string]string) error

    // Search matches query ONLY against vectors stored under model.
    // Refused unless len(query) == model.Dim. Vectors of any other
    // model are invisible to this call by construction.
    Search(model ModelSpec, query []float32, k int, filter map[string]string) ([]Hit, error)

    // DeleteRef removes a ref's vectors under EVERY model (ref retirement).
    DeleteRef(ref string) error

    // DeleteModel drops every vector stored under model — the GC door
    // for a model whose migration is complete and whose A/B trial lost.
    DeleteModel(model ModelSpec) error
}
```

## Lane-safety rules for the extraction

1. **Additive only.** The Go extraction changes no existing import
   list, no existing function signature, no existing package location.
   A lane that built against the tree before the extraction must still
   build after it.

2. **The COMPOUND_HOME file layout does not change.** The file stores
   keep reading and writing exactly where they do today. The
   interfaces describe the existing behavior; they do not move it.

3. **The file backend is the default forever.** Whatever second
   implementation ADR-0006 may eventually admit, `COMPOUND_STORE=file`
   semantics (today: the only behavior there is) must always work.
   Backend SELECTION machinery is ADR-0006's concern, not this seam's.

4. **The arch doctor gates the extraction.** A new home for the
   interfaces needs the three coupled gates (Classification, MintHome,
   status.md) like any new organ.

## Consequences

**Positive:**
- Each store has a named, ratifiable contract instead of an implicit
  file-shaped one; the file implementation becomes auditable against
  it.
- Any future backend question (ADR-0006 or its successors) becomes
  "implement these four interfaces," not "rewrite four subsystems."
- The two corrected requirements (model identity on vectors,
  lifecycle-free work persistence) are settled at the contract level,
  where they are cheap, instead of at the migration level, where they
  are not.

**Negative:**
- Until a second implementation exists, the interfaces have exactly
  one implementer. That is acceptable for a load-bearing seam: the
  contract decides the shape of every future backend question, and
  the shape is least constrained now.

**Risks:**
- The seam overfits to the file implementation. Mitigation: the two
  requirements that forced changes to the draft sketch both came from
  a non-file future (swappable embedding models; divergent-backend
  state-machine forks) — they are the overfitting test, applied once.

## Ratification

This ADR requires operator ratification before the Go extraction
begins. The document itself is informational and breaks nothing.
