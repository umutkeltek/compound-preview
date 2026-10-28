# ADR 0003: ActivityWatch Virtual Stream + Tier-1 Envelope

Status: accepted

## Context

The operator runs ActivityWatch (AW) locally on their Mac. Two problems surfaced
from direct inspection of AW's data:

1. **Categorizer holes.** AW's regex-based categorizer missed `cmux` (the
   operator's primary coding terminal), `Brave Browser`, `Google Chrome`, and
   `WhatsApp` (whose app string carried an invisible U+200E prefix). A local
   repro on the operator's current bucket showed 89.5% of tracked time landing
   in `Uncategorized`. The remaining 12.1% resolved to system-level events
   (`SecurityAgent`, `Stream Deck`, `UserNotificationCenter`) that were 90%+
   AFK-filtered anyway.

2. **Hostname fragmentation.** The Mac was renamed six times in nine months.
   AW creates a new bucket per hostname, so the AW webui only shows data for
   the current hostname. The operator's view was "nothing is being tracked"
   while events were flowing under seven other hostnames.

Compound's existing `internal/activity` package captures app focus / idle / lock
via a Swift launchd agent — its own collector, content-free by schema. AW is a
*parallel* activity source the operator already has running, with much richer
event data (URLs, titles, project paths for editors) but a content-bearing
schema. The Tier-0 ambient-activity family is content-free by doctrine; AW is
not, by design.

Two capabilities are needed:

- **A virtual stream** that joins AW events across hostnames into a single
  normalized Tier-0 stream compound can ingest. Useful on its own, independent
  of Tier-1.
- **A Tier-1 envelope collector** that captures per-focused-window
  `pane_cwd` / `git_remote` / `agent_kind` from terminals — content-free (no
  `title`/`url`/`document`), multi-terminal (cmux, iTerm2, Warp, Terminal.app,
  VS Code/Cursor) so other people with the same problem can use it too.

Both fit compound's existing `internal/activity` Tier-0 ambient-activity
schema family. `KindWindowFocused` and `KindPermissionLost` are already reserved
slots — schema-valid but no-op in the slice-1 reducer. This ADR lights them up.

## Forces

- **No Go daemons.** Doctrine: "a blind cron is a liar with a schedule."
  Tier-1 collection must be a bounded CLI verb, not a long-running process.
- **Content-free by schema AND by strict-decode.** The Tier-0 schema has no
  `title`/`url`/`document` field; the strict decoder refuses any meta key in
  `ForbiddenMetaKeys`. Tier-1 inherits the same invariant and extends it: file
  paths, command lines, env vars are also forbidden.
- **One source of truth for forbidden keys.** Both Tier-0 (schema-level F5)
  and Tier-1 (runtime invariant) consult the same `ForbiddenMetaKeys` set.
  Duplication invites drift; the strict decoder is the load-bearing gate.
- **Public/private boundary.** The repo is destined to be public. Tests use
  generic hostnames (`host-a.example`), generic paths
  (`/Users/example/projects/example`), generic org names
  (`git@example.com:operator/example.git`). No `umut`, no real org names, no
  real hostnames.

## Decision

### Architecture

```
                   compound (no Go daemons — bounded CLI commands)
                                      │
        ┌─────────────────────────────┼──────────────────────────┐
        │                             │                          │
        ▼                             ▼                          ▼
  ┌─────────────┐             ┌──────────────┐           ┌─────────────────┐
  │ awstream     │  ────────► │ tier1/       │ ────────► │ observed/       │
  │ (joins AW    │  Tier-0    │ collector    │ Tier-1    │ activity/       │
  │  across      │  events    │  ──────────  │ events    │ segments|spans/ │
  │  hostnames)  │            │  per-app     │           │ <day>.ndjson    │
  └─────────────┘             │  sources:    │           │ <day>.json      │
                              │   cmux IPC   │           └─────────────────┘
        ▲                     │   iterm2 lsof│
        │                     │   warp lsof  │
        │                     │   term lsof  │
        │                     │   vscode AW  │
        │                     └──────────────┘
        │
   AW buckets (8 hostnames), reads via :5600 REST
```

Three new packages live under `internal/activity/`:

1. **`internal/activity/awstream/`** — read-only AW HTTP client and a
   `Normalize()` function that joins events across hostnames. Joins on
   `(bucket_type, timestamp)` and emits deterministic
   `(activity.Event, envelope.HostBindings)` pairs. Drops any event whose data
   payload carries a banned meta key at the read boundary (F5).

2. **`internal/activity/tier1/source/`** — per-app envelope source registry.
   `EnvelopeSource` interface with `AppBundleID() / TryEnvelope(ctx, focused) /
   ContentFreeInvariant()`. Built-in sources: cmux (IPC fast path), iTerm2 /
   Warp / Terminal.app (AppleScript + `lsof` cwd probe), VS Code / Cursor
   (consume AW `aw-watcher-vscode_*` events for `project` only — never `file`
   which is content-bearing). Every source's `ContentFreeInvariant` is checked
   at startup.

3. **`internal/activity/tier1/`** — runtime wiring. `Collector.Run(ctx, duration)`
   reads AW window events, resolves the focused app's envelope, derives
   `git_remote` via `git -C <cwd> config --get remote.origin.url` (cached per-cwd
   5 min), writes Tier-1 events as `KindWindowFocused` with envelope meta keys.
   `Collect(home, ..., workID, duration, now)` is the three-gate governance
   wrapper mirroring `Drain`: enabled → policy/grant → byte cap.

### Two CLI verbs, capability-exact

- `compound activity aw materialize` — reads AW across hostnames, writes
  Tier-0 spool events. Governed by the existing `activity.ingest.drain`
  capability.
