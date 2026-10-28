# ADR-0007: Proveable-Pair Law

**Status:** Accepted; named law recorded 2026-08-02
**Date:** 2026-08-02
**Supersedes:** none
**Retires:** nothing yet
**Related:** `internal/arch/mint.go` (two-class law), `internal/arch/arch.go` (architecture doctor), `internal/work/work.go` (close path Receipt + Transition pair)

## Context

An audit on 2026-08-02 mapped elements of the OpenAI "Ten advances in
mathematics and theoretical computer science" announcement (2026-08-01) onto
organs already present in compound:

| OpenAI element                                        | Compound counterpart                                                                  |
|-------------------------------------------------------|---------------------------------------------------------------------------------------|
| Mathematical argument                                 | `envelope.ActionIntent` (model intent before execution)                              |
| Lean certificate                                      | `envelope.Receipt` (bounded proof of what was learned)                               |
| Reasoning walkthrough                                 | `internal/runattempt` + `internal/eventstore` frames, replayable                      |
| "$2k across 10 problems" cost framing                 | `BudgetEnvelope` (per-channel spend/rate caps) + `landlease` TTL + `latenttax`        |
| Model → human → model → Lean pipeline                 | `WorkEnvelope` close emits `Receipt` + `Transition` together (both validated)        |
| Replayable LLM extraction artifact                    | `distiller request --record` under `COMPOUND_HOME/reviews/distiller-requests/`       |
| Honest attribution ("AI-generated proofs")            | `Receipt.UsedDeclared` + `routeevidence` ("requested route is intent, observed route is evidence, neither proves task success") |
| Public/private boundary                               | `internal/release` + `arch.go` leak gate + `internal/finding` (private direct-write)  |
| Search-with-feedback (branching proof search)         | `work` lifecycle (`verifying` state) + `proof-check` battery as the gating feedback   |

The audit's load-bearing finding: every claim the OpenAI announcement rests on
*is paired with its proof*. Drop either side and the announcement collapses to
free-text assertion.

That same pairing is already load-bearing in compound, but unnamed. A future
agent reading `arch.go`, `work.go`, `mint.go`, and `internal/eventstore` has
to re-infer that the discipline is intentional rather than accidental. Naming
makes the inference free.

## Decision

Name the discipline: **the proveable-pair law.**

> Every claim produced by an agent must be paired with a content-bound,
> mechanically-verifiable, replayable record of what actually changed; the
> difference between the claim and the record is a typed surface, not a
> hidden one.

The law already holds in compound today by construction:

- `WorkEnvelope.Close` (`internal/work/work.go`) refuses to terminalize
  without persisting both `Receipt` and `Transition`, each validated by
  `envelope.Validate`.
- `envelope.Receipt` carries `MoveType`, `UsedDeclared`, `ProofRefs`,
  `Skills.{Used,Missed,FalseTriggered,Candidate}` — the claim surface.
- `envelope.Transition` carries `BeforeRefs`, `ActionRefs`, `AfterRefs`,
  `SecondOrder.{FutureBehaviorChanged,ShouldHarden}`, `OpensBranches` —
  the proof surface.
- The architecture doctor (`internal/arch/arch.go`) mechanically certifies
  that `envelope.Transition`'s Go fields match
  `docs/architecture/transition.md`, closing the loop between code and
  documentation.
- `distiller request --record` writes replayable request JSON under
  `COMPOUND_HOME/reviews/distiller-requests/`, so the LLM extraction path
  is also paired (request → response → adjudication).
- `internal/eventstore.ScanFrom` issues a content-bound resumable cursor,
  so the reasoning walkthroughs (event frames) are replayable in full.

This ADR records the law so a future reader does not have to re-infer it,
and registers the status-note marker in the mint ratchet so the doctor does
not reject the marker as an unclassified mint.

## Examples (sibling laws)

- **Two-class law** (`internal/arch/mint.go:11-29`, RATIFIED 2026-07-10):
  every minted identifier must be enforcement or evidence, never neither.
  Same posture, different target — that law classifies *what kind of thing
  an identifier is*; this one classifies *what must accompany every claim*.
- **Placement guard** (`internal/policy/policy.go`):
  mutating actions require an allowing PolicyDecision. Same pairing
  discipline in a narrower domain: the `ActionIntent` (claim) is gated by
  the `PolicyDecision` (proof of permission), the gate holds even when a
  caller skips the evaluator.

## Consequences

**Positive:**

- A future agent reading `docs/architecture/status.md` sees "Proveable-pair
  law note" alongside "Capability status note", "Spine orientation note",
  "Install/setup status note", and the other named disciplines, and
  immediately recognizes the constraint without re-deriving it.
- The naming cost is bounded: a 5-line status note, a single `mintHomes`
  entry, and this ADR. No new internal/ package is minted. The
  `ConvergePendingBudget = 0` ratchet is unaffected.
- The law's enforcement remains by construction (close path), so no new
  doctor check is added in this round.

**Negative:**

- The status-note marker must earn a `mintHomes` registration or fail the
  mint ratchet (`mint.go:11-37`). The cost of this registration is one
  entry (`status-note:Proveable-pair law note`), with `MintEvidence` class
  because the law is *asserted* by construction, not *certified* by a
  doctor check.
- Without a corresponding doctor check, the law is not mechanically
  certified — only asserted. A refactor that breaks the pair (e.g., close
  emits only `Transition` and skips `Receipt`) would not be caught by
  `compound architecture doctor --strict`. The mitigation: any future
  refactor of the close path must re-validate that the close still emits
  both, and the existing `Receipt.Validate()` + `Transition.Validate()`
  calls at `envelope.Validate` already refuse the close on shape mismatch.
- Naming the law creates the false impression that it is *enforced* by
  compound's own organs. It is enforced by *construction* in the close
  path, not by a doctor check. The risk lives in editor behavior, not
  runtime behavior.

**Risks:**

- A `checkProveablePairLaw` doctor check is not added in this round. If the
  close path is later refactored in a way that preserves shape validation
  but loses one side of the pair, the doctor will not catch it. A follow-up
  ADR can add the audit (`internal/eventstore` scan for orphan receipts or
  transitions) if the construction-only enforcement ever proves insufficient.

## Sequence

- 2026-08-02: ADR-0007 filed; status-note marker added to
  `docs/architecture/status.md` (near "Spine orientation note");
  `mintHomes["status-note:Proveable-pair law note"]` registered as
  `MintEvidence` with Purpose and Consumer.
- Future: optional `checkProveablePairLaw` audit (orphan receipt / orphan
  transition scan) is a separate ADR if construction-only enforcement ever
  proves insufficient.

## Provenance

- Audit: conversation thread on 2026-08-02 (OpenAI "Ten advances" page →
  compound organ audit → law naming).
- Files referenced:
  - `internal/arch/mint.go:11-29` — two-class law (sibling discipline).
  - `internal/arch/arch.go:320-386` — architecture doctor that mechanical
    invariant verification rests on.
  - `internal/work/work.go` close path — `CloseResult{Work, Receipt, Transition}`
    is the load-bearing triple.
  - `internal/eventstore/store.go` — replayable, content-bound event ledger.
  - `internal/distiller/distiller.go` — `distiller request --record`
    replayable extraction artifact.
  - `docs/architecture/transition.md` — code/doc contract that the
    architecture doctor mechanically certifies.