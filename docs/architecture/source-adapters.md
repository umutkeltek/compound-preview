# Source Adapters And Generated Views

Source adapters let a running Compound instance learn from existing systems without
turning those systems into permanent peer authorities.

The hard case is generated files: agent instructions, dashboard JSON, rendered
registries, hook configs, skill manifests, and other read models. They are useful
evidence, but they are not canon by themselves.

```text
external source or generated view
  -> SourceObservation
  -> EvidenceItem / Candidate / ProjectionFreshness
  -> AdjudicationDecision
  -> ledger object or NoPromoteDecision
  -> optional OwnerWritebackGate
```

Product canonical metrics records enter through `compound product canonical-metrics <json> --product <id>`. The adapter treats canonical latest ideas as source observations for the product loop: numeric idea evidence becomes neutral snapshot ProductSignals, the idea becomes a parked ProductHypothesis if absent, and the next action remains a governed review artifact. It does not call analytics providers, infer causality, mutate product surfaces, or promote claims.

## Rule

```text
Adapters observe. They do not promote.
Generated views prove freshness or drift. They do not prove truth.
Writeback requires an OwnerWritebackGate plus PolicyDecision.
```

This is the difference between using an existing operating system as a dogfood
source and rebuilding its fragmentation inside Compound.

## Source Connections State

`compound source connections --json` exposes two state axes per connection:

| Axis | Values | Meaning |
|---|---|---|
| `availability` | `unknown`, `not_configured`, `needs_private_config`, `configured`, `healthy`, `failing` | Whether Compound can reach or preflight the source. `needs_private_config` means operator/private setup is required; `configured` means setup is known enough to run a read-only preflight; `healthy` means live proof exists. `failing` is reserved for connector probe failures; no live connector probe emits it yet. |
| `evidence_state` | `never_seen`, `observed`, `imported`, `stale`, `has_cards` | Whether source evidence has entered Compound. `observed` is runtime/source-row evidence, `imported` is imported knowledge evidence, `stale` is evidence past the freshness window, and `has_cards` means pending source action exists before seen data. |

The flat per-connection `status` field is a compatibility bridge. Console and
CLI consumers read `availability` and `evidence_state`; remove `status` only
after external consumers confirm migration.

Conversation coverage summaries are source-quality evidence, not corpus imports.
`compound distiller import-conversation-coverage <summary.json>` is summary-only:
it records source counts, mapped/unmapped coverage, retained-ratio signal,
Dream-candidate counts, missing embeddings, and top-tag count as
`SourceObservation -> method_upgrade candidate -> NoPromoteDecision`, then prints
the reviewed next step `compound distiller improvement-plan --candidate <id>`.
It has no row import, no Dream execution, no memory promotion, no source-owner
mutation, and no raw conversation/body ingestion.

Session distill `multi-lens.json` outputs are already-run extraction evidence,
not truth. `compound distiller import-session-lenses <multi-lens.json>` records
only six-lens counts and the source digest as
`SourceObservation -> method_upgrade candidate -> NoPromoteDecision`, then prints
the reviewed next step. It imports no rows and promotes no memory.

## SourceObservation

Every adapter emits observations with enough metadata to answer: what was read,
what kind of surface it was, who owns it, what it was generated from, what changed,
and what Compound object family it may affect.

Minimum shape:

```json
{
  "schema_version": "compound.source_observation.v0.1",
  "id": "srcobs_...",
  "adapter_id": "operator-os.generated-views",
  "source_system": "operator-os",
  "observed_at": "2026-06-29T00:00:00Z",
  "source_ref": {
    "path": "operator-os/spine/views/skill-manifest.json",
    "kind": "generated_view",
    "owner": "spine",
    "source_class": "generated",
    "revision": "sha256:..."
  },
  "generated_view": {
    "view_kind": "skill_manifest",
    "consumer": "agent harnesses",
    "generated_from": [
      {
        "path": "operator-os/spine/skill-manifest.ts",
        "revision": "sha256:..."
      }
    ],
    "generator": "bun operator-os/spine/generate.ts",
    "marker": "generated",
    "freshness": "fresh",
    "drift_reason": null
  },
  "maps_to": [
    {
      "object_family": "capability",
      "candidate_kind": "capability_observation",
      "entity_hint": "skill-manifest",
      "promote_allowed": false
    }
  ],
  "evidence_refs": ["artifact_sha256_..."]
}
```

`promote_allowed: false` is intentional for generated views. A generated file can
support a candidate or freshness verdict, but the promotion basis must come from a
source owner, decision record, runtime proof, explicit confirmation, or trusted
deterministic import.

## GeneratedViewObservation

Generated views get one extra interpretation layer:

```text
view exists
view has marker
view was built from declared sources
view hash matches current generator output
view consumer is known
view is fresh | stale | drifted | unknown
view maps to one object family
```

