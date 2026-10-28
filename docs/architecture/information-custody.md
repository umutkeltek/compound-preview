# Information Custody: one physical home, one meaning owner

This document owns the **information-custody slice** of the architecture. It is not a second object model:
the envelope spine remains the runtime grammar, and the contracts remain the object model. This document
answers a narrower question that those documents previously left ambiguous: *where are the durable bodies,
who may interpret them, how do users edit and restore them, and what must be proven before a legacy owner can
retire?* ADR 0002 records the decision.

## 1. Four contracts that must never collapse

Calling all four "memory" recreates the fragmentation Compound is meant to remove.

| Contract | Owns | Does not own |
|---|---|---|
| **Physical custody** | exact recoverable bodies and source versions inside `COMPOUND_HOME`, with hashes, provenance, privacy, retention, and restore metadata | truth, relevance, promotion, or currentness |
| **Semantic authority** | entity identity, evidence, claims, positions, decisions, policy, registries, supersession, visibility, and retirement | source rendering or retrieval ranking |
| **Projection** | a rebuildable consumer view such as Markdown, Obsidian, a dashboard, a harness file, or a compact timeline | canonical bytes or an independent write lifecycle |
| **Index** | disposable FTS/vector/graph/fact acceleration over Compound-owned refs | canonical IDs, ambient eligibility, currentness, or truth |

An owned source body may contain a false or superseded statement. A promoted claim may cite several body
versions. A fresh Markdown projection may still display a disputed claim. A perfect vector hit may be excluded
by privacy or currentness policy. The separate contracts make those cases ordinary instead of exceptional.

There is one owner **per contract**, not one object pretending to own every concern. The artifact store may own
the body while the ledger owns its interpreted meaning and a dashboard renders a projection. What is forbidden
is two physical owners for the same required body, two semantic owners for the same position, or a projection or
index acquiring an independent write lifecycle.

**Physical ownership invariant:** a source family is not in Compound custody until an empty-home or isolated
restore can reproduce its declared body and required metadata while the original source is unavailable.
Inventory, a path, a hash, a database row count, or a search result is not custody.

The private artifact writer and exact-stage recovery share an exclusive lock on the existing
`private/artifacts/sha256` directory. An interrupted `.artifact-*` file is evidence of observed bytes,
not proof that its writer completed the intended body. `home private-custody repair --recover-stage
NAME=REF` can publish only an explicitly selected, identity-stable, hash-matching stage without replacing
an existing digest. It leaves unsafe or ambiguous stages for manual review; old binaries must be
quiesced before adopting this lock protocol. The repair assumes a trusted local
home: its cooperative lock and inode checks cannot exclude a hostile same-UID
swap between final validation and unlink.

## 2. The runtime habitat

The end-state top-level habitat is intentionally small:

```text
$COMPOUND_HOME/
  manifest.yaml
  config/       sources/       artifacts/     ledger/        registries/
  inbox/        work/          events/        context/       reviews/
  projections/  indexes/       archives/      tmp/
```

| Root | Durable responsibility |
|---|---|
| `config/` | explicit operator settings, policy inputs, adapter configuration, schema/contract locks, and secret references; never learned guesses |
| `sources/` | source-family records, capture generations, stable source identities, connector checkpoints, and custody manifests |
| `artifacts/` | immutable content-addressed bodies and large evidence; a body ref survives source-path and index changes |
| `ledger/` | append-only evidence, claims, positions, decisions, transitions, and route evidence |
| `registries/` | current inside-world and outside-world authority: entities, projects, services, capabilities, policies, deployments, adapters, and source bindings |
| `inbox/` | untrusted observations and candidates awaiting reject, park, promote, or operationalize |
| `work/` | WorkEnvelope lifecycle, leases, checkpoints, runs, and delivery proof |
| `events/` | normalized raw runtime/hook/tool events; evidence, not automatically meaning |
| `context/` | ContextRequests, frozen ContextPacks, inclusion/exclusion reasons, exact injected text, and backup-preserved coordination-delivery cursors/reports/measurement receipts under `context/coordination-delivery/` |
| `reviews/` | adjudication, owner writeback, contradiction, policy, and retirement review artifacts; versioned core reviews may carry append-only digest-bound `work-binding` companions whose historical digests remain immutable and whose exact-head supersessions preserve source provenance and select explicit active authority without rewriting the core |
| `projections/` | rebuildable Markdown, dashboard, harness, timeline, and export views |
| `indexes/` | disposable local or remote retrieval state and build manifests |
| `archives/` | sealed local snapshots, import/export bundles, retention manifests, and rollback material |
| `tmp/` | restart-safe scratch only; no fact or cursor whose loss changes behavior |

