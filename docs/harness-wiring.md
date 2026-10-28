# Harness Wiring — connect Compound to your agent

> Compound is **agent-agnostic**: it works with Claude Code, Codex, OpenCode,
> or any harness that can call a CLI at lifecycle boundaries. This guide shows
> how to wire the hooks so your agent sessions compound instead of evaporating.

## How it works

Compound exposes **lifecycle hooks** — CLI commands your harness calls at
session boundaries. Each hook produces a typed spine object:

```text
session-start      → ContextPack (what the agent should know right now)
user-prompt-submit → ContextPack (refreshed context mid-session)
pre-tool-use       → PolicyDecision evidence (blocking only under explicit enforcement)
post-tool-use      → ToolEvent (evidence of what happened)
session-end        → Receipt + Transition (what was learned, what changed)
```

In interactive hook mode, the harness calls Compound. Governed delegate
execution is separate: Compound can launch an enrolled executor through its
adapter, policy and RunAttempt owners. Interactive hooks are
**hooks-as-clients**: the harness is the client, Compound is the authority.

Every task-scoped CLI orientation path (`orient`, `contract compile`, and
`intent plan`) validates its client identity before repo detection or runtime
materialization. Dashed aliases resolve to the canonical HarnessRegistry name,
and the requested working directory must resolve to an existing directory.
Unknown harnesses and fictitious workspaces fail without persisting a
ContextPack or EffectiveContract. Operator orientation is a separate aggregate
view and intentionally does not impersonate a task harness or project root.

## One semantic front door

Choose the posture in Compound terms, not in vendor event names:

```sh
compound harness posture-plan --harness claude-code
compound harness posture-plan --harness claude-zai
compound harness posture-plan --harness codex
compound harness posture-plan --harness opencode
```

The stable intentions are `prepare_context`, `prepare_turn_context`,
`observe_action`, `guard_mutation`, `observe_outcome`, and `record_receipt`.
Session orientation and per-turn injection are two intentions, not one, because a
harness can want one without the other. The selected adapter
owns native events, settings schema, matchers, and limitations. For
Claude-family harnesses, `record_receipt` maps the native `SessionEnd` event to
the bounded receipt adapter. Every compiled intention says `native`, `fallback`,
`degraded`, or `unsupported` and cites the harness attestation revision.

Default postures select the subset appropriate to their role. Interactive
Claude Code and Codex select `observe_action`: `PreToolUse` records the
ActionIntent and PolicyDecision but cannot publish a native permission decision
or a blocking exit. Isolated Claude-family executor profiles select
`guard_mutation` and install `PreToolUse` with explicit `--enforce`.
Interactive Claude Code does not select `prepare_turn_context`: it takes
orientation once at `SessionStart` and installs no `UserPromptSubmit` binding.
Codex and the isolated Claude-family profiles select both context intentions.
If an observation-only live hook cannot record its evidence, it reports the
runtime error but still exits zero without a native permission decision; that
path makes no containment claim. Explicitly enforced profiles remain blocking.

Enforcement is never claimed from configuration alone. An enforcement claim
requires behavioral proof for the exact launch profile: `compound doctor`
probes the installed binary with a nonce-bound fixture before the posture
counts as mutation-blocking, and a harness that cannot synchronously deny
before tool execution is never mutation-ready. The supported scope for what a
wired harness may claim is defined in
[`security/supported-scope.md`](./security/supported-scope.md).

Claude-family and Codex `compound hook install` consume this projection when
they write managed hooks. The OpenCode installer asks Compound for the same
projection and refuses adapter drift before writing its config/plugin. OpenCode
currently reports `guard_mutation=unsupported`; its before-tool callback cannot
deny, so Compound will not call it mutation-ready.

Each non-check native install also records only that compiled managed roster in
`compound://config/installed-hooks`; sequential harness installs preserve the
other harness rosters. `--check` does not mutate native config or authority.
Mutating `compound hook install` and legacy hook-authority imports share the
Compound admission lock, so cooperating CLI writers serialize the native-config
and authority transaction. External config editors do not take that advisory
lock; preimage validation refuses drift it observes, without claiming mandatory
filesystem exclusion.

