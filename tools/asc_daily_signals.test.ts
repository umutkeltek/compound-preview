import { describe, expect, test } from "bun:test";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { exportsForSnapshot, parseConfig, signalExport } from "./asc_daily_signals.ts";

describe("asc_daily_signals edge", () => {
  test("does not invent USD or total proceeds from generic or subscription metrics", () => {
    const config = { apps: [{ product_id: "product:demo", asc_app_id: "123", metrics: ["developer_proceeds_usd"] }] };
    const ambiguous = [
      { proceeds: 12.5 },
      { metrics: { proceeds: 12.5 } },
      { metrics: [{ name: "subscription_developer_proceeds", unit: "currency", thisDay: 0, status: "ok" }] },
      { metrics: [{ name: "renewal_developer_proceeds", unit: "currency", thisDay: 12.5, status: "ok" }] },
    ];
    for (const row of ambiguous) {
      expect(exportsForSnapshot(config, { date: "2026-07-04", apps: [{ asc_app_id: "123", ...row }] })).toEqual([]);
    }
    for (const { row, expected } of [
      { row: { developer_proceeds_usd: 0 }, expected: 0 },
      { row: { metrics: { developer_proceeds_usd: 12.5, proceeds: 99 } }, expected: 12.5 },
      { row: { metrics: [{ name: "developer_proceeds_usd", unit: "currency", thisDay: -2, status: "ok" }] }, expected: -2 },
    ]) {
      const result = exportsForSnapshot(config, { date: "2026-07-04", apps: [{ asc_app_id: "123", ...row }] });
      expect(result).toHaveLength(1);
      expect(result[0].metric.name).toBe("developer_proceeds_usd");
      expect(result[0].metric.after.value).toBe(expected);
    }
  });

  test("maps every configured app metric to deterministic ProductSignalExport documents", () => {
    const config = parseConfig("fixtures/product/asc-products.json");
    const snapshot = JSON.parse(readFileSync("fixtures/product/asc-daily-signals.json", "utf8"));
    const exports = exportsForSnapshot(config, snapshot);

    expect(exports).toHaveLength(9);
    expect(exports[0].schema_version).toBe("compound.product_signal_export.v0.1");
    expect(exports[0].source.kind).toBe("app_store_connect");
    expect(exports[0].metric.before).toEqual(exports[0].metric.after);
    expect(exports.map((doc) => doc.product_id)).toContain("product:focus-notes");
    expect(exports.map((doc) => doc.metric.name)).toContain("product_page_conversion_rate");

    const again = exportsForSnapshot(config, snapshot);
    expect(again.map((doc) => doc.signal_id)).toEqual(exports.map((doc) => doc.signal_id));
  });

  test("aliases downloads but preserves currency-unspecified proceeds", () => {
    const downloads = signalExport(
      { product_id: "product:demo", asc_app_id: "123" },
      "downloads",
      7,
      "2026-07-13",
      "2026-07-13T12:00:00Z",
    );
    const proceeds = signalExport(
      { product_id: "product:demo", asc_app_id: "123" },
      "proceeds",
      3.5,
      "2026-07-13",
      "2026-07-13T12:00:00Z",
    );
    expect(downloads.metric.name).toBe("units");
    expect(proceeds.metric.name).toBe("proceeds");
  });

  test("maps daily count metrics without relabeling subscription proceeds as a USD total", () => {
    const exports = exportsForSnapshot(
      {
        apps: [
          {
            product_id: "product:sample-health-app",
            asc_app_id: "123456789",
            metrics: ["units", "developer_proceeds_usd", "product_page_views"],
          },
        ],
      },
      {
        date: "2026-07-04",
        captured_at: "2026-07-04T12:00:00Z",
        apps: [
          {
            asc_app_id: "123456789",
            metrics: [
              { name: "monetized_units", unit: "count", thisDay: 2, previousDay: 1, status: "ok" },
              {
                name: "subscription_developer_proceeds",
                unit: "currency",
                thisDay: 4.25,
                previousDay: 0,
                status: "ok",
              },
            ],
          },
        ],
      },
    );

    expect(exports).toHaveLength(1);
    expect(exports.map((doc) => doc.metric.name)).toEqual(["units"]);
    expect(exports.map((doc) => doc.metric.after.value)).toEqual([2]);
  });

  test("accepts operator-owned YAML app config", () => {
    const dir = mkdtempSync(join(tmpdir(), "compound-asc-config-"));
    const path = join(dir, "asc-products.yaml");
    writeFileSync(
      path,
      `apps:
  - product_id: product:focus-notes
    asc_app_id: "123456789"
    surface: app-store-page
    metrics:
      - units
      - impressions
`,
    );
    const config = parseConfig(path);
    expect(config.apps).toHaveLength(1);
    expect(config.apps[0].metrics).toEqual(["units", "impressions"]);
  });
});
