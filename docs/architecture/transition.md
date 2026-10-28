# The Transition Primitive + Second-Order Design

Compound's core object is not `MemoryRecord`. It is `Transition`.

A system **compounds** only when the output of today's run modifies the affordance landscape
of tomorrow's run. So the object the kernel optimizes is the state change itself:

```text
State_t  →  Action_t  →  Receipt_t  →  State_t+1
```

`State_t` = identity graph + memory graph + capability graph + tool/model/harness graph +
product/market signals + cost gradients + risk gates + taste standards + operator attention +
system trust + open loops + dead surfaces.

## External effects with initially unknown outcomes

An adapter must not reserve success before a filesystem effect has happened.
`work.ReceiptedActionInput.OutcomePending` reserves action/policy authority while
preventing receipt publication. `SealReceiptedActionOutcome` binds the actual
outcome once, without changing the action, policy, authorization time, running
work admission or preimages. Partial failures retain their changed-state evidence.
The reservation remains until Receipt, Transition and work links converge; exact
replay cannot alter the sealed outcome. Omitted false preserves existing input
digests. The index-lock recovery adapter consumes this seam.

A remote effect that cannot be observed uses `state_delta.change_knowledge:
"unknown"` and omits `changed` entirely. Neither boolean is evidence for an
unknown remote result. Completed and no-op receipts require known effects.
Known-effect serialization and omitted optional fields retain legacy bytes and
reservation digests. The schema export exposes the two mutually exclusive
representations; the native validator enforces their outcome semantics.

An adapter may freeze `RequiredAttemptCompletion` into the reservation before
launch. Receipt/Transition publication does not release that blocker: the native
attempt must also carry its same-action terminal owner proof. Exact retries
cannot omit the obligation. For a failed committed or uncertain prepared launch,
a frozen recovery requirement additionally names a separate Work, owner,
capability and exact argument bindings. The finalizer looks up a succeeded,
committed native recovery attempt and verifies its action, allowing decision,
receipt, transition, completion hashes and lifecycle event. Its proof must bind
the original action, attempt, request and failed outcome; its target must match.
An original failed owner proof alone never discharges required recovery. A
successful recovery does not rewrite the original attempt's failed terminality.
A recorded start error with no committed launch is the separate zero-launch
case. Missing or invalid recovery proof keeps the original Work blocked.

A dangerous action admitted through a consumed exact approval may reuse its
durably published ActionIntent and issued PolicyDecision only when both bind
byte-for-byte to the consumed grant. A completed action cannot reserve again:
the completion marker is checked before the authorization callback or a new
reservation can run.

A canonical owned failed report with a known nonzero producer exit may finalize
non-certifying audit evidence after its original private-source review expires.
The exact historical action, allowing policy, actor, Work, source profile and report
remain mandatory; no review timestamp, expiry, result or source Work is rewritten.
New reservations, verified reports, unknown or inconsistent outcomes and source
publication retain current authorization. A failed audit emits no VerificationID.

A retained failed proof may also be finished after its source checkout moved or
disappeared, through a recovery-only route that reads the persisted reservation's
original workspace, command and sealed profile instead of the current checkout. It
reuses the ordinary completion machinery and the existing output custody to restore
the original owned report, and it never admits a battery. The exact Work, actor,
reservation and the expected pending-journal bytes are mandatory, and the pending-byte
fence is enforced inside the guard that performs the recovery effects — before any
report, custody, rearm or attempt mutation — not only in a wrapper precheck. The
dedicated route requires an existing, valid completion governance seal and validates
the pending, attempt, lineage and holder barriers before it reaches the owned report.
A hash the recovery itself generates later (a legitimate retry reseed) is allowed. The
outcome stays a failed battery verdict with no VerificationID: the original report,
attempts and refusals are preserved. A repetition, or any request that matches no
retained completion, refuses without writing an output, recreating consumed evidence
or admitting a battery.

## Evidence of later benefit

A completed changed receipt and its transition establish a **proof-backed change**.
The console names that evidence level explicitly. Later levels need separate evidence:
**observed reuse** of an identified artifact revision; a **verified downstream outcome**
joined to that use; and **measured improvement** against a comparable baseline with
quality, failures, coordination effort, and resource costs retained. A reference or
`next_consumer` label alone does not advance the level. Missing observations stay unknown.
The current-state `cause-consumption` report is attribution visibility, not a historical
state-at-use reconstruction or a causal-effect estimate. Historical evaluation must bind
the object revision and state at use time; later promotion cannot rewrite that observation.