The shared **HabitatCatalog** now declares every observed top-level root and drives home scaffolding, placement's
canonical allow-set, doctor inventory, backup inclusion, compaction, and restore validation. An unknown top-level
directory or file makes doctor and backup fail. Transitional feature-shaped roots remain explicit `bridge`
entries: each names its canonical destination and retirement condition, is preserved by backup, and is excluded
from canonical placement so new writes still receive a redirect. This is inventory/control ownership, not a claim
that the layout migration is complete; path constructors move behind the catalog one producer at a time before
their bridges retire. Behavior-bearing historical files under `projections/` are catalog exceptions: backup keeps
them and compaction refuses them until their producers cut over to their true typed homes. Connector offsets belong
with the source adapter, work cursors with work, and schema/contract locks with config.

The first nested convergence is executable: `sources/manifests/` owns SourceManifest storage, while
`registries/source-manifests/` is a backup-preserved, placement-denied bridge. Canonical state always wins. The
reader falls back only when the canonical file is absent, never when it is corrupt or invalid. A write after a
legacy-only read publishes canonical schema and authority without deleting the bridge. This gives each source
family a reversible migration path and a visible `degraded` doctor state until the bridge is independently proven
unnecessary.

The opt-in streamed-v3-event-snapshot backup profile warms native clones outside
home admission, then freshly enumerates the namespace under admission. It reuses
a warm clone only when its exact path, device, inode, size, mode, modification
time and change time still match. Changed/new bodies are cloned again and deleted
entries are omitted. Selection is sealed before admission is released; replay,
hashing, serialization, verification and cleanup run outside admission. The
existing 30-second cooperative admission deadline is unchanged. A blocked
filesystem syscall cannot be interrupted by that deadline; there is no unlimited
locked fallback.

Archive headers retain numeric UID/GID and file metadata without resolving host
account names. Backup and capture therefore do not require an account service;
restore continues to create files as the invoking user.

Final reconciliation uses Darwin bulk metadata with aligned 128 KiB buffers and
at most four traversal workers. Visits are serialized, each parent joins its
descendants before checking its pinned identity, and delayed child opens retain
the observed directory identity. Unsupported bulk metadata falls back to the
existing rooted walker only before any entry is emitted; the original directory
fence still applies. Malformed or late errors refuse the capture. Unsupported
native clones fail closed without a copying fallback. Other backup profiles
retain their existing behavior.

The disk index is fixed at 128 MiB. Each of the warm and selection journals keeps
its existing 1 GiB limit, giving an aggregate temporary metadata ceiling of
2 GiB plus 128 MiB. Both cache sizes are checked before hashing. Warm and final
inventories each retain their existing 1.5-million-entry and 64 GiB logical-body
limits; a completely changed selection can temporarily retain two inventories
(up to 128 GiB of logical clone data). Physical clone allocation depends on
filesystem sharing and subsequent writes. Body storage uses 256 lazy shards,
up to 512 retained shard descriptors, plus four depth-bounded traversal chains.
Descriptor exhaustion fails closed. These temporary resources are removed by
the owning invocation through pinned, identity-checked handles.

Bounded journals seal entry metadata, source keys, references and clone
identities. Replay checks references against sealed warm records and rechecks
the cache digests. Paths must be valid UTF-8. The shared serializer consumes
only the selected private capture; archive format, chunk budgets, link records
and restore remain v0.3. Source identity stays stable through clone completion;
non-event files also retain size/modification time, while an event append may
continue on its pinned inode. Admission fences cooperating writers, so this is
a collection of per-file observations, not a globally transactional snapshot.
Later live-file replacements or appends cannot alter the selected archive.

Falsifiers cover changed membership/generations, admission release/deadline,
cache/reference tampering, replaced shards, unsupported bulk fallback, delayed
child and ancestor replacement, and joined worker failure. The cardinality
benchmark supports an exclusive caller-owned retained fixture so large synthetic
cleanup is a separate operation, outside the application deadline. A million-file
run reached archive publication and all-body restore checks with 14.04 seconds
of reconciliation while 6,797 writer transactions completed during warming.
That attempt timed out during synthetic cleanup and remains failed. A separate
fresh restore of its retained archive passed every body and paired-generation
check; this evidence does not relabel the timed-out attempt. The existing
64 MiB admission-release and 120 MiB live-append falsifiers remain.

Native archive verification also hashes the exact compressed source through the
same verified file descriptor. A checksum is returned only after file identity,
size, modification time, and path are rechecked. The checksum names the actual
archive bytes; it is not a substitute for member or restore verification.
Scheduled transfer consumers can use this owner-produced checksum without
launching a separate interpreter to reopen a protected archive.

## 3. Information placement grammar

