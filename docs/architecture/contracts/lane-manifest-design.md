# Lane Manifest — design brief (launch owner × compound authority)

Status: **proposal — compound-only slice implemented (2026-08-07)**. Author: 2026-08-07
session. Consumer: compound governed adapters + the launch owner's profile generator. This
brief maps the current surface, proposes one declarative unit (the **lane manifest**), and
defines the authority split. The compound-owned half of the slice is landed: the manifest
schema (`internal/lanemanifest`), `compound hook install --lane-file` deriving harness +
roster, and the governed-spawn full-roster gate. The launch-owner half (manifest as launch
authority, `sb profile plan/apply` consuming it) remains a starting hypothesis to prove only
via a future vertical slice.

**§9-§14 (naming and combination extension)** are proposal-only and land nothing. They settle
what sections 1-8 left open: what a lane is *called*, how a harness is registered without a
code change, how valid `(harness, lane)` combinations are computed rather than remembered, and
where account/entitlement truth lives. Read §14 for the recommended slice order.

## 6b. Landed compound slice (2026-08-07)

The compound-only half of §6, implemented end to end:

1. **`internal/lanemanifest`** — the declarative unit. `Parse` (YAML/JSON),
   `Validate` (harness family, roster, schema version), `InstallHarness()`
   (lane → install harness via the lanes.json `hook_harness` convention).
   Reference manifest: `fixtures/lanemanifest/minimax.lane.yaml`.
2. **Roster selection** — `hookplan.RosterEventSet`: `minimal` = {SessionStart,
   PreToolUse}; `full` = {SessionStart, UserPromptSubmit, PreToolUse,
   PostToolUse, SessionEnd}. Bindings are narrowed to the roster; semantic
   posture validation reads the full posture. `InstallHooksForLane` + a
   roster-aware `InstallHooksWithRoster`; the pre-roster signature is unchanged
   (default full).
3. **`compound hook install --lane-file <manifest> [--roster minimal|full]
   [--check]`** — derives the install harness and roster from the manifest;
   `--roster` overrides. The default flips: `minimal` needs zero ceremony, `full`
   is the governed selection.
4. **Governed spawn gate** — `work advance --execute` refuses a hook-governed
   lane whose observed settings do not cover the full roster (`gateRefusal`,
   fail-closed, read-only; never installs on spawn). Lanes without an
   observable profile are governed by tuple preflight + permission mode instead.
5. **Two-writers regression** — the NO-CLOBBER reconciler already preserves
   every non-compound settings key; tests now pin that contract for the
   launch-owner-regen → drift → re-apply loop.

The launch-owner half (manifest as the launch/capture/route authority) is unchanged
and still requires the cross-repo slice.

## 1. Problem

Setting up a harness lane today is a many-piece ceremony:

- launch owner: launch profile (`sb profile apply` → wrapper, `settings.json`,
  `harness_settings`, capture policy, tap/Headroom bindings).
- compound: per-harness hook roster (`compound hook install --harness …`), isolated
  `--config-dir` profiles per lane, `install-lane.sh` glue mapping lane → hook harness,
  plus hand-written settings blocks that the launch owner's conformance regen can revert
  (`find_bc58676341a9`).

Consequences: every new harness × lane combination re-derives the ceremony; hook fixtures
(PreToolUse/PostToolUse/FailedFirst/session-end/stop) are prerequisites instead of
refinements; the same provider lane can be reached as an API endpoint, a tap, or a harness —
but nothing expresses "this lane is the same thing as that endpoint" in one place.

## 2. Current state map

| Concern | Owner today | Artifacts |
|---|---|---|
| Provider lane (model, upstream) | launch owner provider registry | `provider-registry.json`, routes |
| Launch profile (harness × lane × capture) | launch owner | wrapper, `settings.json`, capture policy, tap/Headroom |
| Harness authority hooks (governance) | compound | `hook install` rosters per harness type |
| Lane identity / executor roster | compound | `compound executors`, lease/attempt state |
| Lane-as-API | launch-owner gateway | OpenAI-compatible endpoints, taps (`:1877x`) |

Two authorities, one seam: compound **consumes** the launch owner's published projections
(profiles, account projections, lane conformance); the launch owner never reads compound
state. The seam today is implicit (paths, env, wrapper ids) — no schema names it.

## 3. Target model: one manifest, two projections

**Lane manifest** (tracked source in the launch owner's repo as the authority; compound
consumes it via `--lane-file`):

