# Memory Layer Study

This file records product lessons from private dogfooding and external memory
products without making either one public authority.

## Keep

- **Hybrid retrieval behind an adapter.** Keyword, vector, graph, and recency
  signals are useful breadth tools, but the engine remains rented.
- **Provenance fields.** `valid_from`, `valid_until`, `superseded_by`,
  confidence, and source references are the right shape.
- **Portable storage.** The core should run locally, rebuild indexes, and keep the
  ledger as the source for derived knowledge objects.
- **Structured cues.** Recall improves when information has deliberate placement
  and entity/topic cues instead of only semantic similarity.

## Reject

- **Summary as memory.** A summary may be evidence, but it is not canon.
- **First/last-message distillation.** Sessions need whole-conversation treatment
  when the task is to preserve decisions and evidence.
- **Async-only truth.** A background extractor cannot be the only path to current
  authority.
- **Undifferentiated piles.** Capturing more raw material without promotion,
  supersession, and visibility gates creates a searchable landfill.

## Compound Thesis

Own deep, evidence-backed encoding:

```text
whole source
-> evidence
-> candidate claims
-> promotion / adjudication
-> positions
-> context packs
```

Rent retrieval and model execution. Keep the object lifecycle, visibility,
placement, and current truth inside Compound.

## Alive Memory

Memory must evolve:

- duplicate candidates can be dropped before promotion;
- contradicted or outdated claims can be superseded;
- superseded claims stay in history but drop out of current positions;
- context packs should prefer active positions while citing the evidence chain.

The goal is not a larger archive. The goal is recall that improves because old
claims can be retired without deleting evidence.
