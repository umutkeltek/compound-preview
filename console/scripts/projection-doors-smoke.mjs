import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { constants, accessSync, mkdtempSync, readdirSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { performance } from "node:perf_hooks";
import { fileURLToPath, pathToFileURL } from "node:url";

import { DOORS } from "../src/lib/doors.ts";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const CONSOLE_ROOT = path.resolve(HERE, "..");
const BIN = process.env.COMPOUND_BIN || "compound";
const HOME = process.env.COMPOUND_HOME?.trim() || "";
const ENFORCE_BUDGETS = process.env.PROJECTION_DOORS_ENFORCE_BUDGETS === "1";
// CI hands this gate a freshly built binary and a disposable initialized home,
// so a skip THERE is a wiring regression, not a host fact. The switch is set by
// the workflow only; a local or host-bound run keeps the skip.
const REQUIRE_CLI = process.env.CONSOLE_SMOKE_REQUIRE_CLI === "1";
const MAX_BUFFER = 64 * 1024 * 1024;

// The manifest is deliberately scoped to the six briefing doors. These other
// literal read calls predate that contract and remain visible here so any new
// literal runJson/runProjection argv still fails the drift guard.
const PREEXISTING_NON_BRIEFING_ARGV = new Set(
  [
    ["contract", "hooks-authority"],
    ["decisions", "authority", "--json"],
    ["decisions", "standing-law", "--json"],
    ["director", "propose", "--dry-run", "--json"],
    ["memory", "candidates", "--json", "--include-evidence"],
    ["memory", "candidates", "--json", "--state", "parked", "--kind", "decision"],
    ["memory", "engine-config", "--json"],
    ["packets", "list", "--json"],
    ["policy", "autonomy-halt", "--json"],
    ["policy", "editable-surfaces", "--json"],
    ["policy", "operator-approval-frontier-authority", "--json"],
    ["policy", "operator-prompts-authority", "--json"],
    ["policy", "placement-authority", "--json"],
    ["policy", "release-goal-holds-authority", "--json"],
    ["policy", "standing-grants-authority", "--json"],
    ["product", "authority", "--json"],
    ["product", "loops", "--json"],
    ["reviews", "--json"],
    ["route", "evidence-authority", "--json"],
    ["route", "model-capabilities", "--json"],
    ["source", "connections", "--json"],
    ["work", "leases", "--json"],
    ["work", "list", "--json"],
    ["workbench", "--json"],
  ].map((argv) => JSON.stringify(argv)),
);

function skip(reason) {
  if (REQUIRE_CLI) {
    console.error(`projection-doors-smoke FAIL: CONSOLE_SMOKE_REQUIRE_CLI=1 but ${reason}`);
    process.exit(1);
  }
  console.log(`projection-doors-smoke SKIP: ${reason}`);
  process.exit(0);
}

function binaryAvailable() {
  try {
    if (BIN.includes(path.sep)) accessSync(BIN, constants.X_OK);
    execFileSync(BIN, ["version"], { encoding: "utf8", stdio: "ignore" });
    return true;
  } catch (error) {
    return error?.code !== "ENOENT" && error?.code !== "EACCES";
  }
}

function sourceFiles() {
  const files = [];
  const appRoot = path.join(CONSOLE_ROOT, "src", "app");
  const libRoot = path.join(CONSOLE_ROOT, "src", "lib");

  const walkApp = (dir) => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const absolute = path.join(dir, entry.name);
      if (entry.isDirectory()) {
        walkApp(absolute);
      } else if (/\.(?:ts|tsx)$/.test(entry.name)) {
        files.push(absolute);
      }
    }
  };

  walkApp(appRoot);
  for (const entry of readdirSync(libRoot, { withFileTypes: true })) {
    if (entry.isFile() && /\.ts$/.test(entry.name)) files.push(path.join(libRoot, entry.name));
  }
  return files;
}

