---
name: compound-execute
description: The full take-a-job loop for the compound repo — resolve a work item or packet, claim it, dispatch the right executor lane, land with ceremony, close with proofs, and route learnings back into the system. Use when the operator says "take job X", "execute this packet", "continue the queue", or hands over any work item to run end-to-end.
---

# compound-execute — from "take that job" to closed Receipt, one ceremony

Proven shape: 2026-07-09, six packets landed in one day through exactly this loop (HANDOFF
LATEST-22..24). This skill replaces re-deriving that round pattern per session. It COMPOSES with
`compound-land` (step 6 defers to it entirely — do not restate landing rules here).

## 0 · Resolve the work (what did the operator actually hand you?)

**Orient before you resolve.** FIRES: any non-trivial task touching meaning, authority, or routing —
`compound orient --harness claude_code --task "<task>"` before the first mutation. AMBIGUOUS: "tiny
fix, but in a governed surface" → orient anyway; the ContextPack may carry a decision already in
force. DOES-NOT-FIRE: a mechanical edit inside an already-oriented session's claimed scope. The
failure it prevents: acting on a lane rule you remember instead of the one the runtime serves — a
superseded routing rule misrouted execution for eleven days because nobody re-read the served
surface.

- A work id (`work_…`) → `compound work show <id>` — read predicate, state, owner, packet content
  (`work active-packet-proof` serves packet markdown to any reader).
- A packet path (`docs_private/plan/packets/*.md`) → `compound work import-packet <path> --owner <lane>
  --state proposed` if no envelope exists yet; note the returned work id.
- A description ("the parity thing") → `compound workbench` + `compound attention --json` to find
  the matching item; confirm the match by predicate text in one line to the operator ONLY if two
  items genuinely collide — otherwise pick and proceed.
- NO free-floating execution: if the task has no envelope, create one FIRST
  (`compound work create --owner <surface> --predicate "<falsifiable done-state>" --by
  orchestrator --state ready`, then claim + advance). For source-controlled work, creation must
  also bind `--target-worktree-root <canonical-root> --target-local-ref refs/heads/<target>
  --expected-base-sha <full-sha>` and exactly one of `--publication-ref
  refs/remotes/<remote>/<target>` or `--local-only`. The K1 incident rule: unenveloped work is
  invisible to liveness probes; a `commit:*` artifact on an unbound envelope cannot complete.
- If a source-controlled legacy envelope is already nonterminal but lacks `git_delivery`, do not
  recreate it or work around close proof. An operator may bind the complete contract once through
  the existing same-state advance seam (add `--steal --reason` when another actor holds the lease):

  ```bash
  compound work advance <work_id> --to <current-state> --by operator:<id> \
    --steal --reason "operator-approved legacy delivery binding" \
    --target-worktree-root <canonical-root> --target-local-ref refs/heads/<target> \
    --expected-base-sha <captured-target-base> --publication-ref refs/remotes/<remote>/<target>
  ```

  Ordinary executor actors, partial declarations, lifecycle-transition-plus-bind, and rebinding
  are rejected. Use `--local-only` only when non-publication is an intentional task contract, not
  as a shortcut around push proof. `--steal` authorizes this one binding mutation; it does not
  transfer the lease, so subsequent lifecycle commands continue under the recorded lease holder.
- **Footgun (2026-07-09):** `compound work dispatch-claim` with NO ARGS claims the next ready item
  for real. Never run mutation verbs argless to probe usage — read `compound work` verb list or the
  dispatch-plan JSON instead.

## 1 · Claim, narrowly

- `compound work claim <id> --by <actor>` (lease) — proposed ≠ held; unclaimed work gets
  double-executed by a concurrent lane.
- Append a SCOPE CLAIM to `docs_private/plan/CODEX-WORKLOG.md`: exact packages/files, executor
  lane, expected base SHA, what you will NOT touch. Check the tail for live claims first — three
  lanes landed ~35 commits on one day (2026-07-09) with zero conflicts BECAUSE of this.

## 2 · Author or refine the packet (the executor's whole world)

