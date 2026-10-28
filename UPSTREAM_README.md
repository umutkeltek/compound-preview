# compound

Compound keeps your goals, decisions and work history across agent sessions.
Use your supported harness to do the work; Compound supplies relevant context,
coordinates governed execution, and records evidence you can inspect later.

It runs locally under one trusted OS user. Your runtime data lives in
`COMPOUND_HOME` (default `~/.compound`); Compound needs no cloud account.
Your chosen harness still needs its own installation and authentication.
The optional dashboard helps you see what is progressing, what needs a decision,
and which observations are stale or unavailable.

**Current status: developer preview.** Setup still includes terminal steps.
A no-terminal onboarding journey and universal harness compatibility are not
accepted release claims. Recording work is not itself proof that future work
became easier.

## Supported Scope (Public Alpha)

The public alpha is a strictly scoped, file-backed, single-user development
system. Its intended boundary (target contract):

- file backend only — Postgres remains an experimental, never-authority fence;
- one trusted OS user on local disk;
- explicit home authority — ambient ancestor `.compound` discovery is not
  an authority target;
- interactive hooks may observe; enforcement claims require behavioral proof;
- the console is projection-only; direct private reads are unavailable.

The runtime enforces the projection-only console default. The unsupported
mutation extension requires an explicit home and an out-of-band capability;
it is outside the supported alpha console surface.

Postgres-backed storage, shared homes, server/multi-tenant use, remote or
proxied console, and arbitrary private embedding endpoints are unsupported.
Full contract, including current blockers:
[Supported Scope and Security Boundary](docs/security/supported-scope.md).
See also the [support matrix](SUPPORT.md), [security policy](SECURITY.md),
[recovery procedure](docs/operations/recovery.md), and [changelog](CHANGELOG.md).

## Start here

Follow [Getting Started](docs/getting-started.md) for the supported installation
path. Prebuilt release instructions apply only to an actually published stable
release; otherwise use a reviewed contributor checkout.

1. **Install and check the local home.** Start with `compound version` and
   `compound home doctor`. Keep data outside your project checkout.
2. **Connect one harness.** Follow [Harness Wiring](docs/harness-wiring.md).
   Claude Code and Codex have managed interactive hooks; OpenCode has an
   observation adapter. Other harnesses need an enrolled, supported adapter,
   not just a name in a command.
3. **Give your agent one goal and boundaries.** Ask it to inspect existing work,
   preserve the chosen executor, and use the governed work lifecycle. You should
   not need to copy internal IDs between sessions.
4. **Inspect the result.** Use `compound workbench`, `compound coordination`,
   or the optional [dashboard](docs/console.md). Execution success is separate
   from verification and acceptance.

For an already claimed task with a saved execution selection, the existing
selected-work path resolves the actor's sole held task:

```sh
compound work continue --selected --actor <your-existing-actor> --json
```

