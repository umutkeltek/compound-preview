# Compound Layers — object families inside the one spine

> **The normalizing sentence every other doc points back to:**
>
> **Compound is one architecture — the envelope spine. The spine defines lifecycle, execution
> order, authority, proof, and anti-fragmentation. The "layers" are not a second architecture:
> they are the object families the spine's transitions create, own, and adjudicate. Rented
> engines and executors sit at the edge; the compounding loop feeds objects back in.**

> **Hard rule — read it literally:**
> **Layers are not services, not phases, not owners, and not an alternate architecture. They are
> object families processed by the envelope spine.**

There is **one** architecture here: the **Runtime / Envelope Spine.** An earlier "information
kernel" generation is not a peer architecture and not an "Axis B" — its concerns survive only as
**object families** that flow through the spine. When a doc, package, or command seems to belong
to "the other architecture," it belongs to **an object family the one spine processes.** See
[`architecture/status.md`](./architecture/status.md) for which package/command is canonical vs
retired/importer.

---

## The spine (the architecture)

The typed runtime grammar every harness, hook, and run flows through, hung off one root
`work_id`. This is what defines lifecycle, authority, and proof:

```text
Intent
→ EffectiveContract      what applies right now (kills global instruction bloat)
→ ContextPack            frozen context an agent consumes (records included AND excluded)
→ WorkEnvelope           one root identity unit of work (the JobUnit)
→ ActionIntent           proposed mutation, pre-policy
→ PolicyDecision         allow / allow_with_obligations / hold / deny (enforced, not asked)
→ Run / ToolEvent / RouteEvidence
→ Receipt                how a run is learned from (state_delta, cost, next_consumer)
→ Transition             the proof apex — state change + projections
```

**Packages:** `internal/{arch, contract, envelope, harness, home, hook, policy, schema, work, writeback}`
— the ratcheted spine allowlist; `arch.Classification` (`internal/arch/arch.go`) is the source of
truth. Built P0–P5.
Full schemas: `architecture/runtime.md`. Proof object: `architecture/transition.md`.

---

## Object families (what the spine processes)

These are the durable object kinds compound is the **authority** for. This is the old
"information kernel," recast: **not a co-equal axis — object families inside the spine.** Each
family must answer four questions, or it is not coherent:

1. **Created/updated by** which spine transition?
2. **Owned by** which ledger / inbox / projection?
3. **Promote / park / reject / retire** — what adjudicates it?
4. **Retires** which old surface?

| Object family | Objects | 1 · Created/updated by | 2 · Owned by | 3 · Adjudicated by | 4 · Retires |
|---|---|---|---|---|---|
| **Memory** — knowledge objects | Evidence, Claim, Position, Pattern | distiller intake seam → `Adjudicate`; Receipt→Transition (memory projection) | `ledger/claims,edges`; `inbox/memory-candidates` | candidate-first: reject\|park\|promote\|no_promote; supersede, never delete | retired Gen-1 memory packages + top-level memory-era verbs; canonical surface is `compound memory ...` |
| **Authority** — external-state objects | Entity, RegistryEntry, RuntimeBinding, ServiceContract | source adapter → candidate → adjudication; host/path move updates bindings | `ledger/entities`; registries; `inbox/*-candidates` | promote/park/reject; `ProjectionFreshness` for derived views | infra-os runtime truth (when `runtime doctor` answers host/service/deploy/data/health) |
| **Decisions** — committed-choice objects | Decision (+ `DecisionLifecycle`) | decision adapter → `inbox/decision-candidates`; status changes via transition | `ledger`; `inbox/decision-candidates` | promote/park/reject; status proposed→accepted→superseded→deprecated | decisions repo's status semantics, claim-flattening |
| **Procedures / Capabilities** — reusable-action objects | Capability nodes, procedures, scripts, InstructionRules (the CapabilityGraph) | `CapabilityObservation` (intake) → `missed_when`/`false_triggered_by`; strict `SkillManifestAdapter` imports external/OS skill manifests into the same graph; `CapabilityEvaluationAdapter` imports scored capability-evaluation records as evidence-only `eval` nodes plus `evaluated_by`/positive `validated_by` edges; harness-compatible capability and InstructionRule offers surface into ContextPack | `registries/capability-{nodes,edges}`; `inbox/capability-observations` | operationalize/park; `surfaces_with`/`validated_by`/`evaluated_by`/`retired_by` edges; evaluation records cannot install, retire, execute, or surface as capability offers; needs_review/deprecated/retired do not surface | flat capability lists are retired; the graph is the public surface |
| **Permissions** — policy/autonomy objects | PolicyDecision, AutonomyEnvelope, StandingGrant | ActionIntent → PolicyDecision (spine); grants set the envelope | `ledger` (policy decisions are append-only) | the policy evaluator (deterministic); humans own irreversible | ad-hoc prompt-obedience gating |
| **Doctrine / Product / Taste** — durable preference/product objects | Doctrine, Product, TasteProfile, Hypothesis, ProductSignal, ProductSourceIssue | taste/product observation (intake) → candidate/source issue; ProductSignal→review-plan→Hypothesis review→Transition | `ledger`; `inbox/{taste-observations,product-hypotheses}` | operationalize/park/review; hypotheses resolve supported\|refuted; source issues close by review | bland-automation / repo-summary "project pages" |

