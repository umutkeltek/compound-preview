# Supported Scope and Security Boundary (Public Alpha)

Status: public-alpha support contract, frozen 2026-08-10.

This page defines the supported authority and runtime scope of the public
alpha. It states the intended boundary as a **target contract**, and it is
careful to separate that target from **current behavior**: where the runtime
has not yet been hardened to enforce the boundary, the gap is named below as
a current blocker, not described as if it were already closed. Everything
outside this boundary is unsupported, not merely undocumented. The
architecture doctor pins these statements; runtime hardening lands the
enforcement in focused commits, and this contract is the boundary those
commits prove.

## Supported target contract

### File backend only

The file backend is the only supported storage backend. All durable state
lives in local files under `COMPOUND_HOME`. Postgres (`COMPOUND_STORE=postgres`,
the `compound store` surface, the ADR-0005 experiment) is an explicit
experimental fence: it is never authority, it is unsupported in the public
alpha, and it must not carry durable alpha state. Runtime admission requires a
second exact opt-in (`COMPOUND_EXPERIMENTAL_POSTGRES=1`) plus a nonsecret
`COMPOUND_POSTGRES_PROFILE`; reports and authority objects carry only that
opaque profile reference, never a raw or "masked" DSN. Unknown or partial
backend selection fails closed without falling back to the file recall lane.

### One trusted OS user on local disk

The trust model is one trusted OS user and local disk. Compound's private
custody is `0700` directories and `0600` files owned by that user. Compound
is not a defense against another local user on the same account, a shared
account, or a shared disk.

### Explicit or enrolled home authority

The supported home authority target is explicit: the `--home` flag, the
`COMPOUND_HOME` environment variable, a command's explicit local selection
(for example `compound home --local`), or a home the user enrolled.

Ambient ancestor `.compound/manifest.yaml` discovery is not an authority target:
a cloned repository or ancestor directory does not select policy, credentials,
transports, storage, or harness authority merely because a manifest file
exists. Pin `COMPOUND_HOME`, `--home`, or an explicit local selector for
mutating commands that should operate on a repository-local `.compound` home.

### Hooks: observation by default, enforcement by proof

Interactive hooks may observe: the default interactive posture records
ActionIntent/PolicyDecision evidence and makes no containment claim.
Enforcement claims require behavioral proof — an explicitly enforced isolated
profile plus a nonce-bound behavioral probe (`compound doctor`) for the exact
launch profile. A configured binary or installed hook is not mutation-ready
by configuration alone. A harness that cannot synchronously deny before tool
execution (currently OpenCode) is never mutation-ready.

### Console: projection-only by default

The console is a local projection of spine objects for the single trusted
user. The supported target is projection-only. The standard browser never
receives or mints the server capability; mutations and private reads remain
unavailable in the supported default. Unsupported mutation API extensions
require an operator-selected home and an out-of-band capability header in
addition to their explicit flag and local-origin checks. The operator owns
high-entropy generation; Compound validates the exact 64-hex transport format
and value. Direct private document reads are
unavailable; the legacy flag and document-root environment cannot enable them.
The supported console is not a shared, remote, or proxied surface.

## Current blockers (behavior not yet hardened)

The target contract above is the boundary; the runtime does not enforce all
of it yet. These gaps are named so the docs do not claim a fix already
landed. Each is closed by a focused hardening commit, and until it is closed
the target contract is a commitment, not a description of current behavior.

The strict-resolver hardening (closing the ambient ancestor authority blocker)
has landed: authoritative `Resolve` no longer adopts an ancestor `.compound`
marker. Ancestor discovery remains available only as a non-authoritative
read-only compatibility API; mutating commands require an explicit
`--home`, `COMPOUND_HOME`, or `--local` selection.

- Enforcement is observed, not yet behaviorally proven per launch profile. A
  configured binary is not mutation-ready without a fresh nonce-bound
  behavioral probe for the exact profile; until that proof exists for a
  profile, treat it as observation-only.

## Unsupported

Unsupported in the public alpha:

- Postgres or any non-file backend for durable state;
- shared homes, multi-user homes, or homes on shared/network filesystems;
- server, daemon, or multi-tenant deployments;
- remote or proxied console access — exposing the console beyond the local
  machine is out of scope;
- delegated launch, steer, progress, and mutating delegate sweep — these are
  experimental, disabled by default, and outside the v0.1 supported execution
  contract; `work advance --execute` is the supported governed path;
- `work sweep-stale --yes` mutation — it is experimental, disabled by default,
  requires `--experimental-sweep-stale-mutations`, and remains outside the v0.1
  supported contract; the default and `--dry-run` faces are read-only reports;
- unattended memory promotion, registry import through `memory
  promote-driver`, and lane-belief mutation are experimental and outside the
  v0.1 supported contract; the supported promote-driver apply is frozen
  candidate-only and registry flags are plan-only;
- arbitrary private embedding endpoints — memory engines are local sidecars,
  and private corpora require a private, local/loopback lane posture.

Running outside the supported scope is not a supported configuration, and
posture observed there is not a defect report against this contract.

## Boundary discipline

This contract moves only through receipted changes: a supported-scope change
lands together with the runtime proof that the boundary holds, and the
architecture doctor fails when these statements drift out of the public
docs. When a current blocker is closed, the hardening commit that closes it
updates this page in the same change, and the doctor's required phrases move
with it.
