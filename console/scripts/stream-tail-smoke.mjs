/**
 * Executes (not just typechecks) the SSE tail-parser depth test
 * (tail-parser.selftest.ts). The selftest imports "./tail-parser" WITHOUT a file
 * extension, which `node --experimental-strip-types` does not resolve, so this
 * runs under `bun scripts/stream-tail-smoke.mjs` (bun resolves the extensionless
 * .ts import). It is wired into `compound proof-check` as a bun smoke.
 */
import { runTailParserSelfTest } from "../src/lib/stream/tail-parser.selftest.ts";

const result = runTailParserSelfTest();
for (const line of result.details) {
  console.log(line);
}

if (!result.ok) {
  console.error("stream-tail smoke FAILED");
  process.exit(1);
}

console.log(
  "stream-tail smoke ok: partial-line buffering, truncation reset, and the dedupe key hold under adversarial chunking",
);