```yaml
lane: minimax
harness: claude-code          # claude-code | codex | opencode | api
model: minimax/MiniMax-M3
effort: xhigh
capture: segmented_full_wire  # metadata_only | segmented_full_wire | none
permission: bypass            # plan | acceptEdits | bypass | default (harness defaults)
api:
  enabled: true               # expose as OpenAI-compatible endpoint
  route: minimax/*            # gateway route pattern
hooks:
  roster: minimal             # minimal | full (default minimal)
```

Two projections are generated from one manifest, by their owner:

- **launch owner** owns: wrapper + `harness_settings` + capture policy + tap/Headroom
  binding + gateway routes. The `api:` block maps directly onto existing route targets —
  "lane as API" stops being a separate config.
- **compound** owns: adapter wiring (exact `model:` on every Agent/Workflow spawn),
  hook roster, config-dir profile. The `hooks.roster` field selects the roster class:

| Roster | Hooks | When |
|---|---|---|
| `minimal` | session-start (authority attestation), pre-tool-use (landing/deny gates) | default — a lane works with zero hook ceremony |
| `full` | + post-tool-use, session-end, stop, user-prompt-submit (governance/telemetry) | opt-in refinement for governed dispatch |

The default flips: **hooks are refinements, not prerequisites.** `full` is selected
automatically when a governed adapter spawns a lane (`compound work advance --execute`),
so governance is preserved where it matters without burdening every interactive lane.

## 4. Combination semantics

One manifest expresses all three consumption modes:

- **API**: `api.enabled: true` → endpoint + route (existing gateway behavior, now derived).
- **Harness**: `harness: claude-code` → wrapper + isolated profile + hooks.
- **Both**: the normal case — the same lane is addressable as `http://127.0.0.1:<tap>` AND
  as `claude-minimax`. The manifest is the single place that names "this lane is the same
  thing as that endpoint".

Authority split (unchanged doctrine, now explicit):
- launch owner: meaning-neutral launch/capture/route truth (it owns the wire).
- compound: governance truth (it owns what harnesses may do, and the receipts).
- compound never mutates launch-owner state; the launch owner never reads compound state.

## 5. Generation rules

1. `sb profile plan --lane <manifest>` validates the manifest against provider registry +
   account projections; `sb profile apply` emits the artifacts.
2. `compound hook install --lane-file <manifest>` derives harness + roster from the
   manifest; `--config-dir` gating stays (isolated profiles per lane).
3. A manifest change is a **binding**: re-apply + first real launch after apply is the proof
   (launch args are a binding — existing doctrine, unchanged).
4. Retirement: the manifest replaces hand-written `install-lane.sh` glue and the
   implicit path coupling; retire those once the slice proves the consumer path.

## 6. Slice proposal (when approved)

Vertical slice: **claude-minimax as both harness and API from one manifest.**

1. Manifest tracked in the launch owner's repo; `sb profile plan`/`apply` consumes it (or a
   `--lane-file` passthrough — decide in slice).
2. Compound `hook install --lane-file` derives the minimax roster (claude-minimax hook
   harness per the lanes.json mapping) — `minimal` default, `full` on governed spawn.
3. Prove: `claude-minimax --version` works from a fresh profile; a governed
   `compound work advance --execute` on the minimax lane passes readiness with `full`
   roster; the same lane serves an API call through its tap route.
