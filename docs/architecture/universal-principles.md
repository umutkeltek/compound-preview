---
id: universal-principles
title: Adaptive AI Operating System — Universal Principles (Law Layer)
kind: policy
type: principles
status: accepted
version: v2
date_stated: 2026-06-21
pairs_with: [COHERENCE-CHARTER.md, OPERATING-MAP.md, ETHOS.md]
tags: [principles, laws, routing, memory, skills, governance, evolution, solo-builder-os]
note: >
  Canon, but LIVING — versioned forward, contradictable with evidence (memory updates, it does not
  rule). Canonizing this is a FLOOR, not a cage: it encodes the standard ("what good looks like"),
  while exploration stays liquid (A6 creative idempotency). Compression-handle phrases are preserved
  verbatim on purpose. The Open Frontier is unresolved by design.
falsifier: A future Compound contract proves the Tier-0 Laws no longer guide routing, memory, governance, execution, and evaluation across harnesses.
review_trigger: Re-check when Compound changes its Tier-0 constitution renderer, authority import path, or instruction generation contract.
owner: compound
scope: public-core
---

# Adaptive AI Operating System — Universal Principles for Routing Intelligence

**What this is:** A portable, fully generic principle set for designing AI systems that treat
intelligence as infrastructure. No personal or setup-specific content — it transfers to any builder,
team, or agent. Read the thesis first, then the five layers, then the Laws. The *Open Frontier* at the
end is unresolved on purpose. (v2 — pairs with a separate execution layer that instantiates these laws
in a concrete system; that layer is where the specifics live, never here.)

**Core thesis:** AI systems should route intelligence the way infrastructure routes power — use the
cheapest sufficient capability, preserve context, adapt over time, and automate anything deterministic.
Intelligence should be applied only where uncertainty, synthesis, or judgment exists; everything else
should become infrastructure.

The whole thing compresses to five subsystems working as one:
**model routing + memory governance + skill graph + deterministic execution + continuous evaluation** —
all sitting on top of one architectural commitment: *own the substrate, rent the intelligence.*

## How to read this
- Each principle = a statement + why it matters (the failure it prevents).
- Principles are grouped into five layers; the layers are a system, not a menu.
- The **Laws** are the irreducible compression. If only one section survives transfer, keep that one.
- Vocabulary is deliberate. *skill graph*, *Decision OS not Agent OS*, *evidence not truth*,
  *controlled creativity*, *creative idempotency*, *state delta*, *orientation < pointer < merge < proof*,
  *the map is not the work* are compression handles — preserve them verbatim so meaning reconstructs cleanly.

## Layer A — Routing & Efficiency · *where intelligence is spent*
- **A1 — Job-relevant efficiency, not benchmark efficiency.** Evaluate a model by what the *job* costs (tokens, latency, energy, quality, error rate, human-correction cost), not benchmark scores. Cheapest = finishes the job *well* with fewest tokens.
- **A2 — Route to capability, don't be loyal to a model.** Cheap models for trivial/deterministic; strong models for ambiguity/synthesis/architecture/debugging/high-risk. Both directions waste money.
- **A3 — Make routing a shared map, not private trial-and-error.** Model-for-task balance should be discoverable guidance. (Meta-cost that routing is itself judgment is unresolved — Open Frontier.)
- **A4 — Deterministic work must escape the LLM loop.** Generate a script/tool once, run it forever. Intelligence stays where it belongs.
- **A5 — Optimize total cost, not token cost.** Tokens, latency, money, human review, maintenance, complexity are all costs. Smoothness/reliability outrank raw token cost — a cheap path that fails costs more.
- **A6 — Creative idempotency.** Harden the proven into cheap deterministic reusable form; keep the unproven liquid, multi-angle, exploratory. Timing is the discipline: standardize too early kills discovery, too late cements a mess. A4 = *what* to make deterministic; this = *when*, and leave exploration alone.

## Layer B — Memory & Knowledge · *what the system knows*
- **B1 — Memory must be recall-aware.** Needs trigger rules for *when to recall/ignore/update/delete*; incoming info is *adjudicated* (reject/park/promote/operationalize), not merely stored. Knowing **when** is the hard part.
- **B2 — Memory is evidence, not truth.** Stored = claim, not true. Every memory carries source, confidence, last-verified date, contradiction history, review/expiry policy.
- **B3 — Detect drift and update in batches.** Know when facts/docs/skills/procedures go stale, refresh in batches rather than rot in place.
- **B4 — Load context by relevance, not by location.** Ask "what is relevant now?", not "which folder?". Semantic, perspective-weighted retrieval.
- **B5 — Learn system-wide from local events.** A local discovery may matter elsewhere; operating history is itself a *behavioral dataset* (episodes, workflows, contradictions, taste, decision habits, failure modes) to be mined, not a log to search.

