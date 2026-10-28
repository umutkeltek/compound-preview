# The Execution Plane (model-traffic gateway)

The optional [shared Bitwarden adapter](../../adapters/bitwarden-session/README.md)
is an owned credential-session bridge at the rented edge. Its macOS source,
installer and real-terminal regression tests live in this repository; installed
binaries and build provenance live in the instance's existing `actuator/` habitat.
Bitwarden owns the encrypted vault and Keychain holds its shared unlock session.
All harnesses discover the same installed entry point through the existing
tool-topology graph. This is not a new permission owner: task authority still
comes from the envelope spine. Updates serialize with active vault operations,
preserve credentials and replace only a successfully built binary. Account
identities and operational access evidence remain private instance data.

compound is the **control / meaning plane**. Model/provider traffic belongs to a separate
**execution / data plane** — an AI provider gateway that receives OpenAI/Anthropic-compatible
requests, routes them by policy/health/cost/latency/quota/credentials, returns the response in the
caller's wire format, and emits an inspectable route decision. They are **siblings**, not nested.

> compound stays vendor-neutral about the gateway (rent, don't hardcode). The operator's reference
> implementation and its specific lanes/ports live in the private overlay, not here.

## The split (the same doctrine, a different medium)

```text
compound  doctrine:  own the meaning, rent the index.
gateway   doctrine:  own the route DECISION, rent the provider/account/model.

compound asks:  What is this job? What matters? What memory/skill/product context applies?
                What future behavior should change?   (route POLICY)
gateway asks:   Given this request shape and policy, which provider/model/account carries it?
                What did it cost? What failed? What was rejected?   (route DECISION + enforcement)
```

If compound absorbs provider routing it becomes bloated and fragile. If the gateway absorbs
memory/product/skill/taste it becomes a generic router pretending to understand the work. Both wrong.

The interactive harness boundary follows the same split:

- **Harness posture** is Compound-owned meaning: permissions, hooks, task intent, work identity, and policy.
- **Inference transport** is gateway-owned execution: proxy/tap/native path, provider/account routing, capture,
  retries, and route evidence.
- A **harness-transport adapter** is the thin composition edge. It resolves posture from Compound, passes only
  explicit launch inputs to the transport, and can be deleted without moving either authority. The gateway must
  not read Compound policy files, and Compound must not proxy model traffic.

### Native OpenCode prompt-only adapter

The `opencode` adapter is a native, prompt-only consumer of the existing model-router
authority. A selected entry must have a literal `provider/model` ID, matching
`provider_route`, executable alias, and clean HTTPS `provider_endpoint`. The
optional endpoint field round-trips through imports, merge, projections, semantic
diffs, and authority revisions; lanes that do not use it remain unchanged.
Sensitive endpoint components are redacted from projections. Native catalog output
is evidence for admitting a tuple, never route authority.

A saved execution selection may omit effort or request exactly `low`. The
adapter freezes the requested/applied effort, evidence and snapshot presence
with the launch binding, and emits `low` through the isolated model options.
Omitted effort preserves the existing request. Unsupported effort and selection
drift refuse before launch; caller argument/config/environment overrides remain
forbidden. RunAttempt effort evidence attests immutable configuration, not
provider-observed identity or a guarantee about reasoning behavior. The 800-token
and 90-second bounds remain unchanged.

Preflight freezes the authority revision, executable path and digest, tuple,
requested workspace, and prompt. The launch guard reloads authority and hashes the
executable immediately before starting. No caller args, environment, config,
permission override, or reused session reach the native child. The Claude-family
adapters add one adapter-owned exception in kind, not in authority: they allocate a
fresh UUIDv4 native session id per `Run`, pin it on the launch argv as
`--session-id`, and record it in the COPIED per-run selection snapshot as
`pinned:--session-id` evidence before the shared runner persists that snapshot. The
caller can neither supply nor reuse a session id, so two concurrent launches never
share a session and a successor never inherits its predecessor's; the work owner's
recipient resolution is the only consumer that may bind that id to a recipient. The
prompt travels
on stdin and is limited to 64 KiB. The native process runs in a new scratch cwd,
with isolated HOME, native search home, and XDG config/data/state/cache directories.
RunAttempt records the actual sandbox executable and scratch cwd, with separate
native-executable and requested-workspace digests. It does not claim execution in
the product repository or observed provider-request identity.

Only the selected saved native API entry is read after RunAttempt mint. Its source
must be a bounded regular leaf owned by the current UID with exact mode 0600 and a
nonempty single-line key. The isolated auth snapshot contains only that entry;
the key never enters argv, environment, config, or receipts. The shared runner
replaces exact selected-key literals before durable transcript writes. Encoded or
transformed credentials are outside that literal-redaction guarantee. Cleanup
must remove the isolated credential/search root before successful terminalization;
cleanup errors fail the attempt with a content-free code on every exit path.

Config JSON and native `.gitignore` seeds are mode 0400, config/search directories
are 0500, and the macOS sandbox denies `file-write*` over both complete subtrees.
Other writes stay inside scratch runtime storage. Tools, plugins, MCP, external
skills, catalog fetch, updates, sharing, and auto-compaction are disabled. Existing
late host-managed native config causes refusal because it cannot be isolated by
this adapter. The sandbox permits outbound IP port 443 for provider transport and
the exact local `/private/var/run/mDNSResponder` socket for macOS hostname lookup.
Other outbound connections, including unrelated Unix sockets, remain denied; the
resolver exception does not open direct DNS network ports. The regression probes
local socket connections without making a DNS or provider request: loopback TLS
controls alone do not exercise the system resolver path. Port restriction does not
enforce a hostname or prove TLS. Immutable validated `baseURL` config selects the
endpoint. This adapter requires macOS sandbox-exec.

The shared runner owns heartbeats, governed start, process-group cancellation,
partial capture, and terminalization. The adapter caps total runtime at 90 seconds,
native output at 800 tokens per request, and durable transcript at 64 KiB plus a
content-free truncation marker. Truncation, scanner failure, provider JSON errors,
tool events, missing text, or non-normal finish fail the attempt even after child
exit zero. Transport completion remains separate from task-quality acceptance.

Installed OpenCode 1.18.34 synthetic TLS controls proved one request on success and
nonretryable 401, and six requests on retryable 500/429: zero Compound retries,
SDK `maxRetries: 0`, and up to five native session retries. The native processor
unconditionally installs that session retry policy; no supported disable option
was found. Retryable runs remain under the same 90-second total deadline. Real
request count is unobservable unless native evidence supplies it; these fixture
counts do not attest arbitrary executables. Admission must review the attested
binary digest and synthetic controls. The opt-in installed-native test uses only
a temporary synthetic credential and local TLS provider. No private tuple or
provider credential is shipped in source.

### Public browser task adapter

The standalone Jev goal refuses any WorkEnvelope with a saved execution selection until a browser-compatible launch profile is defined; it cannot silently override another harness tuple.

Jev execution now enters the governed Work rail. Create a ready WorkEnvelope with `--canonical-surface` set to the exact initial public HTTPS URL (including path), `--outcome` set to the browser goal, and `--predicate` set to the expected readback text; claim it with the actor that will run the task. For example: `compound work create --work-id work_example_browser --owner browser-lane --by browser-lane --canonical-surface https://example.com/ --outcome "Read the heading" --predicate "Example Domain"`, then `compound work claim work_example_browser --by browser-lane`, then `compound browser goal --work work_example_browser --actor browser-lane --url https://example.com/ --goal "Read the heading" --expect-text "Example Domain" --provider-budget-usd 0.10`. Missing, unclaimed, terminal, differently leased, or conflicting work is refused before provider access. The exact initial URL is the saved scope; subsequent browser steps remain restricted to its public origin.

The browser adapter uses the same claimed-to-running reservation, guarded subprocess start, RunAttempt, and exact-generation finalization as governed harness execution. The request digest binds work, actor, URL, goal, predicate, non-goals, model, and budget to the reservation and bridge input. Saved non-goals reach Jev's agent prompt. A prediction request/ack before every provider call reloads route, policy, pricing, credentials, and the current Work generation; revocation denies that call. Browser readback sets the task result's `verified` field, while Work advances only to `verifying` for separate acceptance. Cancellation keeps partial browser receipts and a terminal failed attempt on the same work. The public CLI uses the execution signal context so SIGTERM cancels the owned Jev process group and allows Work finalization. Cleanup errors clear `verified` and fail the attempt. Process reaping reports unavailable inspection, signal errors, and surviving owned processes rather than treating them as successful cleanup.
If a verified group leader becomes an unreaped zombie after termination, cleanup
requires a fresh matching live descendant before force-stopping its unchanged
group; a zombie's lost command text does not prove either foreign ownership or
successful cleanup.

Proof-fixture descriptor closure is synchronized with Go's fork lock to exclude
unrelated subprocess inheritance before close-on-exec. Production termination
attestation remains fail-closed while any inherited barrier descriptor is live;
fixture synchronization never substitutes for owned descendant termination.

`compound browser settings` is the Compound-owned, policy-gated Jev switch;
missing settings route to the established Playwright backend. `compound browser route`
exposes the selected backend to every harness. `compound browser goal`
uses pinned `jev-ultrafast` only when the switch is on and no profile is
requested. Its bridge starts an ephemeral, isolated headless Chrome rather
than allowing Browser Harness to attach to an existing logged-in browser.
Jev cannot attach to Compound's enrolled `user_data_dir`, so
profile-bound and authenticated work remains on Playwright. A `goal` request
that selects Playwright exits non-success with an explicit handoff; it does
not pretend to have executed that task. The first Jev slice accepts only a
public HTTPS origin and terminal decisions, scroll, or wait; all clicks, form
edits, and other model-proposed actions are held before `act`. Each proposal has an ActionIntent and PolicyDecision, and
each outcome has a Receipt/Transition pair in the event store. A `DONE`
proposal is not success: the bridge observes the page again and must match the caller's explicit expected
text. These receipts are `audit_only` because a public read does not create a
durable outward state delta. The rented Python bridge and provider keys own no
Compound meaning or credential body.

Provider prediction precedes each browser-action proposal, so Jev launch requires an operator-supplied per-request TypeSafe cost ceiling and an explicit per-task USD budget. The launch ActionIntent carries the worst-case estimate for three HTTP attempts per step. This is an admission bound, not observed billing or a provider-enforced hard spending cap; without a trustworthy provider/account ceiling, keep Jev disabled. The CONNECT proxy limits Chrome to one public origin, but TLS is opaque to it: page scripts can still send same-origin POSTs, so this slice is unsuitable for pages where merely loading the page can mutate external state.

The launch is classified `financial` and stays held without a policy standing grant for `browser.goal.jev` whose `max_usd` covers that estimate. A budget flag alone never authorizes provider contact.

The TypeSafe value is Compound-custodied but never a registry value: the capability graph stores only the `tool:browser-jev-typesafe` handle to `home-secret://typesafe/jev-api-key`. `compound browser credential-set --work W --confirm` takes a bounded key on stdin through a `security`-class PolicyDecision and writes a mode-0600 secret slot with a content-free receipt. The Jev caller pins the exact handle, custodian, and environment name. A registered or malformed registry fails closed; a caller `TYPESAFE_API_KEY` is a compatibility fallback only before any connection-surface registry exists.
`compound browser credential-rotate --work W --confirm` replaces only that exact registered slot through the same security-class gate. Neither enrollment nor rotation accepts a credential in arguments or receipts.
The exact credential-slot lock spans read, replacement, and proof publication. Before changing the live slot, Compound durably stages secret bytes under private `secrets/` and writes a content-free recovery journal under backed-up `registries/`. Retry first syncs that journal, then reconciles the staged value and original policy decision before publishing the original Receipt-to-Transition link; a post-rename sync error remains uncertain until retry syncs the slot. If a restore has the journal but neither staged bytes nor a live slot, recovery records the original attempt as failed and permits fresh governed enrollment instead of inventing success.

`compound browser provider-pricing` pins `jev-1.13.0` and its per-request ceiling in governed browser settings; harness environment cannot override the registered credential or priced model. TypeSafe's published Jev 1.13 price is $0.042 per million input tokens with a 64k-token maximum request, or at most $0.002688 per request; the configured $0.003 ceiling leaves margin. This price admission still requires a separate per-task budget and bounded `browser.goal.jev` financial standing grant. See [TypeSafe models](https://docs.typesafe.ai/models) for the external pricing evidence, which must be revisited if the pinned model or terms change.

## Door map: one cached snapshot served at session start

Every harness used to rediscover its doors (which tools work, which MCP servers exist, which harness
version is installed, which model each launcher routes to). `compound doors refresh` now observes that
once into `COMPOUND_HOME/observed/door-map/snapshot.json` (previous snapshot kept as `previous.json`), and
the live `session-start` hook appends a block of at most 3000 bytes to the harness payload. Door recipes are
served whole when they fit; harness versions stay in `compound doors show`. The oh-my-pi shim keeps the
session-start context and prepends it to the first agent message. Static clients without that hook get a
one-line pointer to `compound doors serve` in their compiled instruction block.

- Observe, never execute: inputs are a door-doctor JSON report (`--doctor-json` or
  `COMPOUND_DOOR_DOCTOR_JSON`), MCP config files (server names only, never env/args/URLs), harness binaries
  resolved on PATH (version inferred from the install path or package metadata, never by running them), and
  the launch owner's lane projection (`COMPOUND_LANE_PROJECTION_DIR`).
- Freshness is explicit: the block states when doors were checked. Past a 24h TTL it withholds all door
  statuses and says how to refresh. A missing or unreadable snapshot is announced, not omitted.
- `changed_since_last` is the diff against the previous snapshot; `recent_changes` rolls it forward 72h.
- The snapshot is evidence, not truth. Fixture replays of session-start do not include it.

## The execution handshake

A harness is not a model; a provider is not a transport; native inference effort is not orchestration; a route
alias is not an observed model identity. Compound stores these axes independently and never derives one from
another.

`ExecutionProfile` is the Compound-owned semantic request. It carries role/capability class, harness requirements,
privacy/risk, semantic effort, optional native-effort constraint, orchestration policy, candidate bounds, tools,
permissions, and request/token/time/cost ceilings. It is a plan, not evidence that a call occurred. The read-only
`compound route execution-profile` resolver proves profile coherence without contacting a provider or changing
gateway/harness configuration.

Experimental `compound work delegate` can select an existing profile with `--execution-profile`, but its mutation
surface is disabled by default and unsupported in v0.1; `work advance --execute` is the supported governed path. Optional
role/capability/harness/transport/route/model/effort/orchestration/privacy selectors narrow that profile and are
refused when no profile is selected. `--launch-profile` selects the executor owner's concrete launch/account-policy
profile and is likewise invalid without an execution profile. The existing `--harness` flag still selects the exact
Compound model-router `LaneTuple`. The route-evidence owner resolves one bounded canonical execution-plan snapshot
and attests compatibility against the selected harness, lane, model ID, provider route, and transport evidence;
`internal/work` does not maintain harness-family, model, route-alias, or executor-transport tables.

`LaneTuple` resolution deliberately leaves transport empty because the lane projection declares it executor-owned.
When an explicit launch profile is present, route-evidence may defer only the requested executor-owned gateway
transport while the remaining tuple is exact. The CLI then asks the executor owner for the full read-only profile
plan. Compound accepts only the owner's exact `launch-profile-plan@1` consumer projection, with current
generated artifacts, `segmented_full_wire` capture, an opaque nonempty client profile, and exact
harness-family/provider-lane/effort compatibility. A registered lane such as a Claude-family token lane remains
distinct from the owner's `claude-code` client family: the route-evidence registry owns that family mapping.
Likewise, the selected tuple's plan identity and provider route class remain distinct from the owner's concrete
model and route. `provider_route_kind` declares the one owner axis the selected provider route must match:
`provider_class` compares exactly with the owner-attested `provider_lane`, while `direct_model` compares exactly
with the owner-attested concrete `route`. Compound never infers the kind from `/`, the requested model, or an
owner alias. Both values compare case-sensitively after trimming surrounding whitespace. The owner's concrete model
and route are sealed from the plan rather than inferred from or equated to tuple aliases. The owner's exact
`gateway|tap|headroom` transport is sealed beside the selected gateway transport family; it is never inferred from
a route name.

Legacy model-router manifests may omit `provider_route_kind` and continue to import and serve ordinary delegation
without an owner launch profile. Selecting `--launch-profile` requires one of the two explicit values; a missing or
unknown kind refuses before profile resolution, readiness, capacity, RunAttempt mint, or process creation. The
model-router tuple is resolved again immediately before mint, and the selected route plus kind are persisted
independently on the spawned binding so recovery cannot accept a coherently resealed launch plan on another axis.
New launch bindings pair `compound.work_delegate_event.v0.2` with
`compound.delegate_launch_profile_plan.v0.2`. Recovery still reads paired v0.1 records created before the route-kind
field existed, without inventing independent route facts; mixed-version or downgraded current records fail closed.
Missing client profiles, drifted materializations, non-capturing or unknown transports, or changed authority
revisions fail at the same boundary.

Admission validates both snapshots' size, JSON, schema, required identity, and exact attestations. Typed
execution-profile and launch-profile readiness proofs carry those snapshots plus references and digests; both are
hashed into the canonical readiness `snapshot_revision` and persisted on the delegate binding. The exact
RunAttempt stores the execution plan ID and requested client-harness/transport/native-effort fields plus the launch
profile ID, owner revision, and opaque client-profile ID before spawn—not either full plan and never an account ID
or credential. The supervised tmux runner installs `work_id`, `ratt_*`, execution-plan identity, launch-profile
identity, and opaque client-profile identity as session-environment argv before respawn, without shell
interpolation. Observed model/provider fields remain empty until route evidence attests them. Gateway
account/credential selection remains gateway-owned and is not stored here. The lane-selected executable must match one
healthy owner-published wrapper artifact. Its path is used only for the spawn and RunAttempt command, never sealed into
the public-safe profile snapshot; that snapshot is persisted on the binding so status, steer, attach, sweep, and retry
recovery revalidate the same identity. Delegation without
`--launch-profile` retains its legacy behavior, so regular native Claude Code and existing tuple-backed lanes remain
available while their authority is migrated to publish an explicit route kind.

Profile compatibility binds the selected canonical tuple rather than trusting selectors in isolation. After
trimming surrounding whitespace, opaque model IDs, provider routes/route aliases, and transport IDs compare
exactly and case-sensitively; hyphens and underscores are not interchangeable. A nonempty model constraint must
equal the selected model ID; a nonempty route alias must equal the selected provider route or selected route
identity; and transport is compatible only through the explicit owner-plan handoff above.

Harness and transport are independent selectors: for example, Claude Code may run over native transport or an
observed gateway transport without becoming a different harness. Selecting a transport does not let Compound
claim the provider/model that actually ran; those remain `unknown_until_receipt`.

The gateway publishes a revisioned, non-secret conformance view: protocols, route aliases, provider/model
resolution rules, supported native effort per adapter/protocol, cost/account classes, and catalog/adapter revision.
Compound admits a profile only against a specific conformance revision. The gateway then returns observed route
evidence bound to that request and revision:

```text
Compound: requested profile + PolicyDecision + reserved ceilings
  → gateway: admitted route envelope + conformance revision
  → harness/adapter: requested native controls
  → gateway: resolved provider/model/account class + effective controls + attempts/fallbacks
  → Compound: ExecutionRouteEvidence + Receipt cost/outcome reconciliation
```

Requested and effective values are both durable. A gateway may translate a semantic effort into a different
harness-native value only when the profile allows that translation; the evidence records requested value,
effective value, disposition, and reason. Unsupported explicit combinations fail before provider contact. Alias or
adapter changes create a new revision and never rewrite historical plans.

Children receive new profiles. They inherit ceilings—privacy, targets, tools, mutation classes, and remaining
budget—but not provider, model, transport, account, native effort, or orchestration. Independent review is checked
against observed provider/model identity, not different selector labels.

Failure identity is part of the interface. Do not flatten these into `no eligible target` or an untyped 400:

| Failure class | Meaning / next branch |
|---|---|
| `unsupported_combination` | selected harness/transport/model/effort cannot faithfully apply the request; choose an allowed translation or another profile |
| `no_capable_target` | no candidate satisfies declared capabilities; change constraints or capability evidence |
| `no_account_or_quota` | a capable route exists but no admitted account/capacity is available; wait, reserve, or select another economics class |
| `circuit_open` | route is temporarily unhealthy; obey bounded retry/fallback policy |
| `provider_rejected` | provider received and rejected the request; preserve provider status/reason without relabeling it as capability absence |
| `transport_failed` | codec, relay, network, or persistence path failed before a trustworthy provider result |
| `unknown_effective_parameters` | execution may have happened but the adapter cannot attest what actually ran; evidence remains non-deciding |

The rollout order is conformance before enforcement: first prove the gateway's typed lane and requested/effective
receipt; then let Compound profiles authorize one shadow consumer; then enforce one bounded real consumer. A good
resolver or gateway doctor without that consumer is bench-only.

## OMP prompt-only proof and consumption

The owner projection's requested model alias and canonical request model are distinct:
argv, native requests, and custody bind the canonical `request_model`, never the alias.
Ingress normalizes the five owner binding hashes from `sha256:<hex>` to bare digests;
legacy bare digests remain accepted, while malformed hashes still refuse. Conformance
revision, content hashes, file-byte checks, and producer lineage are unchanged.

The dedicated `omp-native-verifier` and `omp-prompt-only` adapters do not change
ordinary `oh_my_pi` execution. The producer exercises the owner-pinned native
prompt-only contract with a hostile tool response; `--print --no-tools` alone
is not that contract. Its successful governed RunAttempt owns a strict public
proof plus the recipe/report and five private capture/binding bodies.
Failed post-exit validation or publication retains the exact private capture
under a failure-only digest; it never attaches certifying public proof.

Consumer admission reads existing custody without creating or repairing paths.
It binds the selected proof to its own succeeded, committed producer attempt,
that producer's work/action/policy, and the exact current owner projection
frozen in the consumer's opaque preflight handoff. A different producer work is
valid; a proof for another profile or the consumer's own launch action is not.
Admission runs before the WorkEnvelope advances and again inside its exact
reservation start guard. The prepared work must match the claimed identity and
the home state/policy/action generation being reserved.
Caller command, argv, environment, and permission
overrides cannot replace the owner wrapper's prompt-only launch.

The existing launch-profile selector passes the canonical owner profile ID into
preflight; an explicit missing profile refuses instead of falling back. Prepared
inputs freeze that ID, and start revalidation reloads the same owner profile.
Saved selections drive the unattended adapter input and direct launches using
the saved harness; a different harness override does not inherit its profile.
The shared runner records requested profile identity before command construction
and refuses disagreement with a nonempty selection snapshot before minting.
This is requested identity, not evidence of an observed model or provider.

The two harnesses have distinct, exact `prompt_only` posture markers; neither
marker authorizes the other harness or a Claude mutation mode. Source delivery
and route evidence do not establish live consumption: the approved runtime
cutover and an observed governed consumer still need their own proofs.

## Execution selection: what will run, what did run, and why they differ

Every launch is a tuple — launch profile (the lane), harness family, provider route, resolved model id,
requested effort — and Compound stores that tuple at two levels with two different lifetimes:

| Level | Type | Lifetime | Owner |
|---|---|---|---|
| Work-level saved selection | `envelope.ExecutionSelection` on `WorkEnvelope.execution_selection` | mutable by explicit save only; every save bumps `revision` and records a lifecycle event | `internal/work` |
| Attempt-level snapshot | `runattempt.ExecutionSelectionSnapshot` on `RunAttempt.execution_selection` | stamped once before the subprocess starts; append-only, never rewritten | `internal/runattempt` + the launch wire |

The saved selection is why "open work, pick a combination, exit, reopen" yields the same combination: the
choice lives on the envelope, not in the launcher, the route plan, or a preset. A renamed model alias or an
edited preset changes nothing that is already saved — the selection carries the resolved model id and the
owner profile revision it was made against, and a launch whose observed profile revision differs reports
`selection_drift` on the attempt instead of silently applying the new meaning.

**Precedence at launch, highest first.** (1) An explicit one-run override on the launch call (`--harness`,
`--launch-profile`) applies to that run only; the attempt records `source: override` and the saved selection is
untouched. (2) The work's saved selection (`source: saved`, with the `selection_revision` consumed). (3) The
project/profile default — for unattended work the route plan for the capability (`source: project_default`).
(4) The system default (`source: system_default`). Only an explicit save (`ExecutionSelection.Source =
explicit`) replaces the saved selection; a one-run override never does.

**Requested is not applied.** Effort is recorded three ways on the attempt: `requested_effort` (semantic),
`applied_effort` (the harness-native value the adapter actually passed, possibly a documented translation), and
`effort_evidence` (`argv:<flag>`, `env:<VAR>`, `settings:<path>`, `receipt:<ref>`, `unsupported:<why>`,
`unobservable:<why>`). A harness with no effort control records `unsupported` and an empty applied value; a
label that merely travelled through a wrapper records `unobservable`. The validator refuses an applied value
under either. Nothing here claims a provider received the value — that remains route evidence
(`unknown_until_receipt`).

**Sessions and lineage.** `native_session_id` plus `native_session_evidence` (`pinned:<flag>`, `derived:<path>`,
`unsupported:<why>`, `unobservable:<why>`) distinguish captured sessions from missing observation. A native
resume targets a known session and never "the most recent one". `continuation_of` names the predecessor attempt
or `native:<id>` a launch resumed. A
cross-harness continuation keeps the Compound work lineage (work id, predecessor attempt, context pack) and
records the new harness on a new attempt; it never pretends one harness's session format was resumed by
another. Intentional changes of model, effort, or harness are new attempts with a new snapshot and, when saved,
a new selection revision with its lifecycle event — never an edit of a historical attempt.

**What a reader sees.** `compound work show <id>` prints the saved selection under `execution_selection:` (what will run); each attached
attempt prints its snapshot's `source: saved|override|project_default|system_default`, `selection_rev`, and any `selection_drift` line (what did run, from which source, with which evidence); a difference between the two
is visible as `source: override` or `selection_drift` on the attempt. No launcher script has to be read.

**Every governed path carries the snapshot.** The public `work advance --execute`
CLI resolves the choice once — explicit override, else the saved selection, else the
project default — and stamps the full snapshot (harness, profile, requested effort, consumed
selection revision) onto the RunAttempt through the shared runner, on the same boundary it
prints `selection: source=… profile=… rev=…`. `--expected-selection-revision N` is the CAS
guard on that path: a stale revision produces zero execution and lifecycle delta. Unattended
picking refuses a candidate whose saved selection differs from the selected profile
(`ineligible:saved execution_selection is not selected profile …`) — the global route never
silently overrides the work's own saved choice; route and capacity checked on one lane cannot
attest a candidate saved for another.

Unattended selection consumes the route authority's ordered eligible candidates and reads fresh
capacity for each. The authority supplies routing order and capability eligibility; there is no hardcoded provider roster.
Stale, unknown, exhausted, removed, and unready routes produce bounded refusals before a claim.
At most five routes and five ready items per route are examined, with at most one launch.
Distinct profiles using the same harness remain separate candidates. A saved choice must match
profile, harness, model and provider. It overrides creator provenance only on dispatcher-owned
work; unselected foreign work remains ineligible.

The detached door carries a digest of profile, harness, model, provider, capacity lane and
source revision through `--expected-execution-tuple`. The child checks fresh owner resolution
before mutation. The separate integer `--expected-selection-revision` guards the saved choice:
selected work retains its saved snapshot; unselected work receives a one-run profile override.
Adapter readiness and final start revalidation consume the same profile.

A saved profile may use a wrapper alias while routing names a descriptive model
entry. Its exact profile is preserved when its saved model, provider and harness
match the candidate; owner preflight re-resolves that profile against the sealed
manifest before claim. A removed, ambiguous or foreign alias still refuses.

Claude-family and Codex governed adapter prompts carry the current reserved WorkEnvelope's work/context IDs, desired outcome, success predicate, non-goals, and saved selection. The caller's latest explicit request takes precedence, including on resume. Raw argv commands and prepared prompt-only adapters retain their caller-supplied input contracts.

`work advance --execute` consumes the context already attached to the work.
A parent `--context-pack` flag is a usage error, including with
`--resume-verifying`, and refuses before adapter preflight, lifecycle changes,
or a RunAttempt. Attach the context in a separate same-state advance first,
for example `compound work advance work_example --to claimed --by codex
--context-pack ctx_example`, then execute without that flag. For work already
in `verifying`, use `--to verifying` for the attachment. Arguments after `--`
belong to the child command; its own `--context-pack` is passed through unchanged.

Codex keeps a fresh isolated `CODEX_HOME` per invocation. Explicit `COMPOUND_CODEX_USE_EXISTING_SUBSCRIPTION=1` binds only a private, owner-owned ChatGPT auth snapshot into that home after RunAttempt mint; it omits refresh authority and API keys, preserves the source refresh timestamp, refuses non-native wrappers and caller auth overrides, and removes the snapshot on return. The child uses native read-only approval/sandbox flags. Native Codex selections require a model ID and supported reasoning effort; legacy wrappers retain their selected model contract but cannot claim a native effort they do not attest.

For opt-in subscription execution, the saved Codex model, route, profile, and revision reach preflight before execution; an owner-selected native model/provider tuple is required. Supported requested effort is applied in native argv and attested there, never inferred from the route. The child ignores user config and execpolicy rules, pins the OpenAI provider, and emits JSONL events. Snapshot cleanup failures are surfaced.

## Cancellation and budget are owned by the canonical execution seam

`ExecuteGovernedHarnessAdvance` is the one mutation seam shared by the public CLI, the
selector-driven execution, and (through the detached door, which invokes the public CLI) the
unattended worker. It takes the caller's launch context — production: the signal-derived
`executewire.LaunchContext()` — so SIGINT/SIGTERM cancel the governed run itself. The shared
subprocess runner translates cancellation and the RunInput's finite `Timeout` into a
process-group kill with `cancel:context_cancelled` / `timeout:exceeded` evidence on the
attempt, then finalizes the envelope failed. `work advance --execute` binds the resolved
`--timeout` budget (absent = the wire default, capped at the detached budget) onto RunInput
BEFORE adapter preflight, so the tuple-backed capacity admission above compares the quota
reset against the run's actual rounded duration; binding it after preflight would leave the
adapter admitting against an uncapped horizon. The bound is preserved through readiness and guarded-start
revalidation; neither may reconstruct an uncapped input. A caller passing `context.Background()` forfeits
the guarantee: cancellation never reaches the adapter, and the detached worker inherits only
its own external budget.

The first SIGINT or SIGTERM requests cancellation. Repeated SIGTERM during
finalization is the same request: a process-group TERM can also reach GNU timeout,
which forwards another TERM to the door. It must not restore default TERM handling
and kill the door before the attempt and lifecycle settle. The launch owner keeps
TERM handled, restores its Ctrl-C handling before publishing cancellation, and
hard-stops its own process after 60 seconds if cleanup cannot finish. A subsequent
explicit Ctrl-C or SIGKILL remains a hard stop; a forced stop is not evidence of
successful finalization. Selected-work interruption returns to verifying with a
failed attempt, not accepted work. The public synthetic journey emits its work,
lease, attempt, detached log and event evidence on failure; CI retains those gate
logs with the report and verdict output, never a complete runtime home.

If a descendant retains capture pipes after the first cancellation/timeout signal,
the runner retries the same process group while its unreaped leader pins the identity.
Retries are disabled under the capture/reap guard before `Wait` can release the PID.
The initial timeout remains armed for a child that closes its pipes early. This
closes the one-shot fork/cancellation race without widening the signal target or
changing the run budget.

The shared runner is the sole cancellation path for the adapter command. Immediately
after the adapter builds the command and before it starts, the runner clears the
command's inherited `Cancel` callback and `WaitDelay`, so `os/exec` arms no context
watchdog: the default callback would kill only the direct process, and a nonzero
`WaitDelay` would add a second direct-process killer that can also force-close the
capture pipes. Either would race the runner's process-group kill. Pre-start rejection
is unchanged — the runner refuses a launch whose context is already cancelled, and
`os/exec` independently refuses to start a command with a done context. Sole ownership
of cancellation is not sole ownership of the child's life: the child can still exit
on its own, or be signalled from outside the runner.

The process-group guarantee is exactly a group guarantee. The kill reaches the direct
child and every descendant that stayed in the runner's process group. It does not
reach a descendant that starts its own process group or session, nor work handed to
a remote or external job; such a descendant can outlive the group kill, and no
process-tree guarantee is claimed for it.

The reap guard that keeps the kill paths off an already-reaped PID is unchanged, and
it is not airtight: `cmd.Wait` runs outside the guard and the `reaped` mark follows
it, so a narrow wait-return-to-mark window remains in which the first
cancellation/timeout signal can still reach a group id the OS has begun to reuse. No
reproduction is claimed; closing that window is outside this repair.

### Actuator admission accounting is not a mission budget

The separate trusted-local actuator run, post, and execution-mode consumers reserve one
product/channel action in the existing `actuator/budget/consumption.jsonl`
before dispatch. Current-budget recheck and reservation append share the short
home mutation guard; execution never holds that lock. Settlement preserves
reservation identity, work, action, channel and timestamp and counts once.
Exact settlement retry is idempotent. An in-flight or uncertain reservation
does not age out; failure to persist settlement returns non-green while keeping
the actual execution/receipt evidence. Inspect the named reservation and its
work/attempt before reconciliation; do not delete ledger rows to recover room.

Consumption v0.2 reads legacy v0.1 rows without rewriting them. Quiesce all old
actuator writers before the first v0.2 reservation; mixed old/new budget writers
against one home are unsupported. Source acceptance is not installed cutover.

These limits retain their existing product/channel rolling semantics, not
mission/account semantics. The wildcard is a channel fallback, not a sum of all
channel usage. Local staging/config writes do not call a provider. Harness runs
have no bounded provider-cost/token settlement here, so a configured finite
spend/token ceiling holds rather than advertising enforcement without accounting.
This does not certify `work advance --execute` mission spending, provider billing,
authenticated approvals, or autonomous containment.

## Detached start/record failures are recoverable, not "failed to spawn"

Explicit selected-work Continue reuses this door without unattended route selection
or forced bypass. It carries the saved tuple/revision and a full source-generation
digest to `work advance --execute`; explicit verifying resumes add `--resume-verifying`.
The current goal and context come from the reserved WorkEnvelope, not repeated caller
prose. Source drift refuses before minting; fresh policy and exact start/finalization
reservations remain mandatory. A launch acknowledgment means only that a bounded door
started, not that its adapter passed preflight or its task was accepted. New attempts
retain retry lineage and archived launch evidence; observed model/provider identity
remains unknown unless attested by route evidence. This is a mechanics contract, not
evidence of improved product quality, saved human time, or successful model transfer.

The detached door protocol is start/record-safe (2026-09-15):

```text
durable reserved launch identity (sidecar, outcome=reserved, no pid)
→ guarded process start (a start failure removes the reserved sidecar)
→ started-identity line in the door log (pid/pgid)
→ sidecar updated to running+pid
→ observation (reconcile) → finalization/recovery
```

A recording failure after `cmd.Start` returns a typed `DetachedLaunchRecordError` carrying the
started identity. The door owns the envelope, so the stage leaves it claimed (`launch_record_recovery`)
and never closes it as failed-to-spawn. Log writes and sync errors are checked; sidecar publication
syncs its temporary file before rename and its directory afterward. Reconciliation adopts a `reserved`
record when the log contains a pid for that exact launch identity.

If both recording paths fail, or the launcher crashes immediately after `cmd.Start`, a free ownership
lock proves only that the launcher left its critical section. A durable `start` decision with no pid
therefore yields `recovery_required`, while the durable sidecar remains `reserved` and blocks duplicate
dispatch. Legacy reservations without a launch identity are equally uncertain without matching log
evidence. No elapsed grace period proves that nothing started. A later matching pid can be adopted;
otherwise exact recovery needs additional process evidence. Only a durable `revoke` decision before
start, or an observed dead recorded pid, permits the existing lost-door lifecycle path. The kernel
lock, exact launch-id binding, and revoke-before-close ordering remain the ownership mechanism.

## Route evidence is a projection, not a verdict

> A gateway `RouteDecision` is **execution-route evidence, NOT proof of task success.**

Recorder provenance is assigned at ingress, not accepted from payloads.
Public imports are `external_import`; ordinary producers, including current
Panel/OpenAI-compatible callers, are `generic`; missing historical values
normalize to generic for resume. The reserved `trusted_panel_runtime` value
has no production caller until an attested adapter exists. Imported, generic,
fixture, and legacy rows remain reviewable metadata but cannot decide
autonomy or shadow a differently bound Panel request id.

Panel call reservations and completions under
`panel-runs/attempts/<run-digest>/` are durable recovery evidence owned by
`internal/panel`. A resumed run validates every persisted call reservation
and completion before another model call; unresolved reservations remain
`unknown_spend` and fail closed rather than replaying earlier lenses. Keep
attempts with their run until a Panel-owned recovery or retirement action can
disposition them. Generic home cleanup and doctors do not delete or repair
this evidence, and no automatic retry may infer that an unresolved call was
free.

It becomes one projection inside a compound `Transition` (the cost/routing projection), joined with
the harness receipt and the real state delta:

```text
gateway trace alone   = a model-traffic fact
harness receipt alone = a work claim
compound Transition   = experience (the join is where compounding happens)
```

compound imports route decisions as `ExecutionRouteEvidence` (see `contracts/README.md`): selected
target, rejected candidates + reasons, model/provider/account, cost, latency, status — **metadata
only, no prompt/response/secrets.** Task success still requires a state delta (tests passed, file
changed, PR opened, product signal moved, or an explicit blocker).

External gateways use `compound route import <execution-route-evidence.json>`; provider-call NDJSON
ledgers use `compound route import-provider-ledger <provider-calls.ndjson> [--limit N|--tail N]`.
Both paths write metadata-only evidence rows and emit the same audit-only Transition routing/cost
projection. `compound route cost-doctor <provider-calls.ndjson> [--record]` records missing
cost/metadata coverage as a parked route observation plus no-promote decision, not billing truth or
policy. The provider-ledger adapter copies route/cost fields and drops source-specific row payloads
instead of importing prompts, responses, paths, or provider secrets.

Local `compound distiller extract` follows the same rule: successful extraction writes metadata-only
`ExecutionRouteEvidence` and an audit-only Transition whose routing/cost projections point at that
evidence; it still does not prove the extracted candidates are true or promoted.

Compound-owned route policy authority is separate from route execution evidence. The authoring door is
`route promotion-apply`: it revalidates a current `review_required` promote/demote artifact immediately
before mutation, mints an ActionIntent bound to the before and exact proposed authority revisions,
requires an allowing PolicyDecision, changes one claim through the act seam, and records the receipt,
transition, pre/post images, and literal `promotion-revert` command. Revert re-plans from that applied
record and restores only the canonical content-addressed pre-image; exported plan structs and applied
record bodies are untrusted input. `promotion-record` remains hold-only and never mutates authority.

`compound route import-model-capabilities <model-capabilities.yaml>` remains an additive
migration/backfill bridge into `compound://registries/model-router`; it is not the steady-state
authoring route. The compatibility direction is Compound outward: `route model-capabilities --yaml`
renders the deterministic OS view, while `--check-view FILE` compares every manifest field and exits
nonzero on semantic drift. The external generator consumes that projection instead of maintaining a
second meaning owner. `compound route plan --capability-class <class> [--privacy-sensitive]` only
renders a planned lane chain with `lane_justification`: it does not call models, rank live providers,
mutate gateway config, or certify task success. Privacy-sensitive plans filter to `local-batch` lanes;
free judge/critique lanes are tripwires, not verdict gates.

Lane resolution is typed independently of its execution address. `provider_route` says how to call
the lane; `resolution.mode=adaptive` means the requested alias must be resolved by gateway evidence,
while `resolution.mode=direct` declares one or more exact provider/model response identities. A
legacy lane with no resolution block may still be planned and called, but it cannot contribute a
deciding autonomy sample. This permits an incremental registry migration without inventing local
model identities or silently disabling existing route consumers.

Harness execution consumes the same authority instead of maintaining a second wrapper map. Exact
model id, alias, then harness id form the resolution precedence; ambiguity fails closed. A declared
`capacity_source` binds quota evidence to its own provider ref (for example
`codexbar://provider/qwencloud`) even when the execution route has another name. Capacity admission
answers to the run's own bounded duration: a Claude-family lane preflight and governed start
revalidation compare the quota reset against `ceil(RunInput.Timeout / minute)` (minimum one minute,
exact multiples exact), so a bounded run requires quota to outlive that rounded bound,
while a zero/uncapped timeout keeps the 60-minute horizon and stale, unknown, exhausted, and
exactly-at-reset evidence still refuses. Unrepresentable rounded horizons fail closed. Inline governed
execution and experimental delegated tmux sessions both resolve this tuple before work moves or a RunAttempt is
minted. Delegated activation additionally requires the explicit process-local
`--experimental-delegate-mutations` gate. A raw command override is not executable authority: delegated activation refuses it
until an authority can attest that exact executable/profile. The delegated argv policy is also
fail-closed: the current tuple-backed policy admits no argv or one non-flag positional prompt; it
refuses flag-shaped or multi-token argv, so permission/profile flags cannot borrow the wrapper's
attestation. RunAttempt records requested tuple/provider separately from observed
`model_id`/`provider_route`, which remain unknown until ExecutionRouteEvidence supplies them.

After the work and lease holder are validated, delegated activation consumes a read-only
harness-readiness check at that boundary. Permission authority and applied posture must agree; the
permission owner hashes the exact target bytes it validated, and the authority revision is re-read
after posture validation so neither unvalidated replacement bytes nor a mixed-generation check can
be attested.
The installed-hooks declaration must expose one exact native-blocking, all-tool/mutation-covering
`PreToolUse` binding whose matcher equals the current canonical harness matcher (`*` today), keeping
every shell, browser, connector, messaging, secret, install, deploy, and newly added tool behind the
action monitor. Read-only live reconciliation must still match that binding's id, matcher, invocation
revision, and handler revision—not merely some ready `PreToolUse` row. The current, non-degraded
InstructionPack must
agree with current policy/hook authorities, while the WorkEnvelope's ContextPack selects its
immutable owned EffectiveContract artifact by `contract_id`; rotation of the unrelated
`contracts/effective/latest.json` projection cannot invalidate older work.

Failure returns structured `readiness_unmet` evidence before capacity admission, event persistence,
tmux creation, or a `harness_execution` RunAttempt mint. A missing readiness checker fails closed.
The admitted evidence rows plus the full lane-capacity verdict are canonicalized into one
`snapshot_revision`. Immediately before mint, the lifecycle-admission lock re-reads the exact WorkEnvelope hash
and lease, re-resolves the execution profile, revalidates the compatibility attestation against the exact selected
harness/lane/model/provider-route/transport tuple and canonical plan identity, re-runs harness readiness and
capacity with the same captured inputs, and requires the same snapshot revision. Any changed authority or capacity
returns `authority_changed` with no mint, refusal-event persistence, or spawn. A successful activation carries the
revision-bearing snapshot, including the typed canonical execution-profile proof, in its returned result and persisted
`work.delegate.spawned` binding. Recovery revalidates the plan projection, proof body, reference, digest, and
aggregate readiness revision, so it can name what admitted the run without trusting mutable current
configuration.

**Teardown reaps what activation created.** A delegate leaves two artifacts that outlive its
process: the tmux session and the `git worktree lock` over its workspace. Every terminal path
(`DelegateSweep`, and the spawn-failure finalizer) releases both, and the sweep outcome always
reports what happened to the lock (`released` / `not_locked` / `held_by_live_attempt` /
`no_workspace_evidence` / `reaper_absent`) so "nothing released" stays legible instead of silent.
Ownership is the constraint, not convenience: the workspace comes from the terminalized RunAttempt's
own record, the path must actually be locked, and no other non-terminal RunAttempt may name that
workspace. On the session side the same principle applies — a delegate re-spawning over its own
recorded exit reaps only a DEAD pane under that name; a LIVE pane is the ownership uncertainty
`spawn_ambiguous` exists for and is never touched. Lock reaping is reported, never fatal: an
advisory lock that cannot be released must not strand a terminal work item. What teardown could not
reap is surfaced by `compound doctor`, which joins locked delegate workspaces against their
RunAttempt status and flags every lock whose owning delegates are all terminal; doctor reports and
never releases.

## Second-order effects (design against these)

- **Cheap-mediocre drift.** If the router optimizes cost/latency without *outcome* feedback, the
  system gets cheaper but worse. Antidote = **routing remorse**: sample cheap-route outputs against a
  stronger judge; promote a cheap route only after outcome evidence; auto-escalate for canonical
  memory, product positioning, taste, irreversible actions, or high-risk code.
- **Credentials become infrastructure.** Centralizing provider/account selection means agents stop
  carrying provider truth — but gateway config drift can drift *all* traffic. Treat gateway config
  changes as governed infrastructure that emits compound evidence.
- **Autonomy gets a real budget boundary.** Autonomy = a semantic gate (compound: risk tier, allowed
  providers, privacy class, budget) **+** a runtime gate (gateway: cost ceilings, quotas, provider
  restrictions, concurrency) **+** a state receipt after. A Tier-1 job cannot accidentally overspend
  or use a forbidden provider.
- **Native-direct vs ingress vs relay.** A client may talk directly to a vendor (gateway can only
  import metadata), through gateway ingress (full route/usage trace), or via a native relay (gateway
  leases native auth). Native relay is the hardest mode — keep it behind a conformance/fixture gate
  and fail-closed; integrate **read-only/observation first → controlled ingress → relay canary**.
- **Multimodal is not free.** A text+tools gateway should reject multimodal at ingress (not silently
  drop it). Route visual/taste work (screenshots, UI QA, image review) to a vision-capable harness or
  a manual lane, and record the text/judge calls around it — do **not** push visual work through a
  text-shaped pipe and quietly degrade product quality.

## What the gateway must NOT become

Not the source of truth (eval evidence is preview until compound promotes it for a scope); not the
whole `Harness` registry (it contributes facts; compound owns the registry); not a cheapness-only
optimizer (cost is one dimension among taste/judgment/outcome/privacy/latency/reliability); not
another dashboard tax. Gateway work counts only if it reduces provider/model ambiguity, creates
usable route evidence, enforces a real cost/risk gate, removes duplicate routing config, or enables a
real product/system loop.

The execution-plane construction track is in `../roadmap/phases.md` (P10 — execution route evidence).

### Bounded upstream evidence producer

`scripts/upstream-learning/learning_tick.py` is an optional external scheduler
entry point for explicit GitHub source frontiers. It creates governed work for
one bounded acquisition/intake pass, requires an allowing action policy, custodies
hash-checked text through findings, and sends no-promote source observations to
the existing candidate owner. It drains pending delivery before acquiring more;
unchanged inspected content stays quiet even when repository HEAD advances.
Implementation/configuration hashes and truthful audit-only receipts make the
actual producer visible. The canonical Bun inventory runs its synthetic and
temporary-home intake tests.

Acquisition performs no model calls, installations, graph mutations or automatic
adoption. The optional `review_tick.py` consumes acknowledged pinned findings and
makes at most one governed read-only audit (`classify-extract`) or independent
provider challenge (`certify`) per invocation. It uses registered Claude-family
adapters with enforced plan permissions and fresh alive capacity. Route planning
uses the explicit `--read-only` capability-plan mode, which preserves refusals
without recording them; candidate planning refuses that flag. Saved execution
selection and actual attempt provenance must agree before intake. Completed
responses survive custody retries without another model call; a blocked
finalization does not starve other sources. In-progress work waits. Invalid
responses have bounded retries that prefer untried eligible model/provider tuples
for the same job without mutating shared route authority. Citation-checked model hypotheses
remain operational evidence, never promoted truth or demonstrated improvement.
Priority bands expose duplicates, disagreements, missing evidence and candidates
for experiments. Experiment selection, reviewed adoption and runtime feedback
remain with existing work, capability and receipt owners. A failed audit may use an existing eligible
`certify` route only after all current cheap audit tuples have failed that same
job; shared ratings and admission remain unchanged. Reports accept a JSON
document or one explicit JSON fence, retaining strict schema and pinned quotes.
New review work declares an accepted-or-rejected validation predicate before
launch. Invalid output becomes a distinct owned rejection finding, never an
assessment; the validation pass completes `audit_only` without relabeling its
successful process. Replaying the same transcript and pinned revision validates
rejection retry evidence. Legacy valid-hypothesis predicates still require a
valid response through governed continuation. Host scheduling and
source configuration are private deployment inputs, not a second scheduler or
meaning authority in the core.
### Harness settings: one defaults owner, declared executor observations

`compound harness settings-matrix [--harness H] --json` is a read-only, freshly
derived projection over every registered harness identity and existing
`config/harness-permissions/<harness>.yaml` authority. Missing ownership is explicit;
the adapter registry is not the identity inventory. Native keys have same-purpose
labels, **not** asserted model or reasoning-scale equivalence across providers.
No new preference catalog or cached matrix becomes authority.

`PermissionAuthority.setting_sources` optionally declares executor-owned literal
observations: `id`, `owner`, `format: shell_env`, absolute `path`, and `mappings`
from owned string settings to source environment keys (for example
`model: LAUNCH_MODEL`). Only the declared regular leaf is read, bounded to 128 KiB;
it is never sourced or executed. Reports contain key status and exact-byte revision,
never values or bodies. Mapped expansion, duplicate/missing assignments, unsupported
formats, unowned mappings, symlink leaves and unavailable files are non-clean.
Existing `harness drift` and doctor consume this result, including native-clean /
launcher-mismatched cases; rendering never writes executor sources.

Update requested defaults through the existing Compound authority, then render the
native target through its governed apply seam. The executor owner updates its
launcher; declare its observed mappings in that same authority and check drift.
Re-run the matrix after harness/setting/source changes: new identities and owned
keys appear without editing a second table. This observes declarations, not every
possible overlay or actual inference. Fresh session model/effort, undeclared CLI
flags, profiles and remote app overrides remain explicitly unverified.

### Selection capability: fail closed before launch, record what was observed

Each registry row also carries `selection` (`model_passable`, `effort_passable`,
`effort_observable`, `session_pinnable`, a required `basis`), shown in the settings
matrix. It is the one source the snapshot builder and the shared subprocess runner
read; there is no per-harness name list. A requested model or effort the row says
the launch cannot apply is refused before any RunAttempt is minted with
`selection_unsupported:<harness>:<field>` (also at public-CLI bind and wire
preflight, so the envelope is not advanced). A request is never cleared and run
without. A name with no row (including the raw `claude` adapter and
`os-harness-entrypoints`) refuses every requested model or effort.

At stamp time `execution_selection.settings_digest` digests the harness's live hook
posture (`enforce | observe | none`), read with the same installed-binding reader
doctor probes; a harness Compound installs no hooks for has no digest (unknown, not
`none`). The append-once `observed` block beside the snapshot records what the
built launch will run: codex model/effort from argv, claude lane model/effort from
the wrapper's literal `SB_LANE_CLAUDE_MODEL` / `SB_LANE_REQUESTED_EFFORT` exports
(read with the settings-source reader, never sourced), and the hook posture. It is
absent, not zeroed, when nothing was observable.


Native collaboration recipients validate the exact ActionIntent body, policy body identity,
lease work/actor, and agreement between the attempt, selection snapshot, and query harness.
A same-state authority replacement invalidates the old recipient. Generic
pinned:<flag> evidence retains the execution-selection schema's meaning.

Only the Stop fallback consults runattempt.ListByNativeSessionID: it probes
main-record session IDs before loading matching launch sidecars and preserves
unreadable-record errors. Terminal attempts remain known even when their work
is archived or the reservation-loss path has no envelope forward-link.
Ordinary tool hooks continue to inspect nonterminal attempts only.
