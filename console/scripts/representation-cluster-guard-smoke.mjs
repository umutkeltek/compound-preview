import assert from "node:assert/strict";
import { readdir, readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

/**
 * FR5 (cluster boundary guard) for the representation cluster
 * (packet 2026-07-11-representation-cluster-v1, settled decision #1).
 *
 * THE LAW: `console/src/representation/**` is a volatility cluster behind a
 * frozen interface. Dev lanes loop INSIDE it; the governed act path
 * (acts/policy/receipts) is never churned by it. So the cluster may NEVER
 * import the governed act client directly — every act leaves only as a typed
 * `ActRequest` through the registry seam, dispatched by a consumer OUTSIDE the
 * cluster. No ESLint is configured here, so this filesystem grep is the
 * enforcement (mirrors scripts/ai-boundary-smoke.mjs). It joins the
 * `compound proof-check` console gate list (cmd/compound/proof_check.go).
 *
 * Green-at-birth by construction; teeth are proven by a scratch mutation
 * (add a forbidden import, watch this go red, revert) — see the packet handoff.
 */

const consoleRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const clusterRoot = path.join(consoleRoot, "src", "representation");

// Modules that ARE the governed act path — the cluster must reach none of them
// directly. Matched against the resolved import specifier's tail.
const FORBIDDEN = [
  "lib/governed-action-client",
  "lib/decision-execute",
  "lib/decide-resolution-acts",
  "lib/generic-actions",
  "lib/console-mutations",
];

const IMPORT_SPECIFIER_RE =
  /\bimport\s+["']([^"']+)["']|\bfrom\s+["']([^"']+)["']|\brequire\(\s*["']([^"']+)["']\s*\)|\bimport\(\s*["']([^"']+)["']\s*\)/g;

async function sourceFiles(dir) {
  const entries = await readdir(dir, { withFileTypes: true });
  const files = [];
  for (const entry of entries) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) files.push(...(await sourceFiles(full)));
    else if (entry.isFile() && /\.(ts|tsx)$/.test(entry.name)) files.push(full);
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

/** Normalize `@/lib/x`, `../lib/x`, `../../lib/x` all down to the `lib/x` tail. */
function tail(specifier) {
  return specifier.replace(/^@\//, "").replace(/^(\.\.?\/)+/, "");
}

const violations = [];

for (const file of await sourceFiles(clusterRoot)) {
  const rel = path.relative(consoleRoot, file).split(path.sep).join("/");
  const source = await readFile(file, "utf8");
  for (const specifier of specifiersIn(source)) {
    const t = tail(specifier);
    for (const forbidden of FORBIDDEN) {
      if (t === forbidden || t.startsWith(`${forbidden}/`) || t.endsWith(`/${forbidden}`)) {
        violations.push(`${rel}: representation cluster imports the governed act path "${specifier}" — acts must exit only as an ActRequest`);
      }
    }
  }
}

assert.deepEqual(violations, []);
console.log(`representation cluster guard ok: ${FORBIDDEN.length} governed-act-path modules unreachable from src/representation/** (acts exit only as ActRequest)`);
