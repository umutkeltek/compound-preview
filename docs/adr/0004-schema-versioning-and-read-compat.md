# ADR 0004: Schema Versioning & Read-Compatibility Policy

Status: accepted

## Context

Compound mints many self-describing artifacts, each carrying a `schema_version`
string of the form `compound.<domain>_<kind>.v0.N` (activity observations,
source manifests, distiller plans, executor-lens reports, status surfaces, …).
Direct inspection found the versioning discipline was inconsistent:

1. **Most schemas are frozen at `v0.1` and drift silently.** Fields are added
   over time without a version change, so the version number communicates
   nothing and a real breaking change has no signal.

2. **Only one family was genuinely versioned.** `source_manifest` carries
   legacy constants (`v0.1`, `v0.2`) and real migration handling up to `v0.3`
   (`internal/sourceobs/source_manifest.go`). It was the exception, not a
   pattern others followed.

3. **Doctrine existed but wasn't applied.** `internal/activity` already bumped
   `v0.1 → v0.2` (ADR 0003) and its allowlist comment said *"adding a key here
   is a schema-level decision; bump SchemaVersion in lockstep"* — yet additive
   keys (e.g. `pane_resolution`) were being added under the existing version
   with no rule for when that is or isn't acceptable.

4. **Code contradicted its own contract.** `internal/activity/schema.go` pinned
   the decoder to an exact version (`ev.SchemaVersion != SchemaVersion` →
   refuse), while `activity.go` promised *"older v0.1 spool files are still
   readable."* Both cannot be true. The latent consequence: the next honest
   bump (to `v0.3`) would make the exact-pin decoder **refuse every existing
   `v0.2` spool line on upgrade** — silent data loss at drain. This is the
   complexity the policy must prevent before more schemas accrete.

## Forces

- **Fail-closed is doctrine.** The strict decoder refuses unknown fields, kinds,
  and meta keys. Version handling must stay fail-closed (refuse the genuinely
  unknown) without becoming fail-against-our-own-past (refuse readable history).
- **Producer and consumer ship together.** Compound is a single binary; spool is
  day/size-rotated. So the read-compat window can be short — but it must be
  explicit and tested, not accidental.
- **Versioning rigor should scale with persistence.** An ephemeral in-process
  struct needs only a constant. An artifact persisted to disk and read by FUTURE
  binaries (spool, spans, ledger, manifests, proof) needs a real contract:
  version constant + decoder rule + legacy handling + a guard test + a changelog.
- **No theater.** Bumping on every additive optional field drives a version to
  `v0.47` with no migrations to show. The policy must distinguish changes that
  demand a bump from changes that only demand a record.

## Decision

### Change classes

Every schema change is classified:

- **PATCH** — additive AND optional (`omitempty`) AND closed-vocabulary AND
  content-free. Backward-compatible: a new reader handles old data unchanged.
  **No version bump.** Must be recorded as a one-line entry under the current
  version in the family's `SCHEMA.md`.
- **MINOR** — additive but load-bearing: a new REQUIRED field, or a reserved
  value gets lit-up semantics (as `window_focused` did in v0.2). **Bump `v0.N`**
  and extend the read-compat window to keep the prior version readable.
- **MAJOR** — breaking: rename, remove, retype, or invert the meaning of an
  existing field/kind. **Bump `v0.N`**, add a legacy version constant, and
  provide explicit migration/read handling (the `source_manifest` pattern).

### The read-compat window (the load-bearing fix)

A decoder never exact-pins the writer version. It consults an explicit, tested
set — `acceptedSchemaVersions` — containing the current version plus every prior
version still considered readable. Implemented for activity in
`internal/activity/activity.go`:

```go
const SchemaVersion          = "compound.activity_observation.v0.2" // writer
const legacySchemaVersionV01 = "compound.activity_observation.v0.1" // read-only
var acceptedSchemaVersions = map[string]bool{
	SchemaVersion:          true,
	legacySchemaVersionV01: true,
}
func AcceptsActivitySchemaVersion(v string) bool { return acceptedSchemaVersions[v] }
```

Adding the new `SchemaVersion` to `acceptedSchemaVersions` is the read-compat
half of any bump. Removing a version is a deliberate "stop reading old spool"
decision (short window is fine because spool rotates).

### Per-family changelog + drift guards

Each persisted family keeps a `SCHEMA.md` (see `internal/activity/SCHEMA.md`)
recording its writer version, read-compat window, and every PATCH/MINOR/MAJOR
change. Three guard tests enforce the contract for activity (the template other
families should adopt):

- `TestAcceptedSchemaVersionsIncludesCurrent` — the writer version is always in
  its own read set (catches "bumped the constant, forgot the decoder").
- `TestDecodeStrictAcceptsPriorVersionInReadCompatWindow` — a prior-version line
  actually decodes (the window is real, not aspirational).
- `TestSchemaChangelogCoversCurrentVersion` — `SCHEMA.md` documents the current
  writer version (catches "bumped without recording it").

### Scope of this ADR

This codifies the policy, fixes the one place where code contradicted its
contract (activity), and establishes the template (constants + window +
`SCHEMA.md` + three guards). It does **not** retroactively rewrite the dozens of
`v0.1` families — that would be churn. Those families adopt the policy at their
next change; ephemeral structs may stay light.

## Consequences

- The next activity bump (v0.3) is safe: the decoder keeps draining v0.2 spool.
- Additive content-free keys (like `pane_resolution`) have a clear, low-ceremony
  path: allowlist + `SCHEMA.md` line, no bump.
- Breaking changes have a clear, high-ceremony path: bump + legacy constant +
  migration + window extension + changelog.
- Version numbers mean something again: a bump signals "readers built for the
  old version may need the window," not "someone touched a struct."
- Other families inherit a copy-paste template instead of inventing ad hoc rules.

## Alternatives Considered

### **Bump on every change, including additive** (rejected)
The letter of the old "bump in lockstep" comment. Rejected: it inflates the
version with no migration content, trains people to ignore bumps, and doesn't
address the actual hazard (the read side). PATCH-class recording achieves
traceability without noise.

### **Exact-pin the decoder and rely on rotation** (rejected)
Keep `!= SchemaVersion` and accept that old spool is refused after a bump,
arguing rotation drains it fast. Rejected: it makes every bump a small data-loss
event and keeps the code/contract contradiction. The window is cheap and honest.

### **SemVer with a stable v1.0 now** (rejected)
Premature. The schema families are still evolving under `v0.x`; declaring
stability would over-promise. The `v0.N` + change-class model keeps the honest
"not yet stable" signal while adding discipline.

## Rollback

The read-compat window is additive: reverting `schema.go` to the exact pin and
removing `acceptedSchemaVersions` restores prior behavior (and the prior latent
bug). The `pane_resolution` key and the three guard tests are independently
revertible. Reversibility: easy.
