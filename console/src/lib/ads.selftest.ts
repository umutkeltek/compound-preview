/*
 * Depth tests for the ads panel's data plane (pure mapping + derivation over
 * `compound product ads --json`). Would FAIL if the panel rendered a ratio
 * whose denominator is zero, summed mixed currencies into one number, dropped
 * a not-connected platform from the registry view, showed a delta against an
 * empty previous window, or built the wrong projection argv. Run via
 * scripts/ads-smoke.mjs.
 */
import {
  ADS_DEFAULT_WINDOW,
  adsHeadline,
  adsProjectionArgs,
  formatCpa,
  formatCpc,
  formatCtr,
  formatMixedSpend,
  formatMoney,
  formatRoas,
  mapAdsProjection,
  mapTotals,
  orderPlatforms,
  parseAdsWindowParam,
  windowDelta,
  type AdsPlatformView,
  type AdsTotals,
} from "./ads";

type Check = { name: string; ok: boolean; detail: string };

function finalize(checks: Check[]): { ok: boolean; details: string[] } {
  return {
    ok: checks.every((c) => c.ok),
    details: checks.map((c) => `${c.ok ? "ok" : "FAIL"}: ${c.name} — ${c.detail}`),
  };
}

function totals(over: Partial<AdsTotals> = {}): AdsTotals {
  return {
    spend: 0,
    impressions: 0,
    clicks: 0,
    conversions: 0,
    revenue: 0,
    hasRevenue: false,
    revenueCoverage: null,
    spendUnknownCurrency: 0,
    rowsUnknownCurrency: 0,
    ctr: 0,
    cpc: 0,
    cpa: 0,
    cpm: 0,
    roas: 0,
    currency: "",
    mixedCurrency: false,
    spendByCurrency: {},
    days: 0,
    rows: 0,
    ...over,
  };
}

function platform(over: Partial<AdsPlatformView>): AdsPlatformView {
  return {
    platform: "x",
    label: "X",
    status: "not_connected",
    exportHint: "",
    firstDate: "",
    lastDate: "",
    lastImportAt: "",
    daysSinceLastDate: 0,
    totals: totals(),
    previous: totals(),
    daily: [],
    products: [],
    campaignsActive: 0,
    campaignsTotal: 0,
    nextMove: { text: "", command: "" },
    ...over,
  };
}

const FIXTURE = {
  schema_version: "compound.product_ads.v0.1",
  authority_ref: "compound://ledger/product-ads",
  generated_at: "2026-09-03T12:00:00Z",
  status: "ok",
  as_of: "2026-09-03",
  window: { days: 3, start: "2026-09-01", end: "2026-09-03", previous_start: "2026-08-29", previous_end: "2026-08-31" },
  filters: {},
  counts: { rows: 9, rows_in_window: 9, campaigns: 3, campaigns_active: 3, products: 2, platforms_connected: 2, platforms_stale: 0, platforms_not_connected: 6, imports: 2 },
  totals: { spend: 3865.65, impressions: 61277, clicks: 2160, conversions: 110, revenue: 9090, has_revenue: true, ctr: 0.035, cpc: 1.79, cpa: 35.1, cpm: 63, roas: 2.35, currency: "MIXED", mixed_currency: true, spend_by_currency: { USD: 112.5, TRY: 3753.15 }, days: 3, rows: 9 },
  previous: { rows: 0 },
  platforms: [
    { platform: "apple_search_ads", label: "Apple Search Ads", status: "connected", totals: { spend: 112.5, currency: "USD", rows: 6, impressions: 24097, clicks: 915, conversions: 55, ctr: 0.03797, cpc: 0.1229, cpa: 2.045 }, previous: { rows: 0 }, daily: [{ date: "2026-09-01", spend: 37.5 }, { date: "2026-09-02", spend: 35.6 }, { date: "2026-09-03", spend: 39.4 }], products: ["product:demo-ios"], campaigns_active: 2, campaigns_total: 2, next_move: { text: "Current.", command: "compound product ads signal --platform apple_search_ads" } },
    { platform: "google_ads", label: "Google Ads", status: "connected", totals: { spend: 3753.15, currency: "TRY", rows: 3, impressions: 37180, clicks: 1245, conversions: 55, revenue: 9090, has_revenue: true, revenue_coverage: 1, roas: 2.4219, ctr: 0.0335, cpc: 3.0145, cpa: 68.24 }, previous: { rows: 0 }, daily: [], products: ["product:demo-web"], next_move: { text: "Current.", command: "x" } },
    { platform: "meta_ads", label: "Meta Ads", status: "not_connected", totals: {}, previous: {}, daily: [], products: [], next_move: { text: "Not connected.", command: "compound product ads import --platform meta_ads --product <product:id> --report <export.csv>" } },
  ],
  products: [{ product_id: "product:demo-web", totals: { spend: 3753.15, currency: "TRY", rows: 3 }, previous: {}, by_platform: [{ platform: "google_ads", label: "Google Ads", spend: 3753.15, share: 1 }], daily: [] }],
  campaigns: [{ platform: "google_ads", label: "Google Ads", product_id: "product:demo-web", campaign_id: "20001", campaign_name: "demo search TR", status: "active", first_date: "2026-09-01", last_date: "2026-09-03", totals: { spend: 3753.15, currency: "TRY", rows: 3, conversions: 55, cpa: 68.24 }, previous: {}, daily: [], source_ref: "google-ads.csv@sha256:abc" }],
  imports: [{ id: "adimp_1", platform: "google_ads", product_id: "product:demo-web", source_file: "google-ads.csv", source_sha256: "abc", rows_parsed: 3, rows_new: 3, imported_at: "2026-09-04T08:00:00Z" }],
  gaps: [{ kind: "mixed_currency", text: "mixed" }, { kind: "platform_not_connected", platform: "meta_ads", text: "Meta Ads has no imported report yet.", command: "compound product ads import --platform meta_ads" }],
};

