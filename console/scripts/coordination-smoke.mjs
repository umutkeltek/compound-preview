/**
 * Runs the coordination view's data-plane depth tests — the pure builder over
 * `compound coordination --json` (compound.coordination.v0.1) that feeds the
 * Me / Others / Waiting-on surface. Pure TS mapper, so `bun
 * scripts/coordination-smoke.mjs` runs it directly (resolves the @/ tsconfig
 * paths). Exits non-zero on any FAIL so the battery can gate on it.
 */
import { runCoordinationSelfTest } from "../src/lib/coordination.selftest.ts";

let ok = true;
const suites = [["coordination", runCoordinationSelfTest()]];
for (const [name, result] of suites) {
  for (const line of result.details) console.log(`[${name}] ${line}`);
  if (!result.ok) {
    console.error(`[${name}] selftest FAILED`);
    ok = false;
  }
}
if (!ok) {
  console.error("coordination smoke FAILED");
  process.exit(1);
}
console.log("coordination smoke ok: Me / Others / Waiting-on data plane is honest");
