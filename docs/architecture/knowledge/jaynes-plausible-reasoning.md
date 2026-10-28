---
schema_version: compound.knowledge_page.v0.1
id: kg-jaynes-plausible-reasoning-20260728
title: Jaynes Plausible Reasoning — formal grounding for evidence-not-truth
entity: plausible-reasoning
owner: architecture
visibility: contextual
claims:
  - predicate: evidence_updates_plausibility
    object: verification of a consequence makes a hypothesis more plausible, never proven; the weak syllogisms are an extension of deductive logic with new content, so evidence corroborates a claim and only a separate judgment promotes it to accepted truth
    observation_date: "2026-07-28T00:00:00Z"
    claim_type: constraint
    confidence: 0.95
    derivation: extracted
    promotion_basis: curated_page
    qualifiers:
      domain: memory_doctrine
      source: jaynes-2003-ch1
    evidence_quote: The evidence does not prove that A is true, but verification of one of its consequences does give us more confidence in A.
  - predicate: plausibility_is_conditional
    object: every plausibility is A given B, conditioned on stated evidence; there is no freestanding plausibility, so every stored claim must carry the provenance it was derived from and legitimately moves when the conditioning evidence changes
    observation_date: "2026-07-28T00:00:00Z"
    claim_type: constraint
    confidence: 0.95
    derivation: extracted
    promotion_basis: curated_page
    qualifiers:
      domain: memory_doctrine
      source: jaynes-2003-ch1
    evidence_quote: The plausibility that the robot assigns to some proposition A will, in general, depend on whether we told it that some other proposition B is true.
  - predicate: consistency_desiderata
    object: a consistent reasoner must satisfy path independence (every way of reasoning out a conclusion gives the same result), total evidence (never arbitrarily ignore relevant evidence), and equivalence (equivalent states of knowledge get equivalent plausibility assignments)
    observation_date: "2026-07-28T00:00:00Z"
    claim_type: constraint
    confidence: 0.95
    derivation: extracted
    promotion_basis: curated_page
    qualifiers:
      domain: memory_doctrine
      source: jaynes-2003-ch1
    evidence_quote: If a conclusion can be reasoned out in more than one way, then every possible way must lead to the same result.
  - predicate: mind_projection_fallacy
    object: an epistemological statement disguised as an ontological one — treating one's own state of knowledge as a fact about external reality; an index or store describes a state of knowledge, never the world, so its contents are evidence rather than authority
    observation_date: "2026-07-28T00:00:00Z"
    claim_type: constraint
    confidence: 0.95
    derivation: extracted
    promotion_basis: curated_page
    qualifiers:
      domain: memory_doctrine
      source: jaynes-2003-ch1
    evidence_quote: To interpret the first kind of statement in the ontological sense is to assert that one's own private thoughts and sensations are realities existing externally in Nature.
  - entity: compound
    predicate: doctrine_grounding
    object: Compound's evidence-not-truth doctrine implements Jaynes' plausible-reasoning desiderata — evidence corroborates candidates without promoting them, recall is provenance-conditioned, synthesis reconciles contradictions for path independence, and bounded candidate confidence realizes degrees of plausibility as real numbers
    observation_date: "2026-07-28T00:00:00Z"
    claim_type: architecture
    confidence: 0.85
    derivation: inferred
    promotion_basis: curated_page
    qualifiers:
      domain: memory_doctrine
      source: jaynes-2003-ch1
    evidence_quote: These rules will be deduced from simple desiderata which, it appears to us, would be desirable in human brains.
---

# Jaynes Plausible Reasoning — formal grounding for evidence-not-truth

Curated from E. T. Jaynes, *Probability Theory: The Logic of Science* (Cambridge
University Press, 2003), chapter 1, "Plausible reasoning". This page extracts the
chapter's load-bearing principles as explicit claims; it does not reproduce the
chapter.

## Why this source matters to Compound

Chapter 1 is a formal ancestor of Compound's memory doctrine. Jaynes builds an
idealized reasoner ("the robot") whose rules are deduced from desiderata rather
than adopted ad hoc, and shows that ad-hoc inference schemes "must generate
demonstrable inconsistencies when we try to apply [them] beyond some restricted
area" — the argument for one meaning owner with explicit rules.

## The principles extracted above

**Weak syllogisms.** Deduction's two strong syllogisms are joined by weaker ones:
observing a consequence of A makes A *more plausible*, never proven. This is the
candidate lifecycle — evidence corroborates, adjudication promotes. Jaynes is
explicit that this is "not a 'weakened' form of logic; it is an extension of
logic with new content".

**Conditional plausibility is provenance.** All plausibility is written A|B —
conditioned on stated evidence, never freestanding. A recalled claim is A|B where
B is when and how it was written; new evidence legitimately moves it. This is why
recall output is a provenance-tagged hypothesis, not law.

**The consistency desiderata (IIIa–IIIc).** Path independence maps to synthesis
contradict/reconcile; total evidence maps to recall-before-work and
zero-result-is-not-proof; equivalence maps to idempotent, content-addressed
candidates and dedup.

**The mind projection fallacy.** Mistaking an epistemological statement for an
ontological one. In Compound terms: source stores and indexes are evidence, not
authority — an index's belief about the world is a state of knowledge, not the
world.

**Degrees of plausibility as real numbers (Desideratum I).** Grounds bounded
confidence on candidates and claims. Later chapters (Cox's theorem, ch. 2) derive
that any consistent combination rule is isomorphic to probability — relevant if
candidate-confidence combination ever moves beyond hand-tuning.

## Provenance

- Source: Jaynes 2003, ch. 1 (operator-supplied copy; not tracked here).
- Claims were curated by hand from the chapter text on 2026-07-28; quotes are
  brief and verbatim.
