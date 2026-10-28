import assert from "node:assert/strict";

import { invalidateRunJsonCache, runProjection } from "./compound";

export async function runProjectionTimeoutSelfTest(): Promise<{ ok: true; details: string }> {
  invalidateRunJsonCache();

  const argv = ["timeout-escalation", "--json"];
  const shortReadPromise = runProjection(argv, { timeoutMs: 25 });
  await new Promise((resolve) => setTimeout(resolve, 5));
  const extendedReadPromise = runProjection(argv, { timeoutMs: 2_000 });

  const [shortRead, extendedRead] = await Promise.all([shortReadPromise, extendedReadPromise]);
  assert.equal(shortRead.status, "unreachable", "the intentionally short caller must time out");
  assert.equal(
    extendedRead.status,
    "live",
    "a longer projection caller must not inherit an already-running shorter timeout",
  );
  assert.equal(extendedRead.value?.marker, "extended-caller-succeeded");

  invalidateRunJsonCache();
  const seededFailure = await runProjection(argv, { timeoutMs: 25 });
  assert.equal(seededFailure.status, "unreachable", "the seed call must leave a cached cold failure");

  const cachedShortReadPromise = runProjection(argv, { timeoutMs: 25 });
  await new Promise((resolve) => setTimeout(resolve, 5));
  const cachedExtendedReadPromise = runProjection(argv, { timeoutMs: 2_000 });

  const [cachedShortRead, cachedExtendedRead] = await Promise.all([
    cachedShortReadPromise,
    cachedExtendedReadPromise,
  ]);
  assert.equal(cachedShortRead.status, "unreachable", "the repeated short caller must still time out");
  assert.equal(
    cachedExtendedRead.status,
    "live",
    "a longer caller must recover a cached never-succeeded projection instead of inheriting its short refresh",
  );
  assert.equal(cachedExtendedRead.value?.marker, "extended-caller-succeeded");

  return {
    ok: true,
    details: "longer callers escaped short in-flight and cached-failure timeouts and published live projection data",
  };
}
