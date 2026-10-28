import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const consoleRoot = path.resolve(scriptDir, "..");
const repoRoot = path.resolve(consoleRoot, "..");
const envelopeSource = await readFile(path.join(repoRoot, "internal/envelope/envelope.go"), "utf8");
const glossarySource = await readFile(path.join(repoRoot, "internal/envelope/glossary.go"), "utf8");
const moveTypesSource = await readFile(path.join(consoleRoot, "src/lib/move-types.ts"), "utf8");
const glossaryModuleSource = await readFile(path.join(consoleRoot, "src/lib/glossary.ts"), "utf8");
const registrySource = await readFile(path.join(consoleRoot, "src/lib/decision-registry.ts"), "utf8");
const decisionExecuteSource = await readFile(path.join(consoleRoot, "src/lib/decision-execute.ts"), "utf8");

function parseGoMoveTypes(source) {
  const match = source.match(/MoveTypes\s*=\s*\[\]string\{([^}]+)\}/m);
  assert.ok(match, "internal/envelope MoveTypes enum not found");
  return [...match[1].matchAll(/"([^"]+)"/g)].map((m) => m[1]);
}

function parseGoMoveTypePlain(source) {
  const match = source.match(/MoveTypePlain\s*=\s*map\[string\]string\{([\s\S]*?)\n\}/m);
  assert.ok(match, "internal/envelope/glossary.go MoveTypePlain map not found");
  // Each entry is on its own line: "<key>": "<value>",  — strip the trailing comma.
  const out = {};
  for (const entry of match[1].matchAll(/^\s*"([^"]+)"\s*:\s*"([\s\S]*?)",?\s*$/gm)) {
    out[entry[1]] = entry[2];
  }
  return out;
}

function parseGoMoveTypeLabel(source) {
  const match = source.match(/MoveTypeLabel\s*=\s*map\[string\]string\{([\s\S]*?)\n\}/m);
  assert.ok(match, "internal/envelope/glossary.go MoveTypeLabel map not found");
  const out = {};
  for (const entry of match[1].matchAll(/^\s*"([^"]+)"\s*:\s*"([\s\S]*?)",?\s*$/gm)) {
    out[entry[1]] = entry[2];
  }
  return out;
}

function parseConsoleMoveTypes(source) {
  // Matches `export const MOVE_TYPES[: Type] = [...]` whether as a literal or
  // as a re-export from move-types.ts. The move-types module is the canonical
  // home post-refactor (decision-registry.ts and glossary.ts both import from
  // it). The regex tolerates the optional type annotation read-only array.
  const match = source.match(/export const MOVE_TYPES(?:\s*:\s*[^\n=]+)?\s*=\s*\[([\s\S]*?)\];/m);
  assert.ok(match, "console MOVE_TYPES registry not found");
  return [...match[1].matchAll(/value:\s*"([^"]+)"/g)].map((m) => m[1]);
}

function parseConsoleMoveTypeLabels(source) {
  const match = source.match(/export const MOVE_TYPE_LABELS\s*:\s*Record<string,\s*string>\s*=\s*\{([\s\S]*?)\n\};/m);
  assert.ok(match, "console MOVE_TYPE_LABELS map not found");
  const out = {};
  for (const entry of match[1].matchAll(/^\s*([a-z_]+)\s*:\s*"([^"]*)",?\s*$/gm)) {
    out[entry[1]] = entry[2];
  }
  return out;
}

function parseConsoleMoveTypeGlossary(source) {
  const match = source.match(/export const MOVE_TYPE_GLOSSARY\s*:\s*Record<string,\s*string>\s*=\s*\{([\s\S]*?)\n\};/m);
  assert.ok(match, "console MOVE_TYPE_GLOSSARY map not found");
  const out = {};
  for (const entry of match[1].matchAll(/^\s*([a-z_]+)\s*:\s*"([\s\S]*?)",?\s*$/gm)) {
    out[entry[1]] = entry[2];
  }
  return out;
}

// Canonical label derivation: kept here as a comment-only reference for the
// "default" form (snake_case → "Title Case"). The actual byte-equal check
// uses Go MoveTypeLabel as the canonical source because the canonical
// "Control-plane repair" carries a hyphen the default derivation cannot
// produce ("control_plane_repair" → "Control plane repair" without the
// hyphen). The Go-owned label wins; update the Go side, not the smoke.
// console/label_reference_only(key) — DO NOT call.
function _labelReferenceOnly(key) {
  return key.replace(/_/g, " ").replace(/^./, (c) => c.toUpperCase());
}

