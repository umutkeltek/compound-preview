import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { mock } from "bun:test";

// Keep real functions before Bun replaces the module's live export bindings.
const actual = { ...fs };
const diskDir = join(tmpdir(), "compound-console-run-json");
const isDiskFile = (path) => String(path).startsWith(`${diskDir}/`) && String(path).endsWith(".json");

function barrier() {
  let release;
  const promise = new Promise((resolve) => { release = resolve; });
  return { promise, release };
}

async function bounded(promise, label) {
  let timer;
  try {
    return await Promise.race([
      promise,
      new Promise((_, reject) => { timer = setTimeout(() => reject(new Error(`${label} was not reached`)), 5000); }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

export function installProjectionDiskProbe() {
  const control = { deletion: null, deletionStarted: null, read: null, readStarted: null, readsDuringDeletion: 0 };
  mock.module("node:fs/promises", () => ({
    ...actual,
    async rm(path, options) {
      const pause = control.deletion;
      if (pause && isDiskFile(path)) {
        control.deletionStarted.release();
        await pause.promise;
      }
      return actual.rm(path, options);
    },
    async readFile(path, ...args) {
      if (control.deletion && isDiskFile(path)) control.readsDuringDeletion += 1;
      const contents = await actual.readFile(path, ...args);
      const pause = control.read;
      if (pause && isDiskFile(path)) {
        control.readStarted.release();
        await pause.promise;
      }
      return contents;
    },
  }));
  return control;
}

async function waitForPersistedProbe(probe) {
  const deadline = Date.now() + 5000;
  while (Date.now() < deadline) {
    for (const name of await actual.readdir(diskDir).catch(() => [])) {
      if (!name.endsWith(".json")) continue;
      const entry = await actual.readFile(join(diskDir, name), "utf8").then(JSON.parse).catch(() => null);
      if (entry?.value?.probe === probe) return;
    }
    await Bun.sleep(5);
  }
  throw new Error(`successful projection ${probe} never reached the disk cache`);
}

export async function runProjectionDiskProbe(api, control) {
  const argv = ["timeout-escalation", "delete-before-cold-read"];
  assert.equal((await api.runProjection(argv, { timeoutMs: 3000 })).status, "live");
  await waitForPersistedProbe(argv[1]);
  control.deletion = barrier();
  control.deletionStarted = barrier();
  let pending;
  try {
    api.invalidateRunJsonCache();
    await bounded(control.deletionStarted.promise, "paused invalidation deletion");
    pending = api.runProjection(argv, { timeoutMs: 25 });
    assert.equal(control.readsDuringDeletion, 0, "a cold read must wait for invalidated disk entries to be removed");
  } finally {
    control.deletion.release();
    control.deletion = null;
    if (pending) await pending;
  }
  assert.equal((await pending).status, "unreachable", "an invalidated successful disk value must not replace the cold timeout");

  const racingArgv = ["timeout-escalation", "invalidate-during-disk-read"];
  assert.equal((await api.runProjection(racingArgv, { timeoutMs: 3000 })).status, "live");
  await waitForPersistedProbe(racingArgv[1]);
  // Public invalidation also deletes disk state. Drop only process memory to
  // model a fresh reader of a retained entry, then invalidate during its read.
  globalThis.__compoundConsoleRunJson.cache.clear();
  control.read = barrier();
  control.readStarted = barrier();
  let racing;
  try {
    racing = api.runProjection(racingArgv, { timeoutMs: 25 });
    await bounded(control.readStarted.promise, "paused disk read");
    api.invalidateRunJsonCache();
  } finally {
    control.read.release();
    control.read = null;
  }
  assert.equal((await racing).status, "unreachable", "a disk read from an invalidated generation must not repopulate memory");
  console.log("projection disk invalidation: paused deletion and in-progress read controls passed");
}
