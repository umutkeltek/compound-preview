# Operating ceremonies (tracked, fresh-clone reachable)

These are the binding **build ceremonies** for agents working *in the compound repo* — the
procedural half of [`AGENTS.md`](../AGENTS.md). Each is a plain-markdown skill body any harness can
read directly. `compound orient --harness <you> --task "<task>"` serves this index as an
`operating-ceremonies` context source, so a fresh clone reaches the ceremonies without any private
overlay.

| Ceremony | Read it when | Body |
|---|---|---|
| **compound-execute** | Taking a job/packet end-to-end: resolve → claim → dispatch → land → close → route learnings. | [`compound-execute/SKILL.md`](./compound-execute/SKILL.md) |
| **compound-land** | Getting a change onto main: scope claim, verification battery, rehearsed `--ff-only` with a fresh-SHA guard, post-land duties. | [`compound-land/SKILL.md`](./compound-land/SKILL.md) |
| **compound-verify** | Proving a change is ALIVE (wired, fed, cut over, provable) — not merely merged — before closing any work item. | [`compound-verify/SKILL.md`](./compound-verify/SKILL.md) |
| **extract-approach** | After any non-trivial solved problem: extract the approach and route each learning to the owner that will consume it, in the same change. | [`extract-approach/SKILL.md`](./extract-approach/SKILL.md) |
| **corpus-audit** | Auditing a large external repository corpus into decision-grade verdicts: per-project independent reading, mechanically attested coverage, and each project routed to an existing owner. | [`corpus-audit/SKILL.md`](./corpus-audit/SKILL.md) |
| **memory-injection-rules** | Compiling ambient memory-injection rules from receipt evidence — what to inject, when, and never ambient by default. Default for general users: pull-only. | [`memory-injection-rules/SKILL.md`](./memory-injection-rules/SKILL.md) |

## Optional product lifecycle bundle

The [public product-pipeline bundle](product-pipeline/README.md) supplies an original
generic router and fifteen branching research/design/delivery/growth/learning contracts.
It is not a build ceremony or an automatic scheduler. Its portable generator emits the
existing native SkillManifest for governed import; no private skill overlay is required.
[Source credits and license scopes](product-pipeline/SOURCES.md) identify conceptual
influences and optional upstream complements, none silently installed.

## Tracked bodies vs. harness overlay

- These bodies are **tracked in-repo** and are the source of truth for a public clone — no gitignored
  directory is required to read them.
- A harness may ALSO expose the same ceremonies as local skills (for auto-load on description match)
  under its own skills directory; that overlay is an operator convenience, not the source of truth.
  Keep an overlay in sync by pointing it at these bodies.
- Bodies stay **generic**: no operator-personal paths, account names, or private tool names. The
  armed doc-leak gate scans this tree by default — a leak here fails the doctor like a leak in
  `docs/`.
