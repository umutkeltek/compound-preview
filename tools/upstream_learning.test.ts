import { expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { resolve } from "node:path";

test("upstream evidence producer preserves governance, custody, bounds and retries", () => {
  const result = spawnSync("python3", [
    "-m", "unittest", "discover", "-s", "scripts/upstream-learning", "-p", "test_*.py", "-v",
  ], {
    cwd: resolve(import.meta.dir, ".."),
    env: { ...process.env, PYTHONDONTWRITEBYTECODE: "1" },
    encoding: "utf8",
    timeout: 120_000,
    maxBuffer: 4 * 1024 * 1024,
  });
  expect(result.error, result.error?.message).toBeUndefined();
  expect(result.status, result.stdout + result.stderr).toBe(0);
}, 125_000);
