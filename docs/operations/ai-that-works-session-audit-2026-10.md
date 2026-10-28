# ai-that-works session corpus audit — what Compound absorbs, extends, and refuses

- Audit scope: `ai-that-works/ai-that-works` at `327d980819817d63d98cb1114eb4e6d2df9d0209` — all 84 dated project directories (80 numbered episodes, three unnumbered workshops, one uncatalogued project directory).
- Work envelope: `work_bc7451ff2987` (owner `oh_my_pi`).
- Upstream license is **not established** in the audited snapshot. Concepts are referenced; no upstream text or code is copied. Existing credit stays revision-scoped in `skills/product-pipeline/SOURCES.md` (`3274d8b0`, three projects); this audit concerns `327d9808` and does not widen that credit.
- Every claim below cites an upstream relative path or a Compound path. "Already present" means the owner and mechanism exist — not feature equivalence, and not a passing-execution claim.

## 1. Decision

Absorb the corpus as **evidence for existing owners**, extend five bounded paths, and refuse new orchestration, memory, evaluation, and tool-catalog platforms. Compound already owns candidate adjudication, governed execution, and receipt-backed closure. The opportunity is stronger evidence at those boundaries.

The intake path is proven end to end today:

```text
scripts/upstream-learning/upstream_scout.py   pinned upstream text → staged content + content hashes
  → scripts/upstream-learning/learning_tick.py  governed WorkEnvelope + ActionIntent + PolicyDecision
      → compound finding record                 hash-checked body custody
      → compound source observe                no-promote capability_observation candidate + NoPromoteDecision
      → compound capability import-evaluations  evidence-only eval node + evaluated_by / validated_by edges
```

Review is a **separate branch of that path**, not its tail. A no-promote candidate is already
adjudicated, so `compound panel plan --target-kind capability_observation` selects only candidates
still awaiting a verdict — it does not re-open a no-promote one. The panel leg was proven
separately against a verdict-pending candidate.

That path was exercised against this upstream repository during the audit, in an isolated smoke
instance. A six-file frontier (`README.md`, `2026-10-06-building-a-memory-pipeline/README.md`,
`2026-07-28-your-model-is-already-obsolete/README.md`, `2026-08-04-slop-code-bench/README.md`,
`2026-04-21-harness-engineering-without-the-hype/README.md`,
`2026-02-10-agentic-backpressure-deep-dive/EPISODE.md`) produced exactly what it should:

| Stage | Observed artifact |
|---|---|
| Pinned custody | Six `observed/findings/*` bodies whose content hashes match the observation's `pending_body:` refs |
| Source observation | One `compound.source_observation.v0.1` record pinned to `git:327d980819817d63d98cb1114eb4e6d2df9d0209`, `promote_allowed: false` on its map |
| Candidate | One `compound.source_candidate.v0.1` of kind `capability_observation`, `verdict: no_promote` |
| Decision | One `compound.no_promote_decision.v0.1` — basis "source observation map forbids promotion", with content-hash evidence refs |
| Evaluation | One `eval:capability-evaluation-audit-ai-that-works-session-corpus` node marked `evidence_only: true` |
| Graph edges | `evaluated_by` and `validated_by` edges from the capability node to that evaluation node, both at confidence 0.74 |
| Panel | A planned panel run resolving one `awaiting_verdict` target, four included lenses, twelve excluded lenses, and a judge route |

Nothing was promoted: no memory claim exists, the capability carries no usage or injection, and
the evaluation node is evidence-only. The capability graph node exists as a review target — a
graph entry, not an installed, triggered, or consumed capability.

| ID | Slice | Priority | Owner / seam | Upstream projects covered |
|---|---|---|---|---|
| S1 | Finish upstream review with attributable evaluation evidence | P2 (operational first) | `scripts/upstream-learning/`, `internal/capgraph/evaluation.go` | Software Factory for Agent Tools |
| S2 | Qualify execution against correction, denial, and stale completion | P1 | `internal/work/work.go`, `internal/policy/action_monitor.go`, `internal/work/standing_proof.go` | Interruptible Agents; Email Is All You Need; Agentic Backpressure; PII Redaction |
| S3 | Compare replacement routes using matched, receipt-backed outcomes | P1 | `internal/routeevidence/outcome.go`, `internal/latenttax/latenttax.go` | Your Model Is Already Obsolete; Eval Many Models; Bash vs. MCP |
| S4 | Bind improvement acceptance to the change and its negative control | P2 | `internal/distiller/eval.go` → `internal/distiller/improvement_plan.go` | Prompt Optimizer; SOTA Coding Agent Benchmarks |
| S5 | Make benchmark certification input-bound and lane-aware | P2 | `internal/bench/bench.go` | No Vibes March; Performance Engineering |

S2 combines two separate P1 candidates; S3 preserves the third. No other candidate is promoted to P1.

## 2. Coverage attestation

The audit ran as 28 independent worker batches over the upstream tree, three projects per batch,
each worker reading its assigned projects separately and citing exact paths. Verification found
84/84 projects covered, zero assignment gaps, and zero duplicate project records.

| Evidence depth | Projects |
|---|---:|
| A — code, tests, and recorded results | 3 |
| B — code only | 40 |
| C — narrative only (transcript, email, whiteboard) | 35 |
| D — metadata / announcement only | 6 |
| **Total** | **84** |

| Classification | Projects |
|---|---:|
| Already present | 36 |
| Extend existing owner | 17 |
| Worth a vertical slice | 16 |
| New but should not build | 15 |

