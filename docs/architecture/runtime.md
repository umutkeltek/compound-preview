# Runtime: the `.compound` home, the envelope spine, and hooks-as-clients

This is how a *running* compound instance works. Three parts: the owned runtime home, the typed envelope
spine, and the thin hook clients. Build this **before** memory ingestion (`../roadmap/phases.md`). One doc on
purpose — fragmenting "generated-state", "envelopes", and "hooks" into three would reproduce the disease.

`compound contract import-hooks --codex-hooks-json <hooks.json>` or `--hook-registration-ts <hook-registration.ts>`
installs proven generated hook rosters as `compound://config/installed-hooks`; after import, `contract compile`
and runtime ContextPacks read the Compound-owned manifest while generated sources remain evidence/rollback input.

For `SessionStart`, the same hook-install transaction owns predecessor retirement: it removes only exact
Claude/Codex `RecallContext.hook.ts` command shapes plus older Compound bindings, preserves unrelated hooks,
installs one current Compound binding, verifies the written bytes, and restores native configs and authority
preimages on any commit, verification, or publication failure. The disconnected legacy handler files may remain
rollback evidence; they are not an active hook owner.

`compound contract compile --instructions` is also the sole installed static-instruction compiler. Its
`--materialize-instructions` mode atomically replaces only the unique Compound-managed region and preserves
unmanaged bytes. Nested takeover reuses the preserved final newline rather than adding a blank line.
`--bootstrap-replace` is an explicit one-time takeover of recognized legacy-generated `CLAUDE.md` /
`AGENTS.md` files or nested product-steering blocks. Preset renders, symlinks, malformed markers, and raced
preimages are refused. The compiled `InstructionBlockRender` plus the installed region is the check surface;
no parallel installed-instruction registry or generator exists.

`install.sh` publishes the verified `compound` binary and optional breadth
engine. The Compound-owned heartbeat plus bounded decision letter are private
autonomy-lab opt-ins (`COMPOUND_INSTALL_AUTONOMY_LAB=1`); the compatibility
`os` alias is a separate migration opt-in (`COMPOUND_INSTALL_OS_ALIAS=1`), not
part of a default public install. When opted in, the installer replaces only a
recognized legacy OS shim or existing Compound alias and refuses a foreign
file/symlink. Basename-aware dispatch retains the migrated muscle-memory verbs;
the alias never reads an OS dashboard or embeds an operating-system path.

When the autonomy lab is opted in, `attention-letter.py` is the interruption budget for the private
operator loop: it renders at most three already-ranked `needs_you` groups per
cycle. Omitted groups remain ledger-visible and are not re-presented in that
cycle; authoritative counts and review state stay in `compound attention`.
This is decision compression, not hidden approval or deletion.

`compound policy import-operator-state <operator-state.json>` installs the global autonomy ceiling
`compound://registries/operator-state` under `COMPOUND_HOME`; policy evaluation cites that authority and uses
it as a ceiling, not a bypass for public, spend, destructive, or security holds.

`compound schedule list|show|doctor` projects declared plist settings separately from exact current-user GUI launchctl overrides and loaded state. Unknown/error responses remain unknown; loaded does not mean running or successful. `schedule repair plan|review|apply|rollback` delegates one existing calendar-only LaunchAgent to the authority owner. Plans seal original/candidate bytes (only ProgramArguments may differ, preserving executable/script/subcommand prefix), exact target/work, runtime preimage, and explicitly supplied executable/script proofs. Symlinks, Git-owned targets, additional bootstrap triggers and unknown runtime state refuse.

The OS adapter requires an imported, exact `system.launchd.repair_existing` external-side-effect grant with narrow blast radius, exact plist TargetPath, expiry and Destination `launchd:<gui-domain>/<label>:<work-id>:<authorization-digest>`. Review first returns a stable ActionIntent; existing `policy action-plan`/`policy action-approve` and `work act --intent` must consume an exact one-use operator approval before repeating review can seal it. Baseline allows, reviewer strings, broadened grants and later policy revisions cannot authorize effects. Each replace/enable/bootstrap phase holds the label lock, journals start before effect, rechecks policy/lease/work/source/runtime authority, probes bounded readback, then seals its native Receipt/Transition and resulting work generation. A started phase without an observed outcome receipts ambiguity and stops; it never blindly replays. Other labels remain independent. No kickstart occurs. Before publishing a completed terminal record, apply revalidates exact candidate bytes, enabled/loaded service identity and plan ownership; rollback revalidates exact original bytes, disabled/unloaded state and its expected restored ownership. Terminal drift refuses completion without rewriting successful phase receipts or repeating their effects.

Review intents explicitly declare `requires_exact_approval`. Policy applies this requirement after standing-grant, autonomy and live-context evaluation, so L3 target authority cannot record a review before authentic one-use approval consumption. Deny, action security floors and autonomy halts retain their stronger fences; the requirement does not change the action capability, target, destination or mutation class. The omitted/false field preserves existing serialized intent bindings. Versioned review requests issue a new action without rewriting legacy requests, actions or approvals; legitimate legacy reviews and phase proofs still require their original consumed approval evidence.

Explicit rollback needs a fresh rollback review following the owned phase receipt anchor. It may boot out only this plan's loaded path/argv, restore the exact disabled override and restore original bytes only over its own candidate. Foreign file/service/override edits refuse every rollback effect. Crash journals and original bytes remain under existing `reviews/launchd-repair`; successful apply and failed/partial results retain exact-plan rollback ownership and block a new same-label plan until verified terminal rollback/reconciliation. No new scheduler, WorkDelivery kind or memory authority is introduced.

`schedule repair plan --handoff-from OLD_PLAN` permits an explicit successor only after the predecessor has a complete successful native apply chain, authentic consumed review approval, and an authenticated completed Work close. Planning still requires separately authorized quiescence: exact predecessor candidate bytes/argv/mode at the same user/domain/label/path, disabled and unloaded, with current source proofs and unchanged foreign overrides. The successor plan seals the predecessor plan, terminal report, native phase chain, close proof and exact owner bytes into its authorization digest; a fresh exact successor grant and consumed review approval remain required. Historical executable/script upgrades require an explicit `source_reattestations` entry binding each changed predecessor path and its old/current resolved path and SHA256. Every changed path must have one canonical, exact current proof in the successor's supplied source set; missing, stale or duplicate proofs refuse. These attestations enter the successor authorization digest and existing exact review, while historical plans and receipts remain immutable. Current sources are rechecked before ownership CAS and every OS phase; post-review drift refuses. Unchanged handoffs omit the new field and preserve their serialized proof contract. Under the same label lock, an additional native `handoff` phase revalidates those proofs and atomically compares/replaces the ownership pointer before any plist or launchctl effect. Its journal uses apply index 3, preceding unchanged legacy indexes 0–2. The native receipt binds before/after ownership hashes. An interrupted owner CAS without a sealed outcome receipts ambiguity and stops; replay never repeats it or proceeds to OS repair. Fresh reviewed rollback restores original bytes and disabled/unloaded state before a final receipted `restore_owner` phase returns the exact predecessor pointer. A restored pointer remains ineligible until its started successor's native rollback completes; a missing handoff-start file cannot hide remaining apply/rollback journals. Planned successors with no phase evidence do not reserve ownership. Immutable predecessor and successor history remains intact; active, failed, partial or ambiguous predecessors, unreviewed source changes and runtime/ownership drift cannot be handed off.

When the failed handoff's native outcome proves exact predecessor ownership, file and runtime remained unchanged, and no later forward phase exists, fresh reviewed rollback can receipt that unchanged ownership without a CAS. Only this read-only observation may repeat after interruption, under fresh lease/work/policy/review/source/readback checks. The actual forward failure and its native receipt remain failed and immutable; a changed owner, tampered proof or later phase refuses no-effect classification.

An expired or failed no-effect rollback observation can receive a fresh exact continuation through the same review and reservation owners. Its new request binds the complete authenticated failed prefix, next rollback index, actual current Work snapshot, current grant/configuration/expiry, and a newly consumed one-use approval; legacy request paths and bytes remain unchanged. The next phase calls observation-only code directly and cannot invoke an ownership CAS or OS effect. Failed prefix journals and receipts remain failed and immutable. Later successor eligibility authenticates that exact prefix plus its fresh successful unchanged-owner tail; missing/tampered phases, unknown journal suffixes, OS phases, stale approval or drift refuse continuation.

---

## 1. `COMPOUND_HOME` / `.compound` — the runtime body

The repo is **product source**. The running instance owns **state and durable information bodies**, under one
controlled home — never the public repo tree. The custody contract, edit/writeback flow, and restore gates are
owned by [`information-custody.md`](./information-custody.md).

```text
repo            = source code, schemas, docs, tests, fixtures
COMPOUND_HOME   = owned private runtime state + durable bodies (a typed habitat, not a folder pile)
private overlay = ingress bindings, credentials/secret refs, and operator-specific adapter configuration
```

Resolution order: `1) --home <path>  2) $COMPOUND_HOME  3) ~/.compound`. Repository-local homes are authority-bearing and are never selected by ancestor marker presence; use an explicit `--local`, `--home`, or `COMPOUND_HOME` selection when a command should operate on `<cwd>/.compound`. Commands: `compound home init [--global|--local]`, `home doctor`, `home where`.

```text
$COMPOUND_HOME/
  manifest.yaml
  config/       sources/       artifacts/     ledger/        registries/
  inbox/        work/          events/        context/       reviews/
  projections/  indexes/       archives/      tmp/
```

These are the accepted end-state roots, not a claim that every producer has migrated. HabitatCatalog now owns the
closed set: init, placement, doctor, backup, compaction, and restore consume it; unknown top-level entries fail
doctor/backup. Existing feature-shaped roots are explicit non-canonical bridges with destinations and retirement
conditions, and producer path constructors cut over incrementally before bytes move. Cataloged behavior-bearing
exceptions under `projections/` survive backup and compaction until their producers reach the correct typed home;
new behavior-bearing cursors may not be placed there. The detailed responsibilities are in the custody contract.

`SourceManifest` storage follows the habitat meaning: canonical manifests live under
`sources/manifests/`, because they are source-family custody and provenance records, not inside-world registry
authority. `registries/source-manifests/` is an explicit compatibility bridge only. New capture and lifecycle
writes publish to the canonical path. Reads consult the bridge only when the canonical file is absent; a present
but corrupt canonical file fails closed. `manifest-doctor` reports bridge consumption as `degraded` with
`storage_location=pending`, so compatibility cannot look like completed topology migration.

`ExecutionRouteEvidence` records the requested route separately from the observed identity: `selected_target` is the requested lane/alias, while `resolved_provider`, `resolved_model`, `resolution_source`, and (for gateway resolution) `gateway_request_id` identify what actually served the call. Gateway aliases use `resolution_source=gateway_route`; direct OpenAI-compatible lanes may use the response body's exact `model` with `resolution_source=response_body`. Measurements and autonomy evidence MUST attribute to the observed provider/model, never the alias or provider route string.

Typed semantics (the layout *is* the guardrail): `sources/` custody records (including
`sources/manifests/`) · `artifacts/` exact bodies ·
`ledger/` durable meaning · `inbox/` untrusted candidates · `registries/` current typed authority ·
`projections/` rebuildable views · `indexes/` rented · `events/` raw · `work/` execution lifecycle ·
`context/` what agents actually consumed. `compound home doctor` asserts the cataloged root set and reports present
bridge roots; per-family semantic restore and real-consumer replay remain migration gates beyond byte inventory.

`internal/eventstore` is the sole physical-layout owner beneath `COMPOUND_HOME/events`. New records append to
framed, self-validating JSONL segments (per-family, per-UTC-day, flock-serialized, fsync-ordered, tail-repairing)
with sealed-segment sidecar indexes that are rebuildable cache, never authority. Legacy loose records remain
readable during the migration era; `compound event-store migrate` folds them into segments under a manifest with
per-family count and digest verification, and a published completion gate retires the loose fallback from the hot
path (a loose record appearing after completion is a doctor-visible defect).

The delegated-session reducer used by `ExecutorsProjection` and `Coordination` resolves exact
attachment IDs with `ReadMany` from the existing Workbench lifecycle snapshot. Every original
attachment still passes identity and Work-binding validation in the same ordered reducer.
`ReadMany` shares family inventories, hot frames and sealed indexes only within one call,
preserving native point-read validation and index repair. Results match `Read` on a stable
store; concurrent reads are nontransactional, with no family-wide snapshot or current-at-return
guarantee. Later calls reacquire freshness through existing owner checks; no batch verdict is
persisted. Rooted `ReadRegular` callers keep their existing validation path.

A post-completion loose record is
resolved only through `compound event-store repair plan|review|supersede|apply`. Idempotent planning seals the complete
loose-byte and segment-authority snapshot plus a bounded two-snapshot observation of the retired-writer surfaces.
Review is a separate immutable operator seam: it binds the exact plan digest/revision, reviewer identity, explicit
decision, writer-stop attestation, and a second stable observation. This is positive bounded evidence and a refusal
fence, not a permanent proof that an external process cannot restart. The production observer records the interval
between snapshots; synthetic tests inject a zero-duration observer without sleeping. Apply revalidates that fence and derives
readiness from the current PolicyDecision for the concrete target before reserving the existing running-work
`ActionIntent → PolicyDecision` transaction. An allowing private-scope policy outcome never substitutes for the
exact review. The policy input describes manual recovery, bounded event-store blast radius, verified recovery
evidence, and continuity of operator control; it does not reduce the operation to a location or generic label.

Apply fails closed if the frozen migration baseline, planned loose bytes, loose set, reviewed writer fence,
current policy-authority revision, or relevant segment coordinates drift. Content-addressed archive bytes are
idempotent recovery state; an immutable digest-bound `.pending.json` snapshot plus a bounded, hash-chained,
append-only `.progress.jsonl` journal persists each record's append reservation, append, exact frame verification,
removal reservation, and removal phase. The repair root owns the snapshot and journal as distinct identities; it
binds the immutable journal header once, while every transition is fsynced before its dependent mutation boundary,
so per-step persistence is one O(1) append rather than a snapshot/root rewrite. Retry strictly replays the header
and linear transition chain. A missing or torn final append-only suffix is recoverable only when the owned
reservation, durable physical evidence, and current chain identify exactly one next phase or authorization
transition; recovery appends only the missing suffix and fsyncs partial progress before returning an interruption.
Ambiguous candidates, foreign or non-prefix bytes, an interior gap, or truncation of completed history fail closed.
For an absent id, the segment coordinate is durable before append; after append the repair rereads that exact
frame and verifies its id, canonical body, and hash before retiring the loose source. A retry finds the reserved
frame instead of appending a second one. Every original loose body—including an identical or divergent duplicate—
is copied byte-for-byte into content-addressed recovery storage before hot-state removal. Only ids absent from
segments are appended; existing segment authority is never overwritten or selected away.

The immutable original migration plan/manifest/completion stay frozen. A CAS-updated repair-root inventory owns
the identities and digests of every plan, review, zero-progress supersession, pending snapshot, progress journal,
result, and proof; `migrate --verify` rejects a
missing indexed artifact, a digest mismatch, or an orphan artifact, so deleting durable repair history cannot make
the count silently return to zero. Progress remains armed until the same-work ActionIntent, PolicyDecision,
Receipt, and Transition resolve and a digest-bound repair proof records that chain. If policy authority changes
after all physical bytes are complete, finalization uses the sealed original tuple and exposes the authority as
historical. While mutation remains, the old decision cannot authorize another byte: apply must resolve a current
allowing decision for the same exact action, atomically replace the unpublished work reservation, append an
authorization transition to the repair progress journal, and advance the repair root's authorization head. The
work reservation's convergence digest excludes only the replaceable policy decision and its single authority proof
ref; action, outcome, plan, review, and all other evidence remain exact. This lets a retry converge from a stale
predecessor without opening the action scope. On the supported CLI crash path, retry may restore the exact
unpublished reservation already installed for that action, append its missing authorization transition, and then
append a later current-authority transition only when the revisions differ. If exact reservation, history, or
identity cannot be proven—or time moves backward, action semantics change, policy is unreadable, or the current
decision disallows the mutation—the repair fails closed.
Before progress starts, an exactly reviewed plan may be superseded only by an immutable broader plan over the same
baseline that preserves every old entry. The first supersession write revalidates current authority immediately
before publishing its sealed history/successor tuple. If a crash leaves that exact terminal file durable before its
repair-root update, retry validates the persisted tuple and repairs or verifies root ownership under the migration
lock before replaying the reserved Receipt/Transition. It does not re-evaluate authority for bytes already made
immutable, and it cannot use that replay path to mint replacement bytes: a missing or different terminal file, a
foreign rooted identity, or sealed-tuple drift fails closed. A separate supersession proof then binds the rooted
terminal to the same work transaction.
The frozen census ends at the published completion instant: a manifest row recorded after it is runtime growth a
pre-refusal binary folded in, never legacy census. `migrate --verify` excludes such rows from the frozen plan
comparison, reports them separately, and accepts each only while its segment authority and archived original both
survive byte-exactly — a missing or divergent archive fails closed.
After completion, legacy `migrate --apply` refuses whenever loose records exist, so this governed path is the only
physical repair owner. Loose-path drift fails closed. The sole tolerated coexistence is a byte-identical same-ID
regular-file recurrence proven against the sealed archive and result. Old result replay and proof finalization do
not consume or hide that recurrence: it remains doctor-visible and advances only after the old proof into the
deterministic next occurrence. Exact terminal replay is a byte-no-op for the old lifecycle artifacts.
`compound event show <id> --json` also resolves exact approval plan and grant IDs
(`approval_plan_` and `approval_grant_`) through that same event-store read seam.
It returns the stored bytes without consuming, renewing or changing approval authority;
callers still verify the action binding and live one-use consumption through policy.
Other unknown approval prefixes remain errors.

