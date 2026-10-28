---
name: corpus-audit
description: "Audit a large external repository corpus (many dated project directories) so the result is decision-grade: per-project independent reading, mechanically attested coverage, and each project routed to an existing owner instead of a new one. Use when asked to comprehensively audit, review, or map an external repo's projects against what this system already owns."
---

# corpus-audit — auditing many external projects into owned decisions

The failure this ceremony prevents: a synthesis that *reads* like it covered everything but
rests on a sample, a README, or the analyst's memory — and that proposes new subsystems for
mechanisms the system already owns.

A corpus audit answers two different questions. **Coverage** asks "did you read every project?"
and is answered mechanically. **Verdict** asks "what does each project change about us?" and is
answered per project. Keep them separate: a defensible verdict on 12 of 84 projects is worth
less than a mechanical attestation that you read all 84.

## 1 · Scope before reading

Pin the upstream revision and enumerate the corpus from the tree itself, not from a description
of it. Record the revision, the project count, and the enumeration rule. A corpus index that
lists 83 episodes while the tree holds 84 dated directories is a finding, not a rounding error.

## 2 · Read each project independently

Give each project (or a small batch) to a reader that opens the project's own files and cites
exact paths. Independence matters more than speed: a reader that has seen another project's
verdict starts reconciling instead of reading, and the corpus silently becomes one analyst's
opinion applied 84 times.

A reader must answer, per project:

- **Mechanism** — what the code or narrative actually does, in its own terms.
- **Demonstrated** — what it shows working, with the path that shows it.
- **Not established** — what it does *not* prove. A narrative session proves a mechanism was
  described; it does not prove the mechanism works, scales, or is reproducible.
- **Owner overlap** — which existing owner already covers this mechanism, and how.

Record the files opened and the counts that classify the evidence. Derive the evidence depth
from what was read, never from the project's self-description:

| Depth | Read | Means |
|---|---|---|
| A | code + tests + recorded results | a runnable claim |
| B | code only | a mechanism, unverified |
| C | narrative only (transcript/email/whiteboard) | a described mechanism |
| D | metadata/announcement only | an intent, not a mechanism |

Depth is the single most useful column in the final table: it tells a reader how much weight a
row can bear, and it is the difference between "this works" and "this was discussed".

## 3 · Attest coverage mechanically

After the readers finish, verify coverage from the artifacts — not from memory. Produce, and
keep, a machine-checkable attestation:

```
assigned == covered
gaps == []          # every enumerated project has a verdict
duplicates == {}    # no project read twice
empty_reports == [] # no reader returned nothing
```

This check is cheap and it catches the most common silent failure: a reader that crashed, timed
out, or quietly returned a truncated report. Re-derive the depth column from the recorded file
counts and confirm it agrees with the assigned depths; a disagreement means a reader's summary
and its evidence drifted apart.

## 4 · Route each project to an owner, not to a proposal

Classify every project into exactly one bucket, and make the buckets mean something operationally:

| Verdict | Meaning |
|---|---|
| already present | an existing owner already holds this mechanism |
| extend an existing owner | the mechanism fits a named owner; it needs coverage, not a new home |
| worth a vertical slice | a bounded change to a named owner, with a consumer and a test |
| should not build | a mechanism that would duplicate, fossilize, or bypass an owner |

Two disciplines make the classification honest:

- **Name the owner.** "Extend an existing owner" is only a verdict if it names the owner and
  the seam. An unnamed owner is a proposal in disguise.
- **Refuse mechanisms, not projects.** A project can carry a mechanism worth refusing *and* a
  mechanism worth absorbing. Split by mechanism; do not discard the whole project or promote
  the whole project.

## 5 · Reconcile the arithmetic

The counts must close before the synthesis is written. Worth-a-slice projects partition into
those covered by your slices plus those deliberately parked — with no project in both groups
and none unassigned. State the partition explicitly; a reader who cannot reconstruct it cannot
audit your synthesis.

## 6 · Write two artifacts, in two places

- **Public decision record** (tracked): the verdict, the per-project ledger with depth, the
  slices, the refusals, the measurement criteria, and the risks. This is what a future agent
  reads to decide what to do next.
- **Private evidence detail** (private overlay): for every ledger row, the files opened, the
  mechanism, what it does not prove, the owner overlap, the limitations, and the slice sketch
  behind the verdict. The public table summarizes; the private file keeps the reasoning that
  would let a later lane contest or re-derive a row without re-auditing the corpus.

The split is the point: the public record must stay free of copied upstream text and of
private paths, while the reasoning stays complete. Never put upstream prose in the public file.

## 7 · Verify before publishing

- Every path you cite exists in the pinned revision.
- A verbatim-match scan against the upstream tree finds no copied passage. Referencing a
  mechanism is fine; reproducing text is a licensing question you did not answer.
- The upstream license is stated. If it is not established, say so and reference concepts only.
- Every "already present" claim names a real owner file that exists.
- The slices name the seam they touch and a check that would catch their absence.

## 8 · Findings that change the system

An audit that finds a real defect in *this* system has two obligations, and the second is
usually forgotten.

- Fix it, with a regression test proven to fail before the fix and pass after.
- Verify the claimed root cause against live records before you build the fix. A status note
  says what someone concluded; the run attempts, receipts, and logs say what happened. When they
  disagree, the note is stale and the fix built from it is the wrong fix.

Record the corrected cause where the next agent will read it: a rule in `AGENTS.md` if it is an
agent-behavior rule, a test if a check can catch it, the owning doc if it is a contract fact.

## Reusable shape

```text
enumerate + pin revision
  → one independent reader per project (mechanism / demonstrated / not-established / owner overlap)
  → mechanical coverage attestation (assigned == covered, no gaps, no duplicates, no empty reports)
  → depth derived from files actually read
  → per-project verdict into an existing owner
  → arithmetic closes (slices + parked == worth-slice, no overlap, none unassigned)
  → public decision record + private evidence detail
  → cite-verified, no copied prose, license stated
  → any system defect found: live-record root cause, fix + red-to-green test, rule routed
```

## Consumer

Read by an agent asked to audit an external corpus against what this system already owns, and by
the reviewer who must decide whether the coverage claim and the per-project verdicts hold up.
Its authority is evidence only: it licenses no action, changes no routing, and proves no
adoption.