| Information encountered | Physical body | Meaning owner | User-facing form |
|---|---|---|---|
| authored note, policy, document, or attachment | immutable artifact plus versioned SourceManifest | evidence/candidate/claim or the relevant typed registry after adjudication | Markdown/Obsidian projection |
| conversation or imported corpus page | transcript/export artifact plus event/source records | observations first; promoted claims only after adjudication | conversation/history projection with drilldown |
| explicit user preference | source evidence plus versioned config mutation receipt | scoped `PreferenceSetting` in config/registry authority | settings screen and explain view |
| inferred preference | observation artifact if needed | candidate or claim; never silently a setting or policy | review suggestion |
| one-task instruction | ContextRequest/TaskContract body | frozen TaskContract on the WorkEnvelope | task view; expires with work |
| approval, permission, or prohibition | decision evidence | Policy/StandingGrant authority | policy review and audit trail |
| project, service, host, source, or tool fact | source evidence | typed registry entry with status and provenance | topology/dashboard projection |
| deployed location or version | runtime observation/heartbeat evidence | `RuntimeBinding` with environment, artifact/config revisions, currentness, and health refs | deployment status card |
| procedure, skill, model, harness, or adapter | source/evaluation evidence | CapabilityGraph plus lifecycle and evaluation edges | task-scoped capability offer |
| action, model call, cost, or result | event and provider/gateway evidence | Work/Receipt/Transition and ExecutionRouteEvidence | activity, cost, and “why” views |
| vector, FTS, graph, fact row, or embedding | rebuildable index bytes | no meaning authority | search accelerator only |

Two sharp boundaries follow:

- An explicit preference may become config through a governed write. A learned preference stays a claim until
  the operator accepts it. An enforceable rule is policy, not a “strong preference.”
- A deployment is not a prose fact. It is a temporal `RuntimeBinding`; the old binding becomes stale or
  retired when a new one is evidenced. A dashboard cannot make it current by rendering it.

### One-owner matrix

This matrix covers the legacy families and clients most likely to become accidental peer owners. The bridge is
temporary; the exit proof is what permits subtraction.

| Family or client | Physical body owner | Semantic owner | Temporary bridge / projection | Exit proof |
|---|---|---|---|---|
| authored note or policy tree | source versions + immutable artifacts | evidence, claims, decisions, config, or CapabilityGraph according to content | Markdown/Obsidian source and editable projection | isolated restore, governed writeback, real-reader replay, old writer quiet |
| retrieval/fact database and raw corpus | database-native export artifact plus source-family manifest | Compound ledger; raw corpus remains evidence | read-only retrieval/import adapter and rented index | frozen contract parity, production-reader replay, legacy-unavailable negative control, terminal export |
| control-plane repository | Git/source artifacts preserve exact history; Compound owns required runtime bodies | EffectiveContract, policy, registries, work, and capabilities | compatibility CLI, generated harness files, source adapters | every real consumer uses Compound; generators and writers stop; rollback remains verified |
| conversation and session trace | transcript/tool-output artifacts plus normalized events | candidates first; promoted claims, rules, and capabilities only after adjudication | harness-native history and import adapters | capture and recall replay without the old history store; no silent fallback |
| user preference or setting | config source/receipt plus cited preference evidence | explicit scoped setting; inferred preference remains a claim | settings UI and generated client config | revisioned read/write round trip, conflict proof, client drift check |
| hook installation | tracked handler source plus installed-manifest/config evidence | semantic lifecycle requirements in the effective contract; policy remains policy authority | harness-native HookPlan and installed hook files | native/fallback behavior replay, content/readiness drift check, old hook unavailable |
| dashboard or console | source assets in the repo; cached view state is disposable | the ledger, registries, policy, work, and runtime bindings it reads | one bounded projection with governed action clients | stale data is visible; actions traverse the owner; no hidden write API or duplicate port |
| model gateway | gateway owns its live provider/account/credential/config bodies | Compound owns semantic profile, admission, budget, outcome, and learning | revisioned conformance projection plus route evidence | requested/effective tuple reconciles to a task Receipt; no duplicated provider registry in Compound |

### Preference and setting flow

Explicit and inferred preference take different paths:

```text
explicit instruction
  → scoped setting proposal → ActionIntent → PolicyDecision → versioned setting → client projections

observed behavior or mined conversation
  → evidence → preference candidate → park/reject/promote as claim
  → operator acceptance, if desired → scoped setting proposal
```

Setting precedence is explicit and scope-aware: task override → project/product setting → operator default →
system default. A narrower setting may override a broader one only inside its declared scope. Every effective
value cites the winning revision and the shadowed values. Conflict, unknown scope, stale base revision, or an
attempt to turn a preference into policy goes to review; none use last-write-wins. Removal supersedes or retires
the setting and regenerates clients—it does not erase the evidence that explained the old value.

