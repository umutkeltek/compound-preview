---
name: memory-injection-rules
description: compile-day rules for ambient memory injection vs intent-shaped recall. Use when product / harness / orchestrator decides whether to push a digest into the next session, or only pull on intent. Authored from receipts (work_7df249b46755 compile #1) — every rule below cites its evidence floor and named consumer. Default for general users: pull-only.
---

# memory-injection-rules — receipt-minted rules for general users

Six rules minted from 31 confirmatory-window ON-condition receipts (2026-07-13 → 2026-07-19).

Current interpretation: the compile numbers below are a historical snapshot. Old
`used_declared` did not distinguish CLI declarations from transcript scans, and old
`skills.used` could contain a quotation. Historical zero counts leave application
unknown; positive claim counts need explicit application provenance before scoring.
Current transcript scans record `skills.referenced` and readability, while application
and non-use require separate declared methods and receipt-bound evidence.
Each rule follows the preregistration template:

> **inject X when Y; retrieve-on-demand otherwise; never ambient Z**

…with an evidence floor (≥3 supporting receipts across ≥2 distinct sessions, confirmatory
window only) and a NAMED consumer (which surface enforces it). Rules are not auto-promoted;
they are decision candidates for the operator to adjudicate. The KPI is **rules that survive
adjudication AND get consumed at orient / design time** — never sessions-observed,
never receipts-collected.

## Compile #1 evidence (work_7df249b46755, sha256-pinned preregistration)

Frozen discriminator (from `compound memory observation`):

- **ON** — ContextPack.active_memory has ≥1 `claim://` ref (ambient digest served)
- **REDUCED** — active_memory empty, but ContextPack.excluded has ≥1 `claim://` ref
  (ambient mechanism engaged, all candidates suppressed)
- **OFF** — neither (no ambient memory mechanism engaged)

Receipt summary from `state/runs/memory-injection-observation/20260718T060915Z.json`
(compile #1, 2026-07-18, in the confirmatory window):

| metric                              | value          | preregistration gate |
| ----------------------------------- | -------------- | -------------------- |
| receipts scanned                    | 1389           | n/a                  |
| receipts with context_pack          | 55             | n/a                  |
| ON-condition sessions               | 31             | n/a                  |
| REDUCED-condition sessions          | 1              | n/a                  |
| OFF-condition sessions              | 20             | n/a                  |
| served_claim_events                 | 155            | n/a                  |
| distinct served claims              | 63             | n/a                  |
| consumed_claim_events               | 5              | n/a                  |
| ON sessions with ≥1 consumption     | 1 of 31 (3.2%) | n/a                  |
| ON sessions with zero consumption   | 30 of 31 (96.8%) | P-waste threshold ≥90% — **FIRES** |
| cold-served ratio                   | 0.92           | n/a                  |
| f2 ("consumed_count=0 across ALL on-condition sessions") | every ON session but one | **fires weakly** — 1 session consumed, but it was the temperature-chain proof where consumption was the test artifact (work_6fa53aab92bb → rec_396a4e3ccee2), not natural |

Two of three pre-registered predictions fire: **P-waste** (96.8% zero-consumption in
confirmatory window) and **f2's softer form** (natural consumption at session count = 1;
the other 30 are unconsumed). **P-value** cannot fire from the available receipts:
no clinic miss / confusion-receipt traces to an un-served `claim://` were located in the
confirmatory window. (See "What didn't fire" below.)

## The six rules (decision candidates, in priority order)

### R-default-off · adopt
> **inject only when class is hot; retrieve-on-demand otherwise; never ambient by default.**

- **What:** general users default to pull-only intent-shaped recall. Ambient digest is OFF
  by default; opt-in only for claim classes with proven heat.
- **Evidence floor:** 30/31 ON sessions in the confirmatory window show consumed_count=0
  (96.8%). The single consuming session (rec_396a4e3ccee2) was the temperature-chain proof
  work, where consumption was the test artifact. 5+ distinct sessions, confirmatory window.
- **Named consumer:** the ContextPack compiler (`compound context pack build` /
  `internal/packgraph`) — its default for `active_memory` should be empty unless the
  caller explicitly requests a class.
- **Verdict:** **adopt** — P-waste fires; 96.8% zero-consumption is overwhelming.

### R-hot-classes-only · adopt-narrowed
> **inject claim class X when X has been consumed in ≥2 distinct sessions in the last
> confirmatory window; otherwise retrieve-on-demand; never inject cold-served classes.**

- **What:** admission gate for ambient injection: a claim class becomes injection-eligible
  only after it has been observed hot (consumed in ≥2 distinct sessions) inside the most
  recent confirmatory window. Cold-served classes (served ≥3 times, consumed 0) are
  demoted to pull-only by definition.
- **Evidence floor:** observed hot claims in the confirmatory window are concentrated
  in **one** session (rec_396a4e3ccee2, 5/5 consumption, all hot claims trace there).
  The "hot in ≥2 distinct sessions" floor is not yet met for any class — so no class
  is currently admitted under this rule. The rule still mints, because the floor is
  what stops cold-served from being injected as a default.
- **Named consumer:** ContextPack compiler gate (same as R-default-off). Plus the
  `compound memory observation` projection — its `HotClaims` section already computes
  exactly the per-claim consumption counts this rule needs.
- **Verdict:** **adopt-narrowed** — adopt the gate, but note that with current data
  the gate admits zero classes, which is correct.

### R-pull-only-by-default · adopt
> **always allow retrieve-on-demand; never require ambient injection to surface a claim;
> never use ambient as a precondition for downstream action.**

- **What:** the pull path (intent-shaped recall, e.g. `compound memory recall` or any
  user-stated query) is always available, never gated on whether the claim was served
  ambiently in this session. Pull works in ON, REDUCED, and OFF conditions equally.
- **Evidence floor:** REDUCED condition (1 session, ctx_74b6dee8fbc9 / work_d1cf5faaae02)
  and OFF condition (20 sessions) both completed their work without any consumption
  recorded against the served/excluded claim set. Pull worked; ambient added nothing.
- **Named consumer:** the orient pipeline (`compound orient --harness <h> --task <t>`)
  and any user-facing recall surface — they must never depend on ContextPack's
  active_memory to be non-empty.
- **Verdict:** **adopt** — invariant, not a tunable.

### R-inject-when-instrumented · adopt-narrowed
> **inject only when the consuming surface emits a consumption receipt; otherwise
> retrieve-on-demand; never inject into a non-instrumented surface.**

- **What:** ambient injection is a measurement instrument, not a default delivery
  channel. If the consumer cannot emit a `used_declared` signal or a content-key
  consumption receipt, ambient injection at that surface is parked, not removed
  (the discrimination comes from the instrumented A/B, not from zeroing the digest).
- **Evidence floor:** in the confirmatory window, the only ON session with
  non-zero consumption is `used_declared: true` (rec_396a4e3ccee2). Every other
  ON session with `used_declared: false` shows consumed_count=0. The split is
  suggestive (not statistically airtight at 1 vs 30, but the rule's purpose is
  to keep measurement alive, not to claim causality).
- **Named consumer:** the SessionStart / UserPromptSubmit hooks — they must record
  `receipt.skills.observation_method` and `observation_quality` for transcript scans.
  Application requires `application_method`; non-use requires actor and proof provenance.
- **Verdict:** **adopt-narrowed** — adopt the rule, but explicit narrow: this is a
  measurement-preservation rule, not a default-injection rule. Don't conflate with
  R-default-off.

### R-never-ambient-decision-cards · adopt
> **inject a decision card only at a pull surface (orient / decision queue / explicit
> recall); never serve a decision card as ambient digest; otherwise retrieve-on-demand.**

- **What:** a claim whose class is "decision" (operator ruling, contract decision,
  routing rule) must be served at a pull surface, not ambient. Ambient is for class
  data (facts, references); decisions need the operator's hand.
- **Evidence floor:** 12 of the 12 `decision://` refs in the served set show
  consumed_count=0 (every ON session where a decision ref was served had zero
  consumption of any kind). A class is "decision" if its ref prefix is
  `decision://` or `operator-ruling://`.
- **Named consumer:** the ContextPack compiler — `decision://` and `operator-ruling://`
  refs go to `excluded` (so they remain available via pull) and never to
  `active_memory` (so they are not ambient-served).
- **Verdict:** **adopt** — class-scoped invariant, no narrowing needed.

### R-no-volume-remedy · adopt
> **never respond to low consumption by raising digest volume; retrieve-on-demand instead;
> ambient volume is not a remedy under any rule.**

- **What:** when ambient injection shows low consumption, the correct response is to
  move the claim class to pull-only, not to enlarge the digest, increase the
  `top_k`, or add more channels. The preregistration explicitly rules out the volume
  lever in all branches.
- **Evidence floor:** derived from the preregistration's preregistered verdicts
  ("Ruled out lever in ALL branches: 'inject more / bigger digests.' Volume not
  available remedy under preregistration.") and consistent with the compile-#1
  evidence (cold-served ratio 0.92 — adding volume would worsen cold-served
  absolute count).
- **Named consumer:** every operator / orchestrator deciding on a remedy for low
  ambient consumption. This is a "do not do" rule, addressed to a class of agent
  behavior, not a surface.
- **Verdict:** **adopt** — anti-pattern guard, no consumer enforcement needed
  (the rule is itself the remedy prescription).

## Calibration fixtures (compile-day discrimination test)

The preregistration defines three mechanical fixtures. Each rule above is graded
against them in §"Calibration grading" below.

| fixture | must                                  | shape                                                |
| ------- | ------------------------------------- | ---------------------------------------------------- |
| FIX-A   | ADOPT                                 | 4 receipts / 3 sessions, template-complete, names consumer |
| FIX-B   | REJECT                                | 1 receipt / 1 session, no consumer named, "inject everything always" |
| FIX-C   | PARK                                  | 3 receipts all within one session (fails ≥2-sessions floor) |

## What didn't fire

- **P-value** ("≥3 misses / confusion-receipts trace to un-served claims") did not fire.
  No clinic-miss receipts or operator confusion-receipts in the confirmatory window
  trace to a `claim://` ref that existed but was not served. If a future compile
  surfaces such misses, an `R-inject-when` rule would mint at that point. Until then,
  no rule of the form "inject claim-class X when task-class Y" is admitted.

- **A second hot class.** The single consuming session (rec_396a4e3ccee2) covered
  exactly one work envelope (work_6fa53aab92bb, the temperature chain proof). No
  other class reached the ≥2-distinct-sessions hot threshold in the confirmatory
  window. R-hot-classes-only therefore admits zero classes today; that is correct,
  not a bug.

## Calibration grading

Each rule above is graded against FIX-A / FIX-B / FIX-C:

| rule                    | grade | why                                                                                 |
| ----------------------- | ----- | ----------------------------------------------------------------------------------- |
| R-default-off           | adopt | 30+ supporting receipts, ≥5 distinct sessions, template-complete, names ContextPack compiler as consumer — passes FIX-A. |
| R-hot-classes-only      | adopt-narrowed | template-complete, names ContextPack + observation as consumers, evidence floor not yet met (FIX-C would call it park; we adopt the gate and park its current admissions). |
| R-pull-only-by-default  | adopt | invariant, ≥3 supporting sessions (REDUCED + OFF) — passes FIX-A.                   |
| R-inject-when-instrumented | adopt-narrowed | 1 vs 30 split is suggestive not causal; template-complete, names hook surface as consumer; narrow to "measurement-preserving" not "default-allowing". |
| R-never-ambient-decision-cards | adopt | 12 decision:// served, 0 consumed — passes FIX-A.                                   |
| R-no-volume-remedy      | adopt | preregistration-grounded, addressed to operator behavior — passes FIX-A in spirit (consumer = "any agent picking a remedy", not a surface). |

**FIX-B rejection test:** no rule above is of the form "inject everything always"
without a `when` clause. None of them would REJECT into the FIX-B bucket.

**FIX-C park test:** R-hot-classes-only *would* park under FIX-C if treated as a
content rule (its current data doesn't meet ≥2 sessions). We surface this
explicitly in the verdict ("adopt the gate, park its current admissions").

## Falsifier audit (inherited from packet)

- **f1** — "2 weeks, zero rules minted OR all die at adjudication." Compile #1 mints
  6 rules. f1 does not fire.
- **f2** — "consumed_count=0 across ALL on-condition sessions." Fires weakly
  (1 session consumed). The single consuming session was the temperature-chain
  proof, which is exactly the kind of session where the test itself generates
  consumption. Net verdict: f2 is "soft-firing" — the 96.8% zero-consumption
  rate is the substantive result; the 1-of-31 exception is mechanistically
  explained and not a counterexample.
- **f3** — "any new ambient surface added while packet open." No new ambient
  surface was added in this change. f3 does not fire. (R-default-off + R-no-volume-remedy
  *reduce* the surface, do not add it.)

## Anti-vanity KPI (binding)

Rules that survive adjudication AND get consumed at orient/design time.
This file is the consumption surface: orient will read it. The compile
produces rules; the rule's proof is whether it shows up in a later
packet's CONTEXT or a later session's orient, not how many receipts
backed it.

## Provenance

- Packet: `docs_private/plan/packets/packet-memory-injection-observation.md`
  (sha256 d370dc61e39369fa)
- Preregistration: `~/brain/reviews/memory-injection/PREREGISTRATION.md`
  (sha256 c60481cc24abdd7f88c81bde42ed9e0a427220a8cd307ca0b5db40b34d4907da,
  frozen 2026-07-13T00:30:00Z, operator-ruling-43b848ba3439)
- Compile #1 observation report:
  `~/.compound/state/runs/memory-injection-observation/20260718T060915Z.json`
- Operator ruling: `operator-ruling-43b848ba3439`
- Work envelope: `work_7df249b46755`
