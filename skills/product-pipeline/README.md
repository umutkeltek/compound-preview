# Public product lifecycle skills

A reusable, branching process for a product—not a claim of a flawless autonomous business.
These original Apache-2.0 procedures compose generic methods with verified optional
platform/product/channel skills. [Sources and license scopes](SOURCES.md) explain the
influences and what is **not** bundled.

The public core contains one skill router and fifteen conditional branch InstructionRules.
Each branch has an input, procedure, output/proof, next context, silence and recovery.
The surrounding product facts, account grants and installed specialists come from the user's
instance. No ignored overlay, provider account, private registry or upstream skill install is required.

## Use from a fresh checkout

Read [SKILL.md](SKILL.md) directly with any capable harness. For Compound offers, from the
repository root with the Go toolchain required by go.mod and a configured Compound instance:

```bash
# Read-only: resolves bundle-relative paths and emits native SkillManifest JSON.
go run ./scripts/productpipeline --root skills/product-pipeline

# Governed mutation: inspect first, then let the native importer own policy and writes.
compound capability import-skills <(go run ./scripts/productpipeline --root skills/product-pipeline)

compound capability surface --harness codex --json "Design a product lifecycle pipeline."
compound orient --harness codex --task "Design a product lifecycle pipeline."
```

Process substitution requires bash/zsh. Other shells may save the generated JSON through
their normal file mechanism and pass its path to the same importer. The bundle's explicit
`--root` may be a relocated checkout, including a path containing spaces; paths are resolved
from it, never from a personal home or this executor's directory.

To try a separate instance, set an explicit **temporary home on each runtime command**:

```bash
PRODUCT_PIPELINE_TEST_HOME="$(mktemp -d)"
env COMPOUND_HOME="$PRODUCT_PIPELINE_TEST_HOME" compound home init
env COMPOUND_HOME="$PRODUCT_PIPELINE_TEST_HOME" compound capability import-skills <(go run ./scripts/productpipeline --root skills/product-pipeline)
env COMPOUND_HOME="$PRODUCT_PIPELINE_TEST_HOME" compound capability surface --harness codex --json "Design a product lifecycle pipeline."
```

Do not run a bare initialization command for a test: it defaults to the existing runtime
home. Preserve this temporary evidence until any required review/recovery is complete.
Import deliberately: the stable node identities update
existing matching entries. Preserve and review existing adaptations/bindings first. An
import does not install a harness, configure a provider, schedule work or retire old edges.
Removing routes from a later template requires explicit governed retirement at their owner;
import is not a graph replacement operation.

`routing.template.json` is a relative-path input to the generator, not directly importable
runtime state. The generator rejects unknown fields, missing/escaped/symlinked targets,
unbundled route/relation references and native validation failures. It produces a content
revision independent of checkout location and a current observation time. Read it from a
stable checkout; it is not a sandbox for concurrently attacker-modified files.

### Routing versus execution

Codex and Claude Code are compatibility defaults, not readiness attestations. Other lanes
may be bound only after verifying their actual harness/provider authority. Offers do not
auto-load specialists or launch workers. InstructionRule destinations identify available
context, not actor addresses or acknowledged handoffs.

Token conditions are not a semantic negation parser. A fully matching negated request can
still offer the router. Semantic retrieval may offer branch context outside its token
conditions; the real consumer must recheck intent, readiness and silence. Tests of local
routing do not prove model invocation, successful task execution or business efficacy.

## Lifecycle map

Choose the current constraint; do not march through every row. Follow an edge only after
its output and the receiving owner's readiness are real.