export function runAdsSelfTest(): { ok: boolean; details: string[] } {
  const checks: Check[] = [];

  /* ---- argv: the exact door the page opens ---- */
  const args = adsProjectionArgs({ window: 7, product: "product:demo-app" });
  checks.push({
    name: "projection argv is the product ads --json door with window + product",
    ok: args.join(" ") === "product ads --json --window 7 --product product:demo-app",
    detail: args.join(" "),
  });
  const defaultArgs = adsProjectionArgs({});
  checks.push({
    name: "no window → default window, no filters",
    ok: defaultArgs.join(" ") === `product ads --json --window ${ADS_DEFAULT_WINDOW}`,
    detail: defaultArgs.join(" "),
  });
  checks.push({
    name: "window param accepts 7/14/30/90 (with or without d) and falls back to the default",
    ok: parseAdsWindowParam("90d") === 90 && parseAdsWindowParam("14") === 14 && parseAdsWindowParam("3") === ADS_DEFAULT_WINDOW && parseAdsWindowParam(undefined) === ADS_DEFAULT_WINDOW,
    detail: `${parseAdsWindowParam("90d")}/${parseAdsWindowParam("14")}/${parseAdsWindowParam("3")}/${parseAdsWindowParam(undefined)}`,
  });

  /* ---- mapping: untrusted JSON → typed, nothing invented ---- */
  const proj = mapAdsProjection(FIXTURE as never);
  checks.push({ name: "null projection maps to null (unreachable door stays honest)", ok: mapAdsProjection(null) === null, detail: "null → null" });
  checks.push({
    name: "projection header + window map",
    ok: !!proj && proj.schemaVersion === "compound.product_ads.v0.1" && proj.window.days === 3 && proj.window.previousStart === "2026-08-29" && proj.status === "ok",
    detail: proj ? `${proj.schemaVersion} ${proj.window.start}..${proj.window.end}` : "null",
  });
  checks.push({
    name: "mixed-currency totals keep spend_by_currency and the MIXED marker",
    ok: !!proj && proj.totals.mixedCurrency && proj.totals.currency === "MIXED" && proj.totals.spendByCurrency.USD === 112.5 && proj.totals.spendByCurrency.TRY === 3753.15,
    detail: proj ? JSON.stringify(proj.totals.spendByCurrency) : "null",
  });
  checks.push({
    name: "platform status is one of connected/stale/not_connected, unknown → not_connected",
    ok: !!proj && proj.platforms[0].status === "connected" && proj.platforms[2].status === "not_connected" && mapAdsProjection({ platforms: [{ status: "weird" }] } as never)!.platforms[0].status === "not_connected",
    detail: proj ? proj.platforms.map((p) => `${p.platform}:${p.status}`).join(",") : "null",
  });
  checks.push({
    name: "campaign name falls back to campaign id; unknown status reads quiet",
    ok: mapAdsProjection({ campaigns: [{ campaign_id: "c9", status: "paused" }] } as never)!.campaigns[0].campaignName === "c9" && mapAdsProjection({ campaigns: [{ campaign_id: "c9", status: "paused" }] } as never)!.campaigns[0].status === "quiet",
    detail: "c9/quiet",
  });
  checks.push({
    name: "imports and gaps map with their commands",
    ok: !!proj && proj.imports[0].rowsNew === 3 && proj.gaps[1].command.startsWith("compound product ads import --platform meta_ads"),
    detail: proj ? `${proj.imports.length} imports, ${proj.gaps.length} gaps` : "null",
  });

  /* ---- ratios: never render a number the rows cannot back ---- */
  const noClicks = mapTotals({ spend: 10, impressions: 100, clicks: 0, conversions: 0, currency: "USD", rows: 1 });
  checks.push({
    name: "CTR renders with impressions, CPC/CPA read — without clicks/conversions",
    ok: formatCtr(noClicks) === "0.00%" && formatCpc(noClicks) === "—" && formatCpa(noClicks) === "—" && formatRoas(noClicks) === "—",
    detail: `${formatCtr(noClicks)} ${formatCpc(noClicks)} ${formatCpa(noClicks)} ${formatRoas(noClicks)}`,
  });
  const noImpr = mapTotals({ spend: 10, impressions: 0, rows: 1 });
  checks.push({ name: "CTR reads — without impressions", ok: formatCtr(noImpr) === "—", detail: formatCtr(noImpr) });
  const mixed = proj!.totals;
  checks.push({
    name: "mixed currency never renders CPC/CPA/ROAS as one currency",
    ok: formatCpc(mixed) === "—" && formatCpa(mixed) === "—" && formatRoas(mixed) === "—" && formatMoney(mixed.spend, mixed.currency) === "mixed" && formatMixedSpend(mixed) === "₺3,753 + $112.50",
    detail: `${formatMoney(mixed.spend, mixed.currency)} · ${formatMixedSpend(mixed)}`,
  });
  const google = proj!.platforms[1].totals;
  checks.push({
    name: "single-currency ratios render with the row currency",
    ok: formatCpa(google) === "₺68.24" && formatRoas(google) === "2.42×" && formatCpc(google) === "₺3.01" && formatCtr(google) === "3.35%",
    detail: `${formatCpa(google)} ${formatRoas(google)} ${formatCpc(google)} ${formatCtr(google)}`,
  });
  checks.push({
    name: "money without a currency is a bare number, unknown ISO code is suffixed",
    ok: formatMoney(12.5, "") === "12.50" && formatMoney(12.5, "SEK") === "12.50 SEK" && formatMoney(1234.5, "USD", { compact: true }) === "$1,235",
    detail: `${formatMoney(12.5, "")} · ${formatMoney(12.5, "SEK")} · ${formatMoney(1234.5, "USD", { compact: true })}`,
  });

  /* ---- deltas: only against a real, comparable previous window ---- */
  const now = mapTotals({ spend: 120, conversions: 12, currency: "USD", rows: 3 });
  const prev = mapTotals({ spend: 100, conversions: 8, currency: "USD", rows: 3 });
  checks.push({
    name: "delta vs previous window is a rounded percent",
    ok: windowDelta(now, prev, "spend") === 20 && windowDelta(now, prev, "conversions") === 50,
    detail: `${windowDelta(now, prev, "spend")} / ${windowDelta(now, prev, "conversions")}`,
  });
  checks.push({
    name: "empty previous window → no delta (not 100%)",
    ok: windowDelta(now, mapTotals({ rows: 0 }), "spend") === null,
    detail: String(windowDelta(now, mapTotals({ rows: 0 }), "spend")),
  });
  checks.push({
    name: "spend delta across different currencies → no delta",
    ok: windowDelta(now, mapTotals({ spend: 100, conversions: 24, currency: "TRY", rows: 3 }), "spend") === null && windowDelta(now, mapTotals({ spend: 100, conversions: 24, currency: "TRY", rows: 3 }), "conversions") === -50,
    detail: "spend null (currencies differ), conversions -50 (counts still compare)",
  });
  checks.push({
    name: "zero → zero is a flat delta, zero → something is undefined",
    ok: windowDelta(mapTotals({ rows: 1 }), mapTotals({ rows: 1 }), "clicks") === 0 && windowDelta(mapTotals({ clicks: 5, rows: 1 }), mapTotals({ rows: 1 }), "clicks") === null,
    detail: "0 / null",
  });

  /* ---- ordering + headline ---- */
  const ordered = orderPlatforms([
    platform({ platform: "a_not", status: "not_connected" }),
    platform({ platform: "b_stale", status: "stale", totals: totals({ spend: 999, rows: 1 }) }),
    platform({ platform: "c_small", status: "connected", totals: totals({ spend: 5, rows: 1 }) }),
    platform({ platform: "d_big", status: "connected", totals: totals({ spend: 50, rows: 1 }) }),
    platform({ platform: "e_not", status: "not_connected" }),
  ]).map((p) => p.platform);
  checks.push({
    name: "platforms order: connected by spend, then stale, then not connected in registry order",
    ok: ordered.join(",") === "d_big,c_small,b_stale,a_not,e_not",
    detail: ordered.join(","),
  });
  const live = adsHeadline(proj!);
  checks.push({ name: "live headline counts current/stale platforms and active campaigns", ok: live.state === "live" && live.text === "2 platforms current · 3 campaigns active", detail: live.text });
  const quiet = adsHeadline({ ...proj!, status: "quiet", counts: { ...proj!.counts, rowsInWindow: 0 } });
  checks.push({ name: "quiet headline names the window and the fix", ok: quiet.state === "quiet" && quiet.text.includes("last 3 days"), detail: quiet.text });
  const empty = adsHeadline({ ...proj!, status: "empty", counts: { ...proj!.counts, rows: 0 } });
  checks.push({ name: "empty headline says no reports yet", ok: empty.state === "empty" && empty.text.startsWith("No ad reports imported yet"), detail: empty.text });

  return finalize(checks);
}