const spine = parseGoMoveTypes(envelopeSource);
const goPlain = parseGoMoveTypePlain(glossarySource);
const goLabels = parseGoMoveTypeLabel(glossarySource);
const consoleTypes = parseConsoleMoveTypes(moveTypesSource);
const consoleLabels = parseConsoleMoveTypeLabels(moveTypesSource);
const consoleGlossary = parseConsoleMoveTypeGlossary(moveTypesSource);

// Existing assertions (kept verbatim, just re-targeted at move-types.ts):
// guards decision-registry ≠ Go enum and the memory-adjudicate contract that
// the orchestrator pinned in a prior slice.

const memoryBlock = registrySource.match(/"memory-adjudicate":\s*\{([\s\S]*?)\n\s*\},/m)?.[1] ?? "";
assert.match(memoryBlock, /source:\s*"orient"/, "memory-adjudicate should stay orient-backed");
assert.match(
  memoryBlock,
  /requiresResolvedItem:\s*true/,
  "memory-adjudicate must require candidate projection resolution before execution",
);
assert.match(
  decisionExecuteSource,
  /spec\.requiresResolvedItem/,
  "decision executor must enforce requiresResolvedItem specs",
);

assert.deepEqual(
  consoleTypes,
  spine,
  `console MOVE_TYPES drifted internal/envelope MoveTypes\nconsole=${consoleTypes.join(",")}\nspine=${spine.join(",")}`,
);

// New EP-U1 assertions (the plain-register owner promotion)

// (a) Go MoveTypePlain keys == Go MoveTypes enum — every move type has
// plain language; a new enum entry without a one-liner reds here.
assert.deepEqual(
  Object.keys(goPlain).sort(),
  [...spine].sort(),
  `internal/envelope/glossary.go MoveTypePlain keys drifted MoveTypes enum\nkeys=${Object.keys(goPlain).join(",")}\nspine=${spine.join(",")}`,
);

// (b) Console move-types values == Go enum — already covered above by the
// consoleTypes/spine deepEqual; re-state explicitly for the plain-register
// contract so the falsifier names the assertion that failed.
assert.deepEqual(
  consoleTypes,
  spine,
  "console move-types values drifted internal/envelope MoveTypes",
);

// (c) Go MoveTypePlain values byte-equal console MOVE_TYPE_GLOSSARY values.
// F2 teeth: scratch-mutate either side's wording -> red.
for (const key of spine) {
  assert.equal(
    consoleGlossary[key],
    goPlain[key],
    `MOVE_TYPE_GLOSSARY wording drifted for ${key}\ngo=${goPlain[key]}\nconsole=${consoleGlossary[key]}`,
  );
}

// (d) Console MOVE_TYPE_LABELS keys == Go enum + values byte-equal Go
// MoveTypeLabel. F2 teeth variant: scratch-mutate a console label -> red.
for (const key of spine) {
  assert.equal(
    consoleLabels[key],
    goLabels[key],
    `MOVE_TYPE_LABELS for ${key} drifted Go MoveTypeLabel\ngo=${goLabels[key]}\nconsole=${consoleLabels[key]}`,
  );
}

// (e) F3: glossary.ts does NOT carry a literal `MOVE_TYPE_LABELS:` definition.
// It must import + re-export from move-types.ts; re-growing the literal
// inside glossary.ts is the bypass the refactor just retired.
const glossaryHasLiteralLabels =
  /export const MOVE_TYPE_LABELS\s*:\s*Record<string,\s*string>\s*=\s*\{/.test(glossaryModuleSource);
const glossaryHasLiteralGlossary =
  /export const MOVE_TYPE_GLOSSARY\s*:\s*Record<string,\s*string>\s*=\s*\{/.test(glossaryModuleSource);
assert.ok(
  !glossaryHasLiteralLabels,
  "glossary.ts re-grew a MOVE_TYPE_LABELS literal; import from ./move-types instead (the plain register owner)",
);
assert.ok(
  !glossaryHasLiteralGlossary,
  "glossary.ts re-grew a MOVE_TYPE_GLOSSARY literal; import from ./move-types instead (the plain register owner)",
);

console.log(`move-type drift smoke ok: ${consoleTypes.join(", ")}`);
