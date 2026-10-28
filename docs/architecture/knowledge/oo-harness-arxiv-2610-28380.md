---
schema_version: compound.knowledge_page.v0.1
id: kg-oo-harness-paper-20261022
title: OO-Harness (arXiv 2610.28380) — harness hierarchies, cluster-scoped evolution, trajectory-informed routing
entity: oo-harness
owner: architecture
visibility: contextual
claims:
  - predicate: harness_definition
    object: an agent harness is the executable code around a fixed language model — prompts, tools, context management, completion checks, middleware, file backends, subagents — so harness improvement is a code-evolution problem, not a weight-tuning problem
    observation_date: "2026-10-20T00:00:00Z"
    claim_type: architecture
    confidence: 0.95
    derivation: extracted
    promotion_basis: curated_page
    qualifiers:
      domain: harness_evolution
      source: arxiv-2610.28380v1
    evidence_quote: Agent harnesses are executable code that specifies prompts, tools, and control flow around a language model.
  - predicate: global_harness_regresses
    object: one global harness edited for all tasks regresses previously effective behavior on other task groups; the fix is specialized harnesses that share common code through inheritance plus a router that picks one per task
    observation_date: "2026-10-20T00:00:00Z"
    claim_type: constraint
    confidence: 0.9
    derivation: extracted
    promotion_basis: curated_page
    qualifiers:
      domain: harness_evolution
      source: arxiv-2610.28380v1
    evidence_quote: A single global harness can struggle with heterogeneous tasks that require different execution strategies.
  - predicate: hierarchy_representation
    object: specialized harnesses are subclasses of a base or of another specialized harness; a subclass overrides methods, calls super() to keep inherited behavior, and may add methods; every node is a complete runnable harness and the selected class executes within one instance
    observation_date: "2026-10-20T00:00:00Z"
    claim_type: architecture
    confidence: 0.95
    derivation: extracted
    promotion_basis: curated_page
    qualifiers:
      domain: harness_evolution
      source: arxiv-2610.28380v1
    evidence_quote: Each specialized harness inherits from the base or an existing specialized harness, overriding inherited methods or adding new ones.
  - predicate: cluster_by_execution_need
    object: discovery tasks are clustered by shared execution needs visible in base-run records (missing writes, arithmetic verification, exploration budget, injection defense, self-verify gates), not by domain; clusters are re-examined each iteration and may be split or merged
    observation_date: "2026-10-20T00:00:00Z"
    claim_type: architecture
    confidence: 0.9
    derivation: extracted
    promotion_basis: curated_page
    qualifiers:
      domain: harness_evolution
      source: arxiv-2610.28380v1
    evidence_quote: On AutomationBench, clusters capture failures and corrective needs visible in trajectories.
  - predicate: evolution_step_shape
    object: one evolution iteration is — pick one cluster and one parent class, state one shared harness-fixable mechanism, write one subclass with a JSON manifest {op, cluster, parent, name, seam, hypothesis}, evaluate it only on that cluster's tasks, accept only if the cluster score improves; repeated rejections lower the cluster's search priority
    observation_date: "2026-10-20T00:00:00Z"
    claim_type: architecture
    confidence: 0.95
    derivation: extracted
    promotion_basis: curated_page
    qualifiers:
      domain: harness_evolution
      source: arxiv-2610.28380v1
    evidence_quote: Evaluation runs directly on the target cluster without routing, avoiding rerunning the full discovery set for every candidate.
  - predicate: two_lesson_stores
    object: execution records are distilled into two separate stores — design lessons (useful changes and failed approaches, consumed by code revision) and routing lessons (when a harness applies, consumed by selection); both accumulate during discovery and are frozen before deployment
    observation_date: "2026-10-20T00:00:00Z"
    claim_type: architecture
    confidence: 0.95
    derivation: extracted
    promotion_basis: curated_page
    qualifiers:
      domain: harness_evolution
      source: arxiv-2610.28380v1
    evidence_quote: Design lessons describe useful changes and failed approaches; routing lessons describe when a harness applies.
  - predicate: router_inputs_and_gate
    object: the router runs with tools disabled and sees only the task, a bounded digest of the observable base trajectory (tool calls, write counts, error patterns; no grader output), harness descriptions of at most 120 characters, and at most 12 frozen routing lessons of at most 180 characters each; it returns JSON {choice, rule, why}, chooses base unless a mechanism clearly fits, and a specialist needs self-reported confidence of at least 0.7
    observation_date: "2026-10-20T00:00:00Z"
    claim_type: architecture
    confidence: 0.95
    derivation: extracted
    promotion_basis: curated_page
    qualifiers:
      domain: harness_evolution
      source: arxiv-2610.28380v1
    evidence_quote: Choose base when no mechanism clearly fits or a learned anti-pattern applies.
  - predicate: base_reuse_is_the_safety_valve
    object: when the router chooses base, the base result is reused without a rerun; in one run 44.7 percent of tasks reused base, specialization was directed at tasks with weak base runs (44.40 mean) while strong base runs (77.15 mean) were kept, and on an out-of-distribution split the router kept base on every task and preserved performance where a learned global harness lowered PASS
    observation_date: "2026-10-20T00:00:00Z"
    claim_type: constraint
    confidence: 0.9
    derivation: extracted
    promotion_basis: curated_page
    qualifiers:
      domain: harness_evolution
      source: arxiv-2610.28380v1
    evidence_quote: Keeping Base as a routing option allows OO-Harness to handle unfamiliar tasks without requiring the use of specialization learned on different application families.
  - predicate: trajectory_routing_beats_task_only_routing
    object: routing on the observable base trajectory beats routing on the task description alone by 12.64 points on AutomationBench and 4.37 PASS on Toolathlon-GYM, but only 0.53 on Workspace-Bench where the requested output type is already in the task text; the gain depends on whether the clusters encode execution failures or task types
    observation_date: "2026-10-20T00:00:00Z"
    claim_type: constraint
    confidence: 0.9
    derivation: extracted
    promotion_basis: curated_page
    qualifiers:
      domain: harness_evolution
      source: arxiv-2610.28380v1
    evidence_quote: The information needed for routing depends on how the evolution agent groups tasks and specializes harnesses to address them.
  - predicate: inheritance_beats_copying
    object: organizing evolved code by inheritance beat flat copy-based reuse by 4.96 rubric points and both beat starting each candidate from base; reusing evolved code matters and the inheritance structure itself adds value
    observation_date: "2026-10-20T00:00:00Z"
    claim_type: constraint
    confidence: 0.85
    derivation: extracted
    promotion_basis: curated_page
    qualifiers:
      domain: harness_evolution
      source: arxiv-2610.28380v1
    evidence_quote: OO achieves the highest micro rubric pass at 40.56%, exceeding Flat by 4.96 points.
  - predicate: headline_results
    object: with the same fixed executor (gemini-3.5-flash), evolution agent Claude Opus 4.8 and router Claude Sonnet 5, OO-Harness reached AutomationBench mean 77.41 vs base 61.62 (+15.79 absolute, fewest steps), Workspace-Bench 39.48 vs 25.45, Toolathlon-GYM PASS 37.45 vs 31.15, beating Meta-Harness, Agentic Harness Engineering and Adaptive Auto-Harness; best-of-K and sequential self-refine test-time scaling did not consistently help
    observation_date: "2026-10-20T00:00:00Z"
    claim_type: status
    confidence: 0.9
    derivation: extracted
    promotion_basis: curated_page
    qualifiers:
      domain: harness_evolution
      source: arxiv-2610.28380v1
    evidence_quote: OO-Harness achieves the best performance among compared methods, with an absolute improvement of up to 15.79% over the base harness.
  - predicate: concrete_mechanisms_learned
    object: the mechanisms the evolution agent actually wrote were small execution checks — a missing-write completion check that requests another turn when a required mutating call never happened, a near-turn-budget reminder, tool-error guidance for unrecoverable errors, an API error-normalizer with query-repair hints, a resource-manifest for connected services, a formula-literal check and a percent-cell check before finishing a workbook, and an early-finish condition that stops compressing tool output after a first completion attempt
    observation_date: "2026-10-20T00:00:00Z"
    claim_type: architecture
    confidence: 0.9
    derivation: extracted
    promotion_basis: curated_page
    qualifiers:
      domain: harness_evolution
      source: arxiv-2610.28380v1
    evidence_quote: Successive nodes add a missing-write completion check, a near-budget reminder, and tool-error guidance.
  - entity: compound
    predicate: global_lesson_accretion_is_the_regression_the_paper_measures
    object: Compound serves learned lessons mostly as one global layer per harness — the static contract block, the hook-served memory digest and the harness-memory index — which is the single-global-harness shape the paper shows regresses other task groups; the paper's evidence favors cluster-scoped specializations gated by observed execution with the base kept as the default
    observation_date: "2026-10-22T00:00:00Z"
    claim_type: architecture
    confidence: 0.75
    derivation: inferred
    promotion_basis: curated_page
    qualifiers:
      domain: harness_evolution
      source: arxiv-2610.28380v1
    evidence_quote: Specialized changes leave the base and other learned harnesses intact, so new tasks can use the appropriate harness without inheriting unrelated changes.
  - entity: compound
    predicate: router_today_is_task_text_only
    object: every automatic selection in Compound today — capability-class regex rules, routing_defaults per class with quota and latency vetoes, skill surfacing by trigger overlap, memory ranking by token overlap — reads the task text and never an observed run; the per-(lane, class) success posterior that lane beliefs already fold from receipts is not read by the route planner and records class as unknown because Receipt carries no capability_class
    observation_date: "2026-10-22T00:00:00Z"
    claim_type: status
    confidence: 0.85
    derivation: inferred
    promotion_basis: curated_page
    qualifiers:
      domain: harness_evolution
      source: compound-internal-routeevidence-lanebeliefs
    evidence_quote: capability_class is NOT carried on Receipt or on its WorkEnvelope.
  - entity: compound
    predicate: inheritance_seam_exists_but_is_unused
    object: the harness-neutral lifecycle intentions in hookplan (prepare_context, prepare_turn_context, observe_action, guard_mutation, observe_outcome, record_receipt) are the method set a specialized harness would override; skill manifests already carry a specializes field that becomes a graph edge but no constraint reads it, and role overlays are the one working base-plus-patch mechanism and are single-level
    observation_date: "2026-10-22T00:00:00Z"
    claim_type: architecture
    confidence: 0.8
    derivation: inferred
    promotion_basis: curated_page
    qualifiers:
      domain: harness_evolution
      source: compound-internal-hookplan-capgraph-profiles
    evidence_quote: The specialization must retain the generic outcome, proof and authority requirements.
  - entity: compound
    predicate: research_intake_skims_but_does_not_synthesize
    object: the daily research collector captured this paper the day it appeared as one 300-byte abstract among about 160 items, and the digest model proposed nothing from it; turning one paper into evidence and candidates took a manual full read, a knowledge export and a knowledge import, which is the procedure a per-paper synthesis step should run automatically for items that match the harness filter
    observation_date: "2026-10-22T00:00:00Z"
    claim_type: status
    confidence: 0.9
    derivation: inferred
    promotion_basis: curated_page
    qualifiers:
      domain: research_intake
      source: research-collector-raw-2026-10-21
    evidence_quote: "Capture edges stay dumb: fetch or export bounded source material, shape the envelope, and hand the file to compound knowledge import."