The output is `ProjectionFreshness`, not canon. If a dashboard says a service is
healthy, Compound records that the dashboard projection said so at a time and
revision. `projection doctor --record-context` and `projection dashboard-read-model
--record-context` can persist that freshness under
`COMPOUND_HOME/projections/context/`; `orient` may include it as a generated
`projection://...` ref for the declared repo kind. Runtime truth still needs a
runtime source or ServiceContract evidence.

## Adapter Port

The public seam should stay small:

```text
DiscoverSources(root, config) -> []SourceRef
Observe(ref) -> SourceObservation
Map(obs) -> CandidateSet + ProjectionFreshness
Explain(obs) -> authority chain
```

Private adapters can carry local paths and account-specific logic. Public adapters
must use synthetic fixtures and relative paths.

## Operating-System Dogfood Mapping

An existing operating system should enter Compound through observations, not file
copies. The mapping is by surface *kind*, not by any specific repo layout:

| Operating-system surface | Compound interpretation |
|---|---|
| generated spine views (`spine/views/*`) | `GeneratedViewObservation` + `ProjectionFreshness`; optional `--record-context` makes freshness visible to scoped `ContextPack` without running generators |
| dashboards / read models | `ProjectionFreshness` through dashboard read-model adapter; card/source coverage evidence, optional scoped context ref, never authority |
| event streams (`events/*.ndjson`) | `TraceEnvelope` / event evidence |
| conversation-mining reports / mined tag rows | `ConversationMiningAdapter` -> `SourceObservation -> method_upgrade candidate -> NoPromoteDecision`; preserves processed/accepted/rejected counts, provider mix, accepted-for-review rows, and lossiness/known lossy corpus rules. `compound distiller import-conversation-mining` prints `compound distiller improvement-plan --candidate <id>` as the reviewed next step. It does not execute Dream, does not promote memory, and does not mutate source owners; facts still require distiller extraction plus adjudication or reviewed write-plan. |
| parked memory candidates -> curated knowledge markdown | `MeaningWritePlan` review artifacts under `COMPOUND_HOME`; no direct source mutation or memory promotion |
| append-only ledgers (`ledger/*.ndjson`) | receipt / route / cost evidence; provider-call rows can become metadata-only `ExecutionRouteEvidence`, and cost-coverage gaps can become parked `route_observation` candidates plus no-promote decisions |
| contract files | source material for contract candidates |
| source-control snapshots and retirement | `SourceAuthority` status claims via read-only git inspection; branch/head/ahead/behind plus a dirty-state fingerprint over porcelain-v2 status/paths, complete index stages/blob ids, and changed working bytes, with no cleanup/staging/push authority. `PreToolUse` canonicalizes relative, symlinked, and future action targets, holds dirty-repo mutations, and may consume only an exact recorded batch capability. `authority dirty-plan` without action ids remains cleanup review only; with repeated `--action-intent` ids it loads persisted actions, derives their shared executable identity and complete target set, and emits an immutable mutation-bound plan. `dirty-review ... --reviewed-by operator:<actor>` is mandatory for `authorize_mutation`; the canonical reviewed YAML source hash and content digest over the recorded authorization are checked again at consumption, so edited reports/sources fail closed. Owner WorkID is provenance; a real non-`work_live_*` action WorkID must equal it, while a synthetic live action remains explicitly operator-linked. The dirty-state adapter never mutates the repo or promotes claims. Redundant linked worktrees/local branches retire only through `authority worktree-retire plan|review|apply`: the plan seals exact repo/worktree/ref/authority/publication, a proof-complete `completed` or `failed` source-work chain whose Receipt outcome exactly matches that terminal state, a current landed land-check, running cleanup lease, liveness, lineage and content-addressed recovery evidence; `cancelled`, nonterminal, and state/outcome-mismatched source work refuse. A failed source authorizes only removal of a proven-redundant workspace surface and remains failed with its original Receipt and Transition. Operator review binds the immutable plan; apply revalidates every live observation, persists mutation-started/completed phases, uses non-force worktree removal and expected-tip CAS only, and records the governed ActionIntent/PolicyDecision/Receipt/Transition under the cleanup work. Missing, dirty, locked, live, unique, stale or ambiguous state refuses without metadata repair or manual fallback. |
| registries | `RegistryEntry` candidates |
| capability evaluation records | `CapabilityEvaluationAdapter` -> `CapabilityGraph` evidence-only eval nodes + `evaluated_by` edges to already-known capability nodes; positive useful/keep/adapt/merge/publicize verdicts also add `validated_by` evidence edges; imported evaluation source records `SourceObservation -> capability_observation candidate -> NoPromoteDecision`; unknown capabilities are skipped rather than installed and make the import report `status: review` with `skipped_unknown_count`; `capability evaluation-plan <eval.json|dir> --capability <id-or-name>` writes review artifacts plus a normalized `needs_review` skill-manifest draft for one unknown evaluated capability under `COMPOUND_HOME/reviews/capability-evaluation-intake/`; `capability evaluation-review <review.yaml>` validates an edited review decision and writes a reviewed manifest without mutating graph/source owners; records never retire/execute/surface/promote claims |
| skill surfacing miss reports | `SkillSurfacingMissAdapter` -> `CapabilityGraph` `missed_when` edges for already-known capability nodes using receipt/reason context; imported report source also records `SourceObservation -> capability_observation candidate -> NoPromoteDecision`; unknown skills are review debt, never installed |
| tool resolver / MCP catalogs | `ToolTopologyAdapter` -> `CapabilityGraph` tool/mcp/adapter nodes + fallback edges from normalized topology YAML, generated resolver JSON, or `mcporter list --json` catalog output; imported topology source also records `SourceObservation -> capability_observation candidate -> NoPromoteDecision` plus a capability source-authority handoff: Compound graph authoritative, source view evidence-only rollback input |
| skill manifests / external skill catalogs | `SkillManifestAdapter` -> `CapabilityGraph` skill/lens/procedure nodes + graph edges from normalized manifests or generated skill-manifest JSON; imported manifest source also records `SourceObservation -> capability_observation candidate -> NoPromoteDecision` plus a capability source-authority handoff: Compound graph authoritative, source view evidence-only rollback input; offers only, never execution |
| work packets | `work import-packet` -> deterministic source-backed `WorkEnvelope` keyed by packet hash through the same create/admission policy gate as `work create`; packet remains evidence, no source mutation, no receipt/transition until actual close. Packet-sourced work can be imported as `ready` or advanced through the normal lifecycle, then close emits the same-`work_id` Receipt + Transition proof chain |
| runtime / service contracts | `RuntimeBinding` / `ServiceContract` candidates; failing doctor reports can emit gated runtime repair-plan artifacts without owner mutation |
| product project/release contracts | `ProductContractAdapter` -> existing `Product` registry bundle; deterministic identity/surface/channel/privacy claims only, never analytics signals or causality |
| decision records | `DecisionLifecycle` candidates; generic narrative note imports (`decisions import --source <md>` / `--source-dir <dir> --source-kind note`) record note evidence, derive stable ids from page metadata/heading when needed, extract explicit verdict/review-trigger text, park incomplete lifecycle proof, and emit reviewable lifecycle repair artifacts through the same repair-plan path; compatibility `--brain <md>` preserves compatibility source semantics without making brain a second authority. Policy markdown source import records source evidence, supports single-file and directory batch import through the same adapter, preserves supersedes/replaced_by lifecycle transitions, gives each decision a stable entity, parks incomplete or non-accepted decisions, verifies source templates/docs contain lifecycle fields, emits reviewable lifecycle repair artifacts, applies lifecycle fields only when the repair plan status is source-owner reviewed, and records source-authority coverage that `decisions authority ...` can audit |
| knowledge-base pages | `compound memory import-page <markdown>` reads explicit `compound.knowledge_page.v0.1` `claims[]`, records one `EvidenceItem`, writes deterministic proposed memory candidates, and creates no promoted claims. Claims must still pass existing memory adjudication or reviewed `MeaningWritePlan`; page prose is not inferred into truth. |
| product analytics exports | `ProductSignalExport`, App Store Connect datapoints snapshot rows, and explicit-window App Store Connect sales-series rows -> `ProductSignal` evidence + product `Transition` projection, never causality; ASC datapoints are neutral provider observations (before=after), while ASC sales-series requires explicit before/after ranges and a change event; `tools/asc_daily_signals.ts` is a rented edge that normalizes daily ASC app metrics into the same `ProductSignalExport` door, and `product doctor` warns when a registered app-store product has no ASC signal or its latest ASC signal is older than 48h; mined `ProductHypothesis` candidates can become reviewed hypothesis-plan artifacts before any strategy/surface action; `hypothesis-plan` also emits a gated ProductSignal draft that is invalid until real provider/surface/before/after evidence is filled; after a ProductSignal is imported, `product doctor` can report signal-backed `review_ready` pairs and the exact review-plan command, while `product review-plan --hypothesis <id> --signal <id>` creates gated review artifacts that cite signal evidence but still require explicit reviewer result before `product review` updates the hypothesis; unknown/unsupported source fields fail strict doctor and may record `NoPromoteDecision`; source-issue review records closure evidence while preserving the original no-promote row, and reviewed `resolved` follow-up can create a parked ProductHypothesis candidate without creating ProductSignals or claims; product timeline reads valid signals plus open/closed product-scoped source issues together |
| a retrieval / embedding store | retrieval engine or source index, never authority |
| ActivityWatch events | `internal/activity/awstream` reader joins `aw-watcher-{window,afk,vscode}_*` events across hostnames into Tier-0 `activity.Event` (kind=`app_activated`/`idle_started`); `internal/activity/tier1/source` registry reads cmux IPC + iTerm2/Warp/Terminal.app via `lsof` + VS Code/Cursor from AW `project` only and emits Tier-1 envelope events (kind=`window_focused`) with `pane_cwd`/`git_remote`/`agent_kind` meta. Two CLI verbs: `compound activity aw materialize` (Tier-0 ingest, `activity.ingest.drain` grant) and `compound activity tier1 collect` (Tier-1 envelopes, `activity.tier1.collect` grant). AW remains a passive reader; `aw-watcher-vscode_*.file` and any other title/url/document field are refused at the read boundary. See [ADR 0003](../adr/0003-activity-watch-virtual-stream-and-tier1.md) |

