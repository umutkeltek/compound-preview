# Compound Architecture — overview & reading order

> This file is an **overview and index**, not a second object model. The canonical architecture is
> defined **once** in [`LAYERS.md`](./LAYERS.md): **one architecture — the envelope spine — with
> object families inside it, a rented edge, and the compounding loop.** Read that first.

Compound is a **local-first meaning + execution substrate** — a local monolith with deep internal
modules. Agents and users see one coherent system, not a scattered microservice estate. There is
**one** architecture, the **Runtime / Envelope Spine**; the "layers" are not a second architecture,
they are **object families the spine's transitions create, own, and adjudicate.**

- **The spine** (the architecture — how work flows): `architecture/runtime.md`,
  `architecture/substrate.md`; proof object in `architecture/transition.md`.
- **Object families** (what the spine processes — Memory, Authority, Decisions,
  Procedures/Capabilities, Permissions, Doctrine/Product/Taste): defined in
  `architecture/contracts/README.md`; the Memory family in depth in `memory-architecture.md`.
- **The rented edge** (engines / harnesses / providers / adapters — never own meaning):
  `architecture/execution-plane.md`.
- **Source adapters and generated views** (how existing systems enter Compound without
becoming peer authorities): `architecture/source-adapters.md`.
- **Information custody** (where durable bodies live, projection/writeback, restore, and legacy retirement):
  `architecture/information-custody.md`.
- **The compounding loop** (turns spine events into object families): `LAYERS.md`.

> Layers are not services, phases, owners, or an alternate architecture. They are object families
> processed by the envelope spine.

## Reading order

1. [`LAYERS.md`](./LAYERS.md) — **the canonical architecture.** Start here.
2. [`architecture/status.md`](./architecture/status.md) — which package / command is **canonical
   vs retired vs importer.** Read before touching code, so you don't extend the wrong
   family. (`compound architecture doctor` enforces it.)
3. [`architecture/runtime.md`](./architecture/runtime.md) — the spine: `COMPOUND_HOME`, the
   envelopes, hooks-as-clients.
4. [`architecture/contracts/README.md`](./architecture/contracts/README.md) — every contract /
   object family (the lego seams).
5. [`architecture/source-adapters.md`](./architecture/source-adapters.md) — generated/source
observations before importing existing views, dashboards, registries, or instructions.
6. [`architecture/information-custody.md`](./architecture/information-custody.md) — physical bodies,
   authority, projections, indexes, restore, and retirement.
7. [`memory-architecture.md`](./memory-architecture.md) — the Memory family in depth.
8. [`architecture/transition.md`](./architecture/transition.md) — the proof seam / compounding.
9. [`roadmap/phases.md`](./roadmap/phases.md) — the envelope-first construction order.
10. [`BOUNDARIES.md`](./BOUNDARIES.md) — public core vs private runtime data.

## Why a monolith, not microservices

The reference-system failure mode: many useful surfaces become hard to operate when every tool has
a different root, cwd assumption, generated view, and authority interpretation. Compound removes
that tax by giving agents **one stable interface** over the spine and its object families:

```text
compound orient         # the spine: what applies right now
compound work ...        # the spine: one unit of work
compound policy ...      # the spine: gates
compound memory ...      # object family: meaning / memory
compound capability ...  # object family: skills / tools / models graph
compound transition ...  # the proof seam
compound architecture doctor   # is the scaffolding still coherent?
```

Adapters can read files where they already live during migration, but a running instance **physically owns
durable bodies and semantically owns their adjudicated meaning**. Implementation packages, adapters, and
optional engines are internal seams — they must not become separate authority centers.

## Information placement

- **Compound-owned** canonical ledger objects (append-only JSONL under one ledger root):
  `evidence`, `entities`, `claims`, `positions`, `patterns`, `edges`. Scope is represented today by typed
  edges such as `part_of`; no separate `scope_assignments` ledger is implemented.
- **Compound-custodied source bodies**: durable private notes, policies, transcripts, exports, and attachments
  have versioned source records plus immutable artifact bodies under `COMPOUND_HOME`. Original paths remain
  provenance/rollback evidence; `source_in_place` is migration-only and is not physical custody.
- **Projections and indexes**: Markdown/dashboard/harness views are rebuildable clients; retrieval engines are
  rented acceleration. Neither owns semantic currentness or canonical bodies.

## Cwd independence

Compound never infers truth from the working directory. Correct attribution comes from explicit
identifiers, registered aliases, content signals, decision-turn signals, and source metadata. If
unclear, it returns `unresolved` / `multi_scope` rather than guessing.

## Public core and private overlay

The open-source core ships the substrate (object model, ledger, scope resolver, recall planner,
promotion/reconciliation lifecycle, context-pack assembly, fixtures, adapter interfaces). A private
running instance owns private meaning **inside `COMPOUND_HOME`** — not as a permanent external
"overlay." The public core is useful without it. Detail and rules: `BOUNDARIES.md`.
