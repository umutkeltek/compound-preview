/**
 * Executes (not just typechecks) `view-registry.selftest.ts` — the depth test
 * for Phase 5 (Plans/i-am-not-happy-rosy-floyd.md Part 6). Run with `bun`, not
 * plain `node`: the selftest resolves through kit `.tsx` components, and
 * `node --experimental-strip-types` (the runner `compound proof-check` uses
 * for the other console smoke scripts) strips types but doesn't transform
 * JSX. Joining the proof-check gate list is a Go change (cmd/compound/proof_check.go),
 * out of scope for this phase — note it there when this script is wired in.
 */
import { runViewRegistrySelfTest } from "../src/lib/view-registry.selftest.ts";

const result = await runViewRegistrySelfTest();
for (const line of result.details) {
  console.log(line);
}

if (!result.ok) {
  console.error("view-registry smoke FAILED");
  process.exit(1);
}

console.log("view-registry smoke ok: stub fixture renders through the registry; malformed/unknown invocations are rejected without throwing");
