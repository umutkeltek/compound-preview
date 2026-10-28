/**
 * Runs the delegated-session console slice's pure normalize + render falsifiers
 * (packet: delegated-session console slice). Pure TS mappers, so
 * `bun scripts/delegated-sessions-smoke.mjs` runs them directly (resolves the
 * @/ tsconfig paths). Exits non-zero on any FAIL so the battery can gate on it.
 */
import { runDelegatedSessionsSelfTest } from "../src/components/agents/delegated-sessions.selftest.ts";

const result = runDelegatedSessionsSelfTest();
for (const line of result.details) console.log(`[delegated-sessions] ${line}`);
if (!result.ok) {
  console.error("delegated-sessions smoke FAILED");
  process.exit(1);
}
console.log("delegated-sessions smoke ok: supervised-session board is safe, empty-clean, and honest");
