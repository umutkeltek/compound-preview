# Knowledge Intake

One generic door turns captured content from any medium into adjudicable meaning.
Edges capture; Compound owns the import contract, evidence ledger, candidates,
source registry, idempotency, and promotion boundary.

## Envelope

`compound.knowledge_export.v0.1` is strict-decoded. Unknown fields fail import.

```json
{
  "schema_version": "compound.knowledge_export.v0.1",
  "source": {
    "kind": "arxiv",
    "ref": "arxiv:cs.AI+cs.CL",
    "captured_at": "2026-07-02T12:00:00Z"
  },
  "privacy": "public",
  "producer": {
    "kind": "edge-script",
    "name": "arxiv-capture",
    "version": "0.1"
  },
  "items": [
    {
      "ref": "arxiv:2506.12345v1",
      "url": "https://arxiv.org/abs/2506.12345",
      "title": "Synthetic Agents Learn Evidence Discipline",
      "authors": ["A. Researcher"],
      "published_at": "2026-06-30",
      "topics": ["cs.AI"],
      "content": "Captured artifacts become evidence before memory claims.",
      "lang": "en"
    }
  ]
}
```

Rules: `source.kind` is a source slug, `source.captured_at` is RFC3339, every item
needs `ref` and either `content` or `content_ref`, and `privacy` is `public` or
`private`. Privacy defaults to `public` except source kinds whose material is
operator-private by default. Item-level source occurrence metadata can use
`occurred_at` or an edge alias such as `bookmarked_at`; import records that as
the evidence event time. Item-level `captured_at`, `author_handle`, `media_refs`,
`operator_note`, and `candidate_classes` are optional generic edge metadata.

## Import

`compound knowledge import <export.json>` does this per item:

1. **Evidence**: records a bounded `EvidenceItem` with source kind/id, quote,
   event time, actor, and propagated visibility. Evidence is citable raw material,
   not truth.
2. **Parked source-index metadata candidates**: records deterministic reference
   metadata candidates for fields such as title, authors, author handle,
   published/occurred/captured time, topics, media refs, operator note, and URL.
   These share the evidence ref and normalized `knowledge:<kind>:<ref>` entity.
3. **SourceObservation/no-promote candidates when source classes are declared**:
   classes such as `reference`, `design_taste_example`, `product_idea`,
   `technique`, and `market_take` map to existing candidate inboxes plus
   `NoPromoteDecision` rows. This makes curation reviewable without creating
   source truth, product changes, or promoted memory.
4. **Extraction request**: writes `inbox/knowledge-extractions/kx_*.json`
   with state `pending`. Extraction is a later gated step; it produces candidates
   only.
5. **Echo folding**: the same canonical identity, URL first then ref, seen again
   within seven days records distribution evidence without duplicate candidates
   or extraction requests.
6. **Source registry and idempotency**: records first-seen source rows and writes
   import rows to `ledger/knowledge-imports.jsonl` plus the rebuildable index.

No import path writes promoted claims. Promotion remains an explicit governed
operator/review action.

## Edges

Capture edges stay dumb: fetch or export bounded source material, shape the
envelope, and hand the file to `compound knowledge import`. They do not dedupe,
summarize, write into `COMPOUND_HOME`, adjudicate, or promote.

The public fixture set includes synthetic exports under `fixtures/knowledge/`.
Real source captures belong outside the public repo and must enter only through
the same envelope.

## Downstream

Extraction bridge -> parked annotated-evidence candidates. Memory adjudication,
panel review, and product/distiller plans consume candidates through their own
gates. `compound orient --task` serves only promoted results.

## Applications

- **Paper syntheses** (`knowledge/oo-harness-arxiv-2610-28380.md`): one external paper, read
  in full, enters twice through the same door — first as an `arxiv` export (`compound
  knowledge import`, evidence plus parked candidates), then as a curated page whose
  frontmatter claims map the paper onto Compound's spine (`compound memory import-page`,
  candidates on a `curated_page` basis). The page's "slices" section names the executor
  whose next branch changes, which is the test that a synthesis is more than a document.
- **Expertise packs** (`knowledge/expertise-packs.md`): "everything a world-class X
  knows" compressed into a `source.kind: expertise-synthesis` bundle of `technique`
  items. Model-synthesized profession knowledge enters as low-trust evidence, grounds
  through extraction, and promotes only on a governed basis (review → curated page) —
  the worked example that model output cannot self-certify into canon.