## 4. Source custody and meaning flow

```text
register source identity
  → capture exact version into artifacts
  → verify body/hash/metadata without the original
  → record SourceManifest lineage
  → emit EvidenceItem / SourceObservation
  → resolve entity (uncertain means parked)
  → reject | park | promote | operationalize
  → Claim / Position / Decision / RegistryEntry
  → ContextPack, projection, and index
```

Capture and promotion are independent transactions. Failure after capture leaves owned evidence, not ambient
truth. Failure after an observation leaves a candidate, not a half-written registry. A recapture of the same
stable source identity creates a new version and explicit supersession; it does not replace history. Identical
bodies deduplicate. A path move must not create a new identity once an explicit source ID or entity binding is
known.

`source_in_place` remains readable only as a migration bridge. `owned_snapshot` means Compound can serve the
body from its artifact store. A database export manifest that lists rows or hashes but does not custody each
declared recoverable body remains `custody_complete=false`. Database-native dumps, Git history, directories,
symlinks, xattrs, ACLs, and compound document packages require source-family adapters; a regular-file snapshot
must not overclaim those semantics.

An `owned_snapshot` body is resolved by the corpus ONLY through its hash- and size-verified `BodyRef` (the
existing `home.ArtifactStore`). The corpus resolver enforces the same three-way invariant on every consumer —
BodyRef digest equals `sha256(bytes)`, `sha256(bytes)` equals the recorded `FileHash`, and the byte length
equals `BodyBytes` when `BodyBytes` is recorded as positive — and refuses to fall back to `SourceRef` if the
artifact is missing, corrupt, size-mismatched, or hash-mismatched. A private `owned_snapshot` item additionally
requires a private + local/loopback lane posture before any indexing body read or embedder call; a public lane
carrying a private `owned_snapshot` item fails closed before the embedder interface is touched. Source-ref
fallback is forbidden on purpose: completed custody means the owned body IS the source, and any silent
bridge would hide custody corruption.

The `body_policy=source_in_place` seam has a parallel opt-in gate exposed as
`LaneConfig.SourceInPlacePrivacyGate`. Default off preserves the documented legacy behavior:
`BuildSourceManifest` emits `source_in_place` items with `privacy=private` while the published
quickstart runs public lanes — that contract is older than this gate and silently widening it
would break a documented external surface. When the operator opts in, a `source_in_place` item
whose `privacy=private` is refused unless the lane posture is `privacy=private` plus a local/loopback
embedder (the same `laneIsPrivateLocal` predicate the `owned_snapshot` gate uses). One shared
current-item access predicate guards index/rebuild before body reads, query before query-engine or
embedder work, and chunk-body resolution before `source_in_place` bytes are read — including
`IncludeText=true`. Query reloads privacy from the current `SourceManifest`, so an index created
while the gate was off cannot be queried as a bypass after the gate is enabled, and a post-index
privacy change takes effect without waiting for rebuild. Governed items missing from the current
manifest and malformed privacy labels fail closed. The refusal error names the item id, lane privacy,
and embedder identity so operators can pinpoint the leak path without grepping production logs.
The compatibility resolver form that has no lane config retains default-off legacy behavior; the
configured query path always supplies the validated lane and has no caller-side widening override.
The gate's configured shape is refuse, never widen, and `item.Privacy` values other than exactly
`public` / `private` (case-insensitive, trimmed) fail closed with a malformed field error. The
off-by-default posture is the load-bearing claim this evidence packet makes; enabling the gate is an
explicit, audited `LaneConfig` mutation that should land in the same commit as the operator's privacy
review, not be inferred from an ambient config knob.

The first database-export custody adapter uses `manifest-create --from-export ...
--dump ...` accepts a dump only when its streamed SHA-256 equals the export's exact `dump_ref`, then records
`dump_body_ref` and `dump_bytes` at family scope. A mismatch publishes no artifact. `manifest-doctor` keeps an
inventory-only export pending and fails a missing, corrupt, wrong-ref, or wrong-size owned dump;
`manifest-materialize-dump` streams the private dump back with mode `0600` without requiring the original.
This proves dump-byte custody only. Database restore, schema/application validity, real-consumer replay,
negative control, write cutover, quiet window, and the operator retirement act remain separate proofs.

Production-reader parity is also dependency-attested, not inferred from matching refs. `memory parity-replay`
may bind `--expect-engine <id>` and `--forbid-engine <id>` into its checkpoint identity; every orient/query row
then records bounded requested/observed engine identity, outcome, embedder, and corpus generation from the real
consumer path. An expected engine passes only when that observed engine was actually used, and a forbidden
requested or observed engine fails even if result refs match. Omitting both flags preserves the existing parity
exit contract while still emitting engine evidence.

