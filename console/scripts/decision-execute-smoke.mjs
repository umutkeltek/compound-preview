/**
 * Executes (not just typechecks) `decision-execute.selftest.ts` — the
 * execute-route ↔ decide-model agreement falsifiers (a bespoke owner with zero
 * compatible options stays unavailable; generic actions are ownerless only;
 * lifecycle revisions and reserved-resume argv remain version-bound). Runs under
 * `bun`: decision-execute/decision-registry resolve `@/lib` path aliases,
 * which plain `node --experimental-strip-types` cannot.
 */
import { runDecisionExecuteSelfTest } from "../src/lib/decision-execute.selftest.ts";
import { runSelectedContinueSelfTest } from "../src/lib/work-selected-continue.selftest.ts";

const result = await runDecisionExecuteSelfTest();
for (const line of result.details) {
  console.log(line);
}

if (!result.ok) {
  console.error("decision-execute smoke FAILED");
  process.exit(1);
}

console.log(
  "decision-execute smoke ok: owner effect boundaries, revision conflicts, refs, and reserved resume are pinned",
);
const selectedContinue = runSelectedContinueSelfTest();
for (const line of selectedContinue.details) console.log(line);
if (!selectedContinue.ok) {
  console.error("decision-execute smoke FAILED: selected-work continuation");
  process.exit(1);
}
