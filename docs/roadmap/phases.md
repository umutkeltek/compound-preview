# Roadmap — envelope-first construction order

Construction is **vertical slices**, built **envelope-first, not memory-first**. The runtime spine
(`../architecture/runtime.md`) — `.compound` home, typed envelopes, hook fixtures — comes before memory
ingestion, because every adapter (memory, a legacy index, the model gateway, product) needs the same envelope spine; wire
them first and you recreate "powerful parts, weak circulation."

Each phase proves one transition with a real command + real/synthetic fixtures. A phase is done only when
its acceptance test (behavioral, not a document) passes, it ends in a state delta, and it names what it
**retired**. Never build a service ahead of a slice that needs it. The map is not the work.

> Generic roadmap. Operator-specific sequencing + real data sources live in `docs_private/plan/HANDOFF.md`; the old coherent plan is archived under `docs_private/archive/plan/COHERENT-PLAN.md`.

## P0 — `.compound` home (habitat only)
`compound home init/doctor/where`. **Proof:** `home doctor` reports generated state lives under
`COMPOUND_HOME`, not the repo; manifest/ledger/events/work/context/inbox/indexes/projections OK;
private_overlay absent\|present; `generated_state_in_repo_source: false`. Host-aware: the home can record
what runs where (`host_bindings`). No memory, no hooks, no legacy index.

## P1 — envelope schemas
Go structs + JSON validation + round-trip tests for every envelope in `../architecture/runtime.md`.
**Proof:** `compound schema doctor`; `compound envelope validate fixtures/envelopes/*.json` — typed before clever.

## P2 — EffectiveContract + ContextPack  (orientation coherence)
`compound contract compile --harness --cwd --task`; `compound orient`. **Proof:** the contract lists active
instructions (generated vs manual), active hooks, memory-class eligibility, capability availability,
conflicts/unknowns, and source revisions; the **same task in a product repo vs the control-plane repo yields
different ContextPacks** (product repo omits OS-governance context + "all skills/all MCP by default"). The
ContextPack records what was **excluded** and why → enables replacement. Capability availability is graph-backed:
surfaced `CapabilityGraph` nodes enter `compound orient` as generated `capability://...` offers, filtered by
harness compatibility edges when present, never execution.

## P3 — WorkEnvelope lifecycle  (one root identity)
`compound work create/show/claim/advance/close`; `compound transition show`. **Proof:** one stable `work_id`
links intent → context_pack → action → policy → run → receipt → transition. This is the first unification
proof and exercises the `Transition` object mechanically.

## P4 — policy v0  (governance enforced, not asked)
`compound policy evaluate <action-intent>`. **Proof:** local reversible write → `allow_with_obligations`;
forbidden path → `deny`; public/spend/destructive/security → `hold`. No model bypasses this via prompt
obedience; a mutating adapter requires the `PolicyDecision`.

## P5 — hook fixture loop  (no live install yet)
`compound hook {session-start,user-prompt-submit,pre-tool-use,post-tool-use,session-end}` over fixtures.
**Proof:** SessionStart fixture → HookEvent + redacted ContextRequest + EffectiveContract + ContextPack + exact context injection artifact;
live UserPromptSubmit → redacted ContextRequest + EffectiveContract + ContextPack + exact bounded native injection, with opaque persisted intent and no WorkEnvelope (explicit fixture replay retains proposed-work compatibility); PreToolUse → ActionIntent + PolicyDecision; SessionEnd → Receipt + Transition.
Capability-aware per harness (a `HarnessRegistry` records what each actually supports).

## P6 — memory candidate / adjudication  (memory is ONE slice, now)
`compound memory candidate/adjudicate/promote/timeline/doctor`. Inputs: the typed distillation roles —
**SELECT** (user decisions/constraints/preferences), **SEE** (user questions/reactions/uncertainty),
**TELL** (agent conclusions), **EXECUTE** (evidence of performed work) — plus CONTRADICTIONS, taste,
skills, outcomes. These fan out to MemoryCandidate / SkillCandidate / DecisionCandidate / FailurePattern /
TasteObservation / ProductHypothesis / CapabilityObservation — **not just memory** — and **all as
candidates first**. **Proof:** one real trace →
MemoryCandidate → parked\|promoted\|rejected → EvidenceItem/Claim → Transition projection; recalled later
only under relevant scope; no duplicate entity (entity-resolution precedence, `../architecture/contracts/`);
distiller output is never piped straight into canon. `memory doctor` reports *which* hop failed
(capture→promotion→sync→embedding→retrieval→authority), not just "missing"; mining classifies metadata
before content, emits bounded snippets + owner targets (never copies), gates writeback behind an apply
step, and checkpoints/resumes long mines (`../memory-architecture.md`).

