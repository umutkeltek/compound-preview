# runattempt — schema changelog

This is the per-family record required by
[ADR 0004](../../docs/adr/0004-schema-versioning-and-read-compat.md).

- **Writer version:** `compound.runattempt.v0.2`
- **Read-compat window:** `compound.runattempt.v0.2` (current) and
  `compound.runattempt.v0.1` (legacy)
- **Nested terminalization-intent writer:**
  `compound.runattempt_terminalization_intent.v0.1`
- Launch-boundary authority remains in its independently versioned immutable
  sidecars; it is not inferred from the parent RunAttempt schema.

## compound.runattempt.v0.2 (current)

- **[MINOR]** Added load-bearing optional `terminalization_intent`,
  `terminalization_owner`, `terminalization_receipt_id`,
  `terminalization_transition_id`, and a distinct
  `terminalization_reason`. A work-close-owned terminalization prepares its
  immutable intent before publishing terminal work, fences generic terminal
  writers, then completes against that exact intent.
- **[MINOR]** Added optional append-once `observed` (observed model, effort,
  session, hook posture, and source) beside `execution_selection`. It records
  what the launch read back before start; it never rewrites the requested
  snapshot, and it is absent (not zeroed) when nothing was observable.
- A terminal harness- or caller-owned completion may record owner/proof fields
  without a prepared work-close intent. The absence of an intent is therefore
  not invalid by itself; ownership determines whether preparation was owed.
## compound.runattempt.v0.1 (readable legacy)

- Records predate terminalization reservation semantics. Readers preserve the
  original schema on ordinary mutations. Installing a v0.2 exact intent—or
  recording v0.2 owner proof—is the explicit narrow migration to the current
  writer.
- v0.1 records cannot carry v0.2 terminalization fields. Readers reject that
  mixed generation instead of assigning current meaning to legacy bytes.

## compound.runattempt_terminalization_intent.v0.1 (current)

- Initial closed vocabulary for the exact pre-close work generation, attempt
  authority, canonical close-input digest, close action/policy,
  receipt/transition, outcome, target status, owner, and optional owner reason.
- Readers fail closed on an unknown intent version, owner, status, malformed
  digest, or partial proof pair. There is no legacy intent version; pre-field
  RunAttempts express compatibility by omitting the nested object.
