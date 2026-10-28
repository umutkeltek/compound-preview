import assert from "node:assert/strict";
import { chmod, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

/*
 * Honest-degradation acceptance for console projection reads. The executable
 * is always a disposable stub and COMPOUND_HOME is always a disposable home;
 * this smoke never reaches a real Compound instance.
 *
 * Run one falsifier (useful for red -> green evidence):
 *   bun scripts/projection-degradation-smoke.mjs lie
 *   bun scripts/projection-degradation-smoke.mjs never-cache-null
 *   bun scripts/projection-degradation-smoke.mjs no-false-alarms
 *
 * Run the complete acceptance:
 *   bun scripts/projection-degradation-smoke.mjs
 */

const requested = process.argv[2] ?? "all";
const allowed = new Set(["all", "lie", "never-cache-null", "no-false-alarms", "health"]);
assert.ok(allowed.has(requested), `unknown projection-degradation case: ${requested}`);

const scratch = await mkdtemp(join(tmpdir(), "compound-projection-degradation-"));
const bin = join(scratch, "compound-stub");
const home = join(scratch, "home");
const state = join(scratch, "state");

await writeFile(
  bin,
  `#!/usr/bin/env bun
import { existsSync, mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const mode = process.argv[2] ?? "";
const state = process.env.PROJECTION_SMOKE_STATE ?? "";
mkdirSync(state, { recursive: true });

if (mode === "slow") {
  await Bun.sleep(16_000);
  console.log(JSON.stringify({ generated_at: new Date().toISOString(), groups: [] }));
  process.exit(0);
}

if (mode === "cache") {
  const marker = join(state, "cache-succeeded");
  if (!existsSync(marker)) {
    writeFileSync(marker, "ok");
    console.log(JSON.stringify({
      generated_at: new Date().toISOString(),
      marker: "last-good",
      open: [{ work_id: "work_example" }],
    }));
    process.exit(0);
  }
  console.error("simulated projection failure");
  process.exit(7);
}

if (mode === "empty") {
  console.log(JSON.stringify({
    generated_at: new Date().toISOString(),
    counts: { needs_you_groups: 0 },
    groups: [],
  }));
  process.exit(0);
}
if (mode === "executors") {
  const repo = process.argv[process.argv.indexOf("--repo") + 1];
  if (process.argv[3] !== "worktrees" || !repo || !process.argv.includes("--json")) process.exit(2);
  console.log(JSON.stringify({
    schema_version: "compound.worktree_inventory.v0.1", repo, generated_at: new Date().toISOString(),
    rows: [{ path: repo + "/lane", branch: "fix/example", class: "unknown",
      dirty_files: 1, unlanded_commits: 2, liveness: { state: "unknown" },
      work_id: "work_example", next_action: "preserve; liveness unresolved", reason: "probe unavailable" }],
    counts: { total: 1, unknown: 1 },
  }));
  process.exit(0);
}

console.error("unknown stub mode");
process.exit(2);
`,
  "utf8",
);
await chmod(bin, 0o755);

process.env.COMPOUND_BIN = bin;
process.env.COMPOUND_HOME = home;
process.env.PROJECTION_SMOKE_STATE = state;

try {
  // Pay the one-time OS cold-start tax on this brand-new executable at a
  // brand-new temp path BEFORE any timed read. That tax (code-signing
  // validation + page-cache miss on the first exec) is a property of the host,
  // not of the degradation semantics under test, and on a loaded machine it
  // alone exceeded the seed read's budget — a healthy projection reported as
  // `unreachable` purely from machine load. `empty` is the side-effect-free
  // stub mode, so warming through it seeds nothing the cases depend on.
  await Bun.spawn([bin, "empty"], { stdout: "ignore", stderr: "ignore" }).exited;

  const compound = await import("../src/lib/compound.ts");
  const hasProjectionRead = typeof compound.runProjection === "function";

  let feedRenderModel;
  const feedModelPath = join(import.meta.dir, "../src/components/kit/FeedDegraded.tsx");
  if (await Bun.file(feedModelPath).exists()) {
    ({ feedRenderModel } = await import("../src/components/kit/FeedDegraded.tsx"));
  }

  // The fallback deliberately reconstructs the old value-only contract so the
  // red baseline names the lie instead of failing at module import time.
  const readProjection = async (args, opts) => {
    if (hasProjectionRead) return compound.runProjection(args, opts);
    const value = await compound.runJson(args);
    return {
      status: value === null ? undefined : "live",
      value,
      generatedAt: typeof value?.generated_at === "string" ? value.generated_at : null,
      lastGoodAt: null,
      error: null,
      elapsedMs: 0,
    };
  };
  const renderModel = (read, hasData) =>
    typeof feedRenderModel === "function"
      ? feedRenderModel(read.status, hasData)
      : hasData
        ? "data"
        : "empty";

  async function lie() {
    compound.invalidateRunJsonCache();
    const read = await readProjection(["slow", "--json"], { timeoutMs: 25 });
    const model = renderModel(read, false);
    assert.equal(
      model,
      "degraded",
      `a failed projection derived the ${model} model (the old clean-seam lie)`,
    );
    assert.equal(read.status, "unreachable", "a cold timeout must be explicitly unreachable");
    assert.equal(read.value, null, "an unreachable projection must not invent data");
    assert.ok(read.error, "an unreachable projection must retain its failure reason");
    console.log("  lie ok: cold timeout -> unreachable -> degraded");
  }

  // The "cache"/"empty" stub modes never sleep - they should resolve almost
  // instantly - but a brand-new executable at a brand-new temp path pays a
  // one-time OS cold-start tax on its FIRST exec (measured up to ~300ms on
  // this host; every invocation after that is ~10-20ms). 2s is generous
  // headroom over that tax while staying far below the `lie` case's
  // deliberately-real 16s slow path, so this remains a fast smoke.
  async function neverCacheNull() {
    compound.invalidateRunJsonCache();
    const realNow = Date.now;
    try {
      const first = await readProjection(["cache", "--json"], { timeoutMs: 2_000 });
      assert.equal(first.status, "live", "the successful seed read must be live");
      assert.equal(first.value?.marker, "last-good", "the stub must seed a last-good value");

      Date.now = () => realNow() + 30_000;
      const second = await readProjection(["cache", "--json"], { timeoutMs: 2_000 });
      assert.equal(second.status, "stale", "an expired last-good value must revalidate as stale");

      // The failed background refresh settles on a SUBPROCESS's schedule, not a
      // fixed one: a 100ms sleep raced a `bun` spawn and went red purely from
      // machine load. Converge on the condition instead — re-read (the stub fails
      // every call after the seed, so the refresh failure is durable once it
      // lands) until the failure surfaces, bounded so a genuinely lost error
      // still fails the smoke rather than hanging it.
      let afterFailure = await readProjection(["cache", "--json"], { timeoutMs: 2_000 });
      const settleDeadline = realNow() + 10_000;
      while (!afterFailure.error && realNow() < settleDeadline) {
        await Bun.sleep(20);
        afterFailure = await readProjection(["cache", "--json"], { timeoutMs: 2_000 });
      }
      assert.equal(afterFailure.status, "stale", "a failed refresh with last-good data must stay stale");
      assert.equal(afterFailure.value?.marker, "last-good", "a failed refresh must not cache null over good");
      assert.ok(afterFailure.lastGoodAt, "a failed refresh must preserve lastGoodAt");
      assert.ok(afterFailure.error, "the stale read must expose the latest refresh failure");
    } finally {
      Date.now = realNow;
    }
    console.log("  never-cache-null ok: failed refresh preserved value + lastGoodAt");
  }

  async function noFalseAlarms() {
    compound.invalidateRunJsonCache();
    const read = await readProjection(["empty", "--json"], { timeoutMs: 2_000 });
    const model = renderModel(read, Array.isArray(read.value?.groups) && read.value.groups.length > 0);
    assert.equal(read.status, "live", "a fast valid empty projection must be live");
    assert.equal(model, "empty", "live + genuinely empty must retain the doctrine empty state");
    assert.ok(read.lastGoodAt, "a successful empty read must record lastGoodAt");
    assert.equal(read.error, null, "a successful empty read must not carry a false alarm");
    console.log("  no-false-alarms ok: live + empty -> doctrine empty state");
  }

async function healthTruth() {
  const { createElement } = await import("react");
  const { renderToStaticMarkup } = await import("react-dom/server");
  const { selfHealing } = await import("../src/lib/briefing.ts");
  const { HealthBackup } = await import("../src/components/settings/HealthBackup.tsx");
  const good = {
    schema_version: "compound.home_doctor_report.v0.1",
    generated_at: new Date().toISOString(),
    verdict: "ok", manifest_ok: true, dirs_ok: 12, dirs_total: 12,
    inside_git_repo: false, generated_state_in_repo_source: false,
    operational_hygiene: { ok: true, terminal_hot_work_count: 0, stale_lease_count: 0 },
  };
  const read = (value, status = "live") => ({
    value, status, generatedAt: value?.generated_at ?? null,
    lastGoodAt: value ? good.generated_at : null,
    error: status === "unreachable" ? "timed out" : null, elapsedMs: 1,
  });
  const render = (projection) => renderToStaticMarkup(createElement(HealthBackup, { doctor: projection }));
  for (const projection of [read(null, "unreachable"), read({}), read({ verdict: "ok" })]) {
    assert.equal(selfHealing(projection).ok, false, "missing/incomplete doctor must not certify health");
    const html = render(projection);
    assert.doesNotMatch(html, /Clean home|>healthy<|>true</, "unknown facts must not render healthy or true");
    assert.match(html, /unavailable|unknown|incomplete/i);
  }
  const stale = render(read(good, "stale"));
  assert.equal(selfHealing(read(good, "stale")).ok, false, "last-known health is not current health");
  assert.match(stale, /last-known|last known/i);
  assert.doesNotMatch(stale, /Clean home|>healthy</);
  assert.doesNotMatch(stale, />live ·/, "a recent timestamp must not override stale projection state");
  for (const failure of [
    { ...good, verdict: "fail" },
    { ...good, operational_hygiene: { ...good.operational_hygiene, ok: false } },
    { ...good, manifest_ok: false },
  ]) {
    assert.equal(selfHealing(read(failure)).ok, false, "backend failure must survive projection");
    assert.doesNotMatch(render(read(failure)), /Clean home|>healthy</);
  }
  const healthy = render(read(good));
  assert.equal(selfHealing(read(good)).ok, true);
  assert.match(healthy, /Clean home/);
  assert.match(healthy, /home restore .*--target /);
  assert.match(healthy, /home restore-history --id .*--dry-run/);
  assert.match(healthy, /home archive-history --kind work --dry-run/);
  assert.match(healthy, /home verify /);
  console.log("  health ok: missing/malformed/stale/failed != healthy; recovery commands are bounded");
  const { HarnessesHooks } = await import("../src/components/settings/HarnessesHooks.tsx");
  const renderHooks = (projection) => renderToStaticMarkup(createElement(HarnessesHooks, { harnessText: "", hooks: projection }));
  assert.match(renderHooks(read(null, "unreachable")), /unreachable|unavailable/i);
  const hooks = renderHooks(read({
    installed: true, hook_count: 2,
    pre_tool_use_readiness: [
      { harness: "observation", status: "non_blocking", native_block: false, installed: false },
      { harness: "missing", status: "not_installed", native_block: true, installed: false },
      { harness: "broken", status: "handler_unavailable", native_block: true, installed: false },
    ],
    hook_readiness: [{ harness: "observation", hook: "SessionStart", fixture_present: true, live_adapter: false, status: "fixture_only" }],
  }));
  assert.match(hooks, /observation only/i);
  assert.match(hooks, /not installed/i);
  assert.match(hooks, /handler unavailable/i);
  assert.match(hooks, /fixture only/i);
  assert.doesNotMatch(hooks, /adapter required/);
  const { ConsoleEnv } = await import("../src/components/settings/ConsoleEnv.tsx");
  const env = renderToStaticMarkup(createElement(ConsoleEnv, {
    attention: null, attentionRead: read(null, "unreachable"),
    mutationsEnabled: false, mutationFlagEnabled: false, instancePinned: false,
    standardClientAuthorized: false, privateFallbacks: false,
  }));
  assert.doesNotMatch(env, /No server-adapter gaps/);
  assert.doesNotMatch(env, /live adapters|live fallback|coverage lead/);
  assert.match(env, /unavailable|unreachable/i);
  const { toAttentionReport } = await import("../src/lib/attention.ts");
  const malformedCoverage = renderToStaticMarkup(createElement(ConsoleEnv, {
    attention: toAttentionReport({}), attentionRead: read({}),
    mutationsEnabled: false, mutationFlagEnabled: false, instancePinned: false,
    standardClientAuthorized: false, privateFallbacks: false,
  }));
  assert.doesNotMatch(malformedCoverage, /No server-adapter gaps|live adapters|live fallback/);
  console.log("  settings ok: unavailable hooks/attention do not become zero gaps");
  assert.equal(typeof compound.getWorktreeInventory, "function", "dashboard must consume the owned worktree inventory");
  const unbound = await compound.getWorktreeInventory("");
  assert.equal(unbound.read, null, "no explicit repo must not inspect cwd");
  const relative = await compound.getWorktreeInventory("relative/repo");
  assert.equal(relative.read, null, "relative repo must not silently become cwd authority");
  const inventory = await compound.getWorktreeInventory(scratch);
  assert.equal(inventory.read.status, "live");
  assert.equal(inventory.read.value.repo, scratch, "exact repo must reach the public command");
  const { WorktreeInventory } = await import("../src/components/agents/WorktreeInventory.tsx");
  const treeHTML = renderToStaticMarkup(createElement(WorktreeInventory, inventory));
  assert.match(treeHTML, /work_example/);
  assert.match(treeHTML, /liveness unresolved/);
  assert.match(treeHTML, /fix\/example/);
  assert.doesNotMatch(treeHTML, /<button|retire now|safe to delete/i);
  for (const classification of ["unknown", "live", "probe"]) {
    const uncertain = { ...inventory, read: read({
      ...inventory.read.value, rows: [{ path: "/fixture/lane", class: classification,
        dirty_files: 0, unlanded_commits: 0, reason: "Git probe unavailable" }],
    }) };
    const html = renderToStaticMarkup(createElement(WorktreeInventory, uncertain));
    assert.doesNotMatch(html, />0 \/ 0</, "unobserved Git counts must not become zero");
    assert.match(html, /unknown \/ unknown/);
  }
  const missingTrees = renderToStaticMarkup(createElement(WorktreeInventory, { repo: scratch, read: read(null, "unreachable") }));
  assert.match(missingTrees, /unreachable|unavailable/i);
  const staleTrees = renderToStaticMarkup(createElement(WorktreeInventory, { ...inventory, read: { ...inventory.read, status: "stale" } }));
  assert.doesNotMatch(staleTrees, />live ·/);
  const malformedTrees = renderToStaticMarkup(createElement(WorktreeInventory, { ...inventory, read: read({ ...inventory.read.value, rows: [null] }) }));
  assert.match(malformedTrees, /unreachable|unavailable/i);
  const emptyTrees = renderToStaticMarkup(createElement(WorktreeInventory, unbound));
  assert.match(emptyTrees, /COMPOUND_WORKTREE_REPO/);
  console.log("  worktrees ok: explicitly bound read-only inventory, unknown liveness preserved");
}

console.log(`projection-degradation-smoke (${requested})`);
if (requested === "all" || requested === "health") await healthTruth();
  if (requested === "all" || requested === "lie") await lie();
  if (requested === "all" || requested === "never-cache-null") await neverCacheNull();
  if (requested === "all" || requested === "no-false-alarms") await noFalseAlarms();
  console.log("projection-degradation-smoke ok");
} finally {
  await rm(scratch, { recursive: true, force: true });
}