Durable event references
use `compound://events/<id>`; the resolver also accepts bare IDs and historical `events/<id>.json` references,
but rejects traversal and physical segment/index paths. The lifecycle and compatibility streams remain distinct:
`events/work.jsonl`, `events/hook-events.ndjson`, and `events/traces.ndjson` retain their existing contracts while
their physical access stays inside the event-store seam.

Point reads resolve sealed segments through their sidecar indexes and the single hot (last-written) segment by a
full parse. Because a bulk migration can fold an entire history into one hot segment per family, that hot parse is
memoized per process behind a stat guard, keyed by the family segment DIRECTORY — exactly one live entry per family
(measured live 2026-07-16: 158-212s attention collapsed to single-digit seconds). Repeated Gets against the same
hot segment reuse one parsed frame set, and any size-or-mtime change (a same- or foreign-process append, torn-tail
completion) discards the entry and reparses, so a reader never serves state older than the file it just statted.
Keying by family directory rather than path means a day or seq rollover REPLACES the family's entry with the new
hot segment instead of stranding the prior day's parsed frames, so the memo stays bounded by the live-family count
in a long-lived process instead of accumulating one dead ~day-of-frames entry per family per rollover. Read
semantics are byte-identical to the full parse (newest-occurrence-wins, offsets preserved), and rooted
`ReadRegular` reads bypass the memo entirely so their per-read validation always runs.

Sealed-segment sidecar indexes are memoized the same way, keyed by the index-file path and stat-guarded on both the
index file and its sealed segment. Without it, a projection or close path (ConsumeReceipts reads every receipt)
re-opened and re-JSON-parsed a multi-MB index on every sealed Get after the day seal (measured live 2026-07-16:
attention steady-state CPU ~8s → ~4-5s once each sealed index parses once per process). A rebuild or an
`rm -rf index/` recovery moves the index-file stat and invalidates the memo; a segment that drifts underneath moves
the segment stat and forces the unchanged validate/rebuild path — the index stays cache, never authority.

---

## 2. The envelope spine (the typed grammar everything runs through)

Current evidence-autonomy status:

- `job_health` is the sole implemented class, not a live production
  consumer. No public path binds a running Director WorkID to a job-health
  Panel target, runs an attested non-fixture gateway, records a trusted judge
  row, and closes that same envelope with a genuine delta.
- Current Panel and arbitrary OpenAI-compatible calls record
  `recorded_via=generic`; public imports record `external_import`.
  `trusted_panel_runtime` is reserved for a future attested adapter and has
  no production caller. Missing legacy provenance is generic for resume,
  never trusted.
- New terminal events seal canonical Work and Receipt hashes. A deciding work
  has exactly one canonical Director trigger, and eligibility
  `generated_at` must equal the admission instant.
- Current `operator:*`/`human:*` checks prove lexical provenance, not
  authenticated identity. Until authenticated operator authority and an
  attested same-WorkID Panel feeder exist, production eligibility has no
  legitimate sample and remains fail closed.

Task meaning is an additive value on the existing spine, not a tenth envelope
or a second task identity. `EffectiveContract` and `ContextPack` may carry one
provider-neutral `TaskContract` (intent, mode, desired outcome, success
predicate, non-goals, proof medium); legacy v0.1 objects without it remain
valid. Dispatch preserves the frozen value and its originating `contract_id`
while CapabilityGraph results remain offers. `panel plan --work-id work_...`
binds a nonterminal parent before any model call, freezes that task contract
into the Panel run, and fixture-only lens/judge evidence records the same
`work_id`. A non-fixture caller and bound apply both fail closed until Work owns
a delegated operation lease spanning provider spend and target mutation; Panel
therefore cannot claim, advance, mutate, or close the parent in this slice. The
parent owner remains the sole terminal Receipt/Transition authority. This path
is still `recorded_via=generic`, not the attested
`trusted_panel_runtime` required for autonomous production eligibility.

Hung off one root `work_id`. The header matters more than any body — it prevents *retrieved ≠ relevant ≠
authoritative ≠ current*.

Current source-delivery close rule (superseding the earlier v0.4 migration wording above): only a revalidated `landed` `compound.work_land_check.v0.5` report is current close evidence. It binds the candidate tip to the latest attached, owned, content-addressed `proof-check:pfrs_*` source snapshot when one exists. The normal proof observes the bound target checkout. A checkout-independent CAS landing is also certifiable when the bound repository identity remains valid, no worktree checks out the target ref, and the target plus required publication refs already contain the certified candidate; dirt in the active foreign feature checkout is then outside target cleanliness. A target ref checked out in any worktree retains the ordinary checkout gate. Historical v0.3 and v0.4 reports remain readable stale evidence and require a fresh v0.5 check; v0.3 predates repository identity and v0.4 predates sealed proof source identity.
The native private-source publisher can finish a sealed publication whose factory-generated
action was already linked by `RecordAction` to a different allowing PolicyDecision. Its
unexported typed completion path authenticates the unchanged running Work against the
preceding native Work hash, the original consumed exact approval at the saved authorization
time, the immutable confirmed source observation, and the exact sealed outcome. It excludes
only that authenticated pre-admission event from completion-conflict checks; it preserves
history and leaves generic receipted-action callers strict. Sealed completion may finish
after approval expiry without another publication. First-effect authorization must still
be live, and unexplained Work, policy, observation, or later lifecycle drift refuses
reconciliation. Ordinary completed publications retain generic historical replay semantics.

Non-source delivery is an exact per-action classification, not a work-wide Git exemption.
An operator-provenanced same-state `work advance` may bind `--delivery-action`,
`--delivery-kind private_runtime` and an immutable `--delivery-evidence` reference.
`advance-plan` emits that tuple and exact expected state/policy/action-generation guards.
The evidence must bind canonical repository identity, the original action bytes, finite
ignored private-overlay files, custodied before/after bodies and an actual readback
Receipt/Transition. Tracked files, symlinks, arbitrary ignored source, broad repository
targets and uncovered mutations retain the Git-delivery gate. Binding is immutable;
custody, live hashes and the consumer chain are revalidated before terminal publication.
An interrupted binding may recover only its exact request. Once terminal authority
exists, frozen close cleanup may finish without rejecting legitimate later edits.
Private-delivery manifests/reports are bounded to 1 MiB, individual bodies to 64 MiB;
the artifact owner enforces limits before allocation and while reading. Classification
never replaces canonical proof, genuine failed-attempt recovery, or task acceptance.

`work delivery-capture <work_id> --request <file> --by <actor> --json` produces
private-runtime evidence from actual bounded file reads. The strict 1 MiB request
names the original action, exact repository commit and running work generation,
and a finite file inventory with expected SHA-256 values.
Each file declares prior absence or supplies a private rollback preimage path and
hash. Absence is a historical declaration, not an observed past fact. All bodies
must match before custody begins; capture admits at most 128 MiB of body reads.
The existing receipted-action owner authorizes and records the readback; callers
cannot supply a successful consumer report. Success is `captured_not_bound`:
use its delivery tuple in `advance-plan`, then the exact generated command.
`capture_incomplete` can include persisted custody, receipt and transition
references; retry the unchanged request using the absolute-path `retry_command`.
It neither repairs a failed proof nor accepts the parent work. Reserved receipt
recovery reuses the durable completion time and validates the original transaction,
including crashes between receipt publication, completion marking and cleanup.

The same door accepts `compound.installed_package_delivery_capture.v0.1` for an
already installed Homebrew cask copied into its owner's ignored prefix. The request
names `capture_id`, the original `action_intent_id`, `manager: homebrew-cask`,
`prefix`, `package`, `version`, and the exact running state/policy/action generation.
It never invokes an installer. The owner reads the cask's installed receipt, requires
its recorded recursive copy to own the exact action target, rejects tracked files,
nested Git repositories and escaping symlinks, and hashes the bounded installed tree.
The receipt and readback report enter private artifact custody under an authenticated
read-only Receipt/Transition. Operator-provenanced same-state `installed_package`
binding covers only that action; capture alone does not bind or accept work.
Close rechecks the receipt, version, owner identity and installed bytes. An uncovered
source edit or commit still requires Git delivery. Unsupported package layouts fail
closed; arbitrary ignored directories, caller reports and executable samples do not
establish package ownership. Installed package bytes remain package-manager-owned;
Compound custodies their receipt and content fingerprint, not a second SDK copy.

The same capture command accepts `compound.worktree_retirement_delivery_capture.v0.1`
with `capture_id`, the exact running state/policy/action generation, and the
original `plan_path`, `review_report_path`, and `result_path`. Authority remains
the sole planner/reviewer/retirement producer; its wire types and pure validators
are shared with Work through `authority/gitrepository`. Capture reads the exact
reviewed YAML, authenticates the original ActionIntent/PolicyDecision and completed
Receipt/Transition/lifecycle event, and preserves the original result bytes.
It does not repeat deletion, infer success from self-consistent digests, or compare
historical authority tips to today's main. New plans use
`compound.worktree_retirement_plan.v0.2`. Historical v0.1 plans retain their exact
wire identity and remain readable as completed delivery evidence; new review and
new/resumed APPLY require fresh v0.2 planning and operator review. The runtime
never rewrites historical plans into current authority.

Native retirement PLAN and new/resumed APPLY require a Work-owned read-only
recovery-inventory preflight, injected into Authority by the CLI. It reads and
hash-verifies at most 256 recovery files, each at most 64 MiB, charging every
proof toward 128 MiB of recovery reads even when distinct paths share a digest.
Capture uses the same bounded reader. Preflight does not custody bytes, reserve
capacity, or guarantee later capture: file drift can still refuse capture, and
the four metadata bodies retain their separate 1 MiB limits outside that recovery
aggregate. Settled completed-result replay reports historical Git truth before
new capacity admission; it neither repeats effects nor claims successful custody.
Supported `sha256-file` recovery bytes enter private custody within those limits;
missing, changed or unsupported recovery evidence refuses capture. Optional `action_intent_id` selects an earlier
failed retirement action instead of the completed action. Its authentic failed
receipt must precede the separately validated completion witness in the owned
lifecycle and bind the same sealed plan/review. Each action needs its own operator
binding; no failure is rewritten, and this classification does not claim
cryptographic ancestry of overwritten result files. Finalization retries minting
a new retirement action refresh its PolicyDecision even when Git effects have
already settled, without repeating those effects.
The typed `worktree_retirement` binding covers
only that original action. Close revalidates all custodied evidence, including
recovery bodies; explicit Git contracts, commit artifacts and uncovered mutations
still require their normal source-delivery proof.

Canonical source proof executes every gate in a detached temporary checkout of
the sealed HEAD. Only the ceremony-owned ignored inputs are copied as snapshots;
the original worktree remains the canonical locator and revalidation target, not
the subprocess execution surface. Copied regular files receive fresh access
timestamps without changing their modification times, bytes, or modes. This
prevents temporary-directory age cleanup from treating newly cloned APFS inputs
as stale. Refresh is cancellable, does not follow symlinks, and fails closed on
filesystem errors; source inputs and cleanup policy remain unchanged.

External Go/Xcode required tests use the source-sealed
[native consumer proof contract](contracts/native-consumer-proof.md), including
native evidence inside the owned report and exact finalizer validation.

### Universal header (every envelope begins with this)
```json
{ "schema_version":"compound.<kind>.v0.1", "id":"kind_...", "created_at":"...",
  "producer":{"kind":"hook|cli|daemon|adapter|manual|harness","name":"compound","version":"..."},
  "ids":{"work_id":null,"run_id":null,"session_id":null,"transition_id":null,"context_pack_id":null,"contract_id":null},
  "scope":{"entity_id":null,"project_id":null,"product_id":null,"surface":null},
  "authority":{"source_class":"source|generated|evidence|review|runtime|private_overlay","owner":null,"source_revision":null,"status":"current|stale|disputed|superseded|unknown"},
  "privacy":"public|private|sensitive|secret", "risk":"tier0|tier1|tier2|tier3",
  "evidence_refs":[], "uncertainty":{"level":"known|inferred|suspected|unknown","reason":null} }
```

| Envelope | Role | Key body fields |
|---|---|---|
| **EffectiveContract** | the compiled "what applies right now" artifact every run references by `contract_version = sha256(...)` | active_instructions[], active_hooks[], hook_roster[], hook_readiness[], active_decisions[], memory_policy{ambient/explicit_only/forbidden}, capability_policy{tools_allowed/on_demand/mcps_not_loaded_by_default}, mutation_policy, source_revisions[], unknowns[], conflicts[] |
| **ContextRequest** | what a hook asks compound for | intent_text_ref (hash/redacted), harness, cwd, task_guess, requested_mode (observe\|plan\|execute\|review), constraints{max_chars, allow_private_memory:false, allow_raw_corpus:false, allow_provisional_reviews:false} |
| **ContextPack** | what an agent consumes (frozen, replaceable) | summary_md, included[]{ref,source_class,reason,confidence,status}; surfaced capabilities and matched InstructionRules appear as `capability://...` generated refs when present, filtered by harness compatibility edges when present, carrying read/route/boundary constraints, **not execution**; runtime memory appears as relevance-gated `claim://...` refs in active_memory[] only after ledger visibility/scope policy, **not from a rented index**; volatile `repo:*` source-control snapshot claims stay queryable memory, not ambient context, because live adapters/policy gates provide fresh source-control evidence; recorded `ProjectionFreshness` context appears as `projection://...` generated refs filtered by repo kind, with stale/drifted status preserved as context evidence, **not authority or regeneration**; close records capability offers in Receipt `skills.candidate` and Transition `projections.skill` as provenance, not use; an optional `retrieval{level,fallback,authority,requested_engine,observed_engine,error_class}` records the bounded breadth-dependency posture bound into the sealed pack, never provider/query/body data; **excluded[]{ref,reason}**, active_instructions[], active_hooks[], hook_roster[], hook_readiness[], active_decisions[], active_memory[], source_revisions[], required_checks[], avoid[] |
| **WorkEnvelope** | the one root identity for a unit of work | work_id, contract_id, owner, canonical_surface, state (proposed→admitted→ready→claimed→running→verifying→completed\|held\|blocked\|failed\|cancelled), desired_outcome, success_predicate, non_goals[], risk, context_pack_id, route_decision_id, policy_decision_id, run_ids[], run_attempt_ids[], artifact_ids[], verification_ids[], memory_candidate_ids[], optional target_surface, optional proof_medium, optional prediction{outcome,move_type,note}, optional git_delivery{target_worktree_root,repository_fingerprint,target_local_ref,expected_base_sha,publication_mode,publication_ref}. **`run_ids[]` is a legacy wire name and does not hold run identity: it holds ActionIntent ids (`act_*`), and the Go field is `ActionIntentIDs`.** The key is deliberately frozen because the canonical work hash is `sha256(json.Marshal(WorkEnvelope))` and that hash is already persisted in lifecycle events, leases, and terminal proof evidence — renaming the key changes the marshaled bytes and invalidates every stored `work_sha256` at once (measured against real state: 120/121 envelopes match their stored event hash under `run_ids`, 0/121 under a renamed key). It becomes `action_intent_ids` at the **`compound.work.v0.2`** schema bump, where new envelopes carry the new shape and v0.1 envelopes keep theirs, so hash continuity is preserved per-version instead of rewritten. `run_attempt_ids[]` was reserved and hash-neutral at v0.1; S1 (`compound.runattempt.v0.1`) populates it for panel apply, holding `ratt_<UTC>_<8 hex>` ids minted by `internal/runattempt.Mint`. The validator (`envelope.validateRunAttemptIDs`) enforces the `ratt_` prefix at the envelope boundary so the slot cannot re-accumulate the heterogeneous run-shaped evidence (`prun_*`, `run_*`, free strings) that the v0.1 ActionIntentIDs mix carried. `ActionIntentIDs` is now `act_*`-only on every path that flows through `Store.Advance` (the only production advance writer post-S1). `compound work create-plan` records proposal-only admission policy readiness under `COMPOUND_HOME/create-plans` and its generated create command carries the planned `--work-id` plus planned envelope fields; `compound work create [--work-id id]` enforces that policy path before writing work/action/policy/event state, preserving the planned root id when supplied. Markdown work packets enter here through `work import-packet`, keyed by packet hash as proposed source-backed work through the same admission gate, not as proof. `compound workbench [--actor A] [--active-packet-surface S] [--source-packet packet.md] [--packet-dir D]` is the read-only board over open work, held/blocked review, terminal receipt/transition proof, optional source-fresh `active_packet_proof`, offered packet files under `offered_packets`, observed run evidence, recent dispatch-step-run summaries, and `dispatch_next` command suggestion/wait reason for that actor; it never dispatches or mutates. `compound workbench --operator` is an additive bounded human operator view over that same read model and review queue: it renders highest-priority Needs you / Working / Ready missions from existing WorkEnvelope/proof/review/action-context fields, groups duplicate rows, caps each visible stage with `--operator-limit` (default 3; terminal proof remains bounded by `--recent`), hides raw IDs/proof dumps unless `--advanced` or `--operator-advanced` is supplied, preserves the default human output, and leaves `--json` output unchanged even when paired with `--operator`. `compound work active-packet-proof [--surface S] [--source-packet packet.md]` is the read-only active packet proof projection: it reports `ok` only when one canonical-surface WorkEnvelope has context pack, receipt, transition proof, and, when source is supplied, matching packet evidence ref; served readers should call it instead of scanning `COMPOUND_HOME`. `compound work claim-plan` records proposal-only claim policy readiness under `COMPOUND_HOME/claim-plans`; `compound work claim` enforces that policy path and records the claim action/policy envelopes before writing the lease. `compound work advance-plan` records proposal-only lifecycle transition readiness under `COMPOUND_HOME/advance-plans`; `compound work advance` enforces that policy path before changing state or attaching run/artifact/verification refs. `compound work close-plan` records proposal-only terminal close readiness under `COMPOUND_HOME/close-plans`; `compound work close` enforces that policy path before emitting close action/policy, Receipt, Transition, and terminal work link. `compound work dispatch-plan` records a proposal-only next-claim plan under `COMPOUND_HOME/dispatch-plans`; `--source-packet <packet.md>` is a read-only packet proof preview and writes no WorkEnvelope state. `compound work dispatch-claim` records the dispatch plan/result and claims at most one policy-allowed ready work item through the same claim path; source-packet flags on this mutating path import packet-hash WorkEnvelope state first, then claim through policy. It does not execute, advance, close, or promote memory. |
| **ProofChainDoctorReport** | the v0.3 terminal-chain audit schema | `compound.work_proof_chain_doctor.v0.3` adds always-serialized RunAttempt outcome and exact terminalization-intent integrity to v0.2. Every terminal work is checked against its declared `run_attempt_ids`; each `ratt_*` must resolve, carry an accepted RunAttempt schema and matching embedded identity, belong to the same work/action chain, and reach an outcome-consistent terminal status. Only completed work may retain a contradictory historical status: a retry ancestor must use an exact canonical predecessor id, occur earlier in both attachment and creation order, and lead to a later `succeeded` harness attempt with committed-launch or post-mint heartbeat evidence; an already-terminal claim reservation likewise requires a later actually executed harness. Failed/cancelled work, unrelated leaves, origin-only attempts, backward/padded edges, and cyclic lineage still fail closed. Independently, a `succeeded` proof-resource RunAttempt backed by its complete committed canonical verification chain proves the proof subprocess rather than the later Work outcome; harness names or caller-authored refs alone never qualify. When a current record carries `terminalization_intent`, the doctor verifies its owner, target, authority generation, pre-close work digest, close input, receipt, transition, and closing-event digest as one exact proof. Accepted v0.1 records and empty legacy `action_intent_id` remain readable compatibility evidence. The additive counters include `missing_run_attempt`, `run_attempt_unreadable`, `run_attempt_invalid`, `run_attempt_work_mismatch`, `run_attempt_action_mismatch`, `run_attempt_nonterminal`, `run_attempt_outcome_mismatch`, and `run_attempt_terminalization_mismatch`; items carry `run_attempt_ids[]` and the report retains `work-proof-chain-run-attempt`. Non-`ratt_` slot entries remain `run_attempt_invalid`, never a zero-ref pass. |
Dispatch execution note: `compound work dispatch-execute <work_id>` is the explicit bounded consumer of capability resolution. Preview selects the first eligible adapter-backed route without mutation. Confirmed mode validates workspace and hook roster and runs the selected adapter's preflight before claim when it exposes one, then uses the governed execution rail to reach `verifying` or `failed` while preserving selected lane/model/provider intent and route authority on the terminal RunAttempt. The caller supplies no actor, harness, or model; the command is one-shot and introduces no scheduler.