The current generic v0.2 SourceManifest slice owns regular-file bytes, media type, byte count, permission mode,
stable inferred source key, and version lineage. Its inferred key uses explicit entity refs when available and
otherwise path identity. Explicit source IDs and path-move reconciliation are required before bulk migration.

## 5. Recall, context injection, and hooks

An index may return candidate refs; Compound re-resolves them against ledger identity, visibility, currentness,
scope, privacy, and supersession before inclusion. The index result itself never enters ambient context.

```text
task + live scope
  → ContextRequest
  → EffectiveContract
  → ledger/registry retrieval (index-assisted, authority-checked)
  → ContextPack {included + reason, excluded + reason, source revisions}
  → exact injected bytes persisted
  → harness consumes
  → Receipt cites context_pack_id and resulting state delta
```

This makes “why did it know that?” answerable from a stable chain: injected text → ContextPack entry →
claim/position → evidence → source version/body. It also makes “why did it not know that?” answerable from
`excluded[]`, freshness, privacy, uncertainty, or capability/readiness evidence.

The semantic requirement comes before the harness event. Compound compiles work/session-scoped lifecycle
intentions against a revisioned harness capability attestation:

```text
TaskContract + EffectiveContract + risk/privacy + context needs
  → LifecyclePlan {prepare context, guard, observe, preserve, restore, close}
  → harness capability attestation
  → HookPlan {native binding | fallback | degraded | unsupported}
  → installation observation + readiness/drift evidence
```

`LifecyclePlan` is the semantic view of what the run needs; `HookPlan` is its harness-native projection. This
evolves the existing hook-plan owner instead of creating a peer hook system. Both are immutable and revision-bound
for the work/session they serve. Neither promotes memory, mutates policy, or executes the requested action.
Unknown harness capability is `unsupported` or `degraded`, never an optimistic event mapping. A formatter need,
for example, may lower to a post-tool event, a wrapper, or a commit-time check without changing its meaning.

Governed collaboration delivery is one LifecyclePlan intention family, not a second lifecycle planner. The
canonical collaboration replay stays in Compound custody. `internal/hookplan` evaluates each event against the
receiving actor's work, claimed scope, dependency, policy, base revision, and reserved-resource refs, then records
one of four dispositions: `silent_state`, `next_boundary`, `interrupt`, or `human_attention`. Relevant compatible
`next_boundary` and `interrupt` events coalesce into one bounded structured agent card containing only state deltas
and artifact refs. `human_attention` is operator-projection state only: a bounded, level-triggered, read-only fold
returns at most eight current unresolved `{event_ref,work_ref,delivery_class,delta,artifact_refs,semantic_revision}`
items and never compiles native model context. Per-recipient cursors and delivered semantic state make agent retries
idempotent; interrupt-class intentions bypass the next-boundary queue and remain a level-triggered execution deny
until newer governed semantic state clears them, without repeating the card. Codex and Claude-family adapters compile
the same card and interrupt-deny meaning into native presentation, while hooks merely pass recipient state and return
the compiled bytes. Native delivery and artifact retrieval require the exact owned session/work/run binding; an
unknown session never inherits a sole workspace candidate. Artifact bodies are fetched only by an explicit retrieval
call whose populated relevance dimensions are conjunctive. An acknowledgement is absent unless it would itself change
governed state. Clocks may drive heartbeat, debounce, TTL, and stale sweeping, but never decide semantic relevance or
delivery.

Compaction, resume, delegation, and harness replacement are normal lifecycle branches. The working object and
remaining envelope are persisted before a lossy boundary, restored from owned refs afterward, and recompiled for
the new harness. Child runs inherit ceilings, not stale HookPlans. A missing native event must not silently drop
the intention; the plan either names a proved fallback or exposes the uncovered branch.

Hooks remain thin clients:

- Session start and prompt hooks request/freeze context; they do not maintain a private memory cache.
- Pre-tool hooks turn intended mutations into `ActionIntent → PolicyDecision`.
- Post-tool hooks record normalized evidence and verification candidates.
- Stop/session-end hooks record outcome evidence on the same `work_id` where the harness supports them.
- Missing, stale, or fail-open hook capability is recorded as readiness evidence, never hidden by a generic
  “installed” flag.

## 6. Markdown, Obsidian, dashboard, and settings clients

Markdown and Obsidian remain first-class **interaction formats**, not alternate authorities. A projected note
carries stable object IDs, source/ledger revision, projection revision, and freshness metadata. Regeneration is
safe because the authoritative objects and owned source bodies live elsewhere.