Branch-debt examples fold the full ordered transition history and retain its newest ten
unverifiable entries, newest first. The total remains the complete count.

## The Transition object

A `Transition` is a state change plus its **projections** into multiple representational spaces.
The anonymous `Header` field supplies the universal envelope keys and flattens them into JSON; in
particular, `ids.work_id` is the root link from intent through transition, and the transition's own
identifier is the flattened header `id` (`tr_...`). `ids.transition_id` is NOT set on the Transition
itself — close-minted transitions leave it empty; the link lives on the WorkEnvelope, whose
`ids.transition_id` the close path sets to the minted transition's id. The direct struct-field
contract is:

| Go field | Go type | JSON tag |
|---|---|---|
| `Header` | `Header` | `-` |
| `SourceTraceID` | `string` | `json:"source_trace_id,omitempty"` |
| `BeforeRefs` | `[]string` | `json:"before_refs"` |
| `ActionRefs` | `[]string` | `json:"action_refs"` |
| `AfterRefs` | `[]string` | `json:"after_refs"` |
| `StateDelta` | `string` | `json:"state_delta,omitempty"` |
| `Projections` | `Projections` | `json:"projections"` |
| `SecondOrder` | `SecondOrder` | `json:"second_order"` |
| `Cause` | `*TransitionCause` | `json:"cause,omitempty"` |
| `OpensBranches` | `[]BranchRef` | `json:"opens_branches,omitempty"` |

The Transition-specific JSON body has this shape (the flattened `Header` keys precede it in a full
envelope):

```json
{
  "source_trace_id": "trace_...",
  "before_refs": ["claim_...", "metric_..."],
  "action_refs": ["run_..."],
  "after_refs": ["ev_...", "artifact_..."],
  "state_delta": "...",
  "projections": {
    "memory": ["claim_candidate_..."],
    "skill": ["capability://skill_or_procedure_offered"],
    "product": ["hypothesis_..."],
    "cost": ["cost_record_..."],
    "risk": ["tier_..."],
    "taste": ["taste_observation_..."],
    "routing": ["route_decision_..."],
    "human_attention": ["approval_needed_..."]
  },
  "second_order": {
    "future_behavior_changed": true,
    "should_harden": false,
    "should_create_rule": false,
    "should_decay": false
  },
  "cause": {
    "delta_cause": "accept",
    "caused_by_refs": ["work_..."],
    "next_consumer": "memory-review"
  },
  "opens_branches": [
    { "branch": "branch-lifecycle", "owner": "workbench", "kind": "opens" }
  ]
}
```
`runtime.md` and `contracts/README.md` reference this canonical shape. `source_trace_id` identifies
an originating raw trace when one exists.

It forces three questions on every run: **What changed? Across which spaces? What future
behavior should this affect?**

`projections.skill` may include capability refs from two sources: used capabilities from allowed recorded ActionIntents, and `capability://...` offers from the ContextPack. Offers alone are not proof the skill executed; actual mutation still needs ActionIntent, PolicyDecision, run/artifact refs, and Receipt state delta. Close/session-end automatically feeds the emitted receipt into CapabilityGraph when graph state exists; used nodes validate only from completed changed receipts, explicit `skills.missed` refs add `missed_when` graph context, and explicit `skills.false_triggered` refs add `false_triggered_by` graph context.

## The medium spaces (projections)

Do not collapse everything into one schema. The ledger preserves evidence; the system needs
*projections*. Same event, many projections.

```text
1. Identity space         entities, aliases, owners, paths, source bindings
2. Evidence / time space  traces, receipts, sessions, before/after states
3. Semantic / vector      fuzzy similarity, discovery, analogy, broad recall  (RENTED engine)
4. Graph space            skills, tools, models, products, decisions, claims, substitutions
5. Workflow space         jobs, DAGs, gates, scripts, handoffs, retries, escalations
6. Cost / latency space   tokens, sub/API/local routing, caching, provider limits, runtime
7. Risk / policy space    reversible vs irreversible, public/private, spend/destructive
8. Taste space            examples, anti-examples, aesthetic standards, product judgment
9. Product / market space analytics, experiments, pricing, paywalls, ASO, support, distribution
10. Human-attention space synthesis debt, interruption cost, trust, focus
```

### The central invariant (this is what "coherent tensors across media" means)