Work origin note: new create paths populate optional
`origin{accountable_owner,actor,harness,requested_model,lane_id,identity_source}`.
The accountable owner is a qualified stable reference derived without an
operator prompt when the caller supplied a legacy free-form owner. Harness/model
describe the creator of the work intent; observed model/provider execution stays
in RunAttempt and ExecutionRouteEvidence. The field is omitted on legacy
envelopes, preserving their bytes and hashes.
Git delivery note: source-controlled work created or admitted through current commands must declare the complete envelope-owned delivery contract: canonical target worktree root, derived repository fingerprint, exact `refs/heads/*` target, full expected-base SHA, and either `publication_mode=required` with one bound `refs/remotes/*` ref or explicit `local_only`. A nonterminal legacy envelope with no `git_delivery` may bind that complete contract exactly once through a same-state `work advance`; the actor must carry operator provenance, a matching legacy `expected_base_sha` is cleared, and rebinding or combining the bind with a lifecycle transition fails closed. A pre-fingerprint `compound.work.v0.1` envelope whose other delivery fields are complete remains readable and re-marshals to the same bytes/hash, but it is execution-ineligible: Workbench exposes repository-identity migration, while claim, dispatch, worker-prompt creation, and ordinary advance fail closed. Only an operator-provenance same-state advance declaring the exact five persisted delivery fields may derive and backfill the missing fingerprint; field drift, executor migration, terminal migration, and a second backfill are rejected. These are governed migrations through the existing advance journal, not a second repair system. `work land-check` treats caller repo/ref values only as assertions and evaluates the attached final `commit:<full-sha>` against the envelope authority: repository identity, base lineage, clean target, local containment, and required publication containment. Only a successful current `compound.work_land_check.v0.5` report is attached to that WorkEnvelope as `land-check:*` close evidence; v0.3 predates sealed repository identity and v0.4 predates sealed proof-source identity. Both remain historical/stale evidence and require a fresh v0.5 check rather than being mislabeled as tampered. The publication observation is the bound local remote-tracking ref after push/fetch, not independent network attestation. Any completed close carrying `commit:*` through artifacts, direct proof refs, or an action-plan receipt draft fails without `git_delivery`. For non-audit moves, an explicit absolute `target_surface` or a valid persisted mutating `ActionIntent.target.path` inside a Git worktree also requires the contract even when a caller omits `commit:*`; missing, invalid, or mismatched `act_*` evidence fails loudly. ContextPack scope and `canonical_surface` are orientation, not mutation evidence, so they do not manufacture a delivery hold by themselves. `workbench`, `close-plan`, dispatch-close, and `close` re-evaluate current delivery state; local-only landing remains `awaiting_publication` when publication is required, stale/dirty proof is not advertised as closeable, and terminal work retains its terminal action instead of becoming `ready_to_close` again.
Plan-class note: a WorkEnvelope may carry optional `plan_class` (`oneshot | plan_doc | phased`, set at create via `work create --plan-class`), declaring how much alignment the work owes before it may run; empty means `oneshot`, so every pre-field envelope stays valid and re-marshals byte-identically. Exactly one class gates today: `phased` work is refused entry into `running` at the generic advance seam — every path, including `--execute` and dispatch-claim auto-advance — unless a `design-sketch:<ref>` artifact is already attached (`work advance <id> --to <state> --artifact design-sketch:<ref>`, where `<ref>` is typically a finding id or a repo-relative path). The refusal is fail-closed and records an ActionIntent plus a deny PolicyDecision, leaving the envelope in place. Like `commit:`, the `design-sketch:` prefix is a scan-time convention parsed by its consumer, not a centrally validated ref format.
Rework-lineage note: a WorkEnvelope may carry optional `rework_of_work_id` (set at create via `work create --rework-of <work_id>`), recording the lineage edge "this work exists because an earlier landed work broke something" — the input a rework-rate fold per class/lane/`plan_class` is computed from later. It is create-time only and immutable thereafter (there is no attach-at-advance path to police), and empty means "not a rework", so every pre-field envelope stays valid and re-marshals byte-identically. Two checks, both fail-closed before anything persists: the envelope validator enforces the loose durable-work-id shape (single path element, `work_` prefix, outside the `work_live_` namespace) and refuses self-reference, while admission proves the target resolves in the store — no phantom links, mirroring `runattempt.SetPredecessor`. Validation is deliberately not transitive: a rework of a rework is ordinary, and no cycle walk is needed because a create-time-only edge to an already-existing target cannot form one. The field is distinct from `Transition.Cause.CausedByRefs`, which is an unrelated canon-evolution mechanism with its own consumer.
Repository identity binding note: `target_worktree_root` is a locator, never repository identity. On create or governed legacy binding, Compound derives `repository_fingerprint` from the Git object format, the lineage roots reachable from the bound expected-base commit, and the exact trimmed origin; any caller-supplied value is overwritten. The fingerprint is carried through action identity, Workbench and dispatch projections, worker prompts, land-check reports, and sealed recorded proof, then re-derived before every authoritative delivery check. Ordinary descendant commits remain valid, while a different lineage, changed origin, or same-path repository replacement fails closed and requires governed replacement rather than silent rebinding. Close has a deliberately narrower atomic guarantee: after preparing native locks for the exact target and required publication refs, Compound revalidates repository identity one final time before writing any terminal envelope. The Git transaction keeps those refs fixed through terminal event persistence, but cannot lock repository config, origin identity, or object roots; a repository-identity mutation after that final check is outside the atomic ref guarantee and is detected by the next authoritative validation rather than retroactively invalidating an already-persisted close.

Schema-evolution invariant: a field added to a hash-bound envelope under an unchanged schema must preserve the omitted legacy byte shape (normally `omitempty`) and ship with a frozen pre-field JSON/hash round-trip fixture. A field added to digest-covered sealed content requires a schema-version bump plus a frozen previous-version fixture; validation classifies the schema before applying the current digest codec. Historical proofs remain identifiable evidence, never current authority and never a false tamper accusation. Tests must pin both the previous payload and the current schema literal so a future field addition cannot silently redefine an already-issued version.

Work lifecycle transaction note: authoritative create, claim, advance, action-record, auto-admit, and close writers serialize in the fixed lock order `autonomy-admission -> work-event` and strictly validate the complete shared `events/work.jsonl` before changing any WorkEnvelope, lease, ActionIntent, PolicyDecision, Receipt, or Transition byte. A corrupt or incomplete event tail therefore fails closed before those side effects. Advance additionally stages the next envelope at `work/work/<work_id>.json.advance-pending` before publishing its hash-bound event. The marker is a crash-recovery journal, not disposable scratch: backup retains it, compaction and operational-history archiving do not remove it, and a retry of the exact `work advance` or `AdmitAuto` transition validates the old/new work hashes plus the attached action and policy before finalizing without a second event. A marker with no matching durable event is safely discarded by that retry; an invalid, conflicting, or multiply matched marker remains in place and fails closed for operator inspection. `home doctor` does not auto-repair or delete this journal. The operator response is to preserve the marker and event/envelope evidence, retry the exact transition once, and investigate or restore through the home owner if validation still fails—never delete `.advance-pending` by hand.
Runtime materialization identity note: `compound contract compile`, `compound orient`, and context-producing hooks seal each exact EffectiveContract/ContextPack pair before exposing or recording it. Their 16-hex `contract_*` and `ctx_*` ids are content-bound over the complete materialized bodies (including creation time, source revisions, capability offers, retrieval posture, and installed-entrypoint evidence); a dated artifact collision with different bytes fails closed, so a WorkEnvelope or RunAttempt ref continues resolving to the bytes it actually received. `contracts/effective/latest.json` alone is the mutable current projection and is never an immutable execution-proof ref. Installed instruction packs bind explicitly through an immutable ContextPack to `contracts/effective/scopes/<context-key>/latest.json`; the key covers harness, canonical workspace, explicit repository-kind override, topic, and full task contract. Automatic repository classification remains source authority and can drift that same selection. Ordinary rendering retains that selection; `harness instruction-pack --context-pack <id>` changes it. Unrelated orientations cannot drift the selected contract, while its own materializations still do. Legacy global and unresolved bindings refuse verification; the refresh ladder compiles and binds each recorded lane context after all hook installs.
EffectiveContract v0.2 carries the optional digest-covered `context_key`; v0.1 rejects that field and remains readable with its frozen byte/hash codec. Unbound contracts of either version are evidence, never instruction-pack binding authority. The shared materializer canonicalizes registered harness names and resolvable workspace aliases before compilation. An installed profile's canonical binding is independent of a delegated work's immutable ContextPack: both validate internally, and rebinding a profile does not rewrite work evidence. Legacy/unbound installations without lane defaults must compile an explicit intended cwd/task and bind the returned ContextPack with `harness instruction-pack --context-pack <id>`; refresh must never infer that choice.
ContextPack retrieval posture note: newly materialized task ContextPacks project the existing privacy-safe breadth `EngineDependencyTrace` before sealing. `level` is exactly `used|degraded|refused|not_attempted`: `not_attempted` means breadth was not attempted (including unconfigured/offline operation) and does not guess an outage; `degraded` accepts only the existing unavailable/unsupported/retrieval classes; and `refused` accepts only integrity classes such as embedder/protocol/generation/ownership mismatch. `authority` is always `compound_ledger`. Every non-`used` outcome names only `ledger_ranker` as `fallback`; `used` carries no error or fallback. `requested_engine` is a required bounded slug for attempted outcomes and may optionally preserve the selected engine when the outcome is `not_attempted`; `observed_engine` is required for `used`, may be absent when failure precedes capability observation, and is forbidden for `not_attempted`. The object has no raw error, query, task, prompt, body, path, endpoint, credential, request, response, or retry field. Omission remains valid for pre-field ContextPacks. The projection does not invoke another engine or retry, and hooks persist/consume it from the typed ContextPack without adding hook-facing prose or spending the 6,000-rune injection budget.

Successful breadth queries can carry degraded-lane diagnostics with usable hits. Memory binds the diagnostics to the query that produced them, including expansion queries; ContextPack materialization projects any degraded lane as `degraded/retrieval_error` while retaining ledger authority and a bounded observed engine identity. A later healthy query cannot inherit the diagnosis. Lane labels remain bounded internal identifiers; raw diagnostics never enter the sealed ContextPack.
Decision context selection note: accepted active decisions have no control-plane include-all path. After the product fence, materialization ranks decisions by normalized task-token overlap with meaning-bearing decision text (object, non-structural predicate, and scoped domain/product/project/topic tags), gives an exact canonical decision-id mention priority without scoring shared id fragments, breaks remaining ties by canonical `decision://` ref, and selects at most 12 positive-relevance refs. Every otherwise in-scope omission remains explicit in `excluded[]` as either `no task relevance` or `relevant but outside 12-ref task context budget`; only selected refs contribute decision source revisions. EffectiveContract and ContextPack consume the same frozen selection. `compound decisions show <decision://ref|decision_id|claim_id>` resolves exactly one active record and never falls back to the full accepted inventory.
Realtime collaboration note: the existing `work_id` is the multiplayer room/root identity. `compound work collaborate <work_id>` records a lease-holder-authored `compound.work_collaboration_event.v0.2` event with bounded actor/session/target metadata, optional same-work `ratt_*` and causal `wco_*` lineage, a compact structured state delta, relevance refs, one of `silent_state|next_boundary|interrupt|human_attention`, and a required `artifact:sha256:<digest>` body reference that resolves and hash-verifies in Compound custody; inline message, prompt, transcript, and artifact bodies are not part of the schema. Historical v0.1 observations remain readable but are silent on every fresh delivery cursor until an explicit migration/reclassification exists. The deterministic work-scoped idempotency key covers the complete semantics, so a retry finishes an interrupted lifecycle attachment without duplicating the event or governed ActionIntent. The attached work-log row projects through finite or following cursor reads as one `collaboration_event` frame.

`internal/hookplan` is the one lifecycle planner: hooks remain thin clients. It consumes the governed replay through a normalized recipient-context cursor carrying exact `work_id` and `run_attempt_id`, coalesces by subject/effective-state/delivery-class semantic revision rather than event ID, and compiles at most one reference-only card for the current boundary. A legitimate recipient scope, dependency, or run revision preserves already-applied semantics, clears stale pending state, and replays from zero so newly relevant evidence is neither lost nor accepted under the old context. Actor matching can narrow a route but never establishes relevance by itself. Interrupt admission requires one exact active nonterminal run attempt, its bound action, and a persisted hold/deny for that action; only the addressed `ratt_*` receives bytes. An interrupt remains pending at prompt/resume/compact boundaries and terminalizes only at PreToolUse, where every normally delivered or over-budget interrupt persists an execution hold before mutation. When that exact run terminalizes, its formerly active interrupt is stale projection state: the next delivery removes it instead of returning an eligibility error or holding a successor run. Human attention requires unresolved hold/deny evidence for a hard/irreversible, public, security, financial, destructive, or authority-changing decision plus `compound://registries/operator-state`; it is Attention/UI-only and never enters any agent recipient context. Its production read seam is a bounded, level-triggered, replay-zero fold of current unresolved state that ignores ordinary actor targeting, returns only event/work/class/delta/artifact refs plus semantic revision, and writes no cursor, report, native context, or acknowledgement. Irrelevant and silent events never enter context, interrupts bypass the next-boundary queue, and Codex/Claude native forms retain one semantic meaning digest. Native boundary sources are carried explicitly as prompt, resume, compact, or pre_tool_use evidence in the LifecyclePlan even when adapter presentation differs.

