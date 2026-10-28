# Compound as a Substrate Kernel (target topology)

This extends `../ARCHITECTURE.md` and `../LAYERS.md`. Those describe Compound as an *information* kernel.
This describes the larger target: Compound as a **unified meaning + execution substrate** — the owned core
that lets agents, models, tools, memory, skills, registries, product loops, evidence, cost, taste, and
autonomy compose without fragmenting.

Read this as the **map, not the work.** The topology is the destination; execution is one thin vertical
slice at a time (`../roadmap/phases.md`), built **envelope-first** (`runtime.md`), not memory-first. A
substrate diagram is not a substrate. Build one concern, prove it with a command + fixtures, retire the
parallel handling it replaces.

## The one invariant: one meaning owner

> There can be many sources, many indexes, many harnesses, many projections. **There must be one meaning
> owner.** That owner is compound.

The disease in a setup that grew by need is not that the parts exist — it is that **too many surfaces can
act like the source of truth** (source, generated views, runtime config, hooks, receipts, memory pages, DB
projections, decisions all drift out of sync). compound's job is to *reduce active meaning owners to one*,
not to be a better connector between five systems forever.

So the relationships are migration relationships, not permanent peerage:

```text
a private brain corpus     → compound's MEANING LAYER, imported/classified/materialized/governed/owned
a control-spine repo       → compound's instruction/work/hook/policy/capability SOURCE MATERIAL
a legacy vector/fact store  → a BRIDGE/INDEX adapter (retrieval + history), never the future authority
a model-traffic gateway    → ExecutionRouteEvidence joined into compound transitions (execution-plane.md)
harnesses (Claude/Codex/…) → CLIENTS of compound
```

A private corpus is not an external overlay sitting *next to* compound. It is **compound running with
private data**. The public repo must not *ship* that data (a distribution boundary), but a running instance
*owns* it internally. That is how compound is both "for everyone" and "does not leak the private corpus."

## What the substrate does (six jobs, at once)

```text
1. Know what exists.            (identity + registries)
2. Know what can act.           (capability graph: skills/tools/MCPs/models/harnesses)
3. Know what matters now.       (memory + orientation, relevance-gated)
4. Route the right work.        (job + routing engine: cheapest sufficient capability)
5. Convert outcomes to learning.(evidence → memory/skills/rules/product hypotheses)
6. Reduce future entropy.       (evaluation/hardening + subtraction)
```

## Two planes: control + execution

compound is the **control / meaning plane**. Model traffic flows through a separate **execution / data
plane** (an OpenAI/Anthropic-compatible gateway). Siblings, not nested. Compound owns semantic execution
profiles, work, policy, and outcome receipts; the gateway owns live providers/models/accounts, codecs,
circuits, route resolution, and enforcement. A `RouteDecision` is execution-route evidence, **not** proof of
task success — it becomes one projection in a `Transition`. Detail: `execution-plane.md` and
`information-custody.md` §7.

## Core doctrine: own the meaning, rent everything replaceable

```text
OWN  (the spine, never delegated):
  identity, the trace/evidence ledger, the MEANING CORPUS, memory promotion, registries, routing policy,
  work identity, receipts, promotion/supersession, writeback, cost/evidence accounting, doctrine.

RENT (disposable, swappable behind adapters):
  vector/search/fact engines — a LEGACY store is a bridge/index, never the authority; the arrow is
    compound → materialized objects → engine index, never engine → truth → compound reads
  models / providers (behind the execution gateway)
  tools, MCPs, browsers
  harnesses (Claude Code, Codex, OpenCode, ChatGPT, local LLMs, future agents)
```

Agent-agnosticism is proven by **replacement** (checkpoint → swap executor mid-task → verification succeeds
from spine context), not by an adapter interface existing.

## Three places (this is not optional)

```text
repo            = public product source: code, schemas, contracts, docs, tests, fixtures
COMPOUND_HOME   = the private runtime body: custodied corpus bodies, ledgers, work, context, receipts,
  (`.compound`)   transitions, registries, indexes, projections, inbox candidates, artifacts
private overlay = ingress paths, credentials/secret refs, accounts, and operator-specific adapter bindings
```

Private runtime state and durable bodies are written to `COMPOUND_HOME`, **never into the public source
tree**, and operator paths are **never hardcoded into the public core**. An overlay is ingress/configuration,
not a permanent body or meaning owner. Layout + resolution: `runtime.md`; physical and semantic boundaries:
`information-custody.md`.

## The runtime grammar is the envelope spine (first-order)

Memory is **one** vertical slice, not the substrate. The thing every harness and hook actually runs through
is the typed envelope spine, hung off one root `work_id`:

```text
Intent → EffectiveContract → ContextPack → WorkEnvelope → ActionIntent → PolicyDecision
       → Run/ToolEvents/RouteEvidence → Receipt → Transition → projections
```

This is what stops "Claude knows one version, Codex another," and what lets governance be *enforced at
adapters* (a mutation requires a `PolicyDecision`), not merely asked for in a prompt. Full schemas and the
hook-as-client design: `runtime.md`.