What the corpus does **not** prove: passing execution, production reliability, comparative savings,
or generalization. Even its strongest artifacts need qualification — the model-swap matrix records
two repeats while its runner defaults to three
(`2026-07-28-your-model-is-already-obsolete/results/matrix.json`;
`2026-07-28-your-model-is-already-obsolete/harness/runner.py`).

## 3. Final slice set

Every slice uses the same authority path:

```text
Intent → EffectiveContract → ContextPack → WorkEnvelope → ActionIntent → PolicyDecision
       → Run / ToolEvent / RouteEvidence → Receipt → Transition
```

Evaluation and panel output remain evidence. Reusable findings enter candidates and adjudication;
nothing enters ambient recall because a test, model, or reviewer produced a favorable result.

### S1 — Finish upstream review with attributable evaluation evidence

**Priority:** P2; first operational task.

**Upstream evidence.** Software Factory for Agent Tools describes findings, human revision, and
comparative trials but supplies no reproducible arena implementation or raw trial results
(`2026-06-23-software-factory-for-agent-tools/transcript.txt`).

**Owner and flow.** Existing scout → pinned finding custody → `compound source observe` → no-promote
`capability_observation` → `compound capability import-evaluations` → evidence-only evaluation node.
Owner review runs as a branch of that path (`compound panel plan --target-kind
capability_observation` over a candidate still awaiting a verdict). Repair the review tick inside
`scripts/upstream-learning/`; strengthen provenance only at `internal/capgraph/evaluation.go`.
Govern repairs and imports through the spine; attach their actual outcomes to receipts and
transitions. Consumers are upstream owner review and capability review — not a new inbox
(`scripts/upstream-learning/README.md`).

**Baseline and acceptance — verified, and corrected against the live instance.** The operator status
note claimed the upstream-review tick times out. Live evidence contradicts that: no review attempt
timed out, and the attempts that mattered **succeeded** (`run_attempts/*.json`, status `succeeded`,
two governed plan-mode runs each). The real baseline is a **blind retry loop**. Each attempt's output
is rejected by a deterministic validator, and the tick discards the rejection reason before the next
attempt, so every retry repeats the same defect until the three-pass bound is exhausted
(`scripts/upstream-learning/review_tick.py`, `role_state`/`prompt_for`; observed rejection reasons:
`Review requires one JSON document or one explicit JSON block` and `Claim citation does not resolve
to pinned source text`). The operator-visible symptom is `attempts_exhausted` with no reviewable
proposal — the same source sat at that state across 404 logged waits.

Fixed in this change: a rejected attempt now hands its validator reason to the next attempt
(`role_state` → `prompt_for` → `run_one`), so the retry can correct the named defect instead of
repeating it. Proven by `test_retry_receives_the_reason_the_previous_attempt_was_rejected`, which
fails against the pre-fix code and passes after it.

Remaining baseline: each configured repository must reach an explicit bounded disposition, and a
controlled eligible finding must reach a reviewable artifact. Missing receipts, failed trials,
revision mismatches, and duplicate imports must not create unsupported positive validation or
ranking benefit. "No adoption" is a successful outcome.

**Cost / risk.** The retry-repair fix is landed here. Provenance work remains provisionally 3–5
engineering days. External receipts remain unresolved; the upstream corpus cannot supply them
(it ships no reproducible arena implementation or raw trial results).

**Rollback / retirement.** Restore prior processing while retaining evidence; suspend new positive
validation if binding fails. Retire the demonstrated blind-retry path and, only if reproduced,
verdict-only positive validation. No additional acquisition queue.

**Why this, not the nearest alternative.** Complete the working learning loop before building
another software factory.

### S2 — Qualify execution against correction, denial, and stale completion

**Priority:** P1.

**Upstream evidence.** Interruptible Agents checks phase boundaries but can retain obsolete research
after replanning (`2025-08-19-interruptible-agents/runtime.py`;
`2025-08-19-interruptible-agents/manager.py`). Email discusses correction and effect races without
implementation (`2026-01-20-email-is-all-you-need/transcript.txt`). Backpressure supplies
effect-checking assertions (`2026-02-10-agentic-backpressure-deep-dive/05-hooks-and-side-effects.test.ts`).
PII supplies adversarial inputs without correctness assertions
(`2026-03-03-pii-redaction-and-sensitive-data-scrubbing/baml_src/redact_tests.baml`).

**Owner and flow.** Harness or configuration change, or operator correction → pinned runtime and
configuration plus ordered action evidence → deterministic reservation and policy checks → governed
execution → truthful receipt and transition → capability-evaluation evidence consumed by harness
qualification. Findings enter existing candidate review; replacement plans require fresh
authorization. Owners: `internal/work/work.go`, `internal/policy/action_monitor.go`,
`internal/work/standing_proof.go`, `internal/mem/standing_proof.go`.

**Baseline and acceptance.** Inventory current coverage first. The current architecture
distinguishes observational Codex `PreToolUse` from opt-in native blocking and already documents a
blocking proof; the observational path must not be certified as enforcement. Deterministic
interleavings must show:

- Invalidated or denied actions make zero external calls.
- Stale completion cannot close replacement work.
- Committed or uncertain effects survive cancellation accounting.
- Synthetic prohibited source-to-sink transfers remain denied regardless of spelling.
- Permitted controls succeed; missing hooks or changed configuration invalidate qualification.

**Cost / risk.** Roughly 2–5 days for the first boundary; broader adapter coverage is separate.
Check/effect races and false cancellation guarantees dominate.

**Rollback / retirement.** Revert only the new integration; keep existing checks and hold affected
execution while safety is unresolved. Retire demonstrated adapter bypasses or configuration-only
qualification. If existing behavior passes, retire the feature proposal.

**Why this, not the nearest alternative.** Test the existing execution boundary rather than add a
cancellation service or a model supervisor.