An edit follows a governed round trip:

```text
projection edit
  → capture edited bytes as evidence
  → compare projected base revision with current authority
  → conflict, reject, park, or create typed writeback proposal
  → ActionIntent → PolicyDecision → owner write
  → Receipt + Transition
  → regenerate projection
```

Direct ledger mutation from a file watcher is forbidden. Concurrent authority drift creates a reviewable
conflict; it never uses last-write-wins. Attachments enter artifact custody. Deleting a projected note requests
retirement; it does not erase shared evidence or claims.

Settings use the same round trip. The settings screen edits a base revision, previews the effective scope and
affected clients, records a governed owner mutation, and shows the resulting projection revisions. A generated
harness file is therefore a client of the setting and LifecyclePlan owners, not a convenient second source of
truth. Hand edits become drift evidence or an import proposal; they never silently win.

The ordinary dashboard is a bounded projection over the same owners. It should answer, without exposing the
full kernel by default:

- **What changed?** state delta, responsible work, before/after, and undo/rollback availability.
- **Why did it know or do that?** context, evidence, policy, capability, execution, and receipt drilldown.
- **Where is it deployed?** current RuntimeBinding plus freshness and health.
- **What needs me?** held policy, contradiction, writeback, restore, or retirement gates.
- **Is my information safe?** custody coverage, source drift, backup age, last isolated restore, index health.

Buttons that mutate settings, memory, policies, projections, or deployments are clients of the same action and
policy path. A dashboard never grows a hidden write API.

## 7. Compound and execution-gateway boundary

A harness is not a model, a model is not a provider, native inference effort is not orchestration, and an alias
is not an observed identity. Compound stores those axes independently.

Compound owns the semantic request: task/job class, agent role, harness requirements, privacy/risk, semantic
effort, orchestration policy, budget ceilings, capability constraints, standing grants, and the WorkEnvelope.
The execution gateway owns live provider/model/account availability, transport, codec compatibility, retries,
circuits, fallback execution, provider catalog revision, and exact resolution. The handshake is:

```text
Compound ExecutionProfile + budget + policy
  → execution gateway revision-bound transport request
  → resolved provider/model/native effort/effective parameters + gateway request id
  → ExecutionRouteEvidence
  → Receipt/Transition joined to task outcome
```

Historical evidence stores both requested alias/profile revision and observed provider/model. Alias drift must
not rewrite history. An explicitly requested unsupported native effort fails or records an allowed translation;
it never silently changes effort. `ultra`, `xhigh`, and orchestration policies are not interchangeable.

The full execution identity has four stages that remain separate:

| Stage | Required evidence |
|---|---|
| requested | profile revision, role/class, client harness requirement, route/model constraint, semantic/native effort request, orchestration, privacy, permissions, and budget |
| admitted | PolicyDecision, standing-grant/budget reservation, accepted gateway catalog/adapter revision, and allowed translation/fallback bounds |
| observed | harness/adapter revision, gateway request id, transport, provider/model/account class, effective native parameters, attempts/fallbacks, usage, latency, and terminal route status |
| outcome | Receipt state delta, verification, cost reconciliation, and Transition; provider success alone is insufficient |

A child execution receives a new plan. It inherits only ceilings—privacy, targets, tools, mutation classes, and
remaining request/token/time/cost budget. Model, transport, account, native effort, and orchestration are selected
again and observed again. Two different harness selectors do not prove independent review if their observed
provider/model identity is the same.

The rollout is deliberately A then B. In A, the gateway owns and validates self-contained lane definitions and
provider/account reality. In B, Compound's `ExecutionProfile` becomes semantic authority and the gateway lowers
it into an executable lane. A specifically named interactive driver may choose the highest native effort;
that effort is never the global default, is not inherited by child agents, mining, or routine work, and every
child operation receives its own bounded profile and receipt requirement. “Route accepted” remains route
evidence, not task success.

Required hardening before B is complete: validate aliases against a revisioned gateway catalog, preserve the
full requested/effective execution tuple in Panel and receipts, make the highest native effort reachable only
through explicit profile semantics, accept valid cross-model harness combinations, and distinguish
no-account/circuit-open from no-capable-target failures. Those are open integration gates, not reasons to
duplicate the gateway inside Compound.

## 8. Backup, restore, multi-device, retention, and deletion

A backup is successful only when it restores behavior, not merely bytes.