---

# OO-Harness (arXiv 2610.28380) — harness hierarchies, cluster-scoped evolution, trajectory-informed routing

Curated from Li et al., *Object-Oriented Harness: Evolving Agent Harnesses through
Object-Oriented Design*, arXiv 2610.28380v1 (UIUC + Google Cloud AI Research, 20 Oct
2026). The page extracts the load-bearing mechanisms as explicit claims and maps them
onto Compound's existing spine; it does not reproduce the paper.

## What the paper shows, in one paragraph

A harness is the code around a fixed model. One global harness, edited for every task,
regresses the tasks it was not edited for. The paper instead grows a *tree* of harness
classes: a base, and subclasses that each add one mechanism (a completion check, a
budget reminder, an error normalizer) and call `super()` for the rest. During training
it clusters tasks by the failure mechanism visible in base-run records, lets a coding
agent write one subclass per cluster, and accepts the subclass only if that cluster's
score improves. At run time a tool-less router reads the task plus a bounded digest of
a cheap base run and picks one class, or keeps the base result. The base is the default;
a specialist needs confidence ≥ 0.7. Everything learned is frozen before deployment.

| Benchmark | Base | OO-Harness | Best prior method |
|---|---|---|---|
| AutomationBench mean | 61.62 | 77.41 | 75.40 (per-domain specialists) |
| Workspace-Bench mean | 25.45 | 39.48 | 38.62 |
| Toolathlon-GYM PASS | 31.15 | 37.45 | 36.23 (global harness) |