### S3 — Compare replacement routes using matched, receipt-backed outcomes

**Priority:** P1.

**Upstream evidence.** The model-swap gate checks accuracy, cost, and latency but does not itself
verify matched case populations, and missing usage can become zero
(`2026-07-28-your-model-is-already-obsolete/harness/gate.py`;
`2026-07-28-your-model-is-already-obsolete/harness/runner.py`). Earlier model comparison mainly
persists generations (`2025-07-29-eval-many-models-same-prompt/main.py`). Bash/MCP examples vary
conditions and do not establish causal superiority
(`2025-09-16-coding-agent-tools-bash-vs-mcp/README.md`).

**Owner and flow.** One real provider-retirement or route-change decision → matched
incumbent/candidate runs through existing governed executors → original route evidence and graded
receipts → deterministic comparison → candidate route-policy review → separately authorized change
→ subsequent receipt and transition. `internal/routeevidence/outcome.go` owns assessment;
`internal/latenttax/latenttax.go` derives measurements, not execution. Captured-response replay
reuses `internal/distiller/eval.go`.

**Baseline and acceptance.** Establish incumbent correctness, critical-case outcomes, complete cost,
and latency on pinned cases with declared repetitions. Missing usage or prices, unmatched
populations, insufficient repetitions, and critical regressions cannot recommend replacement.
Generation success is insufficient. Consumer: the operator deciding that specific route change.

**Cost / risk.** Medium implementation effort plus separately bounded provider spend. Cache
conditions, unequal tool semantics, and model drift can invalidate comparisons.

**Rollback / retirement.** Disable comparison configuration; restore any changed route through
governance and preserve trials. Replace the manual comparison artifact for that decision. Without a
concrete decision and a displaced practice, park implementation.

**Why this, not the nearest alternative.** Support one decision rather than create a leaderboard or
an automatic model-switch daemon.

### S4 — Bind improvement acceptance to the change and its negative control

**Priority:** P2.

**Upstream evidence.** Prompt Optimizer warns that shared instructions affect multiple consumers;
its reported toy improvement lacks held-out proof (`2025-12-16-prompt-optimizer/README.md`;
`2025-12-16-prompt-optimizer/transcript.md`). SOTA Benchmarks describes testing against the pre-patch
implementation, not general mutation testing
(`2026-07-14-sota-coding-agent-benchmarks/transcript.txt`).

**Owner and flow.** Existing improvement candidate → baseline and proposed target digests, affected
consumers, and fixed replay cases → deterministic proof validation → existing improvement review →
separately governed owner change → receipt identifying the applied revision and its proofs →
transition. Consumers remain the improvement review and owner writeback
(`internal/distiller/improvement_plan.go`; `internal/distiller/eval.go`).

**Baseline and acceptance.** First reproduce whether unrelated proof can satisfy that review. Reject
wrong-target, wrong-revision, and incomplete-consumer-set evidence, and reject cross-consumer
regressions. A regression-fix claim requires the same case to fail the intended semantic assertion
before and pass after; infrastructure failure, both-green pairs, and unpaired passes cannot
establish a fix. Replay stays zero-live-call.

**Cost / risk.** Medium, roughly 3–5 days once fixtures exist. Revision replay and incomplete
consumer declarations are the principal risks.

**Rollback / retirement.** Keep targets unchanged and reviews pending if binding fails. Preserve
legacy evidence without silently certifying it. Retire unbound proof sufficiency and prose-only
regression attestations where demonstrated.

**Why this, not the nearest alternative.** Strengthen acceptance before adding prompt-search
machinery.

### S5 — Make benchmark certification input-bound and lane-aware

**Priority:** P2.

**Upstream evidence.** No Vibes March distinguishes measurements from acceptance but supplies no
runnable compiler change (`2026-03-31-no-vibes-march/transcript.txt`). Performance Engineering
separates standalone timing from integrated measurement, and its deletion-first benefit remains
speculative (`2026-09-15-performance-engineering/transcript.txt`).

**Owner and flow.** Surfacing or recall change → before/after reports bound to canonical cases,
assertions, splits, and lane membership → deterministic comparison → existing harness-change review →
governed implementation → receipt and transition carrying acceptance proof. Extend only `Suite`,
`Report`, and `Compare` in `internal/bench/bench.go`; bench remains a scorer, not an executor.

**Baseline and acceptance.** Reproduce the audited name-only comparison limitation first. The same
suite name with changed inputs must not certify, and equal aggregate scores must not hide a required
lane regression. Missing lanes, skipped cases, and missing legacy digests are non-certifying.
Identical evaluation inputs remain comparable across different builds or graph revisions — those are
the intended experimental variables.

**Cost / risk.** Small to medium. Canonicalization, report compatibility, and sparse lanes need care.

**Rollback / retirement.** Keep old reports readable; suspend certification if the new comparison
logic fails. Retire name-only eligibility and aggregate-only acceptance for applicable suites.

**Why this, not the nearest alternative.** Repair comparison validity instead of building a general
coding benchmark.

The benchmark report now carries the complete declared evaluation inventory: case identity, kind, lane, split, expected outcomes and canonical input hashes. Certification checks scored coverage against that inventory, validates counts and metrics, and requires the same input identity across reports. Empty, legacy, shortened and unmatched evidence cannot produce an acceptance proof. Build, graph and method revisions may differ. Input identity establishes comparability; report authenticity still depends on the surrounding evidence custody.

## 4. Cluster map

