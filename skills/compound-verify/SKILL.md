---
name: compound-verify
description: Prove a compound change is ALIVE (wired, fed, cut over, provable) — not merely merged. Use before closing any work item, before claiming "done"/"landed", when landing an executor's diff, or when auditing whether a "landed" marker in HANDOFF/status/VISION actually works. The anti-"landed ≠ working" skill.
---

# compound-verify — consumption-grade verification

The 2026-07-06 solid-ground audit found the repo's dominant failure: **"landed" markers track
code commits, not live consumption.** Machinery merges green while the wiring, data, cutover, or
proof never happens — and the system silently fails its own north-star test. This skill is the
checklist that catches that *before* close, instead of in a 12-agent audit months later.

It specializes the generic `verification-before-completion` / `finishing-a-development-branch`
skills for this repo; it replaces re-deriving the verification ritual from
`docs_private/plan/HANDOFF.md` prose each session.

## The four gaps (every "landed ≠ working" case is one of these)

Verified instances from the audit — memorize the pattern, not the instance:

| Gap | Question | Real failure it caught |
|---|---|---|
| **WIRING** | Is the new code reachable from the canonical surface, or does the old path still run? | owned breadth engine built (`internal/breadth` + `cmd/compound-breadth-engine`) but recall still shelled to the external legacy CLI |
| **DATA** | Does real (or synthetic-fixture) data actually flow through it? | consume-receipts machinery landed but starved — 0 live skill edges ever recorded |
| **CUTOVER** | Did the default/consumer flip to the new path, and is the old path retired or tombstoned? | live hook adapter landed; sessions still ran the slow `go run` path — cutover never flipped |
| **PROOF** | Does a command or test now FAIL if this breaks? | recall "works" but no golden set / bakeoff harness existed — unprovable either way |

A change is **alive** only when all four hold. Anything else is **landed-not-live** — say which
gap, loudly, in the receipt/close notes.

### The vacuous pass — a PROOF gap wearing a green check

A gate that cannot fail is not a weaker gate, it is **not a gate**, and it is far harder to notice
than a missing one because the suite is green. Two shapes, both lived 2026-07-27 (four gates at
once, `fc4cd8e3`):

- **Empty is not failing.** `mem.Report` only flips `OK` on a `"fail"` hop, so on a home with no
  claims every hop reported `"empty"` and `memory doctor` exited 0 having asserted nothing.
  `workbench --json` returned every count `0` with no error, and its gate read only the exit code.
  Both console consistency smokes explicitly tolerate an empty projection. **Ask of any gate: what
  does it do when its subject is absent rather than broken?**
- **Isolation can CREATE the vacuum.** Removing a check's dependence on ambient state (a live home,
  an installed binary, the clock) is usually right — it was worth ~109s/battery here — but the same
  move can turn a slow real check into a fast fake one. A bare temp home made all four gates
  *faster and more wrong* than the live home they replaced.

**So when you cut a check off from ambient state, seed a fixture and assert its content, then prove
the assertion is load-bearing:** neuter the assertion and confirm the unseeded-input control goes
green. If a control cannot be made to fail, it is documentation, not proof.

Two corollaries worth their own line:

- **A function's tests do not test its wiring.** Per-gate tests that call the runner directly all
  stay green when the one dispatch line is deleted — and the gate silently reverts to ambient state.
  Assert the dispatch too (`proofCheckHomeOwningGates` + an absent `COMPOUND_HOME`).
- **Some failure modes are unreachable through the public API, so a control must bypass the write
  door — deliberately and with a comment saying why.** An unparsable ledger row and a
  zero-provenance claim cannot be produced by `Adjudicate`; an unreadable work envelope cannot be
  produced by `Create`. A control that respected the door could not prove those gates can go red.

Tests that use user-cache APIs must isolate the user and cache environment under a temporary fixture, verify that ownership before cache side effects, and rely on fixture-owned cleanup rather than deleting an unchecked returned path.

### Process readiness is not admission readiness

If marker existence signals readiness, publish its complete contents by sibling-file rename;
creating an empty marker before writing exposes false readiness to the observer.

For subprocess lifecycle tests, start the process-readiness budget only after successful
`cmd.Start`, not before durable admission. Keep admission bounded by the test context,
surface early admission/start/exit errors, and cancel and join the worker before fixture
cleanup. Prove delayed admission does not spend the unchanged readiness budget, while a
missing marker, early exit, or cancellation still fails. Never relax takeover exclusion
or termination assertions to accommodate setup latency.

