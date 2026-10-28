# ADR 0002: Compound Physically Owns Durable Information Bodies

Status: accepted

## Context

Compound already owns semantic lifecycle through ledgers, registries, work, policy, and transitions, but its
earlier source-in-place doctrine allowed durable bodies to remain physically dependent on legacy note trees,
control-plane repositories, and retrieval databases. A path plus a hash inventories information; it does not
let an empty Compound home restore it after the source disappears. Keeping bodies elsewhere would leave the
legacy systems as permanent co-owners and make their retirement impossible to prove.

Moving files wholesale into one folder would create the opposite failure: it would preserve bytes while
collapsing source evidence, promoted meaning, editable projections, and rented indexes into a new untyped pile.

## Decision

A running Compound instance physically owns every durable private information body required for its operation
inside `COMPOUND_HOME`, using versioned source records and immutable content-addressed artifacts. It separately
owns semantic authority in its ledgers and registries. Markdown/Obsidian, dashboards, and harness files are
projections with governed writeback. Search/vector/fact systems are rebuildable rented indexes.

`source_in_place` is a compatibility state during migration, never a completed custody state. Capture is not
promotion: incoming bodies remain evidence until the normal adjudication lifecycle resolves identity,
visibility, currentness, supersession, and authority.

The physical layout converges on the habitat defined in
[`information-custody.md`](../architecture/information-custody.md). One catalog will eventually drive habitat
construction, placement, doctor, backup, compaction, and restore; current transitional roots remain explicit
debt until that vertical slice lands.

Legacy writers retire only after owned capture, isolated restore, real-consumer replay, old-path-disabled
negative control, governed write cutover, and a measured quiet window. Destructive deletion or service
shutdown remains a separate operator decision.

## Consequences

- Compound can restore durable information without the original note tree, database, repo, or index.
- Source bodies, claims, projections, and indexes can evolve on independent lifecycles without becoming peer
  authorities.
- The public repository still ships no private bodies; physical ownership belongs to a private running home,
  not to the distribution.
- Backups must prove consumer behavior after isolated restore, not only archive hashes.
- Multi-device operation needs one fenced writer and object/log replication; generic live-folder sync is not a
  supported authority merge.
- Source-family adapters must declare what they preserve. Regular-file capture cannot claim Git history,
  database-native restore, ACL, xattr, symlink, or directory semantics.
- Existing source-in-place adapters remain migration bridges and acquire explicit retirement conditions.

## Rejected alternatives

- **Keep durable bodies in legacy sources forever:** prevents ownership transfer and keeps multiple required
  systems alive.
- **Treat the retrieval database as the corpus authority:** makes a rented index own visibility, identity, and
  supersession.
- **Copy the old roots wholesale:** preserves fragmentation and hides information-class boundaries.
- **Make Markdown canonical:** couples authority to one editor format and recreates file-watcher truth.
- **Put the execution gateway inside Compound:** duplicates provider/account/transport ownership; the systems
  instead use a revisioned execution handshake.
