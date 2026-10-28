/**
 * Executes the E17 filesystem-containment falsifier for the lineage
 * candidate reader. The selftest imports extensionless TypeScript and the
 * console alias, so the proof battery runs this wrapper with Bun.
 */
import { runLineageSelfTest } from "./lineage.selftest.ts";

const result = await runLineageSelfTest();
for (const line of result.details) {
  console.log(line);
}

if (!result.ok) {
  console.error("lineage smoke FAILED");
  process.exit(1);
}

console.log(
  "lineage smoke ok: private candidate reads stay retired for canonical and adversarial ids",
);