| Mechanism cluster | Upstream projects and evidence | Existing Compound owner; verdict |
|---|---|---|
| Typed proposals and human gates | Twelve-factor workshop `2025-05-17-workshop-sf-twelve-factor-agents/agents-workshop/10-human-approval/src/agent.ts`; async humans `2025-06-03-humans-as-tools-async/src/server.ts` | Work and exact policy binding; **absorb**, no parallel approval service (`internal/work/work.go`; `internal/policy/action_monitor.go`) |
| Correction and effect boundaries | `2025-08-19-interruptible-agents/runtime.py`; `2026-02-10-agentic-backpressure-deep-dive/05-hooks-and-side-effects.test.ts` | Harness, hook, and work qualification; **extend via S2** |
| Retrieval and context economy | `2025-03-31-large-scale-classification/hello.py`; `2025-05-27-mcp-with-10000-tools/tools.py` | CapabilityGraph, ContextPack, bench; **absorb cases**, extend comparison via S5 |
| Comparative evaluation | `2026-07-28-your-model-is-already-obsolete/harness/gate.py`; `2025-12-16-prompt-optimizer/README.md` | Route outcomes and distiller review; **extend via S3/S4**, refuse a universal optimizer |
| Learning from operational evidence | `2026-06-23-software-factory-for-agent-tools/transcript.txt`; `2026-10-06-building-a-memory-pipeline/README.md` | Source intake, distiller, adjudicated memory; **extend via S1**, refuse a second memory pipeline |
| Presentation and delivery | `2026-04-07-sse-streaming/main.py`; `2026-04-14-agentic-coding-for-frontend-apps/03-wired-vs-pure/stories/TodoCard.stories.tsx` | Stream projections and product UI/QA; **absorb**, no new platform |
| Replaceable execution | `2026-05-05-openai-tells-you-not-to-build-your-own-harness/transcript.txt`; `2026-09-01-code-mode-extensible-software/transcript.txt` | Rented harnesses beneath policy and work; **refuse core runtime duplication** (`docs/LAYERS.md`) |

## 5. Explicit NO-BUILD list

Each entry refuses one **mechanism**, not the whole upstream project. A project can appear here and
still contribute an absorbed mechanism elsewhere — the split is by mechanism, not by directory.

1. **Another software-factory queue** — duplicates existing candidate and work ownership; a
   narrative workflow is not replacement evidence
   (`2026-06-23-software-factory-for-agent-tools/README.md`; `docs/LAYERS.md`).
2. **A perpetual permission-bypassing coding loop** — no explicit completion or spend bound
   (`2025-10-12-unconference-sf/dex-ralph-demo/loop.sh`).
3. **A separate semantic tool registry** — the "10,000 tools" selector considers only the first
   100 parsed tools, so catalog scale is not selection proof
   (`2025-05-27-mcp-with-10000-tools/tools.py`; `internal/bench/bench.go`).
4. **A standalone prompt optimizer** — toy narrative evidence does not justify another improvement
   owner (`2025-12-16-prompt-optimizer/transcript.md`;
   `internal/distiller/improvement_plan.go`).
5. **A model-based permission supervisor** — upstream fails open and reviews an earlier response
   snapshot (`2025-09-02-voice-agent-supervisor-threading/voice_agent.py`;
   `internal/policy/action_monitor.go`).
6. **Hosted redaction as an outbound privacy guarantee** — detection already sends the source to a
   hosted model (`2026-03-03-pii-redaction-and-sensitive-data-scrubbing/baml_src/redact.baml`).
7. **A core QuickJS or compiler platform** — batching does not establish atomicity, and the
   transcript acknowledges partial edits (`2026-09-01-code-mode-extensible-software/transcript.txt`;
   `docs/architecture/transition.md`).
8. **A separate decay or team-memory truth store** — duplicates adjudicated memory ownership
   (`2025-07-15-decaying-resolution-memory/README.md`; `docs/memory-architecture.md`).
9. **A telemetry warehouse or typed query language** — requirements lack implementation and no
   demonstrated Compound diagnostic gap exists (`2026-07-07-agent-observability/transcript.txt`;
   `internal/observation/observation.go`).
10. **A VM optimizer or writing registry from announcements** — no demonstrated mechanism
    (`2026-05-26-no-vibes-allowed-performance-engineering/meta.md`;
    `2026-11-03-using-ai-for-writing/meta.md`).

## 6. Sequencing and dependencies

Start with **one S1 review through the existing path**. It proves value alone by replacing a
blind retry loop with a bounded, reviewable disposition; it needs neither a new evaluator nor adoption.

Then qualify one execution boundary in S2 before using it for S3's paid comparison. S4 and S5 are
independent proof-hardening work, admitted only after reproducing their local gaps.

The 16 projects classified as worth a vertical slice reconcile exactly: 12 are covered by S1–S5,
and 4 are deliberately parked. No project appears in both groups, and none is left unassigned.

Four candidates are deliberately parked:

- **Useful-result latency** — no named affected workflow, timestamp coverage, or validated milestone
  (`2026-01-06-latency/main.py`).
- **Code-mode conformance** — no identified adapter, nested policy traces, or displaced
  truncate/rerun workflow (`2026-05-12-code-mode-deep-dive/transcript.txt`).
- **Cumulative checkpoints** — no verified close-path integration or local benchmark implementation
  (`2026-08-04-slop-code-bench/README.md`; `internal/work/standing_proof.go`).
- **Memory-input filtering** — no independent held-out corpus and no demonstrated Compound cost
  warranting optimization (`2026-10-06-building-a-memory-pipeline/transcript.txt`;
  `internal/distiller/eval.go`).

The 17 owner extensions remain targeted coverage opportunities, not seventeen projects. Each
requires a real consumer, a reproduced omission, and a named retirement before admission.

## 7. Measurement

Thresholds below are proposed acceptance rules, not measured outcomes.

