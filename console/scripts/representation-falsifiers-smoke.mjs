/**
 * Executes (not just typechecks) the representation cluster falsifiers FR1-FR4 + FR6
 * (packet 2026-07-11-representation-cluster-v1). Run under `bun`: the shapes are
 * `.tsx` and FR4 renders to static markup via `react-dom/server`, which
 * `node --experimental-strip-types` cannot do (no JSX transform). Joins the
 * `compound proof-check` console gate list (cmd/compound/proof_check.go).
 */
import { runRepresentationFalsifiers } from "../src/representation/falsifiers.selftest.ts";

const result = runRepresentationFalsifiers();
for (const line of result.details) {
  console.log(line);
}

if (!result.ok) {
  console.error("representation falsifiers smoke FAILED");
  process.exit(1);
}

console.log("representation falsifiers smoke ok: FR1 (receipt-or-error chip), FR2 (home number == actionable), FR3 (predicate labelled as goal), FR4 (a11y-reachable affordance), FR6 (proof-backed change without assumed benefit)");
