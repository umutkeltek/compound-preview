import { runAskTriageSelfTest } from "../src/lib/ask-triage.selftest.ts";
const result = runAskTriageSelfTest();
if (!result.ok) { console.error("ask-triage smoke FAILED\n" + result.details.join("\n")); process.exit(1); }
console.log("ask-triage smoke ok: " + result.details.join("; "));