Daily ASC metric intake preserves the reported measurement scope. Only an explicit
`developer_proceeds_usd` field can produce that USD-total metric; subscription or
renewal proceeds are subsets with unspecified currency and cannot substitute for it,
including when their value is zero. A generic `proceeds` metric retains its generic
name. Missing total/currency evidence produces no USD-total signal. Existing historical
signals remain evidence for review; this change does not rewrite their history.

Change-stream promotion candidate pages are product source observations, not
product truth. `compound product change-stream <candidates.md>` preserves the
generated page hash/capture and each candidate's product, surface, repo, score,
and dirty preview as product-scoped source issues backed by `NoPromoteDecision`;
review/closure still goes through `product source-issue-review`, and durable
changes/signals/hypotheses require their own evidence path.
`compound product source-issue-plan --product <id> --issue <id> [--target <file>]`
turns one open source issue into JSON/Markdown review artifacts plus a gated
`.review.yaml` draft. The draft preserves original evidence but keeps an
invalid placeholder result until a reviewer edits it, so the next
`product source-issue-review` step remains explicit and evidence-backed. A
reviewed `follow_up` block is allowed only with `result: resolved` and becomes a
parked ProductHypothesis candidate; it is not a signal, claim, or product-surface
mutation.

A concrete mapping for a specific operating system — real repo paths and named
tools — belongs in that operator's private notes (`docs_private/`), never in
this public doc. Public adapters use synthetic fixtures and relative paths.