This does not create or claim initial work, choose an arbitrary task, or grant
new permission. Ambiguous selection, unsettled execution and missing authority
remain explicit stops. [Daily use and recovery](docs/getting-started.md#daily-use-and-recovery)
explains resuming and verification.

The [explicit lifecycle demonstration](docs/getting-started.md#2-orient-on-your-repo)
is for contributors learning the underlying commands, not the required first-run
user experience.

### Optional dashboard

The prebuilt archives contain the CLI and engine, **not** a dashboard bundle.
The console currently requires a matching source checkout, Bun and Node 20+.
Its supported browser surface is local and read-only; it is not a replacement
for the harness's governed execution path. See the [console setup guide](docs/console.md).

For contract consumers:

```bash
go run ./cmd/compound schema list
go run ./cmd/compound schema export context_pack
go run ./cmd/compound envelope validate fixtures/envelopes/*.json
```

That last command exits non-zero on purpose: `fixtures/envelopes/work.invalid.json`
is a deliberately malformed fixture, so a correct run reports it `INVALID` and
fails. Every other fixture must report `ok`.

For semantic memory (search over your docs): see the
[Memory Quickstart](docs/memory-quickstart.md).

For reusable product workflows—from discovery and UI/UX through release, distribution,
retention and skill improvement—see the [public lifecycle skills](skills/product-pipeline/README.md).
They include conditional branches, proof/recovery contracts and
[pinned source credits](skills/product-pipeline/SOURCES.md). No private overlay or upstream
bulk installation is required; offers are not unattended execution or business-outcome proof.

## Why

Four failure classes eat agent-assisted work, and most stacks have no organ
against any of them:

- **The rediscovery tax.** An agent burns rounds re-deriving what the system
  already knew, because knowledge existed but nothing served it at the moment
  of need.
- **Landed is not working.** A change merges, a status doc says done, and the
  behavior never actually changed. Reports are trusted; flows are not exercised.
- **The blind cron.** Scheduled automation asserts effects on a schedule,
  exits 0 while broken, and nothing consumes its absence-of-effect as a signal.
  A blind cron is a liar with a schedule.
- **Decision homework.** Every judgment reaches the human as a CLI incantation
  or a wall of context to reverse-engineer, instead of a plain-language card
  with one honest act.

Compound closes these as loops, not as features: evidence comes in, receipts go
out, and the system's next behavior provably differs. When it doesn't, that gap
is itself recorded as a failure class — and compiled into a lens that fires the
next time the same shape appears.

## One night, end to end

A real sequence from building this system (details sanitized; the mechanics are
exactly what this repo ships):

An operator's work sat blocked on a host everyone believed was on. A network
probe said it was dead; a second look inverted the probe — it was the overlay
network's plane that had failed, and no single observation had the authority to
say so. The moment compiled into a lens: *every single-plane observation is a
hypothesis — assertions and probes alike.* It now fires at orientation whenever
work depends on a host.

The operator then ruled, in one sentence, that the system should self-heal
rather than run blind cron jobs. By morning the ruling was runtime: divergence
observations (`expected_state != observed_state`) surface immediately as cards,
a director proposes remediation envelopes from them, and evidence-thresholded
grants let a proven work class admit itself — while anything external,
destructive, or irreversible stays with the human by construction, not by
policy prose.

Then the punchline: a decision the operator kept being handed as homework
turned out to have been made days earlier. Nothing could say so until an organ
owned that state. The card built to *ask* the question instead correctly
reported there was nothing to ask.

Feeling → compiled lens → landed code → live proof, each step with a receipt.
The loop this repo exists to make ordinary.

## Principles

- **Discover reality before acting.** Probe first; the map is never the
  territory, including the system's own map of itself.
- **Own the spine; rent the executors.** Meaning, policy, and proof are owned;
  models, engines, and harnesses are replaceable edges.
- **Store evidence, not truth.** Every claim carries provenance and a
  falsifier; supersession is a record, never a prose edit.
- **Recall only what is relevant.** Context is served in task-scoped slices,
  not preambles.
- **Keep humans on irreversible decisions** — by setting envelopes and standing
  grants, not by being paged. A nagging agent violates this rule; it does not
  enforce it.
- **Prove the change; don't trust the report.** A close without a live-flow
  observation is a claim, not a fact.
- **Convert repetition into infrastructure.** Repetition count — not idea
  elegance — is what earns a new organ.
- **Subtract by default.** Everything created is either enforcement (compiled
  from a receipted ruling, blocks silently, carries undo) or evidence (names
  its consumer, or decays). Anything else is not built.
- **Articulate boldly, hold falsifiably.** Even a wrong articulation is a kind
  of sense: a specific claim gets corrected with one line and a receipt, while
  unarticulated sense stays mush — and mush cannot compound.

## What It Is

Compound is built envelope-first, not memory-first. Hold the shape as five
places, not eleven nouns:

- **The door.** Everything that enters — a task, an observation, a model's
  output — arrives as a typed envelope and is validated before it can matter.
- **The ledger.** Meaning lives in one adjudicated home: nothing becomes
  ambient truth without a recorded verdict, and supersession is a record,
  never a prose edit.
- **The edge.** Executors — models, harnesses, engines — are rented: they
  propose, extract, and execute, but never own truth, promotion, or policy.
- **The gate.** Every mutation asks policy first; destructive and irreversible
  actions stay with the human by construction.
- **The receipt.** After action, a receipt records what actually ran, and a
  transition records what it means for future behavior.

In ontology terms, Compound's typed object families form an operational
ontology: shared entities, constraints, lifecycles, and evidence rules that
every executor must use at the runtime boundary. Probabilistic models may
extract, propose, or execute; deterministic code validates and adjudicates
their output before mutation; receipts and transitions verify what actually
changed.

The typed grammar behind those five places, for contract consumers:

```text
ContextRequest
-> ContextPack        the briefing: what the agent is told, and what was left out
-> WorkEnvelope       the job: one stable identity for one unit of work
-> ActionIntent       what an executor wants to do
-> PolicyDecision     the gate's answer: allow, deny, or hold
-> Receipt            proof the job ran: what was touched, what came out
-> Transition         the ledger entry: state A -> B because of this work
-> candidates / adjudication / projections
```

Memory is one object family on that spine, alongside decisions, capabilities,
products, route evidence, runtime bindings, and policy. The apex is the
`Transition`: what changed, which evidence supports it, and how future behavior
should differ.

## Where State Lives

The repo is source: code, schemas, tests, docs, and fixtures.

`COMPOUND_HOME` is the runtime body: ledgers, events, work, context packs,
candidate inboxes, registries, artifacts, reviews, exports, and rebuildable
indexes/projections.

Resolution order:

```text
--home -> --local (for commands that support it) -> COMPOUND_HOME -> ~/.compound
```

Ambient ancestor discovery (nearest `.compound/manifest.yaml`) is retained only
as a non-authoritative internal compatibility/read-only discovery API. Normal
home resolution never selects an ancestor marker as authority: use `--home`,
`COMPOUND_HOME`, or an explicit local selector such as `compound home --local`
for mutating commands that should operate on a repository-local `.compound`
home; see the [supported scope contract](docs/security/supported-scope.md).

Private paths, credentials, accounts, raw personal corpora, and real operator
data belong in a private Compound instance, not tracked public files.

Useful home commands:

```bash
compound home backup [--target archive.tar.gz]
compound home export --public-fixture [--target fixture.tar.gz]
compound home verify [archive.tar.gz]
compound home restore archive.tar.gz --target <new-absent-home-path>
compound home compact [--dry-run] [--rebuildable]
```

Atomic `home restore` publication is supported on Darwin and Linux. Other
platforms fail closed before publishing the staged home.

Restore accepts Compound backup archives only; public fixtures are not recovery
artifacts. Verification completes before a staging directory is allocated. If a
later restore step fails, Compound reports and retains the exact empty `0700`
staging directory for explicit inspection and cleanup. A Compound home root is
private (`0700`); diagnose or repair an older root explicitly with
`compound home private-custody repair [--dry-run]`.

Backups preserve durable evidence/work/config state while excluding transient or
rebuildable roots such as `tmp`, `lock`, `exports`, `indexes`, and
`projections`. Public fixture export emits sanitized layout and manifest
metadata only.

## Main Surfaces

Runtime and contracts:

- `compound home ...` manages the runtime body.
- `compound envelope validate/kinds` validates typed runtime envelopes.
- `compound schema list/export/validate` exports JSON Schema and validates files
  through the owning runtime validators.
- `compound contract compile` and `compound orient` produce EffectiveContract
  and ContextPack artifacts, including included/excluded refs, active hook
  rosters, and structured capability offers.
- `compound hook install` writes a compiled managed native roster and records
  the same posture as `compound://config/installed-hooks`; manual
  `compound contract import-hooks --codex-hooks-json <hooks.json>` or
  `--hook-registration-ts <hook-registration.ts>` imports generated rosters,
  and `compound contract hooks-authority` reports that handoff.

Execution and policy:

- `compound work ...` creates, claims, advances, closes, and imports work.
- `compound workbench` is a read-only board over open WorkEnvelopes,
  terminal receipts, transitions, and optional CapabilityGraph offers; it does
  not dispatch, execute offers, or mutate work.
- `compound transition ...` exposes the proof apex joining work, receipts,
  evidence, and typed projections.
- `compound proof-check --work <work_id> --actor <lease-holder> --out
  <report.json>` runs the canonical deterministic local proof bundle: tests,
  vet, build, diff check, strict architecture doctor, memory doctor, and public
  redact scan. Certification is custody-based — the `--out` report held under a
  still-live land-lease is the proof — so headless harnesses and scheduled
  queues can certify without an interactive terminal. Red batteries are
  retained as non-certifying evidence and exit 1; recovery from a dead
  predecessor fails closed behind a kernel execution barrier. The full lease,
  refusal, red-battery, and recovery contract:
  [Proof-Check and the Land-Lease](docs/architecture/proof-check.md).
- GitHub-hosted Actions are disabled in this repository. `.woodpecker.yml` runs
  routine CI and gated release builds on the owned Woodpecker agent. Publication
  starts on the maintainer's Mac as a draft; the maintainer reviews and publishes
  it explicitly. Tracked GitHub workflow files are inactive references kept for
  tests. See [Release procedure](docs/release.md). CI subsets remain fresh-machine
  evidence; landing requires the full local `proof-check` battery.
- `compound reviews [--json]` reads owner-review artifacts without applying
  them.
- `compound policy evaluate` turns ActionIntent into an enforceable
  PolicyDecision.
- `compound authority dirty-plan <repo> --work-id <owner-work>
  --action-intent <persisted-id>...` derives an exact dirty-mutation review
  capability from persisted hook actions; it never trusts caller-authored
  action semantics or performs source-control cleanup. Authorization requires
  `dirty-review <review.yaml> --reviewed-by operator:<actor>` and a sealed,
  source-checked report.
- `compound policy import-placement-rules <placement-rules.json>` installs owner
  placement rules into `COMPOUND_HOME/registries/placement-rules.json`;
  `compound policy placement-authority [--json]` reports the machine-readable
  handoff for OS/operator consumers.
- `compound policy import-operator-state <operator-state.json>` installs the
  global autonomy ceiling into `COMPOUND_HOME/registries/operator-state.json`;
  `compound policy operator-authority [--json]` reports the machine-readable
  handoff for OS/operator consumers.
- `compound writeback ...` gates owner writeback and no-promote decisions
  through policy and receipts.
- `compound hook ...` and `compound harness ...` normalize harness events into
  spine objects; native blocking hooks return deny decisions, while `compound
  hook pre-tool-use --enforce ...` returns policy gate exit codes for
  non-blocking adapter fallbacks.

Meaning and evidence:

- `compound memory ...` owns candidates, evidence, claims, recall, adjudication,
  timelines, and reviewed write plans.
- `compound knowledge ...` imports source-backed knowledge items into evidence
  and candidate paths without direct promotion.
- `compound synthesis ...` records memory synthesis sweeps; contradiction
  sweeps may reopen claims to review, while cluster proposals stay report-only.
- `compound distiller ...` turns traces/runtime source material into candidate
  sets; it never promotes model output by confidence alone.
- `compound decisions ...` imports, repairs, and audits DecisionLifecycle source authority.
- `compound authority ...`, `compound source observe`, and `compound event
  import-os` adapt existing sources into evidence and candidates.

Adaptive systems:

- `compound capability ...` owns the capability graph. It surfaces offers with
  constraints and flat JSON summaries; it does not execute skills/tools.
- `compound panel ...` runs lens-based reviews over candidate/review queues and
  applies only through governed proof paths.
- `compound local-models advise ...` reports local model context/load/unload
  intent without mutating provider runtime.
- `compound route ...` records route evidence, exposes read-only route
  evidence authority, checks provider-call cost coverage, writes
  receipt-linked route outcome reviews, and uses Compound-owned model-router
  route plans from imported capability/default-lane authority.
- `compound runtime doctor ...` checks RuntimeBinding/ServiceContract bundles;
  `--live` runs explicit HTTP/TCP/file/env probes.
- `compound product ...` handles product identity, signals, hypotheses, source
  issues, reviews, and product timelines.
- `compound projection ...` checks generated-view freshness and records scoped
  projection context.

## Guides

| Guide | For |
|---|---|
| [Supported Scope](docs/security/supported-scope.md) | What the public alpha supports, and where its security boundary is |
| [Getting Started](docs/getting-started.md) | First-time users: install, orient, create work, wire your agent |
| [Harness Wiring](docs/harness-wiring.md) | Connect Compound to Claude Code, Codex, OpenCode, or any CLI harness |
| [Memory Quickstart](docs/memory-quickstart.md) | Semantic search over your docs in 10 minutes |
| [Console](docs/console.md) | The visual operator dashboard |
| [Contributing](CONTRIBUTING.md) | Build, test, architecture rules, and PR expectations |

## Architecture Map

Read in this order:

1. `docs/LAYERS.md`
2. `docs/architecture/status.md`
3. `docs/architecture/epistemics.md`
4. `docs/architecture/runtime.md`
5. `docs/architecture/contracts/README.md`
6. `docs/memory-architecture.md`
7. `docs/architecture/source-adapters.md`
8. `docs/operations/legacy-system-retirement.md`

The spine is 60+ packages under `internal/`; `compound architecture doctor
--strict` enforces that each classifies under exactly one layer of
`docs/LAYERS.md`. The load-bearing ones:

```text
cmd/compound/             CLI boundary
internal/home/            COMPOUND_HOME runtime body (layout, doctor, backup)
internal/envelope/        typed runtime grammar
internal/schema/          JSON Schema export + contract-aware validation
internal/contract/        EffectiveContract + ContextPack assembly
internal/work/            WorkEnvelope lifecycle + read-only workbench
internal/runattempt/      RunAttempt lifecycle: spawn -> terminal proof
internal/landlease/       landing mutual exclusion (one lander at a time)
internal/policy/          ActionIntent -> PolicyDecision
internal/act/             governed outward actions: transports + receipts
internal/attention/       hold frontier: what needs the operator, grouped
internal/operatornotice/  attention holds reach the operator's transport
internal/director/        proposals + evidence-thresholded admission
internal/harness/         HarnessRegistry
internal/hook/            lifecycle hook runner
internal/knowledge/       source-backed knowledge intake
internal/candidates/      candidate kind -> inbox dir + verdicts
internal/mem/             canonical memory object family
internal/distiller/       trace -> candidate fan-out + critique
internal/capgraph/        capability graph
internal/panel/           lens review panel
internal/localmodel/      local model advice seam
internal/runtimebinding/  RuntimeBinding + ServiceContract doctor
internal/projection/      generated-view freshness
internal/product/         product signals, hypotheses, reviews
internal/release/         product release planning + observation
internal/routeevidence/   execution route evidence
internal/schedule/        read-only launchd/cron projection
internal/reviewartifact/  shared review artifact writes
internal/writeback/       owner writeback/no-promote gate
internal/arch/            architecture doctor
```

Importer packages adapt existing systems into the spine without becoming new
owners: `authority`, `decisions`, `eventsource`, `sourceobs`, and
`tooltopology`. The remaining packages are support organs; `ls internal/`
lists the full roster.

## Rented Edge

Search engines, vector indexes, model providers, harnesses, MCPs, browser
tools, analytics APIs, and sidecars are replaceable edge adapters. They may
broaden retrieval or execute work; they do not own promotion, visibility,
supersession, policy, or truth. The ledger is authority. Engines are rebuildable
views.

## Direction

The maturity ladder this substrate is built to climb, in order:

1. Reduce its operator's cognitive overhead.
2. Orchestrate many specialized agents reliably.
3. Run parts of a real business under human oversight.
4. Serve as a research instrument that generates and tests genuinely new ideas.
5. Evolve because of the work done with it, not because it keeps being
   redesigned.

This is direction, not commitment; sequencing lives with the operator and is
decided by evidence, one receipted step at a time.

## Status

The first generation is unreleased and actively dogfooded against a private
reference system; governed execution and evidence records are inspectable
through the workbench and proof-chain paths. This repository is the generic
public substrate, not the dogfood dataset. Adaptive income orchestration and
automatic skill lifecycle are still being developed and validated; the public
surface reflects the substrate only.

Internal migration context: Gen-1 memory packages are retired from `internal/`,
and Gen-1 top-level verbs are tombstones rather than compatibility aliases.

Health gates:

- `compound architecture doctor --strict` enforces package classification,
  public-doc leak checks, candidate-map integrity, contract guards, and a
  convergence budget of `0`.
- `compound memory doctor` verifies capture, promotion, retrieval, authority,
  and optional engine sync.

Public behavior must be proven through leak-free fixtures and explicit runtime
inputs.
