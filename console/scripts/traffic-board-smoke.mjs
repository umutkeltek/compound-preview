/**
 * Runs the real-time traffic board's data-plane + live-fold depth tests
 * (packet 2026-07-20-realtime-multiagent-traffic-board). Pure TS mappers, so
 * `bun scripts/traffic-board-smoke.mjs` runs them directly (resolves the @/
 * tsconfig paths). Exits non-zero on any FAIL so the battery can gate on it.
 */
import { runRunAttemptsSelfTest } from "../src/lib/run-attempts.selftest.ts";
import { runTrafficSelfTest } from "../src/lib/traffic.selftest.ts";
import { runUseLiveFleetSelfTest } from "../src/lib/stream/useLiveFleet.selftest.ts";

let ok = true;
const suites = [
  ["run-attempts", runRunAttemptsSelfTest()],
  ["traffic", runTrafficSelfTest()],
  ["useLiveFleet", runUseLiveFleetSelfTest()],
];
for (const [name, result] of suites) {
  for (const line of result.details) console.log(`[${name}] ${line}`);
  if (!result.ok) {
    console.error(`[${name}] selftest FAILED`);
    ok = false;
  }
}
if (!ok) {
  console.error("traffic-board smoke FAILED");
  process.exit(1);
}
console.log("traffic-board smoke ok: real-time board data plane + live fold are honest");
