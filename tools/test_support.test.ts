import { afterAll, describe, expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { createTestTempScope } from "./test_support.ts";

const temporary = createTestTempScope();
afterAll(() => temporary.cleanup());
const supportURL = new URL("./test_support.ts", import.meta.url).href;

// Each child imports the real helper and exits as a real Bun test process. The
// neighboring directory deliberately shares its prefix with the owned roots.
const cases = [
  ["success", 'test("success", () => expect(true).toBe(true));', 0],
  ["assertion failure", 'test("failure", () => expect(false).toBe(true));', 1],
  ["setup build failure", 'test("registered", () => expect(true).toBe(true));', 1],
  ["module load failure", 'throw new Error("synthetic module load failure");', 1],
] as const;

describe("test temporary directory lifetime", () => {
  for (const [name, body, status] of cases) {
    test(`cleans owned binary roots after ${name}, preserving neighbors and symlink targets`, () => {
      const workspace = temporary.dir("test-temp-lifecycle-fixture-");
      const roots = join(workspace, "roots");
      const neighbor = join(roots, "owned-binary-neighbor");
      mkdirSync(neighbor, { recursive: true });
      writeFileSync(join(neighbor, "keep"), "untouched neighbor");
      const fixture = join(workspace, "lifetime.test.ts");
      writeFileSync(fixture, `
import { afterAll, beforeAll, expect, test } from "bun:test";
import { writeFileSync, symlinkSync } from "node:fs";
import { join } from "node:path";
import { createTestTempScope } from ${JSON.stringify(supportURL)};
const owned = createTestTempScope();
afterAll(() => owned.cleanup());
beforeAll(() => {
const root = owned.dir("owned-binary-");
writeFileSync(join(root, "compound"), "synthetic compiled executable");
symlinkSync(${JSON.stringify(neighbor)}, join(root, "neighbor-link"), "dir");
console.log(JSON.stringify({ root }));
${name === "setup build failure" ? 'throw new Error("synthetic build failure");' : ""}
});
${body}
`);
      const result = spawnSync(process.execPath, ["test", fixture], {
        cwd: process.cwd(),
        env: { ...process.env, TMPDIR: `${roots}/` },
        encoding: "utf8",
        timeout: 10_000,
      });
      expect(result.error).toBeUndefined();
      expect(result.status).toBe(status);
      const line = result.stdout.split("\n").find((line) => line.startsWith('{"root":'));
      if (name === "module load failure") {
        expect(line).toBeUndefined(); // registered setup never allocated a root
      } else {
        expect(line).toBeDefined();
        const { root } = JSON.parse(line!);
        expect(existsSync(root)).toBe(false);
      }
      expect(readFileSync(join(neighbor, "keep"), "utf8")).toBe("untouched neighbor");
    });
  }
});

for (const status of [0, 1]) {
  test(`standalone exit fallback releases owned roots with exit status ${status}`, () => {
    const workspace = temporary.dir("test-temp-standalone-fixture-");
    const roots = join(workspace, "roots");
    mkdirSync(roots);
    const sentinel = join(roots, "owned-binary-neighbor");
    writeFileSync(sentinel, "preserved");
    const fixture = join(workspace, "standalone.ts");
    writeFileSync(fixture, `
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { createTestTempScope } from ${JSON.stringify(supportURL)};
const owned = createTestTempScope();
const root = owned.dir("owned-binary-");
writeFileSync(join(root, "compound"), "synthetic compiled executable");
console.log(JSON.stringify({ root }));
process.exitCode = ${status};
`);
    const result = spawnSync(process.execPath, ["run", fixture], {
      env: { ...process.env, TMPDIR: `${roots}/` },
      encoding: "utf8",
      timeout: 10_000,
    });
    expect(result.error).toBeUndefined();
    expect(result.status).toBe(status);
    const { root } = JSON.parse(result.stdout.trim());
    expect(existsSync(root)).toBe(false);
    expect(readFileSync(sentinel, "utf8")).toBe("preserved");
  });
}