> **Target: every projection preserves** `entity_id, scope, evidence_ref, time, actor,
> uncertainty, risk, cost, state_delta`.

This nine-field shape is the design target, not what the shipped schema enforces today. The
implemented `ProjectionRef` (`internal/envelope`) carries `kind`, `ref`, `role`, and optional
`evidence_ref`, `confidence`, `status`; the untyped projection lists (`memory`, `skill`, `product`,
`cost`, `risk`, `taste`, `routing`, `human_attention`) carry refs only. Coherence is not one schema.
Coherence is the ability to transform between spaces without losing those nine fields — a projection
that drops one opens a fragmentation side door, so new projection fields should close the gap toward
the target rather than drift further from it.

## Relationship to the ledger

`Transition` is a higher-order object over the existing `Evidence → Claim → Position → Pattern`
model. Projections are typed, citeable views; they do not create a second truth lifecycle.
Engines (space 3) remain rented and rebuildable. The ledger stays authority.

---

# Second-order design discipline

No field is neutral. A memory schema, a goal prompt, a skill trigger, a cost rule, a trace
format, even a naming convention changes the probability distribution of future actions. The
architecture is judged not by "does it have X?" but by **"when alive, what does each layer make
more likely tomorrow?"**

| Layer | Naïve view | Second-order reality | Primary failure to design against |
|---|---|---|---|
| Memory | storage | **gravity** — recalled claims bend future behavior | promote a weak claim → system manufactures coherence around a false attractor |
| Registries | inventory | **affordance attractors** — entries pull behavior toward them | add many skills → over-trigger → context bloat → taste flattens |
| Routing | cost optimization | **epistemic shaping** — chooses what *kind* of mind touches a problem | cheap-by-default becomes a silent worldview; quality degrades invisibly |
| Receipts | proof / audit | **training data** for the next system | agents optimize for receipt completion → bureaucracy → trust drops |
| Product feedback | analytics | **reality pushing back** | over-credit the last change → imitate the wrong pattern → damage product identity |
| Taste | preference | **policy surface** | optimize visible metrics + tests pass → competent mediocrity, "less yours" |
| Autonomy | permissions | **trust gradient** | punish risky proposals → only trivial safe moves → compounding dies |
| Harnesses | executors | **epistemic environments** | a workflow that worked in one harness silently breaks in another |

## Memory temperature (not just park/promote)

```text
ambient       safe to inject without asking
contextual    load only under matching scope/intent
candidate     visible to reviewers/distillers, not normal agents
volatile      useful now, expires unless reinforced
private       never ambient
operational   directly changes routing/tools/procedures
retired       kept for history, blocked from instruction
```

Every memory promotion is a controlled state mutation, not archival.

## Acceptance tests (a slice is "alive," not "archived," only if it survives these)

1. **Perturbation** — change one memory claim → only relevant scopes change recall; system
   explains why it mattered. (catches: ambient gravity too strong)
2. **Path-move** — move a project's folder → identity stable, old path retired binding, new path
   active. (catches: path-dependent identity fragmentation)
3. **Skill-misfire** — run a visual/UI task without the vision/taste skill → receipt detects the
   likely missing skill; future trigger probability rises. (catches: lazy-but-looks-successful)
4. **Counterfactual routing** — same job to small/frontier/script/human → explains tradeoff,
   records "routing remorse" if cheap underperforms. (catches: silent quality degradation)
5. **Product confounder** — a metric moves alongside a traffic spike → no causal claim; competing
   interpretations + review trigger. (catches: metric worship)
6. **Registry-attractor** — add an external skill → enters `candidate`, needs evidence before
   ambient triggering, does not bloat context. (catches: registry as noise factory)
7. **Control-plane tax** — a dashboard/readme/coherence change does NOT count unless it removes a
   duplicate owner, fixes a recurring failure, or creates runtime proof. (catches: map = work)