Three ablation results matter more than the headline:

- **Routing on the observed base run beats routing on the task text** by 12.6 points
  where clusters encode execution failures, and by only 0.5 where the task text already
  names the output type. The signal that routes is *what went wrong*, not *what was asked*.
- **Inheritance beats copying.** Carrying evolved code forward matters, and organizing it as
  a tree beats flat copies by ~5 points.
- **Base reuse is the safety valve.** Tasks kept on base had strong base runs; tasks sent to
  specialists had weak ones. On out-of-distribution tasks the router kept base everywhere and
  lost nothing, while a learned global harness lost PASS rate.

## Vocabulary map: paper → Compound today

| Paper | Compound today | Where | Gap |
|---|---|---|---|
| Base harness `H0` | One static contract block per harness, hook roster, skills index, harness memory digest | `internal/contract`, `internal/hookplan`, `skills/README.md`, `mem/harness_memory_digest.go` | Flat. The harness registry row is capabilities and limits, not behavior |
| Harness "methods" | Lifecycle intentions: `prepare_context`, `prepare_turn_context`, `observe_action`, `guard_mutation`, `observe_outcome`, `record_receipt` | `internal/hookplan/compile.go` | Not overridable per task class; one plan per harness family |
| Subclass with one override | Role overlays (base + pointer-field patch, single level); skill `specializes` edge (recorded, never read); product-pipeline "generic branch → platform method → product context" doctrine in prose | `internal/profiles`, `capgraph/skillmanifest.go`, `skills/product-pipeline/README.md` | No executable inheritance; `specializes` has no constraint semantics |
| Execution record of a run | ActionIntent + PolicyDecision + HookEvent per tool call; RunAttempt; Receipt; SessionEnd transcript scan (tool calls, bash calls, files changed, skills fired) | `cmd/compound/hook_live.go`, `internal/runattempt`, `internal/hook/session_transcript.go` | No tool-error or exit-code field on any per-call record; no per-run write count (countable from `act_` by mutation class) |
| Observable trajectory digest `ω(t)` | Distiller `LoadRuntimeSources` assembles the chain as prompt input; latenttax `RunMetric` has thrash (denial count), tokens, latency, verdict | `internal/distiller/runtime_sources.go`, `internal/latenttax/metrics.go` | No stored, bounded digest on the receipt; nothing a router can read |
| Task clusters by failure mechanism | Receipt `failure_cause{surface_outcome, mechanism}` aggregated by exact string; `failure_pattern` inbox; negatives keyed by approach + task class | `capgraph/usage.go`, `internal/negatives`, `internal/distiller/failure_rule_plan.go` | Keys are exact strings or slugs; nothing groups runs into mechanisms automatically |
| Evolution agent + acceptance | Director harness-edit: mines misfires, weak bench splits, route remorse; proposes one edit to one surface; before/after bench; accept only on no regression in both splits; operator approval | `internal/director/harness_edit.go`, `internal/bench/bench.go` | Scores on a fixed bench suite, not the target cluster; accepts on "no regression", not "improved" |
| Design lessons (what worked / failed) | Failed: negatives, `failure_pattern`, NoPromote rows fed back as prior signals. Worked: only `validated_by` edges and standing-proof graduation | `internal/negatives`, `internal/writeback` | No store of successful mechanisms with the evidence that proved them |
| Routing lessons (when a harness applies) | Partial: `Node.When/Triggers`, `missed_when` / `false_triggered_by` edges, `FailureRuleDraft.Trigger`, `route_observation` candidates | `internal/capgraph`, `internal/candidates` | All keyed on task text or lane; none keyed on an observed run |
| Router | Regex task classifier → `routing_defaults[class]` → quota / latency / privacy vetoes; skill surfacing by trigger overlap + learned edges; memory ranking by token overlap | `routeevidence/task_classifier.go`, `routeevidence/model_capabilities.go`, `capgraph/capgraph.go`, `contract/contract.go` | Task-text only; no confidence gate; no "reuse base" verdict; the per-(lane, class) posterior from receipts is not read and records class as unknown |
| Frozen caps | Memory 8 refs / 2100 chars, offers 1500 chars, negatives 5 / 900 chars per hook; 5 / 12 / 12 / 5 in the ContextPack | `internal/hook/hook.go`, `contract/contract.go` | Already in the paper's spirit (paper: 12 rules × 180 chars, descriptions ≤ 120 chars, digest ≤ 5000 chars) |

