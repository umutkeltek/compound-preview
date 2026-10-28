import assert from "node:assert/strict";
import { existsSync } from "node:fs";
import { chmod, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

const PRE_INVALIDATION_STAMP = "pre-invalidation-revalidation";

async function waitForFile(path: string, label: string): Promise<void> {
  const deadline = performance.now() + 2_000;
  while (!existsSync(path)) {
    if (performance.now() >= deadline) throw new Error(`timed out waiting for ${label}`);
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
}

/**
 * Falsifies the invalidate-vs-revalidate race at the cache's public projection
 * seam. The disposable executable holds an expired entry's background refresh
 * until invalidation has happened, then returns the pre-invalidation stamp.
 */
export async function runRunJsonInvalidationSelfTest(): Promise<{ ok: true; details: string }> {
  const scratch = await mkdtemp(join(tmpdir(), "compound-run-json-invalidation-"));
  const bin = join(scratch, "compound-stub");
  const home = join(scratch, "home");
  const state = join(scratch, "state");
  const revalidationStarted = join(state, "revalidation-started");
  const releaseRevalidation = join(state, "release-revalidation");
  const mutationComplete = join(state, "mutation-complete");
  const revalidationComplete = join(state, "revalidation-complete");

  await writeFile(
    bin,
    `#!/usr/bin/env bun
import { existsSync, mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const state = process.env.RUN_JSON_INVALIDATION_STATE ?? "";
mkdirSync(state, { recursive: true });
const seeded = join(state, "seeded");
const mutationComplete = join(state, "mutation-complete");

if (!existsSync(seeded)) {
  writeFileSync(seeded, "ok");
  console.log(JSON.stringify({ generated_at: "before-mutation", stamp: "pre-invalidation-seed" }));
  process.exit(0);
}

if (!existsSync(mutationComplete)) {
  writeFileSync(join(state, "revalidation-started"), "ok");
  while (!existsSync(join(state, "release-revalidation"))) await Bun.sleep(5);
  console.log(JSON.stringify({ generated_at: "before-mutation", stamp: "${PRE_INVALIDATION_STAMP}" }));
  writeFileSync(join(state, "revalidation-complete"), "ok");
  process.exit(0);
}

console.log(JSON.stringify({ generated_at: "after-mutation", stamp: "post-invalidation" }));
`,
    "utf8",
  );
  await chmod(bin, 0o755);

  const previousBin = process.env.COMPOUND_BIN;
  const previousHome = process.env.COMPOUND_HOME;
  const previousState = process.env.RUN_JSON_INVALIDATION_STATE;
  const realNow = Date.now;
  let invalidate: (() => void) | undefined;

  try {
    process.env.COMPOUND_BIN = bin;
    process.env.COMPOUND_HOME = home;
    process.env.RUN_JSON_INVALIDATION_STATE = state;

    const { invalidateRunJsonCache, runProjection } = await import("../src/lib/compound");
    invalidate = invalidateRunJsonCache;
    invalidateRunJsonCache();

    let now = realNow();
    Date.now = () => now;

    const argv = ["cache-invalidation-race", "--json"];
    const seeded = await runProjection(argv, { timeoutMs: 2_000 });
    assert.equal(seeded.status, "live", "the seed projection must land as live");
    assert.equal(seeded.value?.stamp, "pre-invalidation-seed");

    now += 25_001;
    const stale = await runProjection(argv, { timeoutMs: 2_000 });
    assert.equal(stale.status, "stale", "the expired seed must start stale-while-revalidate");
    await waitForFile(revalidationStarted, "the deferred background revalidation to start");

    invalidateRunJsonCache();
    await writeFile(mutationComplete, "ok");
    await writeFile(releaseRevalidation, "ok");
    await waitForFile(revalidationComplete, "the pre-invalidation revalidation to complete");
    // The marker is written immediately after stdout. Yield long enough for the
    // child close callback and its promise continuation to commit on buggy code.
    await new Promise((resolve) => setTimeout(resolve, 100));

    const afterInvalidation = await runProjection(argv, { timeoutMs: 2_000 });
    assert.notEqual(
      `${afterInvalidation.status}:${String(afterInvalidation.value?.stamp ?? "")}`,
      `live:${PRE_INVALIDATION_STAMP}`,
      "a revalidation started before invalidation must not republish its old stamp as fresh",
    );

    return {
      ok: true,
      details: "pre-invalidation background data could not become a fresh post-invalidation projection",
    };
  } finally {
    Date.now = realNow;
    invalidate?.();
    if (previousBin === undefined) delete process.env.COMPOUND_BIN;
    else process.env.COMPOUND_BIN = previousBin;
    if (previousHome === undefined) delete process.env.COMPOUND_HOME;
    else process.env.COMPOUND_HOME = previousHome;
    if (previousState === undefined) delete process.env.RUN_JSON_INVALIDATION_STATE;
    else process.env.RUN_JSON_INVALIDATION_STATE = previousState;
    await rm(scratch, { recursive: true, force: true });
  }
}

if ((import.meta as { main?: boolean }).main) {
  const result = await runRunJsonInvalidationSelfTest();
  console.log(`run-json-invalidation selftest ok: ${result.details}`);
}
