# Expertise Packs

Compress the compressed latent knowledge of a profession into a **reusable,
adjudicated, executor-consumed asset** — without letting a model's synthesis become
authority. An Expertise Pack is "everything a world-class X knows" shaped so it flows
through the same generic door as any other captured knowledge: evidence first,
promotion earned, ambient recall last.

This is the compound-native answer to "turn expertise into documentation future
agents can reuse." It is deliberately **not** a folder of manuals. A 500-page manual
in `docs/` is inert (no executor consumes it) and off-doctrine (it treats model
output as truth). See `AGENTS.md` §"Evidence, not truth" and §"a move that produces
'another document' failed unless a real executor will consume it."

## What a pack is

A pack is a `compound.knowledge_export.v0.1` bundle (see `knowledge-intake.md`) where
each item is one self-contained expertise nugget — a mental model, failure mode,
heuristic, rule of thumb, anti-pattern, diagnostic step, or checklist entry — dense
enough that its own text can ground a claim.

```json
{
  "schema_version": "compound.knowledge_export.v0.1",
  "source": { "kind": "expertise-synthesis",
              "ref": "expertise:ios-performance-engineering:v1",
              "captured_at": "2026-07-07T12:00:00Z" },
  "privacy": "public",
  "producer": { "kind": "model", "name": "claude-opus-4-8", "version": "expertise-pack-0.1" },
  "items": [
    { "ref": "expertise:ios-perf:mental-model:frame-loop",
      "title": "Mental model — the render loop and hitches, not 'dropped frames'",
      "topics": ["ios", "performance", "mental-model"],
      "candidate_classes": ["technique"],
      "content": "A world-class iOS performance engineer thinks in the render loop ...",
      "lang": "en" }
  ]
}
```

Shipped packs (public fixtures, synthetic-safe — no personal path, product, or
credential — so they ship with the public repo). All are swept by
`TestAllExpertisePacksImportAsEvidenceOnly`, which globs `expertise-*.json` and
holds every current and future pack to the evidence-not-authority boundary:

| Pack (`source.ref`) | Fixture | Items |
| --- | --- | --- |
| `expertise:ios-performance-engineering:v1` | `fixtures/knowledge/expertise-ios-performance-export.json` | 14 |
| `expertise:app-store-growth:v1` | `fixtures/knowledge/expertise-app-store-growth-export.json` | 18 |
| `expertise:llm-agent-systems:v1` | `fixtures/knowledge/expertise-llm-agent-systems-export.json` | 17 |
| `expertise:vc-due-diligence:v1` | `fixtures/knowledge/expertise-vc-due-diligence-export.json` | 15 |

Each pack covers one profession/decision-domain as mental models, heuristics,
failure databases, rules-of-thumb with concrete numbers, diagnostic if-X-then-Y
flows, hidden/tacit knowledge, and internal monologues — every item one dense,
independently groundable paragraph.

### The one field that carries the doctrine

`source.kind: "expertise-synthesis"` marks the material as **model-synthesized**. It
is registered at the default low `trust_posterior` (0.5) and never promoted by
import. `producer.kind: "model"` records which model compiled it. The synthesis is a
low-trust source, not a citation of external reality.

### Profession-OS taxonomy → candidate classes

The user-facing pack structure (mental models, failure modes, hidden heuristics,
rules of thumb, tradeoffs, patterns, anti-patterns, checklists, diagnostic
flowcharts, internal monologues, case studies) all map to one intake class:

| Pack section kind | `candidate_classes` | Maps to |
| --- | --- | --- |
| heuristic / rule of thumb / pattern / anti-pattern / diagnostic / checklist / mental model | `["technique"]` | `MethodUpgrade` candidate, `method:<slug>` entity |
| named source / case study / canonical reference | `["reference"]` | `CapabilityObs` candidate |

Both map with `PromoteAllowed: false` — the import parks a reviewable no-promote
decision, it does not create source truth. Every pack item is `technique` unless it
is literally pointing at an external named source, which is then `reference`.

## The two gates (why this respects the architecture)

A pack is model output, so two independent gates stand between it and canon. Both are
enforced in code, both have falsifier tests
(`internal/knowledge/expertise_pack_test.go`).

1. **Grounding gate (faithful-to-the-pack).** Extraction (`compound knowledge
   extract`) only shapes a memory candidate if its quote appears verbatim in the pack
   text; anything else is rejected as a hallucination and recorded as residue. This
   stops a downstream model from inventing "expertise" the pack never stated.
   *Caveat that matters:* grounding proves faithful-to-**source**, not
   faithful-to-**truth**. The pack can be confidently wrong and still ground cleanly.
   Truth is handled by the next gate, not this one.

