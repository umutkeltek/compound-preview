/**
 * Entrypoint-level tests: prove the argument gate is actually WIRED into the two
 * executables, not merely present in reclaim_selectors.ts. The pure contract is
 * covered in reclaim_selectors.test.ts; these spawn the real scripts.
 *
 * Only the pre-IO gate is exercised on purpose. Both scripts reject arguments
 * before they touch a cache, so these stay fast and cannot delete anything.
 */
import { describe, expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { join } from "node:path";

const DAILY = join(import.meta.dir, "daily_toolchain_reclaim.ts");
const BUDGET = join(import.meta.dir, "enforce_ssd_budget.ts");

const runScript = (script: string, args: string[]) =>
  spawnSync("bun", [script, ...args], { stdio: "pipe", timeout: 30_000, encoding: "utf8" });

describe.each([
  ["daily_toolchain_reclaim", DAILY],
  ["enforce_ssd_budget", BUDGET],
])("%s argument gate", (_name, script) => {
  test("a mistyped flag exits 2 instead of silently dry-running", () => {
    const r = runScript(script, ["--aply"]);
    expect(r.status).toBe(2);
    expect(r.stderr).toContain("--aply");
  });

  test("the rejection names the allowed flags so the operator can fix it", () => {
    expect(runScript(script, ["--nonsense"]).stderr).toContain("--apply");
  });

  test("a stray positional argument is rejected too", () => {
    expect(runScript(script, ["/tmp"]).status).toBe(2);
  });

  test("--help exits 0 and prints usage without doing any work", () => {
    const r = runScript(script, ["--help"]);
    expect(r.status).toBe(0);
    expect(r.stdout).toContain("usage:");
  });

  test("rejection happens before any reclaim: nothing is reported as executed", () => {
    // If the gate ran after the steps, this would carry a JSON receipt.
    expect(runScript(script, ["--aply"]).stdout).toBe("");
  });
});