A packet must carry: objective (one sentence) · verified seams with file:line (scout them — two
Sonnet scouts in parallel is the proven shape; batch fan-outs ≤3) · settled design decisions ·
falsifier test list (red-without/green-with) · hard file boundaries (touch ONLY / do NOT touch,
naming concurrent lanes' surfaces) · privacy law (repo is destined public; private ids only under
`$COMPOUND_HOME`) · verification the executor runs · handoff report format.
If the packet creates a NEW `internal/` package: include its `arch.Classification` entry
(spine/object-family/leaf/mem-target/importer) in scope — the arch doctor fails the battery on
unclassified packages (learned 2026-07-09, corpus-indexer land; lander patched it as a follow-up
commit). A new ORGAN needs THREE coupled gates, not one: the classification map, a two-class
`MintHome` entry in `internal/arch/mint.go` (`organ:<name>`, purpose + named consumer), AND a
`docs/architecture/status.md` package-table parity row — the doctor enforces Classification↔status.md
parity, and status.md sits AT its 250-line hot-path cap, so the new row usually forces a
compensating same-topic note merge elsewhere in the file (learned 2026-07-17, verdictflow land;
follow-up candidate: generate the table from Classification).
Packet citations are evidence, not canon: before building on a cited field/symbol ("X already
exists"), grep the struct/file it names. A premise error becomes a documented deviation + an
additive fix, not a blocker (learned 2026-07-10, volatile land: packet cited `Claim.ExpiresAt`
as existing; the field lived on Candidate only — added additively, canon_class precedent).
When a packet asserts BASE BEHAVIOR as a falsifier premise ("X clobbers/errors/is-absent at
base"), spot-check that claim against base before dispatch — one grep or one targeted run. A
scout's behavioral claim can be stale (lived 2026-07-16, posture-guard land: the "clobber
landmine" had been fixed at `481594b2`; the executor caught it, but a pre-dispatch check was
cheaper).
When a packet grants a narrow file set, probe the COLLATERAL INVENTORY tests before drawing the
fence: hardcoded step-count/proof-ref/name lists in sibling test files (`main_test.go`-style)
break mechanically when the granted files change, and an executor who correctly respects the
boundary can only hand you the remedy (lived 2026-07-17, smoke-inventory land: wiring four
smokes broke the dry-run gate inventory one file over; lander applied the executor's exact
remedy in the same land). Either grant the collateral file explicitly or pre-write the remedy
into the packet.
A falsifier can be GREEN-AT-BIRTH (existing structure already satisfies it — e.g. a visibility
whitelist already excludes the new value). Don't fake red and don't skip it: land the guard test
anyway and prove its teeth by a scratch mutation (open the gate, show red, revert), then report
green-at-birth honestly (same land). Executors revert scratch mutations with a TARGETED edit —
never `git checkout --` on uncommitted work (wipes everything, 2026-07-16 memo-lane incident) and
never `git stash` in a linked worktree (stash is repo-wide, not worktree-scoped: a pathspec miss
creates NO entry and the next `pop` dumps a DIFFERENT lane's stash into your tree — lived
2026-07-16, event-show land; recovered via `git checkout HEAD -- <conflicted files>`).
An INTERRUPTED lane's banked WIP must be assumed to contain unreverted scratch mutations until a
successor re-proves every falsifier's teeth on it (lived 2026-07-17, freeze-doctor land: the
salvage carried a disabled policy gate — `false && gateErr != nil` — a live governance bypass;
the successor's mandatory teeth-re-proof caught it before landing). Salvage is input, never
pre-verified work.
Packets live in the dir named by `compound workbench --json` → `offered_packets.source_dir` (currently `docs_private/plan/packets/`; gitignored — executors read them by ABSOLUTE path; fresh
worktrees don't contain docs_private).

## 3 · Route the lane (token ruling 2026-07-08, refined 07-09)

- Opus: the hardest packet of the round, and design-related work (operator ruling 2026-07-09).
- Sonnet: mirror-pattern / well-specified mechanical-plus-judgment packets.
- Codex: mechanical batch work (separate quota pool); wrap in an envelope; companion-status before
  declaring artifacts lost.
- Scouts (read-only recon) are a cheap lane. Never the orchestrator model itself as a subagent
  executor. Always pass `model:` explicitly.

## 4 · Dispatch (worktree discipline)

**`work advance --execute` vs `harness run`.** FIRES: any governed mutation on an enveloped item —
`compound work advance <id> --to running --execute --harness <name> --workspace <isolated-worktree> -- <cmd>`, the only governed
execution path. AMBIGUOUS: "let me just reproduce what the executor did, to check it" — still
governed; observe the run you already have (`compound work observe-run <id> --trace <trace_id>`)
instead of shadow-executing a second, unrecorded one. DOES-NOT-FIRE: a read-only probe with no side
effect. `compound harness run` is NOT the fallback: it mints the RunAttempt, prints a warning, and
reports `governed: false` — a manual record of a run, not an execution. When the governed path is
blocked, stop and say so.

Supply the isolated execution workspace explicitly for source-bound work. The
delivery contract names the canonical landing checkout and outranks caller cwd
in workspace resolution; starting the CLI from a worktree alone does not keep
the child out of main. Check the RunAttempt's recorded workspace as launch proof.

In the dispatch prompt, always include: the packet ABSOLUTE path · the envelope id · "read
AGENTS.md first" · commit incrementally, no push, no main, no install · the handoff report format.
For source-controlled work, the exact target root/local ref/base/publication contract comes from
the admitted WorkEnvelope and is rendered by dispatch; caller prose is not authority. Executors
NEVER touch the real `$COMPOUND_HOME` (test homes only) unless the packet names ONE specific
evidence-file write.

Once the executor worktree exists and its branch is created, attach the branch as an artifact on
the envelope so the worktree inventory (`compound executors worktrees`) can join exact envelope
identity onto the row: `compound work advance <work_id> --to <current-state> --by <actor> --artifact
worktree-branch:refs/heads/<branch>`. Attach it ONLY after the branch exists — a placeholder like
`refs/heads/(pending-dispatch)` becomes a dangling ref that matches no worktree and is silently
ignored, so identity stays exact rather than guessed.

## 5 · Watch (a "running" status is a claim, not evidence)

Background executors notify on completion. If one goes silent: verify pid + log recency before
believing any status file (K1 died silently for 2h). Codex jobs: ask the companion where the
worktree is BEFORE re-dispatching.
**In-harness agents are the OPPOSITE liveness problem** (2026-07-17): their task `.output` files
are written only at completion — a byte-stub with an old mtime is NOT death evidence, and they
pause through host sleep and resume on wake (three "dead 6h, zero tokens" agents all finished with
full handoffs). In-harness liveness = worktree file/git activity between two reads minutes apart.
Never dispatch a successor onto a lane without that check; if you do dispatch one, its FIRST act
must be committing the salvaged baseline, and a successor that zero-mutation STOPS on live-collision
evidence is behaving correctly.
**After ANY handoff, verify remote state yourself** (`git fetch` + `git log origin/main`): a
full-tool executor CAN push despite a NO-push fence, and task-notification re-stops give it extra
turns after its report (observed 2026-07-13, narrate land — executor pushed origin in a
post-handoff continuation; redact happened to be clean). Fences are prompts, not walls; the
orchestrator owns the remote.
**Re-read the ENVELOPE state (`work show <id>`) right before landing, not just at claim** (lived
2026-07-13, sycophancy land): the operator can retract mid-flight — steal the lease and advance
the envelope to `held` in a parallel surface while the executor runs. The executor doesn't watch
the ledger and will hand off clean anyway. A land against a held/retracted envelope gets reverted;
the check is one command. If held: do NOT land, do NOT close — bank the branch, log, surface.

## 6 · Land — invoke the `compound-land` skill. All of it, every time.

Non-negotiable additions learned 2026-07-09: land and cleanup are SEPARATE commands; if the guard
trips, main moved — rebase, re-battery, retry; tree-identity before worktree removal.
After local landing (and normal publication when the envelope requires it), record the live proof
with `compound work land-check <work_id> --tip <full-candidate-sha> --actor <lease-holder>`. The
command reads target and publication authority from the WorkEnvelope; do not certify from a clone
or replace the bound refs with caller arguments.

## 7 · Close with proofs (no proof refs, no close)

`compound work close-plan <id> …` then `compound work close <id> --outcome … --move-type …
--proof-ref commit:… --proof-ref check:<falsifier> --proof-ref live:<real-flow observation>`.
The recorded successful land-check is attached to the WorkEnvelope and close-plan must discover it;
local-target-only state remains `awaiting_publication` when publication is required.
`rule_delta` REQUIRES a failure/rule/check/guard proof ref. A live proof (the anti-"landed ≠
working" observation — real home, real flow) belongs on every close whose change has a runtime
surface. A live proof must not mutate the real home on pattern-match: verify an established
verb's semantics against the CURRENT home state first (lived 2026-07-17: `migrate --apply` on a
completed home folded post-plan runtime records into the frozen migration manifest and drifted
the plan digest — find_b642a87db247). Prefer read-only faces (preview/status) as live proof when
the mutating path is grant-gated anyway. Skipping a live proof when the change HAS a runtime surface
is the same class of error as attaching a fabricated one — both make the Receipt assert a
consumption nobody observed, and the omission is the one no gate catches.

**Source work in another repository still requires its own Git delivery proof.** `git_delivery`
binds one WorkEnvelope to one canonical repository, local target ref and publication contract;
that repository need not be Compound. A `commit:` artifact must resolve in that bound lineage.
For delivery closes, `ClosePlan` and `Close` derive scope from recorded actions and artifacts;
a recorded commit artifact requires delivery proof even with `audit_only`. The audit-only path
does not inspect every recorded mutation. Never relabel implemented source work as `audit_only`
or replace its commit artifact with `evidence:` to evade proof: that would misstate the outcome.

Bind the actual repository at creation. For an existing nonterminal envelope missing its delivery
contract, use only the operator-provenanced same-state binding described in §0; do not recreate,
silently retarget or cancel work merely to evade this gate. Execute that repository's supported
canonical proof profile, deliver to its bound refs, then record its `land-check`. An unsupported
consumer profile or missing native report remains an explicit verification gap; a certificate
from Compound or another repository cannot substitute for it.

For a Compound change followed by source changes in a product repository, use separately bound
source work and preserve their lineage. `live:` and `evidence:` refs may record cross-repository
consumption and cutover observations, but they supplement rather than certify the other source
delivery. Non-source runtime observations keep their existing evidence and authorization rules.

An already-recorded private-runtime or worktree-retirement effect is not a source
commit. Use `work delivery-capture` with its typed, exact-generation request to
read and custody the real evidence, then `advance-plan` and its generated guarded
same-state command for the operator-provenanced per-action binding.
If another actor holds the work, preview the approved one-operation override with
`advance-plan --steal --reason "<operator-approved scope>"`; its emitted command
retains the exact generation guards and does not transfer that actor's lease.
Capture is
`captured_not_bound`, not accepted work; a partial failure can have persisted
evidence, so follow its unchanged-request `retry_command`. Never relabel source
work, attach an unrelated commit, or mint a consumer-success report. Retirement
capture authenticates the existing completed transaction and retains recovery
bytes; it grants no new retirement permission and does not rerun Git mutations.
After a recovered retirement failure, capture each earlier failed action explicitly
with `action_intent_id` and bind it separately. The later completed witness must
match its exact plan/review; the earlier receipt stays failed.
Uncovered source effects and failed canonical attempts keep their existing gates.

## 8 · Route the learnings (the operator's law: "change the system based on the information")

A solution without its learnings routed is unfinished work. In the SAME round, write each learning
to the owner that will consume it:
- operational lesson → `CODEX-WORKLOG.md` RESULT entry (always) + `compound-land`/this skill if it
  changes the ceremony;
- program state → `docs_private/plan/HANDOFF.md` LATEST-N entry (commits, proofs, gates, queue);
- durable cross-session fact → auto-memory file + its MEMORY.md index line (keep both current);
- stale handoff/prompt lines elsewhere (NEXT-SESSION-PROMPT etc.) → fix them NOW, cite the
  correction;
- a repeated flaw with a code-shaped fix → file it as a named follow-up packet candidate in the
  worklog, never a silent TODO.
Then surface the OPERATOR GATES (ranked, with the exact one-line command where one exists) as the
final message — gates are decisions, not homework; everything else should already be done.

## Boundaries

- DEC-0021 stands: this skill smooths YOUR ceremony; it never auto-runs queued work. The scheduler
  rung (launchd `dispatch-step` under kill-switch) is W9's trust-dial decision — operator-gated.
- Normal pushes to the existing remote follow the standing grant. A clean `redact` scan is a
  separate required gate before public publication. Force-push, history rewrite, or a new remote
  still requires explicit confirmation; whitelist installs and destructive ops remain present-and-stop.

## Codex dispatch contract (learned 2026-07-11, burn-wave v1 — 3 dispatch-side failures)
- **Never let Codex clone into /private/tmp.** The sandbox edit-guard rejects patches there ("writing outside of the project"), Bun is denied its temp dir, and fresh clones lack node_modules → jobs return BROKEN or zero-work. **The ORCHESTRATOR pre-creates a linked worktree under the repo's `.claude/worktrees/` (inside $HOME, where approval_policy + xhigh reasoning apply), pre-copies `console/node_modules` + `docs_private` (cp -Rc), and dispatches Codex INTO it** ("work in place, commit on this branch, report SHAs").
- **Scope fences must be checked against the packet's own falsifiers before dispatch.** A fence that forbids a surface the packet load-bearingly needs produces a correct STOP (good agent behavior, wasted quota). Read the packet falsifiers, then draw fences.
- **A Codex self-verdict of BROKEN/should-not-be-landed is salvage input, not failure:** fetch the partial commit from the clone (`git fetch <clone-path> HEAD` + cherry-pick), hand the successor the salvage + the remaining delta.
- Require in every prompt: commit + report SHA + per-item red→green proof; ask the companion for artifact locations before declaring anything lost.
