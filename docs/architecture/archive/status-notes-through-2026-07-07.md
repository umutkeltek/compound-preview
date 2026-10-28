Archived per-feature status notes moved out of the hot `docs/architecture/status.md` surface on 2026-07-07 to keep it under its line budget. These are current-behavior notes (not dated proof-journal entries); moved verbatim, unreworded, in original order.

Review surface doctor note: strict architecture doctor fails if `compound reviews` loses decision/action coverage counters, text output, tests, or this public status contract.

Workbench held-condition evidence note: held review cards are reopened only in projection when deterministic receipt or run-observation refs name the card's gate/blocker/verification refs. The card gets `condition_status=condition_stale`, the triggering evidence ref, matched condition refs, and a corrected `next_action`; the source WorkEnvelope remains `held` until an explicit operator-provenance lifecycle action releases or cancels it.

Capability evaluation review-card note: `compound reviews --area capability-evaluation-intake --json` attaches read-only `decision_card` and guarded actions for pending capability evaluation reviews. Choices are `keep_needs_review`, `activate`, `reject`; all point to editing review YAML then running `compound capability evaluation-review`, and none import graph offers or execute tools.

External repo evaluation-state note: `compound capability external-intake --source-list <file>` treats review-required capability-evaluation artifacts as real state: they count as `review_required`, leave `evaluation_next`, and block graph import as `blocked_on_evaluation_review` until an edited evaluation review emits a reviewed manifest.

Memory candidate card-context note: `compound memory candidates --json --include-evidence` keeps evidence hidden by default, then exposes bounded `evidence.quote` plus source metadata and exact-cluster `believed_before` current promoted claim context for operator review cards. This is a generic CLI/read-model door; renderers decide labels and actions.

Review decision-card note: `compound reviews --json` attaches read-only `decision_card` blocks to pending panel verdicts with a plain claim, lens questions, panel lean, choice effects, and canonical command hints. It does not record review, adjudicate memory, or let renderers write state directly.

Capability source freshness note: `compound capability tool-boundary-doctor` reads capability source-authority records and reports `source_freshness` with per-source age, max-age, and refresh command; stale imported MCP/tool/skill snapshots warn through the existing boundary doctor instead of creating a separate registry.

Memory graph zoom note: `internal/mem` exposes `RecallGraphOpts` with optional `Direction` (`up`/`down`) and relation-kind filters; `compound memory graph` accepts `--direction up|down` and repeatable/comma `--kind`, so `part_of` chains can zoom generic-to-specific without pulling sibling subtrees or unrelated relation kinds.

Recall events note: `internal/mem/recallevents.go` owns append-only `ledger/recall-events.jsonl` recall-usage history with required RFC3339 `ts`, required `query_hash`, additive appends, missing-file empty reads, blank-line skip, and malformed-line errors naming the line. It records that recall happened; it does not gate truth, salience, or reranking.

Knowledge conversation-edge note: `fixtures/knowledge/chatgpt-conversation-export.json` proves private `source.kind=chatgpt` conversation exports enter the generic `compound.knowledge_export.v0.1` door with item event time, private evidence/candidate visibility, 4000-char bounded quotes, exact reimport reuse, and cross-source echo folding; no conversation-specific core package or importer branch exists.

Paper selection edge note: `tools/paper_selection_edge.ts` is a rented Bun edge that selects a bounded, diverse batch from Hugging Face Daily Papers and Semantic Scholar citation-ranked search, emits only the generic `compound.knowledge_export.v0.1` envelope, and has temp-home idempotency proof through `compound knowledge import`; no paper-specific core package or memory promotion path exists.

Epistemics doctrine note: `docs/architecture/epistemics.md` is a doctor-parseable doctrine table mapping principles to existing `internal/` object families and `compound` proof commands; strict architecture doctor fails missing/short/phantom rows so doctrine cannot drift back to prose-only guidance.

Adaptation steering note: when a private plan exists, strict architecture doctor checks that the canonical goal prompt routes old operating-system material through context-aware adaptation metadata (trigger, silence, proof, retirement) and that duplicate prompt-named files stay pointer-only, so old instruction bodies cannot reappear as a second authority.

Decision panel review note: `compound panel plan --target-kind decision_candidate` can review parked `decision` candidates with generic panel runner before any source mutation, doctrine change, or claim promotion. The decision-specific `decision-falsifier-review` lens adapts reasoning checks into lifecycle review gates: falsifier, review trigger, owner/source authority, second-order impact, and anti-pattern redirects. Decision source artifacts still belong to `compound decisions candidate-plan`; Panel records review evidence only, never decision truth.

Panel blocked-run guidance note: panel runs that stop at no-lane, metered-budget, budget-limit, or target-failure boundaries persist `next_action` alongside `stopped_reason`, so `panel show` / `--json` consumers know whether to refresh route evidence, create a bounded new plan, or inspect route evidence before retrying.