### The spine, concretely (what's built; what to adapt vs. port)

"The spine" is not a new thing to build — it is what these slices already are:

| Spine role | compound (Go, public core) |
|---|---|
| typed runtime grammar | `internal/envelope` (P1) |
| orientation compiler ("what applies now") | `internal/contract` → EffectiveContract + ContextPack (P2) |
| one work identity | `internal/work` → WorkEnvelope lifecycle (P3) |
| governance gate | `internal/policy` → ActionIntent → PolicyDecision (P4) |
| per-harness capability model + lifecycle | `internal/harness` (HarnessRegistry) + `internal/hook` (P5) |

A mature **TS control-spine already implements this same pattern** — an `effective-contract` compiler,
`gen-*` generators that project one owned source into per-harness views, `check-*` coherence gates, and
per-harness capability graphs. compound is its **Go-native, public-core generalization**, not a rewrite from
zero: the load-bearing contracts are extracted (above); the operator's actual steering/topology/lanes stay in
the **private overlay** (P7), never the public core.

**Adapt the pattern, do not port the code.** The one piece compound does not yet have is the TS spine's
**projection + coherence step**: turning the owned source into actual per-harness config *files* (a served
`AGENTS.md`/hooks/MCP definitions) plus a drift gate proving generated == source. Today compound produces
runtime *objects* (a ContextPack); generating *served config* is a later slice (P7 overlay + a `spine
project`/`spine check` step). Porting the TS spine wholesale would drag the personal control plane into the
public core (a leak and a lock-in) — so compound rents it as source material and owns the generic engine.

## Kernel services (target) mapped onto existing layers

The 6 layers in `../LAYERS.md` (Memory = building; Authority, Decisions = early importers; Procedures,
Permissions, Doctrine = planned) are not replaced — these services are how they express at runtime:

| Service | Question | Maps onto / extends |
|---|---|---|
| Identity kernel | what *is* this thing? (survives path/host moves) | scope resolver + entity model |
| Registry kernel | what exists, and when to use it? | Authority → registry-of-registries |
| Trace / evidence ledger | what actually happened? | ledger (the ground; everything else a view) |
| Meaning / memory | what is known / recalled / retired? | Memory layer (absorbs the brain corpus) |
| Capability graph | what can act/think/judge/execute? | new (skills/tools/MCPs/models/harnesses, one graph) |
| Orientation router | what context *now*? | new (EffectiveContract + ContextPack; phases "Orientation") |
| Work / routing | smallest unit of work, and who does it? | Procedures (WorkEnvelope + JobUnit fields) |
| Execution broker / gateway | act without becoming the truth? | new (rented executors + exec plane) |
| Governance / autonomy | act without chaos? | Permissions (ActionIntent → PolicyDecision → StandingGrant) |
| Product feedback | work → product learning? | new (Authority for products + hypotheses) |
| Evaluation / hardening | what becomes infrastructure / a rule? | new (Law 7 + Law 8) |
| Observability / control surface | what's happening, cost, next? | new (operator cockpit; projections only) |

**The registry is not flat inventory.** It distinguishes **inside-world** (this system's own projects/tools/
products/deployments/configs/constraints) from **outside-world** (frameworks/libraries/methods/models/expert
skills/repos), joined by a **synthesis layer** answering *"given THIS job/context/cost/risk/evidence, which
option is currently preferred, and why"* — not a static "X good / Y good" — and it adapts over time.

**Memory is inside the substrate, not above it, and it is one slice of it.** The trace/evidence ledger is the
ground *and the authority* — promoted claims/positions are durable ledger objects, not rebuildable views.
What *are* views over the ground are projections, timelines, and indexes; product loops, skill-mining, and
self-healing read from the ledger, they do not own it.

**Proof semantics — never collapse the greens.** Four kinds of "green" verify different things and must not be
conflated: a *dashboard* green = a rebuildable view is fresh; *work* green = a verified state delta; a
*memory* green = adjudicated AND recall-tested; a *product* green = an observed signal with a hypothesis and
confounders. A fresh dashboard is not done work; a stored claim is not adjudicated memory.

## The reconciliation principle (why this won't become overbuild)

The substrate vision is seductive and the operator's #1 failure is overbuilding. Hold both:

- The topology is the **target** — captured so it is never re-derived (the rediscovery tax).
- **Execution is thin vertical slices, envelope-first.** Build the `.compound` home + envelope spine + hook
  fixtures **before** memory ingestion; memory is a later slice. Never build a kernel service ahead of a
  slice that needs it.
- **Extract contracts, reject mass.** From the control-spine repo, take the *load-bearing contracts*
  (EffectiveContract, ContextPack, WorkEnvelope, ActionIntent→PolicyDecision, hook-as-client, the
  distillation roles) — never the sprawling runtime tree.
- Default to subtraction; every move = an outward/memory/rule delta, or it is telemetry. Every migrated
  surface gets a **retirement condition**, or compound becomes one more layer.

Construction order: `../roadmap/phases.md`.
