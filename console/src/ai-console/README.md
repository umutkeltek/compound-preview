# src/ai-console — the sandbox

Phase 5 of the console redesign (`Plans/i-am-not-happy-rosy-floyd.md`, Part 6).
`browserAiConsole` calls the read-only `/api/ask` composition seam; the static
`stubAiConsole` remains only as a registry falsifier. No LLM or gateway client is
wired yet.

## Boundary rules

- **This is the only directory that may ever import an LLM SDK or gateway client**
  (e.g. `@tambo-ai/*`, `openai`, `@anthropic-ai/*`, the `ai` package, or a
  `localhost:18765` gateway client). No such import exists yet — `stub.ts` is a
  static fixture. `scripts/ai-boundary-smoke.mjs` enforces this today (no ESLint is
  configured in this console); if ESLint is ever added, replace it with
  `no-restricted-imports` scoped to everywhere except this directory.
- **Nothing outside this directory may deep-import a file inside it.** Import only
  the barrel (`@/ai-console` / `./index`) — never `@/ai-console/stub` or
  `@/ai-console/types` directly. The smoke script enforces this too.
- **This directory never calls `/api/decision/execute`.** An `AiConsole` only
  *proposes*: `ViewProposal.components` resolve through `view-registry.ts`,
  `ViewProposal.actionProposals` render as ordinary governed `DecisionCard`s that a
  human clicks. The mutation gate (DEC-0021, three-gate `decision-execute.ts` path)
  is unchanged and unaware this directory exists.
- **The registry owns the act; this sandbox only composes the view.** `view-registry.ts`
  lives outside this directory precisely so it survives if this directory is deleted.

## Removal procedure

If this experiment doesn't pan out: delete this directory, unmount the one
`<AiConsolePanel>` (a later phase — not mounted anywhere yet in Phase 5), drop the
lint/smoke allowance, and remove any LLM/gateway deps this directory pulled in.
`view-registry.ts` and the component kit are unaffected — they were never inside
this boundary.