The vendor-shaped examples below are adapter evidence and troubleshooting
fallbacks. They are not the operator contract.

## Claude Code

Claude Code supports hooks in `.claude/settings.json` (project-level) or
`~/.claude/settings.json` (global).

### Option A: managed interactive posture (recommended)

```sh
compound hook install --harness claude-code --scope global
compound hook install --harness claude-code --scope global --check --json
```

The managed default includes this observation-only binding. Its equivalent
vendor-shaped entry is:

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "compound hook pre-tool-use --live --harness claude_code"
          }
        ]
      }
    ]
  }
}
```

The CLI accepts the operator-facing installer label `claude-code`, but the
managed command uses the HarnessRegistry identity `claude_code`. The two names
are deliberately distinct: dashed names select an installer/profile; snake-case
names are what live hooks pass to ContextPack and policy compilation.

The same install also reconciles `SessionEnd` to exactly one bounded adapter:
`compound hook session-end-adapter --live --harness claude_code`. It retires
the legacy `SessionReceipt.hook.ts` shim and old direct Compound SessionEnd
commands while preserving unrelated SessionEnd hooks, including foreign hooks
co-located in the same group.

### Isolated Claude-family profiles

Only an isolated guarded executor profile should install the blocking binding.
`CLAUDE_PROFILE_DIR` must identify that profile's separate config directory:

```sh
compound hook install --harness claude-zai \
  --config-dir "${CLAUDE_PROFILE_DIR}"
```

The named guarded installers are `claude-zai`, `claude-minimax`, and
`claude-qwen`. Their live hook commands use `claude_zai`, `claude_minimax`, and
`claude_qwen` respectively, matching Compound's HarnessRegistry. Installation
preserves profile-owned `model` and `effortLevel` settings.

The managed `PreToolUse` entry for that isolated profile explicitly enforces
the decision:

```json
{
  "type": "command",
  "command": "compound hook pre-tool-use --enforce --live --harness claude_zai"
}
```

In this explicitly enforced posture, Compound returns policy gate exit codes:

- Exit 0 → allow (the tool call proceeds)
- Exit 3 → hold (the tool call waits for operator approval)
- Exit 4 → deny (the tool call is blocked and the agent sees the reason)

Some launch profiles, such as a GPT or full Z.ai profile, are model
selection identities rather than distinct Compound harness types. Enroll those
profiles through the canonical generic Claude posture instead of inventing a
new HarnessRegistry row for every model:

```sh
compound hook install --harness claude-code \
  --config-dir "${CLAUDE_PROFILE_DIR}"
```

That path installs observation-only `claude_code` context hooks and preserves
the launch-profile owner's model and effort settings. The profile id remains
visible in route evidence; the hook runtime identity remains `claude_code`.

### Compare models and efforts published by the profile owner

`route experiment-plan` reads the owner's non-secret conformance projection.
Repeat `--role` to expand one subscription profile into trials for its actual
Claude aliases, for example Qwen's `opus`, `sonnet`, `haiku`, and `subagent`
routes:

```sh
compound route experiment-plan \
  --profiles-root "${PROFILE_CONFORMANCE_ROOT}" \
  --profile claude-qwen \
  --role opus \
  --role sonnet \
  --role haiku \
  --role subagent \
  --json
```

The output names the concrete model alias and requested effort from the owner
projection; Compound does not maintain a duplicate model catalog. Repeat
`--profile` to compare providers. `--effort` filters to effort values already
published by those profiles:

```sh
compound route experiment-plan \
  --profiles-root "${PROFILE_CONFORMANCE_ROOT}" \
  --profile claude-qwen \
  --profile claude-minimax \
  --role opus \
  --effort xhigh \
  --json