## The battery (run in order; each step is checkable)

1. **Never trust the checked-in binary.** `./compound` at repo root is a local build artifact and
   is routinely stale. Verify through `go run ./cmd/compound <verb>` or a fresh `make install`.

   The scoped presubmit (`compound-land` §1) is what every landing runs before its fast-forward;
   `go run ./cmd/compound proof-check --dev --base <full-sha> --out <file>` is its whole-module
   cached variant. Both are non-certifying: neither writes the canonical installed-runtime proof
   artifact or satisfies the deterministic bundle below, which runs on the landed tip after the
   land and before any governed close.

2. **Deterministic bundle:**
   ```bash
   # Use the coverage base already selected and validated in compound-land §1.
   test -n "${PROOF_BASE_SHA:-}" &&
   CHECKED_PROOF_BASE_SHA=$(git rev-parse --verify "${PROOF_BASE_SHA:?complete compound-land base selection first}^{commit}") &&
   test "$CHECKED_PROOF_BASE_SHA" = "$PROOF_BASE_SHA" &&
   go run ./cmd/compound proof-check --base "$CHECKED_PROOF_BASE_SHA" \
     --work <work_id> --actor "<harness>:<lane>" \
     --out <absolute-report.json>
   ```
   The base is inherited from `compound-land` §1 and re-checked here, never re-guessed.
   `CHECKED_PROOF_BASE_SHA` resolves the inherited `PROOF_BASE_SHA` to a commit and the
   equality check requires it to be that same commit. A nonempty name is not a base: a
   nonempty `PROOF_BASE_SHA` can still be a moving or mistyped reference, so the resolution
   and the equality check are what certify the base — the proof runs on
   `CHECKED_PROOF_BASE_SHA`, never on the inherited name. Never recapture `main^{commit}`
   here: after landing, `main` has moved, and re-reading it would certify a different
   `base...HEAD` range than the one `compound-land` proved.
   The actor must already hold the named WorkEnvelope lease. Certifying proof is agent-invocable:
   non-terminal, redirected, and backgrounded stdout are supported; the authoritative full report
   is `--out`. Run it unpiped as its own command by default and preserve the actual process exit
   because zsh pipelines can mask failure. This is shell-observation safety, not proof admission.
   Optional --execution-timeout <positive Go duration> bounds active canonical
   proof preparation and admitted execution. It starts before source/profile
   subprocesses; preparation consumes the same allowance later used by the admitted
   run. Queue wait time is excluded, and --ttl remains heartbeat lease liveness.
   Omitting the option preserves interactive unbounded behavior. Lightweight
   and recovery modes refuse it; it has no harness-derived 45-minute ceiling.

   The deadline cancels existing process groups, stops further gate setup and
   checks cancellation after report publication and success-source revalidation.
   That check is the execution cutoff. Expiry before it fails the original attempt,
   retains failed/partial output, and cannot certify. Durable failure publication,
   journal persistence and safe resource cleanup run synchronously afterward and
   may exceed the allowance; a kernel-blocked filesystem call is not a hard
   wall-clock promise. Output rewrites retain exact rooted publication custody.
   Local SSH cancellation does not prove remote quiescence: the exact remote fence
   remains until native reconciliation receives matching quiescent evidence.
   No gate inventory, acceptance condition or Go test package timeout changes.

   For an exact fresh local reservation whose execution barriers have closed,
   proof-check --reconcile-quiescent PFR --work W --actor A --expected-run R
   uses native failed finalization without queue admission, a new attempt, or
   verification credit. Execution quiescence does not assert producer exit.
   Current work action/policy must still belong to that reservation, its rooted
   refresh, or its exact pending recovery completion. Finalization refuses expired,
   remote, live-child, missing-barrier, changed-holder and unrelated-journal cases.
   Only this command's exact authenticated failure journal may resume the original
   running or matching terminal attempt while the same fresh holder remains;
   no migration or retry descendant is admitted.

   If that exact failure already committed and the original holder is absent or
   replaced, the same command can clear only its old journal. It requires the
   matching terminal original, complete stored governance/result/completion and
   lifecycle evidence, and unchanged current work generation. It preserves the
   successor and every other durable byte; resource_released=false and the
   release_warning state that this call only cleared the journal. Already resolved
   runs without that journal refuse. Ordinary retained source profiles do not
   require a readable checkout; private-source profiles retain native current
   review/destination authority. Recovery cannot supply a report, exit code, queue
   wait, execution timeout, or success assertion.

   Custom `--ttl` values below 30s are refused before
   admission; accepted windows refresh at `min(TTL/3, 5s)` and monitor holder loss continuously.
   Each gate inherits the shared land-lease kernel execution barrier. Lease loss, SIGINT/SIGTERM,
   and governed steal requests cancel and wait the whole gate process group; generic landing
   acquire, expiry, and `run-attempt sweep` cannot bypass a live descendant. A first live-steal
   request may refuse while it asks the predecessor to stop; retry the exact command after release.
   A stale already-dead predecessor uses
   `--steal --reason "<why>" --terminated-run <ratt_*>`; Compound's barrier probe produces the
   immutable termination evidence, so caller JSON and bare/reused PIDs are not proof. `--out` receives the full report and
   stdout ends with the truthful battery verdict. Dry-run/CI subsets are lightweight diagnostics and cannot mint
   governed verification. `canonical_full` requires every reserved gate to be `ok`—zero skips—to
   certify. A complete canonical red battery requires every gate `ok|failed` with at least one
   failure; it finalizes non-certifying evidence, releases the resource, preserves the full report,
   prints one `battery: FAILED` verdict, exits 1, and creates no VerificationID. Refusal does not
   create or alter `--out`; successful publication is an atomic synced rename.
   Installed-runtime publication is likewise atomic and completion-marked; a partial prefix is
   ignored, and a post-release publication fault emits a final non-green verdict with exit 2.
   Any error after a complete durable report prints exactly one final non-green battery verdict;
   pre-report errors print none. A committed green or red result with a retained lease is not a green command: it returns an explicit
   release-pending error. The exact retry resumes the prior owned report before reservation and
   atomically restores and byte-verifies `--out` from Compound custody before it releases, without
   rerunning gates or minting a new RunAttempt. Restore failure retains exclusivity and cannot
   print green. The first command preserves `--out`, ends stdout with
   `battery: FAILED release_pending=true`, and exits nonzero. The installed-runtime Workbench
   artifact is published only after green governed completion and release; candidate,
   release-pending, failed, invalid, dry-run, and CI reports cannot consume that action.
   Green means: `go run ./scripts/testbattery general-release` then `go run ./scripts/testbattery home` (every source package once with `-count=1 -timeout=40m`; general packages first, CLI tests alone next, Work tests alone next, release durability tests alone next, hook tests alone next, home tests alone last), `go run ./scripts/testbattery vet`, `go run ./scripts/testbattery build`, `git diff --check`,
   `architecture doctor --strict`, workbench projection, `memory doctor`, and the public redact
   scan of changed files all passed. Any red stops the close — fix or escalate, never weaken the
   check.

   All three Go gates derive package directories from Git tracked/staged files plus nonignored
   new source, then ask Go to resolve them. Copied ignored evidence remains present but cannot
   add packages. No private directory-name allowlist is used. Go still consumes generated files
   inside owned packages; a new generated-only package needs a tracked or nonignored Go source
   anchor. Test-only packages are tested and vetted, but are not standalone build targets.
   Normal platform/build-tag, testdata, hidden-directory and vendor semantics remain Go's;
   unsupported nested source modules, missing/symlinked source and discovery errors refuse.
   Canonical commands also bind the existing sealed HEAD with --source-head SHA and require
   clean tracked/index/nonignored-untracked state before and after each Go gate. Source left
   behind by a test fails that gate and prevents a later gate accepting a different inventory.

   The certifying base is the explicitly selected immutable `PROOF_BASE_SHA` (B).
   Its validated value may equal the contract base or current target, but it is
   never inferred blindly from either. Select it through `compound-land` §1;
   Compound resolves it once. A dirty or uncommitted candidate is refused.
   The report seals repository fingerprint, base SHA, HEAD SHA,
   tree SHA, and clean state; diff-check and redact inspect the same committed `base...HEAD`. A
   rebase, replay, or new HEAD invalidates the prior proof and requires a fresh battery. New
   `--out` reports are private, ordinary existing modes are preserved, symlink/non-regular/raced
   targets are refused, publication is atomic within the captured parent, and timing warnings are
   sealed before the file, installed artifact, and JSON agree.

   Selected console dependency prerequisites are checked before any gate starts.
   A missing dependency or unavailable TypeScript compiler refuses the run without
   claiming completed gate evidence; the full gate inventory and per-gate checks
   remain required. Dry runs still describe the planned inventory.

   Canonical gates execute from a detached temporary checkout of the sealed HEAD,
   with only the five ceremony-owned ignored inputs copied as snapshots; the
   mutable caller worktree is never the gate execution surface.
   Go build-cache identity belongs to the original caller worktree, not the
   temporary sealed checkout: repeated batteries reuse that caller's cache while
   sibling worktrees stay isolated. Ordinary gates and Bun tool children inherit
   the same cache selection; `COMPOUND_PROOF_SHARED_GOCACHE=1` still preserves the
   inherited `GOCACHE`. Cache reuse does not replace the canonical `-count=1` tests
   or prove a speedup; measure complete comparable runs before claiming one.

   Since `fc4cd8e3` the home-reading gates (`memory doctor`, workbench projection, the attention
   and agents console smokes) each seed and own a temp `COMPOUND_HOME` and assert its content, so
   their verdict is about the diff rather than about your dev home's size — and they no longer get
   slower every time you use Compound. Fixture probes and console `COMPOUND_BIN` must resolve
   through the proof context's pinned runtime, including these home-owning gates: a long proof
   can outlive its original `go run` cache executable. A source-removal regression must still
   execute both the nonempty fixture check and its real consumer against the pinned copy.
   They are listed in `proofCheckHomeOwningGates`; a gate that
   leaves that map silently goes back to reading ambient state.