## What this says about Compound

**Observed.** Compound's learned lessons reach executors mostly as one global layer per
harness: the static contract block, the hook-served memory digest, and a harness memory
index that is already over its size cap. Every automatic selection reads the task text.
The one evolve-and-accept loop (director harness edit) scores a global bench suite.

**Interpretation.** That is the single-global-harness shape the paper measures against,
and the paper's evidence says it regresses the tasks a lesson was not written for. The
paper's three moves translate directly: keep the base contract small and default; attach
each learned mechanism to the failure cluster it fixes; let an observed run, not the task
text, decide whether a mechanism applies. The second-order risk the paper does not fully
address is lesson sprawl: Compound's evidence-not-truth gates (candidate → adjudicate →
promote, NoPromote rows as prior signals) are the missing piece on the paper's side, and
they already exist here.

**What is not transferable as-is.** The paper has a grader and 250–300 discovery tasks per
benchmark. Compound has receipts, not scores, and no replayable task corpus. Acceptance
must stay "differential proof + no regression on a replay set" until a scored corpus exists.

## Slices, smallest first

Each slice names the executor whose next branch changes, which is the test for whether
this page is more than a document.

1. **Put `capability_class` on the Receipt.** Lane beliefs already fold a Beta posterior per
   (lane, class) from receipts but record class as `unknown` because neither Receipt nor
   WorkEnvelope carries it. One field, written at close from the route plan, makes the
   existing posterior per-class. Consumer: `internal/lanebeliefs`, then `routeev` cards.
