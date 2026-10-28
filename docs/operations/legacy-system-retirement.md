# Retiring A Legacy Knowledge Or Control System

Retirement is a verified authority transfer, not a folder move and not a delete.
The legacy system stays available as evidence or rollback until Compound has a
receiving owner, a real consumer, preservation proof, and a negative control that
would fail if the consumer silently fell back to the old path.

Use this sequence for note trees, memory databases, retrieval indexes, control-plane
registries, generated dashboards, hook systems, packet stores, and similar sources.

## 1. Put each information class in its actual Compound owner

Do not copy an old root wholesale into `COMPOUND_HOME`. Classify each family first.

| Legacy material | Compound receiving place | What it means after transfer |
|---|---|---|
| authored markdown, policies, source records | immutable artifact body + versioned SourceManifest, then `EvidenceItem` | Compound physical custody plus citation/rollback evidence, not automatic ambient authority |
| durable claims and current positions | `ledger/{claims,positions,edges}.jsonl` through candidate adjudication | owned meaning and temporal authority |
| raw documents, transcripts, exports | a class-separated corpus lane or content-addressed artifact | searchable evidence, never automatic promotion |
| decisions | decision lifecycle ledger | accepted, superseded, parked, and retired decisions stay explicit |
| procedures, skills, tools, doctrines | CapabilityGraph nodes and edges | task-scoped offers, never automatic execution |
| events and run history | typed events / `TraceEnvelope` evidence | forensic history joined to work and transitions |
| projects, services, hosts, data sources | Compound registries and runtime bindings | current operational lookup authority |
| work packets and receipts | `WorkEnvelope` / Receipt / Transition | one proof chain under one `work_id` |
| vector, full-text, graph, or fact indexes | rented engine/index adapter | ranking and breadth only; no promotion, visibility, or truth authority |
| generated dashboards and manifests | projections with freshness evidence | rebuildable views, not canon |

The repo remains source code, schemas, docs, tests, and fixtures. Private runtime
state belongs under `COMPOUND_HOME`; private source paths and account-specific
mappings belong in the private overlay.

## 2. Track the transfer as a retirement matrix

An instance-specific retirement matrix belongs in the private overlay, not public
core. Each row should name:

```text
id
legacy source ref and source kind
old owner and old role
Compound receiving owner
real consumer
preservation/export manifest
retirement condition
proof command and proof refs
rollback path
status: pending | bridge | absorbed | evidence_only | archived | retired
```

A status string is not proof. A row becomes `absorbed` only when a machine check
can resolve the receiving owner and its consumer. A row becomes `retired` only
when the old path is no longer needed for normal operation.

## 3. Use the staged cutover

1. **Observe**: inventory source families and live consumers. Record unknowns.
2. **Capture**: create Compound-owned body versions and source-family manifests before
   narrowing any write or read path; inventory-only/source-in-place rows do not pass.
3. **Import as evidence**: use source adapters, corpus lanes, events, or candidates.
   Do not bulk-promote source bodies.
4. **Shadow**: run Compound and the legacy path over the same frozen fixtures or
   real traffic. Compare coverage, currentness, privacy filtering, and latency.
5. **Restore and replay consumers**: restore into an empty isolated home with originals unavailable,
   then prove each real client succeeds through Compound.
6. **Negative control**: make the legacy read unavailable in an isolated test and
   prove the client does not silently fall back, return stale data, or claim success.
7. **Cut over writes**: route new durable writes through Compound's governed doors.
   Freeze the legacy writer only after capture, isolated restore, and rollback proof are green.
8. **Archive**: retain source/export evidence and restore instructions. Remove the
   old surface from hot operation without deleting history.
9. **Retire or delete**: deletion is a separate operator-gated act after a measured
   quiet window. Retirement does not imply deletion.

Record this progression on the source family's existing `SourceManifest`; do not create a second retirement matrix
or status registry. The operator loop is:

```sh
compound source manifest-doctor <family> --json
compound source manifest-advance <family> --to <next-state> \
  --work <running-work-id> --by <lease-holder> \
  --proof <required-gate>=<evidence-ref> ... --json
# inspect the dry-run and PolicyDecision; add --confirm only when both are correct
```

The SourceManifest body itself has one canonical home: `sources/manifests/`. A legacy
`registries/source-manifests/` body may be read only when canonical state is absent, and doctor reports that posture
as degraded. Any record or lifecycle mutation publishes canonical state; it does not delete the legacy body.
Retire this storage fallback only after the canonical materializer/doctor and a real source-family consumer replay
with the legacy path unavailable. This storage bridge proof does not by itself advance or retire the source system's
own lifecycle.

Served instruction rollback sources use owned item refs such as
`compound://sources/manifests/<family>/items/<item-id>`. Once those item bodies are verified,
the live harness entrypoints and fresh ContextPacks bind to the item refs; do not materialize a
second “absorbed root” merely to keep legacy filenames available.

