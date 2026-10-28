/**
 * Executes (not just typechecks) `memory-data.selftest.ts` — the keep-aside-
 * drain falsifier (work_4033b98c5837): an operator-adjudicated parked standing-
 * law candidate must leave the /memory needs-your-call queue, while an
 * untouched (never-adjudicated) parked decision candidate must still render.
 * Runs under `bun`: memory-data.ts resolves the `@/lib` path alias, which
 * plain `node --experimental-strip-types` cannot.
 */
import { runMemoryDataSelfTest } from "../src/lib/memory-data.selftest.ts";
import "./evidence-card-smoke.mjs";

const result = runMemoryDataSelfTest();
for (const line of result.details) {
  console.log(line);
}

if (!result.ok) {
  console.error("memory-data smoke FAILED");
  process.exit(1);
}

console.log(
  "memory-data smoke ok: operator-adjudicated parks leave the standing-law queue; untouched parks still render",
);