Panel target-provenance note: memory-candidate panel targets produced by `knowledge_extraction` include read-only `source_provenance` joined from `inbox/knowledge-extractions/kx_*.json` (evidence ref, source kind, request state, candidate count, and extraction route model/usage), so lenses judge evidence strength from the extraction request without mutating candidate files or memory truth.

Panel spend-gate note: `GatewayCaller` panel runs refuse metered route lanes (`cheap`, `premium`, etc.) unless the plan carries an explicit `--max-usd-micros` cap; budget-only caps still require `--max-items` or `--max-tokens` because lane cost metadata is not yet enforceable. Fixture/local non-spending callers are unaffected.

Panel local-context preflight note: `GatewayCaller` can preflight LM Studio `/api/v0/models` metadata and block before any model call when a served local model is loaded below required context. By default the required context is the model `max_context_length`; `compound panel run --host-context-limit N` or `COMPOUND_PANEL_HOST_CONTEXT_LIMIT=N` caps the requirement for task/host-adaptive runs while preserving the local-routing owner for actual reload/unload. Local LM Studio default URLs (`127.0.0.1:1234`, `localhost:1234`, `[::1]:1234`) enable the preflight automatically and persist `next_action` for reload/verify retry.

Local-model lifecycle note: `compound local-models advise` is Compound's non-mutating policy/advice surface for required local model context and idle unload candidates. Actionable findings carry structured `lifecycle_intent` records (`owner=local-routing`, local reversible mutation, adapter delegation) so callers can hand off load/reload/unload requests without scraping human steps. For constrained/shared hosts, `--conserve-memory` plus `--max-loaded-models N` emits `pipeline_advice` with sequential/batched lease steps and release intents, so a runner can do one model first, then the next, instead of keeping every required model resident. It reads the local provider base URL through the configured local routing CLI by default, then uses runtime metadata as evidence; the routing layer remains owner for provider routing and any actual load/reload/unload command path.

Panel route-proof note: panel apply receipts include route evidence refs from lens and judge calls as proof refs, so route outcome review can resolve receipt-linked route evidence or report explicit ambiguity for multi-call panel runs instead of losing the execution trail.

Panel run-apply note: `compound panel run --plan ... --fixtures DIR --apply` or gateway flags with `--apply` executes the dry run first and calls `panel.Apply` only after `verdicts_recorded`; standalone `--apply` keeps the existing apply-only path.

Panel domain-lens note: `panel lenses` and `panel plan` accept `--domain` / `--task` context. Known domains such as `paid-app`, `social-distribution`, and `memory-ingestion` select reviewed domain lenses; unknown domains keep generic lenses but write a deterministic `capability_observation` gap candidate instead of silently treating generic coverage as complete.

Skill-surfacing feedback readiness note: consumed skill-surfacing feedback reports with no remaining/proposed/all-miss items are terminal in `orient --operator` only when matching source/no-promote evidence exists for their content hash; unconsumed feedback still surfaces actionable.

Public export status note: `compound release export-public [--target archive.tar.gz]` writes a public source archive from `git archive HEAD`, not a raw working-tree zip, and fails if tracked export entries include private/runtime roots (`docs_private/`, `.claude/`, `.compound-private/`, `Plans/`, `memory/`, `bin/`), Finder metadata, nested `.git`, root built binary, or `*.private.md`.

Dispatch-plan source-packet note: `compound work dispatch-plan --source-packet packet.md` is read-only packet proof preview and writes no WorkEnvelope state. Packet import stays explicit through `work import-packet` or mutating dispatch producers like `dispatch-claim` / `dispatch-step`, which then claim through the normal policy path.

Private plan-draft hygiene note: root `Plans/` is ignored like `docs_private/`; operator packet drafts stay private overlay and do not enter public proof/redact changed-file scans until deliberately promoted into tracked docs or fixtures.

Route outcome status note: `compound route outcome-review` resolves existing joins by `--route-id`/`--route-evidence` plus `--receipt`/`--receipt-id`, or by `--work-id` when the work event log points to a receipt and the route ledger links that receipt/work. Supported joins record as route evidence, not review debt; remorse/failed joins remain the only path to route observations/no-promote.

Workbench route outcome note: `compound workbench --json` attaches compact `route_outcomes[]` summaries from `reviews/route-remorse` to matching work/receipt items and adds `route:outcome` observatory tags, so route evidence joins are visible without opening review artifacts.

<!-- archived 2026-07-21 from status.md (status-hot-path cap) -->
Capability semantic surfacing note: orientation can opt into breadth-resolved node embeddings; failures degrade to literal surfacing and record `semantic_lane=degraded` rather than blocking capability offers.