**TraceDistiller (the P6 distillation front-end — extends the candidate loop, makes it compounding, not
summaries).** Inputs are spine artifacts — TraceEnvelope + Receipt + ContextPack + HookEvents +
ExecutionRouteEvidence + raw refs — not "a session." Pipeline: load **RetrievedPriorState** → extract a
**CandidateSet** → **DistillationCritic** → Adjudicator → Ledger/Inbox → Transition. The depth that separates
it from summarization:

- **Distill against canon, not blind.** Before extracting, the distiller loads RetrievedPriorState — active +
  superseded claims, open decisions, known failure patterns, existing capability nodes, taste standards,
  product hypotheses, prior distillation false-positives/false-negatives, and the current
  distillation-policy version. The critic then asks, per candidate: is it new? does it supersede / contradict
  / duplicate canon? is it lower-quality than what's already promoted? does it reveal a better procedure?
  should it become a skill/rule/eval, or be parked? — and **does it expose a failure in the distillation
  method itself?**
- **Versioned distillation.** Every candidate cites its raw `evidence_refs[]`, carries a stable deterministic
  id (replay ⇒ identical ids), and stamps the `distillation_policy_version` that produced it — so a method
  change is legible, never silent prompt drift.
- **The distiller observes itself (the compounding loop).** A distiller mistake emits an
  **EvalCaseCandidate** (the distiller's own regression corpus); a discovered better method emits a
  **MethodUpgradeCandidate** (not a silent prompt edit); accumulated false-pos/neg emit a
  **DistillationPolicyPatchCandidate**; a capability miss or false-trigger emits a **CapabilityObservation**
  (feeds P8's `missed_when` / `false_triggered_by` edges). All are candidates — adjudicated, never
  auto-applied (model extracts; code decides; nothing reaches ambient recall unadjudicated — already enforced
  in `internal/mem`).
- **Acceptance (added):** same trace replay ⇒ identical candidate ids; a low-confidence candidate parks,
  never promotes; duplicate-vs-new and contradiction/supersession are detected against RetrievedPriorState; a
  distiller mistake yields an EvalCaseCandidate; a missed/false capability trigger yields a
  CapabilityObservation; a better method yields a MethodUpgradeCandidate (not a prompt edit).

## P7 — private dogfood overlay  (connect the real system as adapters)
Overlay/adapters for the control-spine repo, the brain corpus, the legacy vector/fact store, the model
gateway, product registries, analytics. **Public core sees only adapters; no operator paths in core.**
**Proof:** one real brain decision imports as Decision + EvidenceItem; a project page as a Product/Project
entity + claims; a review as provisional (not ambient); an inbox item as candidate (not canon); an entity
survives a path **and** host move (no duplicate).

## P8 — capability registry import (the Capability Graph, not just a skill graph)
Import skills/tools/MCPs/models/providers/harnesses as a typed **graph**. Nodes are RegistryEntry of kind
`lens | procedure | script | tool | mcp | model_profile | harness | eval | workflow | adapter | doctrine |
taste_standard` — skills are one node type. Edges carry routing AND evolution feedback:
`surfaces_with | requires | conflicts_with | specializes | fallback_for | validated_by | missed_when |
false_triggered_by | retired_by`. The feedback edges close the loop with the distiller: a CapabilityObservation
adds a `missed_when` / `false_triggered_by` edge, evidenced repeated success adds `validated_by`, a superseded
capability gets `retired_by` — so the graph *learns which capability fits which context* instead of a static
list. **Auto-surface, never auto-execute:** a match surfaces capabilities into the ContextPack; execution still
requires WorkEnvelope + ActionIntent + PolicyDecision (no chain auto-runs from a match). **Proof:** `compound
capability query --task "…"` returns relevant capabilities, required tools, MCP-vs-CLI recommendation,
model/harness constraints, cost/risk, counter-skills, and excluded capabilities with reasons (loaded on demand,
not all-by-default) — **without executing them**; and a missed/false trigger recorded as a CapabilityObservation
changes future surfacing probability.

## P9 — product loop
`compound product import/signal/review/source-issue-review/timeline/doctor --writeback`. **Proof:** `compound product signal
<signal.yaml|source-export.yaml>` maps normalized signals and ASC/RevenueCat/PostHog-shaped source exports into the same ProductSignal ledger and emits a product
Transition projection; `compound product signal-doctor <yaml> --record` rejects loose provider shapes into `NoPromoteDecision`; `compound product timeline <product>` joins signals, hypotheses, transition deltas, and open/closed source issues to show change → metric signal → *competing*
interpretations (no causal over-claim) → confidence; `compound product doctor <product> --writeback` turns
pending hypothesis review or open product source issues into WorkEnvelope → ActionIntent → PolicyDecision → OwnerWritebackGate → Receipt →
Transition before any product surface or external account changes; `compound product review <review.yaml>`
records evidence, updates the same ProductHypothesis result without promotion, and emits a product Transition; `compound product source-issue-review <review.yaml>` records closure evidence, preserves the original `NoPromoteDecision`, and stops the issue recurring as open feedback debt.

Current P9 compatibility source-in-place proof also includes `compound product signal --asc-datapoints <json> --product <id> --app-id <asc_id> [--metric <metric>]`
and `compound product signal --asc-sales-series <jsonl> --product <id> --app-id <asc_id> --before <range> --after <range> --change-event <text> [--metric <metric>]`,
which map App Store Connect provider outputs into ProductSignal evidence without promoted claims, hypotheses, or causal attribution.
These adapters remain migration ingress until their required durable inputs have SourceManifest custody and an
isolated restore/consumer replay; source-in-place success is not a P11 retirement proof.

## P10 — execution route evidence (model gateway)
Import `ExecutionRouteEvidence` (`../architecture/execution-plane.md`), read-only first → ingress → relay
canary. **Proof:** a route decision joins a WorkEnvelope + Receipt; **route success ≠ task success** (task
success requires a state delta). Then lane normalization, the join with outcome evidence, eval-as-preview,
and budget enforcement.

## P11 — retirement (the step that gets skipped — don't)
Every migrated surface gets a **retirement condition**: the old writer retires only when the Compound
writer is proven; the old receipt path, hook renderer, dashboard, and recall path each require their own
consumer proof. Follow `../operations/legacy-system-retirement.md`: preservation manifest, real-consumer
replay, no-fallback negative control, quiet window, and verified rollback are required before a legacy
authority is retired. Deletion is a separate operator-gated act. The flat skill/capability matrix is
already retired: generated manifests import into `CapabilityGraph`, harness-filtered offers reach
`ContextPack`, and receipts validate used capabilities back into the graph. **Without retirement,
Compound becomes one more layer.**
Hardening (Law 7/8: repeated success → infrastructure; repeated failure → rule; OS-tax receipt) runs
continuously alongside.

## Cross-phase rules
- **Integration has levels: orientation < pointer < merge < proof.** Only merge and runtime proof count.
- Every phase splits **public core** (contracts/lifecycle/CLI/fixtures) vs **private overlay** (paths/
  credentials/accounts/corpus). Generated state → `COMPOUND_HOME`, never the repo.
- **Agent-agnosticism is proven by checkpoint replacement** (Claude starts → checkpoint → Codex resumes →
  verification succeeds from spine context), not by "both integrations exist."
- Every move ends in one canonical `move_type` (`../architecture/contracts/`); `audit_only` does not count.
- **Execution generates the architecture.** When a slice teaches you the design was wrong, update the
  docs/contracts/tests/instructions in the same change (`../../AGENTS.md` → "Execution is a feedback loop").
- **Compounding-knowledge loop:** a transition-oriented observation that teaches something about compound's
  future architecture is written evidence-linked and packet-local, and *earns promotion* to a contract,
  script, check, decision, or recalled context only when consumed by a real executor — never just stored.
  **Then it is archived:** once promoted, the workbench entry is marked `promoted → <destination>` and moved
  out of the live log, so observation logs stay thin and no entry is mined twice. An observation that opens
  no branch and names no consumer is `audit_only` (`../architecture/transition.md` → anti-bloat pipeline).
