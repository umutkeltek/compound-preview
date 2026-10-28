import assert from "node:assert/strict";
import { mkdtemp, mkdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

import { consoleReadiness } from "../src/lib/console-readiness.ts";
import { computeInstancePin } from "../src/lib/instance-pin.ts";

const pin = computeInstancePin({
  COMPOUND_HOME: "/synthetic/console-home",
  COMPOUND_CONSOLE_BUILD_KIND: "production",
	NODE_ENV: "production",
});

test("readiness names only current build assets and never the home path", async () => {
  const dir = await mkdtemp(join(tmpdir(), "console-readiness-test-"));
  try {
    const dist = join(dir, ".next", "production");
    await mkdir(dist, { recursive: true });
    await writeFile(join(dist, "BUILD_ID"), "synthetic-build\n");
    await writeFile(join(dist, "build-manifest.json"), JSON.stringify({
      rootMainFiles: ["static/chunks/main-app-a.js"],
    }));
    await writeFile(join(dist, "app-build-manifest.json"), JSON.stringify({
      pages: {
        "/layout": ["static/chunks/main-app-a.js", "static/css/layout-b.css"],
        "/page": ["static/chunks/page-c.js"],
      },
    }));
    const ready = await consoleReadiness(pin, dir);
    assert.deepEqual(ready.static_assets, [
      "/_next/static/chunks/main-app-a.js",
      "/_next/static/css/layout-b.css",
      "/_next/static/chunks/page-c.js",
    ]);
    assert.equal(ready.home_hash, pin.homeHash);
    assert.equal(ready.data_freshness, "not_checked");
    assert.equal(JSON.stringify(ready).includes(pin.home), false);

    await writeFile(join(dist, "app-build-manifest.json"), JSON.stringify({
      pages: { "/layout": ["static/chunks/main-app-a.js"], "/page": ["static/../private.js"] },
    }));
    await assert.rejects(consoleReadiness(pin, dir));
    await rm(join(dist, "BUILD_ID"));
    await assert.rejects(consoleReadiness(pin, dir));
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});
