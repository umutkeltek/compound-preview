---
schema_version: compound.knowledge_page.v0.1
id: kg-synthetic-visual-standard
title: Synthetic Visual Standard
entity: product:synthetic-ios
owner: product
visibility: contextual
claims:
  - predicate: app_store_visual_standard
    object: screenshots should show the actual workflow state being sold
    claim_type: constraint
    confidence: 0.84
    qualifiers:
      domain: app_store_visuals
  - predicate: feedback_loop
    object: compare visual changes against provider signal evidence before assuming causality
    claim_type: architecture
    confidence: 0.78
---

# Synthetic Visual Standard

Synthetic product pages should be treated as curated source material only.
The importer may create reviewable memory candidates, but it must not promote
claims or mutate the source document.