Detail per object: `architecture/contracts/README.md`. Memory slice in depth: `memory-architecture.md`.
Recurring analytical/audit/optimization prompts are Procedures/Capabilities nodes on this spine, not a
new home: `architecture/recurring-work-as-capability.md`.

> **Enforced, not just narrated:** `internal/arch.Classification` now distinguishes `spine` (the
> runtime grammar itself) from `object-family` (the families the spine processes, above), so not
> everything reads as equally authoritative. Spine membership is an allowlist ratcheted by the
> architecture doctor (`spineCore`): a package cannot quietly join the authoritative spine — a new
> package is classified as an object family, leaf, or importer, and the doctor fails on drift.

---

## The rented edge *(NOT a layer, NOT an object family)*

Engines and executors sit behind adapters. They index, retrieve, route, execute, import, or
render — they **never own meaning, promotion, visibility, or supersession.**

```text
MemoryEngine            turbovec / existing vector+fact index — breadth, not authority
model gateway           a model-routing gateway — route evidence, not task success
LLM providers           swappable
harnesses               agent CLIs (Claude Code, Codex, OpenCode, …) — clients
MCPs / tools / CLIs      on-demand capabilities
source adapters         read existing files in place, emit candidates/evidence only
analytics APIs          app-store / revenue / product-analytics feeds — product feedback
```

Agent-agnosticism is proven by **replacement** (one harness checkpoints, another resumes from
spine context), not by "adapters exist."

---

## The compounding loop *(how object families get created and updated)*

What makes the system *compound* instead of *accumulate* — it turns spine events into objects:

```text
Trace / Receipt / HookEvent / RouteEvidence / ProductSignal
→ TraceDistiller
→ RetrievedPriorState          distill against canon, not blind
→ CandidateSet                 all candidate-first, typed fan-out
→ DistillationCritic           new? supersedes? contradicts? lower-quality? become skill/rule/eval?
→ AdjudicationDecision         reject | park | promote | operationalize | retire
→ object families (above)
→ ContextPack / Capability surfacing / Policy / Hypothesis
→ future WorkEnvelope
→ Receipt → Transition
```

Model extracts; **code decides.** Nothing reaches ambient recall unadjudicated.

---

## The two seams — where objects meet the spine *(read this twice)*

Object families connect to the spine at **two** points, not one:

1. **Intake seam** — `Trace → Distill → Candidate → Critic → Adjudication → object family`.
   A spine event *becomes* an object.
2. **Proof seam** — `WorkEnvelope → Receipt → Transition → projections` back into
   memory / capability / product / cost / risk. A run *proves* an object changed future behaviour.

**Transition is the proof apex — distillation is not.** If objects only entered at distillation,
compound would be a knowledge-ingestion tool. Its product is a **state-transition substrate**:
storing a claim or a timeline does **not** prove relevance-gated recall or a behaviour change — a
Transition does.

```text
Distillation = the intake seam.
Transition   = the proof seam.
Both are required.
```

---

## Cross-cutting invariants

- **Provenance** — every durable object is citeable.
- **Visibility** — privacy and eligibility are filtered before recall (`source_class`).
- **Time** — claims are superseded without destroying evidence (MVCC-by-fold).
- **Placement** — every object has one canonical lookup path; cwd is a hint, never authority.
- **Subtraction** — every object family names the old surface it retires (column 4); adding one
  must retire scattered handling, not create another parallel home. Every move ends in one
  `move_type` (`outward_delta | memory_delta | rule_delta | control_plane_repair | audit_only`);
  `audit_only` does not count.

## Build discipline

Build one object family into the existing spine, prove it with command fixtures,
then **retire** the surface it replaces (its column-4 entry). Code retirement is gated on
**test coverage first**; Gen-1 memory already crossed that gate and is now retired. Do
not start a new service because an object kind feels new, and do not let a second front door
(doc, package, or command) be *presented as canonical* once its replacement is named — that is
how fragmentation survives while everyone believes it was solved.
