import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { existsSync, mkdtempSync, readdirSync, statSync } from "node:fs";
import { registerHooks } from "node:module";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

// This smoke imports the real Next route directly under Node's type-strip
// runner. Next resolves the @/ source alias itself, but bare Node does not, so
// keep the test runner's resolver equivalent without changing production
// imports merely to satisfy the harness.
const consoleSrc = join(dirname(fileURLToPath(import.meta.url)), "..", "src");
registerHooks({
  resolve(specifier, context, nextResolve) {
    if (!specifier.startsWith("@/")) return nextResolve(specifier, context);

    const base = join(consoleSrc, specifier.slice(2));
    const candidates = [
      base,
      `${base}.ts`,
      `${base}.tsx`,
      `${base}.js`,
      `${base}.mjs`,
      join(base, "index.ts"),
      join(base, "index.tsx"),
      join(base, "index.js"),
      join(base, "index.mjs"),
    ];
    const resolved = candidates.find((candidate) => {
      try {
        return statSync(candidate).isFile();
      } catch {
        return false;
      }
    });
    if (!resolved) throw new Error(`unresolved console source alias: ${specifier}`);
    return { shortCircuit: true, url: pathToFileURL(resolved).href };
  },
});

const BIN = process.env.COMPOUND_BIN;
assert.ok(BIN, "set COMPOUND_BIN to a freshly built compound binary");
const home = mkdtempSync(join(tmpdir(), "compound-console-work-trail-home-"));
const executionCwd = mkdtempSync(join(tmpdir(), "compound-console-work-trail-cwd-"));
const formatValidSyntheticCapability = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";
process.env.COMPOUND_HOME = home;
process.env.COMPOUND_CONSOLE_MUTATIONS = "1";
process.env.COMPOUND_CONSOLE_CAPABILITY = formatValidSyntheticCapability;
function compound(args) { return execFileSync(BIN, args, { env: { ...process.env, COMPOUND_HOME: home }, cwd: executionCwd, encoding: "utf8" }); }
function create(id) { compound(["work", "create", "--work-id", id, "--owner", "console-fixture", "--predicate", `console mutation ${id} reaches server state`, "--by", "console-smoke", "--state", "proposed"]); }
function listState(id) {
  const output = compound(["work", "show", id, "--json"]);
  const data = JSON.parse(output.slice(output.indexOf("{"), output.lastIndexOf("}") + 1));
  return data.state ?? "missing";
}

compound(["home", "init", "--home", home]);
const cases = [
  { id: "work_consoleadmit01", option: "admitted", label: "Add to work" },
  { id: "work_consolecancel01", option: "cancelled", label: "Cancel", fields: { summary: "operator cancelled fixture" } },
  { id: "work_consolehold01", option: "held", label: "Needs me" },
];
for (const item of cases) create(item.id);
const beforeFiles = readdirSync(home, { recursive: true }).length;
const { POST } = await import("../src/app/api/decision/execute/route.ts");

function mutationRequest(capabilityHeader, body) {
  const headers = { "content-type": "application/json", origin: "http://localhost", host: "localhost" };
  if (capabilityHeader) headers["x-compound-console-capability"] = capabilityHeader;
  return new Request("http://localhost/api/decision/execute", {
    method: "POST",
    headers,
    body,
  });
}

// Drive the real route handler with deliberately invalid JSON. The access gate
// must reject missing/wrong capabilities before body parsing; the exact valid
// capability reaches the JSON parser and therefore returns 400 instead of 403.
let blocked = await POST(mutationRequest(null, "{"));
assert.equal(blocked.status, 403, "route must reject a missing capability before parsing the body");
blocked = await POST(mutationRequest("f".repeat(64), "{"));
assert.equal(blocked.status, 403, "route must reject a wrong capability before parsing the body");
const admittedToParser = await POST(mutationRequest(formatValidSyntheticCapability, "{"));
assert.equal(admittedToParser.status, 400, "exact capability must pass the access gate and reach body parsing");

for (const item of cases) {
  const response = await POST(mutationRequest(
    formatValidSyntheticCapability,
    JSON.stringify({ area: "work-advance", itemId: item.id, option: item.option, fields: item.fields ?? {}, confirmed: true }),
  ));
  const payload = await response.json();
  assert.equal(response.status, 200, `${item.label}: ${JSON.stringify(payload)}`);
  assert.equal(payload.ok, true, `${item.label} must execute through the governed route`);
  assert.equal(listState(item.id), item.option, `${item.label} must persist ${item.option} server-side`);
}
const afterFiles = readdirSync(home, { recursive: true }).length;
assert.ok(afterFiles > beforeFiles, `mutation trail must grow (${beforeFiles} -> ${afterFiles})`);
const repoLocalCompound = join(executionCwd, ".compound");
assert.equal(existsSync(repoLocalCompound), false, `compound execFileSync must never gain a .compound under ${executionCwd}; --home flag must select the declared temp home, and direct compound calls inherit the smoke's dedicated execution cwd`);
console.log(`work mutation-trail smoke ok: Add to work, Cancel, and Needs me persisted server-side (${beforeFiles} -> ${afterFiles} files)`);