- `compound activity tier1 collect` — bounded collect of Tier-1 envelopes
  (default `--duration 30s`). Governed by the new `activity.tier1.collect`
  capability. Refused without the standing grant — F15.

A grant for `activity.ingest.drain` does NOT authorize Tier-1. A grant for
`activity.tier1.collect` does NOT authorize AW materialize (or any other
Tier-0 ingest). Capability strings ARE the security boundary; aliases would
collapse governance.

### Schema version bump

`SchemaVersion` moved from `v0.1` to `v0.2` in this commit. v0.2 introduces:

- `KindWindowFocused` lit up in the slice reducer (Phase 5): same coalesce
  rule as `KindAppActivated` — same bundle = stay in foreground (keep earlier
  start); different bundle = close the prior and open a new rawSpan.
- `KindPermissionLost` already produced `coverage=incomplete`; Phase 5
  confirmed behavior.
- `ForbiddenMetaKeys` extended with 6 Tier-1 keys: `file`, `command_args`,
  `script`, `initial_command`, `env`, `language`. Single source of truth, two
  enforcement points (strict decoder + `CheckEnvelopeContentFree`).

`Tier1AllowedMetaKeys` (a sibling of `ForbiddenMetaKeys`, in
`tier1/tier1.go`) lists the meta keys the Tier-1 collector may populate:
`pane_cwd`, `git_root`, `git_remote`, `pane_type`, `agent_kind`, `agent_name`,
`workspace_ref`, `surface_ref`, `window_ref`. The compile-time invariant guard
`TestTier1SetsDoNotOverlap` catches any future drift between the two sets.

### Imports allowlist extended

`internal/activity/imports_test.go` (F4+F6) walked only `.` (the parent package).
Phase 5 extended the walk to recurse one level into `awstream/`, `tier1/`,
`tier1/source/` so the F4 (no auto-candidate minting) and F6 (no eventstore
writes) doctrine applies to subpackages. Subpackages of subpackages are out of
scope — those are different doctrine boundaries.

### Doctrine consistency

- **No Go daemon.** Tier-1 is bounded by `--duration`. F17 enforces the
  wall-clock budget (`--duration 100ms` invocation completes within 1s).
- **No content leakage.** Three layers: schema (no field), strict decoder
  (refuses banned meta keys), per-source `ContentFreeInvariant` runtime
  self-test.
- **F12 deterministic.** `awstream.Normalize` is byte-identical across runs
  for the same input. The Tier-1 collector appends only; re-running produces
  duplicate spool lines (Drain dedupes via cursor).
- **No F4 / F6 violation in subpackages.** F16 closed this gap.

## Consequences

- Compound can ingest AW data with the same `compound activity drain` flow
  used for the Swift launchd collector. Operators with AW on their machine
  don't need to install the Swift collector — they can use AW alone.
- Per-project breakdown becomes possible: "Work / Programming / compound"
  vs "Work / Programming / activity-watch-tier1" vs "Research / other-repo"
  is now resolvable from cmux pane metadata, not bundle id alone.
- Agent-kind attribution refines from coarse ("agent-session") to vendor
  (`claude` vs `codex`) in future phases. v0.2 ships `AgentKind = "agent-hook"`
  — cmux's coarse label. Refinement deferred until command-basename parsing is
  approved (content-adjacent).
- ActivitySpan still carries only bundle-level metadata. Phase 5 lit up the
  reducer but didn't propagate envelope meta onto the span. Phase 6+ could add
  `ActivitySpan.Envelope` so downstream consumers see per-span project context
  without re-reading the spool.
- The cross-hostname join is wired into `aw materialize` and the AW reader
  layer, but the Tier-1 `Collector.Run` still reads the FIRST matching
  `currentwindow` bucket. Full integration: drain the materialize spool first,
  then run Tier-1 — documented in the verb's `--help`.

## Alternatives Considered

### **Long-running Tier-1 daemon** (rejected)
A Go daemon that polls every N seconds, like the Swift launchd agent. Rejected:
violates compound's "no Go daemons" doctrine, introduces liveness concerns
(what if the daemon crashes?), and complicates `compound proof-check` (the
daemon's behavior can't be verified in CI).

### **Run Tier-1 in cmux only** (rejected)
The plan considered cmux-only initially. Rejected because the user wants
multi-terminal coverage so others with iTerm2 / Warp / Terminal.app can use it
too. The `lsof` fallback is portable; cmux IPC is the only special case.

### **Reuse AW's categorizer** (rejected)
AW's regex engine can't tell `iTerm2 in compound repo` from `iTerm2 in another
repo` — it has no project-envelope signal. Tier-1 fills this gap by reading
cwd/git remote from the OS layer, which AW does not capture.

### **Store env vars in Tier-1 envelopes** (rejected)
Some sources emit `CMUX_*` env vars that identify the agent kind more
specifically. Rejected: env vars are content-bearing (secrets can leak), and
the doctrine forbids them.

### **Schema-bump the spans version too** (rejected)
`SpansSchemaVersion = "compound.activity_spans.v0.1"` is unchanged. Span
reduction is shape-stable; the v0.1 → v0.2 bump is for the *event* vocabulary
(added handler semantics), not the span shape.

## Rollback

Phase 7+ deliverables are additive and gated by the three falsifiers (F13
content-free, F15 grant gate, F16 subpackage imports, F17 bounded duration).
Reverting the Tier-1 subpackages and the `tier1.*` CLI verbs leaves the
Tier-0 schema-version bump intact — a v0.2 schema is still readable by any
v0.2 reader. Restoring the v0.1 ForbiddenMetaKeys set (5 keys, no Tier-1
extensions) is a single-line change in `internal/activity/activity.go`.

Reversibility: easy. Tier-1 ships behind its own capability constant; removing
the constant and the verb dispatch leaves compound unchanged.
