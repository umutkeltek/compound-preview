import { runWorkActionRowSelfTest } from "../src/components/work/work-action-row.selftest.tsx";

const result = runWorkActionRowSelfTest();
for (const line of result.details) {
  console.log(line);
}
if (!result.ok) {
  console.error("work-action-row smoke FAILED");
  process.exit(1);
}
console.log("work-action-row smoke ok: unready work transitions are copy-only");