3. **Temp-home end-to-end** (proves the slice works outside your polluted dev home):
   ```bash
   export COMPOUND_HOME="$(mktemp -d)"
   go run ./cmd/compound home init   # COMPOUND_HOME selects the temporary home
   ```
   Do not add `--local`: it overrides `COMPOUND_HOME` and selects `<cwd>/.compound`.
   Use synthetic fixtures only. The slice must produce its envelopes/artifacts under the temp
   home; `home doctor` must stay clean; repo tree must stay unchanged (`git status --short`).
   Unset `COMPOUND_HOME` afterwards.

4. **Consumption check — name the live consumer, then watch it consume.**
   Ask: *who reads this output next?* (`orient`? `workbench`? a hook injection? a console card?
   the distiller?) Run that consumer's command and point at the new behavior **in its output**.
   Grep-in-source is not consumption; JSON in the consumer's output is. If no consumer exists
   yet, the move is `audit_only` — it does not count as done; record the missing consumer as the
   open branch.

5. **Cutover check.** Name the old path this change supersedes. Show it is retired, tombstoned,
   or explicitly deferred with a recorded retirement condition. "Both paths live, new one
   optional" is the fragmentation pattern this repo exists to kill.

6. **Depth test.** Point to the specific test that fails if the invariant breaks. If you cannot
   name one, write it — and prove it by breaking the code once (mutate, watch red, revert). A
   test that cannot fail is documentation, not proof.