The v0.1 local archive producer and verifier share one unchanged resource envelope. Before publication,
backup and public-fixture export enforce logical tar-entry counts (including directories and the archive
manifest), path bounds, per-entry and expanded bytes, the actual serialized manifest size, and the actual
decoded/compressed streams including tar metadata, padding, and gzip finalization. Restore retains the
same admitted limits through final staged-file verification. A budget refusal publishes no archive; it
does not authorize pruning source evidence or raising the reader's limits. These checks establish budget
parity, not a coherent live snapshot or support for larger estates.

The explicit `streamed-v2` backup profile stays in the same home owner. A bounded leading descriptor
selects code-owned limits; interleaved checksum chunks and a final index replace the single buffered
manifest. Bounded rooted traversal and chunk comparison replace whole-tree record buffering for that
profile. The global duplicate-path index remains finite and subject to resource acceptance; chunking
is not a claim of constant memory. Verification, pinned-source extraction, staged rehash and
no-replace publication remain separate checks. v0.1 readers and default writers retain their prior
limits. A retained compatible recovery binary is required to restore v0.2 during an application
rollback. Synthetic resource proof does not establish a coherent live snapshot: populated-home
verification, isolated restore and a real consumer must still pass before activation.

- A sealed backup includes manifest/catalog revision, config, sources, artifacts, ledger, registries, work,
  events, context, and reviews. Rebuildable projections/indexes may be omitted only when rebuild is proven.
- Private backups are encrypted; encryption keys/recipients are not stored in the same archive. At least one
  verified copy lives outside `COMPOUND_HOME` and outside the source disk.
- Restore targets an empty isolated home, verifies hashes/schema/refs, rebuilds projections/indexes, replays a
  real consumer, and runs a negative control with legacy sources unavailable.
- A new-machine flow restores identity and settings before enabling writers or hooks. Provider credentials are
  rebound through secret references, not copied into public or plaintext config.
- One authoritative home has one fenced writer at a time. Multi-device replication is log/object replication
  or sealed-backup transfer; live SQLite/JSONL folders are never merged by generic file sync. A writer handoff
  requires epoch/lease proof. Divergent offline writes re-enter as evidence/candidates, not file merges.
- Retention is policy by source/body/privacy class. Content-addressed bodies shared by live refs cannot be
  removed until every reference and backup-retention obligation is satisfied. Deletion emits policy, receipt,
  tombstone, and deletion-certificate evidence. Retirement never implies deletion.

Replication, encrypted backup policy, restore rehearsals, and reference-aware deletion are target contracts
until a vertical slice proves them. No legacy writer or store retires based on this document alone.

The explicit `streamed-v3` profile adds bounded, checksum-bound symbolic-link
records to the final index (at most 4,096 links, still at most 1 MiB). The original
target is retained as private evidence; in-home absolute targets are rebased
for an isolated restored home. Canonical relative, dangling and cyclic links
are preserved without following them. Unsupported spellings, escapes, path
collisions and non-directory parents fail closed. Links count against existing
member/path budgets; they are created only after regular extraction and checked
again before the unchanged durability/publication gate. Tar link headers remain
forbidden. v0.1 historically omits non-regular entries and cannot establish link
custody; v0.2 refuses them. v0.3 therefore requires its own compatible recovery
binary and populated-home acceptance, not an inference from a v0.1 round trip.

### Failure-case contract

“All information lives in Compound” is credible only if unhappy paths preserve a single owner:

| Case | Required behavior |
|---|---|
| source disappears after capture | owned body still restores; source ref remains provenance, not a runtime dependency |
| canonical body is absent, legacy bridge exists | bounded fallback is visible as degraded; the next governed write canonicalizes without deleting rollback evidence |
| canonical body is present but corrupt | fail closed; never hide corruption by falling back to a valid legacy copy |
| capture succeeds, adjudication fails | retain cited evidence; publish no claim, setting, registry entry, or ambient context |
| two writers update one semantic identity | serialize or compare-and-swap the complete owner transaction; preserve both accepted deltas or reject one explicitly |
| source path or host changes | stable source/entity identity survives; ambiguous identity parks rather than duplicating |
| projection edited from a stale revision | create a conflict/writeback review; never last-write-wins |
| index is missing, stale, or contradictory | use authoritative fallback where bounded or report degraded/unavailable; an index hit never bypasses eligibility checks |
| harness lacks a requested lifecycle seam | choose a proved fallback or report degraded/unsupported; never fabricate readiness |
| session compacts, resumes, delegates, or swaps harness | restore the working object and remaining ceilings from owned refs; recompile lifecycle and execution projections |
| native effort/model combination is unsupported | reject or record an explicitly allowed translation before provider contact |
| gateway falls back or resolves an alias differently | preserve requested and observed identities plus revision and reason; historical meaning does not change |
| model route succeeds but work fails | route evidence remains successful traffic evidence; Receipt/Transition records the failed or blocked outcome |
| dashboard cache is stale or an owner is unreachable | show stale/unknown and disable unsupported actions; never render a false green |
| backup races a writer | use a fenced/coherent snapshot or fail verification; a structurally valid mixed generation is not restorable proof |
| another device writes while offline | import divergent work as evidence/candidates after writer-epoch reconciliation; never merge live home files |
| legacy traffic reappears during the quiet window | invalidate the quiet-window gate and investigate the reader/writer; do not proceed to retirement |
| shared body is requested for deletion | retain while any live ref or retention obligation exists; retirement alone does not authorize deletion |