## Layer C — Skills & Capability · *what the system can do*
- **C1 — Skills are a graph, not a flat list.** A skill can conditionally trigger others; agent selects path by context/goal/risk/results. *Controlled creativity* — branching with guardrails. External skills = evidence/inspiration, adapted by fit, never adopted wholesale.
- **C2 — Capabilities are discovered, not hardcoded.** Dynamically learn what each tool/model/skill can actually do, don't bake assumptions and go stale.
- **C3 — Own the substrate, rent the intelligence.** *(keystone)* An owned core — memory, evidence, routing, control/promotion logic, writeback protocol — is never delegated. Executors (models/agents/tools/providers) are swappable; the spine is not; no executor becomes source of truth. Every executor runs the *same* operating loop.
- **C4 — The execution harness is LLM-agnostic.** Models are replaceable workers behind stable interfaces/adapters. Provider-neutrality is the *consequence* of C3, not a goal. Agent-agnosticism is proven by *replacement*, not by adapters existing: swap the primary executor mid-task and work continues from spine context, or it's orientation (D6), not proof (D5).

## Layer D — Governance & Provenance · *how the system stays trustworthy*
- **D0 — Govern in proportion to risk.** *(the meter)* Receipts, observability, claims, approval all cost — spend in proportion to reversibility and blast radius, never uniformly. Tier 0 read/search/classify → automatic, sampled. Tier 1 reversible local edit → automatic, delta + rollback. Tier 2 bounded spend/side effect → pre-authorized budget + full receipt. Tier 3 public/destructive/financial/security → explicit *or pre-authorized* approval, full trace + preview + recovery. D1/D3/D4 describe the Tier-3 ceiling, not a universal floor.
- **D1 — Every action leaves a receipt — proportional to its tier.** What/why/on what evidence/which model-skill-memory-tool/what changed. Tier-0 sampled, Tier-3 fully traced. Invariant = accountability, not "full forensic receipt for everything" (which bankrupts the system).
- **D2 — Preserve explicit uncertainty.** Distinguish *I know / I think / I suspect / I don't know* and carry it through the whole chain. Treating all info as equally certain is a primary failure source.
- **D3 — Humans own irreversible decisions — by setting the envelope, not by being paged.** Authorization is mostly *standing*: grant scope/budget/blast-radius/reversibility ceiling in advance; inside it the agent acts without asking. Escalate only to *leave* the envelope. A nagging agent is *violating* D3, not enforcing it.
- **D4 — Observability before autonomy.** Build the ability to explain *why* before granting autonomy. If you can't explain it, don't automate it. Pair passive observability with *active self-healing* (detect → diagnose → propose → auto-fix only reversible known failures → escalate); never silently rewrite canonical policy/memory rules/approval boundaries.
- **D5 — Proof is a state delta, not a document.** A report describing success ≠ success. Verify by what changed — what retired, got greener, is now recallable, whether surface count went down — never by an artifact's existence.
- **D6 — Integration has levels: orientation < pointer < merge < proof.** Four distinct states. Most "done" claims are the first or second wearing the costume of the fourth. Name the level you're actually at.
- **D7 — Claim before you touch.** An executor leases the surface it will modify, so authority stays singular and two agents never write conflicting truth into the same place.

## Layer E — Evolution & Intent · *how the system grows*
- **E1 — Separate intent from execution.** "I want 10k installs" is intent; SEO/Reddit/ASO/partnerships are execution. Hold goals independently or execution ossifies into rigid habits.
- **E2 — Every repeated success becomes infrastructure — when it *earns* it, not at a rep count.** Promote when recurrence is likely, variance understood, exceptions bounded, expected savings > build+maintenance+coordination+failure cost, and there's a removal path. Default: *"pays for itself within the next 3–5 uses."* The artifact encodes the *standard*, not just steps.
- **E3 — Every repeated failure becomes a rule.** Recurring mistakes generate guardrails/checks/policies/validation. Learn from failure as aggressively as success.
- **E4 — The decision is the primary artifact.** Projects/models/skills/tools churn; decisions are durable. A mature system is a **Decision OS, not an Agent OS**. A decision only counts if it carries its falsifier + review trigger (expected outcome, the assumption that would invalidate it, when to recheck).
- **E5 — Default to subtraction.** Net progress = fewer owners, fewer stale paths, fewer duplicate configs, fewer re-litigated decisions, smallest fix that works. A system that only adds is decaying while looking busy.
- **E6 — The map is not the work.** When analysis/setup/synthesis stands in for execution, force a bounded *kill / keep / merge* — don't produce more map. The most dangerous failure mode because it feels like progress.

