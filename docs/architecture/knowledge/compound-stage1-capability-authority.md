---
schema_version: compound.knowledge_page.v0.1
id: kg-compound-stage1-capability-authority-20260630
title: Compound Stage 1 Capability Authority
entity: compound
owner: architecture
visibility: contextual
claims:
  - predicate: capability_authority_surface
    object: compound orient and compound capability authority consume the capability graph as the authoritative Stage 1 capability surface
    observation_date: "2026-06-30T00:00:00Z"
    claim_type: architecture
    confidence: 0.91
    derivation: deterministic
    promotion_basis: curated_page
    qualifiers:
      domain: capability_authority
      stage: "1"
    evidence_quote: Installed agent steering consumes Stage 1 capability authority directly.
---

# Compound Stage 1 Capability Authority

Commit `9c5af9d` closed the capability authority steering adoption by updating
[status.md](../status.md) to record that installed agent steering now consumes
Stage 1 capability authority directly.

The relevant surface is `compound orient --harness <harness> --task "<task>"`
and `compound capability authority`. Imported generated skill and tool views stay
compatibility or evidence inputs; the capability graph is the Compound authority.
