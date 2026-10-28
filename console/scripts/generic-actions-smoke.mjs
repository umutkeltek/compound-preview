/**
 * Executes (not just typechecks) `generic-actions.selftest.ts` — the depth
 * test for Phase 3a (Plans/i-am-not-happy-rosy-floyd.md Part 4, "Closing the
 * 91-item action gap"). Run with `bun`, not plain `node`:
 * `generic-actions.ts` imports `argvMatchesAllowlist` from
 * `@/lib/decision-registry` via the `@/` path alias, which `node
 * --experimental-strip-types` (the runner `compound proof-check` uses for
 * the other console smoke scripts, e.g. mutation-guard-smoke.mjs) cannot
 * resolve — it only strips types, it does not do tsconfig path-alias
 * resolution. Bun resolves `@/` from tsconfig `paths` natively. Joining the
 * proof-check gate list is a Go change (cmd/compound/proof_check.go), out of
 * scope for this phase — note it there when this script is wired in
 * (view-registry-smoke.mjs left the same note for the same reason).
 */
import { runGenericActionsSelfTest } from "../src/lib/generic-actions.selftest.ts";

const result = runGenericActionsSelfTest();
for (const line of result.details) {
  console.log(line);
}

if (!result.ok) {
  console.error("generic-actions smoke FAILED");
  process.exit(1);
}

console.log(
  "generic-actions smoke ok: shell-form/un-allowlisted/mismatched-verb argv rejected at the right gate; well-formed allowlisted actions (item- and group-scoped) resolve with fresh argv",
);