| Branch / input event | Output/proof | Next context when ready |
| --- | --- | --- |
| [Research](branches/research.md): new customer/problem evidence | Source-backed problem brief | Strategy or experiment |
| [Strategy](branches/strategy.md): audience/promise choice | Explicit target, thesis and falsifier | Experiment, UX or spec |
| [Experiment](branches/experiment.md): hypothesis/test window | Bounded test and measurement contract | Spec; measurement after exposure |
| [UX](branches/ux.md): journey/interaction uncertainty | Selected flow with completion/recovery | Spec; research on contradicted need |
| [UI](branches/ui.md): component/screen polish | Rendered states and visual/accessibility checks | QA; UX on structural failure |
| [Visuals](branches/visuals.md): campaign/store assets | Truthful, rights-checked exported set | Release or distribution |
| [Spec](branches/spec.md): accepted behavior | Falsifiable contracts and dependency slice | Delivery |
| [Delivery](branches/delivery.md): ready slice | Source plus actual consumer proof | QA, release or measurement |
| [QA](branches/qa.md): critical/failure journey check | Reproducible defects or readiness evidence | Delivery or release |
| [Release](branches/release.md): readiness/rollback need | Exact artifact and release evidence | Distribution and measurement |
| [Distribution](branches/distribution.md): channel/audience thesis | Eligible campaign or authorized delivery evidence | Activation and measurement |
| [Activation](branches/activation.md): first-value friction | Observed barrier and testable change | Experiment or spec |
| [Retention/support](branches/retention.md): mature recurring-use evidence | Cause and bounded recovery proposal | Experiment, QA or delivery |
| [Measurement](branches/measurement.md): observed change/cohort | Keep/change/stop/inconclusive decision | Strategy, experiment or learning |
| [Learning](branches/learning.md): repeated skill/route failure | Evaluated delta with a future consumer | Router; actual authoring/check owner |

Each row is generic. Compose **generic branch → touched platform method → current product
context → selected channel method**. Security/privacy, accessibility/localization, data
integrity, billing, performance, support/incident response and cost are cross-cutting checks
at the touched seam, not unconditional work on every request. The specialization must retain
the generic outcome, proof and authority requirements. The source catalog names optional
complements; none is silently installed.

For an unreleased app, the first bounded branch is typically a real first-value/release-
readiness observation. Unknown cohorts/revenue remain unknown. Then admit one reversible
intervention, prove the consumer and observe the declared window before recurring automation.

## Common execution contract

Before admitting a branch, record through existing owners:

- Outcome/metric/denominator/baseline/window, falsifier and guardrails.
- Event/entity/revision and dedupe key; freshness, prerequisites and consumer availability.
- One concrete output and receiving owner; acknowledgment/consumption proof.
- Current work, ActionIntent, PolicyDecision and receipt owners plus exact scope/grant.
- Time/attempt/cost/concurrency/queue/cooldown bounds; next readiness event and stop thresholds.
- Recoverable checkpoint and durable operation IDs; reconciliation for ambiguous effects.

A cadence tick merely checks readiness. Duplicate/stale evidence, already-covered outcomes,
immature experiments, unavailable consumers/capacity and unresolved authority select no-op,
bounded observation or explicit defer. Retry only transient failures within bounds.
Permanent failures stop that branch and report a precise finding. Persist partial progress;
terminalize every RunAttempt. Never retry a possibly completed external effect blindly.

Creative uncertainty warrants two or three structurally different contextual candidates
and a predeclared rubric. Small settled edits do not. Agent selection is allowed only by the
actual product envelope; otherwise preserve the unresolved choice. No universal manual
checkpoint and no universal agent permission are inferred from upstream prose.

Use Compound's existing work/policy/receipts, source adjudication and product measurement
doors. Source changes in this repository follow [execute](../compound-execute/SKILL.md),
[land](../compound-land/SKILL.md) and [verify](../compound-verify/SKILL.md). Account and
communication effects also require [outbound operations](references/outbound.md).
This bundle adds no scheduler, grants, senders, deployment actuator or parallel lifecycle schema.

## Verification and updates

```bash
go test ./scripts/productpipeline -count=1
```

The tests use a relocated public bundle and isolated Compound home, native manifest
validation/import and actual capability surfacing, plus unsafe-path and route/provenance
checks. [Cases](routing.cases.json) are author-written routing diagnostics, not controlled
skill/no-skill efficacy trials. Invocation, autonomous recovery, conversion, retention,
revenue and operator-time savings need separate real-consumer trials.

Follow [skill selection/update](references/skill-selection.md): pinned revision diff,
license/scope check, one gap, baseline/held-out evaluation, governed change, consumer proof,
and retirement of superseded bindings. Do not overwrite local adaptations or copy unlicensed
source material. The [worksheet](examples/loop.example.yaml) is non-executable design evidence.