4. Tests probe depth: manifest drift (provider model removed from registry), roster
   mismatch (lane says minimal but governed spawn requires full), and the two-writers
   regression (launch-owner regen must not clobber compound's applied settings).

## 7. Open questions

1. Manifest source of truth location: the launch owner's repo (owner of launch) vs compound
   (owner of governance) — recommendation: the launch owner's repo, compound consumes with
   authority_ref + source_revision, mirroring existing projection consumption.
2. `permission` in the manifest vs compound-side policy only — recommendation: manifest
   declares default; compound policy can override per work class (governed spawns already
   derive permission from policy, not from the lane).
3. Does `api.enabled` imply the tap routes live with the harness (one manifest) or stay
   independent gateway config? Recommendation: derived from the manifest, one file.

## 8. Retirement conditions

- `adapters/_lib/install-lane.sh` hand-mapping retired once `hook install --lane-file`
  covers all lanes (minimax, zai, zai-direct, qwen, claude-codex, opencode).
- Implicit path coupling (wrappers pointing at repo-relative scripts) retired when the
  manifest is the only thing that names a lane's artifacts.
- Hand-written launch wrappers retired once §11's generator emits them from tuples. In one
  observed deployment 52 wrappers existed against 11 provider lanes, of which 6 carried
  launch-profile ownership and 46 were hand-written — plus 4 stale backup files sharing the
  same namespace. Deletion, not migration, is the retirement.

---

# Naming and combination extension (§9-§14)

Sections 1-8 settle *what a lane declares*. They do not settle *what a lane is called*, and
that turned out to be the load-bearing question: the manifest's `lane:` field is still a flat
name, so it inherits the fragility it was meant to remove.

## 9. Identity: naming is owned by `docs/NAMING.md`

**This section does not define a naming scheme.** `docs/NAMING.md` already owns it, is
enforced by `harness.ValidateHarnessName` plus tests, and answers every question this design
raised: the four distinct nouns (account, lane, harness, profile), the identifier grammar
(`lane <vendor>/<model>[@<variant>]`, `profile <harness>.<lane>[.<account>]`), the rule that
`profile` is always the composite and never shorthand for a part, and the bans on encoding
effort, cost, or comparative suffixes into a name.

A second copy of a naming rule is a copy that drifts, so what follows is only the delta: the
violations this design must repair, and the one rule the owner does not yet state.

### 9.1 Live violations of the owner's rules

The manifest's flat `lane:` field lets all three of these persist:

1. **One lane, one name per surface.** A single provider lane was observed carrying six
   simultaneous names across lane records, per-harness launchers, a governance registry, a
   route target, and a third harness's own config. `NAMING.md` §Locations already forbids
   this — "ask the owner; never keep a second copy" — but nothing derives the names, so each
   surface keeps its own.
2. **A name meaning two things.** The lane-to-harness map contains `"codex" -> "claude-zai"`
   with a comment explaining that the lane named `codex` is not the codex harness. That is
   the exact defect class `NAMING.md` opens with.
3. **A banned comparative suffix.** A `-full` profile persists, which rule 4 forbids by name
   — and for the documented reason: the suffix once described an intended subscription tier
   while capability came from somewhere else.

### 9.2 The additive rule: launcher names are generated, not authored

`NAMING.md` governs identifiers. It does not say who *writes* a launcher file's name, and
that gap is how the violations above entered — every launcher name was typed by hand.

> A launcher filename is derived from its profile id. It is generated output, never an
> authored name. Nothing may key on the string.

This is what makes the owner's rules enforceable rather than aspirational: a generated name
cannot carry an effort, a cost hint, or a comparative suffix, because no human is typing it.
It also makes renaming safe, which `NAMING.md` §Applying this otherwise treats as a
migration with an alias step.

## 10. Harness as registry data, not a closed enum

The loader for this already exists: `compound.harness_registry.v0.1`
(`harness.LoadRegistryManifest`, reference fixture `fixtures/harness/harness-registry.yaml`)
reads harnesses as data — name, instruction mechanism, capabilities, limitations.

**What is missing is authority.** `DefaultRegistry()` remains a hardcoded Go map, and the
lane-manifest family check remains a closed switch over three names. Registering a new
harness therefore still requires editing Go in several places, which is precisely the barrier
that keeps an outside contributor from adding their own.

The change is to make the manifest the registry's source and leave the Go map as the seed
default for a deployment that ships no manifest. Then:

- Adding a harness is **one YAML row**: name, instruction mechanism, capability flags,
  limitations, and the wire protocols it speaks (§11).
- The harness family check reads the registry instead of a switch.
- `compound harness capability-drift` stays the falsifier. It already works and already earns
  its keep: it caught two registry floors declaring less than the lanes actually had, one of
  them on reasoning that had been true when written and silently stopped being true. A
  declaration is only meaningful when something outside it can disagree.

Capability semantics are `docs/NAMING.md` §Capability's, unchanged and not restated here:
declared capability is a floor, over-claiming is unsafe while under-claiming is merely
wasteful, and a declaration must be falsifiable by something outside itself.

## 11. The combination matrix

A harness and a lane compose when their wire protocols agree. **That compatibility fact is
already declared per lane** — each lane record carries the wire API it speaks and whether it
publishes a native Anthropic tap. So the matrix is computable from data that exists today; it
does not need a new registry, only a join.

| lane declares | harness needing a native Anthropic tap | OpenAI-compatible harness |
|---|---|---|
| wire `anthropic_messages` + tap port | native tap | via gateway route |
| wire `chat` + tap port | tap shim | direct route |
| wire `chat`, no tap | **incompatible** | direct route |

This yields three states per `(harness, lane)` cell, and the useful one is the middle:

- **realized** — a launcher exists,
- **possible** — compatible, nothing built yet,
- **incompatible** — the lane cannot serve that harness, with the declared reason.

Surface: `compound lanes matrix [--harness H] [--lane L]`, read-only, no mutation. The
**possible** cells are the answer to "which lanes could this harness use" without anyone
enumerating wrappers from memory.

Generation follows from the same table: a launcher is emitted per realized tuple by the launch
owner's existing wrapper generator, rather than hand-written. This is the retirement path in
§8 — and it makes governance posture structural, since a generated wrapper carries the profile
id, revision, and capture binding by construction. Hand-written launchers were observed to
carry none of these, which is not a lane defect but a *naming* defect: the wrapper was an
authored artifact rather than a derived one.

Evidence that the one-config-many-lanes shape works: a third harness already reaches nine
lanes through a single config file listing route ids against one gateway endpoint, with zero
per-lane wrappers. The wrapper-per-lane explosion is a property of the two harnesses whose
launchers are authored by hand, not a property of lanes.

## 12. The account and entitlement axis

The manifest names a model but never says **which account reaches it**, so "what can I
actually run right now" is unanswerable from the manifest alone. Two stores already hold the
halves, and nothing joins them:

- a **model catalog** — providers, models, pricing, free tiers, refreshable from upstream
  catalogs with drift detection. This is *what exists in the world*.
- an **account store** — per-provider enrollments with state, credential readiness, capacity
  windows, and the discovery sources that observed them. This is *what this operator reaches*.

The join is the runnable set: `catalog x enrollments -> lanes this deployment can actually
run, with cost and remaining capacity`. The manifest gains two optional fields:

```yaml
account: <account-id>      # FK into the account store; omitted = provider default
source:  <source-id>       # which subscription/API serves this lane
```

Both optional, so a single-account deployment writes neither. This also resolves a name
collision worth fixing while the schema is open: in the model catalog, `sources` currently
means *research sources for pricing data*, not *the operator's subscriptions*. Two unrelated
meanings on one word, in the schema that is about to gain the second one.

Once joined, entitlement becomes checkable rather than discovered at runtime: a lane whose
account has no credential, or whose capacity window is exhausted, is knowably unrunnable
before a spawn rather than after a failure.

## 13. Open-source posture

This design is for any deployment, not the one it was observed in. Constraints that follow:

1. **No operator-specific lanes, accounts, paths, or credentials in tracked files.** Provider
   and model names are public facts; account ids and host paths are not. Reference manifests
   ship as fixtures with placeholder accounts.
2. **The registry ships empty-capable.** A fresh clone with no accounts must run: the matrix
   is empty, the doctor says so plainly, and nothing fails closed on absence.
3. **Adding a provider, an account, or a harness is data, never a code change.** This is the
   test of whether §10 and §12 actually landed. If a contributor adding a harness has to edit
   Go, the extension failed.
4. **The two authorities stay separate and one-directional.** The launch owner owns
   launch/capture/route truth; compound owns governance truth and consumes published
   projections with `authority_ref` + `source_revision`. Compound never mutates launch state;
   the launch owner never reads compound state. Nothing in §9-§12 changes this seam.

## 14. Slice order (recommendation)

Smallest first, each independently useful:

1. **§10 authority flip** — manifest owns the harness registry; Go map becomes the seed.
   Proof: register a harness with no Go change and see it in `harness list` and
   `capability-drift`.
2. **§11 matrix (read-only)** — `compound lanes matrix`. Proof: it reports the *possible*
   cells for a harness that currently has no launchers, and the incompatible ones with a
   declared reason.
3. **§11 generator** — emit launchers per realized tuple; delete the hand-written ones.
   Proof: a regenerated launcher is byte-identical to an owned one, and profile doctor is
   clean across every lane.
4. **§12 account join** — optional manifest fields plus the runnable-set query.

Steps 1-2 are additive and reversible. Step 3 deletes files and must be staged behind a
backup plus a clean profile-doctor sweep; it is the only step in this extension that removes
anything an operator might be launching by hand.