## 9. Custody and retirement state machine

```text
registered
  → capturing
  → owned_snapshot
  → verified
  → compound_primary_writer
  → legacy_frozen
  → quiet_window_proven
  → archived
  → retired
```

Every arrow needs evidence. The gates are:

1. inventory all information classes, bodies, identities, writers, readers, jobs, hooks, projections, and
   unknowns;
2. capture exact bodies and source-family metadata into Compound custody;
3. perform an isolated restore with the originals unavailable;
4. shadow semantic lookup and write behavior against frozen fixtures and real traffic;
5. replay each real consumer through Compound;
6. disable the old path in an isolated negative control and reject silent fallback/false green;
7. cut over governed writes, then freeze the old writer;
8. observe a measured quiet window for unexplained legacy reads/writes;
9. archive with a verified rollback procedure;
10. request an explicit operator decision for service shutdown, destructive retirement, or deletion.

High-risk gates do not accept assertion-shaped proof strings. Their proof value is `transition:<id>` for a
separate, completed evidence WorkEnvelope whose canonical surface exactly equals the source family. Its completed,
effective Receipt and linked Transition must share that work id, and the Receipt must carry the exact token
`source-gate:<path-escaped-source-family>:<gate>`. The lifecycle transition's work governs the state advance; the
evidence work proves the gate. Compound persists the evidence work, Receipt, and Transition ids plus canonical
`sha256:` hashes, and `manifest-doctor` re-resolves them so missing or changed evidence fails closed. Legacy
high-risk histories without bindings remain readable for recovery, but doctor fails and no later advance can build
on them until the source family is re-evidenced through the governed path.

`SourceManifest` v0.3 is the executable owner of this family-level state. Legacy v0.1/v0.2 manifests load as
`registered`; they do not inherit a later state from body presence or an old status string. The forward edges require
the exact typed gate set for that edge. Writer cutover additionally records the old writer roster, Compound writer
destination, rollback reference, writer fence, monotonically increasing writer epoch, and required quiet-window
duration. Quiet-window proof carries its measured interval and observed/unexplained read/write counts; any unexplained
traffic or a short interval fails closed.

Its physical authority ref is `compound://sources/manifests/<family>` and its canonical body is
`COMPOUND_HOME/sources/manifests/<family>.json`. A legacy body under `registries/source-manifests/` is readable only
through the observable compatibility fallback. Recording or advancing that family writes the canonical body and
canonicalizes the authority ref; it does not move or delete the legacy file. Retiring the fallback still requires a
canonical restore, real-consumer replay, legacy-unavailable negative control, retained rollback evidence, and the
normal operator gate.

The state machine also has explicit rollback edges (`capture_abandoned`, `custody_invalidated`,
`verification_invalidated`, `writer_rollback`, `legacy_unfreeze`, `quiet_window_invalidated`, and
`archive_rollback`). `retired` is terminal; reactivation starts a new governed lifecycle instead of rewriting
history. Reads and owned-body materialization remain available while migration is incomplete. A state transition
records what an evidenced cutover did; it does not itself stop a service, freeze a writer, delete data, or mutate a
remote system.

`compound source manifest-doctor <family> --json` is the live operator consumer: it reports current/next state,
required gates, read/write posture, rollback target, and next safe action. `manifest-advance` is dry-run by default and
requires a running leased WorkEnvelope, typed proof refs, and a matching allowing `PolicyDecision` before `--confirm`
can publish the next manifest revision. The built-in policy allows the reversible pre-cutover bookkeeping path but
holds writer cutover, freeze, quiet-window, archive, rollback-from-live-writer, and retirement transitions until an
exact reviewed authority exists. The manifest transition retains `work_id`, `ActionIntent`, and `PolicyDecision`
links so the eventual Receipt/Transition closes the same execution spine.

Brain-like Markdown, database retrieval stores, and control-plane repositories each need their own rows because
their bodies, writers, and consumers differ. A single “OS absorbed” status cannot retire all three. The end
state is subtraction: Compound owns durable bodies and meaning; Markdown is a client, indexes are rented,
the gateway executes, and legacy systems are evidence/archives or gone.