2. **Authority gate (earned, not asserted).** A grounded extraction candidate carries
   **no** `promotion_basis`, so `compound memory adjudicate --verdict promote` refuses
   it: `cannot promote without valid promotion_basis`. Model synthesis cannot
   self-certify into canon. A claim promotes only on a governed basis:
   `explicit_confirmation` (a human decided), `curated_page` (a reviewer curated it),
   `decision_record`, `runtime_proof`, or `trusted_deterministic_import`. For a pack,
   the path is **review → curate the keepers into a knowledge page → promote**.

The net effect: a pack floods the ledger with *candidate* expertise cheaply, and a
human/review decides the small durable subset that becomes ambient. The model
compresses; the operator certifies.

## End-to-end path (proven with real state deltas)

```bash
export COMPOUND_HOME="$(mktemp -d)/home"

# 1. Import: evidence + parked no-promote candidates + pending extractions. No authority.
compound knowledge import fixtures/knowledge/expertise-ios-performance-export.json --json
#   → 14 evidence, 14 technique no-promote decisions, source trust_posterior 0.5,
#     ledger/claims.jsonl absent.

# 2. Extract: grounded memory candidates (each heuristic quotes its own pack text).
compound knowledge extract --pending --fixtures <essence-fixtures-dir> --json
#   → candidate state=parked, entity=knowledge:expertise-synthesis:<ref>, NO basis.
#     An ungrounded essence line is rejected here (grounding gate).

# 3. Review + curate the keepers into a knowledge page (this IS the promotion basis).
compound memory import-page curated/ios-perf-hitch-model.md   # basis=curated_page
compound memory adjudicate <candidate> --verdict promote      # → immutable claim + Transition

# 4. Consume. Both are the SAME promoted-only ledger view (contract.PackWithCapabilities):
compound memory recall "ios-performance"                      # human / CLI
compound orient --harness claude --task "reduce iOS frame hitches"   # agent-facing ambient recall
```

Live demo of exactly this chain: `scripts/proof/expertise-pack-e2e.sh`.

Two consumer caveats (verified against `internal/contract` + `internal/mem`):
- **Relevance, not just promotion.** A promoted claim surfaces in `orient`/hook only
  if it is relevant to the task's tokens/entities (or breadth-ranked via
  `COMPOUND_MEM_ENGINE_BIN`). Promotion is necessary, not sufficient — scope pack
  entities/objects with the tokens the consuming task will actually use.
- **Positions need a fold.** Claims are live immediately; the folded `position` view
  reflects a fresh promotion only after `compound memory fold`.

## Authoring a new pack

1. Pick a profession/decision-domain (`iOS performance`, `App Store growth`, `VC
   due-diligence`, `what a principal Anthropic engineer notices`). One pack per
   `source.ref` (`expertise:<domain>:v<n>`).
2. Generate items at frontier quality. Each `content` is one dense, self-contained,
   **groundable** paragraph (≤ 4000 chars — the evidence quote bound). Write it so a
   single sentence can serve as a verbatim quote for a heuristic.
3. `candidate_classes: ["technique"]` for know-how, `["reference"]` for named
   sources. Keep it public-safe (synthetic examples, no private data) if it ships in
   the public repo; real operator packs enter through the same envelope as a private
   overlay.
4. Import → extract → review. Promote only the durable subset, via a curated page.
5. Scale by repeating — the mechanism is fixed; only the content changes. The pack
   list (profession OSes, decision libraries, failure databases, rubrics, internal
   monologues) all ride these rails.

## Retirement condition

Per `AGENTS.md`, every surface names how it retires or compound grows a layer.

- **The generic-door usage is retired** (this doc collapses to a pointer) if intake
  grows a dedicated typed `expertise-pack` adapter that supersedes the
  `source.kind: expertise-synthesis` + `candidate_classes: [technique]` convention.
- **A specific pack version is retired** when a newer `:v<n+1>` supersedes it, or when
  a real captured source (docs, papers, field telemetry) covers the domain better than
  synthesis — at which point the synthesis source's promoted claims should be
  re-grounded against the real source and the synthesis claims superseded.
- **A promoted pack claim is retired** through normal ledger supersession when field
  reality contradicts it — the pack is low-trust by construction, so contradiction
  wins with reason.
