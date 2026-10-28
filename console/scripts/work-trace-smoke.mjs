/**
 * Executes (not just typechecks) the envelope-spine depth test
 * (work-trace.selftest.ts). The mapper is pure TS (no JSX), so plain `node`
 * with `--experimental-strip-types` runs it, matching the other console smoke
 * scripts; `bun scripts/work-trace-smoke.mjs` works too.
 */
import { runWorkTraceSelfTest } from "../src/lib/work-trace.selftest.ts";

const result = runWorkTraceSelfTest();
for (const line of result.details) {
  console.log(line);
}

if (!result.ok) {
  console.error("work-trace smoke FAILED");
  process.exit(1);
}

console.log("work-trace smoke ok: envelope spine maps honestly — one beating node, no fabricated stages");
