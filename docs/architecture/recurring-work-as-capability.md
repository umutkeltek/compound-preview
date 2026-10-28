# Recurring work is a capability node, not a subsystem

A recurring analytical/audit/optimization prompt — "audit my harness", "map my attack surface",
"turn my repeated decisions into policy", "which of my bets is wrong" — is a strong temptation to
fragment: a `prompts/` folder, a per-prompt skill file, a bolted-on second registry. Every one of
those is a **new home**, and a new home is the fragmentation this substrate exists to prevent.

Compound already has exactly the right shape. A recurring prompt is **not a new object kind**. It is:

1. **The reusable "how" → one node in the single CapabilityGraph** (`internal/capgraph`), of kind
   `procedure | workflow | lens | eval | doctrine` (`capgraph.go` `NodeKinds`). Authored once via a
   `compound.skill_manifest.v0.1` manifest (`compound capability import-skills`) or proposed and
   reviewed via a `SkillPlan`. It **auto-surfaces, never auto-executes** (`orient --harness --task`).
2. **Each run → the envelope spine.** A single unit of the recurring work enters as a
   **packet → `WorkEnvelope`** (`internal/work/packet.go`, `work import-packet`) and threads one
   `work_id` through `ContextPack → ActionIntent → PolicyDecision → Receipt → Transition`.
3. **Its output → one of the six object families** (`docs/LAYERS.md`), landing as a real
   `move_type` (`internal/envelope/envelope.go` `MoveTypes`):
   `outward_delta | memory_delta | rule_delta | control_plane_repair`. A run that only emits a
   markdown report is `audit_only` — **it does not count.**

## The seven rules (so future prompts absorb the same way)

1. **Generic-door rule.** Intake doctrine (`knowledge-intake.md`): *"a new medium = one capture edge
   + zero core changes."* Generalise: **a new recurring prompt = one capability node + zero new
   owner.** If absorbing it needs a new folder / package / skill silo / `prompts/` dir / external
   OS bundle, you have segmented — stop.
2. **Executors are rented edges.** The node *references* an executor (a skill, workflow, agent,
   model). It never *becomes* an owner or a source of truth.
3. **Output lands on the spine as a real `move_type`** — memory / decision / rule / product delta,
   never a free-floating document (`AGENTS.md`, execution-feedback-loop).
4. **Adjudication is the one intake door.** `observed → reject | park | promote`; unknown → parked;
   only `promoted` is ambient. This is simultaneously the memory model and the prompt-injection
   defence — one door, not two systems.
5. **Placement.** Generated artifacts → `COMPOUND_HOME`; private data (self-model, credentials, real
   infrastructure) → the private overlay; the tracked repo stays generic and public-safe.
6. **Subtraction.** This pattern retires the ad-hoc "paste a mega-prompt → get an orphan markdown
   report" loop, and any temptation to add a second capability registry. The CapabilityGraph is the
   one public surface.
7. **Prompts enter as evidence first.** External prompt libraries are content — they arrive via
   `knowledge import` → candidates → adjudication, and are *operationalised* into a node only when
   they earn promotion. Never pasted straight into authority.

## Node-kind → object-family shape

| Prompt shape | Node kind | Run deltas into (family / owner) |
|---|---|---|
| audit / characterize / find-what's-wrong | `lens` | Memory (contradiction sweep) or Decisions |
| repeatable multi-step procedure | `procedure` / `workflow` | Decisions / Product / Authority |
| "what does "better" mean" / regression | `eval` | extends `internal/distiller` EvalCase + `proof-check` |
| a standing rule about how the system runs | `doctrine` | Permissions (`internal/policy`) / rule_delta |
| repeated decision → standing policy | `workflow` | Decisions (`internal/decisions`) → Permissions (`internal/policy`), `move_type: rule_delta` |

The graph's feedback edges (`validated_by` / `missed_when` / `false_triggered_by`) are what make
this **compound** rather than accumulate: repeated success becomes infrastructure, repeated failure
becomes a rule — only after evidence.

See also: `docs/LAYERS.md` (the six object families), `docs/architecture/contracts/README.md`
(CapabilityGraph + `move_type`), `docs/architecture/knowledge-intake.md` (the generic intake door).
