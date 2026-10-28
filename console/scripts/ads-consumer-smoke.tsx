import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { AdsPlatformCard } from "../src/components/ads/AdsPlatformCard";
import { AdsProductTable } from "../src/components/ads/AdsTables";
import { formatCpa, formatCpc, formatRoas, mapAdsProjection, mapTotals, sparkValues, windowDelta } from "../src/lib/ads";

// The production importer/reducer emit the JSON used by the actual mapper and
// rendered components. No platform calls or handwritten projection fixtures.
const scratch = mkdtempSync(path.join(tmpdir(), "ads-consumer-"));
const failures: string[] = [];
function check(name: string, fn: () => void) {
  try { fn(); console.log(`[ads consumer] ok: ${name}`); }
  catch (error) { failures.push(name); console.error(`[ads consumer] FAIL: ${name}: ${error}`); }
}
function metric(html: string, label: string): string {
  const match = html.match(new RegExp(`>${label}</div><div[^>]*>([^<]*)</div>`));
  assert.ok(match, `rendered ${label} metric absent`);
  return match[1];
}
try {
  const binary = process.env.COMPOUND_BIN || path.join(scratch, "compound");
  if (!process.env.COMPOUND_BIN) execFileSync("go", ["build", "-o", binary, "./cmd/compound"], {
    cwd: fileURLToPath(new URL("../../", import.meta.url)), stdio: "pipe", timeout: 180_000,
  });
  const cases = [
    { name: "complete", rows: ["2026-09-20,c1,50,1000,10,2,100,USD", "2026-09-21,c1,50,1000,10,2,100,USD"], roas: "2.00×", coverage: 1, monetary: true },
    { name: "measured-zero", rows: ["2026-09-20,c1,50,1000,10,2,0,USD", "2026-09-21,c1,50,1000,10,2,0,USD"], roas: "0.00×", coverage: 1, monetary: true },
    { name: "partial", rows: ["2026-09-21,c1,100,1000,10,2,200,USD", "2026-09-21,c1,900,9000,90,18,,USD"], roas: "—", coverage: 0.5, monetary: true },
    { name: "unknown", rows: ["2026-09-20,c1,500,1000,10,2,100,", "2026-09-21,c1,500,1000,10,2,100,"], roas: "—", coverage: 1, monetary: false },
    { name: "known-unknown", rows: ["2026-09-20,c1,100,1000,10,2,100,USD", "2026-09-21,c1,900,1000,10,2,100,"], roas: "—", coverage: 1, monetary: false },
    { name: "mixed-same-day", rows: ["2026-09-21,c1,100,1000,10,2,100,USD", "2026-09-21,c1,900,1000,10,2,100,EUR"], roas: "—", coverage: 1, monetary: false },
    { name: "mixed-across-days", rows: ["2026-09-20,c1,100,1000,10,2,100,USD", "2026-09-21,c1,900,1000,10,2,100,EUR"], roas: "—", coverage: 1, monetary: false },
  ];
  for (const fixture of cases) {
    const testHome = path.join(scratch, fixture.name);
    mkdirSync(testHome);
    const env = { ...process.env, COMPOUND_HOME: testHome };
    const run = (args: string[]) => execFileSync(binary, args, { env, encoding: "utf8", timeout: 60_000, maxBuffer: 16 * 1024 * 1024 });
    run(["home", "init"]);
    const csv = path.join(scratch, `${fixture.name}.csv`);
    writeFileSync(csv, "date,campaign_id,spend,impressions,clicks,conversions,revenue,currency\n" + fixture.rows.join("\n") + "\n");
    run(["product", "ads", "import", "--platform", "other", "--product", "product:fixture", "--report", csv, "--json"]);
    const raw = JSON.parse(run(["product", "ads", "--json", "--window", "2", "--as-of", "2026-09-21"]));
    const projection = mapAdsProjection(raw)!;
    const view = projection.platforms.find((p) => p.platform === "other")!;
    const card = renderToStaticMarkup(<AdsPlatformCard view={view} windowDays={2} />);
    const table = renderToStaticMarkup(<AdsProductTable products={projection.products} windowDays={2} />);
    check(`${fixture.name}: completeness survives mapping`, () => {
      assert.equal(raw.totals.revenue_coverage, fixture.coverage);
      assert.equal(projection.totals.revenueCoverage, fixture.coverage);
    });
    check(`${fixture.name}: card and table ROAS`, () => {
      assert.equal(formatRoas(projection.totals), fixture.roas);
      assert.equal(metric(card, "ROAS"), fixture.roas);
      assert.ok(table.includes(`>${fixture.roas}</td>`));
    });
    check(`${fixture.name}: monetary ratios and daily chart`, () => {
      if (fixture.monetary) {
        assert.notEqual(formatCpc(view.totals), "—");
        assert.notEqual(formatCpa(view.totals), "—");
        assert.notEqual(metric(card, "CPM"), "—");
        assert.equal(sparkValues(view.daily, "spend").length, 2);
        assert.ok(card.includes("<svg"), "known currency trend missing");
      } else {
        assert.equal(formatCpc(view.totals), "—");
        assert.equal(formatCpa(view.totals), "—");
        assert.equal(metric(card, "CPM"), "—");
        assert.deepEqual(sparkValues(view.daily, "spend"), []);
        assert.ok(!card.includes("<svg"), "incomparable currency trend rendered");
        assert.ok(!table.includes("<svg"), "incomparable product trend rendered");
        const daily = raw.platforms.find((p: { platform: string }) => p.platform === "other").daily;
        assert.ok(daily.every((point: { spend: unknown }) => point.spend === null), "backend emitted incomparable daily money");
        assert.equal(sparkValues(view.daily, "clicks").length, 2);
      }
    });
    if (fixture.name.includes("unknown")) check(`${fixture.name}: unresolved contribution retained`, () => {
      assert.equal(projection.totals.spendUnknownCurrency, raw.totals.spend_unknown_currency);
      assert.equal(projection.totals.rowsUnknownCurrency, raw.totals.rows_unknown_currency);
      assert.match(card, /currency unknown/);
      assert.ok(card.includes(raw.totals.spend_unknown_currency.toLocaleString("en-US", { minimumFractionDigits: 2 })), "unknown amount absent");
      assert.equal(windowDelta(view.totals, view.totals, "spend"), null);
    });
    if (fixture.name === "partial") check("partial: explanation", () => assert.match(card, /partial revenue/i));
    if (fixture.name === "complete") check("legacy payload: unavailable completeness and currency", () => {
      const legacy = structuredClone(raw);
      delete legacy.totals.revenue_coverage;
      for (const row of [...legacy.platforms, ...legacy.products, ...legacy.campaigns]) {
        delete row.totals.revenue_coverage;
        for (const point of row.daily) delete point.currency;
      }
      const old = mapAdsProjection(legacy)!;
      const oldView = old.platforms.find((p) => p.platform === "other")!;
      const oldCard = renderToStaticMarkup(<AdsPlatformCard view={oldView} windowDays={2} />);
      const oldTable = renderToStaticMarkup(<AdsProductTable products={old.products} windowDays={2} />);
      assert.equal(formatRoas(mapTotals(legacy.totals)), "—");
      assert.equal(old.totals.revenueCoverage, null);
      assert.equal(metric(oldCard, "ROAS"), "—");
      assert.match(oldCard, /revenue completeness unknown/);
      assert.match(oldTable, /revenue completeness unknown/);
      assert.ok(oldTable.includes(">—</td>"));
      assert.ok(!oldCard.includes("<svg") && !oldTable.includes("<svg"));
      assert.deepEqual(sparkValues(oldView.daily, "spend"), []);
    });
  }
  assert.deepEqual(failures, [], `ads producer-to-render failures: ${failures.join(", ")}`);
} finally {
  rmSync(scratch, { recursive: true, force: true });
}
