# ADR-0006: Postgres+pgvector Adoption

**Status:** Historical proposal — never ratified; not the current runtime decision
**Date:** 2026-08-02
**Depends on:** ADR-0005 (the storage seam this would plug a second
implementation into)
**In tension with:** ADR-0001 (local monolith information kernel)
**Split from:** the draft ADR-0005 "Postgres+pgvector Storage Engine and
Package Consolidation" (dated 2026-07-31; never ratified)

> This trigger proposal remains useful design evidence but is not current
> authority. The accepted bounded decision is
> [ADR-0005: Postgres+pgvector Storage Experiment](0005-postgres-pgvector-storage-experiment.md):
> preserve the already-built adapter for explicit parity experiments, keep file
> storage as the sole supported authority, and require a separate experimental
> gate plus opaque nonsecret profile identity. No production cutover is ratified.

## Context

The draft ADR-0005 argued for Postgres from MECHANISM: "every query is
a directory scan + file read + JSON unmarshal," and pgvector would
"replace the O(n) directory scan in `compound memory recall`." Those
sentences describe what the current implementation DOES — they do not
establish that what it does is too slow, too small, or too contended at
the corpus compound actually has. "O(n) scan" is a verdict on an
algorithm, not a measurement of a problem: no latency is cited, no
corpus size, no observed failure. The mechanism will certainly be a
bottleneck AT SOME N — and compound's corpus is nowhere documented near
that N today.

Adopting a running database on the strength of a mechanism description
rents operating tax against a problem that may be years away — and, per
"Tension with ADR-0001" below, pays a structural cost that does not
refund if the problem never arrives.

## Decision

Adopt Postgres+pgvector as a second, opt-in implementation of the
ADR-0005 seam ONLY if and when the measurement in the Trigger section
is filed and ratified. Until then: no `pgx` dependency, no DSN
configuration surface, no second backend. The file stores remain the
sole implementation.

When — if — the trigger fires, the adoption SHAPE is already settled
here, so the re-proposal is a measurement attachment, not a redesign:

- **Postgres, not SQLite.** The direction ladder's multi-agent future
  needs multi-process concurrent writes; SQLite's single-writer lock
  is a ceiling. pgvector supplies native similarity search. Postgres
  runs as a local service (Postgres.app, Homebrew, Docker), keeping
  the local-first model — no cloud dependency.
- **Historical proposed opt-in shape, never a replacement.** This proposal
  named `COMPOUND_STORE=postgres` plus `COMPOUND_DSN`; that two-input shape was
  not ratified and is not the current admission contract. The accepted
  experiment additionally requires `COMPOUND_EXPERIMENTAL_POSTGRES=1` and an
  opaque `COMPOUND_POSTGRES_PROFILE`. The file backend stays default and must
  keep working forever.
- **Migration per object family, not all-or-nothing.** A
  `compound store migrate` command copies file-based state into
  Postgres; it never deletes the files. A `compound store doctor`
  validates both backends.
- **Dependency purity break, confined.** `go.mod` gains
  `github.com/jackc/pgx/v5` and `github.com/pgvector/pgvector-go`.
  The break is conscious and confined: the spine (envelope, policy,
  work state machine) stays dependency-free; only the persistence
  adapter imports pgx. This cost is only worth paying once the
  trigger is met.

## Trigger

This section records the trigger the historical proposal required. This ADR
will not change state; any future adoption decision must be a new ADR and must
attach a measurement filing containing ALL of:

1. **Corpus tiers.** `compound memory recall` end-to-end p95 latency
   (query issued → ranked hits returned) on the reference machine,
   measured at 1k, 10k, 100k, and 1M chunks of corpus, with the
   machine spec and the recall query set stated.
2. **The crossing point.** The corpus size N at which recall p95 first
   exceeds **250 ms** — the interactive-recall budget, above which
   recall visibly stalls the loop it serves. **The adoption trigger
   is: recall p95 > 250 ms at N ≤ 100k chunks.** (If the curve crosses
   250 ms only above 100k, the filing states the crossing N and the
   corpus growth rate, and adoption stays deferred — the scan is still
   cheaper than the service at the scale compound will actually run.)
3. **OR the contention trigger.** An observed lost update or corrupt
   write on a shared object family under concurrent lanes — two
   writers, one object, a last-writer-wins that demonstrably lost
   data, with the incident ref cited. The file backend's ceiling is
   file-level atomicity; this is the failure a transactional engine
   actually fixes, and it arms the trigger regardless of latency.

Either trigger arms the re-proposal; latency alone at a corpus compound
will not reach for years does not. No measurement, no adoption —
however elegant the mechanism argument.

**Ownership of the measurement:** whoever lands a change that grows the
corpus ~10x (or migrates a new object family into recall) runs the
recall-latency battery at the new scale and attaches the numbers to
this ADR's filing record. The trigger is measured at scale milestones,
not discovered in an outage.

## Tension with ADR-0001

ADR-0001 makes compound a local monolith: one binary, one canonical
authority, rented adapters — and "Public Compound must work with
synthetic fixtures and no private overlay." A running Postgres instance
is a SERVICE, and a service compound REQUIRES is a service whose outage
stops compound. Every query path routed through a required database
turns "compound is down" into "compound is down OR postgres is down" —
operating tax ADR-0001 exists to prevent, paid in advance for a scale
the current corpus has not reached. This is the structural cost that
does not refund: a dependency can be removed from `go.mod`; an operator
habit of "start postgres before compound" is forever.

The adoption, if it ever fires, must keep these constraints or it
violates ADR-0001 rather than merely tensioning it:

- Postgres is OPT-IN (env switch + DSN). No compound command may
  require a database to be present.
- The file backend is the default forever, not a deprecation runway.
- A compound that cannot recall its own memory because a service is
  stopped has ceased to be the kernel and become a client. If the
  trigger fires and these constraints cannot be kept, the answer is
  "no Postgres," not "weaken the constraints."

## Consequences (of deferral)

**Positive:**
- No operating tax, no new dependency, no second backend to maintain;
  the ADR-0001 posture stays intact.
- The ADR-0005 seam preserves the option at full value: deferring the
  engine costs nothing the seam didn't already protect.

**Negative:**
- If the corpus outgrows the file scan before anyone measures, recall
  degrades silently. Mitigation: the filing obligation above is tied
  to corpus-scale milestones, so the measurement happens where the
  growth does.

**Risks:**
- The trigger is never measured because the milestone obligation goes
  unenforced. Mitigation: the obligation is named per-change, not
  per-quarter; the arch doctor or a recall-scale check can pick it up
  if no replacement decision exists when the corpus is clearly large.
- The tension with ADR-0001 gets "resolved" by quietly making Postgres
  required. Mitigation: the constraints above are written as violation
  conditions, so a future re-proposal that drops them is visibly a
  different (and larger) decision than this one.
