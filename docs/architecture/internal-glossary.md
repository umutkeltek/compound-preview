# Internal Glossary

This file holds architecture terms that should exist in code, docs, receipts,
and drilldown views, but should not dominate the day-to-day operator console.

## Source Surface

An external or legacy place information currently lives: markdown, registry
file, generated view, runtime config, transcript, packet, metric export, or old
control-plane file. During migration its path remains provenance, but durable
private bodies must enter Compound custody before the surface can retire.

## Physical Custody

Compound can reproduce a declared source version and required metadata from
`COMPOUND_HOME` while the original source is unavailable. A path, hash, row
count, manifest, or index hit alone is inventory, not custody.

## Source Record

The stable identity and version lineage of one source item: source key,
provenance, body ref, hash, metadata, privacy, capture time, and supersession.
Capture preserves evidence; it does not promote the body's statements.

## Lossless Source Graph

The preserved layer of source records, observations, artifact refs, hashes,
migration-only external refs, and structure overlays that lets Compound recover original
detail after creating summaries, claims, or projections.

## Authority Surface

The Compound-owned object that decides currentness, visibility, supersession,
promotion, and retirement for a class of information. Examples include the
memory ledger, capability graph, policy registry, work ledger, and product
signal ledger.

## Projection

A rebuildable rendering of authority for a consumer: dashboard card, markdown
view, generated agent file, CLI summary, ContextPack display, or console JSON. A
projection can be fresh or stale; it cannot prove truth by itself.

## Projection Edit

A proposed change made through Markdown, Obsidian, a dashboard, or another
rendered client. It becomes evidence and a governed writeback proposal; it does
not mutate ledger or registry authority directly.

## Index Adapter

A rented retrieval, search, vector, or facts engine that accelerates lookup over
Compound-owned objects. It may rank and retrieve, but it must not own canonical
IDs, visibility, promotion, or supersession.

## Compatibility Wrapper

A command, hook, or file kept so existing workflows continue while Compound
takes authority underneath. A wrapper is acceptable only when it delegates to a
Compound authority surface or clearly reports that it is reading evidence-only
fallback state.

## Drilldown Path

The chain from compact runtime context back to full source detail: ContextPack
or projection line -> claim or position -> evidence -> source record or
artifact. Drilldown lets agents stay compact without losing access to details.

## Retirement Gate

The proof condition that lets an old source, projection, wrapper, or owner lose
authority. A retirement gate names the new Compound owner, the real consumer,
and the command or receipt proving the consumer used the new path.

## Preference Setting

An explicit, scoped, precedence-aware operator choice in config/registry
authority. A learned or inferred preference is a candidate/claim until accepted;
an enforceable permission or prohibition is policy, not a preference.

## Habitat Catalog

The future shared declaration of every top-level `COMPOUND_HOME` root, its owner,
durability, backup, restore, compaction, and placement behavior. A directory
constructor or doctor allowlist that does not consume this catalog is a drift risk.