Harness capability registries follow the same source-observation rule:
`compound harness observe [registry.yaml|--os-entrypoints file]` records built-in,
normalized external, or OS-style harness entrypoint support and limitations as
`SourceObservation -> capability_observation candidate -> NoPromoteDecision`. OS-style
entrypoint imports conservatively infer only `session_start_context` from entrypoint
presence and preserve generator/check/status as limitations. The adapter does not
install hooks, mutate CapabilityGraph nodes, or make registry defaults authority.

Source-control review freshness is resolved at projection time with another read-only snapshot. An
exact dirty snapshot remains reviewable; a changed dirty snapshot or unreadable repository is
blocked without authority; a clean repository files the old dirty plan as resolved historical
evidence. The original plan and sidecars remain intact in every case.

Unique branch retirement may use `candidate_relation=archived_unique` only
when the ordinary contained/patch-equivalent checks fail and a byte-verified
Git bundle recovery proof lists the exact candidate ref at the exact candidate
tip. The plan/review/apply ceremony, liveness checks, main/publication SHA
guards, keep decisions, non-force worktree removal, and CAS ref deletion remain
mandatory. A generic file named `.bundle` is not enough: Compound checks the
Git bundle magic and exact `bundle list-heads` identity before eligibility.

## First Acceptance Test

The first implementation should be deliberately narrow:

```text
one generated control-plane view
 -> ProjectionFreshness
 -> SourceObservation
 -> method_upgrade candidate no_promote
 -> ProjectionRegenerationPlan review artifact
 -> source doctor reports fresh/stale/drifted

one tool resolver view
-> ToolTopologyAdapter
-> CapabilityGraph offers
-> SourceObservation
-> capability_observation candidate no_promote
-> capability source-authority handoff

one skill manifest view
-> SkillManifestAdapter
-> CapabilityGraph offers
-> SourceObservation
-> capability_observation candidate no_promote
-> capability source-authority handoff
```

This proves Compound can learn from generated files without making them new truth
owners.

Distiller improvement review YAML is a source-adapter closure surface: `compound distiller eval <case.json|cases-dir> --record` stores passing eval proof refs, and `compound distiller improvement-review <review.yaml>` validates edited decision/rationale/confidence, requires `eval_proof_refs` for accept, resolves them to recorded passing eval proofs, and emits a reviewed improvement artifact without mutating distiller policy, prompts/code, eval corpus, source owners, or memory truth.
