import assert from "node:assert/strict";
import { chmod, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { renderToStaticMarkup } from "react-dom/server";
import { createElement } from "react";
import { AppRouterContext } from "next/dist/shared/lib/app-router-context.shared-runtime.js";
import { SearchParamsContext, PathnameContext } from "next/dist/shared/lib/hooks-client-context.shared-runtime.js";

const scratch = await mkdtemp(join(tmpdir(), "compound-memory-read-"));
const bin = join(scratch, "compound-stub");
await writeFile(bin, `#!/usr/bin/env bun
const mode = process.env.MEMORY_READ_SMOKE_MODE;
if (process.argv[2] === "version") { console.log("synthetic"); process.exit(0); }
if (mode === "fail") { console.error("synthetic memory projection unavailable"); process.exit(7); }
if (mode === "slow") await Bun.sleep(16000);
let items = mode === "empty" ? [] : [
 {id:"cand_action",entity:"product:synthetic",state:"proposed",predicate:"status",object:"observed",confidence:0.8},
 {id:"cand_source",entity:"knowledge:synthetic",state:"proposed",predicate:"source",object:"evidence",confidence:0.8},
 {id:"cand_law",entity:"decision:synthetic",state:"parked",claim_type:"decision",predicate:"choice",object:"Keep evidence",confidence:0.8},
  {id:"cand_promoted",state:"promoted",created_at:new Date().toISOString()},
  {id:"cand_prior",state:"promoted",created_at:new Date(Date.now()-15*86400000).toISOString()}
];
const state = process.argv.indexOf("--state");
if (state >= 0) items = items.filter(x => x.state === process.argv[state+1]);
const counts = mode === "empty" || mode === "missing-count" ? {} : mode === "invalid" ? {promoted:"unknown"} : {promoted:mode === "undercount" ? 1 : 17,parked:4,proposed:2};
console.log(JSON.stringify({generated_at:new Date().toISOString(),items,counts:{by_state:counts}}));
`);
await chmod(bin, 0o755);
process.env.COMPOUND_BIN = bin;
process.env.COMPOUND_HOME = join(scratch, "home");
process.env.COMPOUND_CONSOLE_MUTATIONS = "0";
// Disk cache is fixture-owned, never the operator console cache.
process.env.TMPDIR = scratch;
await Bun.spawn([bin,"version"],{stdout:"ignore",stderr:"ignore"}).exited;

const originalNow = Date.now;
let clockOffset = 0;
Date.now = () => originalNow() + clockOffset;
const joinRefreshes = async () => Promise.all(
  [...(globalThis.__compoundConsoleRunJson?.inflight.values() ?? [])].map(x => x.promise));

try {
  const compound = await import("../src/lib/compound.ts");
  const memory = await import("../src/lib/memory-data.ts");
  const { default: Memory } = await import("../src/app/memory/page.tsx");
  const { default: Briefing } = await import("../src/app/page.tsx");
  // Supply the framework contexts a real Next render provides. No memory
  // loader, card, projection or page implementation is replaced.
  const router = Object.fromEntries(["push","replace","refresh","back","forward","prefetch"].map(k => [k,()=>{}]));
  const renderPage = async () => renderToStaticMarkup(
    createElement(AppRouterContext.Provider,{value:router},
      createElement(PathnameContext.Provider,{value:"/memory"},
        createElement(SearchParamsContext.Provider,{value:new URLSearchParams()},
          await Memory({searchParams:Promise.resolve({})})))));

  const renderHome = async () => renderToStaticMarkup(createElement(AppRouterContext.Provider,{value:router},
    createElement(PathnameContext.Provider,{value:"/"},
      createElement(SearchParamsContext.Provider,{value:new URLSearchParams()},
        await Briefing()))));

  process.env.MEMORY_READ_SMOKE_MODE = "fail";
  const failed = await memory.loadProposedMemorySplit();
  assert.equal(failed.read?.status, "unreachable", "cold failed memory read must not be indistinguishable from live empty");
  assert(failed.read.error, "failed read retains its error");
  const unavailableHome = await renderHome();
  assert.match(unavailableHome, /Memory unavailable/, "home must not turn failed memory reads into zero");
  assert.doesNotMatch(unavailableHome, /promoted · 0 awaiting · 0 parked/, "unknown counts must not become a zero summary");
  const unavailable = await renderPage();
  assert.match(unavailable, /unreachable/, "actual memory page discloses failed reads");
  assert.doesNotMatch(unavailable, /Clean seam|No proposed source evidence/, "failed page may not claim an empty queue or source set");
  assert.match(unavailable, /—/, "unavailable counts are unknown, not zero");

  process.env.MEMORY_READ_SMOKE_MODE = "empty";
  const empty = await memory.loadProposedMemorySplit();
  assert.equal(empty.read.status,"live");
  const emptyHome = await renderHome();
  assert.match(emptyHome, /promoted · 0 awaiting · 0 parked/, "real empty memory remains zero");
  assert.doesNotMatch(emptyHome, /Memory unavailable|Last known memory/);
  const liveEmpty = await renderPage();
  assert.match(liveEmpty, /No proposed source evidence/);
  assert.doesNotMatch(liveEmpty, /unreachable|previously read/);

  process.env.MEMORY_READ_SMOKE_MODE = "data";
  clockOffset += 26000;
  await memory.loadProposedMemorySplit();
  await joinRefreshes();
  const live = await memory.loadProposedMemorySplit();
  assert.equal(live.read.status,"live");
  assert.deepEqual(live.actionable.map(x=>x.id),["cand_action"]);
  assert.deepEqual(live.sourceEvidence.map(x=>x.id),["cand_source"]);
  await renderPage();
  await renderHome();
  await joinRefreshes();
  const liveHome = await renderHome();
  assert.match(liveHome, />17</, "actual home receives global memory counts");
  assert.match(liveHome, /promotions vs previous 7 days/, "covered live window shows comparison");
  clockOffset += 26000;
  process.env.MEMORY_READ_SMOKE_MODE = "fail";
  const stale = await memory.loadProposedMemorySplit();
  assert.equal(stale.read.status,"stale");
    assert(stale.read.lastGoodAt);
  assert.deepEqual(stale.sourceEvidence.map(x=>x.id),["cand_source"]);
  const staleHome = await renderHome();
  assert.match(staleHome, /Last known memory/);
  assert.match(staleHome, />17</, "home retains last-known count");
  assert.equal((staleHome.match(/Last known memory/g) ?? []).length, 2, "both home memory surfaces label stale totals");
  assert.match(staleHome, /Last successful read:/);
  assert.doesNotMatch(staleHome, /Memory unavailable|promotions vs previous 7 days/);
  const stalePage = await renderPage();
  assert.match(stalePage,/previously read/,"actual page marks last-good memory data as stale");
  assert.doesNotMatch(stalePage,/Clean seam|No proposed source evidence/);
  // Join the failed background refresh before invalidating or cleaning up.
  await joinRefreshes();

  for (const mode of ["invalid", "missing-count", "undercount"]) {
    compound.invalidateRunJsonCache();
    process.env.MEMORY_READ_SMOKE_MODE = mode;
    const invalidHome = await renderHome();
    assert.match(invalidHome, /Memory report incomplete/, mode + " must not become a false zero");
    assert.doesNotMatch(invalidHome, /promoted · 0 awaiting · 0 parked/);
    await joinRefreshes();
  }
  compound.invalidateRunJsonCache();
  process.env.MEMORY_READ_SMOKE_MODE = "slow";
  clockOffset += 26000;
  await memory.loadProposedMemorySplit();
  await joinRefreshes();
  const slow = await memory.loadProposedMemorySplit();
  assert.equal(slow.read.status,"live","memory loader must use the existing bounded heavy-read budget, not 15s");
  assert.equal(slow.sourceEvidence.length,1);
  console.log("memory-read honesty ok: cold failure, live empty, retained stale data, actual page rendering and >15s read");
} finally {
  await joinRefreshes();
  Date.now = originalNow;
  await rm(scratch,{recursive:true,force:true});
}