`manifest-doctor` names the exact next gates and posture. Forward skips, proof-class substitution, stale lifecycle
revisions, incomplete writer handoff fields, short/dirty quiet windows, and non-operator retirement decisions are
rejected without changing the manifest. Writer cutover and every later operational state are policy-held by default.
The transition command records evidenced authority state only; the actual external freeze, cutover, archive, service
stop, or deletion remains a separately governed action with its own receipt.

For isolated restore, semantic parity, real-consumer replay, legacy-unavailable control, writer handoff/cutover,
rollback, freeze, quiet-window, and archive gates, `<evidence-ref>` must be `transition:<id>`. That Transition and its
completed/effective Receipt belong to a separate completed evidence work whose `canonical_surface` is the exact
source family; the Receipt carries `source-gate:<path-escaped-source-family>:<gate>`. The advance work authorizes the
lifecycle mutation, while the evidence work proves the prerequisite. `manifest-doctor` rehashes and revalidates the
bound work/Receipt/Transition on every run; missing, malformed, cross-work, cross-family, wrong-gate, or drifted
evidence blocks the lifecycle without hiding recovery reads.

## 4. Required proof bundle

A retirement row is not green until all applicable checks pass:

- **receiving owner**: the Compound ledger, registry, event store, work store, or
  corpus lane resolves the transferred object;
- **lossless preservation**: source/export manifest doctor reports physical custody (not only inventory),
  hashes are stable, and a restore drill works in an empty home with original sources unavailable;
- **semantic parity**: frozen queries or fixtures meet explicit recall/lookup
  predicates; broad benchmark scores alone are insufficient;
- **decision/evidence separation**: semantic decision authority covers canonical
  policy records; indexes, bootstrap instructions, and historical review notes
  prove physical custody through the source manifest instead of entering the decision candidate queue;
- **real consumer replay**: the production client consumes the Compound path on the
  same task/data shape it uses in practice;
- **negative control**: disabling the legacy path causes no hidden fallback and no
  false-green result;
- **write-path ownership**: new writes enter through Compound policy, candidate,
  decision, event, registry, or work doors;
- **indirect writers**: scheduler definitions are traced through referenced shim/script bodies, and a
  checksum restore comparison must show no post-capture writes before the quiet window starts;
- **root-unavailable decision coverage**: once the source path is absent, the prior eligible set is
  reconstructed only from frozen complete custody plus Compound-primary decision source records;
- **quiet window**: no unexplained reads or writes hit the old system for the agreed
  observation period;
- **rollback**: the export, source refs, adapter configuration, and one-line restore
  or re-enable procedure are verified;
- **operator gate**: destructive deletion, remote database mutation, service
  disablement, and irreversible archive changes remain explicit human decisions.

Useful generic commands include:

```sh
compound home doctor --json
compound home backup --target <archive>
compound home verify <archive>
compound memory doctor --json
compound memory engine-compare <frozen-query>
compound source manifest-doctor <family> --json
compound authority os-absorption-doctor --json   # private dogfood matrix when installed
compound proof-check --work <work_id> --actor <lease-holder> --out <absolute-report.json>
```

For a database export with native-dump support, physical custody is the bounded sequence below. The export's `dump_ref` must
match the supplied dump exactly; do not attach a merely newer or nearby backup generation.

```sh
compound source manifest-create --family <database-export-family> \
  --from-export <export.json> --dump <matching.dump> --json
compound source manifest-doctor <database-export-family> --json
compound source manifest-materialize-dump <database-export-family> \
  --target <isolated-recovery.dump> --json
```

A green materialization closes only the byte-custody part of lossless preservation. Keep the database-service row at
`bridge` until an isolated database restore, production-consumer replay, negative control, write-path cutover,
quiet window, rollback rehearsal, and explicit operator retirement act are all evidenced.

The dogfood absorption doctor is an instance-specific consumer of this protocol,
not a requirement that every public Compound user recreate the same operating
system or source layout.

## 5. Stop conditions

Stop the cutover and keep the bridge when any of these is true:

- a live consumer is unknown or still reads the old authority directly;
- the export is missing, lossy, single-copy, or untested;
- Compound can search the material but cannot preserve its identity, temporal state,
  privacy class, provenance, or supersession chain;
- source manifests contain only paths/hashes or database inventory while declared bodies remain outside
  Compound custody;
- recall/lookup quality passes a synthetic benchmark but is not used by the real
  agent, hook, cron, or operator surface;
- disabling the old path changes outcomes or exposes a silent fallback;
- new jobs still write into legacy directories or databases;
- the proposed replacement creates a second owner, second lifecycle, or second
  ambient recall path;
- the final step requires deletion, remote mutation, or service disablement without
  operator approval.

The target is subtraction: one owned Compound path, with the legacy system reduced
to cited evidence, a rented read-only edge, a rollback archive, or nothing at all.