7. **Work-chain proof** (when the change closes a WorkEnvelope): the same `work_id` must link
   context pack → action → policy → receipt → transition. Check with
   `go run ./cmd/compound workbench --json` / `transition show <id>`. A `RouteDecision`, a plan
   artifact, or a written report is never task success — only the receipt's `state_delta` is.

   For source-controlled work, the `land-check` candidate tip, current attached commit
   candidate, and stored successful proof HEAD must be identical. A rebased or mismatched tip
   is non-certifying and close refuses it until a fresh battery records the new HEAD.

A rebase, replay, or new HEAD invalidates prior proof and requires a fresh battery.
For source-controlled work, a committed stored successful proof is required:
`land-check` candidate tip, current attached commit candidate, and stored proof HEAD
must match; close refuses a missing, mismatched, or rebased proof.

## Verdict format (end every run with exactly one)

- `ALIVE` — all four gaps closed; cite: proof-check result, consumer command + observed output,
  retired old path, failing-test name.
- `LANDED-NOT-LIVE (<wiring|data|cutover|proof>)` — machinery merged, gap named, follow-up
  branch recorded (who closes it, where).
- `BROKEN` — battery red; evidence attached; do not close the work item.

Under-verdicting is not the safe side: withholding `ALIVE` when you already read the new behavior in
the consumer's output is the same class of error as declaring `ALIVE` from a grep hit — both send
the next lane through a full re-verification round to find out which one it was.

Report the verdict with its evidence verbatim — never soften `LANDED-NOT-LIVE` into "done".