```

An effort value that is not materialized by a selected launch profile is
refused. To test `high` against `xhigh`, the launch-profile owner must publish distinct
profiles for those efforts first; rewriting one shared profile between trials
would make the experiment identity unstable. The plan is a trial matrix, not
success evidence: execute each trial through governed work and import observed
provider/model/account route evidence before comparing results.

### Option B: import a generated hook roster

If you have a generated hooks JSON (e.g., from Codex's hook generation):

```sh
compound contract import-hooks --codex-hooks-json fixtures/harness/codex-hooks-json.json
compound contract hooks-authority   # verify the import
```

This installs the roster as `compound://config/installed-hooks`. After import,
`compound orient` and `compound contract compile` read the Compound-owned
manifest.

## Codex

```sh
compound harness posture-plan --harness codex
compound hook install --harness codex --scope global
compound hook install --harness codex --scope global --check --json
```

The Codex adapter compiles the stable intentions into the current native
`hooks.json` event schema and no-clobber merges Compound's bindings. Operators
do not maintain that schema directly. The default is observation-only and does
not claim mutation readiness: it installs `SessionStart`, `UserPromptSubmit`,
observation-only `PreToolUse`, and `PostToolUse`, while preserving unrelated
user hooks. Native blocking remains an explicit isolated-posture choice.

### Activate updated interactive posture

Active Codex and Claude Code processes cache hooks and permission posture at
session start. Installing hooks affects future processes, not the one already
running. Exit, start a fresh process with your existing permission and sandbox
settings, then resume your conversation.

Changing to `approval_policy = "never"`, unrestricted host access or
`bypassPermissions` is not an activation step. These are separate authority
decisions for deliberately scoped executor profiles. Removing prompts does not
expand a sandbox, and an observation hook does not make a broad permission profile
guarded. Require exact behavioral readiness before governed mutation.

## OpenCode

OpenCode is a context-injection and outcome-observation lane. Its current
before-tool callback cannot deny a tool call, so it is **not** a mutation-control
harness. Do not wire `pre-tool-use --enforce` for OpenCode and do not use it for
mutation-gated execution. Use an explicitly enforced isolated Claude-family
executor profile when Compound must block an action before it runs.

Install the repo-owned adapter from the Compound checkout:

```sh
bun adapters/opencode/install.ts
compound harness posture-plan --harness opencode
```

The installer registers only the native lifecycle it can honestly provide:

```text
chat.message        → context injection
tool.execute.after  → outcome observation
guard_mutation      → unsupported
```

It validates that posture before changing `opencode.jsonc` or the plugin.

## Other CLI harnesses

A custom harness may call Compound at lifecycle boundaries, but it is
mutation-ready only if it can synchronously propagate Compound's deny result
before tool execution. Model the harness attestation and inspect
`compound harness posture-plan --harness <name>` before relying on it as a
guard. Context injection and post-tool observation alone do not create a
mutation-control boundary.

## What each hook does

### session-start

Produces a **ContextPack**: task-scoped orientation for the agent.

```sh
compound hook session-start --live --harness claude_code
```