Delivery projections live under the HabitatCatalog-owned `context/coordination-delivery/` child. Initial injected-token hypotheses are `silent_state=0`, `next_boundary<=120/item` and `<=240/card`, `interrupt<=80`, and `human_attention=0`; exact native bytes are factual while estimated tokens are explicitly the `ceil(bytes/4)` hypothesis. Operator DTO bytes are projection bytes, not injected model tokens. One replay-zero, level-triggered operator fold exposes current governed state from all four delivery classes through `coordination [--home P] --json`; `attention --json` filters unresolved `human_attention` through the same fold without adding it to Attention counts, groups, or top identity. Both JSON verbs embed the projection beside their existing report, and Console renders only its references and delta on the existing CoordinationBoard and Attention page. The fold is mechanically capped at eight items regardless of caller and each item has exactly `{event_ref,work_ref,delivery_class,delta,artifact_refs,semantic_revision}`. Overflow leaves event and artifact refs pending; interrupt overflow at PreToolUse persists a policy-owned execution hold. A normally delivered exact-run interrupt also remains a level-triggered execution hold at every later PreToolUse, without redelivering its card, until a newer relevant governed semantic state supersedes it or its exact run terminalizes. Codex and Claude-family adapters both compile that hold to an unconditional native deny; `next_boundary` and `human_attention` do not deny. HookEvent, report, and semantic state publish in that order with deterministic identities and retryable fault boundaries: failure before any later publication leaves the same card recoverable exactly once, and retry resolves every durable report reference before advancing the cursor. A report/state publication failure after the HookEvent emits only the current native deny (never the unpublished card) before nonzero exit, so the healed retry delivers the card once. Reports call sender-declared changes `declared_state_delta_refs`; `compound coordination measure --report drep_... [--home P] [--json]` persists a current receipt that labels every temporal owned action delta as `observed_action_refs` and calls only actions carrying the exact delivery report evidence ref `caused_action_refs`, so an unchanged recipient action measures zero. Native session identity is conjunctive authority: delivery resolves only an exact active delegate-session/work/run/harness binding, never a sole workspace/harness heuristic, and the semantic cursor stays stable across successor runs within that binding. Full bodies remain in Compound artifact custody and are available only through `compound work collaborate body ... --session <owned-session>`. Explicit event relevance dimensions remain conjunctive. A pre-addressed run requires that exact active `ratt_*`; a work-level event that did not pre-name a run may be fetched by the current active run on the exact owned native session only when its actor/work/scope/dependency/policy/base/resource state satisfies the same relevance dimensions used for delivery. The body door holds no second authority path of its own: it revalidates the recorded event and its delivery class through the same delivery-evidence check a delivered card passed, then resolves the recipient with the SAME recipient resolver the delivery projection uses — seeded from the attempt's own recorded harness and workspace — and requires the resolved work/actor/run to equal what the caller claimed. Targeted actor constraints still narrow both paths. Every path verifies the body ref and emits neither an acknowledgement nor a coordination event. No acknowledgement is emitted by default; one would require a separate governed state change. The replay and delivery state are projections, not a second chat ledger or authority: recording/delivery writes no memory candidate or claim, and durable knowledge still enters only through candidate/adjudication.
Create-plan integrity note: `compound.work_create_plan.v0.2` derives its id from the canonical admission payload, including normalized `action_context`, and admission requires both that digest and the `create-plans/<id>.json` path identity. Admission writes a content-addressed `work/action-contexts` snapshot even when context is absent; Workbench and dispatch consume that immutable snapshot, while legacy `v0.1` previews remain visible only as blocked re-plan-or-retire debt.
Evidence-autonomy note: the sole implemented class is `job_health`; it remains bench-only and has no live production consumer. `compound director propose --apply` remains a single synchronous proposal command; it may materialize a job-health create plan as `proposed`, then ask `AdmitAuto` to cross only to `admitted`. A legacy/broad `director-apply` grant authorizes filing but cannot establish eligibility. The separate decision-record grant must carry exact `auto_admit_class`, `min_terminal_samples`, `min_window_days`, and `max_failures` values. `artifacts/autonomy-eligibility/<id>.json` seals the latest deterministic sample window: unique terminal WorkEnvelopes, Receipt outcome/content hashes, genuine non-route state deltas, exact work-bound deciding route-row hashes and request identity, current route-plan id, router YAML revision, requested route id, and actual resolved provider/model mapped to a current eligible capability claim. Missing identity, route aliases, unknown claim levels/tiers, and free or supervised resolved lanes fail closed. `AdmitAuto` replays that artifact against live sources and the exact active grant inside the shared autonomy-admission advisory lock; canonical halt, operational-decision, editable-surface, operator-state, standing-grant, router, route, work/archive, and lifecycle writers take the same lock, and concurrent attempts commit once. `grant`, `freeze`, and `ruling` promotions are reserved to the operator-provenance policy-decision importer; ordinary policy/doctrine imports remain evidence-only. Runtime controls fold from protected raw decision history so generic same-ID MVCC supersession or review edges cannot hide them; the narrow pre-stamp operator-record shape remains supported for existing broad grants/freezes. Generic memory adjudication rejects copied stamps, reserved operational kinds, and attempts to occupy a protected decision slot, so extracted meaning remains evidence. Archive/restore is serialized with admission, and eligibility refuses an unindexed terminal work file left by a move/index crash gap. Stale create plans cannot overwrite an already-created/admitted work id. The halt projection is mutated only through the locked `compound policy autonomy-halt` command; direct halt YAML edits are unsupported. Manual `policy.yaml` or editable-surface YAML edits while autonomy is available are also unsupported: engage the global halt, edit and validate, then clear the halt through its command. New evidence, revocation, duplicate route/Receipt identity, router drift, tamper, route-only proof, or a non-eligible lane fails closed. On success, the WorkEnvelope and proposed→admitted event retain the grant/claim, eligibility, threshold, Receipt, route-plan, route-evidence, resolved provider/model, and router-revision refs. Halt, editable-surface, freeze, and the compiled operator-only risk frontier run first. No class grant is created or enabled by the code path itself.
Atomic publication note: route-ledger and operational-archive-index writers hold the shared autonomy-admission lock, close writable descriptors, prepare and sync a rooted sibling, then publish through a Darwin/Linux atomic name exchange (or a no-replace hard link when the canonical file is absent). The displaced inode is validated after the exchange and before commit; identity drift makes the transaction fail closed with both complete names retained for manual repair. This contract covers every supported Compound writer and concurrent path mutation completed before the exchange. A same-user process that bypasses the admission lock and keeps writing through a descriptor opened before exchange is hostile out-of-contract corruption: Darwin and Linux provide no portable mandatory lock or directory-entry CAS that can revoke such a descriptor.
`compound work dispatch-advance` records dispatch-start results under `COMPOUND_HOME/dispatch-advances` and advances at most one actor-claimed work item to `running` through the policy-backed `work advance` path; it does not execute task commands, close work, or promote memory.
`compound workbench --capability-task <task> [--capability-harness <harness>]` composes that same read-only workbench with the existing CapabilityGraph surface under `capability_surface`, defaulting the harness to the actor; it offers capabilities only and still does not dispatch, execute tools, install skills, activate hooks/MCP/plugins, or mutate graph/work state.
`compound work dispatch-close-plan` records proof-gated close-readiness results under `COMPOUND_HOME/dispatch-close-plans` and records the selected `verifying` work item's normal `ClosePlan` under `COMPOUND_HOME/close-plans`; it requires artifact, verification, or observed-run evidence and does not close work, emit Receipt/Transition proof, execute task commands, or promote memory.
`compound work dispatch-close` consumes proof-gated close readiness and closes at most one selected `verifying` work item through the existing `work close` Receipt/Transition chain; missing proof or policy hold records a no-op result under `COMPOUND_HOME/dispatch-closes` without mutating work.

Packet imports may carry optional `Proof-medium: web-ui | ios | cli | api | none`, stored on WorkEnvelope as `proof_medium`. Dispatch claim/step emits a worker prompt with a `LOAD THESE SKILLS` block built from the existing CapabilityGraph/ContextPack offers for that task and harness; this is an offer surface, not a second resolver and not execution. `work close` enforces packet proof media only for completed outcomes: `web-ui` requires screenshot plus design-review proof refs, `ios` requires screenshot proof, and `cli`/`api` require command-output or test proof refs. Receipts record offered capability refs in `skills.capabilities_offered` alongside candidate/used/missed/false-triggered fields so offer-to-use feedback remains measurable.
`compound work dispatch-step` records one safe lifecycle tick under `COMPOUND_HOME/dispatch-steps`, delegating to existing dispatch-close, dispatch-advance, or dispatch-claim in that priority order; source-packet flags are passed only through the existing dispatch-claim producer path, so packet import + claim remains governed by the normal packet adapter and claim policy. Recorded step results include pre-step `counts`, post-step `post_counts`, `next_action` from the post-step workbench item or no-op reason, plus `can_continue` / `continue_command` when another safe tick is available. When a step cannot continue safely, the result records `wait_reason` and `blocked_on` so callers can distinguish no-open-work, review, run-evidence, close-proof, and actor-claim waits without re-reading the whole workbench. `--until-wait [--max-steps N]` records an aggregate under `COMPOUND_HOME/dispatch-step-runs` while repeatedly invoking the same one-step path until a wait reason or max-step boundary; source-packet producer flags are cleared after the first step so continuation never re-imports the same packet. `workbench` surfaces those aggregate run summaries read-only as `dispatch_step_runs[]` with count, status, wait reason, step actions, work ids, and artifact path. It does not execute task commands or synthesize run observations.
`compound work dispatch-step --source-packet <packet.md>` reports `wait_reason=source_packet_terminal` when the packet hash already maps to a terminal WorkEnvelope, preserving the existing work/source proof and avoiding a misleading generic no-open-work stop.

| **ActionIntent** | a proposed mutation, pre-policy | actor, capability, action, target{kind,id,path}, mutation_class (read\|local_reversible\|external_side_effect\|public\|destructive\|financial\|security), reversibility, blast_radius, estimated_cost{}, preview, rollback_plan |
| **PolicyDecision** | the governance verdict an adapter enforces | outcome (allow\|allow_with_obligations\|hold\|deny), reasons[], obligations[] (write_receipt, include_rollback_ref, run_verification), standing_grant_id, expires_at; policy may consume runtime evidence such as source-control status and placement guard context, but evidence can only raise caution or add provenance, never relax a decision |
| **HookEvent** | one normalized harness lifecycle event (**no raw prompt stored by default** — only `input_hash`) | harness, hook_name, hook_capability (context_injection\|policy_gate\|telemetry\|receipt), ids{work_id,context_pack_id}, input_hash, side_effects[], output_ref, duration_ms, status (ok\|fail_open\|blocked\|error); persisted to `events/<hook_event_id>.json` and `events/hook-events.ndjson` when `COMPOUND_HOME` is available |
| **Receipt** | how a run is learned from (proportional to risk/blast-radius) | outcome, state_delta{changed,summary,files_changed[],artifacts[],verification[]}, cost{model_calls[],tokens,usd_actual,usd_estimated,human_minutes}, skills{used/missed/false_triggered/candidate}; `used` merges declarations and allowed recorded ActionIntents; completed+changed receipts produce bounded `declared_used_by` correlation, not execution observation, capability contribution, or independent validation; completed proof-bearing non-`audit_only` closes infer `changed=true` so omitted flags do not hide landed deltas from Workbench or receipt mining. Explicit `missed` refs add `missed_when` graph edges using the receipt summary as context, explicit `false_triggered` refs add `false_triggered_by` edges for the same context, and `candidate` comes from ContextPack offers and remains inert. `work close` / `hook session-end` auto-consume receipt feedback when graph state exists; `capability consume-receipts` replays it. `work close --missed ... --false-triggered ...` and `hook session-end` fixture fields are the explicit closure inputs for missed/false-triggered feedback. memory{candidates/promoted/parked}, next{suggested_work[],review_trigger} |
| **Transition** | the apex object joining projections (see `transition.md`) | before_refs[], action_refs[], after_refs[], state_delta, projections{memory,skill,product,cost,risk,taste,routing,human_attention}, second_order{future_behavior_changed,should_harden,should_create_rule,should_decay,revisit_at} |

`compound work observe-run <work_id> --trace <trace_id>` is the run-observation seam between `running` and `verifying`: it reads an existing `TraceEnvelope` from `COMPOUND_HOME/events`, requires the trace `ids.work_id` to match when present, records the observation under `COMPOUND_HOME/run-observations`, and delegates the state change to policy-backed `work advance` with the trace attached as a verification ref. `compound workbench` surfaces those observation artifacts as read-only `run_observations[]` on matching items, and `compound work close-plan` surfaces the same verification/observation evidence while generating an explicit `work close --proof ...` proposal. These commands do not execute task commands, close work, emit Receipt/Transition proof, promote memory, or claim task success.

`work_id` is **the** anti-fragmentation object: a control-spine fossil scatters identity across packet-id /
run-id / provider-call-id / receipt-path / memory-slug / dashboard-card. One root id linking
intent→context→action→policy→run→receipt→transition is the cure. Governance is enforced here: **a mutating
adapter rejects a mutation that lacks a valid `PolicyDecision`** — prompt-only "approval" is not enough.
`StandingGrant`s pre-authorize scoped envelopes (`target_path`, `max_usd`, `max_tokens`, `blast_radius`,
`reversibility`) so agents act without being paged only while inside the grant; leaving the envelope escalates
to `hold`/blocked through the same policy gate.

Cost matching is shared by exact standing grants and the L3 path-scope fallback.
An explicitly known zero USD/token estimate needs no ceiling for that dimension;
it is not spending permission. A positive estimate requires its own explicit
ceiling and cannot exceed it. Known estimates and their configured ceilings must
be finite and non-negative to consume a grant. Unspecified estimates retain their
legacy semantics. This changes no capability, scope, destination, presence,
expiry, destructive-operation or security-floor boundary, and non-read receipt
and rollback obligations remain in force.

`PlacementGuard` is policy context, not a second hook path: PreToolUse may pass owner-placement evidence into
`PolicyDecision` with staged `observe -> warn -> block` behavior. Only `confidence:block` can hard-hold;
`OPEN_WRITE` and `PLACEMENT_OVERRIDE=1` fail open, unknown modes fail open as observe, and
interactive Codex records the hold without publishing a native permission
decision. An explicitly enforced isolated posture returns
`permissionDecision=deny`; `compound hook pre-tool-use --enforce <fixture.json>`
returns the same gate exit code for wrapper adapters.
`compound policy import-placement-rules <placement-rules.json>` installs generated owner placement rules
as `compound://registries/placement-rules` under `COMPOUND_HOME`; the source view is evidence/rollback
input after import, while PreToolUse reads the Compound-owned registry.

Live OMP ingress normalizes only native `read/write/edit/glob/grep` to canonical
`Read/Write/Edit/Glob/Grep` before policy and provenance construction. Unknown tools,
other harnesses, tool parameters, and explicit schema revisions remain unchanged.
New mutation-input identities use the canonical name on both PreToolUse and
PostToolUse when the native payload supplies target input.

The `PLACEMENT_OVERRIDE` fail-open stays fail-open by design, but it does not stay silent: whenever it
actually fires, `work.RecordAction` persists one additional observation Event alongside the PolicyDecision
it already writes, so a fired override is an audit observation on the work event log, not only an
in-decision reason string.

A dirty-repository mutation can proceed only through an exact recorded capability, not a general cleanup
acknowledgement. The first blocked PreToolUse persists its ActionIntents with one session-bound,
content-free mutation-input digest shared by the complete target batch. `compound authority dirty-plan
<repo> --work-id <owner-work> --action-intent <id>...` loads those persisted actions and derives the
action work id, digest, capability, action, mutation class, and canonical target set itself. The owner work
id is provenance and must still be `claimed`, `running`, `verifying`, or `held`; executable identity is the
live action work id plus mutation digest, exact repository/root/remote/full HEAD/dirty-state fingerprint,
and exact target set. After an `authorize_mutation` review is recorded, PreToolUse attaches the capability
to every action in the batch or none. A partial target set, another session, changed bytes/index state,
tampered plan, inactive owner work, or legacy count-only review keeps the normal dirty hold.

---

### Reviewed Git index-lock recovery

`compound authority git-index-lock plan|review|apply` owns the narrow stale-lock
recovery path. Raw `.git` mutations remain forbidden. A plan seals primary-checkout
directory and file identities, HEAD, exact index, both binary WIP patches and raw
tracked-byte hashes. Linked/bare checkouts, untracked files, hidden index flags,
special entries, active Git operations, ambiguous liveness and nonempty/fresh locks
are refused. Review binds the entire plan (including work and actor) to explicit
operator provenance and evidence of an actual all-writer pause, expiring within
five minutes. Recovery approval alone is not evidence that writers are paused.

Apply first reserves the exact allowed action on running work. Private 0600
preimages and a durable recovery locator precede the move; 0700 directories and
descriptor-pinned, platform no-replace rename preserve recovery evidence. Synced
move-started evidence precedes final liveness, policy/window/lease and identity
checks. POSIX rename has no inode compare-and-swap: the reviewed writer-free window
is required, not replaced by those checks. Post-move drift is a partial failure,
never authority to overwrite a new lock or index. Concurrent apply is refused.

