import { afterAll, describe, expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { join } from "node:path";

import { parseConfig } from "./asc_daily_signals.ts";
import {
  loadSalesRows,
  parseMetricList,
  planSalesSeriesApplications,
} from "./asc_sales_series_bridge.ts";
import { createTestTempScope, initializeCompoundHome } from "./test_support.ts";

const temporary = createTestTempScope();
afterAll(() => temporary.cleanup());

const seriesPath = "fixtures/product/asc-sales-series.jsonl";

describe("asc_sales_series_bridge edge", () => {
  test("plans existing Compound sales-series adapter commands from JSONL rows", () => {
    const config = parseConfig("fixtures/product/asc-products.json");
    const rows = loadSalesRows(seriesPath);

    const plan = planSalesSeriesApplications(config.apps, rows, {
      seriesPath,
      beforeWindow: "2026-07-01/2026-07-03",
      afterWindow: "2026-07-08/2026-07-10",
      days: 7,
      changeEvent: "Changed App Store screenshot order.",
      metrics: ["units"],
      compoundBin: "compound",
    });

    expect(plan.before).toBe("2026-07-01/2026-07-03");
    expect(plan.after).toBe("2026-07-08/2026-07-10");
    expect(plan.commands).toHaveLength(1);
    expect(plan.skipped).toEqual([
      {
        product_id: "product:other-app",
        asc_app_id: "987654321",
        reason: "no_rows_in_after_window",
      },
    ]);
    expect(plan.commands[0].args).toEqual([
      "product",
      "signal",
      "--asc-sales-series",
      seriesPath,
      "--product",
      "product:focus-notes",
      "--app-id",
      "123456789",
      "--before",
      "2026-07-01/2026-07-03",
      "--after",
      "2026-07-08/2026-07-10",
      "--change-event",
      "Changed App Store screenshot order.",
      "--metric",
      "units",
    ]);
  });

  test("derives adjacent before and after windows when explicit ranges are absent", () => {
    const rows = [
      { apple_identifier: "123", date: "2026-07-01", units: 1 },
      { apple_identifier: "123", date: "2026-07-02", units: 2 },
      { apple_identifier: "123", date: "2026-07-03", units: 3 },
      { apple_identifier: "123", date: "2026-07-04", units: 4 },
      { apple_identifier: "123", date: "2026-07-05", units: 5 },
      { apple_identifier: "123", date: "2026-07-06", units: 6 },
    ];

    const plan = planSalesSeriesApplications(
      [{ product_id: "product:demo", asc_app_id: "123" }],
      rows,
      {
        seriesPath,
        afterEnd: "2026-07-06",
        days: 2,
        changeEvent: "Synthetic change.",
        metrics: ["units", "developer_proceeds_usd"],
        compoundBin: "compound",
      },
    );

    expect(plan.before).toBe("2026-07-03/2026-07-04");
    expect(plan.after).toBe("2026-07-05/2026-07-06");
    expect(plan.commands.map((command) => command.metric)).toEqual([
      "units",
      "developer_proceeds_usd",
    ]);
    expect(plan.skipped).toEqual([]);
  });

  test("rejects unsupported sales metrics before invoking Compound", () => {
    expect(() => parseMetricList("units,not_a_metric")).toThrow(
      "unsupported sales metric not_a_metric",
    );
  });

  test("apply invokes Compound sales-series adapter idempotently in temp home", () => {
    const home = temporary.dir("compound-asc-bridge-home-");
    const bin = join(temporary.dir("compound-asc-bridge-bin-"), "compound");
    const build = spawnSync("go", ["build", "-o", bin, "./cmd/compound"], {
      encoding: "utf8",
    });
    if (build.status !== 0) {
      throw new Error(build.stderr || build.stdout);
    }

    const env = initializeCompoundHome(bin, home);
    const args = [
      "tools/asc_sales_series_bridge.ts",
      "--config",
      "fixtures/product/asc-products.json",
      "--sales-series",
      seriesPath,
      "--change-event",
      "Changed App Store screenshot order.",
      "--before",
      "2026-07-01/2026-07-03",
      "--after",
      "2026-07-08/2026-07-10",
      "--metric",
      "units",
      "--compound-bin",
      bin,
      "--apply",
      "--json",
    ];
    for (let i = 0; i < 2; i++) {
      const run = spawnSync(process.execPath, args, { encoding: "utf8", env });
      if (run.status !== 0) {
        throw new Error(run.stderr || run.stdout);
      }
      const report = JSON.parse(run.stdout);
      expect(report.status).toBe("ok");
      expect(report.planned).toBe(1);
      expect(report.applied).toBe(1);
      expect(report.apply_results).toEqual([
        {
          product_id: "product:focus-notes",
          asc_app_id: "123456789",
          metric: "units",
          signal_count: 1,
          transition_count: 1,
        },
      ]);
    }

    const authority = spawnSync(bin, ["product", "authority", "--json"], {
      encoding: "utf8",
      env,
    });
    if (authority.status !== 0) {
      throw new Error(authority.stderr || authority.stdout);
    }
    const authorityReport = JSON.parse(authority.stdout);
    expect(authorityReport.signal_count).toBe(1);

    const timeline = spawnSync(bin, ["product", "timeline", "product:focus-notes"], {
      encoding: "utf8",
      env,
    });
    if (timeline.status !== 0) {
      throw new Error(timeline.stderr || timeline.stdout);
    }
    expect(timeline.stdout).toContain("app_store_connect_units 9 -> 18");
  }, 15000);
});