8. **Public/private contamination** — building a feature keeps generic contracts in public core
   and all personal paths/credentials/accounts in the private overlay. (catches: "private OS with
   prettier nouns")

## The evaluation rule

Every non-trivial run classifies itself: `outward_delta | memory_delta | rule_delta |
control_plane_repair | audit_only`. A receipt only matters if it becomes searchable experience
or subtractive pressure. The one question every receipt answers: **"Did this run alter the
future?"** If not, it is telemetry, not compounding.

## Future-branch semantics (second-order, operationalized)

Second-order asks "does this make X likelier tomorrow?" The operational form, learned under
execution pressure: a placed artifact compounds *nothing* if it never changes recall, routing,
action, evaluation, or pruning. Owner placement prevents fragmentation; it does not guarantee
compounding. So a `Transition`/`Receipt` declares the branch it opens and who consumes it:

- `opens_branches[]` — `{branch, owner, kind}`, kind ∈ `opens | blocks | supersedes | validates |
  retires | routes_to`. **SHIPPED (2026-07-24, work_18f8ce768471):** validated schema on
  `Transition` (`envelope.BranchRef`), the `compound work close --opens-branch slug:owner:kind`
  write door, and the consumer that un-deferred it — `internal/work` `BranchCoverage`, the
  deterministic read model over `events/tr_*.json` surfaced as `branch_debt` in
  `compound workbench`. Coverage is explicit-only: only `validates | supersedes | retires` on the
  same slug resolve a branch; a repeated `opens` is an expansion that keeps the original
  `opened_at`; covering an unopened slug is a dangling-cover integrity finding.
  Count semantics in `branch_debt.counts`: `opened`/`covered` are cumulative; `open` is the
  snapshot (`opened - covered`) — that is the debt numerator; `stale` is the subset of `open`
  past the 7-day window; `unverifiable_changed` is the count of transitions claiming
  `future_behavior_changed` with no branch and no `next_consumer` — the evaluation rule's first
  teeth. `blocks |
  routes_to` are neutral records. An open branch past the 7-day window surfaces as STALE debt —
  advisory visibility pressure, never auto-closed (humans stay on irreversible decisions).
  Transitions claiming `future_behavior_changed` with no branch and no `next_consumer` are counted
  as unverifiable claims — the evaluation rule's first teeth.
- `next_consumer` — who is expected to act next. No named consumer ⇒ `audit_only` until one exists.
- `delta_cause` — the causal *class* of a source-truth change (`repair | retire | accept |
  invalidate`), carried with the new value so a future agent chooses repair/retirement/acceptance
  instead of re-litigating the old value. A bare value change is ambiguous; the cause is the branch.

**Branch coverage — open or it bloats.** An opened branch is an open loop and must move through
`opened → expanded → resolved → retired`. Presence ≠ coverage: an upstream artifact must not keep
anyone on an earlier branch once downstream evidence already covers it — surfaces compute branch
*coverage*, not artifact *presence*. Every opened branch becomes an owner-bounded step, is explicitly
deferred, or is subtracted; nothing sits unclassified. **Subtraction is a branch operation:** each
retirement records the future wrong move it prevents (`prevents_branch`).

**Brief before mutate.** When evidence is too broad to act on, insert a *transition brief* that
converts the queue into decision classes and names the blocked moves — it narrows branch entropy
*without* mutating. A brief is permission to think, not to mutate; stateful action still requires a
later `ActionIntent` + `PolicyDecision`. This keeps `orient`/ContextPack (read) cleanly separate from
`ActionIntent`/policy (write).

**Anti-bloat pipeline.** An observation/evidence artifact is live only while its branch is open. Once
promoted into a durable contract/check/decision — or subtracted — its workbench entry is marked
`promoted → <destination>` and archived, so observation logs stay thin (consumed entries leave them).
This is the archive half of the compounding-knowledge loop (`../roadmap/phases.md`).

**Harness co-evolution (shipped subset).** `Transition.Cause` (`internal/envelope`) ships a proven
subset of the above: `delta_cause` + `caused_by_refs` (≥1, validated only when `Cause != nil`) +
`next_consumer` (empty ⇒ `audit_only`). `opens_branches[]` is ALSO shipped (see the branch entry
above); `prevents_branch` stays deferred — dead schema without a consumer. The `harness
co-evolution` slice wires Cause end to end: harness-canon files
are a source-manifest family; `compound source canon-observe` (Importer `sourceobs.ObserveCanonDrift`)
re-hashes them, parks one no-promote `CapabilityObs` candidate per drifted file, and the ObjectFamily
`capgraph.RecordCanonEvolution` mints ONE cause-carrying `Transition` projected onto the touched
capability nodes as `capability://<node-id>` in the flat `Projections.Skill` (no new `ProjectionKind`).
Two read surfaces: the manifest json (baseline of record) and `events/tr_*.json` filtered by the
`capability://<node>` projection.