The agent receives:
- Relevant memory claims (only what's relevant to the task)
- Active capability offers (skills/tools that apply)
- Policy posture (what's allowed, what needs approval)
- Excluded context (what was deliberately left out and why)

### pre-tool-use

Evaluates an **ActionIntent** against policy before the tool runs.

```sh
compound hook pre-tool-use --live --harness claude_code
```

Interactive Claude Code records a **PolicyDecision**:

- `allow` → proceed
- `allow_with_obligations` → proceed with conditions
- `hold` → recorded as needing approval
- `deny` → recorded as denied

Without `--enforce`, the hook publishes no native permission decision and does
not turn `hold` or `deny` into a blocking process exit. The isolated
`claude-zai` profile above adds explicit enforcement: it publishes the
harness-native blocking decision and returns the policy gate exit (`3` for hold,
`4` for deny).

Enforcement posture is derived by the adapter from explicit `--enforce`; hook
input cannot claim it. Exact approval is eligible only for a direct,
single-action baseline hold. Observe-only and batch calls, denies, security
floors, placement blocks, dirty source-control state, and coordination holds
are excluded. The adapter persists the `ActionIntent` before minting or reusing
the plan, then carries the canonical plan reference in both the
`PolicyDecision` and `HookEvent`. An approved retry consumes that exact grant
once without mutating global harness permissions or configuration.

### session-end

Produces a **Receipt** (what was learned) and a **Transition** (what changed).

```sh
compound hook session-end-adapter --live --harness claude_code
```

The adapter durably accepts the exact input before starting the existing
`session-end` worker, bounds worker time, records a terminal outcome, and leaves
an exact-ID replay command on timeout or failure. Live hooks remain capped at
five seconds. The host-facing response is the generic empty JSON object (`{}`). Claude's
event-specific `hookSpecificOutput` union does not include `SessionEnd`; durable
acceptance, terminal status, and replay identity live in Compound's event store.

An explicit manual replay can use a separately bounded longer deadline (up to
30 seconds) without changing the accepted input identity:

```sh
compound hook session-end-adapter --replay hook_sessend_attempt_<id> --timeout 30s
```

The replay attempt records that deadline and its predecessor while preserving
the original input hash and artifact reference. This is where compounding
happens: the session's learnings become durable spine objects that the next
session's `session-start` will recall.

## Fixture mode (testing without a live harness)

You can test hooks without a running harness using fixtures:

```sh
# See available fixtures:
ls fixtures/hooks/

# Run a fixture:
compound hook session-start fixtures/hooks/session-start.json
compound hook pre-tool-use fixtures/hooks/pre-tool-use.claude.json
```

Fixtures are synthetic — no personal data, no live harness needed. They're the
same fixtures CI uses to prove hook behavior. Canonical fixture references are
compiled into the CLI, so installed readiness checks and fixture-mode commands
resolve them from any working directory.

## Verifying your wiring

```sh
# Check hook installation status:
compound contract hooks-authority

# Run the full orientation and see what context your agent would get:
compound orient --harness claude-code --cwd /path/to/your/project --task "my task"

# Check the harness registry (bundled registry; pass a registry.yaml to observe another):
compound harness observe
```

## The harness registry

Compound maintains a **HarnessRegistry** that records what each harness
actually supports (native blocking hooks, fallback adapters, etc.):

```sh
compound harness observe                     # observe the bundled harness registry
```

This is evidence-only — it observes what's installed, never configures or
mutates your harness.

## Governed execution and supervised sessions

### First model route

Managed hook registration and governed model execution have separate setup.
A healthy fresh home can have working hooks but no model-route authority.
Your chosen harness must already be installed and authenticated; Compound also
needs metadata matching its executable, provider/model route, permission posture,
and any declared capacity source.

Inspect the current authority without contacting a provider:

```sh
compound route model-capabilities --json
compound route lane-projection --harness codex --json
```

For Claude Code, use `--harness claude-code`. If the authority is absent, these
commands report the missing `registries/model-capabilities.yaml`. Use your
setup's existing model-capability manifest for the executor you intend to run:

```sh
compound route import-model-capabilities /absolute/path/to/your-model-capabilities.yaml
```

Importing enrolls that configuration in Compound's model-router authority; it
does not install a harness, authenticate an account, or call a model. The
[synthetic manifest](../fixtures/route/model-capabilities-tuple.yaml) shows the
format for integrators; it is not a working provider configuration. If you do
not have a manifest for your actual executor, stop model setup here and use the
local-command demonstration in [Getting Started](getting-started.md#2-orient-on-your-repo)
until that configuration is available. Do not import a synthetic fixture to
make readiness pass.

Once the harness has created and claimed the bounded task, check that work's
context and the intended executor's configuration:

```sh
compound harness readiness --harness codex --workspace /absolute/project --work <claimed-work-id> --json
```

Use the same runtime home, chosen harness and real project directory as the
claimed work. Resolve the reported prerequisites before launch. This is a
read-only readiness check, not a provider authentication test or permission to
execute. Without `--work`, task-context readiness remains unproven.

The registry says what a harness can do; `compound://registries/model-router`
owns the executable lane tuple used to run it. A tuple binds the harness id,
wrapper aliases, requested provider route, optional `provider_route_kind`,
permission posture, and optional capacity source. The two explicit route kinds
are `provider_class` (compare the selected route with the launch owner's
`provider_lane`) and `direct_model` (compare it with the launch owner's concrete
`route`); Compound never infers the kind from route spelling or model identity.
Legacy tuples without the field remain valid for delegation that does not select
an owner launch profile. `--launch-profile` fails closed until the tuple
authority publishes a known kind. Compound resolves the tuple before moving work
to `running` and re-resolves it immediately before RunAttempt mint. An unknown
or ambiguous tuple, missing wrapper, invalid posture, stale/exhausted declared
capacity, or launch-profile tuple missing an explicit route kind leaves the work
`claimed` and creates no execution RunAttempt.

Current spawned bindings and launch snapshots use paired v0.2 schemas. The
reader preserves paired v0.1 launch records created before route-kind identity
was persisted, but never upgrades them by inference or accepts a mixed-version
downgrade as a new admission.

Run a bounded task inline:

Delegation readiness also checks the Claude-family adapter's declared capacity
using its read-only process-start revalidation. Expiring, exhausted, or unknown
capacity cannot report ready when inline execution would refuse it. The preview
records no refusal evidence or RunAttempt and grants no permission to launch;
execution still revalidates the live authorities immediately before starting. For
tuple-backed lanes, readiness compares the adapter's revalidated executable,
model, provider route, harness, and lane with the selected request: rotation
between reads cannot report capacity for a different tuple as ready. Raw Claude's
non-tuple command normalization remains unchanged.

Check a particular work item's configuration and immutable context with
`compound harness readiness --harness claude_minimax --workspace /path/to/project --work <work_id> --json`.
The command resolves the canonical executor tuple and reads the selected work;
it neither claims nor launches it. Without `--work`, context readiness remains
unproven; missing or mismatched work context never becomes ready by inference.

```sh
compound work advance <work_id> --to running --execute \
  --harness claude_minimax --workspace /path/to/project \
  --prompt "implement the claimed slice"
```

The governed inline rail accepts exactly one `claimed → running` execution
reservation, compare-and-setting the claimed state, PolicyDecision, and last
ActionIntent after workspace/preflight resolution. For Claude-family adapters
an omitted permission mode resolves to
prompt-free `bypass` only when the prospective PolicyDecision allows the
advance, the build autonomy halt is clear, and the permission posture plus
mutation-blocking hook have one current revision each. Explicit `default`,
`acceptEdits`, and `bypass` require the same evidence; `acceptEdits` is never
chosen automatically. Missing or stale authority resolves to read-only `plan`.
For every Claude-family mode the rail re-resolves the model lane tuple,
executable, and declared capacity; mutation modes additionally revalidate the
exact harness-bound permission/hook authority tuple and halt at the
process-start boundary. Every governed adapter checks its exact running state,
PolicyDecision, and last ActionIntent immediately around `cmd.Start`, then uses
the same three-field compare-and-set when finalizing. Drift, revocation,
exhaustion, or a newly engaged halt terminalizes the attempt without starting
the child, while a later reservation change preserves the newer lifecycle
generation byte-for-byte. The CLI explicitly reports that the durable refused
attempt is reverse-linked only by `work_id` and `action_intent_id`; it never
mints a same-state policy/action generation merely to manufacture a forward
link. The RunAttempt durably records immutable, content-free event sidecars
beneath `run_attempts/launch/`: `prepared` before `cmd.Start`, then exactly one
terminal disposition: `committed` after success or `start_error` when the
callback returned an error before a successful start. Prepared-only and
committed stale launches remain ambiguous; an exact content-free `start_error`
may be recovered as side-effect-free.
Readers materialize `LaunchLifecycle` from those sidecars; the v0.1 main JSON
does not carry the evidence, so an old whole-record decoder cannot erase it.
Malformed or mode-unsafe sidecars on a valid `ratt_*` never disappear from a
successful directory walk: list returns the readable partial set with a typed
unreadable error, sweep counts the affected ids as scanned/skipped/unreadable,
and doctor names them instead of claiming every attempt is terminal. The
operator-facing list and sweep commands emit their readable/partial result
before exiting nonzero; public errors name attempt ids without rendering the
private underlying path cause.
The canonical per-attempt lock spans the callback, while the legacy
`lock/runattempts.lock` fence is held only around short main-JSON transactions.
A start-capable caller must enter that boundary before its external side effect;
this includes both direct harness `cmd.Start` and delegated session `Create`.
A stale current-version sweep therefore cannot abandon an attempt and then let
its child start, and one slow launch does not block unrelated heartbeats or
terminalization.
If delegated `Create` succeeds but later evidence attachment fails, cleanup
terminalizes only after it proves the owned session stopped. A session that
cannot be stopped remains visible as a live binding with its lock and
non-terminal attempt intact; recovery forward-links that attempt through the
same-state WorkEnvelope advance when possible, and returns both the live
binding and an explicit error when the forward link itself cannot commit.

The sidecar boundary is a one-way home-runtime cutover. Before the first
sidecar-backed launch, stop pre-sidecar Compound processes and sweepers; do not
run an older binary against that home afterward. The legacy fence safely drains
an in-flight v0.1 JSON writer, but an old sweeper does not know the per-attempt
launch lock and is not supported in a rolling old/new mix. Every Claude-family
execution RunAttempt persists the resolved mode, actual running PolicyDecision
id, and exact authority revisions.

Delegated tmux mutation is experimental and unsupported in the v0.1 product
contract. The supported governed execution path is `compound work advance
--execute`. For private experiments, every process must opt into delegate
mutation explicitly; status and attach remain read-only:

```sh
compound work delegate <work_id> --harness claude_qwen \
	--experimental-delegate-mutations \
  --workspace /path/to/project --prompt "process the remaining batch"
compound work delegate status <work_id>
compound work delegate progress <work_id> \
	--experimental-delegate-mutations \
  --done 12 --empty 2 --failed 1 --remaining 5
compound work steer <work_id> --experimental-delegate-mutations \
	--text "finish the current item, then stop" --enter
compound work attach <work_id>
```

A raw command after `--` replaces only the executable. It cannot bypass tuple
existence, provider attribution, or capacity admission. RunAttempt stores the
requested tuple separately from observed `model_id`/`provider_route`; those
observed fields remain empty until route evidence attests what actually ran.
The console reads the same binding and RunAttempt, including cumulative
done/empty/failed/remaining progress.

Idle schedulers are not supported clients of delegated mutation in v0.1. A
supported scheduler must enter through `work advance --execute`; private
experiments may select lanes from Compound's capability route plan and invoke
the explicitly gated delegate surface. Provider thresholds, wrapper tables,
STOP files, local attempt logs, and task receipts do not belong in the
scheduler. A terminal delegated experiment still closes through the same
WorkEnvelope Receipt + Transition chain as an interactive run.

## Agent-agnosticism proof

Compound proves agent-agnosticism by **checkpoint replacement**: Claude starts
a task → checkpoint → Codex resumes → verification succeeds from spine context
alone. The ContextPack carries everything the next agent needs, regardless of
which harness runs it.

## Troubleshooting

| Symptom | Fix |
|---|---|
| `compound: command not found` in hook | Ensure `~/.local/bin` is on PATH |
| Hook returns empty context | Run `compound home doctor` — home may not be initialized |
| Policy always denies | Check `compound policy operator-authority` — operator state may not be imported |
| ContextPack has no memory | Run `compound memory doctor` — memory may be empty (expected on fresh home) |