An interrupted invocation reconciles retained evidence read-only and records its
actual outcome; it never repeats the move. Completed replay reports historical
receipt/transition identities, not a claim that no new lock exists now. Missing or
unreadable evidence retains the work reservation for explicit reconciliation.
There is no background lock sweeper and no runtime installation implicit in this command.

### Sandbox notification writes

`compound act paddle-sandbox-notification` is the typed Paddle sandbox consumer of
the existing `act` seam, not a generic HTTP or payment actuator. Preview reads only;
`--plan` records an exact ten-minute approval plan when current policy holds;
`policy action-approve` records operator approval; `--confirm` consumes matching
authority and emits resolvable pre/post-image Receipt + Transition proof. The
caller supplies a claimed work id, lease actor, exact current-state fingerprint,
setting id, HTTPS `/webhooks/paddle` callback and environment-only sandbox key.
No live API origin, arbitrary fields, purchase, or broad standing grant is exposed.

The sealed v0.2 plan includes the prior snapshot, desired URL and exact additive
event union. Only destination and subscribed events enter PATCH. Every untouched
provider field (including the secret) contributes to one opaque preservation
fingerprint; raw keys, secrets and provider error bodies never enter receipts.
The one-shot adapter checks the exact intent body and decision expiry before
provider access, then rechecks expiry and the CLI's admission callback immediately
before PATCH. This is an adapter-specific expiry fence, not a change to the generic
`policy.Gate` contract. No-op confirmation does not consume approval or claim a delta.

A final GET refuses observed drift. There is no established provider-side atomic
CAS: a writer in the GET/PATCH interval cannot be excluded by this API. Operators
must keep this one sandbox setting free of concurrent provider edits during cutover.
An uncertain PATCH is never retried; readback either proves the requested state,
proves no change, or enters existing durable recovery. Automatic compensation
checks the action's expected post-image before restoring; drift or unprovable
recovery produces a finding instead of a blind overwrite. Compensation may finish
after forward approval expires; it is recovery of that action, not a new grant.
Explicit cross-process restore uses `BuildPaddleSandboxNotificationRevertIntent`
to propose a fresh sealed payload. `AuthorizeRevert` must independently verify
both images and the original action's canonical Receipt/Transition in the same
execution home; shaped caller bytes are not provenance. `ExecuteRevert` still
requires fresh allowing policy. Generic `RevertIntent` alone is refused.
The production-callback retirement condition is a verified isolated sandbox
endpoint followed by this exact setting cutover and readback; building the adapter
does not itself establish payment-flow or deployment success.

### Ordinary continuation preview and exact dispatch

`work continue --dry-run` previews the whole command, including `--until-wait`,
without lifecycle, lease, event, RunAttempt or detached-recovery writes. The
existing Continue result carries an explicit `preview`: eligible/refused/waiting,
exact target, proposed effects, resolved execution choice when available, and
performed/deferred checks. Read-only adapter/workspace/readiness preflight runs
before `would_execute`; refusal prose is never readiness evidence. Unsettled
detached launches are reported as pending reconciliation, not recovered by preview.
Fresh admission and process-start checks remain required at actual execution.

An explicit `--expect-work` carries the observed target and state/policy/last-action
generation through the existing dispatcher into claim, advance and close. A changed
queue or generation refuses before nested mutation, including pending-claim recovery;
a post-operation target check is not the safety boundary. Unpinned dispatch retains
policy-aware selection, skips foreign-held work, and binds its actual selected
generation. Preview consults those same lifecycle policy plans. Execution-stage
claim also compares its preflight generation.
The existing default remains one bounded execute step; zero is not a disable flag.

### Governed execution argument ownership

`work advance --execute --timeout <duration> -- <command> <args...>` reads
Compound options only before `--`. The child command and arguments after that
boundary are passed unchanged, including empty arguments and child options named
`--timeout`, `--by`, or `--launch-profile`. A child timeout cannot change the
Compound deadline or cause Compound duration validation to refuse the launch.
An absent or zero Compound timeout retains the five-minute default; positive
durations are capped at 45 minutes. Invalid or negative Compound durations refuse
before a work mutation or execution RunAttempt is minted. Detached execution
reserves two minutes of its 45-minute wrapper budget for finalization.

### Explicit same-work execution resume

`work continue --selected --actor <lease-holder>` is an explicit continuation,
not an unattended queue grant. It resolves the actor's sole claimed/running/verifying
work; ambiguity refuses until an exact `--expect-work` is selected. The existing
lease, saved execution selection and workspace are required. At most one bounded
detached governed door starts. A per-work admission lock excludes duplicate clicks;
the child checks the frozen source revision and tuple before ordinary fresh policy,
permission, reservation and start checks. Only the selected launch is reconciled.
`--resume` explicitly uses the existing verifying-resume owner; it never reopens
terminal work. Selected launches opt into `--keep-verifying-on-failure`: a new,
durable, same-work/action terminal failed attempt retains verifying for a later
explicit retry. An attemptless/unbound failure remains unresolved running; ordinary
first-run failure semantics are unchanged. A successful execution waits
for verification rather than silently spending another attempt. Explicit acceptance
(`--outcome`, `--move-type`, `--summary`) additionally requires a verification reference
distinct from captured executor transcripts before the existing close/proof gates.
`--verification <ref>` attaches a verifier's check through the same selected path,
without copying the work ID. The check must be new after the latest linked execution
finalization; reattaching an older reference cannot accept a newer attempt. Verification
attachment and close each compare the exact source state/policy/action under the
existing lifecycle admission locks. A dry run attaches nothing.
This check does not attest a reference's semantic quality; the verifier still owns it.
The console work-detail reader resolves the exact owner-created ID, including
underscored/custom IDs; it rejects path separators, Unicode control characters and padded
identities rather than assuming every work ID is a generated hex suffix. The
projection-door smoke checks real CLI ID/goal round-trips and zero CLI dispatch for rejected IDs.
Dead explicit doors without a new linked terminal attempt preserve source work and
report loss, never certify an old verifying state as new execution. Settled sidecars
are archived under the existing detached owner before another attempt replaces the
current pointer. Ordinary browser mutation capability gates remain unchanged.

`work close --outcome failed --verification-rejected ratt_X --reason R` explicitly
acknowledges a succeeded execution whose result verification rejected. The work
must currently be `verifying`; each named attempt must be attached and terminal
`succeeded`, and each reason must be nonempty. Repeat the flag for each rejected
attempt. Each pair takes its first following `--reason` before the next
acknowledgement flag, including when mixed with `--acknowledge-abandoned`.
The receipt records `verification_rejected:<ratt_id>:<reason>` alongside the
existing `abandon_ack:<ratt_id>:<reason>` namespace. Neither acknowledgement
rewrites RunAttempt bytes. Unnamed attempts retain the ordinary close grading;
without the flag, a succeeded execution still contradicts a failed close.

The proof-chain doctor accepts this execution/outcome distinction only from the
exact digest-sealed closing receipt for that work and named succeeded attempt.
Missing, wrong-attempt, empty-reason or unsealed acknowledgements do not certify;
the acknowledgement is not an authenticated evaluator result.

Terminal close discovers all same-work reverse links from the existing RunAttempt
store, not only the envelope/caller list. Unreadable or contradictory census
records refuse certification. Validated caller references join the prospective
set before discovery; known launch-free attempts retain the existing exact
terminalization rules. Discovered history preserves forward order and inserts
older retry predecessors before their successors. Active launches, prepared-only
launch effects and
unattached nonterminal executions require reconciliation; a terminal label alone
is insufficient. An exact rooted pre-start refusal is accounted for without
inventing a task failure or rewriting its execution record. The prospective close
generation includes discovered history, and the existing admission lock rechecks
the census before terminal publication. A refusal preserves newer work authority.
The terminal proof-chain doctor takes one fresh store census per pass and reports
reverse-only records omitted from frozen terminal work, including later evidence.
It uses the same uncertain-launch rule as close. Unknown census membership
prevents every terminal item from being certified; an unrelated nonterminal
work's reference cannot hide the unreadable record.

`compound work advance <work_id> --to running --execute --resume-verifying --harness <name> --workspace <project> -- <command>`
continues a verifying WorkEnvelope through a fresh running reservation and back to
verifying after each attempt. A failed resume retains its failed RunAttempt, reports
nonzero exit, and leaves work verifying for a governed retry; it never reports work
completion or closes over contradictory prior successes. Retry provenance is bound
before subprocess construction, and the unchanged terminal close/proof-chain gates
must later accept the actual success/failure/recovery history.
Without the flag, governed execution still requires claimed and failure still closes failed.
The flag cannot be used without `--execute` or combined with `--steal`; it does not
transfer a lease, reopen terminal work, or change `work continue` selection.

The existing actor, current policy, halt, workspace, adapter and exact source
state/policy/last-action CAS remain authoritative. All linked attempts must be
readable, same-work and terminal before reservation; prior success is not required
and is never permission. Each launch mints a new ActionIntent, PolicyDecision and
RunAttempt, retains prior history, and finalizes only its own running generation.
Early refusal leaves the WorkEnvelope unchanged; audit records may be emitted.
A newer lifecycle generation wins over a stale launch/finalizer.

Admission and launch both use current time. A lifecycle advance samples the
store-owned wall clock inside its admission transaction; an older caller timestamp
cannot authorize a mutation. Tests explicitly inject their logical clock.
Grant windows are half-open `[not_before, expires_at)` across ordinary grants,
autonomy envelopes, and desktop-surface grants. Equality at expiry refuses.

The start guard retains the existing admission lock order and exact running
work/policy/action generation. It reads the persisted admission evidence and
current policy before sampling launch time. The shared subprocess runner repeats
that read-only check inside `CommitLaunch`, after its work/per-attempt locks and
immediately before `cmd.Start`. This inner check never reacquires admission and
never persists replacement authority. A grant that expires or is revoked while
launch waits therefore prevents the subprocess from starting. Already-started
execution may finish after its launch grant expires; finalization keeps its
existing generation CAS and then-current ordinary policy instead of requiring
the original launch grant to remain live.


Resume uses the build-implement halt at admission and immediately before OS start.
Claude mutation resumes require fresh permission and blocking-hook readiness even
for explicit bypass; the historical claimed-execution bypass exception is not
inherited. Omitted mode on an unready resume refuses instead of reporting a
successful no-edit fallback. Explicit plan remains read-only. This is an explicit
execution door, not proof that unattended scheduling or any provider lane is ready.

### Scarce full-proof admission

Queued proof admission checks cancellation and its absolute deadline before treating
a vacancy as available, after waiting for the work admission lock, and immediately
before minting its RunAttempt. A cancellation or timeout observed at those boundaries
must leave no new attempt. This guard does not by itself establish FIFO ordering.


Full local proof extends the existing `work` and `landlease` owners. Before the canonical runner
enters its first subprocess, `work` records an allowing reservation ActionIntent/PolicyDecision,
mints and durably links one same-work `ratt_*`, then acquires the existing TTL lease with a
`canonical_full` digest over profile kind, repo, base ref, and the ordered inventory supplied by
`proofCheckSteps`. A live conflict is terminalized and attached as refused evidence before runner
entry. The public and adapter boundaries reject custom TTLs below thirty seconds before reservation;
accepted TTLs refresh at `min(TTL/3, 5s)` and continuously monitor the exact holder. Expiry or an
exceptional steal cancels the active gate process group, and successor admission waits for the
predecessor RunAttempt to confirm termination. Expiry itself never terminalizes a predecessor.
Recovery dynamically follows every immediate reservation holder to the oldest nonterminal
RunAttempt; ambiguous or cyclic chains fail closed, so a P→A→B steal cannot certify A's synthetic
terminal label while P remains live. Every gate subprocess inherits a kernel execution barrier
owned by `landlease`; generic landing acquire/steal and `run-attempt sweep` consult the same barrier,
so expiry or a synthetic terminal label cannot admit a successor while any descendant lives. A
governed live-steal request leaves the holder intact, asks its supervisor to cancel and wait for the
whole process group, and requires an exact retry after the predecessor releases. Externally
terminated or crashed predecessors use `--steal --reason ... --terminated-run <ratt_*>`. That
distinct `security`/manual/repo ActionIntent binds exact grant authority, prior holder, reason, and
non-reusable RunAttempt identity; Compound's barrier probe is the trusted termination-evidence
producer, so caller JSON and bare/reused PIDs never authorize transfer. Governance and
Work/RunAttempt reachability precede lease publication, while the exact reservation event is
published only after the lease write succeeds; the encoded holder tuple recovers the narrow crash
window without false `acquired` evidence.

Refresh governance is durably Work-linked before the lease is extended; only the later `pfrh_*`
record can extend the certifying window, so a crash cannot turn an unaudited heartbeat into proof.

Proof-refresh lookup enumerates the historical event family through complete sealed
segment inventories, preserving hot segments and loose legacy records. Inventory
attestation is separate from point-read index verification: missing or changed
attestation forces a strict source parse and repairs incomplete disposable indexes.
The first inventory check still reads sealed family history; warm lookup skips
unrelated sealed bodies. Reservations without scoped outcomes retain the legacy
work-log fallback, including older writers.

Prepared-close lookup discovers membership freshly from primary RunAttempt records
and validates launch sidecars only for the requested work. Reverse-only attempts and
older writers remain discoverable; unreadable primary authority still fails closed.
This removes unrelated sidecar reads, but intentionally retains the global primary
record census. These changes do not add FIFO admission or separate proof capacity.

Queued coordinated proofs observe occupied class capacity through an atomic,
generation-bound snapshot before entering lifecycle mutation. This avoids
re-reading the complete work log while waiting, accepts same-class holders from
other repositories, and does not wait on a finalizer's class lock. A vacant
snapshot authorizes nothing: the existing FIFO, source, Work, policy, generation
and execution-barrier checks still run at reservation.

Canonical proof v0.6 splits the existing Go inventory into general/release and
home phases. Together they run every original package exactly once, with the
same count and timeout. General, release, runtime, policy, installation and
consumer gates always execute freshly. Earlier report versions, including
v0.5, remain readable.

Only the home phase can reuse source evidence, and only from the same Work's
linked, directly executed, successful committed full proof. The owned artifact,
reservation, action/policy, terminal attempt, completion marker, Work link,
source fingerprint/base/head/tree and input seal must agree. A reused report
cannot donate transitively. Selection, completion and later proof-chain
consumption revalidate the origin. Output JSON alone is never a donor.

On supported Darwin hosts, the native adapter builds a tracked-source-only
fixture and seals its binary, source/metadata, restricted environment, sandbox,
system policy, host identity and observable ancestor metadata and directory entries.
Host boot identity uses the kernel boot-session UUID: wall-clock corrections can
shift the reported boot timestamp during the same live boot. A changed UUID
invalidates reuse; an unavailable or malformed UUID cannot certify a fixture.
Other host inputs and every existing source, policy and ancestor seal remain bound. Literal
ancestor-directory reads support the home code's rooted no-follow opens; names,
inodes and entry types exposed by enumeration are sealed, so unrelated directory
changes may conservatively prevent reuse. The sandbox permits designated system runtime files and devices, and denies
other outside file bodies, input writes and network. Its positive control opens
every ancestor without following symlinks; negative controls still prove
prohibited outside bodies are unreadable.
Only native proof-barrier descriptors survive the Python/process boundary;
a deliberate leaked descriptor is the negative control. Direct tests retain
Go's early-exit panic behavior. When the complete restricted suite passes but
observable ancestors drift during execution, the fresh pass is retained without
a reuse certificate; the suite is not replayed. The helper reserves this
disposition and normalizes nonzero test process exits so a test cannot manufacture
success. Source-integrity drift and cancellation remain failures. Unexpected skips trigger ordinary fresh
execution without a reuse certificate. The two narrowly classified existing
skips are the opt-in retained scale test and the non-root foreign-ownership
fixture with its expected permission-denied reason.

Fixture files are an accelerator in a per-user `.compound-proof-cache-<uid>`
directory below the standard macOS shared-user directory, never authority. The shared parent must
be a real, root-owned directory; group/other write permission requires the sticky
bit. The cache root and each stage remain owned private directories. This avoids
OS cache churn and atomic harness-state replacements under HOME without excluding
any observable ancestor from the seal. An unavailable or unsafe shared parent
uses ordinary fresh proof, never a HOME cache fallback. Prior HOME cache payloads
remain untouched until successful shared-cache proof permits exact owned cleanup.
Pinned no-follow directory descriptors, exact ownership, a cooperating-helper
lock and bounded relative cleanup protect preparation and retirement. Identical
source/binary/policy files retain their identities. Build caches stay outside
the fixture scratch directory. Integration-test roots live below a separate stable private
`.compound-proof-fixture-tests` directory under the user home, so repeated test
creation and cleanup do not change sealed production ancestors. A successful home gate in the admitted canonical
target checkout retires its exact payload while the governed attempt is still
live; the small owner marker remains. This also applies to successful fresh
fallback when its prepared seal still validates. Cleanup exhaustion or drift
retains the exact stage and reports it. Incomplete candidate work retains its
pre-land fixture until target proof or explicit exact cleanup. No global cache
scan or new registry is introduced, and later runtime failure still fails the
overall proof.

