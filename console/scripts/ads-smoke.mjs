/**
 * Runs the ads panel's pure data-plane depth tests (mapping + derivation over
 * `compound product ads --json`). Pure TS, so `bun scripts/ads-smoke.mjs`
 * runs them directly (resolves the @/ tsconfig paths). Exits non-zero on any
 * FAIL so the battery can gate on it.
 */
import { runAdsSelfTest } from "../src/lib/ads.selftest.ts";

const result = runAdsSelfTest();
for (const line of result.details) console.log(`[ads] ${line}`);
if (!result.ok) {
  console.error("ads smoke FAILED");
  process.exit(1);
}
console.log("ads smoke ok: the ads panel never renders a ratio, delta, or currency the rows cannot back");
await import("./ads-consumer-smoke.tsx");
