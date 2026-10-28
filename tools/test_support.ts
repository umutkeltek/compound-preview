import { spawnSync } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const exitCleanups = new Set<() => void>();
process.once("exit", () => {
  const errors: unknown[] = [];
  for (const cleanup of exitCleanups) {
    try {
      cleanup();
    } catch (error) {
      errors.push(error);
    }
  }
  if (errors.length) throw new AggregateError(errors, "test temporary directory cleanup failed");
});

// Allocate in beforeAll/test bodies and release in afterAll. Bun can skip both
// teardown and process exit events on a module-load error, so module evaluation
// must not allocate roots or compile. Standalone scripts get an exit fallback.
// Each scope owns exactly its created roots; no neighboring-prefix scans.
export function createTestTempScope() {
  const roots = new Set<string>();
  let closed = false;
  function cleanup() {
    for (const root of roots) {
      rmSync(root, { recursive: true, force: true });
      roots.delete(root);
    }
    closed = true;
    exitCleanups.delete(cleanup);
  }
  exitCleanups.add(cleanup);
  return {
    dir(prefix: string): string {
      if (closed) throw new Error("test temporary directory scope is closed");
      const root = mkdtempSync(join(tmpdir(), prefix));
      roots.add(root);
      return root;
    },
    cleanup,
  };
}

export function initializeCompoundHome(
  compoundBin: string,
  homePath: string,
): Record<string, string | undefined> {
  const env = { ...process.env, COMPOUND_HOME: homePath };
  const result = spawnSync(compoundBin, ["home", "init"], {
    cwd: process.cwd(),
    env,
    encoding: "utf8",
  });
  if (result.error) throw result.error;
  if (result.status !== 0) {
    throw new Error(
      `${compoundBin} home init failed with status ${result.status}\nstdout:\n${result.stdout}\nstderr:\n${result.stderr}`,
    );
  }
  return env;
}