Completion accepts only an exact owned `canonical_full` report generated inside the continuous
evidenced window with direct producer exit. An `ok` report requires every reserved gate `ok` and
publishes `proof-check:pfrs_*` only after result, Work, lifecycle, completion, and terminal
RunAttempt persistence succeed inside `landlease.ReleaseAfter`. Only after that green commit has
released does the command publish the installed-runtime artifact consumed by Workbench; candidate,
release-pending, failed, invalid, dry-run, and CI reports cannot suppress the action. A post-release
publication fault emits a final non-green post-report verdict and exit 2. Installed-runtime
publication uses a synced same-directory atomic rename and a required completion footer, so partial
prefixes are never consumed. Gate identity uses the stable logical `compound` executable while
runtime dispatch resolves the current binary, keeping exact recovery stable across separate
`go run` temporary paths. A complete
`failed` report requires
every reserved gate terminal `ok|failed` with at least one failure and zero skips; it records
non-certifying failed result/RunAttempt evidence, releases under the same finalizer, preserves the
full report, prints one `battery: FAILED` verdict, and exits 1 without a VerificationID. A
persistence fault retains the holder, terminalizes the returning attempt, and keeps an exact retry
journal; retry
mints a governed predecessor-linked attempt when the prior terminal state cannot finish the same
transaction. The journal advances to the retry before its lifecycle publication and can adopt a
valid same-work descendant after an interrupted older ordering. A newly minted completion retry
stays pending until journal plus Work/lifecycle reachability are durable; if the process dies after
mint but before that publication, the next exact retry discovers and adopts the fully bound
same-work descendant from the existing RunAttempt owner instead of minting around it. Every retry
therefore remains recoverable, terminal, and Work-reachable. The final Work preserves the
completion ActionIntent/PolicyDecision. Retry descendant discovery requires exactly zero or one
matching child at each lineage hop; multiple matching children are made Work-reachable,
terminalized, and rejected as ambiguous. If an authoritative green or red result commits but lease
removal fails, completion returns the committed result
plus a typed release-pending error and retains the exact holder/journal. Because a report exists,
the first command preserves `--out`, emits one final `battery: FAILED release_pending=true` stdout
verdict, and exits nonzero. Every other error after a byte-identical complete `--out` report also
emits exactly one final `battery: FAILED post_report_error=true` verdict; pre-report usage/admission
errors emit no fabricated battery. The public
command checks that journal before reservation: an exact work/actor/workspace/command/profile match resumes the
owned completion, atomically restores and byte-verifies `--out` from the immutable Compound artifact
while the lease is still held, returns its prior report, and releases idempotently without a
heartbeat, new RunAttempt, holder, or gate subprocess. Publication failure retains the journal and
lease and cannot print green. A mismatch fails closed. Journal cleanup
happens only after verification publication and lease release have committed. Reservation state may be exposed by UI/read models,
but is not ContextPack/model context. Dry-run and CI subsets stay outside this certifying path.

### Actor coordination constraints

The read-only coordination projection uses the full shared Workbench for current
dispatch selection, eligibility, candidate preparation and lane capacity. Its
work constraints are narrower: the actor's current claims, unclaimed owned work,
an exactly selected item, and actor-owned admission previews. Typed lane gates
remain visible. Unrelated non-dispatchable work cannot create actor wait rows or
blocked chips. With no relevant item or gate, the actor has a queue-availability
summary and shared queue counts, not fabricated work dependencies. Console labels
these as shared queue counts and Work constraints.

Global continuation eligibility is unchanged: new ready shared work may legitimately
allow an actor to continue, suppressing its wait display as before. This differs
from unrelated claimed/running/held work inventing blockers. No verified general
cross-work dependency model exists in this projection. Opaque evidence references
and historic global wait summaries do not supply one; exactly supplied item
classification is preserved without enumerating foreign work as dependencies.
The projection keeps one Workbench snapshot, deterministic output and read-only
home behavior. It does not truncate a larger dependency list or change policy.
Its observation-summary index includes empty per-work results: missing rows or
unreadable input reported by that index never restart a directory scan for each
work item. Standalone item readers still load their own observations. Index
warnings remain visible once, and hold-evidence consumption is unchanged.

Coordination reads lane capacity through ONE request-scoped observation. The
observation is a read-only projection, not an execution authority: it never
dispatches, never records capacity-refusal evidence, never runs a live provider
probe on the coordination path, and never replaces the package-level lane
capacity guard used by dispatch and delegate. Read-only is enforced, not
assumed: a request that is not read-only is refused before any branch runs, so
it reads no source, probes no provider and writes nothing. One observation is
bound to one home and one request instant; a request naming a different home
or a different instant is refused rather than served from another instant's
facts, and a new request builds a new observation that re-reads the sources.

The observation reads each applicable capacity source at most once per request
— the operator lane-alias file, the CodexBar usage feed, Compound's own
supplement usage feed, the account capacity projection and the multi-auth
quota cache — and evaluates every actor separately against those parsed bytes.
The fresh and observed routes run the SAME composition, the SAME alias resolver
and the SAME alternative-ladder rule, so a decision cannot differ between them.
Alias precedence, provider-family matching, freshness and future-timestamp
rules, expected duration, resets, provenance, confirmation and alternative
ladders, malformed-row skipping versus scanner errors, and absent versus
unreadable sources are unchanged; the observation caches source facts, never
actor verdicts, and never lets one actor see another's rows. The shared usage
parser filters inside the scan, so the fresh reader keeps its provider-filtered
memory behavior, and the observation indexes the parsed feed by capacity family,
so an actor's copy is sized to its own family rather than to the whole feed. An
actor with no capacity family reads no capacity source.

A declared-source or tuple-backed request keeps the existing fresh guard
semantics, including its live-probe fallback. That delegation is an explicit
scope limit: the delegated path reads its own sources, so those reads are not
attributed to the observation's counters and a request mixing a declared-source
actor with a coordination actor reads the usage feeds twice. The projection
chooses the guard once per request, so one request never mixes two gates, and a
hard source failure is reported as an error rather than as an ungated actor: an
unreadable capacity source must never read as unlimited capacity. The delegate
pre-mint capacity recheck is unchanged: ReadOnly suppresses duplicate refusal
evidence, never the re-read, so a lane that walls after admission is still
refused before the spawn.

Workbench and coordination build one request-local Git facts session per projection
call. It reuses only repository-scoped read facts — canonical worktree root,
normalized refs and their commit resolution, current branch, worktree status and
worktree list, commit existence, ancestry between exact object ids, repository
identity keyed by canonical root plus exact expected base, and owned-path dirt —
with explicit first-read snapshot semantics inside that one request. Every Work
still evaluates its own delivery verdict: envelope and contract validation, the
attached candidate artifact, proof resolution, publication requirement, expected
base, and recorded delivery proof are never shared between Works, and a failed
shared Git read is reported as an integrity error for every affected Work. No
LandCheckReport, verdict, next action, or proof crosses Works. The session is
never stored on the home, persisted, or reused across requests, so a new
projection rereads mutable refs, status, origin identity, proofs, and previously
missing inputs; governed land-check, close, and admission keep fresh per-call
reads. A nil session means fresh reads per Work.

### Lifecycle finalization cost and evidence scope

Attaching a batch of retained RunAttempts reads the strict canonical lifecycle
history once for duplicate-attachment checks. The local attachment index lasts
only for that native advance under admission; original action/policy bindings,
whole-log corruption checks, recovery, CAS and append verification remain intact.

A work close uses the existing RunAttempt owner's `ForWork` census. It freshly
enumerates and validates every main record, refusing unreadable or ambiguous
membership, then validates launch sidecars only for that exact work. Reverse-only
owned attempts remain discoverable. Known foreign sidecar corruption remains
visible to the whole-home doctor and cannot impose unrelated-work authority on
this close. This is a scoped read through the existing owner, not a new cache or
membership ledger.

### Exact reviewed candidate proof adoption

Distinct work items may share one original canonical proof only after explicit review of the
same repository, explicitly selected proof coverage base and combined candidate.

`work proof-review WORK --actor ACTOR --file REVIEW` custodies a nonempty review
body of at most 1 MiB for the currently held verifying work. It attaches the exact
content hash through a native same-state ActionIntent and PolicyDecision before
immutable artifact custody, and returns the plain artifact reference consumed by
manifest review refs. Identical retries authenticate that attachment and recover
only its exact pending operation. Foreign holders, unrelated pending mutations,
corrupt existing bodies, and stale explicit generations fail closed. The optional
`--expected-policy` and `--expected-action` flags bind a caller's observed generation;
a completed exact retry is accepted only while its result remains current.
This door does not declare a review sufficient or change the acceptance contract.

The source holder uses
`work proof-manifest WORK --actor ACTOR --file MANIFEST.json` to custody an immutable
`compound.proof_member_manifest.v0.1` artifact. Its source work, repository fingerprint,
base/head, target ref and members are explicit. Every member preserves its original
`commit:` artifact and attached content-addressed `review_refs`; custody fills or verifies
the member acceptance/delivery contract digest. Every work explicitly accepts the combined
`commit:` candidate. Historical creation bases may differ when each remains an ancestor of
the fresh proof base, or when a member's creation base is contained between that proof base
and the reviewed head. A contained base is admitted only when every content-bearing change
the member made appears in the one proof's own scanned inventory, read at the same exact
head, and when that member's own range passes an independent `git diff --check`. Ancestry
alone never supplies coverage. Ancestry never supplies missing membership, review or
acceptance.

The proof base B describes coverage; the landing target T is separate. The
ceremony may retain an earlier B for a reviewed batch only after checking
`B != H`, `B <= T <= H`, and `B <= M <= H` for each member creation base M.
For M equal to B, the full canonical range covers the member range. For B
preceding M, native contained-base admission checks exact inventory coverage
and the independent committed member diff-check. These checks do not claim
that the historical M-before-B path performs a contained-member scan.
A changed B or H requires a new proof. A target that advances within unchanged
certified H retains the original B/H evidence and requires a fresh landing guard.

Authenticated manifest custody must precede the original proof reservation; a later review
cannot retroactively authorize an earlier execution. The original work executes the existing
complete `canonical_full` proof once. A destination
holder in `verifying` calls
`work proof-adopt WORK --actor ACTOR --proof-result pfrs_ID --manifest artifact:sha256:DIGEST`.
The deterministic `proof-adopt:pfrs_ID:DIGEST` ref attaches through the existing same-state
ActionIntent/PolicyDecision and lifecycle transaction. The action binds its exact evidence
arguments; exact retry is idempotent, including interrupted attachment recovery. There is no
second proof result, foreign RunAttempt attachment, adoption ledger or weaker proof profile.

Only landing resolution understands adoption. It reloads immutable review/manifest custody,
current source and destination contracts/candidates, the original direct proof and its complete
finalization chain; pending completion/recovery, red/dry/skipped/differential/transitive proofs
refuse. A closed or archived source remains usable when that chain remains authentic.
Historical admission policy is authenticated without demanding it stay the latest generation.
Private source publication still requires its unchanged direct same-work proof. Each member
retains separate acceptance, guarded landing, publication and consumer verification obligations.
For a pending landing, the current target must remain an ancestor of certified H;
a divergent target refuses without discarding completed evidence. A target that
already contains H follows the normal landed checks. A target advancing within
H preserves the same B/H evidence and requires a fresh main-SHA guard.

## 3. Hooks are clients, not brains

The OMP shim reads each boundary's native HookContext sessionManager identity and
transcript path with its cwd. A native ID remains authoritative while the file is
lazy; an absent or mismatched transcript is omitted, never replaced by a newer
same-cwd session. Header validation reads at most 4 KiB. Switches and forks resolve
at the next event, and cached start context never crosses session IDs. Callers
without the native API retain bounded compatibility discovery; that fallback does
not prove that a native receiver was addressed.

> Hooks are clients. compound is authority.