function literalProjectionCalls(source) {
  const calls = [];
  // The argv array may be followed by an options argument (runProjection's
  // { timeoutMs }); current options objects never contain ")".
  const pattern = /\b(?:runJson|runProjection)\s*\(\s*(\[(?:\s*"(?:\\.|[^"\\])*"\s*,?)*\s*\])\s*(?:,[^)]*)?\)/gms;
  for (const match of source.matchAll(pattern)) {
    calls.push(JSON.parse(match[1].replace(/,\s*]$/, "]")));
  }
  return calls;
}

// A door's argv may be bound to a local before the call (`const args = ["memory",
// "candidates", "--json"]; ... runJson(args)` in src/lib/memory-data.ts, which
// predates this guard), and the call-site pattern above cannot see through the
// binding. These feed the UNUSED half only: the missing half stays scoped to
// literal call sites, exactly as its contract above states, so widening here
// cannot turn a pre-existing non-briefing read into a new failure.
function boundProjectionArgv(source) {
  const argv = [];
  const pattern = /\b(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?::[^=]+?)?=\s*(\[(?:\s*"(?:\\.|[^"\\])*"\s*,?)*\s*\])/gms;
  for (const match of source.matchAll(pattern)) {
    const binding = match[1].replace(/\$/g, "\\$");
    const passed = new RegExp(`\\b(?:runJson|runProjection)\\s*\\(\\s*${binding}\\s*(?:,[^)]*)?\\)`);
    if (passed.test(source)) argv.push(JSON.parse(match[2].replace(/,\s*]$/, "]")));
  }
  return argv;
}

function assertDoorManifestCoversLiteralCalls() {
  const declared = new Map();
  for (const door of DOORS) {
    const key = JSON.stringify(door.argv);
    assert.ok(!declared.has(key), `duplicate door argv ${key}`);
    declared.set(key, door.id);
  }

  const found = new Map();
  const bound = new Set();
  for (const file of sourceFiles()) {
    const source = readFileSync(file, "utf8");
    for (const argv of literalProjectionCalls(source)) {
      const key = JSON.stringify(argv);
      const relative = path.relative(CONSOLE_ROOT, file);
      found.set(key, [...(found.get(key) ?? []), relative]);
    }
    for (const argv of boundProjectionArgv(source)) bound.add(JSON.stringify(argv));
  }

  const missing = [...found.entries()]
    .filter(([key]) => !declared.has(key) && !PREEXISTING_NON_BRIEFING_ARGV.has(key))
    .map(([key, files]) => `${key} at ${files.join(", ")}`)
    .sort();
  assert.deepEqual(
    missing,
    [],
    `projection door manifest missing literal argv:\n${missing.map((item) => `  ${item}`).join("\n")}`,
  );

  const unused = [...declared.entries()]
    .filter(([key]) => !found.has(key) && !bound.has(key))
    .map(([key, id]) => `${id}: ${key}`);
  assert.deepEqual(unused, [], `declared doors have no console call site: ${unused.join(", ")}`);
}

function firstJSONObject(output) {
  const start = output.indexOf("{");
  assert.ok(start >= 0, "stdout contains no JSON object");
  let depth = 0;
  let inString = false;
  let escaped = false;
  for (let index = start; index < output.length; index++) {
    const char = output[index];
    if (inString) {
      if (escaped) escaped = false;
      else if (char === "\\") escaped = true;
      else if (char === '"') inString = false;
      continue;
    }
    if (char === '"') inString = true;
    else if (char === "{") depth++;
    else if (char === "}" && --depth === 0) return JSON.parse(output.slice(start, index + 1));
  }
  assert.fail("stdout contains an incomplete JSON object");
}

function assertRFC3339(value, door) {
  assert.equal(typeof value, "string", `${door.id} generated_at must be a string`);
  assert.ok(value.trim(), `${door.id} generated_at must be non-empty`);
  assert.match(
    value,
    /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/,
    `${door.id} generated_at must be RFC3339`,
  );
  assert.ok(Number.isFinite(Date.parse(value)), `${door.id} generated_at must be parseable`);
}

function runDoor(door) {
  const started = performance.now();
  let output = "";
  let error = null;
  try {
    output = execFileSync(BIN, door.argv, {
      encoding: "utf8",
      env: { ...process.env, COMPOUND_HOME: HOME },
      maxBuffer: MAX_BUFFER,
    });
    const report = firstJSONObject(output);
    assertRFC3339(report.generated_at, door);
  } catch (caught) {
    const stderr = String(caught?.stderr ?? "").trim();
    error = stderr ? `${caught.message}; stderr: ${stderr}` : caught.message;
  }
  return { door, elapsedMs: performance.now() - started, error };
}

// Exercise the production reader with owner-created records, not a duplicated
// regex or a mapped fixture: valid explicit IDs must survive the read boundary.
function assertWorkDetailReader() {
  const fixtureRoot = mkdtempSync(path.join(tmpdir(), "compound-work-reader-"));
  const fixtureHome = path.join(fixtureRoot, "home");
  const env = { ...process.env, COMPOUND_HOME: fixtureHome, COMPOUND_BIN: BIN };
  const ids = ["work_abc123def456", "work_selected_fixture_1", "work_Custom-shaped_1"];
  try {
    execFileSync(BIN, ["home", "init"], { env, stdio: "pipe" });
    for (const id of ids) {
      execFileSync(BIN, ["work", "create", "--work-id", id, "--owner", "console-smoke",
        "--by", "console-smoke", "--predicate", "Exact work remains visible",
        "--outcome", "Inspect the synthetic task"], { env, stdio: "pipe" });
    }
    const moduleURL = pathToFileURL(path.join(CONSOLE_ROOT, "src/lib/compound.ts")).href;
    execFileSync("node", ["--experimental-strip-types", "--input-type=module", "--eval", `
      import assert from "node:assert/strict";
      const { getWork } = await import(${JSON.stringify(moduleURL)});
      for (const id of ${JSON.stringify(ids)}) {
        const work = await getWork(id);
        assert.equal(work?.id, id, "work-detail reader must preserve exact owner-created ID");
        assert.equal(work?.desired_outcome, "Inspect the synthetic task");
      }
    `], { env, stdio: "pipe" });
    // A not-found response would also return null. Observe the CLI boundary to
    // prove invalid input is rejected before dispatch, in a fresh reader process.
    const marker = path.join(fixtureRoot, "cli-called");
    const fakeBin = path.join(fixtureRoot, "compound-probe");
    writeFileSync(fakeBin, `#!/usr/bin/env node
      require("node:fs").writeFileSync(${JSON.stringify(marker)}, "called");
      process.stdout.write("{}");
    `, { mode: 0o700 });
    const invalidIDs = ["work_", "work_../escape", "work_a\\escape", " work_abc123 ",
      "--help", "work_a\u0000b", "work_a\nb", "work_a\u007fb", "work_a\u0085b", "work_a\u009fb"];
    execFileSync("node", ["--experimental-strip-types", "--input-type=module", "--eval", `
      import assert from "node:assert/strict";
      import { existsSync } from "node:fs";
      const { getWork } = await import(${JSON.stringify(moduleURL)});
      for (const id of ${JSON.stringify(invalidIDs)}) {
        const result = await getWork(id);
        assert.equal(existsSync(${JSON.stringify(marker)}), false,
          "unsafe identity must not dispatch CLI: " + JSON.stringify(id));
        assert.equal(result, null, "unsafe read identity must remain rejected");
      }
      await getWork("work_valid_probe");
      assert.equal(existsSync(${JSON.stringify(marker)}), true, "dispatch probe must be live");
    `], { env: { ...env, COMPOUND_BIN: fakeBin }, stdio: "pipe" });
    console.log("projection-doors-smoke: real CLI work-detail IDs and goal round-trip ok");
  } finally {
    rmSync(fixtureRoot, { recursive: true, force: true });
  }
}

if (!HOME) skip("COMPOUND_HOME is not set");
try {
  if (!statSync(HOME).isDirectory()) skip("COMPOUND_HOME is not a directory");
} catch {
  skip("COMPOUND_HOME does not exist");
}
if (!binaryAvailable()) skip(`compound binary is unavailable: ${BIN}`);

const failures = [];
try {
  assertDoorManifestCoversLiteralCalls();
} catch (error) {
  failures.push(`drift-guard: ${error.message}`);
}
try {
  assertWorkDetailReader();
} catch (error) {
  console.error(`FAIL work-detail-reader: ${error.message}`);
  process.exit(1);
}

const results = DOORS.map(runDoor);
console.log("projection-doors-smoke");
console.log("door                 measured   budget   result");
for (const result of results) {
  const overBudget = result.elapsedMs > result.door.budgetMs;
  const status = result.error ? "FAIL" : overBudget ? "WARN" : "ok";
  console.log(
    `${result.door.id.padEnd(20)} ${result.elapsedMs.toFixed(1).padStart(8)}ms ${String(result.door.budgetMs).padStart(6)}ms   ${status}`,
  );
  if (result.error) failures.push(`${result.door.id}: ${result.error}`);
  if (overBudget) {
    const message = `${result.door.id} measured ${result.elapsedMs.toFixed(1)}ms > ${result.door.budgetMs}ms budget`;
    if (ENFORCE_BUDGETS) failures.push(message);
    else console.warn(`WARN ${message}`);
  }
}

if (failures.length > 0) {
  for (const failure of failures) console.error(`FAIL ${failure}`);
  process.exitCode = 1;
} else {
  console.log("projection-doors-smoke ok");
}