| Slice | Owned evaluator and success criterion | Review trigger and stop condition |
|---|---|---|
| S1 | Capability evaluation: traceable positive evidence, idempotent import, bounded review dispositions, no unsupported surfacing lift (`internal/capgraph/evaluation.go`) | Review after the three configured repositories; stop expansion if the same rejected output repeats without a changed approach or receipts cannot be resolved |
| S2 | Capability-evaluation records runtime qualification; deterministic effect and interleaving tests supply the ground truth — zero forbidden effects and correct permitted controls (`internal/capgraph/evaluation.go`; `internal/work/standing_proof.go`) | Requalify on runtime or configuration change; stop affected execution on bypass or ambiguous outcome |
| S3 | Latent-tax plus receipt-linked assessment: critical-case preservation, complete cost accounting, declared repetitions and budgets (`internal/latenttax/latenttax.go`; `internal/routeevidence/outcome.go`) | Review at the trial budget or on material provider change; stop on critical regression, incomparable evidence, or missing accounting |
| S4 | Distiller eval: exact binding, targeted semantic fail/pass pair, unaffected-consumer non-regression, zero live replay calls (`internal/distiller/eval.go`) | Review before target changes; stop acceptance on stale proof or incomplete coverage |
| S5 | Bench: changed-input and masked-lane fixtures refuse certification; valid identical-input controls pass (`internal/bench/bench.go`) | Review on suite or comparator changes; stop certification on missing identity or incomparable lanes |

## 8. Risks and limits

The principal risk is **fossilizing examples into policy**: synthetic cases can become stale
obligations, and consumer declarations can omit the very dependency they should protect. Keep
requirements revision-bound and explicitly retire superseded expectations
(`internal/distiller/improvement_plan.go`; `docs/architecture/transition.md`).

Measurement can also reward the wrong outcome. Cheaper runs may omit work, early progress may be
unusable, and zero tokens may mean missing instrumentation. Capability-evaluation records preserve
evidence but are not independent correctness checks. Require task-specific checks and retain
failures (`internal/routeevidence/outcome.go`;
`2026-07-28-your-model-is-already-obsolete/harness/runner.py`).

Current documentation verifies owners, not every caller's enforcement. In particular, observational
hooks must not be described as blocking, and the existing full proof-check must not be rebuilt under
another name (`docs/architecture/status.md`).

These recommendations are falsified when current tests already prove the proposed boundary, no real
consumer or retired practice can be identified, or controlled evaluation shows no useful
improvement. In those cases, close or park the proposal.

### 8.1 A measurement defect that bounds every slice above

Compound's capability-usage projection can only count the use of a **skill**.
`normalizeSkillRef` (`internal/capgraph/usage.go`) accepts only `capability://skill:*` refs, and
`NormalizeCapabilityUse` (`internal/hook/capability_use.go`) emits a use event only when the
native tool name is `skill`. A tool, instruction rule, doctrine, lens, or procedure that is used
therefore cannot reach a fired or declared-use count at all, so every such row in the
`never_fired` summary is **unmeasured, not unused** — the projection's own field comment already
says so ("Missing evidence does not prove non-use").

Measured on the live home: `capability://tool:headroom` reports 31 offers, 0 fired and 0 declared
use against 2269 recorded MCP invocations; `capability://tool:node-repl` reports 25/0/0 against
1212; `capability://tool:codegraph` reports 14/0/0 against 12. 387 offered rows report
never-fired while the hook stream records roughly 2.8 million tool invocations.

Consequence for this audit: any decision justified by the usage signal — "this capability is
unused, extend it", "this owner never fires, retire it" — reads a measurement artifact as
behavior. Treat `never_fired` as a prompt for instrumentation, never as evidence of non-use.
The S1–S5 recommendations above are chosen on mechanism and owner grounds and do not depend on
this signal, so they stand.

The repair (`work_8e6509d7d901`) is a projection-only change: the projection now normalizes a
ref of any kind, counts non-skill pack offers instead of dropping them, and attributes a
recorded tool invocation to the node it evidences. The invoked tool's native name is read from
the hook event's `side_effects`, where the hook has always recorded it, so the repair reaches
the entire existing history and no shared envelope contract changes. Measured on the live home,
total fired counts go from 221 to 7,998, and `capability://tool:headroom.headroom-retrieve` and
`capability://mcp:headroom` each reach 2,271 fires. A tool the graph does not hold stays
unmeasurable: inventing a node for it would put a capability in the projection that no catalog
declared.

## 9. Per-project ledger

Every dated upstream project, its evidence depth, the audit verdict, priority, value, and the
mechanism actually observed. Depth A = code + tests + results; B = code only; C = narrative only;
D = metadata or announcement only.

| Project | Depth | Verdict | Priority | Value | Mechanism observed |
|---|---|---|---|---|---|

