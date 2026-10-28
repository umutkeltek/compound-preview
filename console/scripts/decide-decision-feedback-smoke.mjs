/**
 * Executes (not just typechecks) `decide-decision-feedback.selftest.ts` —
 * falsifiers for the Decide-page empty-required-field silent no-op and the
 * bare "Owner action unavailable" label, and the final confirmation's exact
 * submitted-value review (work_6047d1a871ed slice 1). Runs
 * under `bun`, not plain `node --experimental-strip-types`: the selftest
 * resolves `@/lib`-style-adjacent relative imports into governed-action-
 * client.ts, which itself carries an `import type` from decision-registry.ts
 * (erased at build, so it stays client-bundle-safe) — same reason
 * generic-actions-smoke.mjs and decide-model-brief-smoke.mjs run under bun.
 * Wired into `compound proof-check` as `console-decide-decision-feedback-smoke`
 * (a bun smoke; see cmd/compound/proof_check.go).
 */
import { runDecideDecisionFeedbackSelfTest } from "../src/lib/decide-decision-feedback.selftest.ts";

const result = runDecideDecisionFeedbackSelfTest();
for (const line of result.details) {
  console.log(line);
}

if (!result.ok) {
  console.error("decide-decision-feedback smoke FAILED");
  process.exit(1);
}

console.log(
  "decide-decision-feedback smoke ok: required-field feedback, named-owner unavailable copy, and exact final submission values are wired",
);
