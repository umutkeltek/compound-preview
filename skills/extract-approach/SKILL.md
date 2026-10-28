---
name: extract-approach
description: After every non-trivial solved problem, extract the approach and route each learning to the owner that will consume it (test, doctor guard, contract doc, AGENTS.md rule, memory candidate, skill) in the same change. The learning law — a solution without its learnings note is unfinished work. Use after fixing a bug, closing a slice, surviving a gnarly debug, or when told "extract learnings".
---

# extract-approach — the learning law, made executable

This repo's doctrine: **execution is a feedback loop** — what a slice teaches you goes back into
docs, contracts, tests, and instruction files *in the same change*, and a learning that only
produces "another document" has failed unless a real executor will consume it. This skill turns
that law into a five-minute routine. It replaces ad-hoc end-of-session curation and unowned
notes-to-nobody.

## 1 · Extract (answer in one or two lines each)

- **Problem class** — not "the bug", the *class* ("stale projection consumed as authority",
  "id collision across inboxes", "check passes because it never runs").
- **Approach that worked** — the move, generalized enough to reuse.
- **Failure modes hit on the way** — what you tried that was wrong, and the signal that told you.
- **Detection** — what observable output would have caught this earlier?
- **Evidence** — commit SHA, test name, work_id/receipt, file:line. A learning without evidence
  is an opinion.

## 2 · Route each learning to its consumer (decision tree — pick the FIRST match)

1. **A check could catch it next time** → write the test or doctor guard now. A regression test
   proven to fail on the unfixed code is the gold standard here (this repo's panel bugs each
   landed with one). For cross-cutting invariants, extend `internal/arch` checks. This is the
   best home — prefer it whenever possible.
2. **It's an agent-behavior rule** ("a weaker model will do X wrong") → add one line to the
   named-mistakes table in `AGENTS.md` (public-safe phrasing) — same commit as the fix.
3. **It's a contract/architecture fact** → the owning doc: `docs/architecture/status.md`
   (respect the 250-line budget — a note, not a journal), `docs/architecture/contracts/README.md`,
   or the relevant contract section. Same commit.
4. **It's operator-sequencing / private-lane knowledge** → `docs_private/plan/HANDOFF.md`
   (private overlay; never a tracked file).
5. **It's a durable fact/decision about the system** → a compound memory/decision candidate
   through the governed door (`go run ./cmd/compound memory candidate …` or the distiller
   path). Candidates park; they never self-promote — the operator adjudicates.
6. **It's a reusable multi-step procedure** → a skill in the tracked `skills/` tree (the source of
   truth; a harness may auto-load it — e.g. Claude Code via `.claude/skills/` — but that overlay is a
   convenience, not the home), or a CapabilityGraph candidate via the intake path (`capability
   skill-plan`) if it should surface to other harnesses. Never hand-edit the graph.
7. **It's an operational finding / runbook note** — a durable operational fact (how a system is
   run, a verb, a gotcha) that is neither a compound-architecture fact nor a reusable procedure,
   and isn't worth adjudication latency → the direct-write finding home: `compound finding record
   --topic T --body B`. **Do NOT invent a path** under `COMPOUND_HOME` (the placement guard denies
   it); if unsure where a write goes, ask the resolver: `compound placement target --kind
   operational-finding` (or `--kind <candidate-kind>`). Findings are `source_class=evidence` and
   recallable via `compound finding list/show`; promotion to canon stays door 5.

One destination per learning — the first match, not all matches. Duplicating a learning into
three homes creates the parallel-authority disease this repo exists to kill.

## 3 · The two gates before writing anything

- **Consumer gate:** name who consumes it (a test runner, a future agent reading AGENTS.md, the
  adjudication queue, the next session's orient). No named consumer → don't write it; it would
  be `audit_only` bloat. Say so and move on — silence is a valid outcome.
- **Same-change gate:** if the learning modifies tracked docs/rules/tests, it belongs in the
  same commit as the fix that taught it. If the commit already landed, a follow-up commit
  within the session is acceptable; "later" is not.

## 4 · Note shape (when a note is the right destination)

Mirror the Transition object — three questions, five lines max:

```md
- what changed: <the approach/rule, one line>
- evidence: <sha / test / work_id / file:line>
- future behavior: <what an agent should do differently tomorrow>
- consumer: <who reads this — where it was routed>
- prevents: <the wrong move this blocks (subtraction framing)>
```

## 5 · Done means

The fix is landed AND every extracted learning is either (a) routed with a named consumer in the
same change, or (b) explicitly judged not worth keeping. State which in your close/receipt notes
in one line, e.g. `learnings: 1 test (TestX), 1 AGENTS.md rule; 2 discarded (no consumer)`.