| `2025-03-31-large-scale-classification` | B | extend owner | P2 | 3/5 | Each category has separate retrieval text and LLM-facing description. The default path loads 16 categories, embeds every category and the query, comp… |
| `2025-04-07-reasoning-models-vs-prompts` | B | already present | P3 | 2/5 | `ChatWithGraph` sends conversation history and a movie graph schema to GPT-4o-mini. Its prompt requests initial reasoning, problems, and improved rea… |
| `2025-04-15-code-generation-small-models` | B | do not build | P3 | 1/5 | BAML defines `FindImports`, using a locally configured Llama client, and `GenerateDiff`, using GPT-4o. The latter receives instructions, one filename… |
| `2025-04-22-twelve-factor-agents` | B | already present | P3 | 2/5 | A `Thread` contains events and serializes them into tagged text. BAML selects a typed next action. Application code appends that action, executes cal… |
| `2025-05-10-workshop-nyc-twelve-factor-agents` | B | already present | P3 | 2/5 | The workshop progressively introduces tools, loops, tests, human interaction, context formatting, HTTP endpoints, state, and approval. Its approval w… |
| `2025-05-13-designing-evals` | B | already present | P2 | 3/5 | One model call generates a structured lesson plan; another evaluates it into categorical pacing, a list of biases, and estimated cost. Python saves t… |
| `2025-05-17-workshop-sf-twelve-factor-agents` | B | already present | P3 | 1/5 | A request creates a thread of events. The agent serializes those events into model context, requests a typed next action, appends the action, dispatc… |
| `2025-05-20-policies-to-prompts` | B | extend owner | P2 | 3/5 | Two related paths exist: 1. `ExtractQuestions` turns a policy document into binary audit questions with source text and optional section citations. I… |
| `2025-05-27-mcp-with-10000-tools` | B | extend owner | P2 | 3/5 | The parser converts supported JSON schemas into dynamic BAML types and injects a server/tool discriminator. The selector then takes **only the first… |
| `2025-06-03-humans-as-tools-async` | B | already present | P3 | 2/5 | An event thread is serialized into a prompt. The model returns a typed next action; code executes ordinary arithmetic but exits the inner loop for cl… |
| `2025-06-10-cracking-the-prompting-interview` | B | extend owner | P2 | 3/5 | The citation example gives the model numbered content blocks and asks for integer references; Python maps returned integers to source URLs. Diarizati… |
| `2025-06-17-entity-extraction` | B | already present | P3 | 1/5 | BAML extracts a resume containing verbatim company names, company categories, and optional guessed legal names. Python checks those guesses against a… |
| `2025-06-24-ai-content-pipeline` | B | already present | P3 | 2/5 | The inspected API background task streams a typed `VideoSummary` into the database, replaces previous drafts, generates a title, and starts email, Tw… |
| `2025-07-01-ai-content-pipeline-2` | B | extend owner | P2 | 3/5 | The background task streams a summary, persists it, clears previous drafts, and generates platform-specific drafts concurrently. Email explicitly cal… |
| `2025-07-08-context-engineering` | B | already present | P3 | 1/5 | The README recommends unified context assembly, decaying-resolution memory, task-specific tools, deterministic injection of required context, and cod… |
| `2025-07-15-decaying-resolution-memory` | B | already present | P3 | 1/5 | The inspected implementation is a preparation workflow: scan Redis thread keys, export thread contents, divide text into 1,000-character chunks, call… |
| `2025-07-22-multimodality` | B | do not build | P3 | 2/5 | The executable path rasterizes a PDF, saves page images, chooses page index 1 as its reference, and compares subsequent pages. It resizes mismatched… |
| `2025-07-29-eval-many-models-same-prompt` | A | candidate slice | P2 | 4/5 | The runner forms a two-fixture-by-four-model matrix, loads typed inputs, overrides the primary BAML client, invokes `DraftEmail`, and writes each res… |
| `2025-08-05-advanced-context-engineering-for-coding-agents` | B | already present | P2 | 3/5 | Human clarification → fresh research context → repository investigation → reviewed implementation plan → implementation in separate worktrees. Resear… |
| `2025-08-12-manus-context-engineering` | C | do not build | P3 | 2/5 | Keep the system-message prefix stable, put dynamic input later, and periodically reintroduce task-relevant information. The material also cautions ag… |
| `2025-08-19-interruptible-agents` | B | candidate slice | P1 | 4/5 | CLI input queues `info`/`replan` messages or sets a cancellation event. A worker thread runs an asyncio loop through planning, parallel searches, and… |
| `2025-08-26-claude-for-non-code-workflows` | B | already present | P3 | 1/5 | A Markdown SOP divides daily work into context gathering and subsequent updates. The first phase gathers journal entries, metrics, user input, and du… |
| `2025-09-02-voice-agent-supervisor-threading` | B | do not build | P3 | 2/5 | User input enters a shared conversation list. The program launches a streaming conversational model and a compliance task concurrently. During respon… |
| `2025-09-09-generative-uis` | B | do not build | P3 | 2/5 | BAML declares required-at-stream-time recipe name and servings fields, and marks individual ingredients complete-before-emission with `@stream.done`.… |
| `2025-09-16-coding-agent-tools-bash-vs-mcp` | B | candidate slice | P2 | 3/5 | Two tool surfaces expose Linear operations: an MCP configuration and a purpose-built CLI. Separate instruction files tell the agent which surface to… |
| `2025-09-23-evals-for-classification` | B | already present | P3 | 2/5 | The pipeline loads categories, narrows them, optionally expands the query **after narrowing**, then selects a final category. Hybrid narrowing retrie… |
| `2025-09-30-dyanmic-schemas` | B | do not build | P3 | 1/5 | The backend accepts text, uploaded content, or a URL. One model call generates BAML schema text and a return type. Execution is a separate endpoint:… |
| `2025-10-07-anthropic-post-mortem` | D | already present | P2 | 3/5 | A narrated incident-analysis workflow: detect subtle quality regressions through user signals and product metrics; roll back changes; investigate con… |
| `2025-10-12-unconference-sf` | B | do not build | P3 | 1/5 | The shell repeatedly pipes `PROMPT.md` into Claude with permission checks bypassed, streams output through a visualization process, sleeps ten second… |
| `2025-10-14-no-vibes-allowed` | C | already present | P2 | 3/5 | Refine the feature specification, research relevant code and testing conventions, resolve design ambiguities, divide implementation into independentl… |
| `2025-10-21-agentic-rag-context-engineering` | B | extend owner | P2 | 3/5 | The runtime supplies conversation history and working directory to a BAML agent call. A structured response either ends the turn or selects a tool. T… |
| `2025-10-28-ralph-wiggum-coding-agent-power-tools` | B | already present | P3 | 2/5 | The prompts instruct an agent to inspect specifications and code, implement one highest-priority item, run checks, update a rolling plan, and commit.… |
| `2025-11-05-event-driven-agents` | B | extend owner | P2 | 4/5 | A bounded Effect PubSub bus distributes typed events. The message reducer queues user input while streaming, appends chunks to the active assistant m… |
| `2025-11-11-dates-and-times` | B | extend owner | P2 | 3/5 | The model extracts a union of absolute dates, relative durations, and recurring cron expressions. `ExtractDates` accepts an optional reference-date s… |
| `2025-11-18-building-an-animation-pipeline` | C | do not build | P3 | 2/5 | The narrated workflow passes an Excalidraw drawing through a forked renderer and browser automation to produce WebM, requests human review, then uplo… |
| `2025-11-25-no-vibes-allowed-using-codelayer-to-build-codelayer` | D | already present | P3 | 1/5 | Research → Plan → Implement, using CodeLayer to develop CodeLayer itself. The artifacts name the workflow but do not expose its data structures, phas… |
| `2025-12-02-multimodal-evals` | A | already present | P3 | 3/5 | Receipt image → grayscale/contrast preprocessing → base64 image → BAML `ExtractReceiptTransactions` → typed receipt → six deterministic checks. Faile… |
| `2025-12-09-git-worktrees` | C | already present | P3 | 2/5 | Research and specification precede implementation. Multiple agents work in separate Git worktrees; a human or another agent synthesizes their outputs… |
| `2025-12-16-prompt-optimizer` | C | candidate slice | P2 | 4/5 | The discussion describes evaluating an initial prompt, generating a candidate from performance feedback, evaluating candidates, maintaining a Pareto… |
| `2025-12-23-founding-humanlayer` | C | already present | P3 | 2/5 | - **Approval at consequential actions:** an agent requests permission from a HumanLayer server; that server contacts a person through Slack/email. Th… |
| `2025-12-30-founding-boundary` | C | do not build | P3 | 2/5 | - **Failures become parser cases:** user-reported malformed model output exposes cases involving quotation marks, embedded code, recursive types, int… |
| `2026-01-06-latency` | B | candidate slice | P2 | 3/5 | The Python loop sends message history to synchronous `b.AgentLoop`, handles replies or validation errors, dispatches filesystem/shell tools, and appe… |
| `2026-01-13-applying-12-factor-principles-to-coding-agent-sdks` | B | already present | P3 | 2/5 | Two concrete patterns: |
| `2026-01-20-email-is-all-you-need` | C | candidate slice | P2 | 4/5 | The discussion proposes serial processing by email thread, with concurrency across threads. Before a database write or outbound send, processing chec… |
| `2026-01-27-no-vibes-allowed` | C | extend owner | P3 | 3/5 | Two distinct mechanisms are described: |
| `2026-02-03-prompting-is-becoming-a-product-surface` | B | do not build | P3 | 2/5 | A natural-language goal goes to `GenerateSchema`, which returns a map of fields using four supported shapes: number, text, dropdown, and bulleted lis… |
| `2026-02-10-agentic-backpressure-deep-dive` | B | candidate slice | P1 | 4/5 | Turn an integration assumption into a small executable experiment before building the dependent feature: 1. Query the SDK, capture its initialization… |
| `2026-02-17-automating-aitw` | B | already present | P2 | 2/5 | Three distinct implemented workflows are visible: - **Email:** transcript/title/description → structured extraction → composition → AI-style critique… |
| `2026-02-24-no-vibes-february` | C | already present | P2 | 3/5 | Probe an opaque dependency before designing around its behavior. The described test asks Claude Agent SDK to run `bash sleep 3`, immediately supplies… |
| `2026-03-03-pii-redaction-and-sensitive-data-scrubbing` | B | candidate slice | P2 | 4/5 | The prose proposes three layers: static matching, matching against known database values, and generative detection of contextual disclosures. A separ… |
| `2026-03-10-claude-agent-skills-deep-dive` | C | already present | P3 | 2/5 | Separate instruction loading from context isolation. Skills inject reusable instructions when needed; subagents run token-heavy work in another conte… |
| `2026-03-17-prompt-injections-guardrails` | B | already present | P2 | 2/5 | Two small extraction examples expose the difference between output structure and security enforcement: - Python sends a secret-bearing system message… |
| `2026-03-24-mcp-is-dead` | C | extend owner | P2 | 3/5 | The session distinguishes integrations the application owns from tools users introduce dynamically: 1. For known integrations, expose narrow applicat… |
| `2026-03-31-no-vibes-march` | C | candidate slice | P2 | 4/5 | The proposed testing design separates: - Per-case named measurements from hard assertions. - Scenario-level acceptance from individual imperfect outp… |
| `2026-04-07-sse-streaming` | B | already present | P3 | 2/5 | A request supplies a URL. The server fetches that page, extracts matching path-prefixed links, deduplicates them, and processes groups of ten. Each t… |
| `2026-04-11-unconf-sf` | B | extend owner | P2 | 3/5 | 1. The segmentation CLI checks the transcript path and `GOOGLE_API_KEY`, then wires a provider, splitter, and writer. The orchestration reads the tra… |
| `2026-04-14-agentic-coding-for-frontend-apps` | B | extend owner | P2 | 4/5 | The wired component fetches todos and manages loading, errors, status changes, and deletion state. It passes data, callbacks, and busy flags into a p… |
| `2026-04-21-harness-engineering-without-the-hype` | C | already present | P3 | 2/5 | The proposed workflow is to improve one agent loop’s instructions, tools, and context before adding orchestration; inspect actual outputs; preserve e… |
| `2026-04-28-no-vibes-design-docs` | C | already present | P3 | 2/5 | A human-led design workflow: |
| `2026-05-05-openai-tells-you-not-to-build-your-own-harness` | C | already present | P3 | 1/5 | Separate the **inner harness**—tool definitions, implementations, and response handling—from the **outer workflow**—planning, domain context, task de… |
| `2026-05-12-code-mode-deep-dive` | C | candidate slice | P2 | 3/5 | Expose tools as callable functions inside a code execution environment. Generated code chains calls, filters intermediate results, and emits only sel… |
| `2026-05-19-feature-flag-everything` | C | already present | P2 | 3/5 | Separate deployment from exposure. Deploy an inactive variant, expose it to a bounded population or time window, collect predefined metrics, then exp… |
| `2026-05-26-no-vibes-allowed-performance-engineering` | D | do not build | P3 | 0/5 | **Not established.** The metadata announces VM performance engineering, resource management, latency, memory bottlenecks, and infrastructure cost. It… |
| `2026-06-02-multilingual-ai-apps` | C | extend owner | P2 | 3/5 | Normalize input into the core pipeline’s language, run one shared agent/tool pipeline, then translate the response into the user’s language and regis… |
| `2026-06-09-fable-5` | C | extend owner | P2 | 3/5 | Two complementary evaluation practices: - Give a new model a difficult, current design problem; ask it to explain the architecture and identify missi… |
| `2026-06-16-product-specs-with-ai` | C | already present | P3 | 2/5 | Codebase research informs separate product and technical discussions. Product design covers experience, success measures, and exclusions. Technical d… |
| `2026-06-23-software-factory-for-agent-tools` | C | candidate slice | P2 | 4/5 | The reported workflow is: 1. Agents implement tasks against BAML; chat logs become evidence. 2. A second agent extracts findings, called trophies. 3.… |
| `2026-07-07-agent-observability` | C | extend owner | P2 | 3/5 | Instrument execution before incidents occur; preserve structured function inputs, outputs, latency, and call relationships; expose queries that follo… |
| `2026-07-14-sota-coding-agent-benchmarks` | C | candidate slice | P2 | 4/5 | The session describes several distinct checks: existing tests with agent test edits removed; comparison against a hidden golden patch; a quality judg… |
| `2026-07-21-no-vibes-july` | C | already present | P3 | 2/5 | Two separate workflows are discussed: 1. **Proposed memory workflow:** repeated statements across team sessions → nightly candidate generation → inte… |
| `2026-07-28-your-model-is-already-obsolete` | A | candidate slice | P1 | 4/5 | The CLI runs the incumbent and candidate against the corpus, then applies a regression budget. The runner selects the BAML client at runtime, repeats… |
| `2026-08-04-slop-code-bench` | C | candidate slice | P2 | 4/5 | The described benchmark reveals requirements incrementally, retaining the model’s previous implementation between checkpoints. It separates current-c… |
| `2026-08-11-unconference-recap` | C | already present | P3 | 2/5 | The recap describes four practical workflows: - Turn recurring anti-patterns into deterministic lint checks. - Replay a known plan while changing a s… |
| `2026-08-18-syncs-and-ab-testing-200-agents` | C | extend owner | P2 | 4/5 | Two separate mechanisms are presented within this episode: 1. **Repeated representation experiments.** Codex implements Ticketmaster features in repe… |
| `2026-08-25-software-factory-design-patterns` | C | already present | P3 | 3/5 | Divide the execution stack into compute, development environment, harness, and orchestration. The orchestration layer dispatches work and reacts to f… |
| `2026-09-01-code-mode-extensible-software` | C | do not build | P3 | 3/5 | Two distinct implementations are described: 1. **HumanLayer:** generate JavaScript that performs multiple document operations against a restricted, Y… |
| `2026-09-08-hands-on-software-factories` | C | already present | P2 | 3/5 | The described workflow is declarative end-state planning → agent implementation → automated verification and human PR review → failure postmortem → G… |
| `2026-09-15-performance-engineering` | C | candidate slice | P2 | 4/5 | Three distinct techniques are described: 1. Separate architectural design, deletion of obsolete paths, and replacement implementation; use fresh cont… |
| `2026-09-22-all-about-jev` | C | do not build | P3 | 2/5 | The described model returns probabilities over predefined choices, scores, or yes/no outputs. Application code consumes these results for routing or… |
| `2026-09-29-gtm-engineering` | C | extend owner | P2 | 3/5 | The reported progression is: 1. Deterministic SQL behind retrieval tools. 2. Agent-generated SQL, with several queries per question and a model asses… |
| `2026-10-06-building-a-memory-pipeline` | C | candidate slice | P2 | 4/5 | The described pipeline separates three learning sources: discovered facts, human corrections, and subagent research/file-reading behavior. A cheaper… |
| `2026-10-20-software-factory-debate` | D | do not build | P3 | 0/5 | Unknown. Metadata announces a moderated debate involving Warp’s CTO and agentic coding; it does not specify a software-factory design. [upstream: `20… |
| `2026-10-27-building-evals-on-production-data` | D | already present | P3 | 1/5 | The announcement proposes using production data to design and extend evaluations. It does not specify sampling, labeling, privacy controls, failure s… |
| `2026-11-03-using-ai-for-writing` | D | do not build | P3 | 1/5 | The announcement promises a workflow for producing readable technical posts with AI. It supplies no concrete drafting, critique, revision, or factual… |