2. **Store a bounded run digest on the Receipt.** `{tool_calls, bash_calls, files_changed,
   writes, denials, repeated_error_patterns, finished_cleanly}` built from the SessionEnd
   transcript scan plus `act_`/`pol_` records, capped at 5,000 characters. This is `ω(t)`.
   Prerequisite: a tool-error / exit-code field on the PostToolUse record, which does not
   exist today. Consumer: the router slice below and failure clustering.
3. **Give `route_observation` a trigger shape.** `{observed: <digest pattern>, choice:
   <skill | overlay | lane | base>, confidence, evidence_refs}`, served in the ContextPack
   under the existing negatives budget, at most 12 rules × 180 characters. Default verdict
   is `base`; a specialist needs confidence ≥ 0.7. Consumer: `compound route plan` and the
   PreToolUse context writer.
4. **Make `specializes` mean inheritance.** When a specialized skill surfaces, its parent's
   outcome, proof and authority requirements are inherited and the child adds one
   mechanism; a child may not surface without its parent being eligible. This is the
   product-pipeline doctrine, moved from prose into a `capabilityConstraints` case.
5. **Cluster-scoped acceptance for director harness edits.** Add a replay set per failure
   mechanism (the `failure_cause` aggregation already groups receipts by mechanism) and
   accept an edit when that mechanism's replay improves and the held-out bench does not
   regress. This is the paper's "evaluate on the target cluster, not the whole set".
6. **Per-paper synthesis step in research intake.** The daily collector captured this paper
   on the day it appeared as one abstract among ~160 items and proposed nothing. For items
   that match the harness filter, fetch the full text, run one bounded synthesis into a
   `compound.knowledge_export.v0.1` envelope, and hand it to `compound knowledge import`.
   That is exactly the manual procedure that produced this page.

Slices 1 and 6 are hours. Slices 2–4 are each one short session. Slice 5 is the real
investment and depends on 2.

## Provenance

- Source: arXiv 2610.28380v1, read in full on 2026-10-22; imported as evidence
  `ev_21a8615683a8` through `compound knowledge import` (parked candidates only).
- Compound mapping: three read-only repo scouts on 2026-10-22, each claim verified against
  the named file before being written here.
- Claims curated by hand; quotes are brief and verbatim.
