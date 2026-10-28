# Console + Hook Hardening Implementation Plan

Date: 2026-07-05

## Scope

This packet implements the confirmed hardening work from the local cross-check of the external audit:

- Gate every console mutation behind an explicit local operator opt-in.
- Collapse legacy console mutation routes into the generic decision executor seam.
- Stop `SessionEnd` from treating harness shutdown as task completion unless explicit close proof is supplied.
- Add console and TypeScript edge checks to `compound proof-check`.
- Retire console direct runtime/private readers until a rooted no-follow owner exists.

## Evidence

- `console/src/app/api/decision/execute/route.ts` is the intended generic mutation endpoint.
- `console/src/app/api/work/act/route.ts` and `console/src/app/api/reviews/record/route.ts` still owned separate mutation paths.
- `console/src/lib/decision-registry.ts` exposed invalid move type `landed_owner`.
- `internal/hook/hook.go` defaulted `SessionEnd` with `work_id` to `completed` and `audit_only`.
- `cmd/compound/proof_check.go` did not include console TypeScript, console smoke, or Bun tool tests.
- Console private readers still reached into `COMPOUND_HOME` and `docs_private` directly.

## Delivery Plan

1. Console mutation gate and executor seam
   - Add one server-side mutation guard.
   - Require `COMPOUND_CONSOLE_MUTATIONS=1`.
   - Require same-origin local browser POSTs.
   - Require an operator-generated, out-of-band
     `COMPOUND_CONSOLE_CAPABILITY` header gate for every scripted mutation
     client; validate its 64-hex transport encoding without claiming that
     formatting proves entropy, and never mint or cookie it through the
     standard UI.
   - Move decision execution into a reusable server helper.
   - Make `/api/work/act` and `/api/reviews/record` compatibility adapters, not separate mutation owners.

2. SessionEnd close semantics
   - `SessionEnd` with only `session_id` remains event-only.
   - `SessionEnd` with `work_id` but missing explicit `outcome` or `move_type` becomes event-only with next action text.
   - Terminal close still works when explicit `outcome` and `move_type` are supplied and the work store allows it.
   - Add regression coverage for no-default-close.

3. Proof-check coverage
   - Add console `tsc --noEmit`.
   - Add console smoke script.
   - Add Bun tool tests when Bun is available.
   - Keep missing Bun as an explicit skipped check, not a silent pass.

4. Private fallback retirement
   - Keep CLI projections as the default.
   - Remove the document API's file-read path and refuse the legacy private-fallback opt-in until a rooted no-follow owner exists.

## Acceptance

- Legacy console mutation routes cannot mutate with console mutations disabled.
- All console mutation routes reject non-local cross-origin POSTs.
- Mutation-mode launch refuses an implicit home or a missing/malformed
  out-of-band capability before emitting a launch plan.
- The real decision route rejects missing/wrong capabilities before JSON
  parsing and admits the exact capability to the governed executor.
- Generic decision route remains the only mutation executor path.
- `landed_owner` is no longer a console move-type option.
- `SessionEnd` no longer closes work by default.
- Explicit `SessionEnd` close still emits receipt and transition.
- `compound proof-check --dry-run --json` lists console and tool checks.
- Go tests, console TypeScript, console smoke, Bun tool tests, and proof-check dry-run pass locally.

## Implemented

- Added `console/src/lib/console-mutations.ts` for default-off mutation gating.
- Added `console/src/lib/decision-execute.ts` as the single server-side decision executor.
- Converted `/api/work/act` and `/api/reviews/record` into guarded compatibility adapters.
- Updated Work and Review forms to call `/api/decision/execute` directly.
- Removed `landed_owner` from console move-type options.
- Changed `SessionEnd` so `work_id` without explicit `outcome` and `move_type` records an event only.
- Added hook regression coverage for no-default-close.
- Added proof-check gates for console TypeScript, console smoke, console mutation-gate smoke, and Bun tool tests.
- Retired direct console runtime/private readers; the document API now returns 403 and the legacy opt-in is refused.

## Proof Run

- `GOTOOLCHAIN=local go test ./...`
- `GOTOOLCHAIN=local go vet ./...`
- `GOTOOLCHAIN=local go build ./...`
- `cd console && node_modules/.bin/tsc --noEmit && node scripts/mutation-guard-smoke.mjs && node scripts/smoke.mjs`
- `bun test tools/*.test.ts`
- `gofmt -l $(git ls-files '*.go')`
- `git diff --check`
- `GOTOOLCHAIN=local go run ./cmd/compound architecture doctor --strict`
- `GOTOOLCHAIN=local go run ./cmd/compound proof-check --dry-run --json`
- `redact --repo-visibility public --from-file <changed files>` reported no findings.

Non-dry `proof-check` was not run because it records a proof artifact into
`COMPOUND_HOME`; this packet stayed inside source/tests/docs.

## Boundaries

- No live `COMPOUND_HOME` mutation beyond read-only verification commands.
- No public/external side effects.
- No social, ASC, GSC, credential, scheduler, launchd, or posting work.
- No CLI rewrite beyond the proof-check step list needed for this packet.
