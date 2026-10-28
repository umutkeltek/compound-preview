# compound console

Operator control surface. It is projection-first: cards should prefer read-only
`compound ... --json` output against `COMPOUND_HOME`, not private schema reads.
That constraint keeps it a control surface instead of a second runtime owner.

## Run

```bash
compound console dev --operator    # interactive development on :3777
compound console build --operator  # exclusive production build while :3777 is free
compound console serve --operator  # serve the already-verified production build
compound console status --json     # identity, lifecycle kind, HTTP + asset health
```

Requirements: `compound` binary on PATH, or `COMPOUND_BIN=/path/to/compound`;
the operator launcher resolves and explicitly pins `COMPOUND_HOME`. Package
lifecycle commands refuse to run without an explicit `COMPOUND_HOME`, because
silently turning a developer command into a canonical mutating instance is
unsafe. Use `compound console dev --operator` when choosing the canonical
default is intentional.

`compound console --operator` remains a compatibility alias for `dev`. The
documented package scripts delegate back to these CLI lifecycle owners; no
package script exposes an unmanaged Next lifecycle. The CLI invokes the local
Next binary only after acquiring an endpoint-scoped advisory lock shared by
every checkout. `build` also holds the
canonical `127.0.0.1:3777` listener for the full build, so no second runtime can
start while production artifacts are changing. Development writes
`.next/development`; production build and serve use `.next/production`.

`serve` never builds. It refuses to start unless the production `BUILD_ID` and
the manifests Next requires are present. Build once while the supervised job is
stopped, then let launchd restart only the immutable serve step. The CLI keeps
the lifecycle lock while Next runs and forwards termination to the whole child
process group, so supervision cannot orphan a server on `:3777`.

`status` probes the small read-only `/api/readiness` route for the frozen
instance home hash, explicit pin, lifecycle kind, and current build's asset
references. It fetches each referenced local Next static asset before marking
the service healthy. This check does not wait for the dashboard's home or work
projections: `data_freshness: not_checked` means their state is still unknown.
An older console without the route reports readiness unavailable until its
server is upgraded. States distinguish `stopped`,
`healthy`, `unhealthy`, and `occupied_foreign`.

The existing `console-instance-pin-smoke` proof gate also runs the readiness
manifest regression. To run that same check from this directory:
`node --experimental-strip-types scripts/instance-pin-smoke.mjs`.

The `ai.compound.console` launchd unit is an adapter to this owner. Its command
must invoke the installed CLI (with its configured repo path), rather than
calling `next start` directly:

```bash
exec "${HOME}/.local/bin/compound" console serve --operator \
  --console-dir "/path/to/compound/console"
```

Run `compound console build --operator --console-dir /path/to/compound/console`
before bootstrapping the unit. The CLI owns artifact validation, port and
lifecycle exclusion, the frozen home binding, and the final Next process. The
launchd label owns only process supervision.

Optional local-only gates:

- `compound console --operator` is projection-only. `--mutations` explicitly
  sets `COMPOUND_CONSOLE_MUTATIONS=1`; that environment value is necessary but
  not sufficient for POST mutation routes. The serving process must also start
  with an operator-selected `COMPOUND_HOME` and an out-of-band capability:

  ```bash
  export COMPOUND_HOME="/path/to/operator-home"
  export COMPOUND_CONSOLE_CAPABILITY="$(openssl rand -hex 32)"
  compound console --operator --mutations
  ```

  The operator owns high-entropy generation. The launcher and request gate
  validate only its exact 64-hex-character transport encoding and value.
  Direct extension clients must send that exact value in
  `x-compound-console-capability`. Missing, malformed, or wrong capabilities fail
  closed. The standard browser UI never receives the capability, so it remains
  projection-only even when the unsupported API extension is configured.
  Capability bytes are never rendered in launch JSON, issued by middleware,
  or accepted from cookies.
- Direct private document reads are unavailable. `--private-fallbacks` is
  retained only as an explicit refusal, and the legacy fallback/document-root
  environment variables do not enable a read path. Use a Compound-owned
  projection or a scoped CLI read surface.

## Truth Contract

- Every card declares source freshness. The home memory cards preserve unavailable, incomplete, last-known and live reads; only a valid live empty report represents zero. Stale totals keep their last-successful-read time and do not claim a current weekly change.
- Reads are projection-only. The document API has no private file-read path and
  returns 403; new private data must first reach a Compound-owned projection or
  scoped CLI read surface.
- Most acts surface exact CLI commands you copy. Mutation routes are
  default-off, require `COMPOUND_CONSOLE_MUTATIONS=1` plus an explicit frozen
  `COMPOUND_HOME`, require same-origin localhost POSTs plus the out-of-band
  capability header, and execute through the server-resolved decision
  endpoint. The standard UI remains copy-only.
  Legacy mutation routes are compatibility adapters only.
- If a card cannot name a projection, it does not ship.

## Stable destinations + contextual append

The left rail is the stable seven-surface map defined by
[`DESIGN.md` §10](./DESIGN.md#10-experience-architecture--simple-first-depth-on-demand):
Today, Live, Work, Decide, Knowledge, System, and Outcomes. Ask Compound is a
global read-only overlay.

New projections append inside their context owner. They do not add a top-level
navigation row merely because a command, object, or read model exists. When a
fact appears outside its owner, render only the decision-relevant summary,
freshness, and a link back to the canonical surface.

## Executor Attribution

Work cards show `owner` / `claimed_by` from work envelopes, so the active lane is
visible even when work is merely offered or unclaimed.
