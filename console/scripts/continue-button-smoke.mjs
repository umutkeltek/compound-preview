import { runContinueButtonSelfTest } from "../src/components/continue-button.selftest.tsx";

const result = runContinueButtonSelfTest();
for (const line of result.details) {
  console.log(line);
}

if (!result.ok) {
  console.error("continue-button smoke FAILED");
  process.exit(1);
}

console.log("continue-button smoke ok: unready instances are copy-only and never mount the live act");