Hook event names are adapter vocabulary, not semantic authority. The stable input is a lifecycle intention such
as `prepare_context`, `observe_action`, `guard_mutation`, `normalize_artifact`, `observe_outcome`, `preserve_before_loss`,
`restore_after_resume`, or `close_with_proof`. The existing hook-plan owner must compile those intentions from
the frozen TaskContract/EffectiveContract plus risk, privacy, and context needs against a revisioned harness
capability attestation. Its HookPlan output is a harness-native projection with one explicit disposition per
intention: `native`, `fallback`, `degraded`, or `unsupported`. This replaces HookPlan-as-authority; it does not
add a peer lifecycle or hook system. The full ownership and compaction/resume flow is in
[`information-custody.md` §5](./information-custody.md#5-recall-context-injection-and-hooks).

`compound harness posture-plan --harness <name>` is the operator-facing
compiler door. Interactive Codex and Claude Code default to the stable
intentions `prepare_context`, `observe_action`, and `observe_outcome`;
their `PreToolUse` bindings persist ActionIntent/PolicyDecision evidence without
publishing a native permission decision or blocking exit. Isolated
Claude-family executor profiles select `guard_mutation` instead and install the
binding with explicit enforcement. The compiler returns the adapter-native
projection plus its attestation revision. The executable CLI registry supplies first-party
shell admission to the hook monitor at runtime; the policy package has no
parallel Compound subcommand allowlist. Unknown registry names and commands
whose registry disposition is `hold` retain the security-floor refusal.

A plan is immutable for the work/session and cites the harness family, surface, version, feature/trust state,
config scope, handler revision, and fallback revision it was compiled against. Installation observation is
separate evidence. “Installed” therefore cannot imply “ready,” and an unknown harness cannot inherit optimistic
capabilities from a familiar one. On delegation, resume, or harness replacement, Compound restores the owned
working object and remaining ceilings, then compiles a new plan for the new executor.

Hooks may: **request** context · **freeze** selected context into a ContextPack · **observe** proposed actions ·
**guard** mutations (only in an explicitly enforced posture where the harness supports blocking) ·
**capture** evidence/receipts · **emit** typed events. Hooks may **not**:
deep synthesis · canonical memory promotion · tool/model routing policy · product judgment · global truth
arbitration. (Hooks sit at high-gravity points — a tiny SessionStart nudge bends the whole session — so keep
them thin.)

Commands: `compound hook {session-start, user-prompt-submit, pre-tool-use, post-tool-use, session-end, stop}`.

Prompt-injection boundary: live `UserPromptSubmit` and `PostToolUse` hooks mint
content-addressed `SourceEnvelope` records and maintain a bounded per-session
lineage projection. `PreToolUse` loads that projection, classifies every tool
domain, binds exact arguments/destination/source refs/tool schema/policy
revision, then records the security-floor result after standing grants.
Interactive Codex and Claude Code keep routine work prompt-free: policy
`hold`/`deny` outcomes remain evidence and do not become native permission
decisions. An explicitly enforced isolated executor profile turns those exact
decisions into the blocking boundary. For an eligible exact approval, runtime
order is: persist the `ActionIntent` → mint or reuse the
`ExactActionApprovalPlan` → add its canonical reference before persisting the
`PolicyDecision` and `HookEvent`. Grant consumption and plan creation use the
same eligibility predicate: an adapter-enforced direct single-action baseline
hold, excluding security, placement, dirty source-control, and coordination
boundaries. A ready grant keeps its plan generation; consumption or expiry
rotates it. Historical plans without a full intent digest retain structural and
original source-binding validation, but inventory scans exclude them and their
grants from current authority. They cannot be reused, approved, or consumed;
malformed history and changed current source bodies still fail closed. The
executor sees only the neutral, evidence-backed
`Exact approval plan approval_plan_<12hex> is ready for operator review.`
sentence. Authentication and approval remain a separate operator boundary,
and no retry mutates global harness permissions or configuration. Exact
approvals cannot override a deny or security floor. Compaction and subagent
transformations preserve parent lineage and cannot raise trust.

Native raw-file permission targets are classified independently of proposal
labels and Git discovery. PreToolUse resolves the actual target (including a
symlinked existing parent for a new file) against the owned
`COMPOUND_HOME/config/harness-permissions/` namespace and reads persisted
director-proposal markers. Director-proposed permission mutations hit the
existing hard-deny core even when the proposal was ordinary or the caller uses
an operator-shaped label. Missing, unreadable, identity-mismatched or invalid
persisted binding produces native denial with a content-free diagnostic, not
an empty runtime-error response. Non-director bound work retains security-class
grant/approval semantics; the dedicated typed permission-apply owner keeps its
own checks. Batch members are checked individually, and a denied member denies
the native operation. Observation-only profiles still observe; only explicit
enforcement projects native denial. Logical namespace entries remain protected
when a final leaf resolves outward; the permission-authority owner reads rooted
regular files and rejects leaf or parent symlink bodies. An escaped external
referent therefore cannot become accepted authority.

This bounded target check does not authenticate actor labels or contain
arbitrary shell processes, hard-link aliases, or target races under the same OS
account. Other protected target families need their own owner mapping.

`work act` uses the work-owned approval handoff: validate current active work
and its lease, persist the immutable ActionIntent as a proposal, and evaluate
current policy and autonomy halt before considering an exact grant. A held
proposal is reviewable through `policy action-plan` but never enters performed
work history. Saved execution-plan decisions are not authority. Same-ID body
conflicts fail before consumption; matching, one-use consumption, and allowed
action recording share one autonomy-admission lock. Exact bindings include the
complete canonical intent, including work, actor, target, risk and input trust;
only retry ID and creation time are excluded. Insufficient legacy bindings do
not match. A persistence failure after consumption can burn a grant, never
authorize a second execution.

Product-hypothesis approval is a different door with its own record.
`product hypothesis-approve` publishes an immutable
`product_hypothesis_approve_intent` transition before it touches the candidate,
then exactly one settled outcome: `product_hypothesis_approve` when the
exact-generation CAS replaced the candidate, or
`product_hypothesis_approve_refused` when a concurrent writer won it. Only the
committed outcome names the approval act in `after_refs`, so a crash between
intent and outcome reads as "intent, no outcome".
The intent binds one private ArtifactStore snapshot containing the original request,
actor/time, exact gate-read preimage with digest and custody token, and proposed
postimage with digest. The candidate CAS appends to `committed_approval_ids` with the approved
sentence; measurement review preserves that dedicated witness field. Generic
evidence refs remain citations and cannot prove that an approval committed. A failed
outcome write returns the original approval ID and `recovery_required`.
`product hypothesis-approve <candidate_id> --resume-approval <approval_id>` loads
that frozen operation and records its original terminal bytes idempotently. It never
rewrites the candidate, including after a newer measurement. Missing/conflicting
witnesses or legacy intents without snapshots remain `recovery_required`; intent
alone never proves commitment. Ordinary retries detect an unsettled witness and
point to the original resume command instead of minting another approval.
This is the same recoverable
publication shape as the work-action handoff above, not the same mechanism: no
`ExactActionApproval` grant, lease, or policy decision is involved.

The full input/model map and migration plan is
[`docs/security/prompt-injection.md`](../security/prompt-injection.md).
`compound contract hooks-authority` reports `hook_readiness[]` from installed-hook authority, checked fixture coverage, and live-adapter support, plus `pre_tool_use_readiness[]` from installed hooks and harness native-block facts. With `--observe-live`, it exposes desired `hook_plan`, read-only `installation_observation`, and deterministic `reconciliation` separately; observed extras remain evidence and never enter desired authority. `compound hook pre-tool-use --enforce <fixture.json>` returns the same gate exit codes as `policy evaluate` for non-blocking adapter fallbacks.
The repo-owned opencode adapter registers only SessionStart, UserPromptSubmit, and PostToolUse with
status non_blocking. It injects through the stable chat.message message.system field, records completed
tools through tool.execute.after, and fails open on every callback. It does not register
tool.execute.before because that callback cannot deny a tool call. These installed rows provide context
and observation only; they never satisfy pre-tool mutation readiness. Its
installer consumes the compiled semantic posture and refuses a native-event or
mutation-readiness mismatch before writing `opencode.jsonc` or the plugin.
Reconciliation reports an active command match as `registered`; it reports `ready` only when desired and
observed handler-content revisions also match. Invocation arguments have a separate privacy-safe digest, so
argument drift remains detectable without treating configuration bytes as handler-content proof.
Hook-plan reconciliation also carries `preserved_bindings` for exact operator-owned declarations outside the
managed plan. Those rows remain `external_preserved`: they are reserved before a managed fallback can claim the
same observed slot, survive narrower imports, and never enter active hooks or readiness. A concrete handler whose
content cannot be attested (including a symlink) retains the fail-loud `unavailable:concrete-handler` sentinel;
authority/readiness reports it as `handler_unavailable`, excludes it from EffectiveContract and ContextPack active
hooks, and requires restoration plus re-import before mutation. Opaque `handler:` and `hook://` identities make no
local content claim and retain their registered/readiness semantics. These fields and statuses are additive v0.1
extensions; the schema version changes only if strict-consumer evidence proves additive compatibility insufficient.
Each import records incoming hook count, the exact resulting authority revision, and the prior authority revision,
so identical source bytes cannot collapse distinct preserved-merge results into one provenance record.
Each: `stdin JSON → normalized HookEvent persisted under events/ → compound service → typed object written under .compound → small
harness-compatible output`. The lessons from a mature hook system map cleanly:

```text
SessionStart   → ContextRequest + EffectiveContract + ContextPack + context/injections/<hook_event_id>.md   (returns a compact orientation summary and persists exact injected text plus inclusion/exclusion provenance, NOT a universe dump)
UserPromptSubmit → ContextRequest + EffectiveContract + ContextPack + one bounded injection artifact (live prompts are transient relevance queries: persisted intent is an opaque digest, breadth retrieval validates but does not capture observed-fact proposals, native ContextOutput is byte-identical to the artifact, Summary is a content-free id/count/hash diagnostic, and no WorkEnvelope is created; explicit fixture replay may still model proposed work for compatibility)
PreToolUse     → ActionIntent + PolicyDecision (+ held WorkEnvelope review or exact batch-bound capability for dirty source-control holds)
                                                                  (returns allow | allow_with_obligations | hold | deny)
PostToolUse    → ToolEvent + ArtifactRef + VerificationCandidate
Stop           → Receipt + Transition + MemoryCandidate/SkillCandidate/FailurePattern/RouteObservation
                                                                  (Stop may inject end-of-turn feedback; capability: StopFeedback)
SessionEnd     → same receipt family, telemetry-only — SessionEnd can never inject context,
                                                                  and some harnesses have no SessionEnd at all (capability: SessionEndReceipt)
```

A workless `SessionEnd` receipt is content-free with respect to transcript text: its summary carries
opaque SHA-256 refs for the first user and last assistant text plus tool/window metadata. The raw
messages remain in the private source transcript referenced as evidence and are never copied into
`COMPOUND_HOME` receipt or event bodies.

For Codex, the live `Stop` adapter is also the stuck/resume consumer. It binds the thread and turn to one
open WorkEnvelope, extracts only a terminal blocking question from the existing transcript reader, and may
return `decision: "block"` with a cited continuation only when current operator-provenanced standing law and
policy both allow it. The continuation is recorded on the same `work_id` as ActionIntent → PolicyDecision →
Receipt → Transition without closing the parent work. Risk-floor, novel, stale, or unbound asks return no
continuation and remain paused as an existing DecisionBrief-backed review item.

The companion must still supply one exact live thread/turn job. Work identity comes first from the
existing owned collaboration-recipient binding (spawned attempt, or exact declaration plus corroborating
lease), not a field invented on the companion record. A third path covers a session this runtime LAUNCHED:
one `harness_execution` attempt whose copied per-run selection snapshot pins that exact session id
(`pinned:--session-id`), whose durable launch sidecar records a committed start, whose work is still
`running` under the exact ActionIntent/PolicyDecision reservation that authorized the launch, and whose
action-derived actor holds the lease. It resolves before the envelope forward-link exists and never mutates
the work to attach the attempt; duplicate live session ids, prepared-only startup races, `start_error`,
terminal attempts, and superseded or canceled authority refuse. Actor identity comes from the authorizing
ActionIntent — never the attempt's harness label or lane profile. Complete legacy work-reference tokens remain a
labeled compatibility bridge only when owned session evidence is genuinely absent. Conflicting prose,
known inactive or superseded bindings, and invalid exact presence records refuse delivery; they never
downgrade to a shorter prefix or weaker prose authority. Only the would-be Stop legacy fallback scans
one work-event snapshot for prior bindings; current exact bindings and ordinary hooks skip that extra
history-sized check. This is identity continuity, not new permission or proof of downstream benefit.

**Capability-aware by harness — not fantasy-uniform.** Current Claude Code and Codex releases support blocking
PreToolUse hooks; Codex also layers `AGENTS.md` + skills through progressive disclosure (it sees skill
name/desc/path and loads full SKILL.md on demand; the initial list is capped). So a
`HarnessRegistry` records what each harness actually supports (`session_start_context`,
`pre_tool_use_block`, `session_end_receipt`, `native_skills`, `mcp`, …) and its `limitations[]` (long-session
drift, fail-open risk, instruction cap, skill truncation, cwd-dependent guidance). Native blocking is claimed
only when both the harness capability and an installed trusted write-scoped hook are proven. External maps enter as normalized `compound.harness_registry.v0.1` manifests, while OS-style entrypoint registries enter through `compound harness observe --os-entrypoints <file>` with only session-start context inferred; observation is evidence/no-promote, not hook installation. **No live hook install before the fixture loop passes.**

**Installed ≠ enforcing (`compound doctor`, hook enforcement section).** Every other hook signal doctor
reports compares CONFIG bytes: `installed`/`ok`/`drift`/`absent` says the harness config names Compound's
command, which stays green when the file behind that command is a no-op — and an exit-0 hook is
indistinguishable from an allow, so a fail-open leaves no signal anywhere. The enforcement check therefore
probes behaviour: it parses each supported installed PreToolUse command into an executable and exact arguments,
without evaluating shell syntax, requires `--harness` to match the configured row's canonical runtime identity,
and invokes its live stdin route. Each distinct argument vector and configured harness gets a probe.
The probe synthesizes a destructive mutation with a run nonce and requires an ActionIntent for its exact target,
a non-allowing PolicyDecision for that action, and a HookEvent with the action-bound input hash. A canned or
recorded response and an exit-0 stub fail. The probe uses an isolated temporary `COMPOUND_HOME` and cwd,
and reports `FAIL` with a reinstall remedy rather than changing doctor's exit code. This is a functional
check of a trusted installed executable, not a sandbox against a hostile program: other environment
variables and the local user's process privileges remain available to that executable.

---

## 4. Instructions: tiny + compiled

A served `AGENTS.md` / `CLAUDE.md` (what a running compound instance gives another repo's agents) is a thin
entrypoint, not the doctrine:

```md
You are a client of compound.
Before non-trivial work: obtain or create a WorkEnvelope; load the active EffectiveContract + ContextPack
(do not infer canon from this file or cwd alone). Mutations require ActionIntent → PolicyDecision. Close
with a Receipt (state_delta + evidence) and a Transition. Retrieved evidence is not canon. Generated state
goes under COMPOUND_HOME, not source. Proof is a state delta, not a report.
```

Everything else loads **by scope** from the EffectiveContract + ContextPack + Skills + Policy. (compound's
*own* repo `AGENTS.md`/`CLAUDE.md` is the build guide — fuller — because agents here are *building* compound,
not consuming a running instance.)

---

## The first runtime slice (boring on purpose)
```text
compound home init --local
compound contract compile --harness claude-code --cwd .
compound orient --task "audit generated-state layout"
compound work create --intent "audit generated-state layout"
compound policy evaluate fixtures/action-intent/local-doc-write.json
compound policy import-placement-rules fixtures/policy/placement-rules.json
compound policy placement-authority
compound policy import-operator-state fixtures/policy/operator-state-l1.json
compound policy operator-authority
compound receipt create  fixtures/receipt/state-delta.json
compound transition create fixtures/transition/basic.json
compound transition show <id>
```

`compound mcp-transport` exposes a newline-delimited JSON-RPC 2.0 MCP server on stdio with two bounded request
modes. Lifecycle clients negotiate an exact supported revision with typed `initialize` parameters, then send
`notifications/initialized` before `tools/list` or `tools/call`. Modern clients may call `server/discover` before
initialization and send the advertised revision in
`_meta["io.modelcontextprotocol/protocolVersion"]` plus an object in
`_meta["io.modelcontextprotocol/clientCapabilities"]` on each stateless `tools/list` or `tools/call`;
`server/discover` and `tools/list` responses include bounded cache hints. Stateless describes the transport
lifecycle, not the tool's write effects.

The closed tool set is `compound_finding_record` and `compound_orient_recall`. The former persists one private,
operational finding (including its evidence refs) under `COMPOUND_HOME/observed/findings`; it never writes memory
authority. The latter persists a sealed EffectiveContract, its `latest.json` projection, and a ContextPack through
the existing orient materializer; it is not read-only or idempotent. Tool results use the portable text-content
`CallToolResult` shape shared by the advertised revisions. Inputs, summaries, and refs are bounded; returned
artifact refs are relative to `COMPOUND_HOME`; errors expose stable codes rather than internal text or paths. The
transport creates no Claim, candidate, promotion, adjudication path, or second memory authority.

Proof: one stable `work_id` links context→action→policy→receipt→transition; **all** generated files under
`.compound`; repo source unchanged except intentional source/test files. No legacy index, no model gateway, no memory
ingestion yet — they all need this spine first.

Home durability is part of the runtime contract:

`manifest.yaml` compatibility fails closed around the fixed identity
`schema_version: compound.home.v0.1` plus `kind: compound_home`. `home_kind` is
nonblank profile/placement metadata, so historical and harness-specific values
remain valid. Legacy v0.1 manifests without `kind` are readable and normalized
only in memory; their bytes are never silently rewritten. Init validates an
existing manifest before mutation and revalidates it through the pinned home
root before scaffolding. Every non-read command admission checks the selected
home before dispatch or delegates that check to a narrower protocol boundary;
read-only flags match exact, case-sensitive tokens and help bypasses admission only for handlers
that prove help returns before mutation.
Runtime hooks preserve their harness response contract when a home is missing
or incompatible: they receive no writable home path, emit a protocol-shaped
fail-open or deny result, and persist nothing there. Init, restore, and
read-only recovery diagnostics retain their explicit boundaries. Doctor and
typed compatibility errors report whether the home manifest or archive
envelope failed, with a backup-first, restore-to-a-new-absent-target recovery
path. Backup pins the exact manifest bytes before walking and verification
checks both envelope metadata and those embedded bytes. Fresh identity and
interrupted-init quarantine publication use atomic no-replace operations; a
retry can reclaim exact owned staging-removal quarantine chains, while fresh
publication still treats those quarantine names as concurrent population. A
partial or raced write never becomes authority.

```text
compound home backup [--target archive.tar.gz]
compound home verify [archive.tar.gz]
compound home restore archive.tar.gz --target <new-absent-home-path>
compound home archive-history --kind work|lease|review|comparison [--dry-run]   # --kind required to archive
compound home restore-history (--all|--id ID...)
compound home export --public-fixture [--target fixture.tar.gz]
compound home compact [--dry-run] [--rebuildable]
```

`backup` stores catalog-classified durable owned state and intentionally excludes transient or
rebuildable paths, while preserving behavior-bearing exceptions even when their historical parent is named
`projections`. `compact --rebuildable` consults the same classification and cannot remove those exceptions.
`restore` consumes one pinned archive handle into a private sibling staging directory, verifies checksums,
home health, and private custody before atomically publishing to an absent target name on Darwin or Linux;
an existing target is never reused or replaced, and other platforms fail closed before publication. Restore
accepts backup archives only (never public fixtures), completes preflight before allocating staging, and on a
post-staging failure reports and retains the exact empty `0700` staging directory for explicit cleanup. Fresh
home roots are `0700`; an older permissive root is diagnosed by doctor and changes only through the explicit
`compound home private-custody repair [--dry-run]` path.
`export --public-fixture` emits only sanitized layout/manifest
metadata, not private events, ledgers, context packs, or work records.
`archive-history` moves terminal operational history out of hot surfaces into
`COMPOUND_HOME/archives/operational/` with an append-only index, without deleting
it. It archives completed/failed/cancelled WorkEnvelopes, stale
`work/leases/*.json` entries whose work is terminal/archived/missing, terminal
owner-review artifacts and sidecars, and judged comparison candidates. Full
`home doctor` and lock-held archive admission share the same structural health
predicate: manifest, scaffold directories, and habitat inventory. Archive
admission does not run informational diagnostics such as private-artifact
hashing; the full doctor still reports them, and they remain outside its
existing health verdict. `home
doctor` reports operational hygiene (`hygiene:no-terminal-work-hot`,
`hygiene:no-stale-leases`) without making hygiene dirty state fatal, because
`archive-history` is the repair path. The read-only hygiene scan indexes
RunAttempt liveness once per pass, then evaluates the same strict heartbeat-or-created-at
freshness predicate for each lease; cost is linear in RunAttempts plus leases rather
than their product. The mutating archive selector deliberately retains a fresh
per-lease liveness probe because RunAttempt heartbeats use separate RunAttempt/work
locks and may advance while the archive holds autonomy admission. Selection is
explicit: an `archive-history`
that would mutate requires `--kind`, and a bare invocation prints usage and exits
2 instead of sweeping every kind — probing a mutating command for its usage must
not be the same keystrokes as running it. `--dry-run` still previews the full
sweep without moving anything, which is how a caller picks the kind. `workbench`
and `reviews` exclude archived entries by default; `--include-archived` restores
query visibility, and `restore-history` moves selected entries back to original
hot paths.
# Reference evidence at close

An offered ContextPack ref and a transcript reference are separate observations. Automatic
SessionEnd and `work close --transcript` scans record exact/content-key matches in
`Receipt.skills.referenced`, with `observation_method=transcript_reference_scan` and
`observation_quality=readable|unobservable`. A readable transcript with no match leaves
application unknown. Mention, quotation, and refutation cannot populate `skills.used` or
cool a claim. Actual skill tool calls and explicit `--used` declarations remain use evidence.
An injected ref contributes negative evidence only when `skills.explicit_non_use` names that
exact ref and `skills.non_use_evidence` cites provenance. Historical `used_declared` receipts
are preserved as recorded; their missing refs do not imply non-use.

### Proof contention observations

The work owner records a bounded advisory history at
`work/proof-resource-timings.json`; `proof-check --timings` consumes it alongside
the existing per-gate distribution. Admission time includes lifecycle preparation
and waiting. Queue wait counts time in the lease wait loop, separately from time
waiting for the lifecycle admission lock. Only successfully admitted runs enter
this history; cancellations and timed-out waiters are not sampled.

The observed reserved interval begins when admission returns successfully and ends
after the lease confirms release. It includes finalization and retained-completion
recovery delay, excludes reservation preparation, and is not an exact lock-hold
measurement. Missing admission or release observations stay unknown. Recovery
updates the same reservation row without extending an already recorded interval.
Repository filtering uses the sealed source fingerprint, so worktrees of one
repository share observations. The reader creates no state.

This history stays outside the sealed report and verification chain. Writes use
a separate nonblocking telemetry lock; failure reports a warning and preserves the
actual admission, gate verdict, and release outcome. The observations do not change
proof or landing exclusivity, cache acceptance, or scheduling.

### Scoped proof and landing resources

One-way resource migration and orphan retirement require exact one-use operator approval,
even when a broad standing grant covers home writes or the actor is an operator. The
adapter preserves policy denies and binds approval to the full current policy, including grants.

Git remains the source-control authority. A governed cutover separates one canonical
proof-capacity lease from landing leases keyed by the WorkEnvelope's validated
repository fingerprint and canonical target ref. Linked worktrees share a landing
slot; different repositories or refs can land independently. Proof capacity remains one per shared backing
root until the explicit two-class cutover below, including across homes that bind that same root.
Gate subprocesses retain the inherited kernel execution barrier. TTL expiry does
not authorize overlap with surviving descendants.

`landlease.Resource` selects the legacy or scoped layout on every operation.
Before cutover, the existing single lease still applies. The migration publishes
an object-valued holder at the old lease path only after rejecting every legacy
lease record (even expired), held execution barrier, pending proof completion or
recovery journal, active proof RunAttempt, and unresolved reservation. Old binaries
cannot decode that marker as a Lease, so they refuse instead of acquiring a second
coordination domain. Selection holds the old layout flock briefly; scoped lease
transactions hold only their own resource lock. Finalizers must not recursively
invoke methods on that same resource.

`compound land-lease migrate --work W --actor A --operator operator:ID
--evidence REF --writers-paused --expires RFC3339 --backing-home P` requires a
current, at-most-five-minute all-writer pause with real operator provenance.
The backing home is required and must be the same canonical home for every
participating client. Migration is a security/irreversible action: its held
ActionIntent is reviewed through `policy action-plan` and `policy action-approve`.
Retry the same arguments after approval, within the pause window. Approval binds
the complete WorkEnvelope generation, canonical backing root, operator, evidence,
and expiry; any drift requires a new approval. Landing steals also bind the
observed durable lease image and compare it under the resource lock. Nanosecond timestamps distinguish subsecond reacquisitions. Allowed proposals
persist before mutation; result events distinguish success, failure, and a
missing outcome after a crash. Failed transitions do not enter performed history. A durable preparation at
`work/resource-leases/migration-preparation.json` bridges marker publication
and outcome/Work writes. An identical retry reconciles only that exact published
marker and its persisted action/decision, even after the pause expires; it cannot
publish again or overwrite a newer Work generation. Keep the work and its owner fixed until reconciliation finishes. An interruption before marker
publication requires a fresh approval. Completed preparation remains as cutover
evidence.

A failed proof-completion journal left by a departed Work has a separate, narrow
recovery. `land-lease orphan-plan` reads the exact historical journal,
active or archived predecessor Work, and terminal RunAttempt bytes; it does
not certify the failed result.
`orphan-apply` requires an active recovery Work, exact operator approval, and
a current all-writer pause. It archives the journal bytes before removing the
active journal, records an explicitly uncertified retirement decision, and
retains a migration-blocking pending marker until the receipt converges.
Replay verifies the exact archive, absent active journal, and full decision
before clearing that marker. Applying a plan never edits the predecessor Work.
Before resource cutover, use `orphan-plan` and `orphan-apply` for a stranded
failed journal; never remove that journal by hand.

Upgrade and quiesce every participating writer/home first; mixed old/new clients
are unsupported. The shared root is owned at `work/resource-leases` under the
chosen backing home, not a cwd or arbitrary cache. An unmigrated external home is
outside this fence. Cutover is explicit; opening a new binary never migrates state.

After cutover, landing mutations require `--work` and `--actor`. Work validates
the current work lease, delivery binding, and policy before changing the resource.
The holder includes the exact work ID as a colon-delimited component. `status
--work W` selects that landing slot, while `status --proof` reports proof capacity.
The fresh-main-SHA guard and exact candidate proof remain required: a landing can now
advance main while another candidate is being tested. Any replay or rebase that changes candidate H spends the prior proof: the presubmit reruns before landing and the canonical proof is for the new landed tip.
A pure fast-forward of that exact proven tip needs no duplicate battery. Scheduled
full runs add evidence only when their actual results have been inspected; scheduling
alone proves neither execution nor correctness.


### Native remote canonical Go gate

The source implementation supports `proof-check --remote-go-test ROUTE_FILE` only
for the exact canonical general-release Go gate. The private route is sealed
before scarce-resource admission: host, absolute runner path, runner digest,
Go version, timeout and config bytes are part of the native profile. Its source
B/H/tree/repository identity, complete ceremony input manifest and exact gate
command are also sealed. Other gates retain their existing routes. No transport
error starts a local heavy-test fallback.

The producer bundles named HEAD and snapshots every input owned by
`ProofCeremonyOwnedIgnoredInputs`. Sorted roots and entries, parent topology,
file hashes/modes and in-root symlinks are validated before transport. A declared
root cannot contain a Git metadata component or be a symlink. Archived Git
metadata strictly below an owned input root remains opaque fixture bytes, as in
the local ceremony; unowned metadata, source shadowing and escaping paths reject.
The receiver validates exact input ownership before staging Git source.

Remote execution requires explicit active proof-domain v0.3. The migration CLI
accepts `--proof-target-revision compound.proof_domain.v0.3`; omission retains
the existing v0.2 default. The exact target is bound into preview, approval,
preparation and outcome recovery. Installation alone does not perform cutover;
the existing owner pause, approval and all-participant census still apply.

Before SSH, the native owner persists the same exact request fence at the class
slot and repository barrier. A disconnect, timeout, unknown response or cleanup
failure keeps those fences closed. A terminal receipt must prove exact request,
source, gate, inputs, runner and toolchain identity, plus clean quiescence.
Request and raw receipt bytes enter immutable native artifact custody. Consumers
revalidate that custody. Remote reports use v0.7, so old v0.6 readers reject the
report instead of silently dropping remote evidence. Local reports remain v0.6.

`proof-check --remote-reconcile RESERVATION --work WORK --actor ACTOR [--cancel]`
uses a fresh governed reconciliation attempt and only inspects or cancels the
original request; it cannot donate a green proof. With a failed prepared native
completion, it resumes the existing finalizer and returns `failed_proof_released`.
If the caller died and its attempt is terminal failed/abandoned before any
completion journal existed, it returns `waiting_for_expiry` (or
`ready_for_fresh_proof`) after exact remote cleanup and local termination
attestation. This disposition includes the exact `eligible_after` and original
native `next_command` invocation identity (`argv-json:` in the CLI), with
`resource_released=false`. Preserve that timed trigger; an ordinary new proof
reservation after expiry must rerun the canonical proof. The predecessor remains
terminal and uncertified. A changed owner/holder or live local descendants refuses
recovery. A resource takeover before expiry still needs the existing separate
takeover authority.

These are source capabilities and synthetic acceptance results. A deployed route,
current runner digest, real remote canonical run, interruption recovery, cleanup
and timings require their own inspected runtime evidence. No duration or nightly
execution claim follows from configuration alone.

### Source-derived proof capacities and conflict FIFO

The original v0.2 proof-domain cutover retains one `system_full` and one
`native_consumer` slot with its original same-class FIFO. A separate explicit,
quiescent v0.3 upgrade enables two `system_full` slots and one
`native_consumer` slot in the same shared backing root. Core and Rust inventories use a system slot. Native consumer classification requires the complete ordered
canonical profile rederived from its clean sealed source, including authenticated
redaction review when present. Neither a caller flag nor a plan name selects the
cheaper slot. Both classes retain `canonical_full` certification semantics.

Coordinated reservations use `compound.work_proof_reservation.v0.3` and require
immutable generation, class, repository fingerprint and repository barrier
identity. Historical v0.2 reservations remain on the historical singleton route;
missing identity is never filled from current configuration. Heartbeat, completion,
rearm, termination, recovery, stale sweep and doctor resolve persisted identity.

Slot 0 of each class is the original class root, and its identity keeps the
`compound.proof_coordination.v0.1` bytes. Slot 1 and higher are sibling roots
(`<class>-slot-<k>`) whose identity carries `slot` under
`compound.proof_coordination.v0.2`; a schema bound of 7 slots is independent of
capacity, so capacity can shrink without making old reservations unreadable.
Queue tickets are slot-less; the turn chooses the slot. A repository's existing
record pins it to that exact slot for recovery; otherwise the lowest free slot,
then the lowest expired slot, then (audited steal only) a held slot is used.
The v0.3 domain manifest rejects older clients before admission; slot identity
alone cannot fence an old client that never reads slot 1 after a crash. The
generation remains stable, so historical records retain their original routes.
New clients keep v0.2 single-slot operation for bootstrap, refuse any nonzero
lease under v0.2, and require v0.3 for additional slots or remote preparation.
`MigrateProofDomain` defaults to v0.2; only an explicit `ProofDomainFencedV3`
target invokes the existing owner pause/census validation and prepared/active
cutover. Every slot must be released, including expired records; live local
descendants and unresolved remote markers refuse the upgrade. No downgrade
is supported.

A descriptor-backed FIFO ticket precedes every immediate or waiting admission.
An earlier live ticket blocks successors sharing its repository, and successors
sharing its class unless that earlier ticket's own repository is occupied (its
repository barrier is held, or any class slot keeps a held or expired lease for
it). Such a ticket waits for its repository, not for capacity, so it does not
hold a free slot idle; tickets that wait for capacity keep strict FIFO order.
The occupancy check reads lease images without a class lock and fails closed on
any read error. The independent other class and repository can progress. Cancellation and deadline
checks occur after lock acquisition and before reservation or RunAttempt creation.
Expired/dead tickets are pruned, and failed preparation releases its ticket.
A capacity lease and repository barrier are both inherited by gate descendants.
For v0.3, the landlease remote-execution primitive also durably prepares an
exact request ID/digest on both barriers before transport may start. The
repository marker is prepared first and resolved last, so interruption remains
blocking. Local process exit never clears a remote marker. The work owner must
authenticate an exact terminal receipt proving remote quiescence or a durable
cancel-before-start tombstone before resolving it; terminal markers refuse
delayed start. Release, expiry recovery, takeover, historical attestation and
new barrier acquisition share this predicate. These primitives do not implement
a remote runner or grant remote authority; the work-owned adapter must supply
request/receipt validation before activation.
The same repository remains exclusive across slots, classes and homes, including
an expired other-class holder until exact recovery. An expired predecessor whose
lineage is absent from the requesting home is preserved for its original owner's
recovery before any successor attempt or lease is created.

The lock order is work admission, queue/domain admission, home layout selection,
class lease, capacity barrier; repository barriers are nonblocking. Heartbeats and
finalization do not reenter the domain lock. The admission callback drains any
already-started acquisition before releasing the domain, so an escaping goroutine
cannot acquire after its ticket turn ends.

The migration command adds `--proof-classes` and repeated
`--participating-home PATH` to `land-lease migrate`. Every participating home must
already be scoped to the same `--backing-home`, have the new binary installed and
be included in a real all-writer pause. Census covers every listed home's pending
proof completion/recovery/retirement journals and proof attempts/reservations.
A pause assertion is evidence supplied by the operator, not inferred from task
authorization or an empty lease. An unlisted independent legacy home is outside
the fence; the operator must establish exhaustive enrollment.

The cutover fences the exact shared historical `canonical-full` lease endpoint
with an object-valued holder before publishing its prepared manifest. Thus old
scoped clients, including omitted scoped homes, cannot acquire a parallel slot.
An interruption before the manifest still leaves a detectable fence. Prepared
state refuses typed admission, and active state requires the matching permanent
fence. New clients from unenrolled homes refuse. Neither startup nor installation
activates the cutover.

The distinct `work/resource-leases/proof-domain-preparation.json` journal binds
exact Work generation, enrollment, action, consumed one-use approval and policy
revision. A scoped legacy marker cannot certify this cutover. Prepared recovery
rechecks current pause, approval expiry, policy revision, stop signals and census
before activation. Expired/changed pause or approval requires fresh exact approval;
a changed policy requires a newly approved action. Physical completion may be
reconciled after approval expiry, but never overwrites newer Work authority.


A completed v0.2 preparation remains historical evidence when a distinct Work requests the
explicit v0.3 upgrade. The native migration owner authenticates the stored action, consumed
approval, completed predecessor Work with its retained action, exact completion event, and
current v0.2 domain/fence/enrollment before preparing fresh approval. That new action binds
the exact predecessor-body artifact reference as well as its own current Work and pause.
Only after fresh approval does the owner preserve the exact old preparation in private
immutable artifact custody and replace the preparation with the v0.3 successor. It never
changes the completed predecessor Work. Missing, corrupt, unfinished, or mismatched history
refuses unchanged. An interruption after custody but before successor preparation leaves the
old record intact; consumed approval cannot replay, so recovery needs fresh pause evidence
and approval. Prepared successors retain their history reference through expiry/reapproval:
a held or denied replacement never erases the pending record. After the new marker progresses,
recovery authenticates the custodied history without demanding the obsolete v0.2 marker.

`land-lease status --proof` reports generation and both resources after activation.
Doctor reports each capacity separately; historical output stays unchanged before
cutover. This removes the single-slot conflict between independent native and
system proofs. It does not remove global work-admission costs, shorten full system
gates, or establish an aggregate duration bound from individual gate timeouts.

The RunAttempt census remains a complete fresh read. Prepared reverse-only intents
can exist without a per-work frame, so an absent frame or stale index cannot prove
that a work item has no pending terminalization. Optimization must retain complete
validation and corruption reporting; a new index is justified only by measured
consumer cost and an explicit writer cutover.

## Existing backup credential handoff

The act backup-credential-stage command is a separate typed native owner. Its
closed request admits exactly four existing backup files into one root-private
recovery directory; it cannot issue credentials, install configuration, start
services, retire source markers or accept a migration. Planning inspects custody
metadata only. The one-use exact approval binds device/inode, owner, mode,
single-link, size and modification/change times for the directory and files,
plus host and transport/code pins. Source content hashes are optional input
pins; the native guard derives and binds actual hashes after admission and
before transfer.

The owner uses native Work generation, receipted-action reservation, honest
actor/compound RunAttempt, launch evidence and completion proof. Credentials
cross only bounded stdin; stdout/receipts contain metadata. Linux publication
uses create-only atomic rename. Its immutable request journal supports a distinct
native recovery Work after uncertain transport outcomes; recovery may complete
a verified interrupted prefix but cannot overwrite published files. Exact retry
does not re-enter transport. Generic forbidden-path evaluation is unchanged;
custom, overlapping and duplicate protected rules still deny the typed owner.

## Source-comparison marker retirement

The act source-marker-retirement command is a distinct metadata-only native owner.
It accepts a source marker rename only after reading genuine accepted native
runtime installation proof: the committed succeeded compound RunAttempt, its
owned receipt/transition, consumed allowing policy, typed installed-inactive
observation, and exact four-file postimages. A caller-created proof or generic
protected-file action cannot substitute for that chain.

Its one-use request pins host, SSH/code, marker, inactive anchor and accepted
runtime metadata. A fixed Docker census and read-only local PostgreSQL probe
require absent application containers and a closed database with zero clients.
Each child timeout is capped by the remaining wire deadline. The complete
runtime fence rechecks that deadline before returning to filesystem effects.
The native program never opens environment bodies. Linux create-only rename
preserves the source marker inode in root-private recovery; the four runtime
files and inactive anchor remain unchanged. Native Work generation, launch and
terminal-owner proofs bound every effect. A lost response requires a separate
reconcile Work; there is no configuration rewrite, rollback or activation effect.
Generic forbidden paths and custom or overlapping denials remain enforced.

Executable pins are full-file SHA-256 values, streamed in 1 MiB chunks with a
256 MiB per-executable ceiling. Oversize files are refused; a truncated prefix is
never treated as the executable. Reconciliation can retain the exact historical
request program pin for the known pre-streaming program, but new retirements
must pin the current program. Every new recovery separately binds the current
transport program in its full ActionIntent evidence and one-use approval, while
preserving the original request bytes, request digest, reserved recovery Work and
recovery arguments hash. Historical immutable reconcile plans may omit only that
transport field for this exact legacy lineage; they remain readable and cannot
launch current transport. Missing fields in fresh plans remain errors.


Native collaboration recipients validate the exact ActionIntent body, policy body identity,
lease work/actor, and agreement between the attempt, selection snapshot, and query harness.
A same-state authority replacement invalidates the old recipient. Generic
pinned:<flag> evidence retains the execution-selection schema's meaning.

Only the Stop fallback consults runattempt.ListByNativeSessionID: it probes
main-record session IDs before loading matching launch sidecars and preserves
unreadable-record errors. Terminal attempts remain known even when their work
is archived or the reservation-loss path has no envelope forward-link.
Ordinary tool hooks continue to inspect nonterminal attempts only.
