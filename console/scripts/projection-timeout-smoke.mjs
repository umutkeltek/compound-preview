import assert from "node:assert/strict";
import { installProjectionDiskProbe, runProjectionDiskProbe } from "./projection-cache-invalidation.selftest.mjs";
import { chmod, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

const scratch = await mkdtemp(join(tmpdir(), "compound-projection-timeout-"));
const bin = join(scratch, "compound-stub");

await writeFile(
  bin,
  `#!/usr/bin/env bun
if (process.argv[2] !== "timeout-escalation") {
  console.error("unexpected stub command");
  process.exit(2);
}
await Bun.sleep(120);
console.log(JSON.stringify({
  generated_at: new Date().toISOString(),
  marker: "extended-caller-succeeded",
  probe: process.argv[3] ?? "",
}));
`,
  "utf8",
);
await chmod(bin, 0o755);

process.env.COMPOUND_BIN = bin;
process.env.COMPOUND_HOME = join(scratch, "home");

try {
  const diskProbe = installProjectionDiskProbe();
  const api = await import("../src/lib/compound.ts");
  await runProjectionDiskProbe(api, diskProbe);
  const { runProjectionTimeoutSelfTest } = await import("../src/lib/projection-timeout.selftest.ts");
  const result = await runProjectionTimeoutSelfTest();
  assert.equal(result.ok, true);
  console.log(`projection-timeout-smoke ok: ${result.details}`);
} finally {
  await rm(scratch, { recursive: true, force: true });
}
