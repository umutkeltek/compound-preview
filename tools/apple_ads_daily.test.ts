import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { createTestTempScope } from "./test_support.ts";
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { CANONICAL_HEADERS, ascEnv, parseArgs, parseConfig, reportPayload, rowsForReport, writeCsvs } from "./apple_ads_daily.ts";

const temporary = createTestTempScope();
afterAll(() => temporary.cleanup());

const REPORT = JSON.parse(readFileSync("fixtures/product/apple-ads-campaign-report.json", "utf8"));

function buildCompound(): string {
  const bin = join(temporary.dir("compound-apple-ads-bin-"), "compound");
  const build = spawnSync("go", ["build", "-o", bin, "./cmd/compound"], { encoding: "utf8", timeout: 120_000 });
  if (build.error) throw build.error;
  if (build.status !== 0) throw new Error(build.stderr || build.stdout);
  return bin;
}

// Compile in setup, outside the per-test timeout; teardown also runs on build failure.
let COMPOUND_BIN: string;
beforeAll(() => {
  COMPOUND_BIN = buildCompound();
}, 120_000);

describe("apple_ads_daily edge", () => {
  test("maps campaign rows to products through asc_app_id and reports unmapped apps", () => {
    const config = parseConfig("fixtures/product/asc-products.json");
    const rows = rowsForReport(config, REPORT);

    expect(rows.campaigns).toBe(3);
    expect(rows.unmapped).toEqual([{ adamId: "999999999", campaignName: "Unmapped App Try" }]);
    expect(Object.keys(rows.byProduct)).toEqual(["product:focus-notes"]);
    const recs = rows.byProduct["product:focus-notes"];
    expect(recs).toHaveLength(6);
    expect(rows.dateStart).toBe("2026-09-01");
    expect(rows.dateEnd).toBe("2026-09-03");
    const first = recs[0];
    expect(first).toEqual({
      date: "2026-09-01",
      campaign_id: "900000001",
      campaign_name: "Focus Notes US — Search Results",
      spend: "12.50",
      impressions: 1200,
      clicks: 40,
      conversions: 9,
      currency: "USD",
    });
    // Same input → same rows (the import door dedupes on identity, not order).
    expect(rowsForReport(config, REPORT)).toEqual(rows);
  });

  test("writes one canonical CSV per product with the headers the ads importer understands", () => {
    const rows = rowsForReport({ apps: [{ product_id: "product:focus-notes", asc_app_id: "123456789" }] }, REPORT);
    const outDir = temporary.dir("apple-ads-daily-");
    const csvs = writeCsvs(rows, outDir);
    expect(Object.keys(csvs)).toEqual(["product:focus-notes"]);
    const text = readFileSync(csvs["product:focus-notes"], "utf8");
    const lines = text.trim().split("\n");
    expect(lines[0]).toBe(CANONICAL_HEADERS.join(","));
    expect(lines).toHaveLength(7);
    expect(lines[1]).toBe("2026-09-01,900000001,Focus Notes US — Search Results,12.50,1200,40,9,USD");
  });

  test("self-built report request covers the last N days ending today and strips proxies", () => {
    const payload = reportPayload(14, new Date("2026-09-15T13:00:00Z"));
    expect(payload.startTime).toBe("2026-09-02");
    expect(payload.endTime).toBe("2026-09-15");
    expect(payload.granularity).toBe("DAILY");
    expect(payload.returnRecordsWithNoMetrics).toBe(true);
    expect(() => reportPayload(0)).toThrow("--days");
    const env = ascEnv({ HTTPS_PROXY: "http://proxy.invalid:1", https_proxy: "x", ALL_PROXY: "y", PATH: "/usr/bin" });
    expect(env).toEqual({ PATH: "/usr/bin" });
  });

  test("argument parsing refuses ambiguous or missing sources", () => {
    expect(() => parseArgs(["--config", "c.json"])).toThrow();
    expect(() => parseArgs(["--config", "c.json", "--report-json", "r.json", "--asc-command", "asc"])).toThrow();
    expect(() => parseArgs(["--config", "c.json", "--org", "1"])).toThrow();
    expect(parseArgs(["--config", "c.json", "--org", "21590290", "--days", "14"]).days).toBe(14);
    const args = parseArgs(["--config", "c.json", "--report-json", "r.json", "--account", "21590290", "--apply", "--json"]);
    expect(args.apply).toBe(true);
    expect(args.json).toBe(true);
    expect(args.account).toBe("21590290");
    expect(args.compoundBin).toBe("compound");
  });

  test("apply invokes the ads import door idempotently in a temp home", () => {
    const home = temporary.dir("compound-apple-ads-home-");
    const bin = COMPOUND_BIN;
    const env = { ...process.env, COMPOUND_HOME: home };
    const init = spawnSync(bin, ["home", "init", "--home", home], { encoding: "utf8", env });
    if (init.status !== 0) throw new Error(init.stderr || init.stdout);

    const args = [
      "tools/apple_ads_daily.ts",
      "--config",
      "fixtures/product/asc-products.json",
      "--report-json",
      "fixtures/product/apple-ads-campaign-report.json",
      "--account",
      "21590290",
      "--out-dir",
      join(home, "state", "apple-ads-daily"),
      "--compound-bin",
      bin,
      "--apply",
      "--json",
    ];
    const expectedRows = [
      { rows_new: 6, rows_unchanged: 0 },
      { rows_new: 0, rows_unchanged: 6 },
    ];
    for (let i = 0; i < 2; i++) {
      const run = spawnSync(process.execPath, args, { encoding: "utf8", env });
      if (run.status !== 0) throw new Error(run.stderr || run.stdout);
      const report = JSON.parse(run.stdout);
      expect(report.status).toBe("partial"); // one unmapped adamId is reported, never silently dropped
      expect(report.apply_results).toHaveLength(1);
      expect(report.apply_results[0].product).toBe("product:focus-notes");
      expect(report.apply_results[0].status).toBe(0);
      expect(report.apply_results[0].import.rows_new).toBe(expectedRows[i].rows_new);
      expect(report.apply_results[0].import.rows_unchanged).toBe(expectedRows[i].rows_unchanged);
      expect(report.apply_results[0].import.account).toBe("21590290");
    }

    const projection = spawnSync(bin, ["product", "ads", "--json", "--as-of", "2026-09-03", "--window", "3"], { encoding: "utf8", env });
    if (projection.status !== 0) throw new Error(projection.stderr || projection.stdout);
    const ads = JSON.parse(projection.stdout);
    expect(ads.status).toBe("ok");
    expect(ads.totals.spend).toBeCloseTo(39.15, 2);
    expect(ads.totals.conversions).toBe(29);
    expect(ads.platforms.find((p: { platform: string }) => p.platform === "apple_search_ads").status).toBe("connected");
  });
});
