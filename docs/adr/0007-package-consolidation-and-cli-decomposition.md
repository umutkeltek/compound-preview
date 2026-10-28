# ADR-0007: Package Consolidation (61→~30) and CLI Decomposition

**Status:** Proposed (window-gated; requires the freeze protocol in the
annex before execution)
**Date:** 2026-08-02
**Annex:** `0007-annex-consolidation-freeze-protocol.md` (the freeze
ceremony; re-filed from the draft ADR-0005 annex, which collided on the
0005 number)
**Independent of:** ADR-0005 and ADR-0006 — consolidation needs NO
database and NO storage work
**Split from:** the draft ADR-0005 "Postgres+pgvector Storage Engine and
Package Consolidation" (dated 2026-07-31; never ratified)

## Context

At draft time (2026-07-31; re-verify at freeze time) the tree held 61
internal packages, of which 14 have a single source file, and
`cmd/compound` was a 5,432-line `main.go` with 75 case branches in one
`package main`. Single-file packages accrete as staging areas that
never graduate; each one is import-graph surface, onboarding cost, and
merge-conflict area for little structural gain.

The draft bundled this with Postgres adoption, which obscured the fact
that consolidation needs no storage work at all — it is a mechanical
import-graph reduction, executable in a single coordinated window.
Unbundling lets it proceed on its own schedule and stay blocked only by
lane activity, not by the storage debate.

One constraint shapes the whole approach: with multiple concurrent
agent lanes (6+ sessions, 80+ worktrees observed at draft time), any
import-path rewrite breaks every unlanded branch. Hence the freeze
protocol in the annex — consolidation is one atomic mechanical commit
inside a coordinated window, never a gradual migration.

## Decision

1. **Absorb the single-file packages** into their primary consumers,
   per the merge list below.
2. **Evaluate the two-file packages** against the stated conditions;
   merge only where the condition holds at freeze time.
3. **Decompose `cmd/compound` AFTER consolidation**, not before: the
   subcommand split (`cmd/compound/<verb>/` packages behind a thin
   dispatcher) depends on the final package layout.
4. **Single mechanical commit.** Import rewrites are script-generated
   (`scripts/consolidate-imports.sh`, produced per the merge list),
   landed as one commit inside the freeze window, with the arch doctor
   (`compound architecture doctor --strict`) and the full battery at
   the boundary.
5. **Post-consolidation target: ~30 packages** (10 spine + 12
   object-family + 8 leaf).

### Consolidation merge list

Single-file packages to absorb into their primary consumer:

| Package | Absorb into | Rationale |
|---------|------------|-----------|
| `attention` | `panel` | ranked projection over panel's review queues |
| `executorlens` | `behaviorallens` | same observer pattern, shared signal type |
| `lensmodulator` | `behaviorallens` | modulator IS a lens composition |
| `negatives` | `panel` | read-only projection over panel queues |
| `reviewqueue` | `work` (workbench) | workbench presentation helper |
| `statussurface` | `harness` | harness metadata projection |
| `stream` | `eventstore` | read-only tail of eventstore's own log |
| `verdictflow` | `panel` | panel's review scaffolding |
| `laneselect` | `routeevidence` | route ranking shared by dispatch+route |
| `finding` | `home` | direct-write home under COMPOUND_HOME |
| `intent` | `work` | thin coordinator over work+mem |
| `narrate` | `panel` | read-only script over attention (absorbed above) |
| `schema` | `envelope` | schema export IS envelope grammar |
| `knowledge` | `candidates` | intake door into candidate inbox |

Two-file packages to evaluate:

| Package | Candidate merge | Condition |
|---------|----------------|-----------|
| `candidates` | `mem` | if candidate inbox stays mem-local |
| `runattempt` | `work` | if no non-work consumer exists |
| `writeback` | `policy` | writeback IS a policy gate |
| `localmodel` | `routeevidence` | model advice is route evidence |
| `bench` | keep | evaluator needs independence |
| `release` | keep | release lifecycle is distinct |

(One annotation carried in from the split: the draft listed `narrate`
as merging into `attention`→`panel`; since `attention` is itself
absorbed here, the direct target is `panel`.)

## Lane-safety rules (non-negotiable)

1. **Consolidation requires a FREEZE.** No lane may be mid-flight.
   `compound coordination` must show zero active claims; the annex
   owns the exact gate commands, announcement, and recovery steps.
2. **Additive only until the window.** No import-path change lands
   outside the freeze. The merge list is a plan, not a partial
   migration — mixed pre/post-move import paths are the ambiguity the
   freeze exists to avoid (annex, "Why not gradual?").
3. **The arch doctor gates the boundary.** `compound architecture
   doctor --strict` and the full proof-check battery pass before the
   freeze lifts. Moved packages need their Classification / MintHome /
   status.md rows updated in the SAME mechanical commit (same-change
   doctrine).
4. **The consolidation commit records the import map** as a work
   artifact, so stale branches rebase mechanically instead of by
   inspection (annex, post-freeze steps).

## Consequences

**Positive:**
- Package count drops ~50%; import graph, onboarding surface, and
  merge-conflict area shrink with it.
- `cmd/compound` stops being a god-package; subcommand packages get
  testable boundaries.
- The merge list forces each single-file package to justify its
  independence on the record — the accretion pattern becomes visible.

**Negative:**
- The freeze window costs every lane a coordination pause (1-2 hours
  expected; annex).
- Post-move, every unlanded branch needs the mechanical import fixup
  (scripted; annex agent-instruction block).

**Risks:**
- The freeze window is never taken, and the package count keeps
  growing. Mitigation: this ADR is Proposed now so the only remaining
  gate is lane activity, not ratification; the window can be scheduled
  the moment the coordination board is quiet.
- A merge later proves wrong (a package re-extracted because its
  consumer and it diverged). Mitigation: merges are ordinary commits,
  reversible by the same freeze protocol at smaller scale; the merge
  list's rationale column is the record to argue against.
