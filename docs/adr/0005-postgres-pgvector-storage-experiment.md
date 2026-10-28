# ADR-0005: Postgres+pgvector Storage Experiment

**Status:** Accepted for additive, opt-in experimentation; production authority cutover is not ratified
**Date:** 2026-07-31
**Supersedes:** none
**Retires:** nothing yet; the file backend remains authoritative
**Related:** ADR-0007 (package consolidation and CLI decomposition)

Implementation has advanced beyond the original proposal: an experimental
`internal/persist/pg` adapter and `compound store` operator surface now exist.
This decision preserves that work for hardening and parity testing. It does not
approve production cutover, file-backend retirement, automatic fallback, or
package consolidation.

## Context

Compound's durable state lives as JSON files under `COMPOUND_HOME`. Every
query is a directory scan + file read + JSON unmarshal. The event store is
append-only JSONL segments (the right idea), but the ledger, work envelopes,
candidates, claims, and registries are loose files with no indexing, no
transactions, and no concurrent-write safety beyond file-level atomicity.

Meanwhile, 61 internal packages exist, of which 14 have a single source file.
The CLI dispatch is a 5,432-line `main.go` with 75 case branches in one
`package main`.

*(Package and line counts as of `5546f479`, 2026-08-02; they age as the tree
moves.)*

Active agent lanes (6+ concurrent Claude/Codex sessions, 80+ worktrees) mean
any breaking import-path change risks compile failures across live work.

## Decision

### 1. Preserve an opt-in Postgres+pgvector experiment behind owner-defined contracts

Keep the existing PostgreSQL work as a bounded experiment, but do not ratify the
current broad `internal/persist` interfaces as the final architecture. The file
stores do not currently satisfy those interfaces, and the copied persistence
models omit authoritative fields.

The corrected target is:

- `mem`, `eventstore`, and `work` own their domain types and persistence contracts;
- file and PostgreSQL adapters implement the same owner-level behavior suites;
- PostgreSQL-specific records and codecs remain inside the adapter;
- backend construction happens at the CLI composition root;
- PostgreSQL remains opt-in and non-authoritative until parity is proven.

**Why Postgres, not SQLite:**
- The direction ladder requires multi-agent concurrency (Phase 2+) and
  eventually multi-process access. SQLite's single-writer lock is a ceiling.
- pgvector gives native vector similarity search for memory recall, replacing
  the O(n) directory scan in `compound memory recall`.
- Postgres runs as a local service (via Postgres.app, Homebrew, or Docker),
  fitting the local-first model. No cloud dependency.
- The `breadth.EmbedderAdapter` already exists as the embedding seam; pgvector
  is its natural persistence target.

**Why an interface layer, not a big-bang migration:**
- Active lanes must not break. The interface layer is purely additive.
- The file-based implementation remains the supported DEFAULT. Postgres is
  admitted only when all four process-local inputs are present:
  `COMPOUND_STORE=postgres`, `COMPOUND_EXPERIMENTAL_POSTGRES=1`, a nonsecret
  `COMPOUND_POSTGRES_PROFILE`, and `COMPOUND_DSN`. Selection alone fails closed.
  `COMPOUND_DSN` is a process-memory-only DSN: it may be used to establish the
  connection but never becomes a report, receipt, transition, or persisted
  configuration value. Every unknown/partial configuration is refused
  without file or breadth fallback.
  Runtime output and durable authority objects use only
  `postgres-profile:<label>`; even a masked DSN is never rendered or persisted.
- Migration is per-object-family, not all-or-nothing.

### 2. Package consolidation is a separate deferred decision

Package moves are not part of this storage experiment. ADR-0007 defines the
operator freeze protocol if consolidation is ever separately approved.

### 3. CLI decomposition is out of scope

The current burst may make the `store` command coherent, but does not authorize
a broad CLI package split.

## Sequencing

```
Experiment baseline — current burst
  ├── Preserve the file backend as authority and default
  ├── Keep Postgres opt-in and non-authoritative
  ├── Repair build, command admission, diagnostics, and tests
  ├── Replace copied domain models with owner-defined contracts
  └── Prove strict migration parity before proposing cutover

Future cutover proposal — requires separate operator approval
  ├── Versioned schema and compatibility policy
  ├── Lossless, resumable migration and restore drill
  ├── Shadow reads with zero-bypass evidence
  └── Family-by-family authority decision

Package consolidation — ADR-0007 only
  └── No package move occurs in the storage burst
```

## Experiment safety rules

1. The file backend remains authoritative and is the zero-configuration default.
2. Postgres is explicit opt-in experimentation; no automatic fallback may hide
   an unavailable or inconsistent selected backend.
3. `compound architecture doctor --strict` and the canonical proof battery gate
   every phase.
4. Existing files are never deleted or rewritten by migration in this burst.
5. No production authority cutover occurs until file/Postgres contract parity,
   strict migration, backup/restore, and anti-bypass evidence are ratified.
6. Unknown backend names and incomplete experimental configuration are
   selected-but-unavailable states, never permission to fall back silently to
   the file/breadth lane.

## Current implementation and required correction

The preserved WIP includes `internal/persist`, `internal/persist/pg`, and
`compound store`. These are an experiment, not the final seam. The current
copied claim/candidate/edge models and broad cross-domain store interfaces must
be replaced by contracts owned by `mem`, `eventstore`, and `work`, with
lossless versioned codecs in the Postgres adapter.

## Dependencies added

```
go.mod additions (Phase 1):
  github.com/jackc/pgx/v5       // Postgres driver (pure Go)
  github.com/pgvector/pgvector-go // pgvector types for Go
```

This breaks the zero-dependency constraint. The trade-off is conscious:
Postgres concurrency + pgvector recall are worth more than dependency purity.
The spine (envelope, policy, work state machine) remains dependency-free;
only the persistence adapter imports pgx.

## Consequences

**Positive if the experiment earns cutover:**
- Indexed relational and semantic queries can avoid repeated full-directory scans.
- PostgreSQL MVCC can support explicitly coordinated multi-process access.
- A shared contract suite can expose semantic differences between file and SQL adapters before authority moves.

**Negative:**
- The opt-in path adds a running service and external Go dependencies.
- File and PostgreSQL adapters must both be maintained during experimentation.
- Backup, restore, schema compatibility, credentials, and failure recovery become part of the operational contract.

**Risks:**
- A broad persistence package can become a second, lossy domain model.
- A successful SQL write is not parity if sequence, provenance, references, or fold behavior changed.
- ANN/vector tuning can produce fast but incorrect recall unless exact-search parity is established first.

## Ratification boundary

This ADR ratifies only additive experimentation and proof work. Production
authority cutover requires a later decision backed by owner-level contract
parity, strict resumable migration, backup/restore rehearsal, and zero-bypass
evidence. The conflicting ADR-0006 adoption proposal remains historical and
never ratified; any future production cutover requires a new decision.
