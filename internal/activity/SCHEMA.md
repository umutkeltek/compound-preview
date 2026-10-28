# activity_observation — schema changelog

The change log for the `compound.activity_observation.*` event vocabulary. This
file is the per-family record required by **ADR 0004 (schema versioning &
read-compat)**. The drift-guard test `TestSchemaChangelogCoversCurrentVersion`
fails if the current writer version below is not documented here.

- **Writer version** (what this binary emits): `compound.activity_observation.v0.2`
- **Read-compat window** (what `DecodeStrict` accepts): `v0.2` (current), `v0.1` (legacy)
- Both are defined in `activity.go` (`SchemaVersion`, `acceptedSchemaVersions`).

## Change classes (ADR 0004)

- **PATCH** — additive + optional (`omitempty`) + closed-vocab + content-free.
  No version bump; record an entry under the current version below.
- **MINOR** — additive but load-bearing (a new required field, or a kind gets
  lit-up reducer semantics). Bump `v0.N`; extend the read-compat window.
- **MAJOR** — breaking (rename / remove / retype / meaning inversion). Bump
  `v0.N`; add a legacy constant and explicit migration handling.

## v0.2 (current)

- Lit up `KindWindowFocused` / `KindPermissionLost` reducer semantics (the
  MINOR change that justified the v0.1 → v0.2 bump).
- Tier-1 envelope meta keys: `pane_cwd`, `git_root`, `git_remote`, `pane_type`,
  `agent_kind`, `agent_name`, `workspace_ref`, `surface_ref`, `window_ref`.
- `ForbiddenMetaKeys` extended (Tier-1 content bans): `file`, `command_args`,
  `script`, `initial_command`, `env`, `language`.
- **[PATCH, no bump]** `pane_resolution` meta key added — a closed confidence
  vocabulary (`cmux_ipc` | `aw_event` | `best_effort_process`) recording HOW a
  `pane_cwd` was resolved. Additive, optional, content-free ⇒ no version bump
  per ADR 0004; recorded here for traceability.

## v0.1 (legacy, read-only)

- Slice-1 Tier-0 vocabulary: `app_activated`, `idle_started`, `idle_ended`,
  `screen_locked`, `screen_unlocked`, `capture_gap`. Shape-compatible subset of
  v0.2 (same `Event` struct); kept readable so an upgrade drains existing spool.