## The Laws · *irreducible core*
1. Discover reality before acting.
2. Own the spine; rent the executors.
3. Route to the cheapest sufficient capability.
4. Store evidence, not truth.
5. Recall only what is relevant.
6. Keep humans on irreversible decisions. *(by setting the envelope, not by being paged: standing grants of scope, budget, and blast radius are the mechanism; inside the envelope the agent acts without asking and escalates only to leave it. A nagging agent is violating this law, not enforcing it.)*
7. Convert repetition into infrastructure.
8. Learn from failures as aggressively as successes.
9. Make everything observable.
10. Prove the change; don't trust the report.
11. Subtract by default.
12. Separate intent from execution.
13. Optimize for decisions, not agent behavior.
14. Don't let the map become the work.
15. The control plane must earn its keep. *(every move yields an outcome delta or a system delta; system-only deltas need a payback argument; cap the OS tax. Law 14 is the trigger, 15 is the meter.)*
16. Govern in proportion to risk. *(intensity of receipt, claim, approval, review scales with reversibility and blast radius — never uniform.)*

## Open Frontier · *unresolved on purpose — do not transfer as solved*
- **The routing regress.** Choosing which model to use is itself judgment; a naive router burns a strong model deciding whether to use a weak one.
- **Receipt overhead.** D1 ("every action leaves a receipt") is in tension with A4 (anti-grind). Provenance can cost more than the work it records. Sharpened, not solved, by D5.
- **Drift-detection cost.** Knowing when things go stale (B3) requires continuous re-verification, which isn't free. Missing: the trigger policy (event-based? scheduled? confidence-decay?).
- **One retrieval mechanism or two?** "Recall-aware memory" (B1, *when*) and "relevance over location" (B4, *how*) may be one weighted-perspective system or two.
- **Who classifies "irreversible"?** D3 puts humans on irreversible decisions, but *labeling* one irreversible is itself a decision — regress unless default is: on uncertainty, escalate.
- **The hardening threshold.** *(Downgraded.)* E2 replaces rep-count with expected-value. Residual: recurrence probability and input variance are estimates; a confident-but-wrong estimate cements noise.
- **Subtraction vs. resilience.** E5 assumes duplication is waste — but some duplication is fault-tolerance. Missing: how to tell dead duplication from load-bearing redundancy.

**The shape underneath several** — routing regress, receipt overhead, drift cost, hardening threshold, claim-leasing overhead — is one question: *when is the meta-work worth its cost?* Partial answer: proportional governance (D0/Law 16), the OS-tax ceiling (Law 15), the expected-value test (E2). Honest residual: all three rest on cost/probability *estimates*, and the policy that estimates those well is still the real prize.

---

*Snapshot v2 — a living set, meant to be versioned forward. When a frontier item gets resolved, move it up into a numbered principle and bump the version. Pairs with the execution layer (OPERATING-MAP / Solo-Builder Execution Layer), which instantiates every law above in a concrete system.*

## Writing principles that route behavior · *the quality bar*

A principle earns injection into agent context only if it:
1. **names its trigger** — when it applies;
2. **changes a specific decision** — an agent acts differently because it read it;
3. **carries the failure it prevents** — its falsifier;
4. **scales with risk** (Law 16) — it never adds uniform caution;
5. **contains no moral vocabulary without an operational test** — "be careful / honest / responsible" is noise unless it names the check that satisfies it.

A principle that fails the bar is calibration debt: hosted models arrive with default
conservatism baked in, and unoperational caution text compounds it — the system asks
instead of works. The injected surface must state what an agent may do *without* asking
(the envelope) at least as clearly as what requires approval. Structural minimum enforced
at import: an instruction rule declares when it fires and at least one boundary naming
what it prevents.
