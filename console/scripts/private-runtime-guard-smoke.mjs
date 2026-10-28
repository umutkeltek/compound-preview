import assert from "node:assert/strict";
import { readdir, readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const consoleRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const sourceRoot = path.join(consoleRoot, "src");

const ownerExceptions = new Set(["lib/compound.ts", "lib/instance-pin.ts"]);
const forbiddenWriters = /\b(writeFile|appendFile|rm|unlink|rename|mkdir)\b/;

async function sourceFiles(dir) {
  const entries = await readdir(dir, { withFileTypes: true });
  const files = [];
  for (const entry of entries) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      files.push(...(await sourceFiles(full)));
    } else if (entry.isFile() && /\.(ts|tsx)$/.test(entry.name)) {
      files.push(full);
    }
  }
  return files;
}

const violations = [];

for (const file of await sourceFiles(sourceRoot)) {
  const rel = path.relative(sourceRoot, file);
  const source = await readFile(file, "utf8");
  if (ownerExceptions.has(rel)) continue;

  const reachesPrivateRuntime = source.includes("COMPOUND_HOME") || source.includes("docs_private");
  const hasStandardProjectionGate = source.includes("privateFallbacksEnabled");
  if (reachesPrivateRuntime && !hasStandardProjectionGate) {
    violations.push(`${rel}: private runtime read must be gated by privateFallbacksEnabled()`);
  }

  if (rel === "app/api/document/route.ts") {
    if (/\breadFile\b|COMPOUND_HOME|docs_private|DOCUMENT_ROOT/.test(source)) {
      violations.push(`${rel}: deferred document API must not retain a private file-read path`);
    }
    if (!source.includes("direct document reads are unavailable") || !source.includes("status: 403")) {
      violations.push(`${rel}: deferred document API must fail closed with an explicit 403`);
    }
  }

  if (forbiddenWriters.test(source)) {
    violations.push(`${rel}: console source must not write runtime/private files directly`);
  }
}

assert.deepEqual(violations, []);
console.log("private runtime guard smoke ok: direct private reads are absent and fail closed");
