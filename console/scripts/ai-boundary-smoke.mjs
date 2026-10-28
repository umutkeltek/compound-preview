import assert from "node:assert/strict";
import { readdir, readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

/**
 * Boundary smoke for `src/ai-console` (Plans/i-am-not-happy-rosy-floyd.md Part 6
 * / Phase 5 doctrine: "the LLM composes the view; the registry owns the act").
 * No ESLint is configured in this console yet, so this script is the
 * enforcement in place of `no-restricted-imports` until one is added — it
 * should join the `compound proof-check` console gate list
 * (cmd/compound/proof_check.go) once that's wired; that's a Go change, out of
 * scope for this phase.
 *
 * Two rules, enforced over every `src/**\/*.{ts,tsx}` file OUTSIDE
 * `src/ai-console/`:
 *   1. no import of an LLM SDK or gateway client (`@tambo-ai/*`, `openai`,
 *      `@anthropic-ai/*`, the `ai` package) — that's `src/ai-console`'s alone.
 *   2. no deep-import into `src/ai-console` — only its barrel
 *      (`@/ai-console` / `./index`) may be imported.
 */

const consoleRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const sourceRoot = path.join(consoleRoot, "src");
const AI_CONSOLE_DIR = "ai-console";

// Anchored to the start of the specifier so "ai-console" itself never matches
// the bare "ai" package rule (next char after "ai" would be "-", not "/"/end).
const FORBIDDEN_SPECIFIER = /^(?:@tambo-ai\/|@anthropic-ai\/|openai(?:\/|$)|ai(?:\/|$))/;

const IMPORT_SPECIFIER_RE =
  /\bimport\s+["']([^"']+)["']|\bfrom\s+["']([^"']+)["']|\brequire\(\s*["']([^"']+)["']\s*\)|\bimport\(\s*["']([^"']+)["']\s*\)/g;

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

function specifiersIn(source) {
  const found = [];
  for (const match of source.matchAll(IMPORT_SPECIFIER_RE)) {
    found.push(match[1] ?? match[2] ?? match[3] ?? match[4]);
  }
  return found;
}

function touchesAiConsole(specifier) {
  return /(^|\/)ai-console(\/|$)/.test(specifier);
}

/** Only the barrel (`.../ai-console` or `.../ai-console/index`) may cross the boundary. */
function isBarrelImport(specifier) {
  return /\/ai-console$/.test(specifier) || /\/ai-console\/index(\.ts)?$/.test(specifier);
}

const violations = [];

for (const file of await sourceFiles(sourceRoot)) {
  const rel = path.relative(sourceRoot, file).split(path.sep).join("/");
  if (rel === AI_CONSOLE_DIR || rel.startsWith(`${AI_CONSOLE_DIR}/`)) continue; // the sandbox may import what it needs internally

  const source = await readFile(file, "utf8");
  for (const specifier of specifiersIn(source)) {
    if (FORBIDDEN_SPECIFIER.test(specifier)) {
      violations.push(`${rel}: forbidden LLM/gateway import "${specifier}" outside src/ai-console`);
    }
    if (touchesAiConsole(specifier) && !isBarrelImport(specifier)) {
      violations.push(
        `${rel}: deep import "${specifier}" bypasses the src/ai-console barrel (import "@/ai-console" instead)`,
      );
    }
  }
}

assert.deepEqual(violations, []);
console.log("ai-console boundary smoke ok: no LLM/gateway imports and no ai-console deep-imports outside the sandbox");
